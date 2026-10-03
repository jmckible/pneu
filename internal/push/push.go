// Package push runs push sync in the daemon (docs/push.md D4, D5): one
// worker per account state.json has `on`, each keeping two pulls
// outstanding on the account's subscription with the owner's token and
// its mailbox's Gmail watch renewed with the mailbox token. A message is
// a nudge: Engine.Nudge, then acknowledge. Nothing in it is read.
//
// The CLI writes state.json; the manager only reads it, at start and on
// push-reload, and holds daemon.lock for its whole life so the CLI can
// tell a running daemon from none. Its context is its own, not the
// engine's: main stops it after the control socket closes and before the
// engine stops.
//
// Logs name accounts, operations and google's local codes, never anything
// Google wrote (google.Error carries nothing else).
package push

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/push/state"
)

// Timings (D5). Vars so tests can shorten the ones measured in real time.
var (
	// JoinWait bounds how long a reload waits for cancelled workers to
	// finish: their requests end on cancellation, so this is slack, and
	// it keeps the ack inside control.ReloadTimeout.
	JoinWait = 2 * time.Second
	// LockRetry is how often the manager tries daemon.lock while someone
	// else holds it (a CLI acting on "no daemon" holds it for a moment).
	LockRetry = time.Second
)

const (
	// HealthTick is how often time-based health transitions are looked
	// at (starting or quiet to failing, delivering to quiet).
	HealthTick = 5 * time.Second
	// FailAfter: no 2xx pull for this long is failing.
	FailAfter = 3 * time.Minute
	// DeliveringFor: a message within this long is delivering.
	DeliveringFor = 24 * time.Hour
	// PullFloor: an empty answer re-pulls at most this often per pull.
	PullFloor = time.Second
	// Pulls is how many pulls each worker keeps outstanding.
	Pulls = 2
	// WatchEvery caps the time between watch renewals.
	WatchEvery = 24 * time.Hour
	// watchFloor is the least time to the next renewal, whatever
	// expiration Google names: never a tight loop.
	watchFloor = 5 * time.Second
)

// Backoffs (D5): pull errors 2s doubling to 5 min, back to 2s after 5
// clean minutes; watch failures 30s doubling to 1h, reset on success.
var (
	pullBackoff  = backoff{lo: 2 * time.Second, hi: 5 * time.Minute, clean: 5 * time.Minute}
	watchBackoff = backoff{lo: 30 * time.Second, hi: time.Hour}
)

// Nudger is the engine's half: *gmi.Engine.
type Nudger interface {
	Nudge(account string) error
}

// Options configure a Manager.
type Options struct {
	Store  state.Store
	API    *google.API
	Engine Nudger
	// Clock defaults to the wall clock.
	Clock Clock
	// Settle is how long after a wake the manager waits before it pulls
	// or renews: the same delay main.go gives its wake sync, for the
	// network to come back.
	Settle time.Duration
	// OnChange is told an account's push health changed (state or
	// reason, or the account went off or on). Called without any of the
	// manager's locks; it may call State.
	OnChange func(account string)
	// Logf defaults to log.Printf.
	Logf func(format string, args ...any)
}

// Manager runs the workers. Its methods are safe for concurrent use.
type Manager struct {
	o     Options
	api   *google.API
	clock Clock
	logf  func(string, ...any)

	ctx    context.Context // the manager's own, ended by Stop
	cancel context.CancelFunc
	run    sync.WaitGroup // the manager's own goroutine
	// flights are owner refreshes in progress; started only by workers,
	// so waited for after the workers.
	flights sync.WaitGroup
	done    chan struct{} // closed when Stop has finished

	reloadMu sync.Mutex // one reload (or the first load) at a time

	// epoch counts wakes; an answer from an older epoch is discarded.
	// Written under epochMu's write lock (and mu); read lock-free, or
	// under the read lock (inEpoch) to act on an answer with no wake in
	// between. epochMu comes before mu.
	epoch   atomic.Uint64
	epochMu sync.RWMutex

	mu       sync.Mutex
	started  bool
	stopped  bool
	ready    bool // daemon.lock held and the first state.json read
	lock     *state.DaemonLock
	gen      uint64 // applied generation; 0: none
	hash     string
	ownerKey string
	owner    *ownerSource
	workers  map[string]*worker
	draining []*worker // cancelled, not yet joined
	// epochCtx is the current epoch's: requests run under it, and a wake
	// cancels it.
	epochCtx    context.Context
	epochCancel context.CancelFunc
	wokeAt      time.Time
	settleUntil time.Time
	wakeCh      chan struct{} // closed and replaced on every wake

	// testAfter runs as each Google call returns, before its answer is
	// looked at; tests only.
	testAfter func(google.Op)
}

