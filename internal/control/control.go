// Package control is the daemon's unix socket: how processes outside the
// browser (`pneu open`, later the bar widget) talk to the running server.
// Browser JavaScript can't reach a unix socket, so nothing a page does can
// send these commands; the boundary is the uid, checked at both ends with
// SO_PEERCRED and by the socket directory's owner and mode.
//
// One request line per connection, one reply line. Commands are a fixed
// set with no arguments (docs/client.md, "The control socket").
package control

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command is one request.
type Command string

const (
	// Launch queues a sync: the window is being opened or focused.
	Launch Command = "launch"
	// Status answers the daemon's build and instance (Info).
	Status Command = "status"
)

const (
	// MaxLine bounds a request or reply line, newline included.
	MaxLine = 1024
	// Timeout is each connection's deadline, at both ends.
	Timeout = 2 * time.Second
)

// ErrNoRuntimeDir: XDG_RUNTIME_DIR isn't set, so there is no socket.
var ErrNoRuntimeDir = errors.New("control: XDG_RUNTIME_DIR not set")

// SocketPath is $XDG_RUNTIME_DIR/pneu/control.
func SocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		return "", ErrNoRuntimeDir
	}
	return filepath.Join(dir, "pneu", "control"), nil
}

// Info is the Status reply.
type Info struct {
	Revision string `json:"revision"` // vcs.revision, "" when built without VCS info
	Modified bool   `json:"modified"` // vcs.modified: built from a dirty tree
	Instance string `json:"instance"` // random per process start: a restart changes it
}

var instance = func() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}()

// Self is this process's Info.
func Self() Info {
	in := Info{Instance: instance}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				in.Revision = s.Value
			case "vcs.modified":
				in.Modified = s.Value == "true"
			}
		}
	}
	return in
}

// Handler is what the commands do. Launch must be quick: it runs before the
// reply, so `pneu open` returns only once the sync is queued.
type Handler struct {
	Launch func()
}

// Server accepts commands on the socket until Close.
type Server struct {
	h   Handler
	ln  *net.UnixListener
	uid int // the only peer uid answered; os.Getuid() outside tests
	wg  sync.WaitGroup
}

// Listen checks the socket's directory, clears a stale socket of ours, and
// starts serving. It refuses (and serves nothing) when the directory isn't
// a 0700 directory of ours, or the path holds anything but our own socket.
func Listen(path string, h Handler) (*Server, error) {
	return listen(path, h, os.Getuid())
}

func listen(path string, h Handler, uid int) (*Server, error) {
	if err := prepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := clearStale(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	s := &Server{h: h, ln: ln, uid: uid}
	s.wg.Go(s.serve)
	return s, nil
}

// prepareDir creates dir 0700 if missing, then insists, via Lstat so a
// symlink is seen as one: a directory, ours, mode exactly 0700.
func prepareDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("control: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("control: %s is not a directory (%v); remove it", dir, fi.Mode().Type())
	}
	if uid, ok := ownerOf(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("control: %s is owned by uid %d, not us", dir, uid)
	}
	if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != 0o700 {
		return fmt.Errorf("control: %s has mode %v, want exactly 0700; chmod 700 it", dir, m)
	}
	return nil
}

func ownerOf(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	return int(st.Uid), true
}

// clearStale removes a socket left by an earlier run, and only that: our
// own socket nobody answers on. Anything else at path is left alone.
func clearStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("control: %s exists and isn't a socket; remove it", path)
	}
	if uid, ok := ownerOf(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("control: %s is owned by uid %d, not us", path, uid)
	}
	if c, err := net.DialTimeout("unix", path, Timeout); err == nil {
		c.Close()
		return fmt.Errorf("control: another pneu answers on %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("control: %w", err)
	}
	return nil
}

// Close stops accepting, removes the socket, and waits for connections in
// hand.
func (s *Server) Close() error {
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *Server) serve() {
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("control: %v", err)
			}
			return
		}
		s.wg.Go(func() { s.handle(c) })
	}
}

func (s *Server) handle(c *net.UnixConn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(Timeout))
	uid, err := peerUID(c)
	if err != nil || uid != s.uid {
		log.Printf("control: refused a connection from uid %d (%v)", uid, err)
		return
	}
	line, err := readLine(c)
	var reply string
	switch {
	case errors.Is(err, errTooLong):
		reply = "error request too long"
	case err != nil:
		return
	default:
		reply = s.answer(Command(line))
	}
	io.WriteString(c, reply+"\n")
}

func (s *Server) answer(cmd Command) string {
	switch cmd {
	case Launch:
		if s.h.Launch != nil {
			s.h.Launch()
		}
		return "ok"
	case Status:
		b, err := json.Marshal(Self())
		if err != nil {
			return "error " + err.Error()
		}
		return string(b)
	default:
		return "error unknown command"
	}
}

var errTooLong = errors.New("control: line too long")

// readLine reads one newline-terminated line of at most MaxLine bytes.
func readLine(c net.Conn) (string, error) {
	r := bufio.NewReaderSize(io.LimitReader(c, MaxLine), MaxLine)
	line, err := r.ReadString('\n')
	if err != nil {
		if len(line) >= MaxLine {
			return "", errTooLong
		}
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// peerUID is the connected process's uid (SO_PEERCRED): the kernel's
// answer, recorded at connect, that the peer can't forge.
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

// Send sends cmd to the daemon at path and returns its reply. It refuses a
// socket answered by another uid, and turns an `error …` reply into an
// error. timeout bounds the whole exchange, capped at Timeout.
func Send(path string, cmd Command, timeout time.Duration) (string, error) {
	return send(path, cmd, min(timeout, Timeout), os.Getuid())
}

func send(path string, cmd Command, timeout time.Duration, uid int) (string, error) {
	c, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return "", err
	}
	defer c.Close()
	uc := c.(*net.UnixConn)
	uc.SetDeadline(time.Now().Add(timeout))
	peer, err := peerUID(uc)
	if err != nil {
		return "", fmt.Errorf("control: %w", err)
	}
	if peer != uid {
		return "", fmt.Errorf("control: %s is answered by uid %d, not us", path, peer)
	}
	if _, err := io.WriteString(uc, string(cmd)+"\n"); err != nil {
		return "", err
	}
	reply, err := readLine(uc)
	if err != nil {
		return "", fmt.Errorf("control: reading the reply: %w", err)
	}
	if msg, ok := strings.CutPrefix(reply, "error "); ok {
		return "", fmt.Errorf("control: %s: %s", cmd, msg)
	}
	return reply, nil
}
