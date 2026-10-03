package push

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/google/googletest"
	"github.com/jmckible/pneu/internal/push/state"
)

// Stale, mismatched and repeated reloads (a lost ack retried).
func TestReloadAnswers(t *testing.T) {
	e := newEnv(t, "personal")
	e.start()
	cur, _ := e.store.Load()
	snap := e.commit(func(f *state.File) {})
	if r := e.reload(snap); r != control.ReloadApplied {
		t.Fatal(r)
	}
	if r := e.reload(snap); r != control.ReloadApplied {
		t.Fatalf("the same generation again (a lost ack): %v", r)
	}
	if r := e.reload(cur); r != control.ReloadStale {
		t.Fatalf("an older generation: %v", r)
	}
	if r, err := e.m.Reload(snap.Generation, strings.Repeat("0", 64)); err != nil || r != control.ReloadMismatch {
		t.Fatalf("a wrong hash: %v %v", r, err)
	}
	if r, err := e.m.Reload(snap.Generation+1, snap.Hash); err != nil || r != control.ReloadMismatch {
		t.Fatalf("a generation not on disk: %v %v", r, err)
	}
	// A commit after the one named: mismatch, and the CLI re-reads.
	next := e.commit(func(f *state.File) {})
	if r, err := e.m.Reload(next.Generation-1, snap.Hash); err != nil || r != control.ReloadMismatch {
		t.Fatalf("%v %v", r, err)
	}
	if r := e.reload(next); r != control.ReloadApplied {
		t.Fatal(r)
	}
	if p := e.state("personal"); p.Generation != next.Generation {
		t.Fatalf("applied generation %d, want %d", p.Generation, next.Generation)
	}
}

// --off's first step: off-pending, reloaded. The ack comes only once the
// worker is joined; nothing for that account is called afterwards, and
// the other account runs on untouched.
func TestReloadOffJoins(t *testing.T) {
	e := newEnv(t, "personal", "work")
	e.f.PullHold = 0
	e.start()
	e.until("delivering", func() bool {
		return e.state("personal").State == control.PushDelivering && e.state("work").State == control.PushDelivering
	})
	e.m.mu.Lock()
	work := e.m.workers["work"]
	e.m.mu.Unlock()
	snap := e.commit(func(f *state.File) {
		a := f.Accounts["personal"]
		a.State = state.OffPending
		f.Accounts["personal"] = a
	})
	if r := e.reload(snap); r != control.ReloadApplied {
		t.Fatal(r)
	}
	if p := e.state("personal"); p.State != control.PushOff || p.Generation != snap.Generation {
		t.Fatalf("after off: %+v", p)
	}
	pulls, watches := e.subCalls(google.OpPull, "personal"), e.watchCalls("personal")
	e.advance(WatchEvery+time.Hour, time.Minute)
	if e.subCalls(google.OpPull, "personal") != pulls || e.watchCalls("personal") != watches {
		t.Fatal("called Google for an off account after the ack")
	}
	e.m.mu.Lock()
	same := e.m.workers["work"] == work
	e.m.mu.Unlock()
	if !same || e.watchCalls("work") < 2 {
		t.Fatal("work's worker was disturbed")
	}
}

// --off racing a renewal in flight, and a mailbox refresh in flight: the
// reload cancels and joins them inside JoinWait; none completes after.
func TestReloadRacesRenewalAndRefresh(t *testing.T) {
	for _, op := range []google.Op{google.OpWatch, google.OpRefresh} {
		t.Run(string(op), func(t *testing.T) {
			e := newEnv(t, "personal")
			e.f.Fail(op, googletest.Failure{Stall: true, Times: -1})
			e.start()
			e.eventually("stalled", func() bool { return e.f.Calls(op) >= 1 })
			snap := e.commit(func(f *state.File) {
				a := f.Accounts["personal"]
				a.State = state.OffPending
				f.Accounts["personal"] = a
			})
			began := time.Now()
			if r := e.reload(snap); r != control.ReloadApplied {
				t.Fatal(r)
			}
			if d := time.Since(began); d > JoinWait {
				t.Fatalf("ack took %v", d)
			}
			e.m.mu.Lock()
			n := len(e.m.draining)
			e.m.mu.Unlock()
			if n != 0 {
				t.Fatal("acked with a worker still draining")
			}
			// The CLI's users.stop now finds no renewal coming after it.
			e.f.ClearFailures()
			calls := e.watchCalls("personal")
			e.advance(time.Hour, time.Minute)
			if e.watchCalls("personal") != calls {
				t.Fatal("renewed after the off ack")
			}
		})
	}
}