// New makes a manager; Start runs it.
func New(o Options) *Manager {
	m := &Manager{o: o, api: o.API, clock: o.Clock, logf: o.Logf, workers: map[string]*worker{},
		wakeCh: make(chan struct{}), done: make(chan struct{})}
	if m.clock == nil {
		m.clock = wallClock{}
	}
	if m.logf == nil {
		m.logf = log.Printf
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.epochCtx, m.epochCancel = context.WithCancel(m.ctx)
	return m
}

// Start takes daemon.lock (waiting while another process holds it), reads
// state.json and starts a worker per `on` account, all in the background:
// push-reload answers an error until then. Only a server's daemon runs
// one; a client's never does.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.stopped {
		return
	}
	m.started = true
	m.run.Go(m.loop)
}

// Stop cancels every worker and waits for them, then releases
// daemon.lock. Every exit path calls it; a second call waits for the
// first.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		<-m.done
		return
	}
	m.stopped = true
	m.cancel()
	ws := slices.Collect(maps.Values(m.workers))
	ws = append(ws, m.draining...)
	m.workers, m.draining = map[string]*worker{}, nil
	m.mu.Unlock()
	for _, w := range ws {
		w.cancel()
	}
	for _, w := range ws {
		w.wg.Wait()
	}
	m.run.Wait()
	m.flights.Wait()
	m.mu.Lock()
	if m.lock != nil {
		m.lock.Unlock()
		m.lock = nil
	}
	m.mu.Unlock()
	if m.api != nil {
		m.api.CloseIdle()
	}
	close(m.done)
}

// loop takes daemon.lock, applies state.json, then looks at health on a
// timer until Stop.
func (m *Manager) loop() {
	logged := ""
	for {
		l, err := m.o.Store.TryDaemonLock()
		if err == nil {
			m.mu.Lock()
			if m.stopped {
				m.mu.Unlock()
				l.Unlock()
				return
			}
			m.lock = l
			m.mu.Unlock()
			break
		}
		if msg := err.Error(); msg != logged {
			logged = msg
			if errors.Is(err, state.ErrDaemonRunning) {
				m.logf("push: waiting for %s, held by another process", state.DaemonLockFile)
			} else {
				m.logf("push: no push sync until %s can be taken: %v", state.DaemonLockFile, err)
			}
		}
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(LockRetry):
		}
	}
	m.reloadMu.Lock()
	snap, err := m.o.Store.Load()
	switch {
	case errors.Is(err, state.ErrNoState):
	case err != nil:
		m.logf("push: not running push sync: %v", err)
	default:
		if err := m.apply(snap); err != nil {
			m.logf("push: not running push sync: %v", err)
		}
	}
	m.mu.Lock()
	m.ready = true
	m.mu.Unlock()
	m.reloadMu.Unlock()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.clock.After(HealthTick):
		}
		m.mu.Lock()
		ws := slices.Collect(maps.Values(m.workers))
		m.mu.Unlock()
		for _, w := range ws {
			w.update(nil)
		}
	}
}

