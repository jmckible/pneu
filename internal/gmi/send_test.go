package gmi

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const rawMsg = "From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\nbody\r\n"

func TestSendArgsAndStdin(t *testing.T) {
	f := newFixture(t)
	a := f.account("personal")
	e := mustNew(t, []Account{a}, f.opts(Options{}))
	// Not running: Send doesn't need the loop, and its SyncNow queues.
	r, err := e.Send(context.Background(), "personal", strings.NewReader(rawMsg))
	if err != nil {
		t.Fatalf("send: %v (%+v)", err, r)
	}
	if r.Op != OpSend || r.ExitCode != 0 || r.Account != "personal" {
		t.Errorf("result %+v", r)
	}
	en := f.entries()
	if len(en) != 2 || en[0].args != "send -t" || en[0].cwd != a.GmiDir || en[0].nmc != a.NotmuchConfig {
		t.Fatalf("invocations %+v", en)
	}
	got, err := os.ReadFile(f.log + ".stdin")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != rawMsg {
		t.Errorf("stdin = %q", got)
	}
	select {
	case <-e.accts["personal"].syncReq:
	default:
		t.Error("successful send didn't queue a sync")
	}
	if st, _ := e.Status("personal"); st.Failures != 0 || !st.LastSync.IsZero() || st.Running {
		t.Errorf("send touched sync status: %+v", st)
	}
}

func TestSendFailure(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_EXIT", "1")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	_, err := e.Send(context.Background(), "personal", strings.NewReader(rawMsg))
	if err == nil || !strings.Contains(err.Error(), "fakegmi: failing with 1") {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-e.accts["personal"].syncReq:
		t.Error("failed send queued a sync")
	default:
	}
	if st, _ := e.Status("personal"); st.Failures != 0 {
		t.Errorf("a failed send counts as a sync failure: %+v", st)
	}
	if _, err := e.Send(context.Background(), "nope", strings.NewReader(rawMsg)); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("unknown account: %v", err)
	}
}

// lieer's own .lock would make a send during a sync fail or stall inside
// PushTimeout; the send must queue behind the sync on our lock instead.
func TestSendWaitsForSync(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", "0.4")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 2*time.Second, "sync start", func() bool { return len(f.entries()) > 0 })
	if _, err := e.Send(context.Background(), "personal", strings.NewReader(rawMsg)); err != nil {
		t.Fatal(err)
	}
	// The send queues a sync; wait for it so all three runs are logged.
	waitFor(t, 3*time.Second, "sync after send", func() bool { return f.count("sync") == 2 && len(f.entries()) == 6 })
	rs := f.runs()
	if len(rs) != 3 || rs[0].op != "sync" || rs[1].op != "send -t" || rs[2].op != "sync" {
		t.Fatalf("runs = %+v, want sync, send, sync", rs)
	}
	assertSerialized(t, rs)
}

func TestSendLockWaitHonoursContext(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", "0.5")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 2*time.Second, "sync start", func() bool { return len(f.entries()) > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.Send(ctx, "personal", strings.NewReader(rawMsg)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if n := f.count("send -t"); n != 0 {
		t.Errorf("gave-up send still ran %d times", n)
	}
}
