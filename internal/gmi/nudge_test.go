package gmi

import (
	"errors"
	"testing"
	"time"
)

// spawnSlack allows for the fake recording its start a little after the
// engine starts the run: the first spawn of a test is the slowest.
const spawnSlack = 50 * time.Millisecond

func shortGap(t *testing.T, d time.Duration) {
	t.Helper()
	old := NudgeGap
	NudgeGap = d
	t.Cleanup(func() { NudgeGap = old })
}

// A nudge turns into one sync, no sooner than NudgeGap after the last one
// started; nudges before it collapse into it.
func TestNudge(t *testing.T) {
	f := newFixture(t)
	shortGap(t, 300*time.Millisecond)
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	if err := e.Nudge("nobody"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatal(err)
	}
	start(t, e)
	waitFor(t, 3*time.Second, "the first sync", func() bool { return len(f.runs()) == 1 && !f.runs()[0].end.IsZero() })
	first := f.runs()[0]
	for range 20 {
		if err := e.Nudge("personal"); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 3*time.Second, "the nudged sync", func() bool { return f.count("sync") == 2 })
	second := f.runs()[1]
	if gap := second.start.Sub(first.start); gap < NudgeGap-spawnSlack {
		t.Fatalf("nudged sync %v after the last start, under the %v gap", gap, NudgeGap)
	}
	time.Sleep(3 * NudgeGap)
	if n := f.count("sync"); n != 2 {
		t.Fatalf("%d syncs: nudges didn't collapse", n)
	}
	// Long after the last sync, a nudge runs at once.
	at := time.Now()
	e.Nudge("personal")
	waitFor(t, 3*time.Second, "an immediate nudged sync", func() bool { return f.count("sync") == 3 })
	if d := f.runs()[2].start.Sub(at); d > 200*time.Millisecond {
		t.Fatalf("eligible nudge waited %v", d)
	}
}

// A nudge made while a sync runs is kept: that sync may have missed the
// mail, so another follows, NudgeGap after the first started.
func TestNudgeDuringSync(t *testing.T) {
	f := newFixture(t)
	shortGap(t, 400*time.Millisecond)
	t.Setenv("FAKEGMI_SLEEP", "0.2")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 3*time.Second, "a sync running", func() bool { return len(f.runs()) == 1 })
	e.Nudge("personal")
	waitFor(t, 3*time.Second, "the follow-up", func() bool { return f.count("sync") == 2 })
	rs := f.runs()
	if gap := rs[1].start.Sub(rs[0].start); gap < NudgeGap-spawnSlack {
		t.Fatalf("follow-up %v after the first started", gap)
	}
	assertSerialized(t, rs)
}

// Any sync answers a pending nudge: SyncNow runs at once (it ignores the
// gap) and leaves nothing for the nudge to run.
func TestSyncClearsNudge(t *testing.T) {
	f := newFixture(t)
	shortGap(t, 300*time.Millisecond)
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 3*time.Second, "the first sync", func() bool { return len(f.runs()) == 1 && !f.runs()[0].end.IsZero() })
	e.Nudge("personal")
	e.SyncNow("personal")
	waitFor(t, 3*time.Second, "SyncNow's sync", func() bool { return f.count("sync") == 2 })
	time.Sleep(3 * NudgeGap)
	if n := f.count("sync"); n != 2 {
		t.Fatalf("%d syncs: the nudge ran after a sync answered it", n)
	}
	// A push runs gmi sync, which pulls: it answers a nudge too. Nudge
	// just after a sync starts, so the debounced push comes due before the
	// nudge does.
	e.SyncNow("personal")
	waitFor(t, 3*time.Second, "another sync", func() bool { return f.count("sync") == 3 })
	e.Nudge("personal")
	e.RequestPush("personal")
	waitFor(t, 3*time.Second, "the push", func() bool { return f.count("sync") == 4 })
	time.Sleep(3 * NudgeGap)
	if n := f.count("sync"); n != 4 {
		t.Fatalf("%d syncs after a push answered the nudge", n)
	}
}

// Inside the failure backoff a nudge waits for it: push never makes a
// failing account sync faster than the engine's own backoff allows.
func TestNudgeRespectsBackoff(t *testing.T) {
	f := newFixture(t)
	shortGap(t, 50*time.Millisecond)
	t.Setenv("FAKEGMI_EXIT", "1")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{Interval: 400 * time.Millisecond, MaxBackoff: time.Hour}))
	start(t, e)
	waitFor(t, 3*time.Second, "the failed sync", func() bool { return len(f.runs()) == 1 && !f.runs()[0].end.IsZero() })
	first := f.runs()[0]
	e.Nudge("personal")
	// One failure: delay is twice the interval.
	waitFor(t, 5*time.Second, "the next sync", func() bool { return f.count("sync") == 2 })
	if gap := f.runs()[1].start.Sub(first.end); gap < 750*time.Millisecond {
		t.Fatalf("nudge ran %v after a failure, inside the %v backoff", gap, 800*time.Millisecond)
	}
	// The count moves as the run starts; its failure lands as it ends.
	waitFor(t, 3*time.Second, "the second failure", func() bool { st, _ := e.Status("personal"); return st.Failures == 2 })
}

// The poll goes on as before around nudges.
func TestNudgeKeepsPoll(t *testing.T) {
	f := newFixture(t)
	shortGap(t, 50*time.Millisecond)
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{Interval: 250 * time.Millisecond}))
	start(t, e)
	waitFor(t, 3*time.Second, "the first sync", func() bool { return f.count("sync") == 1 })
	e.Nudge("personal")
	waitFor(t, 3*time.Second, "polls after the nudge", func() bool { return f.count("sync") >= 5 })
	rs := f.runs()
	for i := 2; i < len(rs); i++ {
		if gap := rs[i].start.Sub(rs[i-1].start); gap > 600*time.Millisecond {
			t.Fatalf("poll gap %v after a nudge", gap)
		}
	}
}

// NudgeGap counts from when gmi starts, not from when the sync began
// waiting: a sync held up by the flock (a manual pneu gmi) and then run
// briefly is followed by a nudge's sync only NudgeGap after that start.
func TestNudgeGapFromGmiStart(t *testing.T) {
	f := newFixture(t)
	shortGap(t, 400*time.Millisecond)
	t.Setenv("FAKEGMI_SLEEP", "0.1")
	a := f.account("personal")
	e := mustNew(t, []Account{a}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 3*time.Second, "the first sync", func() bool { return len(f.runs()) == 1 && !f.runs()[0].end.IsZero() })
	release, err := Lock(DefaultLockPath(a.GmiDir), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.SyncNow("personal")
	time.Sleep(2 * NudgeGap) // the sync waits on the flock past the gap
	release()
	waitFor(t, 3*time.Second, "the held sync to start", func() bool { return len(f.runs()) == 2 })
	e.Nudge("personal") // during that sync
	waitFor(t, 3*time.Second, "the nudged sync", func() bool { return f.count("sync") == 3 })
	rs := f.runs()
	if gap := rs[2].start.Sub(rs[1].start); gap < NudgeGap-spawnSlack {
		t.Fatalf("nudged sync %v after gmi started, under the gap", gap)
	}
}
