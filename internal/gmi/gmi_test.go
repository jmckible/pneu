package gmi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// entry is one line of the fake gmi's log.
type entry struct {
	phase string // start | end
	at    time.Time
	args  string
	cwd   string
	nmc   string
}

type fixture struct {
	t    *testing.T
	log  string
	gmi  string
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gmi, err := filepath.Abs("../../testdata/fakegmi/gmi")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := &fixture{t: t, log: filepath.Join(root, "gmi.log"), gmi: gmi, root: root}
	t.Setenv("FAKEGMI_LOG", f.log)
	t.Setenv("FAKEGMI_SLEEP", "0")
	t.Setenv("FAKEGMI_EXIT", "0")
	t.Setenv("FAKEGMI_OUTPUT", "")
	return f
}

// account makes <root>/mail/<name>/gmail, so the default lock is
// <root>/mail/<name>/.gmi.lock as on the real machine.
func (f *fixture) account(name string) Account {
	dir := filepath.Join(f.root, "mail", name, "gmail")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	// An initialized, authorized repository with a completed first pull, as
	// lieer records them; without these the engine doesn't sync (see
	// TestOnboardingStatesSkipped and onboard_test.go).
	for file, body := range map[string]string{
		".gmailieer.json":             `{"account": "` + name + `@example.com"}`,
		".credentials.gmailieer.json": `{}`,
		".state.gmailieer.json":       `{"last_historyId": 42, "lastmod": 7}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	return Account{Name: name, GmiDir: dir, NotmuchConfig: filepath.Join(f.root, name+"-notmuch-config")}
}

// An account that isn't initialized or authorized runs nothing, and says why.
func TestOnboardingStatesSkipped(t *testing.T) {
	f := newFixture(t)
	a := f.account("personal")
	for _, c := range []struct {
		remove string
		state  State
		err    error
	}{
		{".credentials.gmailieer.json", StateUnauthorized, ErrNotAuthorized},
		{".gmailieer.json", StateUnconfigured, ErrNotConfigured},
	} {
		os.Remove(filepath.Join(a.GmiDir, c.remove))
		var logs []string
		e, err := New([]Account{a}, Options{Interval: 20 * time.Millisecond, PushDebounce: time.Millisecond, GmiPath: f.gmi,
			Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		e.RequestPush("personal")
		e.Run(ctx)
		cancel()
		if n := len(f.entries()); n != 0 {
			t.Fatalf("%s: gmi ran %d times", c.state, n)
		}
		st, _ := e.Status("personal")
		if st.State != c.state || !errors.Is(st.LastErr, c.err) || st.Failures != 0 {
			t.Fatalf("%s: status %+v", c.state, st)
		}
		if len(logs) != 1 {
			t.Fatalf("%s: expected one log line, got %d: %v", c.state, len(logs), logs)
		}
	}
}

// Status reads the repository itself, so it is right before Run's first
// pass and the moment a manual pull completes.
func TestStatusPulled(t *testing.T) {
	f := newFixture(t)
	a := f.account("personal")
	e, err := New([]Account{a}, Options{GmiPath: f.gmi})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status("personal"); !st.Pulled || st.LastErr != nil || st.State != StateReady {
		t.Fatalf("pulled account: %+v", st)
	}
	os.Remove(filepath.Join(a.GmiDir, ".state.gmailieer.json"))
	if st, _ := e.Status("personal"); st.Pulled || st.State != StateNeedsPull {
		t.Fatalf("unpulled account: %+v", st)
	}
}

func (f *fixture) entries() []entry {
	f.t.Helper()
	fh, err := os.Open(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	var out []entry
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) != 5 {
			continue // partial line mid-write
		}
		ns, err := strconv.ParseInt(p[1], 10, 64)
		if err != nil {
			f.t.Fatalf("bad log line %q", sc.Text())
		}
		out = append(out, entry{p[0], time.Unix(0, ns), p[2], p[3], p[4]})
	}
	return out
}

// runs pairs start/end lines per (cwd, op) in log order.
type run struct {
	op, cwd    string
	start, end time.Time // end zero if killed or still running
}

func (f *fixture) runs() []run {
	var rs []run
	open := map[string]int{}
	for _, e := range f.entries() {
		k := e.cwd + "\x00" + e.args
		if e.phase == "start" {
			open[k] = len(rs)
			rs = append(rs, run{op: e.args, cwd: e.cwd, start: e.at})
		} else if i, ok := open[k]; ok {
			rs[i].end = e.at
			delete(open, k)
		}
	}
	return rs
}

func (f *fixture) count(op string) int {
	n := 0
	for _, r := range f.runs() {
		if r.op == op {
			n++
		}
	}
	return n
}

// pushes are the fake's runs after the first. A push runs `gmi sync` (see
// OpPush), and with the fixture's hour-long interval every sync after the
// initial one is a push, in tests that don't call SyncNow.
func (f *fixture) pushes() []run {
	f.t.Helper()
	rs := f.runs()
	if len(rs) == 0 {
		return nil
	}
	for _, r := range rs {
		if r.op != "sync" {
			f.t.Fatalf("runs = %+v, want only gmi sync", rs)
		}
	}
	return rs[1:]
}

func (f *fixture) opts(o Options) Options {
	o.GmiPath = f.gmi
	o.Logf = f.t.Logf
	if o.Interval == 0 {
		o.Interval = time.Hour
	}
	if o.PushDebounce == 0 {
		o.PushDebounce = 50 * time.Millisecond
	}
	return o
}

func start(t *testing.T, e *Engine) (cancel func(), done <-chan struct{}) {
	ctx, c := context.WithCancel(context.Background())
	ch := make(chan struct{})
	go func() { e.Run(ctx); close(ch) }()
	t.Cleanup(func() { c(); <-ch })
	return c, ch
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustNew(t *testing.T, accts []Account, o Options) *Engine {
	t.Helper()
	e, err := New(accts, o)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// assertSerialized checks that no two runs in the same cwd overlap.
func assertSerialized(t *testing.T, rs []run) {
	t.Helper()
	last := map[string]run{}
	for _, r := range rs {
		if p, ok := last[r.cwd]; ok && (p.end.IsZero() || r.start.Before(p.end)) {
			t.Errorf("%s %s started at %v before %s ended at %v", r.cwd, r.op, r.start, p.op, p.end)
		}
		last[r.cwd] = r
	}
}

func TestSyncEnvAndTicker(t *testing.T) {
	f := newFixture(t)
	a := f.account("personal")
	e := mustNew(t, []Account{a}, f.opts(Options{Interval: 100 * time.Millisecond}))
	start(t, e)
	waitFor(t, 3*time.Second, "4 syncs", func() bool { return f.count("sync") >= 4 })

	for _, en := range f.entries() {
		if en.cwd != a.GmiDir || en.nmc != a.NotmuchConfig || en.args != "sync" {
			t.Fatalf("bad invocation %+v", en)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, "mail", "personal", ".gmi.lock")); err != nil {
		t.Errorf("default lock path not used: %v", err)
	}
	st, _ := e.Status("personal")
	if st.LastSync.IsZero() || st.Failures != 0 || st.LastErr != nil {
		t.Errorf("status %+v", st)
	}
}

func TestPushDebounceCollapses(t *testing.T) {
	f := newFixture(t)
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{PushDebounce: 150 * time.Millisecond}))
	start(t, e)
	waitFor(t, 2*time.Second, "initial sync", func() bool { st, _ := e.Status("personal"); return !st.LastSync.IsZero() })

	var last time.Time
	for range 3 {
		last = time.Now()
		if err := e.RequestPush("personal"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	waitFor(t, 2*time.Second, "push", func() bool { return len(f.pushes()) >= 1 })
	time.Sleep(400 * time.Millisecond)
	if n := len(f.pushes()); n != 1 {
		t.Fatalf("pushes = %d, want 1", n)
	}
	for _, r := range f.pushes() {
		if r.start.Before(last.Add(150 * time.Millisecond)) {
			t.Errorf("push at %v, before debounce after last request %v", r.start, last)
		}
	}
	if err := e.RequestPush("nope"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("unknown account: %v", err)
	}
}

func TestPushWaitsForSync(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", "0.5")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 2*time.Second, "sync start", func() bool { return len(f.entries()) > 0 })
	// Two requests while the sync runs, debounce expiring mid-sync: one push, after.
	e.RequestPush("personal")
	time.Sleep(60 * time.Millisecond)
	e.RequestPush("personal")
	waitFor(t, 3*time.Second, "push end", func() bool {
		ps := f.pushes()
		return len(ps) > 0 && !ps[0].end.IsZero()
	})
	time.Sleep(200 * time.Millisecond)
	rs := f.runs()
	if len(rs) != 2 || len(f.pushes()) != 1 {
		t.Fatalf("runs = %+v, want sync then one push", rs)
	}
	assertSerialized(t, rs)
}

func TestAccountsRunInParallel(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", "0.5")
	e := mustNew(t, []Account{f.account("personal"), f.account("work")}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 3*time.Second, "both syncs", func() bool {
		n := 0
		for _, r := range f.runs() {
			if !r.end.IsZero() {
				n++
			}
		}
		return n == 2
	})
	rs := f.runs()
	if rs[0].cwd == rs[1].cwd {
		t.Fatalf("same account twice: %+v", rs)
	}
	if !rs[1].start.Before(rs[0].end) {
		t.Errorf("accounts serialized: %+v", rs)
	}
}

func TestWaitsForHeldFlock(t *testing.T) {
	f := newFixture(t)
	a := f.account("personal")
	a.LockPath = filepath.Join(f.root, "custom.lock")
	lf, err := os.OpenFile(a.LockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	e := mustNew(t, []Account{a}, f.opts(Options{}))
	start(t, e)
	time.Sleep(300 * time.Millisecond)
	if n := len(f.entries()); n != 0 {
		t.Fatalf("gmi ran while the flock was held (%d log lines)", n)
	}
	if st, _ := e.Status("personal"); !st.Running {
		t.Errorf("waiting on lock should report Running")
	}
	released := time.Now()
	syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	waitFor(t, 2*time.Second, "sync after release", func() bool { return f.count("sync") == 1 })
	if r := f.runs()[0]; r.start.Before(released) {
		t.Errorf("sync started %v before release %v", r.start, released)
	}
}

func TestFailureBacksOffAndRecovers(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_EXIT", "1")
	var mu sync.Mutex
	var synced []Result
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{
		Interval:   80 * time.Millisecond,
		MaxBackoff: 400 * time.Millisecond,
		OnSynced:   func(_ string, r Result) { mu.Lock(); synced = append(synced, r); mu.Unlock() },
	}))
	start(t, e)
	// Starts at 0, +160, +320, +400(cap), +400 ...
	waitFor(t, 3*time.Second, "5 failed syncs", func() bool { return f.count("sync") >= 5 })
	rs := f.runs()
	for i, want := range []time.Duration{160, 320, 400, 400} {
		gap := rs[i+1].start.Sub(rs[i].end)
		want *= time.Millisecond
		if gap < want-20*time.Millisecond || gap > want+150*time.Millisecond {
			t.Errorf("gap %d = %v, want ~%v", i, gap, want)
		}
	}
	st, _ := e.Status("personal")
	if st.Failures < 4 || st.LastErr == nil || !strings.Contains(st.LastErr.Error(), "exit 1: fakegmi: failing with 1") {
		t.Fatalf("status %+v", st)
	}
	mu.Lock()
	if len(synced) == 0 {
		t.Errorf("OnSynced not called on failure")
	}
	for _, r := range synced {
		if r.Err == nil {
			t.Errorf("failed run reported without its error: %+v", r)
		}
	}
	mu.Unlock()

	t.Setenv("FAKEGMI_EXIT", "0")
	e.SyncNow("personal")
	waitFor(t, 2*time.Second, "recovery", func() bool { st, _ := e.Status("personal"); return st.Failures == 0 })
	st, _ = e.Status("personal")
	if st.LastErr != nil || st.LastSync.IsZero() {
		t.Errorf("status after recovery %+v", st)
	}
	waitFor(t, 2*time.Second, "OnSynced after recovery", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(synced) > 0 && synced[len(synced)-1].Err == nil
	})
	mu.Lock()
	defer mu.Unlock()
	last := synced[len(synced)-1]
	if last.ExitCode != 0 || last.Op != OpSync {
		t.Errorf("OnSynced after recovery: %+v", last)
	}
}

func TestHungGmiKilledAtTimeout(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", "30")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{SyncTimeout: 300 * time.Millisecond}))
	begin := time.Now()
	start(t, e)
	waitFor(t, 5*time.Second, "timeout failure", func() bool { st, _ := e.Status("personal"); return st.Failures == 1 })
	if el := time.Since(begin); el > 3*time.Second {
		t.Errorf("kill took %v", el)
	}
	st, _ := e.Status("personal")
	if !errors.Is(st.LastErr, context.DeadlineExceeded) {
		t.Errorf("LastErr = %v", st.LastErr)
	}
	if rs := f.runs(); len(rs) != 1 || !rs[0].end.IsZero() {
		t.Errorf("runs %+v, want one killed run", rs)
	}
}

func TestShutdownWaitsForInFlight(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", "0.6")
	a := f.account("personal")
	e := mustNew(t, []Account{a}, f.opts(Options{}))
	cancel, done := start(t, e)
	waitFor(t, 2*time.Second, "sync start", func() bool { return len(f.entries()) > 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run didn't return")
	}
	rs := f.runs()
	if len(rs) != 1 || rs[0].end.IsZero() {
		t.Fatalf("Run returned before gmi finished: %+v", rs)
	}
	lf, err := os.OpenFile(filepath.Join(f.root, "mail", "personal", ".gmi.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("flock still held after shutdown: %v", err)
	}
}

func TestShutdownWhileWaitingOnLock(t *testing.T) {
	f := newFixture(t)
	a := f.account("personal")
	lock := filepath.Join(f.root, "mail", "personal", ".gmi.lock")
	lf, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	syscall.Flock(int(lf.Fd()), syscall.LOCK_EX)
	e := mustNew(t, []Account{a}, f.opts(Options{}))
	cancel, done := start(t, e)
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked on a held flock after cancel")
	}
	if st, _ := e.Status("personal"); st.Failures != 0 || st.Running {
		t.Errorf("status %+v", st)
	}
	if len(f.entries()) != 0 {
		t.Error("gmi ran")
	}
}

func TestChanged(t *testing.T) {
	f := newFixture(t)
	// lieer 1.6's output for a no-op `gmi sync` (non-TTY, not --quiet).
	t.Setenv("FAKEGMI_OUTPUT", "push: everything is up-to-date.\npull: partial synchronization.. (hid: 42)\npull: everything is up-to-date.\ncurrent historyId: 42")
	got := make(chan Result, 4)
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{OnSynced: func(_ string, r Result) { got <- r }}))
	start(t, e)
	r := <-got
	if r.Changed || r.Op != OpSync || r.Account != "personal" || !strings.Contains(r.Output, "current historyId: 42") {
		t.Errorf("no-op sync: %+v", r)
	}
	t.Setenv("FAKEGMI_OUTPUT", "pull: partial synchronization.. (hid: 42)\nfetching changes ...\nreceiving content (1) ...\ncurrent historyId: 43")
	e.SyncNow("personal")
	if r := <-got; !r.Changed {
		t.Errorf("sync with changes: %+v", r)
	}
	t.Setenv("FAKEGMI_OUTPUT", "push: nothing to push\nremote historyId: 43")
	e.RequestPush("personal")
	if r := <-got; r.Changed || r.Op != OpPush {
		t.Errorf("no-op push: %+v", r)
	}
}

// A push's pull reports a change only if it brought more than the labels
// the push sent. lieer 1.6 non-TTY output (nobar.py).
func TestPushPulled(t *testing.T) {
	push := "receiving metadata (2) ...done: 2 its in 00.301s\nresolving changes (2) ...done: 2 its in 00.000s\n" +
		"pushing, 0 changed (2) ...done: 2 its in 00.402s\nremote historyId: 7010\n" +
		"pull: partial synchronization.. (hid: 7000)\nfetching changes ...done: 1 its in 00.2s\nresolving changes (3) ...done: 3 its in 00.0s\n"
	for _, c := range []struct {
		out  string
		want bool
	}{
		{push + "updating tags (0) (2) ...done: 2 its in 00.1s\ncurrent historyId: 7010", false},
		{push + "updating tags (0) (3) ...done: 3 its in 00.1s\ncurrent historyId: 7011", true},
		{push + "receiving content (1) ...done: 1 its in 00.3s\nupdating tags (0) (2) ...done: 2 its in 00.1s", true},
		{push + "removing messages (1) ...done: 1 its in 00.0s\nupdating tags (0) (2) ...done: 2 its in 00.1s", true},
		{"push: everything is up-to-date.\npull: partial synchronization.. (hid: 7000)\npull: everything is up-to-date.", false},
		{"push: everything is up-to-date.\npull: partial synchronization.. (hid: 7000)\nfetching changes ...done: 1 its in 00.2s\nupdating tags (0) (1) ...done: 1 its in 00.1s", true},
	} {
		if got := pushPulled(c.out); got != c.want {
			t.Errorf("pushPulled = %v, want %v:\n%s", got, c.want, c.out)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 8}
	tb.Write([]byte("abcdef"))
	tb.Write([]byte("ghij"))
	if tb.String() != "cdefghij" {
		t.Errorf("got %q", tb.String())
	}
	tb.Write([]byte("0123456789"))
	if tb.String() != "23456789" {
		t.Errorf("got %q", tb.String())
	}
}

// A sync cleans mail/tmp too: a resumed pull records its history id before
// its closing partial pull, so debris from that pull meets syncs, not a
// first pull.
func TestSyncCleansTmp(t *testing.T) {
	f := newFixture(t)
	a := f.account("personal")
	tmp := filepath.Join(a.GmiDir, "mail", "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(tmp, "0123456789abcdef:2,S")
	os.WriteFile(orphan, []byte("x"), 0o644)
	e := mustNew(t, []Account{a}, f.opts(Options{}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()
	waitFor(t, 5*time.Second, "a sync", func() bool { return f.count("sync") == 1 })
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("sync ran with an orphan in mail/tmp")
	}
}
