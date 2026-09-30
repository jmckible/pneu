package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/testmail"
	"github.com/jmckible/pneu/internal/web"
)

func account(a testmail.Account) notmuch.Account {
	return notmuch.Account{Name: a.Name, Email: a.Email, ConfigPath: a.NotmuchConfig}
}

// Extract writes what the thread view renders, newest first, private, and
// leaves the database as it was.
func TestExtract(t *testing.T) {
	env := testmail.Setup(t)
	ctx := context.Background()
	for _, ta := range env.Accounts {
		acct := account(ta)
		before, err := acct.Revision(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := acct.MessageIDs(ctx, Query)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, id := range ids {
			m, err := acct.Message(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if html, ok := web.HTMLBody(&m); ok {
				want = append(want, html)
			}
		}
		if len(want) < 3 {
			t.Fatalf("%s: only %d HTML bodies in the fixture", ta.Name, len(want))
		}

		dir := filepath.Join(t.TempDir(), "bodies")
		got, err := Extract(ctx, acct, 1000, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d bodies, want %d", ta.Name, len(got), len(want))
		}
		st, _ := os.Stat(dir)
		if st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v", st.Mode().Perm())
		}
		for i, e := range got {
			p := filepath.Join(dir, e.File)
			b, err := os.ReadFile(p)
			if err != nil || string(b) != want[i] {
				t.Errorf("%s: %s is not body %d (%v)", ta.Name, e.File, i, err)
			}
			if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v", e.File, st.Mode().Perm())
			}
			if e.Domain == "" || strings.ContainsAny(e.Domain, "@<> ") {
				t.Errorf("%s: domain %q", e.File, e.Domain)
			}
		}
		var man []Entry
		b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err := json.Unmarshal(b, &man); err != nil || !reflect.DeepEqual(man, got) {
			t.Errorf("manifest %s (%v)", b, err)
		}

		// n caps it, newest first; an existing directory is refused.
		two := filepath.Join(t.TempDir(), "two")
		got2, err := Extract(ctx, acct, 2, two)
		if err != nil || len(got2) != 2 {
			t.Fatalf("n=2: %v %v", got2, err)
		}
		if b, _ := os.ReadFile(filepath.Join(two, got2[1].File)); string(b) != want[1] {
			t.Error("n=2: not the newest two")
		}
		if _, err := Extract(ctx, acct, 2, two); err == nil {
			t.Error("an existing directory was reused")
		}

		after, err := acct.Revision(ctx)
		if err != nil || after != before {
			t.Errorf("%s: the database changed: %s → %s (%v)", ta.Name, before, after, err)
		}
	}
}

// No qualifying bodies is an empty manifest, not null; failures are the
// categories, never notmuch's words or a path.
func TestExtractNoneAndErrors(t *testing.T) {
	env := testmail.Setup(t)
	ctx := context.Background()
	acct := account(env.Account(t, "personal"))
	dir := filepath.Join(t.TempDir(), "none")
	got, err := extract(ctx, acct, "id:no-such-message@ctaeval.test", 10, dir)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("none: %v %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "manifest.json")); string(b) != "[]" {
		t.Errorf("manifest %q", b)
	}

	bad := acct
	bad.ConfigPath = filepath.Join(t.TempDir(), "secret-name", "notmuch-config")
	_, err = Extract(ctx, bad, 5, filepath.Join(t.TempDir(), "bad"))
	if err != ErrSearch || strings.Contains(err.Error(), "secret-name") {
		t.Errorf("search failure: %v", err)
	}
	if _, err := Extract(ctx, acct, 5, dir); err != ErrOut {
		t.Errorf("existing dir: %v", err)
	}
}

// The binary's own failures print a category and nothing of the config.
func TestMainErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "ctaeval")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfg := filepath.Join(t.TempDir(), "secret-cfg.json")
	os.WriteFile(cfg, []byte(`{"accounts":[{"name":"p","email":"p@secret.example","notmuchConfig":"/nonexistent/secret-db/notmuch-config","gmiDir":"/nonexistent"}]}`), 0o600)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-config", "/nonexistent/secret-cfg.json", "-account", "p", "-out", filepath.Join(t.TempDir(), "o")}, "ctaeval: cannot read the pneu config\n"},
		{[]string{"-config", cfg, "-account", "nobody", "-out", filepath.Join(t.TempDir(), "o")}, "ctaeval: no such account in the pneu config\n"},
		{[]string{"-config", cfg, "-account", "p", "-out", filepath.Join(t.TempDir(), "o")}, "ctaeval: notmuch search failed\n"},
	} {
		out, err := exec.Command(bin, c.args...).CombinedOutput()
		if err == nil || string(out) != c.want {
			t.Errorf("%v: %q (%v), want %q", c.args, out, err, c.want)
		}
	}
}