// A new mailbox credential means a new worker, from starting; the old one
// is joined first.
func TestReloadNewCredential(t *testing.T) {
	e := newEnv(t, "personal")
	e.f.PullHold = 0
	e.start()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	e.m.mu.Lock()
	old := e.m.workers["personal"]
	e.m.mu.Unlock()
	tok := e.consent(google.Mailbox, e.addr["personal"])
	snap := e.commit(func(f *state.File) {
		a := f.Accounts["personal"]
		a.Refresh = tok.Refresh
		f.Accounts["personal"] = a
	})
	e.f.Fail(google.OpWatch, googletest.Failure{Stall: true, Times: 1})
	if r := e.reload(snap); r != control.ReloadApplied {
		t.Fatal(r)
	}
	if old.ctx.Err() == nil {
		t.Fatal("old worker not cancelled")
	}
	e.m.mu.Lock()
	cur := e.m.workers["personal"]
	e.m.mu.Unlock()
	if cur == old || cur == nil {
		t.Fatal("no new worker")
	}
	e.until("pulled", func() bool { return e.state("personal").State == control.PushQuiet })
}

// A new owner credential restarts every worker; an unchanged reload
// restarts none.
func TestReloadOwnerChange(t *testing.T) {
	e := newEnv(t, "personal", "work")
	e.start()
	workers := func() map[string]*worker {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		out := map[string]*worker{}
		for k, v := range e.m.workers {
			out[k] = v
		}
		return out
	}
	before := workers()
	snap := e.commit(func(f *state.File) {})
	e.reload(snap)
	if after := workers(); after["personal"] != before["personal"] || after["work"] != before["work"] {
		t.Fatal("an unchanged reload restarted workers")
	}
	tok := e.consent(google.Owner, e.owner.Email)
	snap = e.commit(func(f *state.File) { f.Owner.Refresh = tok.Refresh })
	e.reload(snap)
	after := workers()
	for _, a := range []string{"personal", "work"} {
		if after[a] == before[a] || before[a].ctx.Err() == nil {
			t.Fatalf("%s kept its worker across an owner change", a)
		}
	}
}

// A reload adds an account turned on; one with another client recorded is
// refused and changes nothing.
func TestReloadAddAndRefuse(t *testing.T) {
	e := newEnv(t, "personal", "work")
	snap := e.commit(func(f *state.File) { delete(f.Accounts, "work") })
	e.start()
	if p := e.state("work"); p.State != control.PushOff {
		t.Fatalf("%+v", p)
	}
	snap = e.commit(func(f *state.File) {
		f.Accounts["work"] = state.Account{State: state.On, Address: e.addr["work"], Refresh: e.mbx["work"], Granted: googletest.Epoch}
	})
	e.reload(snap)
	e.until("work delivering", e.is("work", control.PushDelivering, ""))

	l, err := e.store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := `{"installed":{"client_id":"999-other.apps.googleusercontent.com","client_secret":"x","project_id":"pneu-push-test"}}`
	if _, err := l.WriteClient([]byte(other)); err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	snap = e.commit(func(f *state.File) { delete(f.Accounts, "work") })
	if _, err := e.m.Reload(snap.Generation, snap.Hash); err == nil {
		t.Fatal("applied with another client.json")
	}
	if p := e.state("work"); p.State == control.PushOff || p.Generation == snap.Generation {
		t.Fatalf("a refused reload changed something: %+v", p)
	}
}

// A daemon starting on a state with an off-pending account runs no worker
// for it, whatever happened in between, and calls nothing for it.
func TestRestartWithOffPending(t *testing.T) {
	e := newEnv(t, "personal", "work")
	e.commit(func(f *state.File) {
		a := f.Accounts["personal"]
		a.State = state.OffPending
		f.Accounts["personal"] = a
	})
	e.start()
	e.until("work delivering", e.is("work", control.PushDelivering, ""))
	e.advance(WatchEvery+time.Hour, time.Hour)
	if p := e.state("personal"); p.State != control.PushOff {
		t.Fatalf("%+v", p)
	}
	res, _ := google.Resource("personal")
	if e.subCalls(google.OpPull, "personal") != 0 || e.watchCalls("personal") != 0 || e.f.Acked(res) != 0 {
		t.Fatal("called Google for an off-pending account")
	}
}

// The handlers over the real control socket: push-reload's ack names the
// generation, and push-state's reply parses strictly with this process's
// instance.
func TestControlSocket(t *testing.T) {
	e := newEnv(t, "personal")
	e.start()
	path := filepath.Join(t.TempDir(), "run", "control")
	ctl, err := control.Listen(path, control.Handler{PushReload: e.m.Reload, PushState: e.m.State})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	e.until("delivering", e.is("personal", control.PushDelivering, ""))
	snap := e.commit(func(f *state.File) {})
	if r, err := control.ReloadPush(path, snap.Generation, snap.Hash); err != nil || r != control.ReloadApplied {
		t.Fatalf("push-reload: %v %v", r, err)
	}
	p, err := control.AskPushState(path, "personal")
	if err != nil || p.Instance != control.Instance() || p.Generation != snap.Generation || p.State != control.PushDelivering || p.LastDelivery.IsZero() {
		t.Fatalf("push-state: %+v %v", p, err)
	}
	if p, err := control.AskPushState(path, "nobody"); err != nil || p.State != control.PushOff {
		t.Fatalf("push-state for an account with no worker: %+v %v", p, err)
	}
	if r, err := control.ReloadPush(path, snap.Generation, strings.Repeat("f", 64)); err != nil || r != control.ReloadMismatch {
		t.Fatalf("mismatch: %v %v", r, err)
	}
}
