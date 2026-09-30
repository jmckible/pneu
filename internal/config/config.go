// Package config loads pneu's JSON config file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Account is one Gmail account: its own lieer dir and its own notmuch database.
type Account struct {
	Name          string `json:"name"`
	Email         string `json:"email"`
	NotmuchConfig string `json:"notmuchConfig"`
	GmiDir        string `json:"gmiDir"`
}

type Config struct {
	Port     int       `json:"port"`
	Accounts []Account `json:"accounts"`
	// Peer turns on the peer listener (docs/client.md, "The listener"):
	// other machines' pneu clients reach this archive over the tailnet.
	// Absent, nothing listens beyond loopback.
	Peer *Peer `json:"peer,omitempty"`
	// Server makes this machine a client of the server it names
	// (docs/client.md): no mail here, the daemon proxies to that archive.
	// Load refuses it alongside accounts or peer: a machine holds an
	// archive or is a window onto one, never both. `pneu client pair`
	// writes it; the credentials live in the state dir, not here.
	Server *Server `json:"server,omitempty"`
}

// Server is a client's link to its server.
type Server struct {
	// SSH is the target the user typed at pairing: for `ssh -- <SSH>`
	// only, never a display name.
	SSH string `json:"ssh"`
	// Node is the server's Tailscale StableID; its address is resolved
	// from tailscaled by this, never by name.
	Node string `json:"node"`
	Port int    `json:"port"` // its peer port
}

// Peer is the peer listener's block.
type Peer struct {
	Port int `json:"port"`
}

// DefaultPort is used when the config names none.
const DefaultPort = 7317

// DefaultPath is ~/.config/pneu/config.json, honoring $XDG_CONFIG_HOME.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pneu", "config.json"), nil
}

// Load reads path. There are no built-in accounts: a missing file is an
// error that names the path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("no config at %s: create it with your accounts (see INSTALL.md)", path)
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c.normalize()
}

// Account returns the named account.
func (c Config) Account(name string) (Account, bool) {
	for _, a := range c.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}

// ReadRaw reads path as written, without expanding "~" or requiring
// accounts; a missing file is an empty config on the default port. It is
// what `pneu account add` edits and writes back with Write.
func ReadRaw(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{Port: DefaultPort}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Write replaces path with c, indented as INSTALL.md shows it, by rename.
func Write(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ValidName reports whether name can be an account name: it becomes a path
// segment and a command-line word.
func ValidName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/ ") && !strings.HasPrefix(name, "-") && !strings.HasPrefix(name, ".")
}

func (c Config) normalize() (Config, error) {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Server != nil {
		switch {
		case len(c.Accounts) > 0:
			return Config{}, errors.New("config: both accounts and server; a machine is a server (accounts) or a client (server), not both")
		case c.Peer != nil:
			return Config{}, errors.New("config: both peer and server; only a server (with accounts) runs a peer listener")
		case !ValidSSHTarget(c.Server.SSH):
			return Config{}, fmt.Errorf("config: server: bad ssh target %q", c.Server.SSH)
		case !nodeRE.MatchString(c.Server.Node):
			return Config{}, fmt.Errorf("config: server: bad node %q", c.Server.Node)
		case c.Server.Port < 1 || c.Server.Port > 65535:
			return Config{}, fmt.Errorf("config: server: bad port %d", c.Server.Port)
		}
		return c, nil
	}
	if c.Peer != nil {
		if c.Peer.Port < 1 || c.Peer.Port > 65535 || c.Peer.Port == c.Port {
			return Config{}, fmt.Errorf("config: bad peer port %d", c.Peer.Port)
		}
	}
	if len(c.Accounts) == 0 {
		return Config{}, errors.New("config: no accounts")
	}
	seen := map[string]bool{}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if !ValidName(a.Name) {
			return Config{}, fmt.Errorf("config: bad account name %q", a.Name)
		}
		if seen[a.Name] {
			return Config{}, fmt.Errorf("config: duplicate account %q", a.Name)
		}
		seen[a.Name] = true
		var err error
		if a.NotmuchConfig, err = ExpandHome(a.NotmuchConfig); err != nil {
			return Config{}, err
		}
		if a.GmiDir, err = ExpandHome(a.GmiDir); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

// nodeRE is a Tailscale StableID, as peer.ValidNode has it.
var nodeRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// ValidSSHTarget reports whether t can be handed to ssh after "--": not
// empty, at most 255 bytes, no leading '-' (never an option, whatever ssh
// does with "--"), and no whitespace or control characters.
func ValidSSHTarget(t string) bool {
	if t == "" || len(t) > 255 || t[0] == '-' || !utf8.ValidString(t) {
		return false
	}
	for _, r := range t {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// ExpandHome expands a leading "~" or "~/".
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, p[1:]), nil
}
