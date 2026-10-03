// Package state is push sync's one file of state, and its locks
// (docs/push.md D2, D4): $XDG_STATE_HOME/pneu/push/state.json, the push
// project's client JSON beside it, push.lock (the CLI's write lock) and
// daemon.lock (the daemon's lifetime lock).
//
// state.json is written only by the CLI, under push.lock: read, modify,
// generation+1, temp + fsync + rename + fsync dir. The daemon only reads
// it, without a lock: a rename hands it a whole snapshot. Its hash is
// SHA-256 over the file's bytes, which push-reload names along with the
// generation. Everything is checked on every open: the directory exactly
// 0700 and ours (Lstat, no symlink), each file regular, ours, 0600,
// opened O_NOFOLLOW, size-capped, and parsed by token (each key once,
// nothing after, every field bounded).
package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/strictjson"
)

// File names in the push directory. The lock files are never replaced or
// removed: a lock on a file a rename retires would be on an inode the
// next writer doesn't open (N8).
const (
	StateFile      = "state.json"
	ClientFile     = "client.json"
	LockFile       = "push.lock"
	DaemonLockFile = "daemon.lock"
)

// Bounds.
const (
	Version     = 1
	MaxState    = 64 << 10
	MaxClient   = 16 << 10
	MaxAccounts = 64
	maxGen      = 1 << 53
)

// Dir is the push directory under the state dir ($XDG_STATE_HOME/pneu,
// web.StateDir).
func Dir(stateDir string) string { return filepath.Join(stateDir, "push") }

// AccountState is a pushed account's state.
type AccountState string

const (
	// On: the daemon runs a worker for it.
	On AccountState = "on"
	// OffPending: disabled locally, users.stop not yet confirmed; the
	// token is kept for that one call. The daemon never runs it.
	OffPending AccountState = "off-pending"
)

// Owner is the push project's owner (D2): sub pinned at init, email for
// showing, the refresh token, and when it was granted.
type Owner struct {
	Sub     string
	Email   string
	Refresh string
	Granted time.Time
}

// Account is one pushed mailbox.
type Account struct {
	State   AccountState
	Address string
	Refresh string
	Granted time.Time
}

// File is state.json's content.
type File struct {
	Generation uint64 // 0 only before the first commit
	Project    string // the push project's ID
	Install    string // this server's install id (google.ValidInstall)
	Client     string // the push client's ID, as recorded at init
	Owner      Owner
	Accounts   map[string]Account
}

func (o Owner) String() string     { return "state.Owner{" + o.Email + "}" }
func (o Owner) GoString() string   { return o.String() }
func (a Account) String() string   { return "state.Account{" + string(a.State) + " " + a.Address + "}" }
func (a Account) GoString() string { return a.String() }

// Snapshot is one generation as read: the file and the hash of its bytes.
type Snapshot struct {
	File
	Hash string
}

// ErrNoState: there is no state.json (push was never set up), or no push
// directory. It wraps fs.ErrNotExist.
var ErrNoState = fmt.Errorf("push: no state (pneu push init sets it up): %w", fs.ErrNotExist)

// Store is the push directory.
type Store struct{ Dir string }

func (s Store) path(name string) string { return filepath.Join(s.Dir, name) }

// Load reads the current generation without a lock (the daemon's read).
// ErrNoState when there's none.
func (s Store) Load() (Snapshot, error) {
	if err := checkDir(s.Dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Snapshot{}, ErrNoState
		}
		return Snapshot{}, err
	}
	b, err := readPrivate(s.path(StateFile), MaxState)
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{}, ErrNoState
	}
	if err != nil {
		return Snapshot{}, err
	}
	f, err := Parse(b)
	if err != nil {
		return Snapshot{}, fmt.Errorf("push: %s: %w", s.path(StateFile), err)
	}
	return Snapshot{File: f, Hash: Hash(b)}, nil
}

