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
	"bytes"
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
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
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
	// ResetWindow arms Clear-Site-Data on the next navigation the daemon
	// answers (the bar menu's Reset window data, docs/client.md N3).
	ResetWindow Command = "reset-window"
	// PeersReload, as "peers-reload <generation> <hash>", asks the daemon to
	// make that generation of peers.json live (ReloadPeers).
	PeersReload Command = "peers-reload"
	// SituationCmd answers what an agent callout needs to know (Situation),
	// in local codes: `pneu agent`, from the bar menu's Fix with agent.
	SituationCmd Command = "situation"
	// UpdateChecked tells a client's daemon that `pneu update` wrote the
	// skew cache: reread it (docs/client.md, "What skews"). No arguments;
	// the daemon reads only its own file.
	UpdateChecked Command = "update-checked"
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
	// Listening: the daemon's loopback HTTP listeners are bound and served
	// (a client's daemon also built), so an HTTP answer carrying Instance
	// is this process's (pneu update's readiness check, N13).
	Listening bool `json:"listening"`
	// Legacy: the reply had no listening field, a daemon from before
	// pneu update's readiness check (its HTTP answers name no instance).
	// Set by ParseInfo, never sent.
	Legacy bool `json:"-"`
}

// listening is MarkListening's.
var listening atomic.Bool

// MarkListening says this process's loopback HTTP is up: from here on
// Status answers listening:true.
func MarkListening() { listening.Store(true) }

// Instance is this process's random id, the one Status answers and every
// HTTP answer carries in Pneu-Instance. Not a secret: it only tells one
// process start from another.
func Instance() string { return instance }

var instance = func() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}()

// Self is this process's Info.
func Self() Info {
	in := Info{Instance: instance, Listening: listening.Load()}
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

// Situation is the situation reply: the daemon's own view of what's
// wrong, in local codes and names only (docs/client.md, "The action
// menu"). The CLI validates every field again before any of it reaches a
// prompt.
type Situation struct {
	Mode string `json:"mode"` // "server" | "client"
	// Link is a client's link state as its local reason code (link.Reason:
	// "up", "starting", "tailscale-down", …); empty on a server.
	Link string `json:"link,omitempty"`
	// ServerRevision is the server's vcs.revision from the last hello, only
	// as 40 hex digits (the link keeps nothing else); "" when unknown.
	ServerRevision string `json:"serverRevision,omitempty"`
	// Failing names accounts whose sync is failing (web.Sick), at most
	// MaxFailing of them; More counts the rest.
	Failing []string `json:"failing,omitempty"`
	More    int      `json:"more,omitempty"`
	// Reauth counts accounts whose Gmail access expired or was revoked
	// (state reauth), among the failing: the fix is a consent, not a
	// diagnosis, so it has its own situation.
	Reauth int `json:"reauth,omitempty"`
	// Update is a client's skew state (internal/update: client-older,
	// server-older, different); "" when the builds are the same or no
	// hello has said the server's.
	Update string `json:"update,omitempty"`
}

// MaxFailing bounds Situation.Failing, so the reply fits one line:
// account names are at most 32 bytes (config.ValidName).
const MaxFailing = 8

// Situation modes.
const (
	ModeServer = "server"
	ModeClient = "client"
)

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
	// ResetWindow arms the next navigation's Clear-Site-Data; nil: refused.
	ResetWindow func()
	// Situation is the daemon's view for an agent callout; nil: refused.
	Situation func() Situation
	// UpdateChecked (client only) rereads the skew cache; nil: refused.
	UpdateChecked func()
	// Status answers Status; nil: Self(). A seam for tests that play a
	// daemon of another build.
	Status func() Info
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
	case ResetWindow:
		if s.h.ResetWindow == nil {
			return "error reset-window unavailable"
		}
		s.h.ResetWindow()
		return "ok"
	case SituationCmd:
		if s.h.Situation == nil {
			return "error situation unavailable"
		}
		return situationReply(s.h.Situation())
	case UpdateChecked:
		if !s.h.Client || s.h.UpdateChecked == nil {
			return "error not a client"
		}
		s.h.UpdateChecked()
		return "ok"
	case Status:
		in := Self()
		if s.h.Status != nil {
			in = s.h.Status()
		}
		var b []byte
		var err error
		if in.Legacy {
			// Only a test's Status plays a daemon from before listening.
			b, err = json.Marshal(struct {
				Revision string `json:"revision"`
				Modified bool   `json:"modified"`
				Instance string `json:"instance"`
			}{in.Revision, in.Modified, in.Instance})
		} else {
			b, err = json.Marshal(in)
		}
		if err != nil {
			return "error " + err.Error()
		}
		return string(b)
	default:
		return "error unknown command"
	}
}

// situationReply encodes sit on one line within MaxLine: at most
// MaxFailing names, then fewer while it's still too long, the dropped ones
// counted in More.
func situationReply(sit Situation) string {
	if len(sit.Failing) > MaxFailing {
		sit.More += len(sit.Failing) - MaxFailing
		sit.Failing = sit.Failing[:MaxFailing]
	}
	for {
		b, err := json.Marshal(sit)
		if err != nil {
			return "error " + oneLine(err.Error())
		}
		if len(b) < MaxLine-1 || len(sit.Failing) == 0 {
			if len(b) >= MaxLine-1 {
				return "error situation too long"
			}
			return string(b)
		}
		sit.Failing = sit.Failing[:len(sit.Failing)-1]
		sit.More++
	}
}

