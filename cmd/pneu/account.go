package main

// pneu account: the one-time, terminal half of setting up an account
// (docs/onboarding.md "Where the line falls"). add lays out the account;
// auth runs the consent flow and proves the token; status reports where
// each account stands. The first pull is the server's.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/remote"
	"github.com/jmckible/pneu/internal/web"
)

const accountUsage = `usage:
  pneu account add <name> <address> [--name "Full Name"] [--client-secret FILE]
  pneu account auth <name> [--force]
  pneu account status [<name>]
On a client each runs on its server over SSH (docs/client.md).`

func account(args []string) error {
	if len(args) == 0 {
		return usageError{accountUsage}
	}
	// The server's half of a client's command: the verb, then --stdin and
	// nothing else; parameters come as JSON on stdin, events go to stdout.
	if len(args) == 2 && args[1] == "--stdin" {
		return accountStdin(remote.Verb(args[0]), os.Stdin, os.Stdout)
	}
	switch args[0] {
	case "add":
		return accountAdd(args[1:])
	case "auth":
		return accountAuth(args[1:])
	case "status":
		return accountStatus(args[1:])
	}
	return usageError{fmt.Sprintf("unknown command %q\n%s", args[0], accountUsage)}
}

// parseInterspersed parses fs from args with flags allowed after positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func configPath(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return config.DefaultPath()
}

// tildePath writes p as "~/..." when it is under HOME, as INSTALL.md does.
func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// addParams are `pneu account add`'s, from the command line or a
// client's request. secret is the OAuth client JSON (nil: none given) and
// secretFrom where it came from, for messages.
type addParams struct {
	name, address, fullName string
	secret                  []byte
	secretFrom              string
}

// accountAdd is `pneu account add`: here, or on a client, on its server.
func accountAdd(args []string) error {
	fs := flag.NewFlagSet("pneu account add", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	fullName := fs.String("name", "", "your name, for From (default: another account's, or your login's)")
	secret := fs.String("client-secret", "", "the OAuth Desktop-app client JSON from Google Cloud (INSTALL.md step 4)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError{accountUsage}
	}
	p := addParams{name: pos[0], address: strings.TrimSpace(pos[1]), fullName: *fullName}
	if !config.ValidName(p.name) {
		return usageError{fmt.Sprintf("bad account name %q: %s", p.name, config.NameRule)}
	}
	if !remote.ValidAddress(p.address) {
		return usageError{fmt.Sprintf("bad address %q", p.address)}
	}
	if !remote.ValidFullName(p.fullName) {
		return usageError{fmt.Sprintf("bad --name: at most %d bytes, no control characters", remote.MaxFullName)}
	}
	if *secret != "" {
		if p.secret, err = readSecretFile(*secret); err != nil {
			return err
		}
		p.secretFrom = *secret
	}
	cfgPath, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	if target, ok, err := clientOf(cfgPath); err != nil || ok {
		if err != nil {
			return err
		}
		req := remote.AddRequest{Name: p.name, Address: p.address, FullName: p.fullName}
		if p.secret != nil {
			// Only the validated fields go to the server.
			clean, err := gmi.CleanClientSecret(p.secret)
			if err != nil {
				return fmt.Errorf("%s: %w", *secret, err)
			}
			req.ClientSecret = string(clean)
		}
		return newRemoteEnv(target).run(remote.Add, req)
	}
	return doAdd(cfgPath, p, termOut{})
}

// maxSecretFile bounds the OAuth client JSON read from disk; Google's is
// well under 1 KiB.
const maxSecretFile = 64 << 10

func readSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSecretFile {
		return nil, fmt.Errorf("%s: over %d bytes; that isn't an OAuth client JSON", path, maxSecretFile)
	}
	return b, nil
}

// clientOf reports whether the config at cfgPath is a client's, and its
// server's SSH target.
func clientOf(cfgPath string) (string, bool, error) {
	raw, err := config.ReadRaw(cfgPath)
	if err != nil {
		return "", false, err
	}
	if raw.Server == nil {
		return "", false, nil
	}
	if !config.ValidSSHTarget(raw.Server.SSH) {
		return "", false, fmt.Errorf("%s: server.ssh isn't a usable ssh target", cfgPath)
	}
	return raw.Server.SSH, true, nil
}