// The browser half's local server has no body route and answers only its
// own Host (scripts/ctaeval.test.mjs).
func TestServer(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command("node", "--test", "../../../../scripts/ctaeval.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}

// With no bodies the browser half prints a zero summary and starts no
// browser.
func TestBrowserHalfEmpty(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "bodies"), 0o700)
	os.WriteFile(filepath.Join(dir, "bodies", "manifest.json"), []byte("[]"), 0o600)
	cmd := exec.Command("node", "../../../../scripts/ctaeval.mjs", dir)
	cmd.Env = append(os.Environ(), "CHROMIUM=/nonexistent/chromium")
	out, err := cmd.CombinedOutput()
	want := `{"summary":{"messages":0,"declared":0,"picked":0,"none":0,"errors":0,"blockedRequests":0}}` + "\n"
	if err != nil || string(out) != want {
		t.Errorf("%q (%v), want %q", out, err, want)
	}
}

func TestSenderDomain(t *testing.T) {
	for in, want := range map[string]string{
		"Robin Hale <robin@Hale.Example>":   "hale.example",
		"robin@hale.example":                "hale.example",
		`"a@b" <x@y.example>`:               "y.example",
		"Broken <x@y.example":               "y.example",
		"=?utf-8?q?Caf=C3=A9?= <c@cafe.ex>": "cafe.ex",
		"no address":                        "",
		"":                                  "",
	} {
		if got := senderDomain(in); got != want {
			t.Errorf("senderDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// The script end to end on the fixture: one line per HTML body with only
// the promised fields, a summary, no subject or address in the output, and
// its temp dir gone afterwards.
func TestScript(t *testing.T) {
	if testing.Short() {
		t.Skip("launches headless chromium")
	}
	for _, bin := range []string{"chromium", "node", "go"} {
		if _, err := exec.LookPath(bin); err != nil && (bin != "chromium" || os.Getenv("CHROMIUM") == "") {
			t.Skip(bin + " not installed")
		}
	}
	env := testmail.Setup(t)
	ta := env.Account(t, "personal")
	cfg := map[string]any{"accounts": []map[string]string{{
		"name": ta.Name, "email": ta.Email, "notmuchConfig": ta.NotmuchConfig, "gmiDir": filepath.Join(ta.Root, "gmail"),
	}}}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	cmd := exec.Command("../../../../scripts/ctaeval", "personal", "50")
	cmd.Env = append(os.Environ(), "CTAEVAL_CONFIG="+cfgPath, "TMPDIR="+tmp)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ctaeval: %v\n%s", err, stderr.String())
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "ctaeval.*")); len(left) > 0 {
		t.Errorf("temp dir left behind: %v", left)
	}

	acct := account(ta)
	ids, err := acct.MessageIDs(context.Background(), Query)
	if err != nil {
		t.Fatal(err)
	}
	var secrets []string // what must never be printed: subjects and addresses
	html := 0
	for _, id := range ids {
		m, _ := acct.Message(context.Background(), id)
		if _, ok := web.HTMLBody(&m); ok {
			html++
			secrets = append(secrets, m.Headers["Subject"], id)
			if i := strings.Index(m.Headers["From"], "<"); i >= 0 {
				secrets = append(secrets, strings.Trim(m.Headers["From"][i:], "<>"))
			}
		}
	}

	fields := []string{"candidates", "declared", "domain", "pick", "runnerUp", "score", "top"}
	lines, picked := 0, 0
	var summary map[string]int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var v map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("not JSON: %s", sc.Bytes())
		}
		if s, ok := v["summary"]; ok {
			if err := json.Unmarshal(s, &summary); err != nil {
				t.Fatal(err)
			}
			continue
		}
		lines++
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, fields) {
			t.Errorf("fields %v in %s", keys, sc.Bytes())
		}
		if string(v["pick"]) != "null" {
			picked++
			var p map[string]any
			json.Unmarshal(v["pick"], &p)
			if len(p) != 2 || p["text"] == nil || p["host"] == nil {
				t.Errorf("pick %s", v["pick"])
			}
		}
	}
	if lines != html {
		t.Errorf("%d lines for %d HTML bodies", lines, html)
	}
	if summary == nil || summary["messages"] != html || summary["declared"]+summary["picked"]+summary["none"] != html || summary["errors"] != 0 {
		t.Errorf("summary %v", summary)
	}
	if picked == 0 {
		t.Error("the fixture's newsletter button was not picked")
	}
	for _, s := range secrets {
		if s != "" && (bytes.Contains(out, []byte(s)) || bytes.Contains(stderr.Bytes(), []byte(s))) {
			t.Errorf("output contains %q", s)
		}
	}
}

