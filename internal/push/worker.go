package push

import (
	"cmp"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/push/state"
)

// worker runs one account: Pulls pull loops on its subscription with the
// owner's token, and one watch loop on its mailbox with its own.
type worker struct {
	m       *Manager
	name    string
	res     string // topic and subscription ID
	project string
	key     string // mailboxKey: a change means a new worker
	owner   *ownerSource
	mbx     mailboxSource // the watch loop's alone

	ctx    context.Context // ends on reload, reauth or Stop
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	f        facts
	shown    [2]string   // state and reason as last reported to OnChange
	pullLog  google.Code // last pull failure logged; "": none since a success
	watchLog google.Code
	nudgeLog bool
}

// facts are what health is computed from (D5, K12), all wall-clock.
type facts struct {
	since        time.Time // the worker's start, or the last wake
	lastPullOK   time.Time // last 2xx pull, empty or not
	watched      bool      // a watch renewed since the worker started
	watchExp     time.Time
	lastDelivery time.Time // last message
	pullErr      google.Code
	watchErr     google.Code
	reauth       string // control.ReasonOwnerReauth or ReasonMailboxReauth
}

// health is the state, first match wins: reauth, then failing (no 2xx
// pull for FailAfter since the worker started or woke, the watch has
// lapsed, or none was ever had in FailAfter), then starting (neither a 2xx pull nor a watch yet), then
// delivering (a message within DeliveringFor), then quiet.
func (f facts) health(now time.Time) (string, string) {
	ref := f.since
	if f.lastPullOK.After(ref) {
		ref = f.lastPullOK
	}
	switch {
	case f.reauth != "":
		return control.PushReauth, f.reauth
	case now.Sub(ref) >= FailAfter:
		return control.PushFailing, cmp.Or(reasonOf(f.pullErr), control.ReasonNetwork)
	case !f.watchExp.IsZero() && !now.Before(f.watchExp):
		switch r := reasonOf(f.watchErr); r {
		case "", control.ReasonNetwork, control.ReasonUnknown:
			return control.PushFailing, control.ReasonWatchExpired
		default:
			return control.PushFailing, r
		}
	case !f.watched && now.Sub(f.since) >= FailAfter:
		// No watch at all yet: watchExp says nothing, and pulls alone
		// would read as quiet.
		return control.PushFailing, cmp.Or(reasonOf(f.watchErr), control.ReasonNetwork)
	case f.lastPullOK.IsZero() && !f.watched:
		return control.PushStarting, ""
	case !f.lastDelivery.IsZero() && now.Sub(f.lastDelivery) < DeliveringFor:
		return control.PushDelivering, ""
	}
	return control.PushQuiet, ""
}

// reasonOf maps a google code to the closed reasons: the ones a person
// can act on kept, a failure to get through network, anything else
// unknown; "" for none recorded.
func reasonOf(c google.Code) string {
	switch c {
	case "":
		return ""
	case google.CodeAPIDisabled:
		return control.ReasonAPIDisabled
	case google.CodePermission, google.CodeScope:
		return control.ReasonPermission
	case google.CodeOrgPolicy:
		return control.ReasonOrgPolicy
	case google.CodeNetwork, google.CodeUnavailable:
		return control.ReasonNetwork
	}
	return control.ReasonUnknown
}

func (m *Manager) newWorker(name, res, project string, a state.Account, creds google.Credentials) *worker {
	ctx, cancel := context.WithCancel(m.ctx)
	w := &worker{m: m, name: name, res: res, project: project, key: mailboxKey(a), owner: m.owner,
		mbx: mailboxSource{creds: creds, refresh: a.Refresh}, ctx: ctx, cancel: cancel}
	w.f.since = m.clock.Now()
	return w
}

// start runs the loops; called with m.mu held. A worker whose owner's
// grant is already dead starts as reauth and runs nothing.
func (w *worker) start() {
	if w.owner.isDead() {
		w.f.reauth = control.ReasonOwnerReauth
		w.cancel()
	} else {
		w.wg.Go(w.watchLoop)
		for range Pulls {
			w.wg.Go(w.pullLoop)
		}
	}
	st, reason := w.f.health(w.m.clock.Now())
	w.shown = [2]string{st, reason}
}