// Hash is the hex SHA-256 of a file's bytes: what push-reload names.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NewInstall is a fresh install id: 16 hex digits from crypto/rand.
func NewInstall() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- the file's shape ------------------------------------------------

// timeLayout is how times are written: RFC 3339, UTC, whole seconds.
const timeLayout = time.RFC3339

func formatTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(timeLayout) }

// parseTime reads a time written by formatTime, and only that spelling,
// between 2020 and 9999.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil || formatTime(t) != s || t.Year() < 2020 {
		return time.Time{}, errors.New("not a UTC RFC 3339 time")
	}
	return t, nil
}

// Validate checks every field against its rule, and that no two accounts
// hold the same address (one topic per mailbox).
func (f File) Validate() error {
	switch {
	case !google.ValidProject(f.Project):
		return errors.New("bad project")
	case !google.ValidInstall(f.Install):
		return errors.New("bad install id")
	case !google.ValidClientID(f.Client):
		return errors.New("bad client id")
	case !google.ValidSub(f.Owner.Sub):
		return errors.New("owner: bad sub")
	case !google.ValidAddress(f.Owner.Email):
		return errors.New("owner: bad email")
	case !google.ValidRefresh(f.Owner.Refresh):
		return errors.New("owner: bad refresh token")
	case !validTime(f.Owner.Granted):
		return errors.New("owner: bad granted time")
	case len(f.Accounts) > MaxAccounts:
		return fmt.Errorf("more than %d accounts", MaxAccounts)
	}
	for name, a := range f.Accounts {
		switch {
		case !config.ValidName(name):
			return errors.New("bad account name")
		case a.State != On && a.State != OffPending:
			return fmt.Errorf("account %s: bad state", name)
		case !google.ValidAddress(a.Address):
			return fmt.Errorf("account %s: bad address", name)
		case !google.ValidRefresh(a.Refresh):
			return fmt.Errorf("account %s: bad refresh token", name)
		case !validTime(a.Granted):
			return fmt.Errorf("account %s: bad granted time", name)
		}
		for other, b := range f.Accounts {
			if other != name && google.SameAddress(a.Address, b.Address) {
				return fmt.Errorf("accounts %s and %s hold the same address", min(name, other), max(name, other))
			}
		}
	}
	return nil
}

func validTime(t time.Time) bool {
	_, err := parseTime(formatTime(t))
	return err == nil
}

// Parse reads state.json's bytes by token: exactly the known keys, each
// once, no null, nothing after, every field bounded and valid.
func Parse(b []byte) (File, error) {
	f := File{Accounts: map[string]Account{}}
	var version uint64
	seen := map[string]bool{}
	err := strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		seen[key] = true
		var err error
		switch key {
		case "version":
			version, err = strictjson.Uint(dec, 1000)
		case "generation":
			f.Generation, err = strictjson.Uint(dec, maxGen)
		case "project":
			f.Project, err = strictjson.String(dec, 64)
		case "install":
			f.Install, err = strictjson.String(dec, 64)
		case "client":
			f.Client, err = strictjson.String(dec, 256)
		case "owner":
			f.Owner, err = parseOwner(dec)
		case "accounts":
			err = strictjson.Object(dec, func(name string) error {
				if !config.ValidName(name) {
					return errors.New("bad account name")
				}
				if len(f.Accounts) >= MaxAccounts {
					return fmt.Errorf("more than %d accounts", MaxAccounts)
				}
				a, err := parseAccount(dec)
				f.Accounts[name] = a
				return err
			})
		default:
			return strictjson.ErrUnknown
		}
		return err
	})
	if err != nil {
		return File{}, err
	}
	for _, k := range []string{"version", "generation", "project", "install", "client", "owner", "accounts"} {
		if !seen[k] {
			return File{}, fmt.Errorf("field %q missing", k)
		}
	}
	if version != Version {
		return File{}, fmt.Errorf("version %d, want %d", version, Version)
	}
	if f.Generation == 0 {
		return File{}, errors.New("generation 0")
	}
	if err := f.Validate(); err != nil {
		return File{}, err
	}
	return f, nil
}

