package push

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/google/googletest"
	"github.com/jmckible/pneu/internal/push/state"
	"github.com/jmckible/pneu/internal/remote"
)

const install = "0123456789abcdef"

// testClock is the manager's clock in tests: Now is the fake's wall clock
// (which Google's fake reads too); After runs on a separate monotonic
// clock. advance moves both; suspend moves only the wall, as a sleep
// does to Go's timers.
type testClock struct {
	wall, mono *googletest.Clock
}

func (c testClock) Now() time.Time                         { return c.wall.Now() }
func (c testClock) After(d time.Duration) <-chan time.Time { return c.mono.After(d) }

// nudger records nudges.
type nudger struct {
	mu      sync.Mutex
	n       map[string]int
	unknown map[string]bool
}

func (n *nudger) Nudge(account string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.unknown[account] {
		return fmt.Errorf("unknown account %q", account)
	}
	n.n[account]++
	return nil
}

func (n *nudger) count(account string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.n[account]
}

type env struct {
	t      *testing.T
	f      *googletest.Fake
	api    *google.API
	clock  testClock
	store  state.Store
	nudges *nudger
	owner  googletest.User
	ownerR string            // the owner's refresh token
	mbx    map[string]string // account -> its mailbox's refresh token
	addr   map[string]string // account -> address

	mu      sync.Mutex
	logs    []string
	changes []string

	m *Manager
}

// newEnv provisions the push project in a fake Google: the owner, and
// for each account a mailbox, its topic (publisher granted) and its
// subscription; then writes client.json and state.json with every
// account on. The manager isn't started.
func newEnv(t *testing.T, accounts ...string) *env {
	t.Helper()
	f := googletest.New(t)
	f.PullHold = 2 * time.Second
	e := &env{t: t, f: f, clock: testClock{wall: f.Clock, mono: googletest.NewClock(googletest.Epoch)},
		store:  state.Store{Dir: state.Dir(filepath.Join(t.TempDir(), "pneu"))},
		nudges: &nudger{n: map[string]int{}, unknown: map[string]bool{}},
		mbx:    map[string]string{}, addr: map[string]string{}}
	e.api = f.NewAPI(google.Options{PullTimeout: 5 * time.Second, Timeout: 5 * time.Second})
	e.owner = f.AddUser("owner@example.com")
	ownerTok := e.consent(google.Owner, e.owner.Email)
	e.ownerR = ownerTok.Refresh
	ctx := context.Background()
	for _, a := range accounts {
		addr := a + "@example.com"
		f.AddUser(addr)
		e.addr[a] = addr
		e.mbx[a] = e.consent(google.Mailbox, addr).Refresh
		res, _ := google.Resource(a)
		if _, err := e.api.EnsureTopic(ctx, ownerTok.Access, f.Project, res); err != nil {
			t.Fatal(err)
		}
		if _, err := e.api.GrantPublisher(ctx, ownerTok.Access, f.Project, res); err != nil {
			t.Fatal(err)
		}
		if _, err := e.api.EnsureSubscription(ctx, ownerTok.Access, f.Project, res, res, install); err != nil {
			t.Fatal(err)
		}
	}
	l, err := e.store.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client := fmt.Sprintf(`{"installed":{"client_id":%q,"client_secret":%q,"project_id":%q,"redirect_uris":["http://localhost"]}}`,
		f.ClientID, f.Secret, f.Project)
	if _, err := l.WriteClient([]byte(client)); err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	e.commit(func(sf *state.File) {
		sf.Project, sf.Install, sf.Client = f.Project, install, f.ClientID
		sf.Owner = state.Owner{Sub: e.owner.Sub, Email: e.owner.Email, Refresh: e.ownerR, Granted: googletest.Epoch}
		sf.Accounts = map[string]state.Account{}
		for _, a := range accounts {
			sf.Accounts[a] = state.Account{State: state.On, Address: e.addr[a], Refresh: e.mbx[a], Granted: googletest.Epoch}
		}
	})
	LockRetry = 10 * time.Millisecond
	t.Cleanup(e.checkLogs)
	return e
}

