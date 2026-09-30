package gmi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// A real run's Result carries the stream scan: the markers survive a tail
// that has long dropped them, and a run that exited on its own is Scanned.
func TestSendScanned(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_EXIT", "1")
	t.Setenv("FAKEGMI_OUTPUT", "sending message, from: a@b..\nreceiving content (1) ...\n"+strings.Repeat("Traceback line\n", 2000))
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	r, err := e.Send(context.Background(), "personal", strings.NewReader(rawMsg))
	if err == nil || strings.Contains(r.Output, "receiving content") {
		t.Fatalf("err %v; tail still holds the marker", err)
	}
	if !r.Scanned || !r.SendLine || !r.AcceptLine || !r.Accepted() || r.NotSent() {
		t.Errorf("result %+v", r)
	}
	t.Setenv("FAKEGMI_OUTPUT", "Traceback\nValueError: Recipients passed")
	r, err = e.Send(context.Background(), "personal", strings.NewReader(rawMsg))
	if err == nil || !r.Scanned || r.SendLine || !r.NotSent() {
		t.Errorf("failed before the send line: %v %+v", err, r)
	}
}

// fakeScript writes an executable gmi stand-in.
func fakeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gmi")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A descendant still holding the output after gmi exits means the scan
// may have missed lines: not Scanned, so never NotSent, whatever Wait says.
func TestSendScannedNeedsEOF(t *testing.T) {
	f := newFixture(t)
	old := pipeDrainWait
	pipeDrainWait = 200 * time.Millisecond
	defer func() { pipeDrainWait = old }()
	o := f.opts(Options{})
	o.GmiPath = fakeScript(t, "#!/bin/sh\ncat >/dev/null\necho Traceback\n(sleep 2; echo 'sending message, from: late') &\nexit 1\n")
	e := mustNew(t, []Account{f.account("personal")}, o)
	r, err := e.Send(context.Background(), "personal", strings.NewReader(rawMsg))
	if err == nil || r.ExitCode != 1 || r.Scanned || r.NotSent() {
		t.Errorf("err %v, result %+v", err, r)
	}
}

// Every run is UTF-8 and unbuffered, whatever pneu's own environment says:
// under an inherited utf-16, lieer's markers wouldn't match.
func TestGmiPythonEnv(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	t.Setenv("PYTHONIOENCODING", "utf-16")
	t.Setenv("PYTHONUTF8", "0")
	f := newFixture(t)
	o := f.opts(Options{})
	o.GmiPath = fakeScript(t, "#!/usr/bin/env python3\nimport os, sys\nsys.stdin.read()\n"+
		"print('env', os.environ.get('PYTHONIOENCODING'), os.environ.get('PYTHONUTF8'), os.environ.get('PYTHONUNBUFFERED'))\n"+
		"print('sending message, from: a@b..')\nprint('receiving content (1) ...')\nsys.exit(1)\n")
	e := mustNew(t, []Account{f.account("personal")}, o)
	r, _ := e.Send(context.Background(), "personal", strings.NewReader(rawMsg))
	if !strings.Contains(r.Output, "env utf-8 1 1") || !r.SendLine || !r.AcceptLine || !r.Accepted() || r.NotSent() {
		t.Errorf("result %+v", r)
	}
}