// doAdd lays out one account, each step checked before it acts, so a rerun
// finishes what an interrupted one started and changes nothing else.
func doAdd(cfgPath string, p addParams, o acctOut) error {
	name, address := p.name, p.address
	raw, err := config.ReadRaw(cfgPath)
	if err != nil {
		return err
	}
	if raw.Server != nil {
		return fmt.Errorf("this machine is a client of %s: accounts live on the server", raw.Server.SSH)
	}
	existing := -1
	for i, a := range raw.Accounts {
		switch {
		case a.Name == name && !strings.EqualFold(a.Email, address):
			return fmt.Errorf("account %s already exists for %s", name, a.Email)
		case a.Name != name && strings.EqualFold(a.Email, address):
			return fmt.Errorf("%s is already account %s", address, a.Name)
		case a.Name == name:
			existing = i
		}
	}
	dir := filepath.Join(filepath.Dir(cfgPath), name)
	acct := config.Account{Name: name, Email: address,
		NotmuchConfig: tildePath(filepath.Join(dir, "notmuch-config")), GmiDir: "~/mail/" + name + "/gmail"}
	if existing >= 0 {
		acct = raw.Accounts[existing]
	}
	nmConfig, err := config.ExpandHome(acct.NotmuchConfig)
	if err != nil {
		return err
	}
	gmiDir, err := config.ExpandHome(acct.GmiDir)
	if err != nil {
		return err
	}
	var others []config.Account
	for _, a := range raw.Accounts {
		if a.Name != name {
			others = append(others, a)
		}
	}
	o.say("Setting up %s (%s)", name, address)

	// 1. Directories, private even if they existed before: the config dir
	// holds the OAuth client, the mail root the mail and its index, and the
	// lieer dir the token, which lieer writes with the umask's mode
	// (remote.py:Remote.__store_credentials__, a plain open("w")).
	for _, d := range []string{filepath.Dir(nmConfig), filepath.Dir(gmiDir), gmiDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}

	// 2. The OAuth client, if given or already in place.
	secretPath := filepath.Join(filepath.Dir(nmConfig), "client_secret.json")
	if p.secret != nil {
		clean, err := gmi.CleanClientSecret(p.secret)
		if err != nil {
			return fmt.Errorf("%s: %w", p.secretFrom, err)
		}
		// Only the validated fields reach lieer.
		if err := writeSecret(secretPath, clean); err != nil {
			return err
		}
		o.say("  copied the OAuth client to %s (mode 0600)", tildePath(secretPath))
	} else if b, err := os.ReadFile(secretPath); err == nil {
		clean, err := gmi.CleanClientSecret(b)
		if err != nil {
			return fmt.Errorf("%s: %w", secretPath, err)
		}
		if err := writeSecret(secretPath, clean); err != nil {
			return err
		}
		o.say("  OAuth client in place at %s", tildePath(secretPath))
	} else {
		o.say("  no OAuth client at %s yet: put the Desktop-app JSON there (INSTALL.md step 4) before `pneu account auth`", tildePath(secretPath))
	}

	// 3. The notmuch config, and this address in the others' user.other_email.
	dbPath := filepath.Dir(gmiDir)
	var otherEmails []string
	for _, o := range others {
		otherEmails = append(otherEmails, o.Email)
	}
	if _, err := os.Stat(nmConfig); err == nil {
		o.say("  kept the existing notmuch config %s", tildePath(nmConfig))
	} else {
		who := p.fullName
		if who == "" {
			who = defaultFullName(others)
		}
		if err := os.WriteFile(nmConfig, []byte(notmuchConfig(dbPath, who, address, otherEmails)), 0o644); err != nil {
			return err
		}
		o.say("  wrote the notmuch config %s", tildePath(nmConfig))
	}
	for _, other := range others {
		if err := addOtherEmail(other, address, o); err != nil {
			return fmt.Errorf("adding %s to %s's user.other_email: %w", address, other.Name, err)
		}
	}

	// 4. The database.
	if _, err := notmuchCmd(nmConfig, "count", "*"); err != nil {
		if out, err := notmuchCmd(nmConfig, "new", "--quiet"); err != nil {
			return fmt.Errorf("notmuch new: %v\n%s", err, out)
		}
		o.say("  created the notmuch database in %s", tildePath(dbPath))
	} else {
		o.say("  notmuch database already exists")
	}

	// 5. The lieer repository, and pneu's throwaway tag in its ignore list.
	if _, err := os.Stat(filepath.Join(gmiDir, ".gmailieer.json")); err != nil {
		if out, err := gmiCmd(o.ctx(), gmiDir, nmConfig, "init", "--no-auth", "--replace-slash-with-dot", address); err != nil {
			return fmt.Errorf("gmi init: %v\n%s", err, out)
		}
		o.say("  initialized the lieer repository %s", tildePath(gmiDir))
	} else {
		o.say("  lieer repository already initialized")
	}
	ignored, err := lieerIgnoreTags(gmiDir)
	if err != nil {
		return err
	}
	if !slices.Contains(ignored, gmi.TouchTag) {
		// --ignore-tags-local replaces the list, so keep what's there.
		list := strings.Join(append(ignored, gmi.TouchTag), ",")
		if out, err := gmiCmd(o.ctx(), gmiDir, nmConfig, "set", "--ignore-tags-local", list); err != nil {
			return fmt.Errorf("gmi set: %v\n%s", err, out)
		}
		o.say("  lieer ignores the local tag %s", gmi.TouchTag)
	}

	// 6. The account in pneu's config.
	if existing < 0 {
		raw.Accounts = append(raw.Accounts, acct)
		if err := config.Write(cfgPath, raw); err != nil {
			return err
		}
		o.say("  added %s to %s", name, tildePath(cfgPath))
	} else {
		o.say("  %s already in %s", name, tildePath(cfgPath))
	}
	if running, knows := serverKnows(raw.Port, name); running && !knows {
		o.say("%s", restartNote(name))
	}
	o.say("Next: pneu account auth %s", name)
	return nil
}