// fields reads an object whose every key is in want, each once, all
// present, all strings of at most max bytes.
func fields(dec *json.Decoder, want map[string]*string, max int) error {
	seen := 0
	err := strictjson.Object(dec, func(key string) error {
		dst, ok := want[key]
		if !ok {
			return strictjson.ErrUnknown
		}
		seen++
		var err error
		*dst, err = strictjson.String(dec, max)
		return err
	})
	if err == nil && seen != len(want) {
		return errors.New("fields missing")
	}
	return err
}

func parseOwner(dec *json.Decoder) (Owner, error) {
	var o Owner
	var granted string
	if err := fields(dec, map[string]*string{"sub": &o.Sub, "email": &o.Email, "refresh": &o.Refresh, "granted": &granted}, google.MaxRefresh); err != nil {
		return Owner{}, err
	}
	var err error
	o.Granted, err = parseTime(granted)
	return o, err
}

func parseAccount(dec *json.Decoder) (Account, error) {
	var a Account
	var st, granted string
	if err := fields(dec, map[string]*string{"state": &st, "address": &a.Address, "refresh": &a.Refresh, "granted": &granted}, google.MaxRefresh); err != nil {
		return Account{}, err
	}
	a.State = AccountState(st)
	var err error
	a.Granted, err = parseTime(granted)
	return a, err
}

// wire is state.json's encoding: fixed key order, times as formatTime.
type wire struct {
	Version    int                    `json:"version"`
	Generation uint64                 `json:"generation"`
	Project    string                 `json:"project"`
	Install    string                 `json:"install"`
	Client     string                 `json:"client"`
	Owner      wireOwner              `json:"owner"`
	Accounts   map[string]wireAccount `json:"accounts"`
}

type wireOwner struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Refresh string `json:"refresh"`
	Granted string `json:"granted"`
}

type wireAccount struct {
	State   string `json:"state"`
	Address string `json:"address"`
	Refresh string `json:"refresh"`
	Granted string `json:"granted"`
}

