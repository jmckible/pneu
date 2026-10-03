package control

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var hashP = strings.Repeat("ab", 32)

func TestPushReload(t *testing.T) {
	type call struct {
		gen  uint64
		hash string
	}
	calls := make(chan call, 10)
	var outcome atomic.Int32
	outcome.Store(int32(ReloadApplied))
	var fail atomic.Value
	fail.Store("")
	_, path := startServer(t, Handler{PushReload: func(gen uint64, hash string) (Reload, error) {
		calls <- call{gen, hash}
		if msg := fail.Load().(string); msg != "" {
			return 0, errors.New(msg)
		}
		return Reload(outcome.Load()), nil
	}})
	if r, err := ReloadPush(path, 7, hashP); err != nil || r != ReloadApplied {
		t.Fatal(r, err)
	}
	if c := <-calls; c.gen != 7 || c.hash != hashP {
		t.Fatalf("handler got %+v", c)
	}
	if got := raw(t, path, []byte("push-reload 7 "+hashP+"\n")); got != "ok 7\n" {
		t.Fatalf("wire ack %q", got)
	}
	<-calls
	for _, o := range []Reload{ReloadStale, ReloadMismatch} {
		outcome.Store(int32(o))
		if r, err := ReloadPush(path, 8, hashP); err != nil || r != o {
			t.Fatalf("%v: %v %v", o, r, err)
		}
		<-calls
	}
	if got := raw(t, path, []byte("push-reload 9 "+hashP+"\n")); got != "mismatch\n" {
		t.Fatalf("wire mismatch %q", got)
	}
	<-calls

	// Only a decimal generation and a 64-digit lowercase hash reach the
	// handler.
	for _, line := range []string{
		"push-reload 7",
		"push-reload 7 " + hashP + " x",
		"push-reload  7 " + hashP,
		"push-reload 07 " + hashP,
		"push-reload 0 " + hashP,
		"push-reload -7 " + hashP,
		"push-reload 7.0 " + hashP,
		"push-reload 18446744073709551616 " + hashP,
		"push-reload 7 " + strings.ToUpper(hashP),
		"push-reload 7 " + hashP[:63],
		"push-reload 7 " + hashP + "0",
		"push-reload 7\t" + hashP,
		"push-reload 7 " + hashP + "\r",
		"push-reload 9007199254740993 " + hashP, // past state.json's generation bound
		"push-reload 18446744073709551615 " + hashP,
	} {
		if got := raw(t, path, []byte(line+"\n")); got != "error bad push-reload\n" {
			t.Errorf("%q: %q", line, got)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("handler ran for a malformed line: %+v", <-calls)
	}
	// A handler error is an error, never an ack, on one line.
	outcome.Store(int32(ReloadApplied))
	fail.Store("state.json\nunreadable")
	if _, err := ReloadPush(path, 10, hashP); err == nil || !strings.Contains(err.Error(), "state.json unreadable") {
		t.Fatalf("handler error: %v", err)
	}
	<-calls
	fail.Store("")
	// A handler with no outcome is an error too.
	outcome.Store(0)
	if _, err := ReloadPush(path, 10, hashP); err == nil {
		t.Fatal("no outcome acked")
	}
	<-calls
	if _, err := ReloadPush(path, 0, hashP); err == nil {
		t.Fatal("gen 0 sent")
	}
	if _, err := ReloadPush(path, 1<<53+1, hashP); err == nil {
		t.Fatal("gen past state.json's bound sent")
	}
	if len(calls) != 0 {
		t.Fatal("handler ran for a refused send")
	}
}

func TestPushReloadAck(t *testing.T) {
	// The handler may take past the plain deadline, up to ReloadTimeout.
	release := make(chan struct{})
	_, path := startServer(t, Handler{PushReload: func(uint64, string) (Reload, error) { <-release; return ReloadApplied, nil }})
	go func() { time.Sleep(Timeout + 500*time.Millisecond); close(release) }()
	if r, err := ReloadPush(path, 3, hashP); err != nil || r != ReloadApplied {
		t.Fatalf("slow ack: %v %v", r, err)
	}

	// No ack within ReloadTimeout: an error, not ErrNotRunning (pending).
	_, path = startServer(t, Handler{PushReload: func(uint64, string) (Reload, error) {
		time.Sleep(ReloadTimeout + time.Second)
		return ReloadApplied, nil
	}})
	start := time.Now()
	_, err := ReloadPush(path, 3, hashP)
	if err == nil || errors.Is(err, ErrNotRunning) || time.Since(start) < ReloadTimeout-100*time.Millisecond {
		t.Fatalf("stalled daemon: %v after %v", err, time.Since(start))
	}

	// Nothing on the socket.
	dir := sockDir(t)
	if _, err := ReloadPush(filepath.Join(dir, "missing"), 3, hashP); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("missing socket: %v", err)
	}
	// No push handler, or a client's daemon: push off.
	_, path = startServer(t, Handler{})
	if _, err := ReloadPush(path, 3, hashP); !errors.Is(err, ErrPushOff) {
		t.Fatalf("push off: %v", err)
	}
	var ran atomic.Bool
	_, path = startServer(t, Handler{Client: true, PushReload: func(uint64, string) (Reload, error) { ran.Store(true); return ReloadApplied, nil }})
	if _, err := ReloadPush(path, 3, hashP); !errors.Is(err, ErrPushOff) || ran.Load() {
		t.Fatalf("client: %v", err)
	}

	// An ack naming another generation, or anything else, isn't this one's.
	fake := filepath.Join(dir, "fake")
	fl, err := net.Listen("unix", fake)
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	replies := []string{"ok 4", "ok 03", "ok", "ok 3 " + hashP, "STALE", "applied", ""}
	go func() {
		for _, reply := range replies {
			c, err := fl.Accept()
			if err != nil {
				return
			}
			readLine(c)
			io.WriteString(c, reply+"\n")
			c.Close()
		}
	}()
	for _, reply := range replies {
		if r, err := ReloadPush(fake, 3, hashP); err == nil {
			t.Fatalf("reply %q accepted as %v", reply, r)
		}
	}

	// A socket answered by another uid is refused before sending.
	ran.Store(false)
	_, path = startServer(t, Handler{PushReload: func(uint64, string) (Reload, error) { ran.Store(true); return ReloadApplied, nil }})
	if _, err := reloadPush(path, 3, hashP, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "not us") || ran.Load() {
		t.Fatalf("foreign uid: %v", err)
	}
}

