// Package link is a client's side of the link to its server
// (docs/client.md, "The link"): its credentials, the checks it runs
// against its own tailscaled before dialing, the pinned mutual-TLS HTTP/2
// transport, the whois lease on its pooled connection, the hello
// handshake, and the link's state as local reason codes.
package link

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/web"
)

// PinFile holds what a client pinned at pairing, next to its key pair
// (peer.ClientKeyFile) in $XDG_STATE_HOME/pneu/peer: a 0700 directory,
// both files 0600 and ours, checked on every load. Not config.json, which
// ends up in dotfiles repos.
const PinFile = "pin.json"

const maxPin = 64 << 10

// Pin is a client's pairing: its name on the server, and the server as it
// answered `pneu peer add`.
type Pin struct {
	Name     string    `json:"name"` // this client, as `pneu peer list` shows it
	SSH      string    `json:"ssh"`  // the target pairing ran over
	Node     string    `json:"node"` // the server's Tailscale StableID
	Port     int       `json:"port"` // its peer port
	SPKI     string    `json:"spki"` // the pin: SHA-256 of its certificate's SPKI
	Cert     string    `json:"cert"` // its certificate, PEM, as handed over
	Protocol int       `json:"protocol"`
	Paired   time.Time `json:"paired"`
}

var spkiRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (p Pin) validate() error {
	c, err := peer.ParseCertPEM(p.Cert)
	switch {
	case err != nil:
		return err
	case !spkiRE.MatchString(p.SPKI) || peer.SPKI(c) != p.SPKI:
		return errors.New("spki doesn't match the certificate")
	case !peer.ValidName(p.Name):
		return errors.New("bad name")
	case !peer.ValidNode(p.Node):
		return errors.New("bad node")
	case p.Port < 1 || p.Port > 65535:
		return errors.New("bad port")
	case !config.ValidSSHTarget(p.SSH):
		return errors.New("bad ssh target")
	}
	return nil
}

// Creds are a paired client's own key pair and its pin.
type Creds struct {
	Identity peer.Identity
	Pin      Pin
}

// Dir is $XDG_STATE_HOME/pneu/peer, where a client's credentials live.
func Dir() (string, error) {
	state, err := web.StateDir()
	return filepath.Join(state, "peer"), err
}

// Paired reports whether dir holds a pin (anything at the path counts:
// LoadCreds judges it).
func Paired(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, PinFile))
	return !errors.Is(err, fs.ErrNotExist)
}

// LoadCreds reads the key pair and pin from dir, checking dir, both files'
// owner and mode, and the pin's content (its SPKI is its certificate's).
func LoadCreds(dir string) (Creds, error) {
	id, err := peer.LoadIdentity(dir, peer.ClientKeyFile)
	if err != nil {
		return Creds{}, fmt.Errorf("link: client key: %w", err)
	}
	path := filepath.Join(dir, PinFile)
	b, err := peer.ReadPrivate(path, maxPin)
	if err != nil {
		return Creds{}, fmt.Errorf("link: %w", err)
	}
	var p Pin
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Creds{}, fmt.Errorf("link: %s: %w", path, err)
	}
	if dec.More() {
		return Creds{}, fmt.Errorf("link: %s: trailing data", path)
	}
	if err := p.validate(); err != nil {
		return Creds{}, fmt.Errorf("link: %s: %w", path, err)
	}
	if p.SPKI == id.SPKI {
		return Creds{}, fmt.Errorf("link: %s pins this client's own key", path)
	}
	return Creds{Identity: id, Pin: p}, nil
}

// WritePin records p in dir (0600, atomically).
func WritePin(dir string, p Pin) error {
	if err := peer.PrepareDir(dir); err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return fmt.Errorf("link: pin: %w", err)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return peer.WritePrivate(filepath.Join(dir, PinFile), append(b, '\n'))
}

// RemoveCreds deletes the pin and the client's key pair from dir. A later
// pairing makes a fresh key; the server forgets the old one only with
// `pneu peer remove` there.
func RemoveCreds(dir string) error {
	var errs []error
	for _, f := range []string{PinFile, peer.ClientKeyFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LockFile is the client daemon's lifecycle lock in the credentials dir:
// a file never replaced or removed, so a lock on it is on the one inode
// everyone opens.
const LockFile = "daemon.lock"

// ErrLocked: another process holds the lifecycle lock.
var ErrLocked = errors.New("link: the client daemon's lock is held")

// PairLockFile is the pairing transaction's lock: `pneu client unpair`
// holds it from before it asks the daemon to unlink until the credentials
// and config are gone, and a starting daemon holds it from taking
// LockFile until it has loaded and checked them. So a daemon restarted
// mid-unpair can't load credentials the unpair is about to delete (E1).
const PairLockFile = "pair.lock"

// PairWait is how long either side waits for the other's transaction.
const PairWait = 10 * time.Second

// Lock takes the lifecycle lock without waiting (flock, exclusive): the
// client daemon holds it for its life, from before it loads credentials,
// and `pneu client unpair` holds it across deleting them when no daemon
// answers, so neither can run under the other. Unlock by closing the file.
func Lock(dir string) (*os.File, error) { return lockFile(dir, LockFile) }

// PairLock takes the pairing transaction's lock, waiting up to wait;
// ErrLocked when it's still held.
func PairLock(dir string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := lockFile(dir, PairLockFile)
		if !errors.Is(err, ErrLocked) || !time.Now().Before(deadline) {
			return f, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockFile takes an exclusive flock on dir/name without waiting: a file
// never replaced or removed, 0600 and ours, in the checked 0700 dir.
func lockFile(dir, name string) (*os.File, error) {
	if err := peer.PrepareDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("link: %w", err)
	}
	fi, err := f.Stat()
	if err == nil {
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !fi.Mode().IsRegular():
			err = fmt.Errorf("link: %s isn't a regular file", path)
		case !ok || int(st.Uid) != os.Getuid():
			err = fmt.Errorf("link: %s isn't ours", path)
		case fi.Mode().Perm() != 0o600:
			err = fmt.Errorf("link: %s has mode %v, want 0600", path, fi.Mode().Perm())
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("link: lock: %w", err)
	}
	return f, nil
}