// encode is f's bytes: deterministic (encoding/json sorts map keys).
func encode(f File) ([]byte, error) {
	w := wire{Version: Version, Generation: f.Generation, Project: f.Project, Install: f.Install, Client: f.Client,
		Owner:    wireOwner{f.Owner.Sub, f.Owner.Email, f.Owner.Refresh, formatTime(f.Owner.Granted)},
		Accounts: map[string]wireAccount{}}
	for n, a := range f.Accounts {
		w.Accounts[n] = wireAccount{string(a.State), a.Address, a.Refresh, formatTime(a.Granted)}
	}
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// clone is a deep copy of f.
func (f File) clone() File {
	f.Accounts = maps.Clone(f.Accounts)
	if f.Accounts == nil {
		f.Accounts = map[string]Account{}
	}
	return f
}

// --- writing, under push.lock ----------------------------------------

// Locked is the store under push.lock: the only way to write it.
type Locked struct {
	s Store
	f *os.File
}

// Lock takes push.lock (exclusive flock), creating the push directory
// (0700) and the lock file (0600) if needed, waiting until ctx ends.
func (s Store) Lock(ctx context.Context) (*Locked, error) {
	if err := prepareDir(s.Dir); err != nil {
		return nil, err
	}
	f, err := openLock(s.path(LockFile))
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Locked{s: s, f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("push: lock: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("push: %s is held by another pneu push or account push: %w", s.path(LockFile), ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Unlock releases push.lock.
func (l *Locked) Unlock() {
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}

// Current is the generation on disk, read under the lock; ErrNoState
// when there's none.
func (l *Locked) Current() (Snapshot, error) { return l.s.Load() }

// Update is the one write: it reads the current generation (a zero File,
// Generation 0, when there's none), lets modify change a copy, checks the
// result, and writes it as the next generation. modify returning an
// error writes nothing.
func (l *Locked) Update(modify func(f *File) error) (Snapshot, error) {
	cur, err := l.s.Load()
	if err != nil && !errors.Is(err, ErrNoState) {
		return Snapshot{}, err
	}
	next := cur.File.clone()
	if err := modify(&next); err != nil {
		return Snapshot{}, err
	}
	next.Generation = cur.Generation + 1
	if next.Generation >= maxGen {
		return Snapshot{}, errors.New("push: generation exhausted")
	}
	if err := next.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("push: %w", err)
	}
	b, err := encode(next)
	if err != nil {
		return Snapshot{}, err
	}
	if len(b) > MaxState {
		return Snapshot{}, fmt.Errorf("push: state over %d bytes", MaxState)
	}
	if err := writeAtomic(l.s.path(StateFile), b); err != nil {
		return Snapshot{}, fmt.Errorf("push: %w", err)
	}
	return Snapshot{File: next.clone(), Hash: Hash(b)}, nil
}

// --- the client JSON -------------------------------------------------

// Client is the push project's OAuth client, from its client JSON (a
// Desktop client: the "installed" shape Google's console downloads).
type Client struct {
	ID      string
	Secret  string
	Project string
}

func (c Client) String() string   { return "state.Client{" + c.ID + "}" }
func (c Client) GoString() string { return c.String() }

// Credentials is the client as google takes it.
func (c Client) Credentials() google.Credentials { return google.Credentials{ID: c.ID, Secret: c.Secret} }

// ErrNotDesktop: the client JSON isn't a Desktop client's.
var ErrNotDesktop = errors.New("push: the client JSON isn't a Desktop app client's (its top level must be \"installed\")")

// ParseClient reads a client JSON by token: one key, "installed", whose
// client_id, client_secret and project_id are required and valid; its
// other keys (auth_uri, token_uri, redirect_uris, …) are skipped, never
// used: pneu's hosts are its own constants. Each key at most once.
func ParseClient(b []byte) (Client, error) {
	if len(b) > MaxClient {
		return Client{}, fmt.Errorf("push: client JSON over %d bytes", MaxClient)
	}
	var c Client
	installed := false
	err := strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		if key != "installed" {
			return ErrNotDesktop
		}
		installed = true
		return strictjson.Object(dec, func(k string) error {
			var err error
			switch k {
			case "client_id":
				c.ID, err = strictjson.String(dec, 256)
			case "client_secret":
				c.Secret, err = strictjson.String(dec, 256)
			case "project_id":
				c.Project, err = strictjson.String(dec, 64)
			default:
				return strictjson.Skip(dec)
			}
			return err
		})
	})
	switch {
	case errors.Is(err, ErrNotDesktop) || (err == nil && !installed):
		return Client{}, ErrNotDesktop
	case err != nil:
		return Client{}, fmt.Errorf("push: client JSON: %w", err)
	case !google.ValidClientID(c.ID):
		return Client{}, errors.New("push: client JSON: bad or missing client_id")
	case !google.ValidSecret(c.Secret):
		return Client{}, errors.New("push: client JSON: bad or missing client_secret")
	case !google.ValidProject(c.Project):
		return Client{}, errors.New("push: client JSON: bad or missing project_id")
	}
	return c, nil
}

// LoadClient reads and checks client.json.
func (s Store) LoadClient() (Client, error) {
	if err := checkDir(s.Dir); err != nil {
		return Client{}, err
	}
	b, err := readPrivate(s.path(ClientFile), MaxClient)
	if err != nil {
		return Client{}, err
	}
	return ParseClient(b)
}

// WriteClient checks b as a client JSON and stores it as client.json
// (0600, atomically), under the lock.
func (l *Locked) WriteClient(b []byte) (Client, error) {
	c, err := ParseClient(b)
	if err != nil {
		return Client{}, err
	}
	if err := writeAtomic(l.s.path(ClientFile), b); err != nil {
		return Client{}, fmt.Errorf("push: %w", err)
	}
	return c, nil
}

// CheckClient is the state's identity check on a client (D1, K6): the
// same project and the client recorded at init. A different client is
// refused; re-init (--replace) is the only way to change it.
func CheckClient(f File, c Client) error {
	switch {
	case c.Project != f.Project:
		return errors.New("push: client.json's project_id isn't the push project's")
	case c.ID != f.Client:
		return errors.New("push: client.json isn't the client recorded at init (pneu push init --replace changes it)")
	}
	return nil
}

// --- the daemon's lifetime lock --------------------------------------

// ErrDaemonRunning: another process holds daemon.lock, so a daemon runs
// push (or a CLI acting as "no daemon" holds it for a moment).
var ErrDaemonRunning = errors.New("push: the daemon's lock is held")

// DaemonLock is daemon.lock, held.
type DaemonLock struct{ f *os.File }

// TryDaemonLock takes daemon.lock without waiting; ErrDaemonRunning when
// it's held. The daemon takes it before it starts any worker and holds it
// for life. The CLI decides "no daemon" only by taking it (never from a
// missing socket), and holds it while it acts on that.
func (s Store) TryDaemonLock() (*DaemonLock, error) {
	if err := prepareDir(s.Dir); err != nil {
		return nil, err
	}
	f, err := openLock(s.path(DaemonLockFile))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrDaemonRunning
		}
		return nil, fmt.Errorf("push: daemon lock: %w", err)
	}
	return &DaemonLock{f: f}, nil
}

