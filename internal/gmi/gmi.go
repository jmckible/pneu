// Package gmi is pneu's sync engine: it owns every unattended lieer (`gmi`)
// run. Per account, one goroutine runs `gmi sync` on a ticker, a debounced
// `gmi push` after tag changes, and on-demand syncs. Every invocation for an
// account is serialized by an in-process mutex and by an flock on the
// account's lock file, the same file manual `pneu gmi` runs wait on.
// lieer's own .lock fails fast rather than waiting, which is why the flock
// exists at all.
package gmi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
)

// Account is one lieer repository and the notmuch database it syncs into.
type Account struct {
	Name          string
	GmiDir        string // lieer dir; gmi's cwd
	NotmuchConfig string // NOTMUCH_CONFIG for this account's database
	LockPath      string // default: <parent of GmiDir>/.gmi.lock
	// ClientSecret is the OAuth client JSON re-auth passes to `gmi auth -c`;
	// default: client_secret.json beside NotmuchConfig (INSTALL.md's layout).
	ClientSecret string
}

// Options tune the engine. Zero values take the defaults noted.
type Options struct {
	Interval     time.Duration // sync period, default 2m
	PushDebounce time.Duration // push fires this long after the last RequestPush, default 3s
	GmiPath      string        // default "gmi"
	SyncTimeout  time.Duration // per invocation, default 10m; then killed
	PushTimeout  time.Duration // per invocation, default 2m; then killed
	MaxBackoff   time.Duration // cap on the failure-backoff sync delay, default 15m
	// StallTimeout kills a first pull that has printed nothing for this
	// long, default 10m. A first pull has no overall timeout: it runs for
	// hours on a large mailbox, and lieer's own HTTP timeout setting is never
	// applied (remote.py:Remote.authorize), so a hung socket would hang it.
	StallTimeout time.Duration
	// AuthTimeout ends a re-auth nobody finished, default 10m
	// (run_local_server waits forever).
	AuthTimeout time.Duration

	// OnStart is called from the account's goroutine when a scheduled or
	// requested sync or push is about to run (it may still wait on the
	// account's flock). OnSynced follows after every such run, success or
	// failure (Result.Err), so a watcher always sees the run end. Keep both
	// quick (an SSE broadcast); they delay the next run.
	OnStart  func(account string, op Op)
	OnSynced func(account string, r Result)
	// OnProgress reports a running first pull, at most once a second and
	// on every phase change. OnAuth reports how a re-auth ended (nil: the
	// account works again). Both are called from engine goroutines.
	OnProgress func(account string, p Progress)
	OnAuth     func(account string, err error)
	Logf       func(format string, args ...any) // default log.Printf
}

// Op names the lieer subcommand run.
type Op string

const (
	OpSync Op = "sync"
	OpPush Op = "push"
	OpSend Op = "send"
	OpPull Op = "pull" // the first pull only; later pulls are part of sync
	OpAuth Op = "auth"
)

// Result is one gmi invocation.
type Result struct {
	Account  string
	Op       Op
	Started  time.Time
	Duration time.Duration
	ExitCode int    // -1 if gmi didn't start or was killed
	Output   string // combined stdout+stderr, last 8KB
	Err      error
	// Changed is false only when lieer said nothing happened: for a sync,
	// "pull: everything is up-to-date."; for a push, "push: everything is
	// up-to-date." or "push: nothing to push". A sync that pulled only the
	// labels we just pushed still reports true.
	Changed bool
}

// Status is an account's sync health.
type Status struct {
	LastSync time.Time // completion of the last successful sync
	LastPush time.Time // completion of the last successful push
	LastErr  error     // nil after any success
	Failures int       // consecutive failed invocations
	Running  bool      // a gmi run is in flight or waiting on the lock
	Pulled   bool      // lieer's state records a completed pull (true mid-way through a resumed one; see State)
	State    State
	Progress *Progress // the first pull's, while State is StatePulling
	Authing  bool      // a re-auth is waiting on the consent screen
}

// ErrUnknownAccount is returned for an account name New wasn't given.
var ErrUnknownAccount = errors.New("gmi: unknown account")

// ErrBusy wraps the context error when Exec or Send gave up waiting for the
// account lock (a sync or push held it): gmi never started, nothing was sent.
var ErrBusy = errors.New("gmi: account busy")

