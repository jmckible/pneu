// Package control is the daemon's unix socket: how processes outside the
// browser (`pneu open`, later the bar widget) talk to the running server.
// Browser JavaScript can't reach a unix socket, so nothing a page does can
// send these commands; the boundary is the uid, checked at both ends with
// SO_PEERCRED and by the socket directory's owner and mode.
//
// One request line per connection, one reply line. Commands are a fixed
// set; the only arguments are peers-reload's decimal generation and hex
// hash, validated to exactly that shape (docs/client.md, "The control
// socket").
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
	"strconv"
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
	// Unlink, to a client's daemon only (pneu client unpair): forget the
	// pairing and close the link's connections; ok once they're closed.
	Unlink Command = "unlink"
	// PeersReload, as "peers-reload <generation> <hash>", asks the daemon to
	// make that generation of peers.json live (ReloadPeers).
	PeersReload Command = "peers-reload"
)

const (
	// MaxLine bounds a request or reply line, newline included.
	MaxLine = 1024
	// Timeout is each connection's deadline, at both ends.
	Timeout = 2 * time.Second
	// ReloadTimeout is peers-reload's, at both ends: the daemon answers once
	// the generation is live and a removed peer's connections are closed.
	ReloadTimeout = 5 * time.Second
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
// reply, so `pneu open` returns only once the sync is queued. PeersReload
// (nil: no peer listener) returns once gen is live, or an error; it must
// return well inside ReloadTimeout.
type Handler struct {
	Launch      func()
	PeersReload func(gen uint64, hash string) error
	// Client: the daemon is a client's, which has no peers to reload.
	Client bool
	// Unlink (client only) returns once the link's connections are closed.
	Unlink func() error
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
		if strings.HasPrefix(line, string(PeersReload)+" ") {
			c.SetDeadline(time.Now().Add(ReloadTimeout))
		}
		reply = s.answer(Command(line))
	}
	io.WriteString(c, reply+"\n")
}

func (s *Server) answer(cmd Command) string {
	if args, ok := strings.CutPrefix(string(cmd), string(PeersReload)+" "); ok {
		gen, hash, ok := parseReload(args)
		switch {
		case !ok:
			return "error bad peers-reload"
		case s.h.Client:
			return "error " + clientMode
		case s.h.PeersReload == nil:
			return "error " + peersOff
		}
		if err := s.h.PeersReload(gen, hash); err != nil {
			return "error " + oneLine(err.Error())
		}
		return reloadAck(gen, hash)
	}
	switch cmd {
	case Launch:
		if s.h.Launch != nil {
			s.h.Launch()
		}
		return "ok"
	case Unlink:
		if !s.h.Client || s.h.Unlink == nil {
			return "error not a client"
		}
		if err := s.h.Unlink(); err != nil {
			return "error " + oneLine(err.Error())
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

// peersOff is the reply when the daemon runs no peer listener; clientMode,
// when it's a client's.
const (
	peersOff   = "peers off"
	clientMode = "client mode: peers are paired on the server"
)

var (
	// ErrNotRunning: nothing answers on the socket, so no daemon has a peer
	// listener up (it won't run one without this socket); the file applies
	// at the next start.
	ErrNotRunning = errors.New("control: pneu isn't running")
	// ErrPeersOff: the daemon runs without a peer listener.
	ErrPeersOff = errors.New("control: the running pneu has no peer listener")
)

func reloadAck(gen uint64, hash string) string { return fmt.Sprintf("ok %d %s", gen, hash) }

// parseReload reads "<generation> <hash>": a decimal uint64 with no sign
// or leading zero, and 64 lowercase hex digits. Nothing else passes.
func parseReload(args string) (uint64, string, bool) {
	g, hash, ok := strings.Cut(args, " ")
	if !ok || !validGen(g) || !ValidHash(hash) {
		return 0, "", false
	}
	gen, err := strconv.ParseUint(g, 10, 64)
	if err != nil || gen == 0 {
		return 0, "", false
	}
	return gen, hash, true
}

func validGen(g string) bool {
	if g == "" || len(g) > 20 || g[0] == '0' {
		return false
	}
	for i := 0; i < len(g); i++ {
		if g[i] < '0' || g[i] > '9' {
			return false
		}
	}
	return true
}

// ValidHash reports whether h is 64 lowercase hex digits (a SHA-256).
func ValidHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// oneLine keeps an error on the reply's one line, within MaxLine.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// ReloadPeers asks the daemon at path to make generation gen (content hash
// hash) of peers.json live, and waits up to ReloadTimeout for the ack that
// names both. ErrNotRunning when nothing answers; ErrPeersOff when the
// daemon runs no peer listener; any other error means the outcome is
// unknown: pending, never done.
func ReloadPeers(path string, gen uint64, hash string) error {
	return reloadPeers(path, gen, hash, os.Getuid())
}

func reloadPeers(path string, gen uint64, hash string, uid int) error {
	if gen == 0 || !ValidHash(hash) {
		return errors.New("control: bad generation or hash")
	}
	c, err := net.DialTimeout("unix", path, Timeout)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w (%v)", ErrNotRunning, err)
		}
		return err
	}
	defer c.Close()
	uc := c.(*net.UnixConn)
	uc.SetDeadline(time.Now().Add(ReloadTimeout))
	peer, err := peerUID(uc)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	if peer != uid {
		return fmt.Errorf("control: %s is answered by uid %d, not us", path, peer)
	}
	if _, err := fmt.Fprintf(uc, "%s %d %s\n", PeersReload, gen, hash); err != nil {
		return err
	}
	reply, err := readLine(uc)
	if err != nil {
		return fmt.Errorf("control: no acknowledgment: %w", err)
	}
	if reply == "error "+peersOff {
		return ErrPeersOff
	}
	if msg, ok := strings.CutPrefix(reply, "error "); ok {
		return fmt.Errorf("control: peers-reload: %s", msg)
	}
	if reply != reloadAck(gen, hash) {
		return fmt.Errorf("control: peers-reload: acknowledgment %q names another generation", reply)
	}
	return nil
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

// SendUnlink asks a client's daemon to forget its pairing, and returns
// once it has closed the link's connections. ErrNotRunning when nothing
// answers; any other error means it may still hold them.
func SendUnlink(path string) error {
	_, err := Send(path, Unlink, Timeout)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	return err
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
