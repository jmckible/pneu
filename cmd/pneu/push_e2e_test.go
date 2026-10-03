package main

// The seam between the CLI (push.go) and the daemon (internal/push behind
// internal/control): task 3's commands run in-process against a real
// push.Manager answering on a real control socket, both finding their
// files the way the binary does ($XDG_RUNTIME_DIR, $XDG_STATE_HOME, both
// temp), with googletest as Google and a recording engine. The wall
// clock (the fake's, which the manager, the CLI and Google's token and
// watch expiry all read) moves with real time, and a test may jump it;
// the manager's waits are real, so the CLI's real 60s budget for
// delivering is the one measured.

import (
	"fmt"
	"net"
	"net/http"
	"os"
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
	"github.com/jmckible/pneu/internal/push"
	"github.com/jmckible/pneu/internal/push/state"
)

// seamClock is the manager's clock here: the fake's wall clock (token
// expiry and watch expiration read it too), real waits.
type seamClock struct{ wall *googletest.Clock }

// runWall moves c along with real time until the test ends.
func runWall(t *testing.T, c *googletest.Clock) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		last := time.Now()
		for {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
			now := time.Now()
			c.Advance(now.Sub(last))
			last = now
		}
	}()
	t.Cleanup(func() { close(done); <-stopped })
}

func (c seamClock) Now() time.Time                         { return c.wall.Now() }
func (c seamClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// seamEngine is the sync engine as push sees it: it records each nudge.
// With slow set, a nudge takes that long (the real one returns at once):
// a worker caught in one can't finish until it returns, which widens the
// window a reload's join has to cover.
type seamEngine struct {
	mu   sync.Mutex
	n    map[string]int
	slow time.Duration
	busy int // nudges under way
}

func (g *seamEngine) Nudge(account string) error {
	g.mu.Lock()
	g.n[account]++
	slow := g.slow
	g.busy++
	g.mu.Unlock()
	time.Sleep(slow)
	g.mu.Lock()
	g.busy--
	g.mu.Unlock()
	return nil
}

func (g *seamEngine) setSlow(d time.Duration) {
	g.mu.Lock()
	g.slow = d
	g.mu.Unlock()
}

func (g *seamEngine) nudging() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.busy > 0
}

func (g *seamEngine) nudges(account string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n[account]
}

// ackRec is one push-reload the daemon answered: the generation, the
// answer, how many requests the fake had logged by then, and how many
// worker goroutines were still running.
type ackRec struct {
	gen     uint64
	r       control.Reload
	err     error
	reqs    int
	workers int
}

// seamDaemon is the server daemon's push half, wired as serve does: the
// manager made, the control socket up with its two handlers, then the
// manager started; stopped socket first.
type seamDaemon struct {
	s   *seam
	m   *push.Manager
	srv *control.Server

	mu   sync.Mutex
	acks []ackRec
	once sync.Once
}

// seam is one machine: a config, the CLI's env from newPushEnv (Google
// swapped for the fake, the consent for a browser that approves as as),
// and at most one daemon at a time.
type seam struct {
	t      *testing.T
	f      *googletest.Fake
	e      *pushEnv
	out    *lockedBuf
	engine *seamEngine
	d      *seamDaemon

	mu     sync.Mutex
	as     string
	opened int
	errs   chan error

	logMu sync.Mutex
	logs  []string
}