// pulled reports whether lieer's state file records a completed pull
// (last_historyId > 0). lieer writes it only at the end of a full pull.
func (a *Account) pulled() bool {
	b, err := os.ReadFile(filepath.Join(a.GmiDir, ".state.gmailieer.json"))
	if err != nil {
		return false
	}
	var st struct {
		LastHistoryID json.Number `json:"last_historyId"`
	}
	if json.Unmarshal(b, &st) != nil {
		return false
	}
	n, err := st.LastHistoryID.Int64()
	return err == nil && n > 0
}

// TouchTag is the throwaway tag that re-marks messages for push (see
// NoteWrite). Each lieer repository must ignore it:
// `gmi set --ignore-tags-local pneu-touch`.
const TouchTag = "pneu-touch"

// noteGrace is how long after a sync's gmi exits a NoteWrite still counts
// as inside it: the tag write finished before gmi did, but its NoteWrite
// call came after. A var so tests can shorten it.
var noteGrace = 2 * time.Second

const outputLimit = 8 << 10

type account struct {
	Account

	run     chan struct{} // cap 1: serializes gmi invocations; a mutex a waiter can abandon
	syncReq chan struct{} // cap 1: pending SyncNow requests collapse
	pushDue chan struct{} // cap 1: debounced push ready; pending pushes collapse

	pmu       sync.Mutex
	pushTimer *time.Timer

	touchDue chan struct{} // cap 1: a NoteWrite landed just after a sync ended

	nmu     sync.Mutex
	syncing bool                // a sync holds the slot
	syncEnd time.Time           // when the last sync's gmi exited
	pending map[string]struct{} // Message-IDs written during a sync, to re-mark

	smu    sync.Mutex
	status Status
	// The first pull's progress while it runs (nil otherwise), and whether
	// a re-auth is waiting on the consent screen; under smu.
	pulling    *progressWriter
	authing    bool
	authCancel context.CancelFunc
}

// Engine runs lieer for a set of accounts. Accounts are independent.
type Engine struct {
	opts  Options
	accts map[string]*account
	order []*account

	// Runs outside the account loops (a re-auth's gmi) take their context
	// from Run's and Run waits for them; under mu.
	mu      sync.Mutex
	runCtx  context.Context
	stopped bool
	aux     sync.WaitGroup
}

// startAux registers a run outside the loops: its base context, and ok
// false once Run has returned. The caller calls e.aux.Done when it ends.
func (e *Engine) startAux() (context.Context, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil, false
	}
	e.aux.Add(1)
	if e.runCtx == nil {
		return context.Background(), true
	}
	return e.runCtx, true
}

// New validates accounts and fills in defaults. Call Run to start syncing;
// RequestPush and SyncNow may be called before Run and take effect once it
// starts.
func New(accounts []Account, opts Options) (*Engine, error) {
	if len(accounts) == 0 {
		return nil, errors.New("gmi: no accounts")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&opts.Interval, 2*time.Minute)
	def(&opts.PushDebounce, 3*time.Second)
	def(&opts.SyncTimeout, 10*time.Minute)
	def(&opts.PushTimeout, 2*time.Minute)
	def(&opts.MaxBackoff, 15*time.Minute)
	def(&opts.StallTimeout, 10*time.Minute)
	def(&opts.AuthTimeout, 10*time.Minute)
	if opts.GmiPath == "" {
		opts.GmiPath = "gmi"
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	e := &Engine{opts: opts, accts: map[string]*account{}}
	for _, a := range accounts {
		if a.Name == "" || a.GmiDir == "" {
			return nil, fmt.Errorf("gmi: account %q needs a name and GmiDir", a.Name)
		}
		if _, dup := e.accts[a.Name]; dup {
			return nil, fmt.Errorf("gmi: duplicate account %q", a.Name)
		}
		if a.LockPath == "" {
			a.LockPath = DefaultLockPath(a.GmiDir)
		}
		if a.ClientSecret == "" && a.NotmuchConfig != "" {
			a.ClientSecret = filepath.Join(filepath.Dir(a.NotmuchConfig), "client_secret.json")
		}
		ac := &account{Account: a, run: make(chan struct{}, 1), syncReq: make(chan struct{}, 1),
			pushDue: make(chan struct{}, 1), touchDue: make(chan struct{}, 1)}
		e.accts[a.Name] = ac
		e.order = append(e.order, ac)
	}
	return e, nil
}

// Run syncs every account immediately, then every Interval (backing off after
// failures), until ctx ends. On cancel it lets an in-flight gmi finish or hit
// its timeout, releases the flocks, and returns ctx.Err().
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	e.runCtx = ctx
	e.mu.Unlock()
	var wg sync.WaitGroup
	for _, a := range e.order {
		wg.Go(func() { e.loop(ctx, a) })
	}
	wg.Wait()
	// A re-auth's gmi stops with ctx (SIGINT); wait so it doesn't outlive
	// the server holding port 8080.
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	e.aux.Wait()
	for _, a := range e.order {
		a.pmu.Lock()
		if a.pushTimer != nil {
			a.pushTimer.Stop()
		}
		a.pmu.Unlock()
	}
	return ctx.Err()
}

