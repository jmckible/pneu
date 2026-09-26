package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	engine "github.com/jmckible/pneu/internal/gmi"
)

// repo is one simulated account in a temp dir, laid out as INSTALL.md lays
// out a real one.
type repo struct {
	t       *testing.T
	bin     string
	dir     string // lieer dir
	nmcfg   string
	secret  string
	baseEnv []string
}

var stubBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stubgmi-test")
	if err != nil {
		panic(err)
	}
	stubBin = filepath.Join(dir, "gmi")
	if out, err := exec.Command("go", "build", "-o", stubBin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("notmuch"); err != nil {
		t.Skip("notmuch not on PATH")
	}
	base := t.TempDir()
	r := &repo{t: t, bin: stubBin, dir: filepath.Join(base, "mail", "acct", "gmail"),
		nmcfg: filepath.Join(base, "notmuch-config"), secret: filepath.Join(base, "client_secret.json")}
	must(t, os.MkdirAll(r.dir, 0o755))
	must(t, os.WriteFile(r.nmcfg, []byte(`[database]
path=`+filepath.Join(base, "mail", "acct")+`
[user]
primary_email=a@example.com
[new]
tags=
ignore=.gmailieer.json;.state.gmailieer.json;.credentials.gmailieer.json;.resume-pull.gmailieer.json;.lock;/.*[.](json|lock|bak)$/
[search]
exclude_tags=spam;trash
[maildir]
synchronize_flags=false
`), 0o644))
	must(t, os.WriteFile(r.secret, []byte(`{"installed":{"client_id":"1-x.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth"}}`), 0o600))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "STUBGMI_") && !strings.HasPrefix(kv, "NOTMUCH_") && !strings.HasPrefix(kv, "BROWSER=") {
			r.baseEnv = append(r.baseEnv, kv)
		}
	}
	r.baseEnv = append(r.baseEnv, "NOTMUCH_CONFIG="+r.nmcfg)
	if out, err := r.notmuch("new", "--quiet"); err != nil {
		t.Fatalf("notmuch new: %v\n%s", err, out)
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (r *repo) cmd(env []string, args ...string) *exec.Cmd {
	c := exec.Command(r.bin, args...)
	c.Dir = r.dir
	c.Env = append(append([]string{}, r.baseEnv...), env...)
	return c
}

// gmi runs the stub and returns its combined output and exit code.
func (r *repo) gmi(env []string, args ...string) (string, int) {
	r.t.Helper()
	out, err := r.cmd(env, args...).CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return string(out), 0
}

func (r *repo) ok(env []string, args ...string) string {
	r.t.Helper()
	out, code := r.gmi(env, args...)
	if code != 0 {
		r.t.Fatalf("gmi %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func (r *repo) notmuch(args ...string) (string, error) {
	c := exec.Command("notmuch", args...)
	c.Env = r.baseEnv
	out, err := c.CombinedOutput()
	return string(out), err
}

func (r *repo) setup() {
	r.t.Helper()
	r.ok(nil, "init", "--no-auth", "--replace-slash-with-dot", "a@example.com")
	r.ok(nil, "auth", "-c", r.secret)
	r.ok(nil, "set", "--ignore-tags-local", "pneu-touch")
}

func (r *repo) state() (hid, lastmod int64) {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0 // lieer writes it only once a pull or push sets it
	}
	must(r.t, err)
	var s state
	must(r.t, json.Unmarshal(b, &s))
	return s.LastHistoryID, s.Lastmod
}

// The files lieer writes, in json.dump's format and lieer's modes.
func TestInitAuthSet(t *testing.T) {
	r := newRepo(t)
	out := r.ok(nil, "init", "--no-auth", "--replace-slash-with-dot", "a@example.com")
	if want := "initializing repository in: " + r.dir + "..\n"; out != want {
		t.Errorf("init printed %q, want %q", out, want)
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, configFile))
	if want := `{"replace_slash_with_dot": true, "account": "a@example.com", "timeout": 600, "drop_non_existing_label": false, "ignore_empty_history": false, "ignore_tags": [], "ignore_remote_labels": ["CATEGORY_PERSONAL", "CATEGORY_SOCIAL", "CATEGORY_PROMOTIONS", "CATEGORY_UPDATES", "CATEGORY_FORUMS"], "remove_local_messages": true, "file_extension": "", "local_trash_tag": "trash", "translation_list_overlay": []}`; string(b) != want {
		t.Errorf(".gmailieer.json:\n%s\nwant\n%s", b, want)
	}
	if fi, _ := os.Stat(filepath.Join(r.dir, configFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf(".gmailieer.json mode %v, want 0600 (NamedTemporaryFile)", fi.Mode().Perm())
	}
	if out, code := r.gmi(nil, "init", "--no-auth", "a@example.com"); code != 1 || !strings.HasSuffix(out, "lieer.local.Local.RepositoryException: '.gmailieer.json' exists: this repository seems to already be set up!\n") {
		t.Errorf("second init: exit %d\n%s", code, out)
	}

	out = r.ok(nil, "auth", "-c", r.secret)
	if !regexp.MustCompile(`(?m)^authorizing\.\.\nauth: using user-provided api id and secret\nPlease visit this URL to authorize this application: https://accounts\.google\.com/o/oauth2/auth\?\S*client_id=1-x\.apps`).MatchString(out) {
		t.Errorf("auth printed:\n%s", out)
	}
	if out := r.ok(nil, "auth"); out != "authorizing..\n" {
		t.Errorf("auth with credentials printed %q", out)
	}

	out = r.ok(nil, "set", "--ignore-tags-local", "pneu-touch")
	if !strings.Contains(out, "Ignore tags (local) .......: {'pneu-touch'}\n") || !strings.HasPrefix(out, "Repository information and settings:\nAccount ...........: a@example.com\nhistoryId .........: 0\n") {
		t.Errorf("set printed:\n%s", out)
	}
	if out := r.ok(nil, "set"); !strings.Contains(out, "{'pneu-touch'}") {
		t.Errorf("bare set lost the setting:\n%s", out)
	}
}

// A command that needs credentials and has none doesn't get them for free.
func TestNoCredentials(t *testing.T) {
	r := newRepo(t)
	r.ok(nil, "init", "--no-auth", "a@example.com")
	out, code := r.gmi(nil, "pull")
	if code != 1 || !strings.Contains(out, "Please visit this URL") {
		t.Errorf("pull without auth: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(r.dir, credentialsFile)); err == nil {
		t.Error("pull without auth wrote credentials")
	}
}

// lieer's non-TTY output for a first pull, newest first, and the state the
// engine's pulled() reads.
func TestFirstPull(t *testing.T) {
	r := newRepo(t)
	r.setup()
	env := []string{"STUBGMI_COUNT=400"}
	out := r.ok(env, "pull")
	want := regexp.MustCompile(`^pull: full synchronization \(no previous synchronization state\)
fetching messages \(1\) \.\.\.\.\.\.\.done: 400 its in \d\d\.\d{3}s
removing deleted \(0\) \.\.\.done: 0 its in \d\d\.\d{3}s
receiving content \(400\) \.\.\.[.\n]*remote: reducing batch request size to: 25
[.]*remote: increasing batch request size to: 50
[.]*done: 400 its in \d\d\.\d{3}s
receiving metadata: everything up-to-date\.
pull: complete, removing resume file
current historyId: 7000400, current revision: \d+
$`)
	if !want.MatchString(out) {
		t.Errorf("first pull printed:\n%s", out)
	}
	content := out[strings.Index(out, "receiving content"):]
	if n := strings.Count(content[:strings.Index(content, "done:")], "."); n != 3+40 {
		t.Errorf("content phase printed %d dots, want 3 + 40 (one per 10)", n)
	}
	if hid, _ := r.state(); hid != 7000400 {
		t.Errorf("last_historyId = %d", hid)
	}
	if _, err := os.Stat(filepath.Join(r.dir, resumeFile)); err == nil {
		t.Error("resume file left after a complete pull")
	}
	if n, _ := r.notmuch("count", "--exclude=false", "*"); strings.TrimSpace(n) != "400" {
		t.Errorf("indexed %s messages", n)
	}
	files, _ := filepath.Glob(filepath.Join(r.dir, "mail", "cur", "*"))
	if len(files) != 400 {
		t.Fatalf("%d files", len(files))
	}
	// Tags from the manifest, plus index-time ones.
	if n, _ := r.notmuch("count", "tag:inbox and tag:unread"); strings.TrimSpace(n) == "0" {
		t.Error("no inbox/unread mail after the pull")
	}
	if out := r.ok(env, "pull"); out != "pull: partial synchronization.. (hid: 7000400)\npull: everything is up-to-date.\ncurrent historyId: 7000400\n" {
		t.Errorf("second pull printed:\n%s", out)
	}
	if out := r.ok(env, "pull", "-t"); !regexp.MustCompile(`(?m)^INBOX {26}INBOX$`).MatchString(out) {
		t.Errorf("pull -t printed:\n%s", out)
	}
}

// Tag writes show up as a push; a full pull reverts unpushed local tags.
func TestPushAndRevert(t *testing.T) {
	r := newRepo(t)
	r.setup()
	r.ok(nil, "pull")
	if out := r.ok(nil, "push"); out != "push: everything is up-to-date.\n" {
		t.Errorf("idle push printed %q", out)
	}
	if out, err := r.notmuch("tag", "-inbox", "--", "tag:inbox"); err != nil {
		t.Fatal(out)
	}
	out := r.ok(nil, "sync")
	if !strings.Contains(out, "pushing, 0 changed (") || !strings.Contains(out, "pull: everything is up-to-date.") {
		t.Errorf("sync after archiving printed:\n%s", out)
	}
	if out := r.ok(nil, "pull", "-f"); !strings.Contains(out, "receiving metadata (") {
		t.Errorf("forced pull printed:\n%s", out)
	}
	if n, _ := r.notmuch("count", "tag:inbox"); strings.TrimSpace(n) == "0" {
		t.Error("forced full pull didn't put the remote's inbox tags back")
	}
}

// A kill mid-content leaves a file in mail/tmp and a resume file; the next
// pull resumes without re-downloading, until it reaches the orphan, where
// real lieer fails for good.
func TestKillMidContent(t *testing.T) {
	r := newRepo(t)
	r.setup()
	env := []string{"STUBGMI_COUNT=200", "STUBGMI_FAIL=kill", "STUBGMI_FAIL_AT=0.5"}
	c := r.cmd(env, "pull")
	err := c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("killed pull: %v", err)
	}
	tmp, _ := filepath.Glob(filepath.Join(r.dir, "mail", "tmp", "*"))
	cur, _ := filepath.Glob(filepath.Join(r.dir, "mail", "cur", "*"))
	if len(tmp) != 1 || len(cur) != 100 {
		t.Fatalf("after kill: tmp %d, cur %d", len(tmp), len(cur))
	}
	// Newest first: everything stored is newer than the message in hand.
	// lieer sets each file's mtime to Gmail's internalDate.
	orphan, _ := os.Stat(tmp[0])
	for _, f := range cur {
		if fi, _ := os.Stat(f); fi.ModTime().Before(orphan.ModTime()) {
			t.Fatalf("%s stored before a newer message", filepath.Base(f))
		}
	}
	if _, err := os.Stat(filepath.Join(r.dir, resumeFile)); err != nil {
		t.Error("no resume file after a killed pull")
	}
	if hid, _ := r.state(); hid != 0 {
		t.Error("killed pull recorded a historyId")
	}
	out, code := r.gmi(env, "pull", "--resume")
	if code != 1 || !strings.Contains(out, "pull: attempting to resume previous pull..") ||
		!strings.Contains(out, "lieer.local.Local.RepositoryException: local temporary file already exists: "+tmp[0]) {
		t.Errorf("pull after kill: exit %d\n%s", code, out)
	}
	os.Remove(tmp[0])
	out = r.ok(env, "pull", "--resume")
	if !strings.Contains(out, "receiving content (100) ") || !strings.Contains(out, "receiving metadata (100) ") ||
		!strings.Contains(out, "pull: resume: performing partial pull to complete") {
		t.Errorf("resumed pull printed:\n%s", out)
	}
}

// A stall prints nothing more and never exits; SIGINT ends it the way
// Python's KeyboardInterrupt does.
func TestStall(t *testing.T) {
	r := newRepo(t)
	r.setup()
	c := r.cmd([]string{"STUBGMI_COUNT=200", "STUBGMI_FAIL=stall"}, "pull")
	out := &syncBuf{}
	c.Stdout, c.Stderr = out, out
	must(t, c.Start())
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "receiving content (200) ") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	before := out.String()
	time.Sleep(500 * time.Millisecond)
	if out.String() != before {
		t.Error("a stalled pull kept printing")
	}
	if c.ProcessState != nil {
		t.Fatal("a stalled pull exited")
	}
	c.Process.Signal(syscall.SIGINT)
	err := c.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT || !strings.HasSuffix(out.String(), "KeyboardInterrupt\n") {
		t.Errorf("SIGINT: %v\n%s", err, out.String())
	}
	// Fired once: the next pull completes.
	r.ok([]string{"STUBGMI_COUNT=200", "STUBGMI_FAIL=stall"}, "pull")
}

// A revoked refresh token fails every run with lieer's traceback until
// `auth -f` gets a new one.
func TestTokenRevoked(t *testing.T) {
	r := newRepo(t)
	r.setup()
	env := []string{"STUBGMI_FAIL=token"}
	r.ok(env, "pull") // not yet pulled: the token dies once the account is live
	for range 2 {
		out, code := r.gmi(env, "sync")
		if code != 1 || !strings.HasSuffix(out, "google.auth.exceptions.RefreshError: ('invalid_grant: Token has been expired or revoked.', {'error': 'invalid_grant', 'error_description': 'Token has been expired or revoked.'})\n") {
			t.Fatalf("sync with a revoked token: exit %d\n%s", code, out)
		}
	}
	out := r.ok(env, "auth", "-f", "-c", r.secret)
	if !strings.Contains(out, "reauthorizing..\n") {
		t.Errorf("auth -f printed:\n%s", out)
	}
	r.ok(env, "sync")
}

// STUBGMI_AUTH=browser: lieer's run_local_server on localhost:8080.
func TestBrowserAuth(t *testing.T) {
	r := newRepo(t)
	r.ok(nil, "init", "--no-auth", "a@example.com")
	// No browser at all: webbrowser.Error before any URL is printed.
	out, code := r.gmi([]string{"STUBGMI_AUTH=browser", "DISPLAY=", "WAYLAND_DISPLAY="}, "auth", "-c", r.secret)
	if code != 1 || strings.Contains(out, "Please visit") || !strings.HasSuffix(out, "webbrowser.Error: could not locate runnable browser\n") {
		t.Fatalf("auth with no browser: exit %d\n%s", code, out)
	}
	c := r.cmd([]string{"STUBGMI_AUTH=browser", "BROWSER=true"}, "auth", "-c", r.secret)
	buf := &syncBuf{}
	c.Stdout, c.Stderr = buf, buf
	must(t, c.Start())
	re := regexp.MustCompile(`Please visit this URL to authorize this application: (\S+)\n`)
	var u string
	for deadline := time.Now().Add(10 * time.Second); u == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(buf.String()); m != nil {
			u = m[1]
		}
	}
	if u == "" {
		c.Process.Kill()
		t.Skipf("no consent URL (is localhost:8080 taken?):\n%s", buf.String())
	}
	if !strings.HasPrefix(u, "https://accounts.google.com/o/oauth2/auth?") {
		t.Fatalf("consent URL %q", u)
	}
	grant(t, u)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { <-ctx.Done(); c.Process.Kill() }()
	if err := c.Wait(); err != nil {
		t.Fatalf("browser auth: %v\n%s", err, buf.String())
	}
	if _, err := os.Stat(filepath.Join(r.dir, credentialsFile)); err != nil {
		t.Error("no credentials after consent")
	}
}