// update changes the facts under the lock and tells OnChange if the state
// or reason moved.
func (w *worker) update(fn func(f *facts)) {
	w.mu.Lock()
	if fn != nil {
		fn(&w.f)
	}
	st, reason := w.f.health(w.m.clock.Now())
	moved := w.shown != [2]string{st, reason}
	w.shown = [2]string{st, reason}
	w.mu.Unlock()
	if moved && w.m.o.OnChange != nil {
		w.m.o.OnChange(w.name)
	}
}

func (w *worker) health() (string, string, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, reason := w.f.health(w.m.clock.Now())
	return st, reason, w.f.lastDelivery
}

// stopReauth stops the worker for a dead grant; the owner's outranks the
// mailbox's.
func (w *worker) stopReauth(reason string) {
	w.update(func(f *facts) {
		if f.reauth == "" || reason == control.ReasonOwnerReauth {
			f.reauth = reason
		}
	})
	w.cancel()
}

func (w *worker) woke(now time.Time) { w.update(func(f *facts) { f.since = now }) }

func (w *worker) pulled(now time.Time, delivered bool) {
	w.update(func(f *facts) {
		f.lastPullOK, f.pullErr = now, ""
		if delivered {
			f.lastDelivery = now
		}
		if w.pullLog != "" {
			w.pullLog = ""
			w.m.logf("push %s: pulling again", w.name)
		}
	})
}

func (w *worker) pullFailed(op google.Op, err error) {
	code := google.CodeOf(err)
	w.update(func(f *facts) {
		f.pullErr = code
		if w.pullLog != code {
			w.pullLog = code
			w.m.logf("push %s: %s: %s", w.name, op, code)
		}
	})
}

func (w *worker) watched(exp time.Time) {
	w.update(func(f *facts) {
		f.watched, f.watchExp, f.watchErr = true, exp, ""
		if w.watchLog != "" {
			w.watchLog = ""
			w.m.logf("push %s: watch renewed", w.name)
		}
	})
}

func (w *worker) watchFailed(op google.Op, err error) {
	code := google.CodeOf(err)
	w.update(func(f *facts) {
		f.watchErr = code
		if w.watchLog != code {
			w.watchLog = code
			w.m.logf("push %s: %s: %s", w.name, op, code)
		}
	})
}

// nudge hands the message to the engine: a sync soon. The account may be
// one the config no longer has; that's said once, and the message is
// acknowledged all the same (a redelivery would say nothing new).
func (w *worker) nudge() {
	if err := w.m.o.Engine.Nudge(w.name); err != nil {
		w.mu.Lock()
		first := !w.nudgeLog
		w.nudgeLog = true
		w.mu.Unlock()
		if first {
			w.m.logf("push %s: the sync engine doesn't run this account; its notifications are dropped", w.name)
		}
	}
}

// opOf is the operation an error names, or dflt (a refresh's error names
// the refresh).
func opOf(err error, dflt google.Op) google.Op {
	var ge *google.Error
	if errors.As(err, &ge) {
		return ge.Op
	}
	return dflt
}

// pullLoop keeps one pull outstanding: messages are a nudge and then an
// ack; an empty answer re-pulls, at most once per PullFloor; an error
// backs off. A 401 drops the token and retries once at once. An answer is
// acted on (health, nudge, ack) only with no wake since its request.
func (w *worker) pullLoop() {
	m := w.m
	b := pullBackoff
	var lastStart time.Time
	lastEp := m.epochNow()
	empty, retried := false, false
	for {
		if empty {
			// Capped, so a wall clock stepped back can't stretch it.
			if d := min(lastStart.Add(PullFloor).Sub(m.clock.Now()), PullFloor); d > 0 {
				if !m.sleep(w.ctx, d, lastEp) {
					return
				}
				continue
			}
		}
		ctx, done, ep, ectx, ok := m.admit(w.ctx)
		if !ok {
			return
		}
		lastEp, empty = ep, false
		tok, err := w.owner.get(ctx, ep, ectx)
		var ids []string
		if err == nil {
			lastStart = m.clock.Now()
			ids, err = m.api.Pull(ctx, tok, w.project, w.res)
			m.after(google.OpPull)
		} else if errors.Is(err, errReauth) {
			done()
			return // ownerReauth stops every worker
		}
		done()
		switch {
		case w.ctx.Err() != nil:
			return
		case errors.Is(err, errStale):
			continue
		case google.CodeOf(err) == google.CodeUnauthenticated && !retried && m.epochNow() == ep:
			w.owner.drop(tok)
			retried = true
			continue
		}
		acted := m.inEpoch(ep, func() {
			if err != nil {
				w.pullFailed(opOf(err, google.OpRefresh), err)
				return
			}
			now := m.clock.Now()
			b.ok(now)
			w.pulled(now, len(ids) > 0)
			if len(ids) > 0 {
				w.nudge()
			}
		})
		if !acted {
			continue // from before a wake: discarded, redelivered later
		}
		retried = false
		if err == nil && len(ids) == 0 {
			empty = true
			continue
		}
		if err == nil {
			err = w.ack(tok, ids, ep, ectx)
			if err == nil || !m.inEpoch(ep, func() { w.pullFailed(opOf(err, google.OpAck), err) }) {
				continue
			}
		}
		if !m.sleep(w.ctx, b.fail(m.clock.Now()), ep) {
			return
		}
	}
}

