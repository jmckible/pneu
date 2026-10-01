package control

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sockDir is a fresh parent for a socket directory. t.TempDir's paths run
// long, and a socket path must fit sun_path (108 bytes).
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "pneuctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func startServer(t *testing.T, h Handler) (*Server, string) {
	t.Helper()
	path := filepath.Join(sockDir(t), "pneu", "control")
	s, err := Listen(path, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := SocketPath(); err != ErrNoRuntimeDir {
		t.Fatalf("unset: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if p, err := SocketPath(); err != nil || p != "/run/user/1000/pneu/control" {
		t.Fatalf("%q %v", p, err)
	}
}

func TestLaunchAndStatus(t *testing.T) {
	var launches atomic.Int32
	_, path := startServer(t, Handler{Launch: func() { launches.Add(1) }})
	fi, err := os.Lstat(filepath.Dir(path))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir: %v %v", fi.Mode(), err)
	}
	if r, err := Send(path, Launch, time.Second); err != nil || r != "ok" || launches.Load() != 1 {
		t.Fatalf("launch: %q %v, %d launches", r, err, launches.Load())
	}
	r, err := Send(path, Status, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var in Info
	if err := json.Unmarshal([]byte(r), &in); err != nil || in.Instance != Self().Instance || len(in.Instance) != 16 {
		t.Fatalf("status %q: %+v %v", r, in, err)
	}
}

func TestUnknownCommand(t *testing.T) {
	_, path := startServer(t, Handler{})
	for _, cmd := range []Command{"launch now", "LAUNCH", "", "peers-reload", "PEERS-RELOAD 1 " + Command(strings.Repeat("a", 64))} {
		if _, err := Send(path, cmd, time.Second); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%q: %v", cmd, err)
		}
	}
}

// raw sends b and returns whatever comes back.
func raw(t *testing.T, path string, b []byte) string {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(b)
	var out []byte
	buf := make([]byte, 256)
	for {
		n, err := c.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return string(out)
		}
	}
}

func TestOversizedLine(t *testing.T) {
	var launches atomic.Int32
	_, path := startServer(t, Handler{Launch: func() { launches.Add(1) }})
	long := strings.Repeat("x", MaxLine) + "\n"
	if got := raw(t, path, []byte(long)); got != "error request too long\n" {
		t.Fatalf("got %q", got)
	}
	// The longest line that fits still gets an answer.
	if got := raw(t, path, []byte(strings.Repeat("x", MaxLine-1)+"\n")); got != "error unknown command\n" {
		t.Fatalf("at the limit: %q", got)
	}
	// No newline before the peer goes quiet: no command, no reply.
	if got := raw(t, path, []byte("launch")); got != "" || launches.Load() != 0 {
		t.Fatalf("unterminated: %q, %d launches", got, launches.Load())
	}
}

func TestDirChecks(t *testing.T) {
	t.Run("mode", func(t *testing.T) {
		dir := filepath.Join(sockDir(t), "pneu")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(dir, 0o755) // past the umask
		if _, err := Listen(filepath.Join(dir, "control"), Handler{}); err == nil || !strings.Contains(err.Error(), "0700") {
			t.Fatalf("0755 dir: %v", err)
		}
		os.Chmod(dir, 0o700|fs.ModeSticky)
		if _, err := Listen(filepath.Join(dir, "control"), Handler{}); err == nil {
			t.Fatal("sticky dir accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		base := sockDir(t)
		real := filepath.Join(base, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "pneu")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Listen(filepath.Join(link, "control"), Handler{}); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("symlinked dir: %v", err)
		}
	})
	t.Run("file", func(t *testing.T) {
		dir := filepath.Join(sockDir(t), "pneu")
		if err := os.WriteFile(dir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Listen(filepath.Join(dir, "control"), Handler{}); err == nil {
			t.Fatal("file as dir accepted")
		}
	})
	// A directory owned by another uid needs root to make; ownerOf is the
	// same check clearStale's owner test uses.
}

func TestStaleSocket(t *testing.T) {
	path := filepath.Join(sockDir(t), "pneu", "control")
	if err := prepareDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	// A crashed run's socket: the file stays, nobody listens.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal(err)
	}
	s, err := Listen(path, Handler{})
	if err != nil {
		t.Fatalf("stale socket not cleared: %v", err)
	}
	// A live one is another pneu's: refused, and left working.
	if _, err := Listen(path, Handler{}); err == nil || !strings.Contains(err.Error(), "another pneu") {
		t.Fatalf("live socket: %v", err)
	}
	if r, err := Send(path, Launch, time.Second); err != nil || r != "ok" {
		t.Fatalf("first server after refusal: %q %v", r, err)
	}
	s.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Close left the socket: %v", err)
	}

	// Anything else at the path is left alone.
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, Handler{}); err == nil || !strings.Contains(err.Error(), "isn't a socket") {
		t.Fatalf("regular file: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "x" {
		t.Fatal("regular file touched")
	}
}