// RequestPush schedules a `gmi push` PushDebounce after the last request.
// Call it after every successful notmuch tag change. Requests that land while
// a sync or push is running queue behind it; queued pushes collapse to one.
func (e *Engine) RequestPush(account string) error {
	a, ok := e.accts[account]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	a.pmu.Lock()
	defer a.pmu.Unlock()
	if a.pushTimer == nil {
		a.pushTimer = time.AfterFunc(e.opts.PushDebounce, func() { signal(a.pushDue) })
	} else {
		a.pushTimer.Reset(e.opts.PushDebounce)
	}
	return nil
}

// NoteWrite records that pneu just changed tags on ids in the account's
// notmuch database. Call it after every successful write, before or after
// RequestPush.
//
// Why: lieer pushes the messages whose notmuch lastmod is above its stored
// lastmod, and a full pull (first run, --force, or an expired historyId)
// ends by setting that stored lastmod to the database revision at the end of
// the pull. A tag written while such a sync runs sits at or below it, and the
// next push reports "everything is up-to-date" and skips it. So ids written
// while the account's sync holds the slot (or within noteGrace after its gmi
// exited) are collected, and when the sync ends they are re-marked with
// +TouchTag then -TouchTag, which bumps their lastmod, and a push follows.
// Writes outside a sync are above lastmod already and are ignored here.
func (e *Engine) NoteWrite(account string, ids []string) {
	a, ok := e.accts[account]
	if !ok || len(ids) == 0 || a.NotmuchConfig == "" {
		return
	}
	a.nmu.Lock()
	late := !a.syncing && !a.syncEnd.IsZero() && time.Since(a.syncEnd) < noteGrace
	if a.syncing || late {
		a.addPendingLocked(ids)
	}
	a.nmu.Unlock()
	if late {
		signal(a.touchDue)
	}
}

func (a *account) addPendingLocked(ids []string) {
	if a.pending == nil {
		a.pending = map[string]struct{}{}
	}
	for _, id := range ids {
		a.pending[id] = struct{}{}
	}
}

// takePending empties the pending set; end also closes the sync window.
func (a *account) takePending(end bool) []string {
	a.nmu.Lock()
	defer a.nmu.Unlock()
	if end {
		a.syncing = false
		a.syncEnd = time.Now()
	}
	ids := make([]string, 0, len(a.pending))
	for id := range a.pending {
		ids = append(ids, id)
	}
	a.pending = nil
	return ids
}

func (a *account) setSyncing(v bool) {
	a.nmu.Lock()
	a.syncing = v
	a.nmu.Unlock()
}

// touch re-marks ids for push (see NoteWrite). The caller holds the account
// slot and flock, so no push, ours or a manual one, runs between the two
// writes and the throwaway tag never reaches Gmail even if a repository
// forgot to ignore it. On failure the ids go back to pending for the next
// sync's end.
func (e *Engine) touch(a *account, ids []string) {
	if len(ids) == 0 {
		return
	}
	nm := notmuch.Account{Name: a.Name, ConfigPath: a.NotmuchConfig}
	ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
	defer cancel()
	err := nm.Tag(ctx, []string{"+" + TouchTag}, ids)
	if err == nil {
		err = nm.Tag(ctx, []string{"-" + TouchTag}, ids)
	}
	if err != nil {
		a.nmu.Lock()
		a.addPendingLocked(ids)
		a.nmu.Unlock()
		e.opts.Logf("gmi: sync [%s]: re-marking %d message(s) tagged during the sync: %v (retried after the next sync)", a.Name, len(ids), err)
		return
	}
	e.opts.Logf("gmi: sync [%s]: re-marked %d message(s) tagged during the sync for push", a.Name, len(ids))
	e.RequestPush(a.Name)
}

// touchTimeout bounds both touch writes together (the Xapian lock wait).
const touchTimeout = 30 * time.Second