// newSeam: accounts are name, address pairs.
func newSeam(t *testing.T, accounts ...string) *seam {
	t.Helper()
	f := googletest.New(t)
	f.PullHold = 2 * time.Second // a pull waits for the watch's message
	runWall(t, f.Clock)
	f.AddUser(ownerAddr)
	var accts []string
	for i := 0; i+1 < len(accounts); i += 2 {
		f.AddUser(accounts[i+1])
		accts = append(accts, fmt.Sprintf(`{"name":%q,"email":%q,"notmuchConfig":"/nonexistent/nm","gmiDir":"/nonexistent/gmi"}`, accounts[i], accounts[i+1]))
	}
	home := t.TempDir()
	cfg := filepath.Join(home, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"port":7317,"accounts":[`+strings.Join(accts, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A short runtime dir: a socket path is capped at 108 bytes.
	run, err := os.MkdirTemp("", "pneuseam")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	if err := os.Chmod(run, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	s := &seam{t: t, f: f, out: &lockedBuf{}, engine: &seamEngine{n: map[string]int{}}, errs: make(chan error, 16)}
	var port int
	out := termPush{w: s.out,
		listen: func() ([]net.Listener, error) {
			port = freeBothPort(t)
			return listenLoopback(port, bindExclusive)
		},
		open: func(u string) error {
			s.mu.Lock()
			s.opened++
			as := s.as
			s.mu.Unlock()
			p := port
			go func() {
				q, err := f.Approve(u, as)
				if err != nil {
					s.errs <- err
					return
				}
				if code, _ := redirect(p, q); code != http.StatusOK {
					s.errs <- fmt.Errorf("the redirect got %d", code)
				}
			}()
			return nil
		}}
	e, err := newPushEnv(cfg, out)
	if err != nil {
		t.Fatal(err)
	}
	if e.sockErr != nil || e.socket != filepath.Join(run, "pneu", "control") {
		t.Fatalf("socket %q, %v", e.socket, e.sockErr)
	}
	if want := state.Dir(filepath.Join(home, "state", "pneu")); e.store.Dir != want {
		t.Fatalf("store %q, want %q", e.store.Dir, want)
	}
	if e.deliverWait != 60*time.Second {
		t.Fatalf("deliverWait %v", e.deliverWait)
	}
	e.api = f.NewAPI(google.Options{})
	e.now = f.Clock.Now // the daemon's wall clock, as on one machine
	e.pollEvery = 20 * time.Millisecond
	s.e = e
	t.Cleanup(func() {
		select {
		case err := <-s.errs:
			t.Error(err)
		default:
		}
		s.checkLogs()
	})
	return s
}

func (s *seam) signInAs(email string) {
	s.mu.Lock()
	s.as = email
	s.mu.Unlock()
}

func (s *seam) consents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened
}

// init is `pneu push init`, signing in as the owner.
func (s *seam) init(p initParams) error {
	s.signInAs(ownerAddr)
	return s.e.init(p)
}

func (s *seam) firstInit() error {
	return s.init(initParams{project: s.f.Project, client: pushClientJSON(s.f)})
}

// on is `pneu account push <name>`, signing in as address.
func (s *seam) on(name, address string) error {
	s.signInAs(address)
	return s.e.on(name, false)
}

func (s *seam) load() state.Snapshot {
	s.t.Helper()
	snap, err := s.e.store.Load()
	if err != nil {
		s.t.Fatal(err)
	}
	return snap
}

// startDaemon plays serve: the manager made, the socket listening, then
// the manager started (it takes daemon.lock in the background). It
// doesn't wait for the manager to be ready.
func (s *seam) startDaemon() *seamDaemon {
	s.t.Helper()
	d := s.listenDaemon()
	d.m.Start()
	return d
}

// listenDaemon is startDaemon without the start: a daemon whose push
// manager hasn't taken daemon.lock.
func (s *seam) listenDaemon() *seamDaemon {
	s.t.Helper()
	if s.d != nil {
		s.t.Fatal("a daemon runs already")
	}
	d := &seamDaemon{s: s}
	d.m = push.New(push.Options{
		Store:  s.e.store,
		API:    s.f.NewAPI(google.Options{Timeout: 5 * time.Second, PullTimeout: 5 * time.Second}),
		Engine: s.engine,
		Clock:  seamClock{s.f.Clock},
		Settle: 15 * time.Second,
		Logf: func(format string, args ...any) {
			s.logMu.Lock()
			s.logs = append(s.logs, fmt.Sprintf(format, args...))
			s.logMu.Unlock()
		},
	})
	srv, err := control.Listen(s.e.socket, control.Handler{
		PushReload: func(gen uint64, hash string) (control.Reload, error) {
			r, err := d.m.Reload(gen, hash)
			rec := ackRec{gen: gen, r: r, err: err, reqs: len(s.f.Requests()), workers: workerGoroutines()}
			d.mu.Lock()
			d.acks = append(d.acks, rec)
			d.mu.Unlock()
			return r, err
		},
		PushState: d.m.State,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	d.srv = srv
	s.d = d
	s.t.Cleanup(d.stop)
	return d
}

// stop is serve's exit: the socket closed, then the manager stopped.
func (d *seamDaemon) stop() {
	d.once.Do(func() {
		d.srv.Close()
		d.m.Stop()
		if d.s.d == d {
			d.s.d = nil
		}
	})
}

// ready waits for the manager to hold daemon.lock and have read
// state.json: until then push-reload answers an error. An unrecorded
// reload of generation 0 is the probe (mismatch, nothing applied).
func (d *seamDaemon) ready() {
	d.s.t.Helper()
	d.s.eventually("the daemon ready", func() bool {
		_, err := d.m.Reload(0, "")
		return err == nil
	})
}

// ack is the daemon's answer to generation gen's push-reload.
func (d *seamDaemon) ack(gen uint64) (ackRec, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range slices.Backward(d.acks) {
		if a.gen == gen {
			return a, true
		}
	}
	return ackRec{}, false
}

func (d *seamDaemon) ackCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.acks)
}

// workerGoroutines counts goroutines running a push worker's loops (a
// watch loop and push.Pulls pull loops per worker). A joined worker's
// loops have all returned.
func workerGoroutines() int {
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "internal/push.(*worker).") {
			n++
		}
	}
	return n
}

// pushState asks over the socket, as the CLI does.
func (s *seam) pushState(account string) control.PushState {
	s.t.Helper()
	p, err := control.AskPushState(s.e.socket, account)
	if err != nil {
		s.t.Fatalf("push-state %s: %v", account, err)
	}
	return p
}

func (s *seam) eventually(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s\n%s", what, s.out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// delivering waits for account to report delivering at generation gen.
func (s *seam) delivering(account string, gen uint64) {
	s.t.Helper()
	s.eventually(account+" delivering", func() bool {
		p, err := control.AskPushState(s.e.socket, account)
		return err == nil && p.State == control.PushDelivering && p.Generation == gen
	})
}

// subRequests indexes the fake's requests on account's subscription or
// watch (pull, ack, users.watch): every call a worker makes but its
// token refreshes.
func (s *seam) subRequests(account string) []int {
	res, _ := google.Resource(account)
	var at []int
	for i, r := range s.f.Requests() {
		switch {
		case (r.Op == google.OpPull || r.Op == google.OpAck) && strings.Contains(r.Path, "/subscriptions/"+res+":"),
			r.Op == google.OpWatch && strings.Contains(string(r.Body), "/topics/"+res+`"`):
			at = append(at, i)
		}
	}
	return at
}