// A SIGTERM mid-run reaches every step: the browser's whole process group
// is gone and the temp dir removed when the script exits.
func TestScriptSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("launches headless chromium")
	}
	for _, bin := range []string{"chromium", "node", "go", "pgrep"} {
		if _, err := exec.LookPath(bin); err != nil && (bin != "chromium" || os.Getenv("CHROMIUM") == "") {
			t.Skip(bin + " not installed")
		}
	}
	env := testmail.Setup(t)
	ta := env.Account(t, "personal")
	b, _ := json.Marshal(map[string]any{"accounts": []map[string]string{{
		"name": ta.Name, "email": ta.Email, "notmuchConfig": ta.NotmuchConfig, "gmiDir": filepath.Join(ta.Root, "gmail"),
	}}})
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfgPath, b, 0o600)
	tmp := t.TempDir()
	cmd := exec.Command("../../../../scripts/ctaeval", "personal", "50")
	cmd.Env = append(os.Environ(), "CTAEVAL_CONFIG="+cfgPath, "TMPDIR="+tmp)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait for Chromium: its profile appears in the temp dir.
	var dir string
	deadline := time.Now().Add(60 * time.Second)
	for dir == "" && time.Now().Before(deadline) {
		if m, _ := filepath.Glob(filepath.Join(tmp, "ctaeval.*", "profile")); len(m) > 0 {
			dir = filepath.Dir(m[0])
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if dir == "" {
		cmd.Process.Kill()
		t.Fatal("chromium never started")
	}
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
		t.Errorf("exit: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir left behind: %v", err)
	}
	if out, _ := exec.Command("pgrep", "-f", dir).Output(); len(out) > 0 {
		t.Errorf("processes left: %s", out)
	}
}

// stubNotmuch puts a notmuch on PATH that says it started (a file in dir)
// and then sleeps as `sleep <marker>`: in place of itself (exec), or as a
// child of the shell. It returns the PATH and the marker.
func stubNotmuch(t *testing.T, execSleep bool) (string, string, string) {
	t.Helper()
	bin := t.TempDir()
	marker := fmt.Sprintf("%d", 100000+time.Now().UnixNano()%800000)
	started := filepath.Join(bin, "started")
	body := "#!/bin/sh\ntouch " + started + "\n"
	if execSleep {
		body += "exec sleep " + marker + "\n"
	} else {
		body += "sleep " + marker + " &\nwait\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "notmuch"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin + string(os.PathListSeparator) + os.Getenv("PATH"), marker, started
}

func waitFile(t *testing.T, p string) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(p); err == nil {
			return
		}
	}
	t.Fatalf("%s never appeared", p)
}

// survivors are the processes still sleeping as marker, after a moment
// for the killed ones to be reaped.
func survivors(marker string) string {
	var out []byte
	for i := 0; i < 20; i++ {
		out, _ = exec.Command("pgrep", "-f", "sleep "+marker+"$").Output()
		if len(out) == 0 {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
	exec.Command("pkill", "-f", "sleep "+marker+"$").Run()
	return string(out)
}

// waitExit waits for cmd to exit, at most d; past that it kills it and
// reports it hung.
func waitExit(t *testing.T, cmd *exec.Cmd, d time.Duration) error {
	t.Helper()
	cmd.WaitDelay = time.Second
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		cmd.Process.Kill()
		t.Errorf("still running %v after the signal", d)
		return <-done
	}
}

func stubConfig(t *testing.T) string {
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"accounts":[{"name":"p","email":"p@x.example","notmuchConfig":"/nonexistent/notmuch-config","gmiDir":"/nonexistent"}]}`), 0o600)
	return cfg
}

// A SIGTERM to the extractor alone cancels the notmuch run it is waiting
// on, which is killed and reaped: nothing outlives it.
func TestExtractorSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not installed")
	}
	bin := filepath.Join(t.TempDir(), "ctaeval")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	path, marker, started := stubNotmuch(t, true)
	cmd := exec.Command(bin, "-config", stubConfig(t), "-account", "p", "-out", filepath.Join(t.TempDir(), "o"))
	cmd.Env = append(os.Environ(), "PATH="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, started)
	time.Sleep(100 * time.Millisecond) // the stub is now sleep
	cmd.Process.Signal(syscall.SIGTERM)
	err := waitExit(t, cmd, 5*time.Second)
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 || stderr.String() != "ctaeval: interrupted\n" {
		t.Errorf("exit %v, stderr %q", err, stderr.String())
	}
	if s := survivors(marker); s != "" {
		t.Errorf("notmuch outlived the extractor: %s", s)
	}
}

// A SIGTERM to the script during extraction reaches the whole step:
// notmuch and anything it started are gone, and so is the temp dir.
func TestScriptSignalDuringExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	for _, b := range []string{"go", "pgrep"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b + " not installed")
		}
	}
	path, marker, started := stubNotmuch(t, false)
	tmp := t.TempDir()
	cmd := exec.Command("../../../../scripts/ctaeval", "p", "5")
	cmd.Env = append(os.Environ(), "PATH="+path, "CTAEVAL_CONFIG="+stubConfig(t), "TMPDIR="+tmp)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, started)
	time.Sleep(100 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	err := waitExit(t, cmd, 5*time.Second)
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
		t.Errorf("exit: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "ctaeval.*")); len(left) > 0 {
		t.Errorf("temp dir left behind: %v", left)
	}
	if s := survivors(marker); s != "" {
		t.Errorf("processes outlived the script: %s", s)
	}
}
