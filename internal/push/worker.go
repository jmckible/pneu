package push

import (
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
// pull for FailAfter since the worker started or woke, or the watch has
// lapsed), then starting (neither a 2xx pull nor a watch yet), then
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
		return control.PushFailing, reasonOf(f.pullErr, control.ReasonNetwork)
	case !f.watchExp.IsZero() && !now.Before(f.watchExp):
		return control.PushFailing, reasonOf(f.watchErr, control.ReasonWatchExpired)
	case f.lastPullOK.IsZero() && !f.watched:
		return control.PushStarting, ""
	case !f.lastDelivery.IsZero() && now.Sub(f.lastDelivery) < DeliveringFor:
		return control.PushDelivering, ""
	}
	return control.PushQuiet, ""
}

// reasonOf maps a google code to the closed reasons. The ones a person
// can act on are kept; a network failure is network; anything else, or
// nothing recorded, is dflt for a lapsed watch (whose renewals say only
// that they didn't get through) and unknown otherwise.
func reasonOf(c google.Code, dflt string) string {
	switch c {
	case google.CodeAPIDisabled:
		return control.ReasonAPIDisabled
	case google.CodePermission, google.CodeScope:
		return control.ReasonPermission
	case google.CodeOrgPolicy:
		return control.ReasonOrgPolicy
	case google.CodeNetwork, google.CodeUnavailable:
		if dflt == control.ReasonWatchExpired {
			return dflt
		}
		return control.ReasonNetwork
	case "":
		return dflt
	}
	if dflt == control.ReasonWatchExpired {
		return dflt
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
// backs off. A 401 drops the token and retries once at once.
func (w *worker) pullLoop() {
	m := w.m
	b := pullBackoff
	var lastStart time.Time
	empty, retried := false, false
	fail := func(op google.Op, err error) bool {
		w.pullFailed(opOf(err, op), err)
		return m.sleep(w.ctx, b.fail(m.clock.Now()))
	}
	for {
		if !m.settle(w.ctx) {
			return
		}
		if empty {
			// Capped, so a wall clock stepped back can't stretch it.
			if d := min(lastStart.Add(PullFloor).Sub(m.clock.Now()), PullFloor); d > 0 {
				if !m.sleep(w.ctx, d) {
					return
				}
				continue
			}
		}
		empty = false
		ctx, done, ep, ectx := m.reqCtx(w.ctx)
		tok, err := w.owner.get(ctx, ep, ectx)
		if err != nil {
			done()
			switch {
			case w.ctx.Err() != nil, errors.Is(err, errReauth):
				return // ownerReauth stops every worker
			case errors.Is(err, errStale), m.epochNow() != ep:
				continue
			}
			if !fail(google.OpRefresh, err) {
				return
			}
			continue
		}
		lastStart = m.clock.Now()
		ids, err := m.api.Pull(ctx, tok, w.project, w.res)
		m.after(google.OpPull)
		done()
		switch {
		case w.ctx.Err() != nil:
			return
		case m.epochNow() != ep:
			continue // from before a wake: discarded, redelivered later
		case google.CodeOf(err) == google.CodeUnauthenticated && !retried:
			w.owner.drop(tok)
			retried = true
			continue
		case err != nil:
			retried = false
			if !fail(google.OpPull, err) {
				return
			}
			continue
		}
		retried = false
		now := m.clock.Now()
		b.ok(now)
		w.pulled(now, len(ids) > 0)
		if len(ids) == 0 {
			empty = true
			continue
		}
		w.nudge()
		if m.epochNow() != ep {
			continue // a wake since: the ack waits for the redelivery
		}
		ctx, done, ep, _ = m.reqCtx(w.ctx)
		err = m.api.Ack(ctx, tok, w.project, w.res, ids)
		m.after(google.OpAck)
		done()
		switch {
		case w.ctx.Err() != nil:
			return
		case err != nil && m.epochNow() == ep:
			if !fail(google.OpAck, err) {
				return
			}
		}
	}
}

// watchLoop renews the mailbox's watch at start, then at min(now +
// WatchEvery, expiration − (expiration − now)/2), backing off on failure.
// A 401 drops the token and retries once at once; invalid_grant on the
// refresh stops the worker as reauth/mailbox.
func (w *worker) watchLoop() {
	m := w.m
	b := watchBackoff
	var next time.Time
	retried := false
	for {
		if !m.settle(w.ctx) {
			return
		}
		if d := next.Sub(m.clock.Now()); d > 0 {
			if !m.sleep(w.ctx, d) {
				return
			}
			continue // a wake cuts the wait short: look again by the wall clock
		}
		ctx, done, ep, _ := m.reqCtx(w.ctx)
		tok, err := w.mbx.get(ctx, m, ep)
		if err != nil {
			done()
			switch {
			case w.ctx.Err() != nil:
				return
			case errors.Is(err, errReauth):
				m.logf("push %s: mailbox: %s: a new consent is needed (pneu account push %s)", w.name, google.CodeInvalidGrant, w.name)
				w.stopReauth(control.ReasonMailboxReauth)
				return
			case errors.Is(err, errStale), m.epochNow() != ep:
				continue
			}
			w.watchFailed(opOf(err, google.OpRefresh), err)
			next = m.clock.Now().Add(b.fail(m.clock.Now()))
			continue
		}
		exp, err := m.api.Watch(ctx, tok, w.project, w.res)
		m.after(google.OpWatch)
		done()
		now := m.clock.Now()
		switch {
		case w.ctx.Err() != nil:
			return
		case m.epochNow() != ep:
			continue // from before a wake: renewed again after the settle
		case google.CodeOf(err) == google.CodeUnauthenticated && !retried:
			w.mbx.drop()
			retried = true
			continue
		case err == nil && !exp.After(now):
			err = &google.Error{Op: google.OpWatch, Code: google.CodeUnknown}
		}
		retried = false
		if err != nil {
			w.watchFailed(google.OpWatch, err)
			next = now.Add(b.fail(now))
			continue
		}
		b.reset()
		w.watched(exp)
		next = exp.Add(-exp.Sub(now) / 2)
		if cap := now.Add(WatchEvery); next.After(cap) {
			next = cap
		}
		if floor := now.Add(watchMin); next.Before(floor) {
			next = floor
		}
	}
}