func (s *seam) watches(account string) int {
	res, _ := google.Resource(account)
	n := 0
	for _, r := range s.f.Requests() {
		if r.Op == google.OpWatch && strings.Contains(string(r.Body), "/topics/"+res+`"`) {
			n++
		}
	}
	return n
}

// requestIndex is the first request of op at or after from, or -1.
func (s *seam) requestIndex(op google.Op, from int) int {
	for i, r := range s.f.Requests() {
		if i >= from && r.Op == op {
			return i
		}
	}
	return -1
}

func (s *seam) says(want ...string) {
	s.t.Helper()
	for _, w := range want {
		if !strings.Contains(s.out.String(), w) {
			s.t.Errorf("no %q in the output:\n%s", w, s.out.String())
		}
	}
}

func (s *seam) neverSays(bad ...string) {
	s.t.Helper()
	for _, b := range bad {
		if strings.Contains(s.out.String(), b) {
			s.t.Errorf("%q in the output:\n%s", b, s.out.String())
		}
	}
}

// checkLogs holds the daemon's logs to push's closed vocabulary.
func (s *seam) checkLogs() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	for _, l := range s.logs {
		if !strings.HasPrefix(l, "push") || strings.ContainsAny(l, "{}") || strings.Contains(l, "ya29.") || strings.Contains(l, "1//") {
			s.t.Errorf("daemon log: %s", l)
		}
	}
}