// writeSecret puts b at path through a file created 0600 (never following
// a planted symlink) and renamed into place, so the client is never
// readable by others, not even between a write and a chmod.
func writeSecret(path string, b []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func defaultFullName(others []config.Account) string {
	for _, o := range others {
		p, err := config.ExpandHome(o.NotmuchConfig)
		if err != nil {
			continue
		}
		if out, err := notmuchCmd(p, "config", "get", "user.name"); err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
	}
	if u, err := user.Current(); err == nil {
		name, _, _ := strings.Cut(u.Name, ",") // GECOS
		return strings.TrimSpace(name)
	}
	return ""
}

// notmuchConfig is INSTALL.md's per-account config: settings that aren't
// notmuch's defaults and that pneu and lieer rely on.
func notmuchConfig(dbPath, name, address string, others []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by `pneu account add`; see INSTALL.md for why each setting.\n[database]\npath=%s\n\n[user]\n", dbPath)
	if name != "" {
		fmt.Fprintf(&b, "name=%s\n", name)
	}
	fmt.Fprintf(&b, "primary_email=%s\n", address)
	if len(others) > 0 {
		fmt.Fprintf(&b, "other_email=%s\n", strings.Join(others, ";"))
	}
	b.WriteString(`
[new]
tags=
ignore=.gmailieer.json;.state.gmailieer.json;.credentials.gmailieer.json;.resume-pull.gmailieer.json;.lock;/.*[.](json|lock|bak)$/

[search]
exclude_tags=spam;trash

[maildir]
synchronize_flags=false
`)
	return b.String()
}

// addOtherEmail lists address in account o's user.other_email, so its
// `notmuch reply` knows the new address is yours too.
func addOtherEmail(o config.Account, address string, w acctOut) error {
	p, err := config.ExpandHome(o.NotmuchConfig)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		return nil // not set up on this machine; its own add will list us
	}
	out, err := notmuchCmd(p, "config", "get", "user.other_email")
	if err != nil {
		return err
	}
	have := strings.Fields(out)
	if slices.ContainsFunc(have, func(s string) bool { return strings.EqualFold(s, address) }) {
		return nil
	}
	if out, err := notmuchCmd(p, append([]string{"config", "set", "user.other_email"}, append(have, address)...)...); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	w.say("  added %s to %s's user.other_email", address, o.Name)
	return nil
}

