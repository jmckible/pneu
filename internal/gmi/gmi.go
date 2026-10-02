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
	"regexp"
	"slices"
	"strconv"
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
	Interval     time.Duration // sync period, default 30s
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
	// OpPush is the debounced push after pneu's own tag writes. It runs
	// `gmi sync`, not `gmi push`: lieer refuses to push a message whose Gmail
	// historyId is newer than its last pull, and pushing a message is such a
	// change, so a second action on a message before the next pull (read,
	// then trash) would be refused and the pull would put the labels back.
	// The pull right after the push moves lieer's historyId past our own
	// change. It is its own op so the page doesn't take a keystroke's push
	// for news (see Result.Changed).
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
	// "pull: everything is up-to-date." (a sync that pulled only the labels
	// we just pushed still reports true); for a push, when its pull brought
	// nothing but the labels it pushed (see pushPulled).
	Changed bool
	// Refused holds lieer's lines saying its push left changes behind (see
	// pushRefused); those changes' writes were re-applied.
	Refused []string
	// SendLine and AcceptLine: a send's whole output, scanned as it
	// streamed (Output is only its tail), held lieer's "sending message"
	// line and an acceptance marker (Accepted). Scanned says the scan saw
	// all of it: gmi exited and its pipes drained.
	SendLine, AcceptLine, Scanned bool
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
	// Queued: a sync was asked for (SyncNow, or the engine's own follow-up)
	// and hasn't started. Syncing: a sync or first pull has started (it may
	// still wait on the slot or flock); a push, send or check never sets it,
	// so a keystroke's push doesn't read as checking for mail.
	Queued  bool
	Syncing bool
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
	syncReq chan struct{} // cap 1: pending SyncNow requests collapse; under smu with status.Queued (queueSync)
	pushDue chan struct{} // cap 1: debounced push ready; pending pushes collapse

	pmu       sync.Mutex
	pushTimer *time.Timer

	touchDue chan struct{} // cap 1: a NoteWrite landed just after a sync ended

	nmu     sync.Mutex
	syncing bool       // a sync or push holds the slot
	syncEnd time.Time  // when the last one's gmi exited
	writes  []tagWrite // pneu's tag writes since the last run settled them (see NoteWrite)
	carried int        // while syncing: writes[:carried] came before the run began

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
	def(&opts.Interval, 30*time.Second)
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

// tagWrite is one tag change pneu made: the changes as the tag handler
// applied them, to exactly these Message-IDs.
type tagWrite struct {
	changes, ids []string
}

// NoteWrite records that pneu just applied changes to ids in the account's
// notmuch database. Call it after every successful write, before or after
// RequestPush.
//
// Two ways lieer loses a write, and what the end of each sync or push does
// about them:
//
//   - A write that lands while the run holds the slot can be overwritten by
//     its pull (which sets every message with Gmail history to its remote
//     labels) or sit below the lastmod a full pull stores at its end, and
//     the next push skips it. So writes made during a run are re-applied
//     after it, then re-marked with +TouchTag -TouchTag (a no-op re-apply
//     doesn't bump lastmod), and pushed.
//   - lieer's push refuses a message whose Gmail historyId is newer than
//     the last one it pulled ("remote has changed, will not update"), and
//     the pull that follows puts the remote labels back. So when a run's
//     output says "not all changes could be pushed", every write that run
//     carried is re-applied the same way. Only pneu's own changes are
//     replayed, not the whole tag set: a remote change to another label
//     survives.
//
// Otherwise a write is settled by the run that pushed it.
func (e *Engine) NoteWrite(account string, changes, ids []string) {
	a, ok := e.accts[account]
	if !ok || len(changes) == 0 || len(ids) == 0 || a.NotmuchConfig == "" {
		return
	}
	a.nmu.Lock()
	a.writes = append(a.writes, tagWrite{slices.Clone(changes), slices.Clone(ids)})
	late := !a.syncing && !a.syncEnd.IsZero() && time.Since(a.syncEnd) < noteGrace
	a.nmu.Unlock()
	if late {
		signal(a.touchDue)
	}
}

// beginSync opens a run's window: writes from here on are its own.
func (a *account) beginSync() {
	a.nmu.Lock()
	a.syncing = true
	a.carried = len(a.writes)
	a.nmu.Unlock()
}

// abortSync closes a run's window when gmi never started: every write stays
// for the next run to carry.
func (a *account) abortSync() {
	a.nmu.Lock()
	a.syncing = false
	a.carried = 0
	a.nmu.Unlock()
}