// touchLate handles a NoteWrite that came just after a sync ended: take the
// slot and flock like any run, then re-mark.
func (e *Engine) touchLate(ctx context.Context, a *account) {
	select {
	case a.run <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-a.run }()
	unlock, err := flockFile(ctx, a.LockPath, nil)
	if err != nil {
		return // pending kept; the next sync's end re-marks it
	}
	defer unlock()
	e.touch(a, a.takePending(false))
}

// SyncNow queues an immediate sync (after any in-flight run). Repeated calls
// before it starts collapse to one. The periodic timer restarts after it.
func (e *Engine) SyncNow(account string) error {
	a, ok := e.accts[account]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	signal(a.syncReq)
	return nil
}

// Exec runs `gmi args...` in the account's lieer dir with stdin, serialized
// with sync and push through the same mutex and flock, and bounded by
// PushTimeout once started. ctx bounds only the wait for the lock: a run that
// has started is never interrupted (a send cut off mid-flight may still have
// gone out). It doesn't touch Status's success/failure counters or call
// OnSynced. The error is Result.Err, or ErrBusy wrapping ctx's error if ctx
// ended before gmi started.
func (e *Engine) Exec(ctx context.Context, account string, stdin io.Reader, args ...string) (Result, error) {
	a, ok := e.accts[account]
	if !ok {
		return Result{}, fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	if len(args) == 0 {
		return Result{}, errors.New("gmi: Exec needs a subcommand")
	}
	r, ok := e.exec(ctx, a, Op(args[0]), args, stdin, e.opts.PushTimeout)
	if !ok {
		return Result{Account: account, Op: Op(args[0]), ExitCode: -1}, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}
	return r, r.Err
}

// Send sends one RFC 822 message with `gmi send -t` (recipients from its
// To/Cc/Bcc). It must hold the account lock like any other run: lieer's send
// takes the repository lock and opens the notmuch database read-write to
// store the sent copy. On success it queues a sync so the view refreshes.
func (e *Engine) Send(ctx context.Context, account string, rfc822 io.Reader) (Result, error) {
	r, err := e.Exec(ctx, account, rfc822, string(OpSend), "-t")
	if err == nil {
		e.SyncNow(account)
	}
	return r, err
}

// Status reports an account's sync health and onboarding state. The
// repository's files are read fresh rather than waiting for the loop's next
// run to notice a change made by hand.
func (e *Engine) Status(account string) (Status, error) {
	a, ok := e.accts[account]
	if !ok {
		return Status{}, fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	files := FileState(a.GmiDir)
	a.smu.Lock()
	st := a.status
	var pw *progressWriter = a.pulling
	st.Authing = a.authing
	a.smu.Unlock()
	// lieer's own record, whatever else the files say: a pulled account
	// whose credentials an abandoned re-auth deleted is still pulled.
	st.Pulled = a.pulled()
	st.State = files
	switch {
	case pw != nil:
		p := pw.snapshot()
		st.State, st.Progress = StatePulling, &p
	case errors.Is(st.LastErr, ErrReauth) && files != StateUnconfigured:
		// Unauthorized too: an abandoned re-auth has already deleted the
		// dead credentials (gmi auth -f), and the way back is the same.
		st.State = StateReauth
	}
	return st, nil
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (e *Engine) loop(ctx context.Context, a *account) {
	e.do(ctx, a, OpSync)
	timer := time.NewTimer(e.delay(a))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.pushDue:
			e.do(ctx, a, OpPush)
			continue // a push doesn't restart the sync period
		case <-a.touchDue:
			e.touchLate(ctx, a)
			continue
		case <-timer.C:
		case <-a.syncReq:
		}
		e.do(ctx, a, OpSync)
		timer.Reset(e.delay(a))
	}
}

// setupPoll is how often an account waiting on `pneu account add` or
// `pneu account auth` looks again, so its first pull starts soon after.
var setupPoll = 5 * time.Second

// delay is Interval doubled per consecutive failure, capped at MaxBackoff;
// setupPoll for an account that isn't set up yet.
func (e *Engine) delay(a *account) time.Duration {
	if st := FileState(a.GmiDir); st == StateUnconfigured || st == StateUnauthorized {
		return min(setupPoll, e.opts.Interval)
	}
	a.smu.Lock()
	n := a.status.Failures
	a.smu.Unlock()
	d := e.opts.Interval
	for i := 0; i < n && d < e.opts.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, max(e.opts.MaxBackoff, e.opts.Interval))
}