// Reload is push-reload: read state.json; if it is generation gen with
// hash hash, apply it. Workers for accounts no longer `on`, or whose
// mailbox credential or the owner's changed, are cancelled and joined
// before it answers; new ones are started. A generation older than the
// one applied is stale; a file that isn't that generation with that hash
// is a mismatch. No network call; bounded by JoinWait.
func (m *Manager) Reload(gen uint64, hash string) (control.Reload, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.mu.Lock()
	ready, stopped, applied, appliedHash := m.ready, m.stopped, m.gen, m.hash
	m.mu.Unlock()
	switch {
	case stopped:
		return 0, errors.New("push: stopping")
	case !ready:
		return 0, errors.New("push: starting (waiting for " + state.DaemonLockFile + ")")
	case gen < applied:
		return control.ReloadStale, nil
	}
	snap, err := m.o.Store.Load()
	if errors.Is(err, state.ErrNoState) {
		return control.ReloadMismatch, nil
	}
	if err != nil {
		return 0, err
	}
	if snap.Generation != gen || snap.Hash != hash {
		return control.ReloadMismatch, nil
	}
	if gen == applied && hash == appliedHash {
		// Applied already (a retry after a lost ack): answer once
		// nothing cancelled is still running.
		if err := m.join(); err != nil {
			return 0, err
		}
		return control.ReloadApplied, nil
	}
	if err := m.apply(snap); err != nil {
		return 0, err
	}
	return control.ReloadApplied, nil
}

// apply makes snap the running state. Called with reloadMu held.
func (m *Manager) apply(snap state.Snapshot) error {
	client, err := m.o.Store.LoadClient()
	if err != nil {
		return err
	}
	if err := state.CheckClient(snap.File, client); err != nil {
		return err
	}
	creds := client.Credentials()
	ownerKey := strings.Join([]string{snap.Project, snap.Install, creds.ID, creds.Secret,
		snap.Owner.Sub, snap.Owner.Refresh}, "\x00")

	var changed []string
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return errors.New("push: stopping")
	}
	newOwner := ownerKey != m.ownerKey
	for name, w := range m.workers {
		a, ok := snap.Accounts[name]
		if newOwner || !ok || a.State != state.On || w.key != mailboxKey(a) {
			w.cancel()
			delete(m.workers, name)
			m.draining = append(m.draining, w)
			changed = append(changed, name)
		}
	}
	if newOwner {
		if m.owner != nil {
			m.owner.cancel()
		}
		m.owner = newOwnerSource(m, creds, snap.Owner.Refresh, snap.Owner.Sub)
		m.ownerKey = ownerKey
	}
	m.mu.Unlock()
	m.notify(changed)
	if err := m.join(); err != nil {
		return err
	}

	changed = changed[:0]
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return errors.New("push: stopping")
	}
	for name, a := range snap.Accounts {
		if a.State != state.On || m.workers[name] != nil {
			continue
		}
		res, ok := google.Resource(name)
		if !ok {
			m.logf("push %s: no worker: not a name a topic can take", name)
			continue
		}
		w := m.newWorker(name, res, snap.Project, a, creds)
		m.workers[name] = w
		w.start()
		changed = append(changed, name)
	}
	m.gen, m.hash = snap.Generation, snap.Hash
	m.mu.Unlock()
	m.notify(changed)
	return nil
}

// join waits up to JoinWait for every cancelled worker to finish.
func (m *Manager) join() error {
	m.mu.Lock()
	ws := slices.Clone(m.draining)
	m.mu.Unlock()
	if len(ws) == 0 {
		return nil
	}
	all := make(chan struct{})
	go func() {
		for _, w := range ws {
			w.wg.Wait()
		}
		close(all)
	}()
	select {
	case <-all:
	case <-time.After(JoinWait):
		return fmt.Errorf("push: a stopped worker is still finishing after %v", JoinWait)
	}
	m.mu.Lock()
	m.draining = slices.DeleteFunc(m.draining, func(w *worker) bool { return slices.Contains(ws, w) })
	m.mu.Unlock()
	return nil
}

func (m *Manager) notify(accounts []string) {
	if m.o.OnChange == nil {
		return
	}
	for _, a := range accounts {
		m.o.OnChange(a)
	}
}