// waitNudges waits for account's nudges to reach n, then holds that no
// more come for a moment.
func (s *seam) waitNudges(account string, n int) {
	s.t.Helper()
	s.eventually(fmt.Sprintf("%d nudges for %s", n, account), func() bool { return s.engine.nudges(account) >= n })
	time.Sleep(50 * time.Millisecond)
	if got := s.engine.nudges(account); got != n {
		s.t.Fatalf("%s nudged %d times, want %d", account, got, n)
	}
}

// Init and the first account push against a running daemon: init's
// handover is acknowledged; the account's commit is applied ("ok <gen>")
// and its worker started; the watch's at-once message arrives and is a
// nudge; the CLI's wait sees this instance at this generation
// delivering, and says so as proof of this watch; mail afterwards is a
// nudge too.
func TestPushSeamOn(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatalf("init: %v\n%s", err, s.out.String())
	}
	s.says("The running pneu has it.", "Push is set up.")
	if a, ok := d.ack(1); !ok || a.r != control.ReloadApplied || a.err != nil {
		t.Fatalf("init's reload: %+v %v", a, ok)
	}
	if p := s.pushState("personal"); p.Generation != 1 || p.State != control.PushOff || p.Instance != control.Instance() {
		t.Fatalf("after init: %+v", p)
	}

	s.out.Reset()
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.says("committed: personal is on (generation 2)",
		"Instant mail is on for personal: Gmail's watch delivered its first notification.")
	s.neverSays("restarted", "not proven", "pending")
	if a, ok := d.ack(2); !ok || a.r != control.ReloadApplied || a.err != nil {
		t.Fatalf("on's reload: %+v %v", a, ok)
	}
	p := s.pushState("personal")
	if p.Generation != 2 || p.State != control.PushDelivering || p.Instance != control.Instance() || p.LastDelivery.IsZero() {
		t.Fatalf("after on: %+v", p)
	}
	if topic, _, ok := s.f.Watch(mailAddr); !ok || topic != "projects/"+s.f.Project+"/topics/pneu-personal" || s.watches("personal") != 1 {
		t.Fatalf("watch %q %v, %d calls", topic, ok, s.watches("personal"))
	}
	// The at-once message: one nudge, acknowledged.
	s.waitNudges("personal", 1)
	s.eventually("the first message acked", func() bool { return s.f.Acked("pneu-personal") == 1 })

	// New mail: a nudge, acknowledged.
	if !s.f.Notify(mailAddr) {
		t.Fatal("no watch to publish to")
	}
	s.waitNudges("personal", 2)
	s.eventually("the second message acked", func() bool { return s.f.Acked("pneu-personal") == 2 })
	if s.f.Queued("pneu-personal") != 0 || s.f.Outstanding("pneu-personal") != 0 {
		t.Fatalf("queued %d, outstanding %d", s.f.Queued("pneu-personal"), s.f.Outstanding("pneu-personal"))
	}
}

// Running account push again with the stored grant: no consent, a new
// generation applied with the worker kept (no second watch, health
// carried), still delivering, worded as a reused subscription.
func TestPushSeamOnAgain(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.waitNudges("personal", 1)
	consents, workers := s.consents(), workerGoroutines()
	if workers != 1+push.Pulls {
		t.Fatalf("%d worker goroutines", workers)
	}

	s.out.Reset()
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("again: %v\n%s", err, s.out.String())
	}
	s.says("the stored mailbox grant works", "committed: personal is on (generation 3)",
		"Instant mail is on for personal: its subscription is delivering. It existed before this run")
	if s.consents() != consents {
		t.Fatalf("consents %d, was %d", s.consents(), consents)
	}
	if a, ok := d.ack(3); !ok || a.r != control.ReloadApplied || a.workers != workers {
		t.Fatalf("the rerun's reload: %+v %v (workers before %d)", a, ok, workers)
	}
	if p := s.pushState("personal"); p.Generation != 3 || p.State != control.PushDelivering {
		t.Fatalf("after the rerun: %+v", p)
	}
	if s.watches("personal") != 1 {
		t.Fatalf("watched %d times: the worker was replaced", s.watches("personal"))
	}
	// Still the same worker taking mail.
	s.f.Notify(mailAddr)
	s.waitNudges("personal", 2)
}

