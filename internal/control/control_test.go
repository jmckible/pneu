package control

import (
	"encoding/json"
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
	for _, cmd := range []Command{"launch now", "LAUNCH", "", "peers-reload 3"} {
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
