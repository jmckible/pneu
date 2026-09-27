package main

// pneu account: the one-time, terminal half of setting up an account
// (docs/onboarding.md "Where the line falls"). add lays out the account;
// auth runs the consent flow and proves the token; status reports where
// each account stands. The first pull is the server's.

import (
	"bytes"
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
	"github.com/jmckible/pneu/internal/web"
)

const accountUsage = `usage:
  pneu account add <name> <address> [--name "Full Name"] [--client-secret FILE]
  pneu account auth <name> [--force]
  pneu account status [<name>]`

func account(args []string) error {
	if len(args) == 0 {
		return usageError{accountUsage}
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

func step(format string, a ...any) { fmt.Printf("  "+format+"\n", a...) }

// accountAdd lays out one account, each step checked before it acts, so a
// rerun finishes what an interrupted one started and changes nothing else.
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
	name, address := pos[0], strings.TrimSpace(pos[1])
	if !config.ValidName(name) {
		return usageError{fmt.Sprintf("bad account name %q: a short word like personal or work", name)}
	}
	if !strings.Contains(address, "@") || strings.ContainsAny(address, " <>") {
		return usageError{fmt.Sprintf("bad address %q", address)}
	}
	cfgPath, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	raw, err := config.ReadRaw(cfgPath)
	if err != nil {
		return err
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
	fmt.Printf("Setting up %s (%s)\n", name, address)

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
	if *secret != "" {
		b, err := os.ReadFile(*secret)
		if err != nil {
			return err
		}
		clean, err := gmi.CleanClientSecret(b)
		if err != nil {
			return fmt.Errorf("%s: %w", *secret, err)
		}
		// Only the validated fields reach lieer.
		if err := writeSecret(secretPath, clean); err != nil {
			return err
		}
		step("copied the OAuth client to %s (mode 0600)", tildePath(secretPath))
	} else if b, err := os.ReadFile(secretPath); err == nil {
		clean, err := gmi.CleanClientSecret(b)
		if err != nil {
			return fmt.Errorf("%s: %w", secretPath, err)
		}
		if err := writeSecret(secretPath, clean); err != nil {
			return err
		}
		step("OAuth client in place at %s", tildePath(secretPath))
	} else {
		step("no OAuth client at %s yet: put the Desktop-app JSON there (INSTALL.md step 4) before `pneu account auth`", tildePath(secretPath))
	}

	// 3. The notmuch config, and this address in the others' user.other_email.
	dbPath := filepath.Dir(gmiDir)
	var otherEmails []string
	for _, o := range others {
		otherEmails = append(otherEmails, o.Email)
	}
	if _, err := os.Stat(nmConfig); err == nil {
		step("kept the existing notmuch config %s", tildePath(nmConfig))
	} else {
		who := *fullName
		if who == "" {
			who = defaultFullName(others)
		}
		if err := os.WriteFile(nmConfig, []byte(notmuchConfig(dbPath, who, address, otherEmails)), 0o644); err != nil {
			return err
		}
		step("wrote the notmuch config %s", tildePath(nmConfig))
	}
	for _, o := range others {
		if err := addOtherEmail(o, address); err != nil {
			return fmt.Errorf("adding %s to %s's user.other_email: %w", address, o.Name, err)
		}
	}

	// 4. The database.
	if _, err := notmuchCmd(nmConfig, "count", "*"); err != nil {
		if out, err := notmuchCmd(nmConfig, "new", "--quiet"); err != nil {
			return fmt.Errorf("notmuch new: %v\n%s", err, out)
		}
		step("created the notmuch database in %s", tildePath(dbPath))
	} else {
		step("notmuch database already exists")
	}

	// 5. The lieer repository, and pneu's throwaway tag in its ignore list.
	if _, err := os.Stat(filepath.Join(gmiDir, ".gmailieer.json")); err != nil {
		if out, err := gmiCmd(gmiDir, nmConfig, nil, "init", "--no-auth", "--replace-slash-with-dot", address); err != nil {
			return fmt.Errorf("gmi init: %v\n%s", err, out)
		}
		step("initialized the lieer repository %s", tildePath(gmiDir))
	} else {
		step("lieer repository already initialized")
	}
	ignored, err := lieerIgnoreTags(gmiDir)
	if err != nil {
		return err
	}
	if !slices.Contains(ignored, gmi.TouchTag) {
		// --ignore-tags-local replaces the list, so keep what's there.
		list := strings.Join(append(ignored, gmi.TouchTag), ",")
		if out, err := gmiCmd(gmiDir, nmConfig, nil, "set", "--ignore-tags-local", list); err != nil {
			return fmt.Errorf("gmi set: %v\n%s", err, out)
		}
		step("lieer ignores the local tag %s", gmi.TouchTag)
	}

	// 6. The account in pneu's config.
	if existing < 0 {
		raw.Accounts = append(raw.Accounts, acct)
		if err := config.Write(cfgPath, raw); err != nil {
			return err
		}
		step("added %s to %s", name, tildePath(cfgPath))
	} else {
		step("%s already in %s", name, tildePath(cfgPath))
	}
	if running, knows := serverKnows(raw.Port, name); running && !knows {
		fmt.Println(restartNote(name))
	}
	fmt.Printf("Next: pneu account auth %s\n", name)
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
func addOtherEmail(o config.Account, address string) error {
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
	step("added %s to %s's user.other_email", address, o.Name)
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
// never gmi's.
func lockAccount(gmiDir string) (func(), error) {
	path := gmi.DefaultLockPath(gmiDir)
	return gmi.Lock(path, gmiWait, func() {
		fmt.Fprintf(os.Stderr, "pneu account: waiting for %s (pneu is syncing)\n", path)
	})
}

// gmiLocked runs gmi in the account's lieer dir; the caller holds the lock.
// With term set, gmi's output goes straight to the terminal (the consent
// flow prints its URL there); otherwise it is returned.
func gmiLocked(gmiDir, nmConfig string, term io.Writer, env []string, args ...string) (string, error) {
	bin, err := exec.LookPath("gmi")
	if err != nil {
		return "", errors.New("gmi (lieer) not found on PATH")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = gmiDir
	cmd.Env = withEnv(os.Environ(), append([]string{"NOTMUCH_CONFIG=" + nmConfig}, env...)...)
	var out bytes.Buffer
	if term != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, term, term
	} else {
		cmd.Stdout, cmd.Stderr = &out, &out
	}
	err = cmd.Run()
	return out.String(), err
}

// gmiCmd is one gmi run under the account's lock.
func gmiCmd(gmiDir, nmConfig string, term io.Writer, args ...string) (string, error) {
	release, err := lockAccount(gmiDir)
	if err != nil {
		return "", err
	}
	defer release()
	return gmiLocked(gmiDir, nmConfig, term, nil, args...)
}

func loadAccount(cfgFlag, name string) (config.Account, error) {
	cfgPath, err := configPath(cfgFlag)
	if err != nil {
		return config.Account{}, err
	}
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

// accountAuth runs lieer's consent flow for an account, then proves the
// token with a label listing. It never reads the credentials lieer writes.
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
	a, err := loadAccount(*cfgFlag, pos[0])
	if err != nil {
		return err
	}
	st := gmi.FileState(a.GmiDir)
	if st == gmi.StateUnconfigured {
		return fmt.Errorf("%s has no lieer repository: run `pneu account add %s %s` first", a.Name, a.Name, a.Email)
	}
	secret := filepath.Join(filepath.Dir(a.NotmuchConfig), "client_secret.json")
	if st != gmi.StateUnauthorized && !*force {
		fmt.Printf("%s already has credentials; checking they work (--force replaces them)\n", a.Name)
	} else {
		b, err := os.ReadFile(secret)
		if err != nil {
			return fmt.Errorf("no OAuth client at %s: download the Desktop-app client JSON there (INSTALL.md step 4). Without it lieer would silently use its own shared client", secret)
		}
		if _, err := gmi.CleanClientSecret(b); err != nil {
			return fmt.Errorf("%s: %w (rerun `pneu account add` with --client-secret)", secret, err)
		}
		if err := gmi.CheckAuthPort(); err != nil {
			return fmt.Errorf("%w: stop whatever listens there and rerun", err)
		}
	}
	// One lock across the consent and the check: released between them, the
	// server (which looks every few seconds) would start the first pull and
	// hold the lock through the check.
	release, err := lockAccount(a.GmiDir)
	if err != nil {
		return err
	}
	defer release()
	if st == gmi.StateUnauthorized || *force {
		fmt.Printf("Opening Google's consent screen for %s. Sign in as %s.\n", a.Name, a.Email)
		fmt.Println(`If Google says "Something went wrong", use a private window and sign in with your password.`)
		authArgs := []string{"auth", "-c", secret}
		if *force {
			authArgs = []string{"auth", "-f", "-c", secret}
		}
		// lieer -f deletes the credentials before consent: an interrupted
		// or refused consent would leave none. Put the old ones back.
		creds := filepath.Join(a.GmiDir, gmi.CredentialsFile)
		old, oldErr := os.ReadFile(creds)
		if _, err := gmiLocked(a.GmiDir, a.NotmuchConfig, &consentOpener{term: os.Stdout}, []string{"BROWSER=true"}, authArgs...); err != nil {
			if _, statErr := os.Stat(creds); oldErr == nil && errors.Is(statErr, os.ErrNotExist) {
				if werr := os.WriteFile(creds, old, 0o600); werr == nil {
					fmt.Println("Consent didn't finish; the previous credentials are back in place.")
				}
			}
			return fmt.Errorf("gmi auth: %w", err)
		}
		if err := gmi.PrivateCredentials(a.GmiDir); err != nil {
			return err
		}
	}
	out, err := gmiLocked(a.GmiDir, a.NotmuchConfig, nil, nil, "pull", "-t")
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
	fmt.Printf("Authorized: Gmail answers for %s.\n", a.Email)
	if gmi.FileState(a.GmiDir) == gmi.StateNeedsPull {
		cfg, _ := config.Load(mustConfigPath(*cfgFlag))
		switch running, knows := serverKnows(cfg.Port, a.Name); {
		case running && knows:
			fmt.Println("The running pneu server starts downloading the mail within seconds, newest first.")
		case running:
			fmt.Println(restartNote(a.Name))
		default:
			fmt.Println("Start the pneu server (INSTALL.md step 6); it downloads the mail itself, newest first.")
		}
	}
	return nil
}

// consentOpener passes `gmi auth`'s output through to the terminal and
// opens the consent URL it announces in the default browser, once, without
// waiting for the browser. lieer itself runs with BROWSER=true: its
// webbrowser.open runs $BROWSER (which Omarchy sets in interactive shells)
// and waits for it to exit, and a browser that wasn't already running exits
// only when it's closed, so Google's redirect would sit unanswered on
// localhost:8080 until then.
type consentOpener struct {
	term   io.Writer
	line   []byte
	opened bool
}

func (c *consentOpener) Write(b []byte) (int, error) {
	n, err := c.term.Write(b)
	for _, ch := range b {
		if ch != '\n' {
			c.line = append(c.line, ch)
			continue
		}
		if u, ok := gmi.ConsentURL(string(c.line)); ok && !c.opened {
			c.opened = true
			if oerr := openURL(u); oerr != nil {
				fmt.Fprintf(c.term, "Couldn't open a browser (%v): open the URL above yourself.\n", oerr)
			}
		}
		c.line = c.line[:0]
	}
	return n, err
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

func mustConfigPath(p string) string {
	p, _ = configPath(p)
	return p
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

// accountStatus prints each account's state: the running server's view
// (status.json) when it's fresh, else what lieer's files say.
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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
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
		if len(pos) > 0 && !slices.Contains(pos, a.Name) {
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
		fmt.Println(line)
	}
	if !found {
		return fmt.Errorf("no such account")
	}
	fmt.Printf("(%s)\n", server)
	return nil
}