// --off against a running daemon: off-pending is applied and the worker
// joined before the ack (the worker is caught in a slow nudge when the
// reload comes, so the join has to wait for it), and users.stop comes
// after it; then the removal. Nothing for the account reaches Google
// after that ack, and mail no longer nudges.
func TestPushSeamOff(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.waitNudges("personal", 1)
	s.engine.setSlow(700 * time.Millisecond) // inside JoinWait
	s.f.Notify(mailAddr)
	s.eventually("a nudge under way", s.engine.nudging)

	s.out.Reset()
	if err := s.e.off("personal"); err != nil {
		t.Fatalf("off: %v\n%s", err, s.out.String())
	}
	s.says("committed: personal is off-pending (generation 3)", "Gmail's watch on "+mailAddr+" is stopped",
		"committed: personal removed (generation 4)", "Instant mail is off for personal.")
	pending, ok := d.ack(3)
	if !ok || pending.r != control.ReloadApplied || pending.err != nil {
		t.Fatalf("off-pending's reload: %+v %v", pending, ok)
	}
	if pending.workers != 0 || s.engine.nudging() {
		t.Fatalf("off-pending acknowledged with %d worker goroutines running (nudging %v)", pending.workers, s.engine.nudging())
	}
	stop := s.requestIndex(google.OpStop, 0)
	if stop < pending.reqs {
		t.Fatalf("users.stop at request %d, before off-pending's ack (%d requests by then)", stop, pending.reqs)
	}
	if removed, ok := d.ack(4); !ok || removed.r != control.ReloadApplied {
		t.Fatalf("the removal's reload: %+v %v", removed, ok)
	}
	if s.f.Stops(mailAddr) != 1 {
		t.Fatalf("users.stop %d times", s.f.Stops(mailAddr))
	}
	if _, ok := s.load().Accounts["personal"]; ok {
		t.Fatal("still in state.json")
	}
	if p := s.pushState("personal"); p.State != control.PushOff || p.Generation != 4 {
		t.Fatalf("after off: %+v", p)
	}
	// Nothing on the subscription or watch since the ack, a while later.
	time.Sleep(1500 * time.Millisecond)
	if at := s.subRequests("personal"); len(at) > 0 && at[len(at)-1] >= pending.reqs {
		t.Fatalf("request %d on personal's subscription after off-pending's ack at %d", at[len(at)-1], pending.reqs)
	}
	if s.f.Notify(mailAddr) {
		t.Fatal("the watch outlived users.stop")
	}
	s.f.Publish("pneu-personal", 1) // a stray message: nobody pulls it
	time.Sleep(200 * time.Millisecond)
	if s.engine.nudges("personal") != 2 || s.f.Queued("pneu-personal") != 1 {
		t.Fatalf("nudges %d, queued %d after off", s.engine.nudges("personal"), s.f.Queued("pneu-personal"))
	}
}

// No daemon: nothing answers on the socket and daemon.lock is free, so
// init and account push say it starts with pneu; a daemon started
// afterwards runs state.json as committed.
func TestPushSeamNoDaemon(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	if err := s.firstInit(); err != nil {
		t.Fatalf("init: %v\n%s", err, s.out.String())
	}
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.says("pneu isn't running; it takes this when it starts.", "pneu isn't running; instant mail for personal starts when it does.")
	s.neverSays("has it", "Instant mail is on", "pending")
	if s.f.Calls(google.OpPull) != 0 || s.f.Calls(google.OpWatch) != 0 {
		t.Fatal("something pulled or watched with no daemon")
	}
	snap := s.load()
	if snap.Generation != 2 || snap.Accounts["personal"].State != state.On {
		t.Fatalf("state: %+v", snap.File)
	}
	// The CLI let go of daemon.lock: a daemon starting now takes it and
	// runs generation 2 without being told.
	d := s.startDaemon()
	s.delivering("personal", 2)
	if d.ackCount() != 0 {
		t.Fatalf("%d reloads", d.ackCount())
	}
	if s.watches("personal") != 1 {
		t.Fatalf("watched %d times", s.watches("personal"))
	}
	s.waitNudges("personal", 1)
	// And the next command finds it.
	s.out.Reset()
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("again: %v\n%s", err, s.out.String())
	}
	s.says("committed: personal is on (generation 3)", "its subscription is delivering")
}