func (e *Engine) do(ctx context.Context, a *account, op Op) {
	if ctx.Err() != nil {
		return
	}
	switch st := FileState(a.GmiDir); st {
	case StateUnconfigured, StateUnauthorized:
		// Nothing to run until `pneu account add` / `pneu account auth`.
		err := ErrNotConfigured
		if st == StateUnauthorized {
			err = ErrNotAuthorized
		}
		a.smu.Lock()
		first := !errors.Is(a.status.LastErr, err)
		if errors.Is(a.status.LastErr, ErrReauth) {
			first = false // a re-auth in the app deleted them; it stays a re-auth
		} else {
			a.status.LastErr = err
		}
		a.smu.Unlock()
		if first {
			e.opts.Logf("gmi: [%s] %v", a.Name, err)
		}
		return
	case StateNeedsPull:
		if op != OpSync {
			return // pushes wait: the account is read-only until it's pulled
		}
		op = OpPull
	}
	if e.opts.OnStart != nil {
		e.opts.OnStart(a.Name, op)
	}
	var r Result
	var ok bool
	if op == OpPull {
		r, ok = e.firstPull(ctx, a)
	} else {
		r, ok = e.invoke(ctx, a, op)
	}
	if !ok {
		return // shut down while waiting for the lock or mid-pull; not a failure
	}
	a.smu.Lock()
	if r.Err != nil {
		a.status.Failures++
		a.status.LastErr = r.Err
	} else {
		a.status.Failures = 0
		a.status.LastErr = nil
		end := r.Started.Add(r.Duration)
		switch op {
		case OpSync:
			a.status.LastSync = end
		case OpPush:
			a.status.LastPush = end
		}
	}
	failures := a.status.Failures
	a.smu.Unlock()

	if r.Err != nil {
		e.opts.Logf("gmi: %v (%d consecutive failures, next sync in %v)", r.Err, failures, e.delay(a))
	}
	if e.opts.OnSynced != nil {
		e.opts.OnSynced(a.Name, r)
	}
	if op == OpPull && r.Err == nil {
		e.opts.Logf("gmi: [%s] first pull complete in %v", a.Name, r.Duration.Round(time.Second))
		signal(a.syncReq) // mail that arrived during the pull, at once
	}
}

func (a *account) setRunning(v bool) {
	a.smu.Lock()
	a.status.Running = v
	a.smu.Unlock()
}

// invoke runs one gmi op under the account mutex and flock. ok is false if
// ctx ended before gmi started.
func (e *Engine) invoke(ctx context.Context, a *account, op Op) (r Result, ok bool) {
	timeout := e.opts.SyncTimeout
	if op == OpPush {
		timeout = e.opts.PushTimeout
	}
	return e.exec(ctx, a, op, []string{string(op)}, nil, timeout)
}

// exec runs `gmi args...` with stdin under the account mutex and flock. ctx
// bounds only the wait for the lock; once gmi starts it runs to completion
// or timeout. ok is false if ctx ended before gmi started.
func (e *Engine) exec(ctx context.Context, a *account, op Op, args []string, stdin io.Reader, timeout time.Duration) (r Result, ok bool) {
	return e.run(ctx, a, op, args, runOpts{stdin: stdin, timeout: timeout})
}

// runOpts vary a gmi run.
type runOpts struct {
	stdin   io.Reader
	timeout time.Duration // 0: none
	// interrupt: ctx's end stops gmi with SIGINT (lieer unwinds as from a
	// Ctrl-C) instead of letting it finish; a run stopped that way returns
	// ok false, like a shutdown before it started.
	interrupt bool
	stall     time.Duration // kill gmi after this long with no output; 0: never
	out       *progressWriter
	before    func() // runs holding the slot and flock, before gmi starts
}

// errStalled is the cause recorded when the stall watchdog stops a run.
var errStalled = errors.New("stalled")