func lieerIgnoreTags(gmiDir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(gmiDir, ".gmailieer.json"))
	if err != nil {
		return nil, err
	}
	var c struct {
		IgnoreTags []string `json:"ignore_tags"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf(".gmailieer.json: %w", err)
	}
	return c.IgnoreTags, nil
}

func notmuchCmd(nmConfig string, args ...string) (string, error) {
	cmd := exec.Command("notmuch", args...)
	cmd.Env = withEnv(os.Environ(), "NOTMUCH_CONFIG="+nmConfig)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func withEnv(env []string, kv ...string) []string {
	for _, v := range kv {
		k, _, _ := strings.Cut(v, "=")
		env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, k+"=") })
		env = append(env, v)
	}
	return env
}

// lockAccount takes the account's lock, the one the server's own runs
// take, for a series of gmi runs (gmiLocked). A failure is a lock error,
// never gmi's. It gives up when ctx ends (a remote client gone).
func lockAccount(ctx context.Context, gmiDir string) (func(), error) {
	path := gmi.DefaultLockPath(gmiDir)
	return gmi.LockContext(ctx, path, gmiWait, func() {
		fmt.Fprintf(os.Stderr, "pneu account: waiting for %s (pneu is syncing)\n", path)
	})
}

// verifyWait bounds the token check (`gmi pull -t`), as the engine's.
var verifyWait = 2 * time.Minute

// gmiLocked runs gmi in the account's lieer dir; the caller holds the lock.
// With term set, gmi's output goes straight to the terminal (the consent
// flow prints its URL there) and gmi stays in the terminal's process
// group, so Ctrl-C reaches it; otherwise its output is returned, and it
// runs in a group of its own (procgroup.go) that ctx's end stops as a
// whole, and that is gone before this returns.
func gmiLocked(ctx context.Context, gmiDir, nmConfig string, term io.Writer, env []string, args ...string) (string, error) {
	bin, err := exec.LookPath("gmi")
	if err != nil {
		return "", errors.New("gmi (lieer) not found on PATH")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = gmiDir
	// Unbuffered and UTF-8, as the engine runs it (gmi.PythonEnv): term is
	// a pipe, and the consent URL must reach it while lieer waits on the
	// consent, not when lieer exits.
	cmd.Env = gmiEnvFor(nmConfig, env)
	if term != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, term, term
		return "", cmd.Run()
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	g, err := startGroup(cmd)
	if err != nil {
		return "", err
	}
	err = g.run(ctx)
	return out.String(), err
}

// gmiCmd is one gmi run under the account's lock.
func gmiCmd(ctx context.Context, gmiDir, nmConfig string, args ...string) (string, error) {
	release, err := lockAccount(ctx, gmiDir)
	if err != nil {
		return "", err
	}
	defer release()
	return gmiLocked(ctx, gmiDir, nmConfig, nil, nil, args...)
}

func loadAccount(cfgPath, name string) (config.Account, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Account{}, err
	}
	a, ok := cfg.Account(name)
	if !ok {
		return config.Account{}, fmt.Errorf("no account %q in %s: run `pneu account add` first", name, cfgPath)
	}
	return a, nil
}

// accountAuth is `pneu account auth`: here, or on a client, on its server
// with the consent forwarded to this machine's browser.
func accountAuth(args []string) error {
	fs := flag.NewFlagSet("pneu account auth", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	force := fs.Bool("force", false, "replace existing credentials; kept if consent fails")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{accountUsage}
	}
	if !config.ValidName(pos[0]) {
		return usageError{fmt.Sprintf("bad account name %q: %s", pos[0], config.NameRule)}
	}
	cfgPath, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	if target, ok, err := clientOf(cfgPath); err != nil || ok {
		if err != nil {
			return err
		}
		return newRemoteEnv(target).run(remote.Auth, remote.AuthRequest{Name: pos[0], Force: *force, ConsentOpen: remote.ConsentPrint})
	}
	return doAuth(cfgPath, pos[0], *force, termOut{})
}

// doAuth runs lieer's consent flow for an account, then proves the token
// with a label listing. It never reads the credentials lieer writes.
func doAuth(cfgPath, name string, force bool, o acctOut) error {
	a, err := loadAccount(cfgPath, name)
	if err != nil {
		return err
	}
	st := gmi.FileState(a.GmiDir)
	if st == gmi.StateUnconfigured {
		return fmt.Errorf("%s has no lieer repository: run `pneu account add %s %s` first", a.Name, a.Name, a.Email)
	}
	secret := filepath.Join(filepath.Dir(a.NotmuchConfig), "client_secret.json")
	if st != gmi.StateUnauthorized && !force {
		o.say("%s already has credentials; checking they work (--force replaces them)", a.Name)
	} else {
		b, err := os.ReadFile(secret)
		if err != nil {
			return fmt.Errorf("no OAuth client at %s: download the Desktop-app client JSON there (INSTALL.md step 4). Without it lieer would silently use its own shared client", secret)
		}
		if _, err := gmi.CleanClientSecret(b); err != nil {
			return fmt.Errorf("%s: %w (rerun `pneu account add` with --client-secret)", secret, err)
		}
		if err := checkAuthPort(); err != nil {
			return fmt.Errorf("%w (`ss -tanp 'sport = :8080'` shows what holds it)", err)
		}
	}
	// One lock across the consent and the check: released between them, the
	// server (which looks every few seconds) would start the first pull and
	// hold the lock through the check.
	release, err := lockAccount(o.ctx(), a.GmiDir)
	if err != nil {
		return err
	}
	defer release()
	if st == gmi.StateUnauthorized || force {
		o.say("Opening Google's consent screen for %s. Sign in as %s.", a.Name, a.Email)
		o.say(`If Google says "Something went wrong", use a private window and sign in with your password.`)
		authArgs := []string{"auth", "-c", secret}
		if force {
			authArgs = []string{"auth", "-f", "-c", secret}
		}
		// lieer -f deletes the credentials before consent: an interrupted
		// or refused consent would leave none. Set the old ones aside (a
		// rename: pneu never reads them) and put them back if it fails.
		creds := filepath.Join(a.GmiDir, gmi.CredentialsFile)
		aside := strings.TrimSuffix(creds, ".json") + ".pneu-old.json" // notmuch ignores *.json
		kept := os.Rename(creds, aside) == nil
		if err := o.consent(a.GmiDir, a.NotmuchConfig, []string{"BROWSER=true"}, authArgs...); err != nil {
			if _, statErr := os.Stat(creds); kept && errors.Is(statErr, os.ErrNotExist) {
				if os.Rename(aside, creds) == nil {
					o.say("Consent didn't finish; the previous credentials are back in place.")
				}
			}
			os.Remove(aside)
			return fmt.Errorf("gmi auth: %w", err)
		}
		os.Remove(aside)
		if err := gmi.PrivateCredentials(a.GmiDir); err != nil {
			return err
		}
	}
	vctx, cancel := context.WithTimeout(o.ctx(), verifyWait)
	out, err := gmiLocked(vctx, a.GmiDir, a.NotmuchConfig, nil, nil, "pull", "-t")
	timedOut := errors.Is(vctx.Err(), context.DeadlineExceeded)
	cancel()
	if o.ctx().Err() != nil {
		return fmt.Errorf("checking the token: %w", o.ctx().Err()) // the client went
	}
	if timedOut {
		return fmt.Errorf("checking the token: no answer from Gmail within %v", verifyWait)
	}
	if err != nil && !errors.As(err, new(*exec.ExitError)) {
		return fmt.Errorf("checking the token: %w", err) // gmi didn't run
	}
	if err != nil {
		msg := lastLine(out)
		if strings.Contains(out, "invalid_grant") {
			return fmt.Errorf("the token doesn't work (%s): rerun with --force", msg)
		}
		return fmt.Errorf("Gmail refused %s (%s): was consent given as another Google account? Rerun with --force", a.Email, msg)
	}
	o.say("Authorized: Gmail answers for %s.", a.Email)
	if gmi.FileState(a.GmiDir) == gmi.StateNeedsPull {
		cfg, _ := config.Load(cfgPath)
		switch running, knows := serverKnows(cfg.Port, a.Name); {
		case running && knows:
			o.say("The running pneu server starts downloading the mail within seconds, newest first.")
		case running:
			o.say("%s", restartNote(a.Name))
		default:
			o.say("Start the pneu server (INSTALL.md step 6); it downloads the mail itself, newest first.")
		}
	}
	return nil
}

// checkAuthPort is gmi.CheckAuthPort; tests whose stub consent never binds
// the port replace it.
var checkAuthPort = gmi.CheckAuthPort

// consentOpener reads `gmi auth`'s output line by line and opens the
// consent URL it announces, once, without waiting for the browser. lieer
// itself runs with BROWSER=true: its webbrowser.open runs $BROWSER (which
// Omarchy sets in interactive shells) and waits for it to exit, and a
// browser that wasn't already running exits only when it's closed, so
// Google's redirect would sit unanswered on localhost:8080 until then.
//
// term gets the output as it comes (the terminal), or lines gets each
// line but the consent URL's (a client's event stream). open is openURL
// here, or the consent-url event (consentOpen "print").
type consentOpener struct {
	term   io.Writer
	lines  func(string)
	open   func(string) error
	line   []byte
	opened bool
}

// maxGmiLine bounds the line consentOpener holds; the consent URL's line
// is far shorter, and the rest of a longer line is dropped.
const maxGmiLine = 16 << 10

func (c *consentOpener) Write(b []byte) (int, error) {
	n := len(b)
	var err error
	if c.term != nil {
		n, err = c.term.Write(b)
	}
	for _, ch := range b {
		if ch != '\n' {
			if len(c.line) < maxGmiLine {
				c.line = append(c.line, ch)
			}
			continue
		}
		c.endLine()
	}
	return n, err
}

// endLine handles the line held so far.
func (c *consentOpener) endLine() {
	line := strings.TrimSuffix(string(c.line), "\r")
	c.line = c.line[:0]
	if u, ok := gmi.ConsentURL(line); ok {
		if !c.opened {
			c.opened = true
			if oerr := c.open(u); oerr != nil && c.term != nil {
				fmt.Fprintf(c.term, "Couldn't open a browser (%v): open the URL above yourself.\n", oerr)
			}
		}
		return
	}
	if c.lines != nil && strings.TrimSpace(line) != "" {
		c.lines(line)
	}
}

// flush handles a last line without a newline.
func (c *consentOpener) flush() {
	if len(c.line) > 0 {
		c.endLine()
	}
}

// openURL opens u with xdg-open in its own session and doesn't wait for it.
func openURL(u string) error {
	cmd := exec.Command("xdg-open", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func restartNote(name string) string {
	return fmt.Sprintf("The pneu server is running, and it reads its accounts only when it starts: restart it so it picks up %s:\n  systemctl --user restart pneu.service", name)
}

// serverKnows reports whether a pneu server is running, and whether it has
// account name. A fresh status file answers both (it lists the server's
// accounts); failing that, a pneu answering on the port is running but its
// accounts are unknown, so knows is false.
func serverKnows(port int, name string) (running, knows bool) {
	if p, err := web.StatusPath(); err == nil {
		if doc, err := web.ReadStatus(p); err == nil {
			t, _ := time.Parse(time.RFC3339, doc.Updated)
			if doc.Running && time.Since(t) < 20*time.Minute {
				return true, slices.ContainsFunc(doc.Accounts, func(a web.StatusAccount) bool { return a.Name == name })
			}
		}
	}
	if port == 0 {
		port = config.DefaultPort
	}
	cfg := config.Config{Port: port}
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodHead, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/", nil)
	req.Host = hostFor(cfg)
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusForbidden, false // pneu answers 403 without its cookie
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// accountStatus is `pneu account status`: here, or on a client, on its
// server.
func accountStatus(args []string) error {
	fs := flag.NewFlagSet("pneu account status", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	cfgPath, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	if target, ok, err := clientOf(cfgPath); err != nil || ok {
		if err != nil {
			return err
		}
		// One name or none: the request carries one.
		if len(pos) > 1 {
			return usageError{"on a client, pneu account status takes at most one account name"}
		}
		req := remote.StatusRequest{}
		if len(pos) == 1 {
			if !config.ValidName(pos[0]) {
				return usageError{fmt.Sprintf("bad account name %q: %s", pos[0], config.NameRule)}
			}
			req.Name = pos[0]
		}
		return newRemoteEnv(target).run(remote.Status, req)
	}
	return doStatus(cfgPath, pos, termOut{})
}

// doStatus prints each account's state: the running server's view
// (status.json) when it's fresh, else what lieer's files say.
func doStatus(cfgPath string, names []string, o acctOut) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Server != nil {
		return fmt.Errorf("this machine is a client of %s: accounts live on the server", cfg.Server.SSH)
	}
	live := map[string]web.StatusAccount{}
	server := "the server isn't running; states are from lieer's files"
	if p, err := web.StatusPath(); err == nil {
		if doc, err := web.ReadStatus(p); err == nil {
			t, _ := time.Parse(time.RFC3339, doc.Updated)
			if doc.Running && time.Since(t) < 20*time.Minute {
				server = "from the running server"
				for _, a := range doc.Accounts {
					live[a.Name] = a
				}
			}
		}
	}
	found := false
	for _, a := range cfg.Accounts {
		if len(names) > 0 && !slices.Contains(names, a.Name) {
			continue
		}
		found = true
		line := fmt.Sprintf("%-12s %s", a.Name, gmi.FileState(a.GmiDir))
		// A status file from before account states has none to show.
		if l, ok := live[a.Name]; ok && l.State != "" {
			line = fmt.Sprintf("%-12s %s", a.Name, l.State)
			if p := l.Progress; p != nil {
				line += " · " + web.DescribeProgress(p)
			}
			if l.Error != nil {
				line += " · " + *l.Error
			}
		}
		o.say("%s", line)
	}
	if !found {
		return fmt.Errorf("no such account")
	}
	o.say("(%s)", server)
	return nil
}