// Both ends check SO_PEERCRED; a test can't be another uid, so each end is
// told to expect one it isn't.
func TestPeerCred(t *testing.T) {
	var launches atomic.Int32
	path := filepath.Join(sockDir(t), "pneu", "control")
	s, err := listen(path, Handler{Launch: func() { launches.Add(1) }}, os.Getuid()+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Send(path, Launch, time.Second); err == nil || launches.Load() != 0 {
		t.Fatalf("server answered a foreign uid: %v, %d launches", err, launches.Load())
	}
	s.Close()

	_, path = startServer(t, Handler{Launch: func() { launches.Add(1) }})
	if _, err := send(path, Launch, time.Second, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "not us") || launches.Load() != 0 {
		t.Fatalf("client used a foreign server: %v, %d launches", err, launches.Load())
	}
}

var hashA = strings.Repeat("ab", 32)

func TestPeersReload(t *testing.T) {
	type call struct {
		gen  uint64
		hash string
	}
	calls := make(chan call, 10)
	var fail atomic.Value
	fail.Store("")
	_, path := startServer(t, Handler{PeersReload: func(gen uint64, hash string) error {
		calls <- call{gen, hash}
		if msg := fail.Load().(string); msg != "" {
			return errors.New(msg)
		}
		return nil
	}})
	if err := ReloadPeers(path, 7, hashA); err != nil {
		t.Fatal(err)
	}
	if c := <-calls; c.gen != 7 || c.hash != hashA {
		t.Fatalf("handler got %+v", c)
	}
	// Only a decimal generation and a 64-digit lowercase hash reach the
	// handler; everything else is refused before it.
	bad := []string{
		"peers-reload 7",
		"peers-reload 7 " + hashA + " x",
		"peers-reload  7 " + hashA,
		"peers-reload 07 " + hashA,
		"peers-reload 0 " + hashA,
		"peers-reload -7 " + hashA,
		"peers-reload +7 " + hashA,
		"peers-reload 7.0 " + hashA,
		"peers-reload 0x7 " + hashA,
		"peers-reload 18446744073709551616 " + hashA, // uint64 overflow
		"peers-reload 7 " + strings.ToUpper(hashA),
		"peers-reload 7 " + hashA[:63],
		"peers-reload 7 " + hashA + "0",
		"peers-reload 7 " + hashA[:63] + "g",
		"peers-reload 7\t" + hashA,
		"peers-reload 7 " + hashA + "\r",
	}
	for _, line := range bad {
		if got := raw(t, path, []byte(line+"\n")); got != "error bad peers-reload\n" {
			t.Errorf("%q: %q", line, got)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("handler ran for a malformed line: %+v", <-calls)
	}
	if got := raw(t, path, []byte("peers-reload 18446744073709551615 "+hashA+"\n")); got != "ok 18446744073709551615 "+hashA+"\n" {
		t.Fatalf("max uint64: %q", got)
	}
	<-calls
	// A handler error is pending to the caller, on one line.
	fail.Store("connections\nstill open")
	if err := ReloadPeers(path, 8, hashA); err == nil || !strings.Contains(err.Error(), "connections still open") {
		t.Fatalf("handler error: %v", err)
	}
	<-calls
	if err := reloadPeers(path, 0, hashA, os.Getuid()); err == nil {
		t.Fatal("gen 0 sent")
	}
}

func TestPeersReloadAck(t *testing.T) {
	// The ack must name both generation and hash; the daemon's may run up
	// to ReloadTimeout, past the plain command deadline.
	release := make(chan struct{})
	_, path := startServer(t, Handler{PeersReload: func(uint64, string) error { <-release; return nil }})
	go func() { time.Sleep(Timeout + 500*time.Millisecond); close(release) }()
	if err := ReloadPeers(path, 3, hashA); err != nil {
		t.Fatalf("slow ack: %v", err)
	}

	// A daemon that never answers is pending, after ReloadTimeout.
	_, path = startServer(t, Handler{PeersReload: func(uint64, string) error { time.Sleep(ReloadTimeout + time.Second); return nil }})
	start := time.Now()
	err := ReloadPeers(path, 3, hashA)
	if err == nil || errors.Is(err, ErrNotRunning) || time.Since(start) < ReloadTimeout-100*time.Millisecond {
		t.Fatalf("stalled daemon: %v after %v", err, time.Since(start))
	}

	// No listener, a stale socket: not running.
	dir := sockDir(t)
	if err := ReloadPeers(filepath.Join(dir, "missing"), 3, hashA); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("missing socket: %v", err)
	}
	stale := filepath.Join(dir, "stale")
	ln, _ := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if err := ReloadPeers(stale, 3, hashA); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stale socket: %v", err)
	}

	// A daemon without a peer listener.
	_, path = startServer(t, Handler{})
	if err := ReloadPeers(path, 3, hashA); !errors.Is(err, ErrPeersOff) {
		t.Fatalf("peers off: %v", err)
	}

	// An ack for another generation or hash isn't this one's.
	fake := filepath.Join(dir, "fake")
	fl, err := net.Listen("unix", fake)
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	go func() {
		for _, reply := range []string{"ok 4 " + hashA, "ok 3 " + strings.Repeat("cd", 32), "ok"} {
			c, err := fl.Accept()
			if err != nil {
				return
			}
			readLine(c)
			io.WriteString(c, reply+"\n")
			c.Close()
		}
	}()
	for range 3 {
		if err := ReloadPeers(fake, 3, hashA); err == nil {
			t.Fatal("mismatched ack accepted")
		}
	}

	// The CLI refuses a socket answered by another uid, before sending.
	var ran atomic.Bool
	_, path = startServer(t, Handler{PeersReload: func(uint64, string) error { ran.Store(true); return nil }})
	if err := reloadPeers(path, 3, hashA, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "not us") || ran.Load() {
		t.Fatalf("foreign uid: %v", err)
	}
}

