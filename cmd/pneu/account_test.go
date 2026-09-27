package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/gmi"
)

// sandbox gives the test a fresh HOME with stubgmi on PATH as gmi, so
// nothing touches ~/.config/pneu, ~/mail or the real lieer.
func sandbox(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("notmuch"); err != nil {
		t.Skip("notmuch not on PATH")
	}
	home := t.TempDir()
	bin := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "gmi"), "../../internal/testmail/cmd/stubgmi").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("NOTMUCH_CONFIG", "")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"STUBGMI_FAIL", "STUBGMI_AUTH", "STUBGMI_DURATION"} {
		t.Setenv(k, "")
	}
	return home
}

const clientJSON = `{"installed":{"client_id":"1-x.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`

func TestAccountAddAuth(t *testing.T) {
	home := sandbox(t)
	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(clientJSON), 0o644)

	if err := account([]string{"add", "personal", "me@example.com", "--name", "Pat Doe", "--client-secret", secret}); err != nil {
		t.Fatal(err)
	}
	// A rerun changes nothing and succeeds.
	if err := account([]string{"add", "personal", "me@example.com"}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if err := account([]string{"add", "personal", "other@example.com"}); err == nil {
		t.Fatal("re-adding a name with another address succeeded")
	}

	raw, err := config.ReadRaw(filepath.Join(home, ".config", "pneu", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Accounts) != 1 || raw.Accounts[0] != (config.Account{Name: "personal", Email: "me@example.com",
		NotmuchConfig: "~/.config/pneu/personal/notmuch-config", GmiDir: "~/mail/personal/gmail"}) || raw.Port != config.DefaultPort {
		t.Fatalf("config.json: %+v", raw)
	}
	nm, _ := os.ReadFile(filepath.Join(home, ".config", "pneu", "personal", "notmuch-config"))
	for _, want := range []string{"path=" + filepath.Join(home, "mail", "personal") + "\n", "name=Pat Doe\n", "primary_email=me@example.com\n", "\ntags=\n", "synchronize_flags=false"} {
		if !strings.Contains(string(nm), want) {
			t.Errorf("notmuch-config lacks %q:\n%s", want, nm)
		}
	}
	if fi, _ := os.Stat(filepath.Join(home, ".config", "pneu", "personal", "client_secret.json")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("client secret: %v", fi)
	}
	if fi, _ := os.Stat(filepath.Join(home, ".config", "pneu", "personal")); fi == nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("account config dir: %v", fi)
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".config", "pneu", "personal", "*.tmp")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	gmiDir := filepath.Join(home, "mail", "personal", "gmail")
	b, _ := os.ReadFile(filepath.Join(gmiDir, ".gmailieer.json"))
	var lc struct {
		Account    string   `json:"account"`
		Replace    bool     `json:"replace_slash_with_dot"`
		IgnoreTags []string `json:"ignore_tags"`
	}
	json.Unmarshal(b, &lc)
	if lc.Account != "me@example.com" || !lc.Replace || len(lc.IgnoreTags) != 1 || lc.IgnoreTags[0] != gmi.TouchTag {
		t.Fatalf(".gmailieer.json: %s", b)
	}
	if st := gmi.FileState(gmiDir); st != gmi.StateUnauthorized {
		t.Fatalf("after add: %s", st)
	}

	// A second account lists the first's address and is listed by it. Its
	// config dir, made loosely beforehand, is tightened.
	os.MkdirAll(filepath.Join(home, ".config", "pneu", "work"), 0o755)
	os.MkdirAll(filepath.Join(home, "mail", "work", "gmail"), 0o755)
	if err := account([]string{"add", "work", "me@work.example"}); err != nil {
		t.Fatal(err)
	}
	for acct, other := range map[string]string{"personal": "me@work.example", "work": "me@example.com"} {
		out, err := notmuchCmd(filepath.Join(home, ".config", "pneu", acct, "notmuch-config"), "config", "get", "user.other_email")
		if err != nil || strings.TrimSpace(out) != other {
			t.Errorf("%s other_email = %q, %v", acct, out, err)
		}
	}

	for _, d := range []string{filepath.Join(home, ".config", "pneu", "work"), filepath.Join(home, "mail", "work"), filepath.Join(home, "mail", "work", "gmail")} {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("existing %s left %v", d, fi.Mode().Perm())
		}
	}
	// auth: no client JSON for work, so it refuses rather than let lieer
	// fall back to its shared client.
	if err := account([]string{"auth", "work"}); err == nil || !strings.Contains(err.Error(), "no OAuth client") {
		t.Fatalf("auth without a client: %v", err)
	}
	if err := account([]string{"auth", "personal"}); err != nil {
		t.Fatal(err)
	}
	if st := gmi.FileState(gmiDir); st != gmi.StateNeedsPull {
		t.Fatalf("after auth: %s", st)
	}
	// lieer wrote the token 0644 (umask 022); auth made it private.
	if fi, err := os.Stat(filepath.Join(gmiDir, ".credentials.gmailieer.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials: %v %v", fi, err)
	}
	for _, d := range []string{filepath.Join(home, "mail", "personal"), gmiDir} {
		if fi, _ := os.Stat(d); fi == nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v", d, fi)
		}
	}
	if err := account([]string{"status"}); err != nil {
		t.Fatal(err)
	}
}