// endSync closes a run's window after gmi exited and returns the writes to
// re-apply: those made during the run, or all of them if its push was
// refused. The rest are settled.
func (a *account) endSync(refused bool) []tagWrite {
	a.nmu.Lock()
	defer a.nmu.Unlock()
	from := a.carried
	if refused {
		from = 0
	}
	ws := a.writes[from:]
	a.writes, a.carried = nil, 0
	a.syncing = false
	a.syncEnd = time.Now()
	return ws
}

// requeue puts ws back in front of the writes, for the next run's end.
func (a *account) requeue(ws []tagWrite) {
	a.nmu.Lock()
	a.writes = append(slices.Clip(ws), a.writes...)
	a.nmu.Unlock()
}

// reapply re-applies ws in order and re-marks their messages for push (see
// NoteWrite). The caller holds the account slot and flock, so no push, ours
// or a manual one, runs between the writes and the throwaway tag never
// reaches Gmail even if a repository forgot to ignore it.
func (e *Engine) reapply(a *account, ws []tagWrite) error {
	if len(ws) == 0 {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, w := range ws {
		for _, id := range w.ids {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	nm := notmuch.Account{Name: a.Name, ConfigPath: a.NotmuchConfig}
	ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
	defer cancel()
	var err error
	for _, w := range ws {
		if err = nm.Tag(ctx, w.changes, w.ids); err != nil {
			break
		}
	}
	if err == nil {
		err = nm.Tag(ctx, []string{"+" + TouchTag}, ids)
	}
	if err == nil {
		err = nm.Tag(ctx, []string{"-" + TouchTag}, ids)
	}
	if err != nil {
		e.opts.Logf("gmi: sync [%s]: re-applying %d tag write(s) on %d message(s): %v (retried after the next sync)", a.Name, len(ws), len(ids), err)
		return err
	}
	e.opts.Logf("gmi: sync [%s]: re-applied %d tag write(s) on %d message(s) for push", a.Name, len(ws), len(ids))
	e.RequestPush(a.Name)
	return nil
}

// touchTimeout bounds both touch writes together (the Xapian lock wait).
const touchTimeout = 30 * time.Second

// touchLate handles a NoteWrite that came just after a sync ended (the write
// finished before gmi exited, the call came after): take the slot and flock
// like any run, then re-apply.
func (e *Engine) touchLate(ctx context.Context, a *account) {
	select {
	case a.run <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-a.run }()
	unlock, err := flockFile(ctx, a.LockPath, nil)
	if err != nil {
		return // writes kept; the next run carries them
	}
	defer unlock()
	// The writes stay: the next run still carries them, in case its push is
	// refused. On failure that run's end is the retry.
	a.nmu.Lock()
	ws := slices.Clone(a.writes)
	a.nmu.Unlock()
	e.reapply(a, ws)
}

// SyncNow queues an immediate sync (after any in-flight run). Repeated calls
// before it starts collapse to one. The periodic timer restarts after it.
// Status says Queued from the moment it returns.
func (e *Engine) SyncNow(account string) error {
	a, ok := e.accts[account]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	a.queueSync()
	return nil
}

// Interval is the sync period, for the page's staleness threshold.
func (e *Engine) Interval() time.Duration { return e.opts.Interval }

// queueSync asks the loop for a sync. Queued and the signal change together
// under smu, and startSync clears both together, so Queued is true exactly
// while a request waits: in the channel, or taken by the loop but not yet
// started.
func (a *account) queueSync() {
	a.smu.Lock()
	a.status.Queued = true
	signal(a.syncReq)
	a.smu.Unlock()
}

// startSync marks a sync begun, answering every request made before it
// (the timer's or a request's, whichever woke the loop); running false
// drops the requests without a run (nothing to run yet).
func (a *account) startSync(running bool) {
	a.smu.Lock()
	a.status.Queued = false
	a.status.Syncing = running
	select {
	case <-a.syncReq:
	default:
	}
	a.smu.Unlock()
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
		if op == OpSync {
			a.startSync(false)
		}
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
	// Before OnStart, so a watcher reading Status there sees it syncing.
	if op == OpSync || op == OpPull {
		a.startSync(true)
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
	a.smu.Lock()
	a.status.Syncing = false
	if !ok {
		a.smu.Unlock()
		return // shut down while waiting for the lock or mid-pull; not a failure
	}
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
			a.status.LastSync = end // it pulled too
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
		a.queueSync() // mail that arrived during the pull, at once
	}
}

func (a *account) setRunning(v bool) {
	a.smu.Lock()
	a.status.Running = v
	a.smu.Unlock()
}

// invoke runs a sync or push (both `gmi sync`, see OpPush) under the
// account mutex and flock. ok is false if ctx ended before gmi started.
func (e *Engine) invoke(ctx context.Context, a *account, op Op) (r Result, ok bool) {
	return e.exec(ctx, a, op, []string{string(OpSync)}, nil, e.opts.SyncTimeout)
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
	syncs := op == OpSync || op == OpPush
	settled := false
	if syncs {
		// The NoteWrite window. On an early return it just closes; every
		// write stays for the next run.
		a.beginSync()
		defer func() {
			if !settled {
				a.abortSync()
			}
		}()
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

	if syncs {
		// sync pushes first, and lieer pushes everything since lastmod: a push
		// that came due before this point is covered. (If the sync fails,
		// lastmod doesn't advance and the next sync pushes it.)
		select {
		case <-a.pushDue:
		default:
		}
	}

	switch op {
	case OpSync, OpPush, OpPull, OpSend: // the runs that store mail (see cleanTmp)
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
	refused := &refusalScan{}
	scan := &sendScan{}
	out := io.MultiWriter(tail, refused, scan)
	if o.out != nil {
		out = io.MultiWriter(tail, refused, scan, o.out)
	}
	cmd := exec.CommandContext(cctx, e.opts.GmiPath, args...)
	cmd.Stdin = o.stdin
	cmd.Dir = a.GmiDir
	cmd.Env = gmiEnv(a.NotmuchConfig)
	// Our own pipe, not exec's: Wait prefers an ExitError to saying the
	// output never finished, and NotSent needs to know it did (Scanned).
	pr, pw, err := os.Pipe()
	if err != nil {
		return Result{Account: a.Name, Op: op, Started: time.Now(), ExitCode: -1,
			Err: fmt.Errorf("gmi %s [%s]: %w", op, a.Name, err)}, true
	}
	defer pr.Close()
	cmd.Stdout, cmd.Stderr = pw, pw
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
	drainedClean := false
	err = cmd.Start()
	pw.Close() // the child's copy is the only writer now
	if err == nil {
		drained := make(chan error, 1)
		go func() { _, err := io.Copy(out, pr); drained <- err }()
		if o.stall > 0 && o.out != nil {
			go watchStall(cctx, cancel, o.out, o.stall, cmd.Process.Pid)
		}
		err = cmd.Wait()
		// A descendant may hold the pipe past gmi's exit: wait for EOF as
		// long as exec would have, then give up on the rest.
		wait := cmd.WaitDelay
		if pipeDrainWait > 0 {
			wait = pipeDrainWait
		}
		select {
		case derr := <-drained:
			drainedClean = derr == nil // io.Copy: nil at EOF
		case <-time.After(wait):
			pr.Close()
			<-drained
			if err == nil {
				err = exec.ErrWaitDelay // as Wait said when the pipe was exec's
			}
		}
	}
	r.Duration = time.Since(r.Started)
	r.Output = strings.ToValidUTF8(tail.String(), "")
	r.SendLine, r.AcceptLine = scan.sendLine, scan.accept
	r.Scanned = cmd.ProcessState != nil && cmd.ProcessState.Exited() && drainedClean
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
	r.Refused = refused.lines
	for _, l := range r.Refused {
		e.opts.Logf("gmi: %s [%s]: %s", op, a.Name, l)
	}
	if syncs {
		// Success or failure: a failed full pull may still have set lastmod.
		settled = true
		ws := a.endSync(len(r.Refused) > 0)
		if e.reapply(a, ws) != nil {
			a.requeue(ws)
		}
	}
	return r, true
}

// Accepted reports whether a send's output shows Gmail took the message even
// though gmi failed: lieer 1.6 prints its "receiving content" progress bar
// (storing the sent copy) and then "message sent successfully" only after
// the API send returned. A failure after that point is the local copy's.
// The markers are caught as the output streams, so a long traceback after
// them can't push them out of the tail.
func (r Result) Accepted() bool {
	return r.Op == OpSend && (r.AcceptLine || acceptedIn(r.Output))
}

func acceptedIn(out string) bool {
	return strings.Contains(out, "message sent successfully") || strings.Contains(out, "receiving content")
}

// NotSent reports whether a failed send proves Gmail never got the
// message, so it may be sent again under the same id: lieer exited on its
// own (a kill proves nothing: ExitCode -1), its whole output scanned,
// without printing "sending message", the line it prints right before its
// one API send call. Anything after that line, short of Accepted, is
// unknown: an HTTP error says nothing reliable (httplib2 resends a POST
// whose connection dropped, and lieer makes more requests after the send).
// ErrBusy (gmi never started) is the caller's check.
func (r Result) NotSent() bool {
	return r.Op == OpSend && r.Err != nil && r.Scanned && r.ExitCode > 0 &&
		!r.SendLine && !r.Accepted()
}

// pipeDrainWait overrides how long a run waits for its output's EOF after
// gmi exits (default: the run's WaitDelay); tests only.
var pipeDrainWait time.Duration

// PythonEnv is set on every gmi run whose output pneu reads: unbuffered,
// so lines arrive while gmi runs, and UTF-8 whatever the environment
// says, so the ASCII markers pneu matches (Accepted, NotSent, the consent
// URL, progress) read as written. Later values win in exec's Env.
var PythonEnv = []string{"PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1"}

// gmiEnv is a gmi run's environment for the account's database.
func gmiEnv(notmuchConfig string, extra ...string) []string {
	env := append(os.Environ(), "NOTMUCH_CONFIG="+notmuchConfig)
	env = append(env, PythonEnv...)
	return append(env, extra...)
}

// sendScan watches a send's output stream for the markers NotSent and
// Accepted read, across write boundaries.
type sendScan struct {
	carry            []byte
	sendLine, accept bool
}

var sendMarks = []string{"sending message", "message sent successfully", "receiving content"}

func (s *sendScan) Write(p []byte) (int, error) {
	buf := append(s.carry, p...)
	str := string(buf)
	if strings.Contains(str, sendMarks[0]) {
		s.sendLine = true
	}
	if acceptedIn(str) {
		s.accept = true
	}
	keep := len("message sent successfully") - 1
	if len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	s.carry = append(s.carry[:0], buf...)
	return len(p), nil
}

// changed reads lieer 1.6's (non-quiet, non-TTY) summary lines. See Result.Changed.
func changed(op Op, out string) bool {
	if op == OpPush {
		return pushPulled(out)
	}
	return !strings.Contains(out, "pull: everything is up-to-date.")
}

// lieer 1.6's non-TTY bars (nobar.py) print their description and total
// once: "pushing, 0 changed (N) ..." counts the messages pushed, "updating
// tags (0) (N) ..." every message with Gmail history since the last pull,
// our own pushes included.
var (
	pushedRE   = regexp.MustCompile(`pushing, 0 changed \((\d+)\) \.\.\.`)
	retaggedRE = regexp.MustCompile(`updating tags \(0\) \((\d+)\) \.\.\.`)
)

// pushPulled reports whether a push's pull brought anything but the labels
// it just pushed: new or removed messages, or more retagged messages than
// were pushed.
func pushPulled(out string) bool {
	if strings.Contains(out, "receiving content") || strings.Contains(out, "removing messages") ||
		strings.Contains(out, "pull: full synchronization") {
		return true
	}
	count := func(re *regexp.Regexp) int {
		n := 0
		for _, m := range re.FindAllStringSubmatch(out, -1) {
			v, _ := strconv.Atoi(m[1])
			n += v
		}
		return n
	}
	return count(retaggedRE) > count(pushedRE)
}

// refusalScan collects lieer's lines saying a push left changes behind:
// "update: remote has changed, will not update: <gid> ..." per message, and
// "push: not all changes could be pushed, ..." once. It reads the whole
// stream, since a long pull after them would push them out of the tail.
// exec serializes writes (see tailBuffer).
type refusalScan struct {
	line  []byte
	lines []string
}

func (s *refusalScan) Write(p []byte) (int, error) {
	for _, c := range p {
		if c != '\n' {
			if len(s.line) < 4096 {
				s.line = append(s.line, c)
			}
			continue
		}
		if l := strings.TrimSpace(string(s.line)); strings.Contains(l, "remote has changed, will not update") ||
			strings.Contains(l, "not all changes could be pushed") {
			if len(s.lines) < 50 {
				s.lines = append(s.lines, l)
			}
		}
		s.line = s.line[:0]
	}
	return len(p), nil
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