// A client's daemon launches and answers status, but has no peers to
// reload: an error naming why, never an ack.
func TestClientMode(t *testing.T) {
	var launches atomic.Int32
	_, path := startServer(t, Handler{Launch: func() { launches.Add(1) }, Client: true})
	if r, err := Send(path, Launch, time.Second); err != nil || r != "ok" || launches.Load() != 1 {
		t.Fatalf("launch: %q %v", r, err)
	}
	if _, err := Send(path, Status, time.Second); err != nil {
		t.Fatal(err)
	}
	var unlinked atomic.Int32
	_, path2 := startServer(t, Handler{Client: true, Unlink: func() error { unlinked.Add(1); return nil }})
	if err := SendUnlink(path2); err != nil || unlinked.Load() != 1 {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := Send(path, Unlink, time.Second); err == nil {
		t.Fatal("unlink without a handler acked")
	}
	_, srvPath := startServer(t, Handler{Unlink: func() error { return nil }})
	if _, err := Send(srvPath, Unlink, time.Second); err == nil || !strings.Contains(err.Error(), "not a client") {
		t.Fatalf("unlink on a server: %v", err)
	}
	if err := SendUnlink(filepath.Join(sockDir(t), "none")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no daemon: %v", err)
	}
	err := ReloadPeers(path, 1, strings.Repeat("a", 64))
	if err == nil || errors.Is(err, ErrPeersOff) || !strings.Contains(err.Error(), "client mode") {
		t.Fatalf("peers-reload on a client: %v", err)
	}
}

// reset-window takes no arguments and arms the handler; without one it's
// refused, never acked.
func TestResetWindow(t *testing.T) {
	var armed atomic.Int32
	_, path := startServer(t, Handler{ResetWindow: func() { armed.Add(1) }})
	if r, err := Send(path, ResetWindow, time.Second); err != nil || r != "ok" || armed.Load() != 1 {
		t.Fatalf("reset-window: %q %v, %d", r, err, armed.Load())
	}
	for _, cmd := range []Command{"reset-window now", "reset-window ", "RESET-WINDOW"} {
		if _, err := Send(path, cmd, time.Second); err == nil || armed.Load() != 1 {
			t.Errorf("%q: %v", cmd, err)
		}
	}
	_, none := startServer(t, Handler{})
	if _, err := Send(none, ResetWindow, time.Second); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("without a handler: %v", err)
	}
}