// A daemon whose push manager doesn't hold daemon.lock yet: push-reload
// answers an error, which the CLI reports as pending, never as done, and
// never mistakes for no daemon. Once the manager has the lock it runs
// what was committed.
func TestPushSeamPending(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	// The socket is up, the manager not started: serve's moment between.
	d := s.listenDaemon()
	err := s.firstInit()
	if err == nil || !strings.Contains(err.Error(), "pending. Running pneu push init again is safe") {
		t.Fatalf("init: %v\n%s", err, s.out.String())
	}
	s.neverSays("has it", "pneu isn't running", "runs no push sync", "Push is set up.")
	if a, ok := d.ack(1); !ok || a.err == nil {
		t.Fatalf("init's reload: %+v %v", a, ok)
	}
	if s.load().Generation != 1 {
		t.Fatal("init didn't commit")
	}

	// Started, but another process holds daemon.lock (a CLI that found no
	// daemon a moment ago): the manager waits for it, and reloads error.
	held, err := s.e.store.TryDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	d.m.Start()
	s.out.Reset()
	err = s.on("personal", mailAddr)
	if err == nil || !strings.Contains(err.Error(), "personal is on in state.json, but the running pneu hasn't confirmed it") ||
		!strings.Contains(err.Error(), "pending. Running pneu account push personal again is safe") {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.neverSays("Instant mail is on", "pneu isn't running", "waiting up to")
	if a, ok := d.ack(2); !ok || a.err == nil {
		t.Fatalf("on's reload: %+v %v", a, ok)
	}
	if s.f.Calls(google.OpPull) != 0 {
		t.Fatal("pulled before daemon.lock")
	}

	// The lock comes free: the manager takes it and runs generation 2.
	held.Unlock()
	s.delivering("personal", 2)
	s.out.Reset()
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("again: %v\n%s", err, s.out.String())
	}
	s.says("committed: personal is on (generation 3)", "Instant mail is on for personal: its subscription is delivering.")
}

// The owner's reconsent (push init --reconsent): a new owner refresh
// token, so every worker is replaced, each renewing its watch and coming
// back to delivering.
func TestPushSeamReconsent(t *testing.T) {
	const workAddr = "work@example.com"
	s := newSeam(t, "personal", mailAddr, "work", workAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on personal: %v\n%s", err, s.out.String())
	}
	if err := s.on("work", workAddr); err != nil {
		t.Fatalf("on work: %v\n%s", err, s.out.String())
	}
	s.waitNudges("personal", 1)
	s.waitNudges("work", 1)
	before := s.load()
	consents := s.consents()

	s.out.Reset()
	if err := s.init(initParams{reconsent: true}); err != nil {
		t.Fatalf("reconsent: %v\n%s", err, s.out.String())
	}
	s.says("Sign in as the push owner, "+ownerAddr, "committed: owner "+ownerAddr, "The running pneu has it.")
	after := s.load()
	if s.consents() != consents+1 || after.Owner.Refresh == before.Owner.Refresh || after.Owner.Sub != before.Owner.Sub {
		t.Fatalf("consents %d (was %d), refresh changed %v, sub kept %v", s.consents(), consents,
			after.Owner.Refresh != before.Owner.Refresh, after.Owner.Sub == before.Owner.Sub)
	}
	a, ok := d.ack(after.Generation)
	if !ok || a.r != control.ReloadApplied {
		t.Fatalf("reconsent's reload: %+v %v", a, ok)
	}
	for _, name := range []string{"personal", "work"} {
		s.eventually(name+" watched again", func() bool { return s.watches(name) == 2 })
		s.delivering(name, after.Generation)
		s.waitNudges(name, 2) // the new watch's at-once message
	}
	if n := workerGoroutines(); n != 2*(1+push.Pulls) {
		t.Fatalf("%d worker goroutines", n)
	}
}