// run is exec with options; see runOpts.
func (e *Engine) run(ctx context.Context, a *account, op Op, args []string, o runOpts) (r Result, ok bool) {
	select {
	case a.run <- struct{}{}:
	case <-ctx.Done():
		return Result{}, false
	}
	defer func() { <-a.run }()
	a.setRunning(true)
	defer a.setRunning(false)
	if op == OpSync {
		// The NoteWrite window. On an early return it just closes; ids
		// already collected stay pending for the next sync's end.
		a.setSyncing(true)
		defer a.setSyncing(false)
	}

	unlock, err := flockFile(ctx, a.LockPath, func() {
		e.opts.Logf("gmi: %s [%s]: waiting for %s", op, a.Name, a.LockPath)
	})
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, false
		}
		return Result{Account: a.Name, Op: op, Started: time.Now(), ExitCode: -1,
			Err: fmt.Errorf("gmi %s [%s]: %w", op, a.Name, err)}, true
	}
	defer unlock()

	if op == OpSync {
		// sync pushes first, and lieer pushes everything since lastmod: a push
		// that came due before this point is covered. (If the sync fails,
		// lastmod doesn't advance and the next sync pushes it.)
		select {
		case <-a.pushDue:
		default:
		}
	}

	switch op {
	case OpSync, OpPull, OpSend: // the runs that store mail (see cleanTmp)
		e.cleanTmp(a)
	}
	if o.before != nil {
		o.before()
	}

	// Shutdown doesn't interrupt gmi unless asked: an in-flight run
	// finishes or hits its own timeout.
	base := context.WithoutCancel(ctx)
	if o.interrupt {
		base = ctx
	}
	cctx, cancel := context.WithCancelCause(base)
	defer cancel(nil)
	if o.timeout > 0 {
		var cancelT context.CancelFunc
		cctx, cancelT = context.WithTimeout(cctx, o.timeout)
		defer cancelT()
	}

	tail := &tailBuffer{max: outputLimit}
	var out io.Writer = tail
	if o.out != nil {
		out = io.MultiWriter(tail, o.out)
	}
	cmd := exec.CommandContext(cctx, e.opts.GmiPath, args...)
	cmd.Stdin = o.stdin
	cmd.Dir = a.GmiDir
	cmd.Env = append(os.Environ(), "NOTMUCH_CONFIG="+a.NotmuchConfig, "PYTHONUNBUFFERED=1")
	cmd.Stdout, cmd.Stderr = out, out
	// Own process group so the kill reaches children holding the pipes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if o.interrupt {
		// SIGINT is lieer's KeyboardInterrupt: its `with notmuch2.Database()`
		// blocks close, committing the batch in hand. WaitDelay then SIGKILLs.
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
		cmd.WaitDelay = 30 * time.Second
	}

	r = Result{Account: a.Name, Op: op, Started: time.Now(), ExitCode: -1}
	if err = cmd.Start(); err == nil {
		if o.stall > 0 && o.out != nil {
			go watchStall(cctx, cancel, o.out, o.stall, cmd.Process.Pid)
		}
		err = cmd.Wait()
	}
	r.Duration = time.Since(r.Started)
	r.Output = strings.ToValidUTF8(tail.String(), "")
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	switch {
	case errors.Is(context.Cause(cctx), errStalled):
		r.Err = fmt.Errorf("gmi %s [%s]: no output for %v; stopped: %w", op, a.Name, o.stall, ErrStalled)
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		r.Err = fmt.Errorf("gmi %s [%s]: killed after %v timeout: %w", op, a.Name, o.timeout, context.DeadlineExceeded)
	case o.interrupt && ctx.Err() != nil:
		return r, false // shut down mid-run
	case err != nil:
		msg := lastLine(r.Output)
		if msg == "" || r.ExitCode == -1 { // killed: its last line is whatever it was printing
			msg = err.Error()
		}
		r.Err = fmt.Errorf("gmi %s [%s]: exit %d: %s", op, a.Name, r.ExitCode, msg)
		if strings.Contains(r.Output, "invalid_grant") {
			r.Err = fmt.Errorf("%w: %w", ErrReauth, r.Err)
		}
	default:
		r.Changed = changed(op, r.Output)
	}
	if op == OpSync {
		// Success or failure: a failed full pull may still have set lastmod.
		e.touch(a, a.takePending(true))
	}
	return r, true
}

// Accepted reports whether a send's output shows Gmail took the message even
// though gmi failed: lieer 1.6 prints its "receiving content" progress bar
// (storing the sent copy) and then "message sent successfully" only after
// the API send returned. A failure after that point is the local copy's.
func (r Result) Accepted() bool {
	return r.Op == OpSend && (strings.Contains(r.Output, "message sent successfully") ||
		strings.Contains(r.Output, "receiving content"))
}

// changed reads lieer 1.6's (non-quiet, non-TTY) summary lines. See Result.Changed.
func changed(op Op, out string) bool {
	if op == OpPush {
		return !strings.Contains(out, "push: everything is up-to-date.") &&
			!strings.Contains(out, "push: nothing to push")
	}
	return !strings.Contains(out, "pull: everything is up-to-date.")
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// tailBuffer keeps the last max bytes written. exec serializes writes when
// Stdout and Stderr are the same comparable writer.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.b = append(t.b[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.b) }