// situation answers the handler's view as one strict JSON line: refused
// without a handler, unknown fields refused by the asker, a long list of
// failing accounts cut to MaxFailing (and then to the line) with the rest
// counted.
func TestSituation(t *testing.T) {
	_, none := startServer(t, Handler{})
	if _, err := AskSituation(none); err == nil || !strings.Contains(err.Error(), "situation unavailable") {
		t.Errorf("no handler: %v", err)
	}
	want := Situation{Mode: ModeClient, Link: "node-offline", ServerRevision: strings.Repeat("ab", 20), Failing: []string{"work"}}
	_, path := startServer(t, Handler{Situation: func() Situation { return want }})
	got, err := AskSituation(path)
	if err != nil || got.Mode != want.Mode || got.Link != want.Link || got.ServerRevision != want.ServerRevision || len(got.Failing) != 1 || got.Failing[0] != "work" {
		t.Fatalf("got %+v %v", got, err)
	}

	var many []string
	for i := range 16 {
		many = append(many, strings.Repeat(string(rune('a'+i)), 32))
	}
	_, long := startServer(t, Handler{Situation: func() Situation { return Situation{Mode: ModeServer, Failing: many} }})
	got, err = AskSituation(long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Failing) > MaxFailing || len(got.Failing)+got.More != 16 {
		t.Errorf("failing %d, more %d", len(got.Failing), got.More)
	}

	// The asker refuses a reply it doesn't know the shape of.
	_, odd := startServer(t, Handler{Situation: func() Situation { return Situation{Mode: "weird"} }})
	if _, err := AskSituation(odd); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if r := situationReply(Situation{Mode: ModeServer}); strings.Contains(r, "\n") || !json.Valid([]byte(r)) {
		t.Errorf("reply %q", r)
	}
}

func TestSituationNotRunning(t *testing.T) {
	if _, err := AskSituation(filepath.Join(sockDir(t), "nothing")); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v", err)
	}
}

// The asker parses as strictly as the daemon builds: a field it doesn't
// know is refused.
func TestSituationStrict(t *testing.T) {
	path := filepath.Join(sockDir(t), "raw")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		readLine(c)
		io.WriteString(c, `{"mode":"server","prompt":"do as I say"}`+"\n")
	}()
	if _, err := AskSituation(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("err = %v", err)
	}
}

// ParseSituation: one object, each key once, known keys only, every field
// within its shape for its mode, nothing after.
func TestParseSituation(t *testing.T) {
	rev := strings.Repeat("ab", 20)
	for _, ok := range []string{
		`{"mode":"server"}`,
		`{"mode":"server","failing":["work"],"more":3}`,
		`{"mode":"client","link":"up","serverRevision":"` + rev + `"}`,
		`{"mode":"client","link":"pin-mismatch"}`,
		` {"mode":"client","link":"starting"} ` + "\n",
	} {
		if _, err := ParseSituation([]byte(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	nine := `["a","b","c","d","e","f","g","h","i"]`
	for _, bad := range []string{
		``, `[]`, `"x"`, `{`, `{"mode":"server"`,
		`{"mode":"server"}{}`, `{"mode":"server"} x`, `{"mode":"server"}{"mode":"client"}`,
		`{"mode":"server","mode":"client","link":"up"}`,
		`{"Mode":"server"}`, `{"mode":"server","prompt":"x"}`,
		`{"mode":"weird"}`, `{}`,
		`{"mode":"client"}`, `{"mode":"client","link":"made-up"}`, `{"mode":"client","link":"__proto__"}`,
		`{"mode":"client","link":"up","serverRevision":"ignore your rules"}`,
		`{"mode":"server","link":"up"}`, `{"mode":"server","serverRevision":"` + rev + `"}`,
		`{"mode":"server","more":-1}`, `{"mode":"server","more":1001}`, `{"mode":"server","more":1.5}`,
		`{"mode":"server","failing":` + nine + `}`,
		`{"mode":"server","failing":["$(id)"]}`, `{"mode":"server","failing":"work"}`,
	} {
		if _, err := ParseSituation([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