// ack acknowledges ids pulled in epoch ep, under that epoch's context so
// a wake cuts it off. A 401 drops the token and retries once with a fresh
// one. nil also when the worker ended or a wake came: nothing to back off
// for (the messages are redelivered).
func (w *worker) ack(tok string, ids []string, ep uint64, ectx context.Context) error {
	m := w.m
	for retried := false; ; retried = true {
		ctx, done := ctxIn(w.ctx, ectx)
		err := m.api.Ack(ctx, tok, w.project, w.res, ids)
		m.after(google.OpAck)
		if err == nil || w.ctx.Err() != nil || m.epochNow() != ep {
			done()
			return nil
		}
		if google.CodeOf(err) != google.CodeUnauthenticated || retried {
			done()
			return err
		}
		w.owner.drop(tok)
		tok, err = w.owner.get(ctx, ep, ectx)
		done()
		switch {
		case w.ctx.Err() != nil, errors.Is(err, errReauth), errors.Is(err, errStale):
			return nil
		case err != nil:
			return err
		}
	}
}

// watchLoop renews the mailbox's watch at start and after every wake's
// settle, then at min(now + WatchEvery, expiration − (expiration − now)/2),
// backing off on failure.
// A 401 drops the token and retries once at once; invalid_grant on the
// refresh stops the worker as reauth/mailbox. An answer counts only with
// no wake since its request.
func (w *worker) watchLoop() {
	m := w.m
	b := watchBackoff
	var next time.Time
	retried := false
	seen := m.epochNow()
	for {
		if ep := m.epochNow(); ep != seen {
			seen, next = ep, time.Time{} // renewed after every wake's settle
		}
		if d := next.Sub(m.clock.Now()); d > 0 {
			if !m.sleep(w.ctx, d, seen) {
				return
			}
			continue // a wake cuts the wait short: look again by the wall clock
		}
		ctx, done, ep, _, ok := m.admit(w.ctx)
		if !ok {
			return
		}
		tok, err := w.mbx.get(ctx, m, ep)
		var exp time.Time
		if err == nil {
			exp, err = m.api.Watch(ctx, tok, w.project, w.res)
			m.after(google.OpWatch)
		}
		done()
		now := m.clock.Now()
		switch {
		case w.ctx.Err() != nil:
			return
		case errors.Is(err, errReauth):
			m.logf("push %s: mailbox: %s: a new consent is needed (pneu account push %s)", w.name, google.CodeInvalidGrant, w.name)
			w.stopReauth(control.ReasonMailboxReauth)
			return
		case errors.Is(err, errStale):
			continue
		case google.CodeOf(err) == google.CodeUnauthenticated && !retried && m.epochNow() == ep:
			w.mbx.drop()
			retried = true
			continue
		case err == nil && !exp.After(now):
			err = &google.Error{Op: google.OpWatch, Code: google.CodeUnknown}
		}
		if !m.inEpoch(ep, func() {
			if err != nil {
				w.watchFailed(opOf(err, google.OpRefresh), err)
			} else {
				w.watched(exp)
			}
		}) {
			continue // from before a wake: renewed again after the settle
		}
		retried = false
		if err != nil {
			next = now.Add(b.fail(now))
			continue
		}
		b.reset()
		next = exp.Add(-exp.Sub(now) / 2)
		if cap := now.Add(WatchEvery); next.After(cap) {
			next = cap
		}
		if floor := now.Add(watchFloor); next.Before(floor) {
			next = floor
		}
	}
}
