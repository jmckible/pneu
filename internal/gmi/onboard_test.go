package gmi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests run the engine against stubgmi, the lieer simulator
// (internal/testmail/cmd/stubgmi), over a real notmuch database in a temp dir.

var (
	stubOnce sync.Once
	stubPath string
	stubErr  error
)

func stubgmi(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("notmuch"); err != nil {
		t.Skip("notmuch not on PATH")
	}
	stubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gmi-stub")
		if err != nil {
			stubErr = err
			return
		}
		stubPath = filepath.Join(dir, "gmi")
		if out, err := exec.Command("go", "build", "-o", stubPath, "../testmail/cmd/stubgmi").CombinedOutput(); err != nil {
			stubErr = errors.New(string(out))
		}
	})
	if stubErr != nil {
		t.Fatal(stubErr)
	}
	return stubPath
}

// stubAccount lays out an account as `pneu account add` would and runs
// stubgmi's init and auth in it. The first pull is left to the engine.
func stubAccount(t *testing.T, name string) Account {
	t.Helper()
	bin := stubgmi(t)
	root := t.TempDir()
	a := Account{Name: name, GmiDir: filepath.Join(root, "mail", name, "gmail"),
		NotmuchConfig: filepath.Join(root, "config", name, "notmuch-config")}
	for _, d := range []string{a.GmiDir, filepath.Dir(a.NotmuchConfig)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "[database]\npath=" + filepath.Join(root, "mail", name) + "\n[user]\nprimary_email=" + name + "@example.com\n" +
		"[new]\ntags=\nignore=.gmailieer.json;.state.gmailieer.json;.credentials.gmailieer.json;.resume-pull.gmailieer.json;.lock;/.*[.](json|lock|bak)$/\n" +
		"[search]\nexclude_tags=spam;trash\n[maildir]\nsynchronize_flags=false\n"
	must(t, os.WriteFile(a.NotmuchConfig, []byte(cfg), 0o644))
	must(t, os.WriteFile(filepath.Join(filepath.Dir(a.NotmuchConfig), "client_secret.json"),
		[]byte(testClient), 0o600))
	t.Setenv("NOTMUCH_CONFIG", a.NotmuchConfig)
	runIn(t, "", "notmuch", "new", "--quiet")
	runIn(t, a.GmiDir, bin, "init", "--no-auth", "--replace-slash-with-dot", name+"@example.com")
	runIn(t, a.GmiDir, bin, "auth", "-c", filepath.Join(filepath.Dir(a.NotmuchConfig), "client_secret.json"))
	runIn(t, a.GmiDir, bin, "set", "--ignore-tags-local", TouchTag)
	return a
}

const testClient = `{"installed":{"client_id":"1-t.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func runIn(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// recorder collects what the engine reports.
type recorder struct {
	mu       sync.Mutex
	results  []Result
	progress []Progress
	auths    []error
	logs     []string
}

func (r *recorder) opts(bin string, o Options) Options {
	o.GmiPath = bin
	o.Interval = time.Hour // only what the test asks for, and first pulls, run
	o.OnSynced = func(_ string, res Result) { r.mu.Lock(); r.results = append(r.results, res); r.mu.Unlock() }
	o.OnProgress = func(_ string, p Progress) { r.mu.Lock(); r.progress = append(r.progress, p); r.mu.Unlock() }
	o.OnAuth = func(_ string, err error) { r.mu.Lock(); r.auths = append(r.auths, err); r.mu.Unlock() }
	o.Logf = func(f string, a ...any) {
		r.mu.Lock()
		r.logs = append(r.logs, strings.TrimSpace(fmt.Sprintf(f, a...)))
		r.mu.Unlock()
	}
	return o
}

func (r *recorder) ops() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Result(nil), r.results...)
}

func (r *recorder) last(op Op) (Result, bool) {
	rs := r.ops()
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Op == op {
			return rs[i], true
		}
	}
	return Result{}, false
}

func (r *recorder) count(op Op) int {
	n := 0
	for _, res := range r.ops() {
		if res.Op == op {
			n++
		}
	}
	return n
}

func (r *recorder) logged(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func withFrontierEvery(t *testing.T, d time.Duration) {
	old := frontierEvery
	frontierEvery = d
	t.Cleanup(func() { frontierEvery = old })
}

// The first pull starts by itself, reports progress with the exact total
// and a frontier, flips the account to ready, and a sync follows at once.
func TestFirstPullAutomatic(t *testing.T) {
	a := stubAccount(t, "personal")
	withFrontierEvery(t, 100*time.Millisecond)
	t.Setenv("STUBGMI_COUNT", "300")
	t.Setenv("STUBGMI_DURATION", "3s")
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	if st, _ := e.Status("personal"); st.State != StateNeedsPull {
		t.Fatalf("before Run: %+v", st)
	}
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()

	waitFor(t, 5*time.Second, "pulling state", func() bool {
		st, _ := e.Status("personal")
		return st.State == StatePulling && st.Progress != nil && st.Progress.Phase == PhaseContent
	})
	if st, _ := e.Status("personal"); st.Pulled || st.Progress.Total != 300 {
		t.Fatalf("while pulling: %+v %+v", st, st.Progress)
	}
	waitFor(t, 15*time.Second, "a sync after the first pull", func() bool { return rec.count(OpSync) > 0 })
	pull, _ := rec.last(OpPull)
	if pull.Err != nil || !pull.Changed {
		t.Fatalf("first pull: %+v", pull)
	}
	if st, _ := e.Status("personal"); st.State != StateReady || !st.Pulled || st.Progress != nil || st.LastErr != nil {
		t.Fatalf("after: %+v", st)
	}
	rec.mu.Lock()
	var sawFrontier, sawTotal bool
	for _, p := range rec.progress {
		sawFrontier = sawFrontier || !p.Frontier.IsZero()
		sawTotal = sawTotal || (p.Phase == PhaseContent && p.Total == 300 && p.Done > 0 && p.Done < 300)
	}
	rec.mu.Unlock()
	if !sawFrontier || !sawTotal {
		t.Errorf("progress reports: frontier %v, partial content %v", sawFrontier, sawTotal)
	}
}

// A pull killed between lieer's tmp write and its rename leaves a file in
// mail/tmp that would fail every later pull; the retry clears it first and
// resumes.
func TestFirstPullKillRecovers(t *testing.T) {
	a := stubAccount(t, "personal")
	t.Setenv("STUBGMI_COUNT", "200")
	t.Setenv("STUBGMI_FAIL", "kill")
	log := filepath.Join(t.TempDir(), "stub.log")
	t.Setenv("STUBGMI_LOG", log)
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()

	waitFor(t, 10*time.Second, "the killed pull", func() bool { return rec.count(OpPull) == 1 })
	if r, _ := rec.last(OpPull); r.Err == nil {
		t.Fatal("killed pull reported success")
	}
	tmp, _ := filepath.Glob(filepath.Join(a.GmiDir, "mail", "tmp", "*"))
	if len(tmp) != 1 {
		t.Fatalf("tmp after the kill: %v", tmp)
	}
	if st, _ := e.Status("personal"); st.State != StateNeedsPull || st.Failures != 1 {
		t.Fatalf("after the kill: %+v", st)
	}
	e.SyncNow("personal") // the app's Retry
	waitFor(t, 10*time.Second, "the retried pull", func() bool { return rec.count(OpPull) == 2 })
	if r, _ := rec.last(OpPull); r.Err != nil {
		t.Fatalf("retried pull: %v\n%s", r.Err, r.Output)
	}
	if !rec.logged("left in mail/tmp") {
		t.Error("the orphan's removal wasn't logged")
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "\tpull --resume\t") {
		t.Errorf("the retry didn't resume:\n%s", b)
	}
	if st, _ := e.Status("personal"); st.State != StateReady {
		t.Fatalf("after the retry: %+v", st)
	}
}

// A pull that goes silent is stopped by the watchdog and retried.
func TestFirstPullStallWatchdog(t *testing.T) {
	a := stubAccount(t, "personal")
	t.Setenv("STUBGMI_COUNT", "200")
	t.Setenv("STUBGMI_FAIL", "stall")
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{StallTimeout: time.Second}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()

	waitFor(t, 10*time.Second, "the stalled pull to be stopped", func() bool { return rec.count(OpPull) == 1 })
	if r, _ := rec.last(OpPull); !errors.Is(r.Err, ErrStalled) {
		t.Fatalf("stalled pull: %v", r.Err)
	}
	e.SyncNow("personal")
	waitFor(t, 10*time.Second, "the retried pull", func() bool { return rec.count(OpPull) == 2 })
	if r, _ := rec.last(OpPull); r.Err != nil {
		t.Fatalf("retried pull: %v", r.Err)
	}
}

// Shutdown interrupts a first pull (SIGINT) rather than waiting hours for
// it, and it isn't a failure; the next server resumes it.
func TestShutdownInterruptsFirstPull(t *testing.T) {
	a := stubAccount(t, "personal")
	t.Setenv("STUBGMI_COUNT", "500")
	t.Setenv("STUBGMI_DURATION", "20s")
	log := filepath.Join(t.TempDir(), "stub.log")
	t.Setenv("STUBGMI_LOG", log)
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	cancel, done := start(t, e)
	waitFor(t, 10*time.Second, "content phase", func() bool {
		st, _ := e.Status("personal")
		return st.Progress != nil && st.Progress.Phase == PhaseContent
	})
	t0 := time.Now()
	cancel()
	<-done
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	if n := rec.count(OpPull); n != 0 {
		t.Fatalf("an interrupted pull was reported as a run (%d)", n)
	}
	if st, _ := e.Status("personal"); st.Failures != 0 || st.State != StateNeedsPull {
		t.Fatalf("after shutdown: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(a.GmiDir, lieerResume)); err != nil {
		t.Fatal("no resume file after an interrupted pull")
	}
	b, _ := os.ReadFile(log)
	if strings.Count(string(b), "start\t") != strings.Count(string(b), "end\t") {
		t.Fatalf("gmi still running after shutdown:\n%s", b)
	}

	t.Setenv("STUBGMI_DURATION", "")
	var rec2 recorder
	e2 := mustNew(t, []Account{a}, rec2.opts(stubgmi(t), Options{}))
	cancel2, done2 := start(t, e2)
	defer func() { cancel2(); <-done2 }()
	waitFor(t, 10*time.Second, "the resumed pull", func() bool { return rec2.count(OpPull) == 1 })
	if r, _ := rec2.last(OpPull); r.Err != nil || !strings.Contains(r.Output, "pull: attempting to resume previous pull..") {
		t.Fatalf("resumed pull: %v\n%s", r.Err, r.Output)
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

func (r *recorder) authCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.auths)
}

// A dead token turns a working account to reauth; the in-app consent
// brings it back and it syncs again. One re-auth at a time.
func TestReauth(t *testing.T) {
	a := stubAccount(t, "personal")
	if err := CheckAuthPort(); err != nil {
		t.Skip(err)
	}
	t.Setenv("STUBGMI_FAIL", "token")
	t.Setenv("STUBGMI_AUTH", "browser")
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()

	waitFor(t, 10*time.Second, "the failed sync after the pull", func() bool { return rec.count(OpSync) == 1 })
	st, _ := e.Status("personal")
	if st.State != StateReauth || !errors.Is(st.LastErr, ErrReauth) {
		t.Fatalf("after invalid_grant: %+v", st)
	}

	// Abandoned: the credentials are gone (auth -f) but it's still a re-auth,
	// and the account, pulled, isn't read-only for it.
	u, err := e.Reauth(context.Background(), "personal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://accounts.google.com/o/oauth2/auth?") {
		t.Fatalf("consent URL %q", u)
	}
	if st, _ := e.Status("personal"); !st.Authing {
		t.Fatal("not authing while the consent screen waits")
	}
	if _, err := e.Reauth(context.Background(), "personal"); !errors.Is(err, ErrAuthing) {
		t.Fatalf("second re-auth while one waits: %v", err)
	}
	e.CancelReauth("personal")
	waitFor(t, 10*time.Second, "the cancelled re-auth", func() bool { return rec.authCount() == 1 })
	if st, _ := e.Status("personal"); st.State != StateReauth || st.Authing || !st.Pulled {
		t.Fatalf("after cancel: %+v", st)
	}

	u, err = e.Reauth(context.Background(), "personal")
	if err != nil {
		t.Fatal(err)
	}
	grant(t, u)
	waitFor(t, 10*time.Second, "the re-auth", func() bool { return rec.authCount() == 2 })
	rec.mu.Lock()
	authErr := rec.auths[1]
	rec.mu.Unlock()
	if authErr != nil {
		t.Fatal(authErr)
	}
	waitFor(t, 10*time.Second, "a good sync", func() bool {
		r, _ := rec.last(OpSync)
		return rec.count(OpSync) >= 2 && r.Err == nil
	})
	if st, _ := e.Status("personal"); st.State != StateReady || st.LastErr != nil {
		t.Fatalf("after re-auth: %+v", st)
	}
	if fi, err := os.Stat(filepath.Join(a.GmiDir, lieerCredentials)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials after re-auth: %v %v", fi, err)
	}
	if _, err := e.Reauth(context.Background(), "personal"); !errors.Is(err, ErrNoReauth) {
		t.Fatalf("re-auth of a working account: %v", err)
	}
}

// Fixed in a terminal while the server still holds the failure: Reconnect
// finds the credentials working and leaves them alone.
func TestReauthAlreadyConnected(t *testing.T) {
	a := stubAccount(t, "personal")
	if err := CheckAuthPort(); err != nil {
		t.Skip(err)
	}
	t.Setenv("STUBGMI_FAIL", "token")
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()
	waitFor(t, 10*time.Second, "the failed sync", func() bool { return rec.count(OpSync) == 1 })

	secret := filepath.Join(filepath.Dir(a.NotmuchConfig), "client_secret.json")
	runIn(t, a.GmiDir, stubgmi(t), "auth", "-f", "-c", secret) // `pneu account auth --force`
	creds := filepath.Join(a.GmiDir, lieerCredentials)
	before, _ := os.ReadFile(creds)
	if st, _ := e.Status("personal"); st.State != StateReauth {
		t.Fatalf("the server should still hold the failure: %+v", st)
	}
	if _, err := e.Reauth(context.Background(), "personal"); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("re-auth of fixed credentials: %v", err)
	}
	if after, _ := os.ReadFile(creds); string(after) != string(before) {
		t.Fatal("working credentials were replaced")
	}
	if st, _ := e.Status("personal"); st.State != StateReady || st.LastErr != nil || st.Authing {
		t.Fatalf("after: %+v", st)
	}
}

// A client JSON changed on disk after `pneu account add` is checked again:
// a "web" block beside "installed" (which google_auth_oauthlib would use)
// stops the re-auth before anything is deleted.
func TestReauthRejectsBadClient(t *testing.T) {
	a := stubAccount(t, "personal")
	if err := CheckAuthPort(); err != nil {
		t.Skip(err)
	}
	t.Setenv("STUBGMI_FAIL", "token")
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()
	waitFor(t, 10*time.Second, "the failed sync", func() bool { return rec.count(OpSync) == 1 })
	bad := strings.TrimSuffix(testClient, "}") + `,"web":{"client_id":"1-t.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://evil.example/token"}}`
	must(t, os.WriteFile(filepath.Join(filepath.Dir(a.NotmuchConfig), "client_secret.json"), []byte(bad), 0o600))
	if _, err := e.Reauth(context.Background(), "personal"); err == nil || !strings.Contains(err.Error(), `unexpected top-level key "web"`) {
		t.Fatalf("re-auth with a doctored client: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.GmiDir, lieerCredentials)); err != nil {
		t.Fatal("credentials deleted")
	}
	if st, _ := e.Status("personal"); st.Authing {
		t.Fatal("left authing")
	}
}

// Run doesn't return while a re-auth's gmi still holds localhost:8080.
func TestRunWaitsForReauth(t *testing.T) {
	a := stubAccount(t, "personal")
	if err := CheckAuthPort(); err != nil {
		t.Skip(err)
	}
	t.Setenv("STUBGMI_FAIL", "token")
	t.Setenv("STUBGMI_AUTH", "browser")
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	cancel, done := start(t, e)
	waitFor(t, 10*time.Second, "the failed sync", func() bool { return rec.count(OpSync) == 1 })
	if _, err := e.Reauth(context.Background(), "personal"); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	if err := CheckAuthPort(); err != nil {
		t.Fatalf("after Run returned: %v", err)
	}
	if rec.authCount() != 1 {
		t.Fatal("the interrupted re-auth wasn't reported")
	}
}

// A batch that streams in silently for longer than the stall timeout is
// alive: it's reading. Only silence without reads is a stall.
func TestFirstPullSilentButReading(t *testing.T) {
	a := stubAccount(t, "personal")
	t.Setenv("STUBGMI_COUNT", "100")
	t.Setenv("STUBGMI_SLOW_BATCH", "3s")
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{StallTimeout: time.Second}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()
	waitFor(t, 15*time.Second, "the pull", func() bool { return rec.count(OpPull) == 1 })
	if r, _ := rec.last(OpPull); r.Err != nil {
		t.Fatalf("a reading pull was stopped: %v", r.Err)
	}
}

// Waiting for the flock (a manual pull, a long consent) isn't pulling, and
// isn't counted against the stall timeout.
func TestFirstPullWaitsForLock(t *testing.T) {
	a := stubAccount(t, "personal")
	t.Setenv("STUBGMI_COUNT", "100")
	release, err := Lock(DefaultLockPath(a.GmiDir), time.Second, nil)
	must(t, err)
	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{StallTimeout: time.Second}))
	cancel, done := start(t, e)
	defer func() { cancel(); <-done }()
	time.Sleep(2500 * time.Millisecond)
	if st, _ := e.Status("personal"); st.State != StateNeedsPull || st.Progress != nil {
		t.Fatalf("waiting on the lock: %+v", st)
	}
	release()
	waitFor(t, 10*time.Second, "the pull", func() bool { return rec.count(OpPull) == 1 })
	if r, _ := rec.last(OpPull); r.Err != nil {
		t.Fatalf("pull after the wait: %v", r.Err)
	}
}

// A bare `gmi` (no flock) holding lieer's own lock keeps mail/tmp safe.
func TestCleanTmpRespectsLieerLock(t *testing.T) {
	a := stubAccount(t, "personal")
	orphan := filepath.Join(a.GmiDir, "mail", "tmp", "0123456789abcdef:2,S")
	must(t, os.WriteFile(orphan, []byte("x"), 0o644))
	// A stalled bare pull holds .lock (lockf) until SIGINT.
	cmd := exec.Command(stubgmi(t), "pull")
	cmd.Dir = a.GmiDir
	cmd.Env = append(os.Environ(), "STUBGMI_FAIL=stall", "STUBGMI_FAIL_REPEAT=1", "STUBGMI_COUNT=100")
	must(t, cmd.Start())
	defer func() { cmd.Process.Signal(syscall.SIGINT); cmd.Wait() }()
	held := func() bool {
		unlock, ok := lockLieer(a.GmiDir)
		if ok {
			unlock()
		}
		return !ok
	}
	waitFor(t, 10*time.Second, "the bare gmi to lock", held)

	var rec recorder
	e := mustNew(t, []Account{a}, rec.opts(stubgmi(t), Options{}))
	e.cleanTmp(e.accts["personal"])
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("cleaned mail/tmp under a running gmi")
	}
	if !rec.logged("mail/tmp left alone") {
		t.Error("skip not logged")
	}
	cmd.Process.Signal(syscall.SIGINT)
	cmd.Wait()
	e.cleanTmp(e.accts["personal"])
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("orphan kept with no gmi running")
	}
	// The cleanup's lock is gone before gmi runs: lieer takes it without waiting.
	runIn(t, a.GmiDir, stubgmi(t), "set")
}

// groupRead sees reads by every process in the group, children included.
func TestGroupRead(t *testing.T) {
	cmd := exec.Command("sh", "-c", "head -c 5000000 /dev/zero >/dev/null; sleep 5")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	must(t, cmd.Start())
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()
	waitFor(t, 5*time.Second, "5MB read in the group", func() bool {
		n, ok := groupRead(cmd.Process.Pid)
		return ok && n >= 5000000
	})
	if _, ok := groupRead(1 << 30); ok {
		t.Error("found an empty group")
	}
}