// pneu's own engine against the stub: the first pull done, it syncs.
func TestEngineSyncs(t *testing.T) {
	r := newRepo(t)
	r.setup()
	r.ok(nil, "pull")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan engine.Result, 1)
	e, err := engine.New([]engine.Account{{Name: "acct", GmiDir: r.dir, NotmuchConfig: r.nmcfg,
		LockPath: filepath.Join(t.TempDir(), "lock")}}, engine.Options{
		GmiPath:  r.bin,
		OnSynced: func(_ string, res engine.Result) { done <- res },
		Logf:     t.Logf,
	})
	must(t, err)
	go e.Run(ctx)
	defer cancel()
	select {
	case res := <-done:
		if res.Err != nil || res.Changed {
			t.Errorf("engine sync: err %v, changed %v\n%s", res.Err, res.Changed, res.Output)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("engine never synced")
	}
}

// send stores the sent copy and says so the way lieer does.
func TestSend(t *testing.T) {
	r := newRepo(t)
	r.setup()
	r.ok(nil, "pull")
	c := r.cmd(nil, "send", "-t")
	c.Stdin = strings.NewReader("From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\nMessage-ID: <sent-1@example.com>\r\n\r\nhello\r\n")
	out, err := c.CombinedOutput()
	if err != nil || !regexp.MustCompile(`message sent successfully: [0-9a-f]+\n$`).Match(out) {
		t.Fatalf("send: %v\n%s", err, out)
	}
	if n, _ := r.notmuch("count", "id:sent-1@example.com and tag:sent"); strings.TrimSpace(n) != "1" {
		t.Error("sent copy not stored with tag sent")
	}
}

// grant does what Google does once the person allows access: send the
// browser to the consent URL's redirect_uri with its state and a code.
func grant(t *testing.T, consent string) {
	t.Helper()
	cu, err := url.Parse(consent)
	must(t, err)
	q := cu.Query()
	resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"4/test-code"}}.Encode())
	must(t, err)
	resp.Body.Close()
}

type syncBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
