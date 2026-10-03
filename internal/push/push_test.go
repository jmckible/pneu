package push

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/google/googletest"
	"github.com/jmckible/pneu/internal/push/state"
)

// The first pull meets the message a new watch publishes at once (C4):
// a nudge, an ack, delivering. Each account's mail nudges only it.
func TestDeliversAndAccountsAreIndependent(t *testing.T) {
	e := newEnv(t, "personal", "work")
	if p := (New(Options{}).State("personal")); p.State != control.PushOff {
		t.Fatalf("before start: %+v", p)
	}
	e.start()
	for _, a := range []string{"personal", "work"} {
		e.until(a+" delivering", e.is(a, control.PushDelivering, ""))
		if _, _, ok := e.f.Watch(e.addr[a]); !ok {
			t.Fatalf("%s: no watch", a)
		}
		res, _ := google.Resource(a)
		e.until(a+" acked", func() bool { return e.f.Acked(res) == 1 && e.f.Outstanding(res) == 0 })
		if n := e.nudges.count(a); n != 1 {
			t.Fatalf("%s: %d nudges", a, n)
		}
		if p := e.state(a); p.Generation != 1 || p.LastDelivery.IsZero() {
			t.Fatalf("%s: %+v", a, p)
		}
	}
	e.f.Notify(e.addr["work"])
	e.until("work's second nudge", func() bool { return e.nudges.count("work") == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := e.nudges.count("personal"); n != 1 {
		t.Fatalf("personal nudged by work's mail: %d", n)
	}
	if e.changed("personal") == 0 || e.changed("work") == 0 {
		t.Fatal("OnChange not told")
	}
	running, err := e.store.DaemonRunning()
	if err != nil || !running {
		t.Fatalf("daemon.lock not held while running: %v %v", running, err)
	}
	e.m.Stop()
	if running, _ := e.store.DaemonRunning(); running {
		t.Fatal("daemon.lock held after Stop")
	}
}

// Two pulls stay outstanding per account, and an empty answer re-pulls at
// most once a second per pull.
func TestTwoPullsAndTheEmptyFloor(t *testing.T) {
	e := newEnv(t, "personal")
	e.start()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	time.Sleep(100 * time.Millisecond)
	if n := e.subCalls(google.OpPull, "personal"); n < Pulls+1 || n > Pulls+2 {
		t.Fatalf("%d pulls with one message and Google holding: want %d outstanding plus the one that delivered", n, Pulls)
	}


	// Every empty pull answered at once: with no fake time passing, no
	// pull loop re-pulls.
	e2 := newEnv(t, "personal")
	e2.f.PullHold = 0
	e2.start()
	e2.until("delivering", e2.is("personal", control.PushDelivering, ""))
	time.Sleep(100 * time.Millisecond)
	base := e2.subCalls(google.OpPull, "personal")
	time.Sleep(200 * time.Millisecond)
	if n := e2.subCalls(google.OpPull, "personal"); n != base {
		t.Fatalf("early empty pulls re-pulled without a second passing: %d -> %d", base, n)
	}
	for range 5 {
		e2.advance(PullFloor, PullFloor)
		time.Sleep(50 * time.Millisecond)
	}
	if n := e2.subCalls(google.OpPull, "personal") - base; n > 5*Pulls || n < Pulls {
		t.Fatalf("%d pulls in 5 empty seconds with %d pulls: want at most one a second each", n, Pulls)
	}
}

// A flood drains ten at a time, each batch nudged and acked; the rate of
// nudges is the engine's to bound (one flag).
func TestNonemptyFlood(t *testing.T) {
	e := newEnv(t, "personal")
	e.start()
	res, _ := google.Resource("personal")
	e.until("first delivery", func() bool { return e.f.Acked(res) == 1 })
	e.f.Publish(res, 95)
	e.until("flood acked", func() bool { return e.f.Acked(res) == 96 && e.f.Queued(res) == 0 })
	if n := e.nudges.count("personal"); n < 11 || n > 96 {
		t.Fatalf("%d nudges for 96 messages in batches of ≤ 10", n)
	}
	for _, r := range e.f.Requests() {
		if r.Op == google.OpAck && strings.Count(string(r.Body), "ack-") > google.MaxMessages {
			t.Fatalf("an ack of more than %d: %s", google.MaxMessages, r.Body)
		}
	}
}

// Notifications keep coming while every ack fails: each pull loop backs
// off, so pulls stay bounded, and once acks work again a redelivery is
// nudged and acked.
func TestAckFailuresHoldTheRate(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.PullHold = 0
	e.f.FailCode(google.OpAck, google.CodeUnavailable, -1)
	e.start()
	res, _ := google.Resource("personal")
	e.until("first delivery", func() bool { return e.nudges.count("personal") >= 1 })
	base := e.subCalls(google.OpPull, "personal")
	for range 60 { // a message a second for a minute, each redelivered too
		e.f.Publish(res, 1)
		e.f.Redeliver(res)
		e.advance(time.Second, time.Second)
	}
	pulls := e.subCalls(google.OpPull, "personal") - base
	// 2s, 4s, 8s, 16s, 32s per pull loop: about five each in a minute.
	if pulls > 7*Pulls {
		t.Fatalf("%d pulls in a minute of failing acks", pulls)
	}
	if e.f.Acked(res) != 0 {
		t.Fatal("acked while failing")
	}
	e.f.ClearFailures()
	e.f.Redeliver(res)
	e.pump("acked again", time.Second, 10*time.Minute, func() bool {
		return e.f.Queued(res) == 0 && e.f.Outstanding(res) == 0 && e.f.Acked(res) > 0
	})
	if p := e.state("personal"); p.State != control.PushDelivering {
		t.Fatalf("after recovery: %+v", p)
	}
}

// A pull that stalls is cut by its client timeout; with no 2xx pull for
// FailAfter the account is failing/network even with its watch renewed.
func TestStalledPullsFail(t *testing.T) {
	e := newEnv(t, "personal")
	e.api = e.f.NewAPI(google.Options{PullTimeout: 50 * time.Millisecond, Timeout: time.Second})
	e.f.Fail(google.OpPull, googletest.Failure{Stall: true, Times: -1})
	e.start()
	e.eventually("watched", func() bool { return e.watchCalls("personal") == 1 })
	e.until("quiet: watched, nothing pulled", e.is("personal", control.PushQuiet, ""))
	e.pump("failing", 5*time.Second, FailAfter+HealthTick*2, e.is("personal", control.PushFailing, control.ReasonNetwork))
	if n := e.subCalls(google.OpPull, "personal"); n < Pulls*2 {
		t.Fatalf("stalled pulls weren't cut and retried: %d", n)
	}
	e.f.ClearFailures()
	e.pump("recovered", time.Second, 10*time.Minute, func() bool { return e.state("personal").State != control.PushFailing })
}

// Renewal OK, pulls refused: failing with the pull's reason, not the
// watch's.
func TestRenewOKPullFailing(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.FailCode(google.OpPull, google.CodePermission, -1)
	e.start()
	e.eventually("watched", func() bool { return e.watchCalls("personal") == 1 })
	e.pump("failing", 5*time.Second, FailAfter+HealthTick*2, e.is("personal", control.PushFailing, control.ReasonPermission))
	if e.nudges.count("personal") != 0 {
		t.Fatal("nudged without a pull")
	}
	// Backoff, not a loop: 2s doubling over 3 minutes is a handful.
	if n := e.subCalls(google.OpPull, "personal"); n > 10*Pulls {
		t.Fatalf("%d pulls in 3 failing minutes", n)
	}
}

// The watch is renewed at min(now+24h, exp − (exp−now)/2), and a failed
// renewal backs off from 30s.
func TestWatchSchedule(t *testing.T) {
	e := newEnv(t, "personal")
	e.start()
	e.eventually("watched", func() bool { return e.watchCalls("personal") == 1 })
	time.Sleep(30 * time.Millisecond) // the loop schedules its next renewal
	_, exp, _ := e.f.Watch(e.addr["personal"])
	if !exp.Equal(googletest.Epoch.Add(googletest.WatchLife)) {
		t.Fatalf("watch expiry %v", exp)
	}
	e.advance(WatchEvery-time.Minute, time.Hour)
	time.Sleep(50 * time.Millisecond)
	if n := e.watchCalls("personal"); n != 1 {
		t.Fatalf("renewed early: %d", n)
	}
	e.f.FailCode(google.OpWatch, google.CodeUnavailable, 2)
	e.advance(time.Minute, time.Minute)
	e.eventually("renewal tried", func() bool { return e.watchCalls("personal") == 2 })
	time.Sleep(30 * time.Millisecond) // the loop schedules its next renewal
	e.advance(29*time.Second, time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := e.watchCalls("personal"); n != 2 {
		t.Fatalf("retried before 30s: %d", n)
	}
	e.advance(time.Second, time.Second)
	e.eventually("retry at 30s", func() bool { return e.watchCalls("personal") == 3 })
	time.Sleep(30 * time.Millisecond) // the loop schedules its next renewal
	e.advance(59*time.Second, time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := e.watchCalls("personal"); n != 3 {
		t.Fatalf("second retry before 60s: %d", n)
	}
	e.advance(time.Second, time.Second)
	e.eventually("retry at 60s", func() bool { return e.watchCalls("personal") == 4 })
	time.Sleep(30 * time.Millisecond) // the loop schedules its next renewal
	if p := e.state("personal"); p.State == control.PushFailing {
		t.Fatalf("a failed renewal with the watch alive: %+v", p)
	}
}

// Asleep past the watch's expiration: on waking it's failing/watch-expired
// at once; nothing is pulled or renewed for the settle; then the watch is
// renewed and it recovers. Old access tokens are dropped.
func TestWakePastWatchExpiry(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.PullHold = 0
	e.start()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	refreshes := e.f.Calls(google.OpRefresh)
	e.clock.wall.Advance(8 * 24 * time.Hour) // suspended: Go's timers didn't move
	e.m.Woke()
	if p := e.state("personal"); p.State != control.PushFailing || p.Reason != control.ReasonWatchExpired {
		t.Fatalf("woke past the watch: %+v", p)
	}
	time.Sleep(100 * time.Millisecond)
	calls := len(e.f.Requests())
	time.Sleep(100 * time.Millisecond)
	e.advance(14*time.Second, time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := len(e.f.Requests()); n != calls {
		t.Fatalf("%d requests during the settle", n-calls)
	}
	e.advance(time.Second, time.Second)
	e.eventually("renewed", func() bool { return e.watchCalls("personal") == 2 })
	e.eventually("not failing", func() bool { return e.state("personal").State != control.PushFailing })
	if e.f.Calls(google.OpRefresh) < refreshes+2 {
		t.Fatal("access tokens kept across the wake")
	}
}

// A wake cancels requests in flight: a stalled renewal and held pulls end
// at once, and new ones follow the settle.
func TestWakeCancelsInFlight(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.PullHold = time.Minute
	e.f.Fail(google.OpWatch, googletest.Failure{Stall: true, Times: 1})
	e.start()
	e.eventually("stalled watch and held pulls", func() bool {
		return e.watchCalls("personal") == 1 && e.subCalls(google.OpPull, "personal") == Pulls
	})
	e.m.Woke()
	e.advance(15*time.Second, time.Second)
	e.eventually("a new renewal and new pulls", func() bool {
		return e.watchCalls("personal") == 2 && e.subCalls(google.OpPull, "personal") >= 2*Pulls
	})
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
}

// An answer from before a wake is discarded: a pull's messages are
// neither nudged nor acked, a renewal's expiration isn't recorded, and a
// refresh's token isn't used.
func TestOldEpochDiscarded(t *testing.T) {
	e := newEnv(t, "personal")
	m := e.newManager()
	e.m = m
	var wakeOn atomic.Value
	wakeOn.Store(google.Op(""))
	m.testAfter = func(op google.Op) {
		if wakeOn.Load().(google.Op) == op && wakeOn.CompareAndSwap(op, google.Op("")) {
			m.Woke()
		}
	}
	wakeOn.Store(google.OpWatch)
	m.Start()
	t.Cleanup(m.Stop)
	res, _ := google.Resource("personal")
	e.eventually("the watch answered", func() bool { return e.watchCalls("personal") == 1 })
	time.Sleep(50 * time.Millisecond)
	if p := e.state("personal"); p.State != control.PushStarting {
		t.Fatalf("a renewal from before the wake counted: %+v", p)
	}
	// The pull that had the message was cut by the wake; after the
	// settle it's delivered on a fresh token.
	e.f.Redeliver(res)
	wakeOn.Store(google.OpPull)
	e.advance(15*time.Second, time.Second)
	e.eventually("a pull answered across the wake", func() bool { return wakeOn.Load().(google.Op) == "" })
	time.Sleep(50 * time.Millisecond)
	if e.nudges.count("personal") != 0 || e.f.Acked(res) != 0 {
		t.Fatalf("a pull from before the wake was acted on: %d nudges, %d acked", e.nudges.count("personal"), e.f.Acked(res))
	}
	e.f.Redeliver(res)
	wakeOn.Store(google.OpRefresh)
	m.owner.forget()
	e.advance(15*time.Second, time.Second)
	e.eventually("refresh across a wake", func() bool { return wakeOn.Load().(google.Op) == "" })
	e.advance(15*time.Second, time.Second)
	e.until("delivered after the settle", func() bool { return e.nudges.count("personal") >= 1 && e.f.Acked(res) >= 1 })
	e.until("watched", func() bool { return e.state("personal").State == control.PushDelivering })
}

// A 401 drops the token and retries at once with a fresh one.
func TestUnauthenticatedRefreshes(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.PullHold = 0
	e.start()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	refreshes := e.f.Calls(google.OpRefresh)
	e.f.FailCode(google.OpPull, google.CodeUnauthenticated, 1)
	e.advance(time.Second, time.Second)
	e.until("refreshed", func() bool { return e.f.Calls(google.OpRefresh) == refreshes+1 })
	res, _ := google.Resource("personal")
	e.f.Publish(res, 1)
	e.advance(time.Second, time.Second)
	e.until("delivered with no backoff", func() bool { return e.nudges.count("personal") == 2 })
}

// The owner's token is refreshed once (for every pull of every account)
// when under 5 minutes remain; a refresh with the wrong scopes is
// refused and the account fails as permission.
func TestOwnerRefreshAheadAndScopes(t *testing.T) {
	e := newEnv(t, "personal", "work")
	e.f.PullHold = 0
	e.start()
	e.until("delivering", func() bool {
		return e.state("personal").State == control.PushDelivering && e.state("work").State == control.PushDelivering
	})
	ownerRefreshes := func() int {
		n := 0
		for _, r := range e.f.Requests() {
			if r.Op == google.OpRefresh && strings.Contains(string(r.Body), "refresh_token="+strings.ReplaceAll(e.ownerR, "/", "%2F")) {
				n++
			}
		}
		return n
	}
	if n := ownerRefreshes(); n != 1 {
		t.Fatalf("%d owner refreshes for 4 pulls: want one shared", n)
	}
	e.advance(googletest.TokenLife-RefreshAhead-10*time.Second, time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := ownerRefreshes(); n != 1 {
		t.Fatalf("refreshed with more than 5 minutes left: %d", n)
	}
	e.advance(20*time.Second, time.Second)
	e.eventually("refreshed ahead", func() bool { return ownerRefreshes() == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := ownerRefreshes(); n != 2 {
		t.Fatalf("%d owner refreshes: want one more, shared", n)
	}

	e.f.MutateNextToken(func(a map[string]any) { a["scope"] = google.ScopePubSub })
	e.advance(googletest.TokenLife-RefreshAhead, time.Minute)
	e.eventually("the bad refresh refused", func() bool {
		return slices.ContainsFunc(e.logged(), func(l string) bool { return strings.HasSuffix(l, ": oauth.refresh: scope") })
	})
	e.pump("refreshed again", time.Second, time.Minute, func() bool { return ownerRefreshes() == 4 })
	e.pump("pulling again", time.Second, time.Minute, func() bool {
		return slices.Contains(e.logged(), "push personal: pulling again") || slices.Contains(e.logged(), "push work: pulling again")
	})
}

// invalid_grant on the owner's refresh stops every worker as
// reauth/owner; nothing more is pulled or renewed.
func TestOwnerInvalidGrantStopsAll(t *testing.T) {
	e := newEnv(t, "personal", "work")
	e.f.PullHold = 0
	e.start()
	e.until("delivering", func() bool {
		return e.state("personal").State == control.PushDelivering && e.state("work").State == control.PushDelivering
	})
	e.f.Revoke(e.owner.Email)
	e.advance(time.Second, time.Second)
	for _, a := range []string{"personal", "work"} {
		e.until(a+" reauth", e.is(a, control.PushReauth, control.ReasonOwnerReauth))
	}
	e.eventually("workers stopped", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		for _, w := range e.m.workers {
			if w.ctx.Err() == nil {
				return false
			}
		}
		return true
	})
	time.Sleep(50 * time.Millisecond)
	n := len(e.f.Requests())
	e.advance(WatchEvery+time.Hour, time.Hour)
	time.Sleep(100 * time.Millisecond)
	if got := len(e.f.Requests()); got != n {
		t.Fatalf("%d requests after the owner's grant died", got-n)
	}
	// A new owner consent, committed and reloaded, starts them again.
	e.f.ClearFailures()
	tok := e.consent(google.Owner, e.owner.Email)
	snap := e.commit(func(f *state.File) { f.Owner.Refresh = tok.Refresh })
	if r := e.reload(snap); r != control.ReloadApplied {
		t.Fatal(r)
	}
	for _, a := range []string{"personal", "work"} {
		e.pump(a+" back", time.Second, time.Minute, func() bool { return e.state(a).State != control.PushReauth })
	}
}

// invalid_grant on a mailbox's refresh stops that worker alone.
func TestMailboxInvalidGrantStopsOne(t *testing.T) {
	e := newEnv(t, "personal", "work")
	e.f.PullHold = 0
	e.start()
	e.until("watched", func() bool { return e.watchCalls("personal") == 1 && e.watchCalls("work") == 1 })
	e.f.Revoke(e.addr["personal"])
	e.pump("personal reauth", time.Hour, WatchEvery+2*time.Hour, e.is("personal", control.PushReauth, control.ReasonMailboxReauth))
	time.Sleep(50 * time.Millisecond)
	pulls := e.subCalls(google.OpPull, "personal")
	workPulls := e.subCalls(google.OpPull, "work")
	e.advance(10*time.Second, time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := e.subCalls(google.OpPull, "personal"); n != pulls {
		t.Fatalf("personal still pulling: %d", n-pulls)
	}
	if e.subCalls(google.OpPull, "work") == workPulls || e.state("work").State == control.PushReauth {
		t.Fatalf("work stopped with personal: %+v", e.state("work"))
	}
	if !slices.Contains(e.logged(), "push personal: mailbox: invalid-grant: a new consent is needed (pneu account push personal)") {
		t.Fatalf("logs: %q", e.logged())
	}
}

// Health over time: delivering turns quiet a day after the last message;
// starting turns failing after 3 minutes without a pull or a watch.
func TestHealthTimer(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.PullHold = 0 // pulls keep up with the fake clock
	e.start()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	before := e.changed("personal")
	// Renewals fail (the watch lives on): a renewal publishes a message.
	e.f.FailCode(google.OpWatch, google.CodeUnavailable, -1)
	e.advance(DeliveringFor+HealthTick, time.Hour)
	e.until("quiet", e.is("personal", control.PushQuiet, ""))
	if e.changed("personal") == before {
		t.Fatal("quiet without OnChange")
	}

	e2 := newEnv(t, "personal")
	e2.f.FailCode(google.OpPull, google.CodeAPIDisabled, -1)
	e2.f.FailCode(google.OpWatch, google.CodeAPIDisabled, -1)
	e2.start()
	time.Sleep(50 * time.Millisecond)
	if p := e2.state("personal"); p.State != control.PushStarting {
		t.Fatalf("%+v", p)
	}
	e2.pump("failing", 5*time.Second, FailAfter+HealthTick*2, e2.is("personal", control.PushFailing, control.ReasonAPIDisabled))
}

// The engine not knowing the account: the message is still acknowledged,
// and that's said once.
func TestNudgeUnknownAccount(t *testing.T) {
	e := newEnv(t, "personal")
	e.nudges.unknown["personal"] = true
	e.start()
	res, _ := google.Resource("personal")
	e.until("acked", func() bool { return e.f.Acked(res) == 1 })
	e.f.Publish(res, 1)
	e.until("acked again", func() bool { return e.f.Acked(res) == 2 })
	n := 0
	for _, l := range e.logged() {
		if strings.Contains(l, "doesn't run this account") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("said %d times", n)
	}
}

// Stop with pulls held open and a renewal stalled: it returns, every
// goroutine of the manager's is gone, and daemon.lock is released.
func TestShutdownLeaksNothing(t *testing.T) {
	e := newEnv(t, "personal", "work")
	e.f.PullHold = time.Minute
	e.f.Fail(google.OpWatch, googletest.Failure{Stall: true, Times: -1})
	e.f.Fail(google.OpRefresh, googletest.Failure{Stall: true, Times: -1})
	e.start()
	e.eventually("refreshes stalled", func() bool { return e.f.Calls(google.OpRefresh) >= 2 })
	done := make(chan struct{})
	go func() { e.m.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung")
	}
	e.m.Stop() // again: harmless
	e.eventually("no goroutines left", func() bool { return len(pushGoroutines()) == 0 })
	if running, _ := e.store.DaemonRunning(); running {
		t.Fatal("daemon.lock held after Stop")
	}
	if _, err := e.m.Reload(1, strings.Repeat("0", 64)); err == nil {
		t.Fatal("reload after Stop")
	}

	e2 := newEnv(t, "personal")
	e2.f.PullHold = time.Minute
	e2.start()
	e2.eventually("pulls held", func() bool { return e2.subCalls(google.OpPull, "personal") >= Pulls+1 })
	e2.m.Stop()
	e2.eventually("no goroutines left", func() bool { return len(pushGoroutines()) == 0 })
}

// While another process holds daemon.lock the manager waits for it, runs
// nothing, and answers reloads with an error (pending, never ok).
func TestWaitsForDaemonLock(t *testing.T) {
	e := newEnv(t, "personal")
	held, err := e.store.TryDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	e.m = e.newManager()
	e.m.Start()
	t.Cleanup(e.m.Stop)
	time.Sleep(100 * time.Millisecond)
	snap, _ := e.store.Load()
	if _, err := e.m.Reload(snap.Generation, snap.Hash); err == nil {
		t.Fatal("reload answered without daemon.lock")
	}
	if e.subCalls(google.OpPull, "personal") != 0 || e.watchCalls("personal") != 0 {
		t.Fatal("pulled without daemon.lock")
	}
	held.Unlock()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	if r := e.reload(snap); r != control.ReloadApplied {
		t.Fatal(r)
	}
}

// No state.json: nothing runs, and the lock is held all the same (the
// CLI must see a daemon).
func TestNoStateThenInit(t *testing.T) {
	e := newEnv(t)
	e.store = state.Store{Dir: state.Dir(t.TempDir() + "/fresh")}
	e.start()
	if running, _ := e.store.DaemonRunning(); !running {
		t.Fatal("no daemon.lock without state")
	}
	if p := e.state("personal"); p.State != control.PushOff || p.Generation != 0 {
		t.Fatalf("%+v", p)
	}
	if r, err := e.m.Reload(1, strings.Repeat("a", 64)); err != nil || r != control.ReloadMismatch {
		t.Fatalf("reload with no file: %v %v", r, err)
	}
}

// A watch that never comes up (every renewal refused) while pulls work is
// failing with the watch's reason, not quiet.
func TestWatchNeverUp(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.PullHold = 0
	e.f.FailCode(google.OpWatch, google.CodePermission, -1)
	e.start()
	e.until("quiet: pulls answer", e.is("personal", control.PushQuiet, ""))
	e.pump("failing", 5*time.Second, FailAfter+HealthTick*2, e.is("personal", control.PushFailing, control.ReasonPermission))
}

// An ack refused as unauthenticated drops the owner's token and is
// retried at once with a fresh one.
func TestAckUnauthenticatedRefreshes(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.FailCode(google.OpAck, google.CodeUnauthenticated, 1)
	e.start()
	res, _ := google.Resource("personal")
	e.until("acked", func() bool { return e.f.Acked(res) == 1 })
	if n := e.subCalls(google.OpAck, "personal"); n != 2 {
		t.Fatalf("%d acks: want the refused one and its retry", n)
	}
	for _, l := range e.logged() {
		if strings.Contains(l, "acknowledge") {
			t.Fatalf("a retried 401 was logged as a failure: %s", l)
		}
	}
}

// A token Google gives for under 5 minutes is used, and refreshed
// halfway, never in a loop.
func TestShortLivedToken(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.MutateNextToken(func(a map[string]any) { a["expires_in"] = 60 })
	e.start()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	time.Sleep(100 * time.Millisecond)
	if n := e.f.Calls(google.OpRefresh); n > 2 {
		t.Fatalf("%d refreshes in a moment", n)
	}
}

// A wake while an ack is in flight cuts it off; nothing is acked during
// the settle, and the redelivery is acked after it.
func TestWakeCutsAck(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.Fail(google.OpAck, googletest.Failure{Stall: true, Times: 1})
	e.start()
	res, _ := google.Resource("personal")
	e.eventually("ack stalled", func() bool { return e.subCalls(google.OpAck, "personal") == 1 })
	e.m.Woke()
	time.Sleep(100 * time.Millisecond)
	if n := e.subCalls(google.OpAck, "personal"); n != 1 {
		t.Fatalf("%d acks during the settle", n)
	}
	e.f.Redeliver(res)
	e.advance(15*time.Second, time.Second)
	e.until("acked after the settle", func() bool { return e.f.Acked(res) >= 1 && e.f.Outstanding(res) == 0 && e.f.Queued(res) == 0 })
}

// A wake renews even a healthy watch once its settle has passed, not at
// the renewal the sleep had scheduled.
func TestWakeRenewsWatch(t *testing.T) {
	e := newEnv(t, "personal")
	e.start()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	e.clock.wall.Advance(time.Hour) // a short suspend
	e.m.Woke()
	e.advance(14*time.Second, time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := e.watchCalls("personal"); n != 1 {
		t.Fatalf("renewed during the settle: %d", n)
	}
	e.advance(time.Second, time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for e.watchCalls("personal") != 2 {
		if time.Now().After(deadline) {
			e.m.mu.Lock()
			su := e.m.settleUntil
			e.m.mu.Unlock()
			t.Fatalf("not renewed after the settle: %d calls, state %+v, settleUntil %v now %v mono %v waiters %d logs %q",
				e.watchCalls("personal"), e.state("personal"), su, e.clock.Now(), e.clock.mono.Now(), e.clock.mono.Waiters(), e.logged())
		}
		time.Sleep(2 * time.Millisecond)
	}
}