// Two accounts: each its own worker; --off of one leaves the other's
// worker untouched and delivering.
func TestPushSeamTwoAccounts(t *testing.T) {
	const workAddr = "work@example.com"
	s := newSeam(t, "personal", mailAddr, "work", workAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on personal: %v\n%s", err, s.out.String())
	}
	s.out.Reset()
	if err := s.on("work", workAddr); err != nil {
		t.Fatalf("on work: %v\n%s", err, s.out.String())
	}
	s.says("Instant mail is on for work: Gmail's watch delivered its first notification.")
	s.waitNudges("personal", 1)
	s.waitNudges("work", 1)
	if s.watches("personal") != 1 || s.watches("work") != 1 {
		t.Fatalf("watches %d, %d: turning work on replaced personal's worker", s.watches("personal"), s.watches("work"))
	}
	// Each subscription nudges its own account.
	s.f.Notify(workAddr)
	s.waitNudges("work", 2)
	s.waitNudges("personal", 1)

	if err := s.e.off("personal"); err != nil {
		t.Fatalf("off: %v\n%s", err, s.out.String())
	}
	snap := s.load()
	pending, ok := d.ack(snap.Generation - 1)
	if !ok || pending.workers != 1+push.Pulls {
		t.Fatalf("off-pending's reload: %+v %v", pending, ok)
	}
	if _, ok := snap.Accounts["personal"]; ok || snap.Accounts["work"].State != state.On {
		t.Fatalf("state: %+v", snap.File)
	}
	if p := s.pushState("personal"); p.State != control.PushOff {
		t.Fatalf("personal: %+v", p)
	}
	if p := s.pushState("work"); p.State != control.PushDelivering || p.Generation != snap.Generation {
		t.Fatalf("work: %+v", p)
	}
	if s.watches("work") != 1 || s.f.Stops(workAddr) != 0 {
		t.Fatalf("work watched %d times, stopped %d", s.watches("work"), s.f.Stops(workAddr))
	}
	if s.f.Notify(mailAddr) {
		t.Fatal("personal's watch outlived --off")
	}
	s.f.Notify(workAddr)
	s.waitNudges("work", 3)
	if s.engine.nudges("personal") != 1 {
		t.Fatalf("personal nudged %d times", s.engine.nudges("personal"))
	}
}

// A subscription deleted and made again (the fix pushwords gives for one
// not as pneu makes it) under an unchanged credential: the daemon keeps
// the account's worker, health and all, so it reports delivering at the
// new generation on the strength of a message from before this run. That
// isn't this watch's first notification, and the CLI mustn't say it is.
func TestPushSeamRecreatedSubscription(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.waitNudges("personal", 1)
	// A minute on, the subscription is deleted and made again: Google's
	// create answers with the new resource.
	s.f.Clock.Advance(time.Minute)
	raw := s.f.Subscription("pneu-personal")
	s.f.SetSubscription("pneu-personal", "pneu-personal", raw)
	s.f.Fail(google.OpSubMake, googletest.Failure{Status: http.StatusOK, Body: raw})

	s.out.Reset()
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("again: %v\n%s", err, s.out.String())
	}
	s.says("subscription pneu-personal: created", "committed: personal is on (generation 3)")
	if s.watches("personal") != 1 {
		t.Fatalf("watched %d times: the worker was replaced", s.watches("personal"))
	}
	s.neverSays("Gmail's watch delivered its first notification")
	s.says("pneu kept the worker it already ran for it", "not that this watch sent them")
}