func (e *env) consent(kind google.Kind, email string) google.Token {
	e.t.Helper()
	c, err := google.NewConsent(e.f.Credentials(), kind, email)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, err := e.f.Approve(c.URL(), email)
	if err != nil {
		e.t.Fatal(err)
	}
	q, err := remote.ParseCallback(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	code, err := c.Callback(q)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.api.Exchange(context.Background(), c, code)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// commit writes the next generation of state.json, as the CLI does.
func (e *env) commit(modify func(*state.File)) state.Snapshot {
	e.t.Helper()
	l, err := e.store.Lock(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	defer l.Unlock()
	snap, err := l.Update(func(f *state.File) error { modify(f); return nil })
	if err != nil {
		e.t.Fatal(err)
	}
	return snap
}

// start makes and starts the manager and waits for it to be ready.
func (e *env) start() *Manager {
	e.t.Helper()
	e.m = e.newManager()
	e.m.Start()
	e.t.Cleanup(e.m.Stop)
	e.eventually("ready", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return e.m.ready
	})
	return e.m
}

func (e *env) newManager() *Manager {
	return New(Options{Store: e.store, API: e.api, Engine: e.nudges, Clock: e.clock, Settle: 15 * time.Second,
		OnChange: func(a string) {
			e.mu.Lock()
			e.changes = append(e.changes, a)
			e.mu.Unlock()
		},
		Logf: func(format string, args ...any) {
			e.mu.Lock()
			e.logs = append(e.logs, fmt.Sprintf(format, args...))
			e.mu.Unlock()
		}})
}

// reload is the CLI's push-reload of snap.
func (e *env) reload(snap state.Snapshot) control.Reload {
	e.t.Helper()
	r, err := e.m.Reload(snap.Generation, snap.Hash)
	if err != nil {
		e.t.Fatalf("reload %d: %v", snap.Generation, err)
	}
	return r
}

// advance moves both clocks by d in steps, giving the workers a moment
// after each.
func (e *env) advance(d, step time.Duration) {
	for d > 0 {
		s := min(step, d)
		e.clock.wall.Advance(s)
		e.clock.mono.Advance(s)
		d -= s
		time.Sleep(2 * time.Millisecond)
	}
}

// eventually waits (real time) for cond.
func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// until is pump in one-second steps, for waits the clock may move
// through: an empty pull re-pulls only as fake seconds pass.
func (e *env) until(what string, cond func() bool) {
	e.t.Helper()
	e.pump(what, time.Second, 10*time.Minute, cond)
}

// pump advances both clocks by step until cond, giving up after limit
// of fake time.
func (e *env) pump(what string, step, limit time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	moved := time.Duration(0)
	for !cond() {
		if time.Now().After(deadline) || moved > limit {
			e.t.Fatalf("timed out waiting for %s (fake time %v)", what, moved)
		}
		e.clock.wall.Advance(step)
		e.clock.mono.Advance(step)
		moved += step
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *env) state(account string) control.PushState { return e.m.State(account) }

func (e *env) is(account, st, reason string) func() bool {
	return func() bool {
		p := e.state(account)
		return p.State == st && p.Reason == reason
	}
}

// subCalls counts requests of op on account's subscription.
func (e *env) subCalls(op google.Op, account string) int {
	res, _ := google.Resource(account)
	n := 0
	for _, r := range e.f.Requests() {
		if r.Op == op && strings.Contains(r.Path, "/subscriptions/"+res+":") {
			n++
		}
	}
	return n
}

// watchCalls counts users.watch calls by account's mailbox: each one's
// token belongs to it, which the request log shows only by topic.
func (e *env) watchCalls(account string) int {
	res, _ := google.Resource(account)
	n := 0
	for _, r := range e.f.Requests() {
		if r.Op == google.OpWatch && strings.Contains(string(r.Body), "/topics/"+res+`"`) {
			n++
		}
	}
	return n
}

func (e *env) logged() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.logs)
}

func (e *env) changed(account string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, a := range e.changes {
		if a == account {
			n++
		}
	}
	return n
}

// checkLogs holds every line logged to the closed vocabulary: an
// account, an operation and a code, never anything Google wrote.
func (e *env) checkLogs() {
	for _, l := range e.logged() {
		for _, bad := range []string{"Google's words", "Error sending", "PERMISSION_DENIED", "ya29.", "1//", "ack-", "Bad Request", "{"} {
			if strings.Contains(l, bad) {
				e.t.Errorf("log line carries Google's text (%q): %s", bad, l)
			}
		}
		if !strings.HasPrefix(l, "push") {
			e.t.Errorf("log line: %s", l)
		}
	}
}

// pushGoroutines lists goroutines running this package's code.
func pushGoroutines() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "internal/push.(") && !strings.Contains(g, "_test.go") {
			out = append(out, g)
		}
	}
	return out
}