// LinkCodes are the link's local reason codes (internal/link's Reason
// values; a test there keeps the two in step): the only values a client's
// Situation.Link may take.
var LinkCodes = []string{"starting", "up", "tailscale-down", "node-offline", "node-mismatch", "refused", "pin-mismatch", "not-paired", "protocol"}

// UpdateCodes are the skew states (internal/update; a test there keeps
// the two in step): the only values Situation.Update may take.
var UpdateCodes = []string{"client-older", "server-older", "different"}

// MaxMore bounds Situation.More: a config holds far fewer accounts.
const MaxMore = 1000

var revisionHex = regexp.MustCompile(`^[0-9a-f]{40}$`)

// AskSituation asks the daemon at path for its Situation, and parses the
// reply as strictly as it's built: one JSON object and nothing after it,
// each known key at most once and no other, every field within its shape
// (ParseSituation).
func AskSituation(path string) (Situation, error) {
	reply, err := Send(path, SituationCmd, Timeout)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return Situation{}, fmt.Errorf("%w (%v)", ErrNotRunning, err)
		}
		return Situation{}, err
	}
	sit, err := ParseSituation([]byte(reply))
	if err != nil {
		return Situation{}, fmt.Errorf("control: situation: %w", err)
	}
	return sit, nil
}

// ParseSituation reads a situation reply by token: encoding/json would
// keep the last of a duplicate key, match keys case-insensitively, and
// stop after the first value.
func ParseSituation(b []byte) (Situation, error) {
	var sit Situation
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return sit, errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return sit, err
		}
		key, _ := t.(string)
		if seen[key] {
			return sit, fmt.Errorf("key %q twice", key)
		}
		seen[key] = true
		var dst any
		switch key {
		case "mode":
			dst = &sit.Mode
		case "link":
			dst = &sit.Link
		case "serverRevision":
			dst = &sit.ServerRevision
		case "failing":
			dst = &sit.Failing
		case "more":
			dst = &sit.More
		case "update":
			dst = &sit.Update
		case "reauth":
			dst = &sit.Reauth
		default:
			return sit, fmt.Errorf("unknown field %q", key)
		}
		if err := dec.Decode(dst); err != nil {
			return sit, fmt.Errorf("%s: %w", key, err)
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return sit, errors.New("unterminated object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return sit, errors.New("trailing data after the object")
	}
	switch sit.Mode {
	case ModeClient:
		if !slices.Contains(LinkCodes, sit.Link) {
			return sit, fmt.Errorf("unknown link code %q", sit.Link)
		}
		if sit.ServerRevision != "" && !revisionHex.MatchString(sit.ServerRevision) {
			return sit, errors.New("serverRevision isn't 40 hex digits")
		}
		if sit.Update != "" && !slices.Contains(UpdateCodes, sit.Update) {
			return sit, fmt.Errorf("unknown update state %q", sit.Update)
		}
	case ModeServer:
		if sit.Link != "" || sit.ServerRevision != "" || sit.Update != "" {
			return sit, errors.New("a server has no link")
		}
	default:
		return sit, fmt.Errorf("unknown mode %q", sit.Mode)
	}
	if len(sit.Failing) > MaxFailing || sit.More < 0 || sit.More > MaxMore {
		return sit, errors.New("failing accounts out of bounds")
	}
	if sit.Reauth < 0 || sit.Reauth > len(sit.Failing)+sit.More {
		return sit, errors.New("reauth count out of bounds")
	}
	for _, n := range sit.Failing {
		if !config.ValidName(n) {
			return sit, fmt.Errorf("bad account name %q", n)
		}
	}
	return sit, nil
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

// AskStatus asks the daemon at path for its build and instance (Info),
// parsed by token as strictly as a situation: the known keys once each,
// revision, modified and instance required (listening absent: Legacy);
// the revision empty or 40 hex, the instance 16 hex.
func AskStatus(path string) (Info, error) {
	reply, err := Send(path, Status, Timeout)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return Info{}, fmt.Errorf("%w (%v)", ErrNotRunning, err)
		}
		return Info{}, err
	}
	in, err := ParseInfo([]byte(reply))
	if err != nil {
		return Info{}, fmt.Errorf("control: status: %w", err)
	}
	return in, nil
}

var instanceHex = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ParseInfo reads a status reply by token (see ParseSituation).
func ParseInfo(b []byte) (Info, error) {
	var in Info
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return in, errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return in, err
		}
		key, _ := t.(string)
		if seen[key] {
			return in, fmt.Errorf("key %q twice", key)
		}
		seen[key] = true
		var dst any
		switch key {
		case "revision":
			dst = &in.Revision
		case "modified":
			dst = &in.Modified
		case "instance":
			dst = &in.Instance
		case "listening":
			dst = &in.Listening
		default:
			return in, fmt.Errorf("unknown field %q", key)
		}
		if err := dec.Decode(dst); err != nil {
			return in, fmt.Errorf("%s: %w", key, err)
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return in, errors.New("unterminated object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return in, errors.New("trailing data after the object")
	}
	// listening is the one field an older daemon lacks: its reply is
	// legacy, not malformed, so updating from it (or rolling back to it)
	// works with what it can say.
	if !seen["revision"] || !seen["modified"] || !seen["instance"] {
		return in, errors.New("missing fields")
	}
	in.Legacy = !seen["listening"]
	if in.Revision != "" && !revisionHex.MatchString(in.Revision) {
		return in, errors.New("revision isn't 40 hex digits")
	}
	if !instanceHex.MatchString(in.Instance) {
		return in, errors.New("instance isn't 16 hex digits")
	}
	return in, nil
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