// --off when the worker outlasts the reload's join (JoinWait): the
// daemon answers an error, the CLI says pending and deletes nothing, and
// users.stop isn't called. Run again once the worker is done, the same
// off-pending generation is acknowledged and the rest follows.
func TestPushSeamOffJoinTimeout(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	if err := s.on("personal", mailAddr); err != nil {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.waitNudges("personal", 1)
	s.engine.setSlow(push.JoinWait + time.Second)
	s.f.Notify(mailAddr)
	s.eventually("a nudge under way", s.engine.nudging)

	s.out.Reset()
	err := s.e.off("personal")
	if err == nil || !strings.Contains(err.Error(), "personal is off in state.json, but the running pneu hasn't confirmed its worker is gone") ||
		!strings.Contains(err.Error(), "Nothing was deleted") {
		t.Fatalf("off: %v\n%s", err, s.out.String())
	}
	s.neverSays("Instant mail is off", "is stopped", "removed")
	if a, ok := d.ack(3); !ok || a.err == nil {
		t.Fatalf("off-pending's reload: %+v %v", a, ok)
	}
	if s.f.Calls(google.OpStop) != 0 || s.load().Accounts["personal"].State != state.OffPending {
		t.Fatalf("stops %d, state %+v", s.f.Calls(google.OpStop), s.load().File)
	}
	if p := s.pushState("personal"); p.State != control.PushOff {
		t.Fatalf("draining worker reported as %+v", p)
	}

	s.eventually("the nudge done", func() bool { return !s.engine.nudging() })
	s.engine.setSlow(0)
	s.out.Reset()
	if err := s.e.off("personal"); err != nil {
		t.Fatalf("off again: %v\n%s", err, s.out.String())
	}
	s.says("personal is off-pending already: retrying Gmail's cleanup", "committed: personal removed (generation 4)", "Instant mail is off for personal.")
	a, ok := d.ack(3)
	if !ok || a.r != control.ReloadApplied || a.workers != 0 {
		t.Fatalf("off-pending's reload again: %+v %v", a, ok)
	}
	if stop := s.requestIndex(google.OpStop, 0); stop < a.reqs || s.f.Stops(mailAddr) != 1 {
		t.Fatalf("users.stop at %d, ack at %d, %d stops", stop, a.reqs, s.f.Stops(mailAddr))
	}
}

// A daemon on its way out: the socket closed, the manager still holding
// daemon.lock. The CLI can't hand over and can't take the lock: pending.
// The next daemon runs what was committed.
func TestPushSeamDaemonStopping(t *testing.T) {
	s := newSeam(t, "personal", mailAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	d.srv.Close()
	s.out.Reset()
	err := s.on("personal", mailAddr)
	if err == nil || !strings.Contains(err.Error(), "its push sync holds its lock, but its control socket didn't take the reload") {
		t.Fatalf("on: %v\n%s", err, s.out.String())
	}
	s.neverSays("pneu isn't running", "Instant mail is on")
	d.stop()
	s.startDaemon()
	s.delivering("personal", 2)
	s.waitNudges("personal", 1)
}

// The owner's grant revoked: every worker reports reauth (owner), which
// the CLI's wait would stop on; plain push init notices the dead grant,
// consents, and its hand-over brings every worker back.
func TestPushSeamOwnerReauth(t *testing.T) {
	const workAddr = "work@example.com"
	s := newSeam(t, "personal", mailAddr, "work", workAddr)
	d := s.startDaemon()
	d.ready()
	if err := s.firstInit(); err != nil {
		t.Fatal(err)
	}
	for name, addr := range map[string]string{"personal": mailAddr, "work": workAddr} {
		if err := s.on(name, addr); err != nil {
			t.Fatalf("on %s: %v\n%s", name, err, s.out.String())
		}
	}
	s.f.Revoke(ownerAddr)
	for _, name := range []string{"personal", "work"} {
		s.eventually(name+" reauth", func() bool {
			p := s.pushState(name)
			return p.State == control.PushReauth && p.Reason == control.ReasonOwnerReauth
		})
	}
	if n := workerGoroutines(); n != 0 {
		t.Fatalf("%d worker goroutines after the owner's reauth", n)
	}
	// account push says so instead of claiming anything.
	s.out.Reset()
	if err := s.on("personal", mailAddr); err == nil {
		t.Fatalf("on with the owner revoked:\n%s", s.out.String())
	}
	s.neverSays("Instant mail is on")

	s.out.Reset()
	if err := s.init(initParams{}); err != nil {
		t.Fatalf("init: %v\n%s", err, s.out.String())
	}
	s.says("the owner's stored grant ("+ownerAddr+") no longer works; asking again", "The running pneu has it.")
	gen := s.load().Generation
	for _, name := range []string{"personal", "work"} {
		s.delivering(name, gen)
		if s.watches(name) != 2 {
			t.Fatalf("%s watched %d times", name, s.watches(name))
		}
	}
}
