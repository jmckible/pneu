// Package peer is the server's side of the link to pneu clients
// (docs/client.md, "The link"): its key pair, the paired peers
// (peers.json), and the peer listener, which admits a connection only with
// a pinned client certificate from the paired Tailscale node, and keeps
// admitting it only while a whois lease holds.
package peer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Record is one paired client.
type Record struct {
	Name   string    `json:"name"`
	Node   string    `json:"node"`   // its Tailscale StableID
	SPKI   string    `json:"spki"`   // hex SHA-256 of its certificate's SubjectPublicKeyInfo
	Origin string    `json:"origin"` // the client's local origin, its pages' data-origin
	Added  time.Time `json:"added"`
}

// File is peers.json: one generation of the peer set. Hash is over the
// generation and peers (hashOf), so a hand edit or a torn copy reads as
// damage, and the daemon's ack can name exactly what it loaded.
type File struct {
	Generation uint64   `json:"generation"`
	Hash       string   `json:"hash"`
	Peers      []Record `json:"peers"`
}

// Lookup is the record pinned to spki, if any.
func (f *File) Lookup(spki string) *Record {
	for i := range f.Peers {
		if f.Peers[i].SPKI == spki {
			return &f.Peers[i]
		}
	}
	return nil
}

// hashOf is the content hash: SHA-256 of the generation and peers as JSON.
// encoding/json is deterministic for these types.
func hashOf(gen uint64, peers []Record) string {
	if peers == nil {
		peers = []Record{}
	}
	b, err := json.Marshal(struct {
		Generation uint64   `json:"generation"`
		Peers      []Record `json:"peers"`
	}{gen, peers})
	if err != nil {
		panic(err) // strings, ints and UTC times always marshal
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	// Tailscale's stable node IDs are short alphanumerics ("nxzXSZ2TfK11CNTRL").
	nodeRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	spkiRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidName reports whether name can name a peer: a lowercase DNS label of
// at most 32 characters. It appears in logs and the list.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// ValidNode reports whether node looks like a Tailscale StableID.
func ValidNode(node string) bool { return nodeRE.MatchString(node) }

// ValidOrigin reports whether origin is exactly http://pneu.localhost:<port>,
// the port decimal in 1..65535 without a leading zero.
func ValidOrigin(origin string) bool {
	p, ok := strings.CutPrefix(origin, "http://pneu.localhost:")
	if !ok || p == "" || len(p) > 5 || p[0] == '0' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

func (r Record) validate() error {
	switch {
	case !ValidName(r.Name):
		return fmt.Errorf("bad peer name %q", r.Name)
	case !ValidNode(r.Node):
		return fmt.Errorf("peer %s: bad node", r.Name)
	case !spkiRE.MatchString(r.SPKI):
		return fmt.Errorf("peer %s: bad spki", r.Name)
	case !ValidOrigin(r.Origin):
		return fmt.Errorf("peer %s: bad origin", r.Name)
	}
	return nil
}

// conflict names what a new record would share with f's: name, node or
// key are each unique (a re-pair is remove, then add).
func (f *File) conflict(r Record) error {
	for _, p := range f.Peers {
		switch {
		case p.Name == r.Name:
			return fmt.Errorf("a peer named %s is already paired (pneu peer remove %s first)", r.Name, r.Name)
		case p.Node == r.Node:
			return fmt.Errorf("node %s is already paired as %s (pneu peer remove %s first)", r.Node, p.Name, p.Name)
		case p.SPKI == r.SPKI:
			return fmt.Errorf("that key is already paired as %s", p.Name)
		}
	}
	return nil
}

// maxFile bounds peers.json.
const maxFile = 1 << 20

// Store is peers.json and its lock in dir ($XDG_STATE_HOME/pneu).
type Store struct{ Dir string }

func (s Store) path() string     { return filepath.Join(s.Dir, "peers.json") }
func (s Store) lockPath() string { return filepath.Join(s.Dir, "peers.lock") }

// Load reads the current generation: an empty File when there's none. The
// file must be ours, a regular file, and not readable by anyone else; its
// hash must match and its records be valid and unique. Reading needs no
// lock: writes replace the file by rename.
func (s Store) Load() (File, error) {
	f, err := os.OpenFile(s.path(), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return File{Peers: []Record{}}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("peers: %w", err)
	}
	defer f.Close()
	if err := checkPrivate(f, s.path()); err != nil {
		return File{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return File{}, fmt.Errorf("peers: %w", err)
	}
	if len(b) > maxFile {
		return File{}, fmt.Errorf("peers: %s is over %d bytes", s.path(), maxFile)
	}
	var pf File
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pf); err != nil {
		return File{}, fmt.Errorf("peers: %s: %w", s.path(), err)
	}
	if dec.More() {
		return File{}, fmt.Errorf("peers: %s: trailing data", s.path())
	}
	if pf.Peers == nil {
		pf.Peers = []Record{}
	}
	if pf.Hash != hashOf(pf.Generation, pf.Peers) {
		return File{}, fmt.Errorf("peers: %s: hash doesn't match its content (edited by hand? pneu peer add|remove write it)", s.path())
	}
	var seen File
	for _, r := range pf.Peers {
		if err := r.validate(); err != nil {
			return File{}, fmt.Errorf("peers: %s: %w", s.path(), err)
		}
		if err := seen.conflict(r); err != nil {
			return File{}, fmt.Errorf("peers: %s: %w", s.path(), err)
		}
		seen.Peers = append(seen.Peers, r)
	}
	return pf, nil
}

// checkPrivate insists an open file is a regular file of ours with no
// group or other bits.
func checkPrivate(f *os.File, name string) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("peers: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("peers: %s isn't a regular file", name)
	}
	if uid, ok := ownerOf(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("peers: %s is owned by uid %d, not us", name, uid)
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("peers: %s has mode %v, want 0600", name, fi.Mode().Perm())
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

// Locked is the store under its exclusive lock: the only way to write it.
type Locked struct {
	s Store
	f *os.File
}

// LockWait bounds how long Lock waits for another add or remove, which
// holds the lock through its daemon's ack (control.ReloadTimeout).
const LockWait = 30 * time.Second

// Lock takes the exclusive flock on peers.lock, a file that is never
// replaced or removed: a lock on peers.json itself would be on an inode the
// next rename retires, and a second writer would lock the new one (N8).
func (s Store) Lock(wait time.Duration) (*Locked, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("peers: %w", err)
	}
	f, err := os.OpenFile(s.lockPath(), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("peers: lock: %w", err)
	}
	if err := checkPrivate(f, s.lockPath()); err != nil {
		f.Close()
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Locked{s: s, f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("peers: lock: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("peers: %s still held after %v (another pneu peer add or remove)", s.lockPath(), wait)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Unlock releases the lock.
func (l *Locked) Unlock() {
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}

// Add writes the next generation with r added, refusing a name, node or
// key already paired.
func (l *Locked) Add(r Record) (File, error) {
	if err := r.validate(); err != nil {
		return File{}, err
	}
	cur, err := l.s.Load()
	if err != nil {
		return File{}, err
	}
	if err := cur.conflict(r); err != nil {
		return File{}, err
	}
	return l.commit(cur, append(cur.Peers, r))
}

// Remove writes the next generation without the peer named name.
func (l *Locked) Remove(name string) (File, Record, error) {
	cur, err := l.s.Load()
	if err != nil {
		return File{}, Record{}, err
	}
	for i, p := range cur.Peers {
		if p.Name == name {
			peers := append(append([]Record{}, cur.Peers[:i]...), cur.Peers[i+1:]...)
			f, err := l.commit(cur, peers)
			return f, p, err
		}
	}
	return File{}, Record{}, fmt.Errorf("no peer named %q", name)
}

func (l *Locked) commit(cur File, peers []Record) (File, error) {
	next := File{Generation: cur.Generation + 1, Peers: peers}
	next.Hash = hashOf(next.Generation, next.Peers)
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return File{}, err
	}
	if err := writeAtomic(l.s.path(), append(b, '\n')); err != nil {
		return File{}, fmt.Errorf("peers: %w", err)
	}
	return next, nil
}

// writeAtomic replaces path with b, 0600: temp file, fsync, rename, fsync
// the directory, so a crash leaves the old generation or the new one.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op once renamed
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