// Unlock releases daemon.lock.
func (d *DaemonLock) Unlock() {
	syscall.Flock(int(d.f.Fd()), syscall.LOCK_UN)
	d.f.Close()
}

// DaemonRunning probes daemon.lock without keeping it: true when someone
// holds it.
func (s Store) DaemonRunning() (bool, error) {
	d, err := s.TryDaemonLock()
	if errors.Is(err, ErrDaemonRunning) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	d.Unlock()
	return false, nil
}

// --- files -----------------------------------------------------------

// prepareDir makes the push directory (and its parent) 0700 if missing,
// then checks it.
func prepareDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("push: %w", err)
	}
	return checkDir(dir)
}

// checkDir insists, via Lstat so a symlink is seen as one: a directory,
// ours, mode exactly 0700.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("push: %s is not a directory (%v); remove it", dir, fi.Mode().Type())
	}
	if uid, ok := ownerOf(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("push: %s is owned by uid %d, not us", dir, uid)
	}
	if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != 0o700 {
		return fmt.Errorf("push: %s has mode %v, want exactly 0700; chmod 700 it", dir, m)
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

// checkPrivate insists an open file is a regular file of ours, mode
// exactly 0600.
func checkPrivate(f *os.File, name string) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("push: %s isn't a regular file", name)
	}
	if uid, ok := ownerOf(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("push: %s is owned by uid %d, not us", name, uid)
	}
	if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != 0o600 {
		return fmt.Errorf("push: %s has mode %v, want 0600", name, m)
	}
	return nil
}

// readPrivate reads path (O_NOFOLLOW, and O_NONBLOCK so a FIFO planted
// there can't hang the open), checked by checkPrivate, at most max bytes.
func readPrivate(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("push: %s is a symlink", path)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("push: %w", err)
	}
	defer f.Close()
	if err := checkPrivate(f, path); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("push: %s is over %d bytes", path, max)
	}
	return b, nil
}

// openLock opens (creating 0600) a lock file, checked by checkPrivate.
func openLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("push: %s is a symlink", path)
		}
		return nil, fmt.Errorf("push: lock: %w", err)
	}
	if err := checkPrivate(f, path); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// writeAtomic replaces path with b, 0600: temp file, fsync, rename, fsync
// the directory, so a crash leaves the old generation or the new one.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := checkDir(dir); err != nil {
		return err
	}
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