func TestCleanClientSecret(t *testing.T) {
	for _, bad := range []string{`nope`, `{"web":{"client_id":"1-x.apps.googleusercontent.com","client_secret":"s"}}`, `{"installed":{"client_id":"x"}}`,
		strings.Replace(clientJSON, "https://accounts.google.com/", "https://accounts.google.com.evil.example/", 1),
		strings.Replace(clientJSON, "https://oauth2.googleapis.com/", "http://oauth2.googleapis.com/", 1),
		strings.Replace(clientJSON, `,"token_uri":"https://oauth2.googleapis.com/token"`, "", 1),
		// A clean "installed" beside a "web" that google_auth_oauthlib prefers.
		strings.TrimSuffix(clientJSON, "}") + `,"web":{"client_id":"1-x.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://evil.example/token"}}`} {
		if _, err := gmi.CleanClientSecret([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	clean, err := gmi.CleanClientSecret([]byte(clientJSON))
	if err != nil {
		t.Fatal(err)
	}
	// Only the validated fields survive.
	extra := strings.Replace(clientJSON, `"client_secret":"s"`, `"client_secret":"s","redirect_uris":["http://evil.example"]`, 1)
	if clean2, err := gmi.CleanClientSecret([]byte(extra)); err != nil || string(clean2) != string(clean) || strings.Contains(string(clean2), "evil") {
		t.Errorf("extra fields: %s, %v", clean2, err)
	}
}

// capture runs fn with stdout going to a pipe and returns what it printed.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	out := make(chan string)
	go func() { b, _ := io.ReadAll(r); out <- string(b) }()
	ferr := fn()
	os.Stdout = old
	w.Close()
	return <-out, ferr
}

// An account added while the server runs isn't picked up until a restart
// (accounts are fixed at startup): add says so, and never restarts it.
func TestAccountAddWhileServing(t *testing.T) {
	home := sandbox(t)
	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(clientJSON), 0o644)
	if err := account([]string{"add", "personal", "me@example.com", "--client-secret", secret}); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(home, ".local", "state", "pneu", "status.json")
	os.MkdirAll(filepath.Dir(status), 0o700)
	os.WriteFile(status, []byte(`{"version":1,"updated":"`+time.Now().Format(time.RFC3339)+`","running":true,"accounts":[{"name":"personal"}]}`), 0o600)

	if running, knows := serverKnows(config.DefaultPort, "personal"); !running || !knows {
		t.Fatalf("personal: running %v knows %v", running, knows)
	}
	out, err := capture(t, func() error { return account([]string{"add", "work", "me@work.example"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "systemctl --user restart pneu.service") {
		t.Errorf("add while serving didn't say to restart:\n%s", out)
	}
	out, _ = capture(t, func() error { return account([]string{"auth", "personal"}) })
	if !strings.Contains(out, "starts downloading the mail within seconds") {
		t.Errorf("auth of an account the server has:\n%s", out)
	}
	// A stale file is no server (and nothing answers on the port here).
	os.WriteFile(status, []byte(`{"version":1,"updated":"2020-01-01T00:00:00Z","running":true,"accounts":[]}`), 0o600)
	if running, _ := serverKnows(1, "work"); running {
		t.Error("stale status file read as a running server")
	}
}

// A browser that wasn't already running only exits when it's closed. auth
// must not wait for it: lieer runs with BROWSER=true and pneu opens the
// consent URL itself, detached. The fake browser here grants consent, then
// stays up; it's both $BROWSER, which lieer would otherwise wait on, and
// xdg-open, which pneu uses.
func TestAccountAuthDoesNotWaitForBrowser(t *testing.T) {
	home := sandbox(t)
	bin := filepath.Dir(must(exec.LookPath("gmi")))
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "rehearse"), "../../internal/testmail/cmd/rehearse").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	browser := filepath.Join(bin, "xdg-open")
	os.WriteFile(browser, []byte("#!/bin/sh\nrehearse grant \"$1\" >/dev/null 2>&1 &\nexec sleep 20\n"), 0o755)
	t.Setenv("BROWSER", browser)
	t.Setenv("STUBGMI_AUTH", "browser")
	if err := gmi.CheckAuthPort(); err != nil {
		t.Skip(err)
	}
	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(clientJSON), 0o644)
	if err := account([]string{"add", "personal", "me@example.com", "--client-secret", secret}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := capture(t, func() error { return account([]string{"auth", "personal"}) }); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("auth took %v: it waited for the browser", d)
	}
	creds := filepath.Join(home, "mail", "personal", "gmail", gmi.CredentialsFile)
	good, err := os.ReadFile(creds)
	if err != nil {
		t.Fatal(err)
	}

	// --force with a consent that fails (a redirect whose state doesn't
	// match) keeps the working credentials.
	os.WriteFile(browser, []byte("#!/bin/sh\ncurl -s 'http://localhost:8080/?state=wrong&code=c' >/dev/null\n"), 0o755)
	if _, err := capture(t, func() error { return account([]string{"auth", "personal", "--force"}) }); err == nil {
		t.Fatal("auth --force succeeded without consent")
	}
	if got, err := os.ReadFile(creds); err != nil || !bytes.Equal(got, good) {
		t.Fatalf("credentials after a failed --force: %q, %v", got, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