func TestPushState(t *testing.T) {
	delivered := time.Date(2026, 10, 3, 12, 30, 15, 500e6, time.UTC)
	asked := make(chan string, 10)
	states := map[string]PushState{
		"personal": {Generation: 7, State: PushDelivering, LastDelivery: delivered},
		"vocal":    {Generation: 7, State: PushReauth, Reason: ReasonMailboxReauth},
		"bad":      {Generation: 7, State: PushQuiet, Reason: ReasonNetwork}, // a handler bug: refused, not sent
	}
	_, path := startServer(t, Handler{PushState: func(account string) PushState {
		asked <- account
		if p, ok := states[account]; ok {
			return p
		}
		return PushState{Generation: 7, State: PushOff}
	}})
	p, err := AskPushState(path, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if p.Instance != Instance() || p.Generation != 7 || p.State != PushDelivering || !p.LastDelivery.Equal(delivered.Truncate(time.Second)) {
		t.Fatalf("%+v", p)
	}
	if <-asked != "personal" {
		t.Fatal("asked")
	}
	if got := raw(t, path, []byte("push-state personal\n")); got != `{"instance":"`+Instance()+`","generation":7,"state":"delivering","lastDelivery":"2026-10-03T12:30:15Z"}`+"\n" {
		t.Fatalf("wire: %q", got)
	}
	<-asked
	if p, err := AskPushState(path, "vocal"); err != nil || p.State != PushReauth || p.Reason != ReasonMailboxReauth {
		t.Fatal(p, err)
	}
	<-asked
	if p, err := AskPushState(path, "other"); err != nil || p.State != PushOff {
		t.Fatal(p, err)
	}
	<-asked
	if _, err := AskPushState(path, "bad"); err == nil {
		t.Fatal("an out-of-shape state was sent")
	}
	<-asked

	// The argument is an account name, checked before the handler.
	for _, line := range []string{"push-state", "push-state ", "push-state ../x", "push-state a b",
		"push-state " + strings.Repeat("a", 33), "push-state .x", "push-state a\r"} {
		got := raw(t, path, []byte(line+"\n"))
		if got != "error bad push-state\n" && got != "error unknown command\n" {
			t.Errorf("%q: %q", line, got)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("handler ran for %q", <-asked)
	}
	if _, err := AskPushState(path, "a b"); err == nil {
		t.Fatal("bad name sent")
	}
	_, off := startServer(t, Handler{})
	if _, err := AskPushState(off, "personal"); !errors.Is(err, ErrPushOff) {
		t.Fatalf("push off: %v", err)
	}
	if _, err := AskPushState(filepath.Join(sockDir(t), "none"), "personal"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("not running: %v", err)
	}
}

func TestParsePushState(t *testing.T) {
	inst := Instance()
	good := `{"instance":"` + inst + `","generation":3,"state":"failing","reason":"watch-expired","lastDelivery":"2026-10-03T12:00:00Z"}`
	if p, err := ParsePushState([]byte(good)); err != nil || p.Reason != ReasonWatchExpired {
		t.Fatal(p, err)
	}
	for _, bad := range []string{
		`{"instance":"` + inst + `","generation":3,"state":"failing","reason":"watch-expired","reason":"network"}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet","extra":1}`,
		`{"instance":"` + inst + `","generation":3}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet"} {}`,
		`{"instance":"` + inst + `","generation":3,"state":"Quiet"}`,
		`{"instance":"` + inst + `","generation":3,"state":"asleep"}`,
		`{"instance":"` + inst + `","generation":3,"state":"failing"}`,
		`{"instance":"` + inst + `","generation":3,"state":"failing","reason":"owner-reauth"}`,
		`{"instance":"` + inst + `","generation":3,"state":"reauth","reason":"network"}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet","reason":"network"}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet","reason":""}`,
		`{"instance":"` + inst + `","generation":3,"state":"failing","reason":"Google says no"}`,
		`{"instance":"` + inst + `","generation":-1,"state":"quiet"}`,
		`{"instance":"` + inst + `","generation":9007199254740993,"state":"quiet"}`,
		`{"instance":"` + inst + `","generation":"3","state":"quiet"}`,
		`{"instance":"XYZ","generation":3,"state":"quiet"}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet","lastDelivery":"2026-10-03 12:00:00"}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet","lastDelivery":"2026-10-03T14:00:00+02:00"}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet","lastDelivery":"1999-10-03T12:00:00Z"}`,
		`{"instance":"` + inst + `","generation":3,"state":"quiet","lastDelivery":null}`,
		`[]`,
	} {
		if _, err := ParsePushState([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
