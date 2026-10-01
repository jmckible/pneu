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
	// Source is where `pneu update` takes code from (docs/client.md,
	// "pneu update"; R15): this machine's own checkout, and the remote and
	// branch recorded at install (`pneu source set`). Either kind of
	// machine; absent, pneu update refuses and says how to record it.
	Source *Source `json:"source,omitempty"`
}

// Source is the checkout pneu is built from. URL is the remote's URL when
// it was recorded: pneu update refuses a remote that has since been
// pointed elsewhere.
type Source struct {
	Dir    string `json:"dir"`
	Remote string `json:"remote"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// remoteRE and branchRE are the names Source accepts: plain git names, no
// leading '-' (never an option), no "..", no "@{", nothing a refspec would
// read as more than a name.
var (
	remoteRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
)

// ValidSource reports what's wrong with s, or nil.
func ValidSource(s Source) error {
	switch {
	case s.Dir == "" || !filepath.IsAbs(s.Dir) || filepath.Clean(s.Dir) != s.Dir:
		return fmt.Errorf("source: dir %q isn't a clean absolute path", s.Dir)
	case !remoteRE.MatchString(s.Remote) || strings.Contains(s.Remote, ".."):
		return fmt.Errorf("source: bad remote %q", s.Remote)
	case !branchRE.MatchString(s.Branch) || strings.Contains(s.Branch, "..") || strings.Contains(s.Branch, "//") ||
		strings.HasSuffix(s.Branch, "/") || strings.HasSuffix(s.Branch, ".lock") || strings.HasSuffix(s.Branch, "."):
		return fmt.Errorf("source: bad branch %q", s.Branch)
	case s.URL == "" || len(s.URL) > 1024 || !utf8.ValidString(s.URL) || strings.IndexFunc(s.URL, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0:
		return fmt.Errorf("source: bad url %q", s.URL)
	}
	return nil
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

// MaxName bounds an account name, in bytes.
const MaxName = 32

// NameRule says what ValidName accepts, for error messages.
const NameRule = "a short word like personal or work: letters, digits, . _ -, starting with a letter or digit, at most 32"

// nameRE is the one account-name rule, everywhere: the config, `pneu
// account add`, a server's /peer/hello, a client's event account set, and
// what a suggested command or an agent prompt may show.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidName reports whether name can be an account name. It becomes a
// path segment and a command-line word, and is shown in pages, the bar
// and (on a client, from a server that may be hostile) suggested
// commands: so nothing a shell or a reader would take for more than a
// word. Rejected, never normalized.
func ValidName(name string) bool { return nameRE.MatchString(name) }

func (c Config) normalize() (Config, error) {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Source != nil {
		if err := ValidSource(*c.Source); err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
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
			return Config{}, fmt.Errorf("config: bad account name %q: %s", a.Name, NameRule)
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

// shellPlain is what ShellWord leaves unquoted.
var shellPlain = regexp.MustCompile(`^[A-Za-z0-9@._:-]+$`)

// ShellWord is s as one word of a shell command line printed for the user
// to copy: as is when it's only [A-Za-z0-9@._:-], else single-quoted (any
// ' inside as '\”). For printing, never for running: pneu runs argv.
func ShellWord(s string) string {
	if shellPlain.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SafeSSHOptions turn off every SSH capability pneu's own sessions don't
// need, whatever the user's ssh config says: agent and X11 forwarding,
// every port forward (a command-line ClearAllForwardings=yes wins over the
// config's forwards, Match blocks included; checked against OpenSSH 10.5
// with ssh -G), connection sharing (a master's forwards would carry over)
// and LocalCommand. Pairing and every forwarded account command run with
// them, and so does every ssh command pneu prints for a person or an agent
// to run (SSHHint).
var SafeSSHOptions = []string{
	"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes",
	"-o", "ControlPath=none", "-o", "PermitLocalCommand=no",
}

// SSHHint is the start of a printed ssh command to target: ssh, the safe
// options, -t when the remote command needs a terminal, and the target as
// one shell word. The caller adds the remote command, single-quoted. For
// printing, never for running.
func SSHHint(target string, tty bool) string {
	s := "ssh " + strings.Join(SafeSSHOptions, " ")
	if tty {
		s += " -t"
	}
	return s + " " + ShellWord(target)
}

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

// Plain is text from elsewhere (a server, a file) made safe to show as
// text anywhere: valid UTF-8, no control, bidi-override or line-separator
// characters, at most n runes.
func Plain(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) || bidi(r) || r == ' ' || r == ' ' {
			continue
		}
		if count == n {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

func bidi(r rune) bool {
	return r == '؜' || r == '‎' || r == '‏' || r >= '‪' && r <= '‮' || r >= '⁦' && r <= '⁩'
}