// mailboxKey is what, changed, means a new worker for the account.
func mailboxKey(a state.Account) string { return a.Address + "\x00" + a.Refresh }

// ownerReauth stops every worker on o, the owner whose grant just died:
// each is reauth/owner until a reload brings a new owner credential.
func (m *Manager) ownerReauth(o *ownerSource) {
	m.mu.Lock()
	if m.owner != o {
		m.mu.Unlock()
		return
	}
	ws := slices.Collect(maps.Values(m.workers))
	m.mu.Unlock()
	m.logf("push: owner: %s: a new consent is needed (pneu push init --reconsent)", google.CodeInvalidGrant)
	for _, w := range ws {
		w.stopReauth(control.ReasonOwnerReauth)
	}
}

// State is push-state: the applied generation and the account's health;
// off when no worker runs for it.
func (m *Manager) State(account string) control.PushState {
	m.mu.Lock()
	gen, w := m.gen, m.workers[account]
	m.mu.Unlock()
	if w == nil {
		return control.PushState{Generation: gen, State: control.PushOff}
	}
	st, reason, last := w.health()
	return control.PushState{Generation: gen, State: st, Reason: reason, LastDelivery: last}
}

// Woke is a wake from sleep (main.go's wake handler): every request in
// flight is cancelled and whatever it answers discarded, access tokens
// are dropped, and nothing is pulled or renewed until Settle has passed.
// No sync of its own: main.go's wake sync covers that.
func (m *Manager) Woke() {
	m.epochMu.Lock()
	m.mu.Lock()
	m.epoch.Add(1)
	m.epochCancel()
	m.epochCtx, m.epochCancel = context.WithCancel(m.ctx)
	now := m.clock.Now()
	m.wokeAt = now
	m.settleUntil = now.Add(m.o.Settle)
	close(m.wakeCh)
	m.wakeCh = make(chan struct{})
	owner := m.owner
	ws := slices.Collect(maps.Values(m.workers))
	m.mu.Unlock()
	m.epochMu.Unlock()
	if owner != nil {
		owner.forget()
	}
	for _, w := range ws {
		w.woke(now)
	}
	if m.api != nil {
		m.api.CloseIdle()
	}
}

func (m *Manager) after(op google.Op) {
	if m.testAfter != nil {
		m.testAfter(op)
	}
}

func (m *Manager) epochNow() uint64 { return m.epoch.Load() }

// inEpoch runs fn if no wake has come since epoch ep, with none able to
// come until it returns: an answer from before a wake changes nothing.
func (m *Manager) inEpoch(ep uint64, fn func()) bool {
	m.epochMu.RLock()
	defer m.epochMu.RUnlock()
	if m.epoch.Load() != ep {
		return false
	}
	fn()
	return true
}

// reqCtx is a context for one request by w: it ends with the worker or at
// the next wake. ep is the epoch it was made in, ectx that epoch's
// context.
func (m *Manager) reqCtx(parent context.Context) (ctx context.Context, done func(), ep uint64, ectx context.Context) {
	m.mu.Lock()
	ep, ectx = m.epoch.Load(), m.epochCtx
	m.mu.Unlock()
	ctx, done = ctxIn(parent, ectx)
	return ctx, done, ep, ectx
}

// ctxIn is a context ending with parent or with ectx.
func ctxIn(parent, ectx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(ectx, cancel)
	return ctx, func() { stop(); cancel() }
}

// settle waits out a wake's settle; false when ctx ended.
func (m *Manager) settle(ctx context.Context) bool {
	for {
		m.mu.Lock()
		until, wake := m.settleUntil, m.wakeCh
		m.mu.Unlock()
		d := until.Sub(m.clock.Now())
		if d <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-m.clock.After(min(d, m.o.Settle)):
		case <-wake:
		}
	}
}

// sleep waits d, or less if a wake comes; false when ctx ended.
func (m *Manager) sleep(ctx context.Context, d time.Duration) bool {
	m.mu.Lock()
	wake := m.wakeCh
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return false
	case <-m.clock.After(d):
	case <-wake:
	}
	return true
}
