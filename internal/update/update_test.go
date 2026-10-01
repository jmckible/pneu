package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/web"
)

// The repo under update is a tiny real module whose cmd/pneu prints
// `pneu <vcs.revision>` for `version`, as pneu does: builds are real Go
// builds, and the live binary's build is read with debug/buildinfo.
const stubMain = `package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		rev, mod := "unknown", false
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					rev = s.Value
				}
				if s.Key == "vcs.modified" {
					mod = s.Value == "true"
				}
			}
		}
		s := "pneu " + rev
		if mod {
			s += " modified"
		}
		fmt.Println(s)
		return
	}
	os.Exit(2)
}
`

// goWrapper runs the real go (logging each run to FAKEGO_LOG), except: FAKEGO_FAIL fails the build;
// FAKEGO_BLOCK=<path> touches <path>.started, leaves a grandchild holding
// its inherited fds until <path>.release exists (what a compiler would be
// when its updater dies), and waits for it.
const goWrapper = `#!/bin/bash
echo "$*" >>"$FAKEGO_LOG"
if [ -n "$FAKEGO_FAIL" ]; then echo "fake go: build failed" >&2; exit 1; fi
if [ -n "$FAKEGO_BLOCK" ]; then
	touch "$FAKEGO_BLOCK.started"
	( while [ ! -e "$FAKEGO_BLOCK.release" ]; do sleep 0.05; done ) &
	wait
fi
exec "$REALGO" "$@"
`

// fakeSystemctl logs its argv, one line per call; FAKESYS_FAIL fails it.
const fakeSystemctl = `#!/bin/sh
echo "$*" >>"$FAKESYS_LOG"
if [ -n "$FAKESYS_FAIL" ]; then exit 1; fi
`

type rig struct {
	t                         *testing.T
	origin, dir, work, bin    string
	state, sock, log, scripts string
	port                      int
	env                       *Env
	out                       *bytes.Buffer

	// The fake daemon: behind the control socket it "runs" whatever the
	// live binary was at its last restart, as a new instance, listening;
	// a revision in broken never comes up (the socket keeps answering as
	// before). Its loopback HTTP answers as pneu with the running
	// instance, or (staleHTTP) as the instance from before the last
	// restart: an old process still holding the port.
	mu        sync.Mutex
	restarts  int
	running   control.Info
	previous  string
	broken    map[string]bool
	staleHTTP bool
	notListen bool
	badStatus bool
	// legacyRevs: builds that answer like a daemon from before the
	// instance check (status without listening, HTTP naming no instance);
	// httpNobody: whatever runs, its HTTP names no instance.
	legacyRevs map[string]bool
	httpNobody bool
	serverRev  string
	checked    int
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit adds a commit on the work clone and pushes it to origin.
func (r *rig) commit(msg string) string {
	r.t.Helper()
	os.WriteFile(filepath.Join(r.work, "file"), []byte(msg+"\n"), 0o644)
	gitIn(r.t, r.work, "add", "file")
	gitIn(r.t, r.work, "commit", "-q", "-m", msg)
	gitIn(r.t, r.work, "push", "-q", "origin", "main")
	return gitIn(r.t, r.work, "rev-parse", "HEAD")
}

// commitMain pushes a commit that changes cmd/pneu/main.go.
func (r *rig) commitMain(msg, src string) string {
	r.t.Helper()
	os.WriteFile(filepath.Join(r.work, "cmd", "pneu", "main.go"), []byte(src), 0o644)
	gitIn(r.t, r.work, "commit", "-qam", msg)
	gitIn(r.t, r.work, "push", "-q", "origin", "main")
	return gitIn(r.t, r.work, "rev-parse", "HEAD")
}

func gitTestEnv(t *testing.T) {
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(gcfg, []byte("[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "Test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	gitTestEnv(t)
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	t.Setenv("REALGO", realGo)
	t.Setenv("FAKEGO_FAIL", "")
	t.Setenv("FAKEGO_BLOCK", "")
	t.Setenv("FAKESYS_FAIL", "")
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOTOOLCHAIN", "local")

	base := t.TempDir()
	r := &rig{t: t, origin: filepath.Join(base, "origin.git"), dir: filepath.Join(base, "checkout"), work: filepath.Join(base, "work"),
		bin: filepath.Join(base, "bin"), state: filepath.Join(base, "state"), scripts: filepath.Join(base, "scripts"),
		log: filepath.Join(base, "systemctl.log"), out: &bytes.Buffer{}, broken: map[string]bool{}, legacyRevs: map[string]bool{}}
	t.Setenv("FAKESYS_LOG", r.log)
	t.Setenv("FAKEGO_LOG", filepath.Join(base, "go.log"))
	gitIn(t, base, "init", "-q", "--bare", r.origin)
	gitIn(t, base, "clone", "-q", r.origin, r.work)
	os.MkdirAll(filepath.Join(r.work, "cmd", "pneu"), 0o755)
	os.WriteFile(filepath.Join(r.work, "go.mod"), []byte("module example.com/pneustub\n\ngo 1.27\n"), 0o644)
	os.WriteFile(filepath.Join(r.work, "cmd", "pneu", "main.go"), []byte(stubMain), 0o644)
	gitIn(t, r.work, "add", ".")
	r.commit("first")
	gitIn(t, base, "clone", "-q", r.origin, r.dir)
	for _, d := range []string{r.bin, r.state, r.scripts} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(r.scripts, "go"), []byte(goWrapper), 0o755)
	os.WriteFile(filepath.Join(r.scripts, "systemctl"), []byte(fakeSystemctl), 0o755)
	// The installed build: the checkout's HEAD.
	build := exec.Command(realGo, "build", "-o", filepath.Join(r.bin, "pneu"), "./cmd/pneu")
	build.Dir = r.dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}

	sockBase, err := os.MkdirTemp("", "pneuup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockBase) })
	r.sock = filepath.Join(sockBase, "pneu", "control")
	r.running = control.Info{Revision: r.liveRev(), Instance: "0000000000000001", Listening: true}
	srv, err := control.Listen(r.sock, control.Handler{Client: true, Status: r.status, UpdateChecked: func() {
		r.mu.Lock()
		r.checked++
		r.mu.Unlock()
	}, Situation: func() control.Situation {
		r.mu.Lock()
		defer r.mu.Unlock()
		return control.Situation{Mode: control.ModeClient, Link: "up", ServerRevision: r.serverRev}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.port = ln.Addr().(*net.TCPAddr).Port
	hs := &http.Server{Handler: http.HandlerFunc(r.serveHTTP)}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })

	r.env = &Env{
		Source: config.Source{Dir: r.dir, Remote: "origin", URL: r.origin, Branch: "main"},
		Port:   r.port, Git: "git", Go: filepath.Join(r.scripts, "go"), Systemctl: filepath.Join(r.scripts, "systemctl"),
		BinDir: r.bin, StateDir: r.state, LockDir: filepath.Dir(r.sock), Socket: r.sock,
		Yes: true, Ready: 600 * time.Millisecond, Poll: 10 * time.Millisecond, Out: r.out, Err: r.out,
	}
	return r
}

// serveHTTP is the fake daemon's loopback: Auth's cookieless 403, as pneu
// answers it, naming the instance that holds the port.
func (r *rig) serveHTTP(w http.ResponseWriter, req *http.Request) {
	r.status()
	r.mu.Lock()
	id := r.running.Instance
	if r.staleHTTP {
		id = r.previous
	}
	named := !r.httpNobody && !r.legacyRevs[r.running.Revision]
	r.mu.Unlock()
	w.Header().Set("Pneu-Policy", "data")
	if named {
		w.Header().Set("Pneu-Instance", id)
	}
	w.WriteHeader(http.StatusForbidden)
}

// liveRev is the revision the live binary was built from.
func (r *rig) liveRev() string {
	b, err := Identify(filepath.Join(r.bin, "pneu"))
	if err != nil {
		return ""
	}
	return b.Revision
}

// status is the fake daemon's: a restart since the last answer starts
// the live binary's build as a new instance, unless that build is broken.
func (r *rig) status() control.Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.restartCount()
	if n != r.restarts {
		r.restarts = n
		if rev := r.liveRev(); !r.broken[rev] {
			r.previous = r.running.Instance
			r.running = control.Info{Revision: rev, Instance: fmt.Sprintf("%016x", 0x100+n), Listening: !r.notListen}
		}
	}
	if r.badStatus {
		return control.Info{Revision: r.running.Revision, Instance: "not-an-instance"}
	}
	in := r.running
	in.Legacy = r.legacyRevs[in.Revision]
	return in
}

func (r *rig) restartCount() int {
	b, _ := os.ReadFile(r.log)
	return strings.Count(string(b), "--user restart pneu.service\n")
}

func (r *rig) head() string { return gitIn(r.t, r.dir, "rev-parse", "HEAD") }

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func (r *rig) stagedLeft() []string {
	ms, _ := filepath.Glob(filepath.Join(r.bin, newPrefix+"*"))
	return ms
}

// untouched: nothing live changed since the snapshot.
func (r *rig) untouched(liveSum, head string) {
	r.t.Helper()
	if got, _ := fileSHA(filepath.Join(r.bin, "pneu")); got != liveSum {
		r.t.Error("the live binary changed")
	}
	if r.head() != head {
		r.t.Error("the checkout moved")
	}
	for _, f := range []string{filepath.Join(r.bin, prevName), filepath.Join(r.state, RecordFile)} {
		if exists(f) {
			r.t.Errorf("%s left behind", filepath.Base(f))
		}
	}
	if s := r.stagedLeft(); len(s) > 0 {
		r.t.Errorf("staged builds left: %v", s)
	}
	if r.restartCount() != 0 {
		r.t.Error("the service was restarted")
	}
	if wt := gitIn(r.t, r.dir, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		r.t.Errorf("a worktree left behind:\n%s", wt)
	}
}

func (r *rig) snapshot() (string, string) {
	s, err := fileSHA(filepath.Join(r.bin, "pneu"))
	if err != nil {
		r.t.Fatal(err)
	}
	return s, r.head()
}

func TestUpdateHappyPath(t *testing.T) {
	r := newRig(t)
	oldSum, _ := r.snapshot()
	target := r.commit("second")
	if err := r.env.Run(); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if testing.Verbose() {
		t.Log(r.out.String())
	}
	if r.liveRev() != target || r.head() != target {
		t.Fatalf("live %s head %s, want %s", r.liveRev(), r.head(), target)
	}
	if !sumIs(filepath.Join(r.bin, prevName), oldSum) {
		t.Error("pneu.prev isn't the old binary")
	}
	if exists(filepath.Join(r.state, RecordFile)) || len(r.stagedLeft()) > 0 {
		t.Error("left update.json or a staged build")
	}
	if r.restartCount() != 1 || r.status().Revision != target {
		t.Errorf("restarts %d, running %+v", r.restartCount(), r.status())
	}
	o := r.out.String()
	if !strings.Contains(o, "second") || !strings.Contains(o, "omarchy-restart-shell") || !strings.Contains(o, "Checkout: at ") {
		t.Errorf("output:\n%s", o)
	}
	// Again: nothing to do.
	r.out.Reset()
	if err := r.env.Run(); err != nil || !strings.Contains(r.out.String(), "Already up to date") || r.restartCount() != 1 {
		t.Fatalf("%v\n%s", err, r.out)
	}
}

func TestUpdateRefusals(t *testing.T) {
	t.Run("lock held", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		f, err := Lock(r.env.LockDir)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		sum, head := r.snapshot()
		if err := r.env.Run(); !errors.Is(err, ErrLocked) {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
	t.Run("dirty tree", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		sum, head := r.snapshot()
		os.WriteFile(filepath.Join(r.dir, "scratch"), []byte("x"), 0o644)
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "uncommitted") {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
	t.Run("wrong branch", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		gitIn(t, r.dir, "switch", "-q", "-c", "elsewhere")
		sum, head := r.snapshot()
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "recorded branch") {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
	t.Run("rewritten history", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		gitIn(t, r.dir, "pull", "-q", "--ff-only")
		_, head := r.snapshot()
		// Upstream drops "second" and goes another way.
		gitIn(t, r.work, "reset", "-q", "--hard", "HEAD~1")
		os.WriteFile(filepath.Join(r.work, "file"), []byte("other\n"), 0o644)
		gitIn(t, r.work, "commit", "-qam", "rewritten")
		gitIn(t, r.work, "push", "-q", "--force", "origin", "main")
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "fast-forward") {
			t.Fatalf("%v", err)
		}
		if r.head() != head || r.restartCount() != 0 || exists(filepath.Join(r.bin, prevName)) {
			t.Error("something changed")
		}
	})
	t.Run("remote repointed", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		sum, head := r.snapshot()
		gitIn(t, r.dir, "remote", "set-url", "origin", r.work)
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "as recorded") {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
	t.Run("confirmation", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		sum, head := r.snapshot()
		r.env.Yes = false
		r.env.Confirm = nil
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("no confirmer: %v", err)
		}
		r.env.Confirm = func(string) (bool, error) { return false, errors.New("stdin isn't a terminal") }
		if err := r.env.Run(); err == nil {
			t.Fatal("no terminal, no --yes: went ahead")
		}
		var asked string
		r.env.Confirm = func(q string) (bool, error) { asked = r.out.String(); return false, nil }
		if err := r.env.Run(); err != nil || !strings.Contains(r.out.String(), "Nothing changed") {
			t.Fatalf("declined: %v", err)
		}
		if !strings.Contains(asked, "second") {
			t.Errorf("the incoming log wasn't shown before asking:\n%s", asked)
		}
		// U2: nothing but git ran before the answer: no Go toolchain.
		if b, _ := os.ReadFile(os.Getenv("FAKEGO_LOG")); len(b) > 0 {
			t.Errorf("go ran before confirmation: %s", b)
		}
		r.untouched(sum, head)
	})
	t.Run("build fails", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		sum, head := r.snapshot()
		t.Setenv("FAKEGO_FAIL", "1")
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "nothing live was changed") {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
	t.Run("does not compile", func(t *testing.T) {
		r := newRig(t)
		r.commitMain("broken", "package main\nfunc main() {\n")
		sum, head := r.snapshot()
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "go build") {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
	t.Run("smoke check fails", func(t *testing.T) {
		r := newRig(t)
		r.commitMain("liar", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"pneu "+strings.Repeat("e", 40)+"\") }\n")
		sum, head := r.snapshot()
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "smoke check") {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
	t.Run("daemon answers out of shape", func(t *testing.T) {
		// U5: a baseline that can't be read is no baseline to judge by.
		r := newRig(t)
		r.commit("second")
		sum, head := r.snapshot()
		r.badStatus = true
		if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "not as expected") {
			t.Fatalf("%v", err)
		}
		r.untouched(sum, head)
	})
}

// The new build never comes up: binary, checkout and service all go back,
// and the old build is verified the same way.
func TestUpdateRollback(t *testing.T) {
	r := newRig(t)
	oldSum, old := r.snapshot()
	oldRev := r.liveRev()
	target := r.commit("second")
	r.broken[target] = true
	err := r.env.Run()
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if got, _ := fileSHA(filepath.Join(r.bin, "pneu")); got != oldSum || r.head() != old {
		t.Fatalf("not rolled back: live %s head %s", r.liveRev(), r.head())
	}
	if r.restartCount() != 2 || r.status().Revision != oldRev {
		t.Errorf("restarts %d, running %+v", r.restartCount(), r.status())
	}
	if exists(filepath.Join(r.state, RecordFile)) || len(r.stagedLeft()) > 0 {
		t.Error("left update.json or a staged build")
	}
	o := r.out.String()
	if testing.Verbose() {
		t.Log(o)
	}
	if !strings.Contains(o, "reset to the old revision") || !strings.Contains(o, "verified at the old build") {
		t.Errorf("report:\n%s", o)
	}
}

// Readiness is the restarted process's (U5): an old process still
// answering HTTP on the port, or a new one not yet listening, isn't ready.
func TestUpdateReadiness(t *testing.T) {
	t.Run("old process holds the port", func(t *testing.T) {
		r := newRig(t)
		oldSum, _ := r.snapshot()
		r.commit("second")
		r.staleHTTP = true
		if err := r.env.Run(); err == nil || !strings.Contains(r.out.String(), "an old process holding the port") {
			t.Fatalf("%v\n%s", err, r.out)
		}
		if got, _ := fileSHA(filepath.Join(r.bin, "pneu")); got != oldSum {
			t.Error("not rolled back")
		}
	})
	t.Run("not listening", func(t *testing.T) {
		r := newRig(t)
		r.commit("second")
		r.notListen = true
		if err := r.env.Run(); err == nil || !strings.Contains(r.out.String(), "hasn't bound its HTTP listeners") {
			t.Fatalf("%v\n%s", err, r.out)
		}
	})
}

// A daemon from before the instance check (status without listening,
// HTTP without Pneu-Instance): the first update from it goes ahead (the
// target held to the full check), and a rollback to it is verified the
// only way it can be, because the record says it was legacy (V1).
func TestUpdateFromLegacyDaemon(t *testing.T) {
	r := newRig(t)
	r.legacyRevs[r.liveRev()] = true
	target := r.commit("second")
	r.broken[target] = true
	if err := r.env.Run(); err == nil || !strings.Contains(r.out.String(), "verified at the old build") ||
		!strings.Contains(r.out.String(), "predates the instance check") {
		t.Fatalf("%v\n%s", err, r.out)
	}
	// The first update from such a daemon: the new build is current.
	r3 := newRig(t)
	r3.legacyRevs[r3.liveRev()] = true
	target3 := r3.commit("second")
	if err := r3.env.Run(); err != nil || r3.status().Revision != target3 {
		t.Fatalf("%v\n%s", err, r3.out)
	}
	// A current build whose HTTP names nobody: not ready.
	r2 := newRig(t)
	r2.httpNobody = true
	r2.commit("second")
	if err := r2.env.Run(); err == nil || !strings.Contains(r2.out.String(), "not the restarted") {
		t.Fatalf("%v\n%s", err, r2.out)
	}
}

// V1: the target answering legacy-style is a readiness failure, rolled
// back, whatever the old build was; and the old build's legacy flag
// survives an interruption into recovery.
func TestUpdateTargetLegacy(t *testing.T) {
	r := newRig(t)
	oldSum, _ := r.snapshot()
	target := r.commit("second")
	r.legacyRevs[target] = true
	if err := r.env.Run(); err == nil || !strings.Contains(r.out.String(), "answers like a build from before the instance check") {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if got, _ := fileSHA(filepath.Join(r.bin, "pneu")); got != oldSum || !strings.Contains(r.out.String(), "verified at the old build") ||
		strings.Contains(r.out.String(), "predates the instance check") {
		t.Errorf("not rolled back to a fully verified old build:\n%s", r.out)
	}

	// Legacy baseline, interrupted after the restart, the target broken:
	// recovery rolls back and verifies the old build the legacy way, from
	// the record alone.
	q := newRig(t)
	q.legacyRevs[q.liveRev()] = true
	target = q.commit("second")
	q.broken[target] = true
	q.env.crash = func(p string) bool { return p == "restarted" }
	if err := q.env.Run(); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(q.state, RecordFile)); !bytes.Contains(b, []byte(`"oldLegacy": true`)) {
		t.Fatalf("record: %s", b)
	}
	q.env.crash = nil
	q.out.Reset()
	if err := q.env.Run(); err == nil || !strings.Contains(q.out.String(), "predates the instance check") || !strings.Contains(q.out.String(), "verified at the old build") {
		t.Fatalf("%v\n%s", err, q.out)
	}
}

// V2: an unreadable baseline. Recovery refuses and keeps the record; a
// rollback still restores the files but says freshness couldn't be
// verified, and fails.
func TestUpdateUnreadableBaseline(t *testing.T) {
	r := newRig(t)
	r.commit("second")
	r.env.crash = func(p string) bool { return p == "swapped" }
	if err := r.env.Run(); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	r.env.crash = nil
	r.badStatus = true
	if err := r.env.Run(); err == nil || !strings.Contains(err.Error(), "recovery stopped") || !exists(filepath.Join(r.state, RecordFile)) {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if r.restartCount() != 0 {
		t.Error("restarted without a baseline")
	}

	q := newRig(t)
	oldSum, _ := q.snapshot()
	target := q.commit("second")
	q.broken[target] = true
	q.env.crash = func(p string) bool {
		if p == "rollback-binary" {
			q.mu.Lock()
			q.badStatus = true
			q.mu.Unlock()
		}
		return false
	}
	if err := q.env.Run(); err == nil || !strings.Contains(q.out.String(), "restart freshness couldn't be verified") {
		t.Fatalf("%v\n%s", err, q.out)
	}
	if got, _ := fileSHA(filepath.Join(q.bin, "pneu")); got != oldSum {
		t.Error("files not restored")
	}
}

// V3: the report reads the checkout when it's made. A branch switch
// during the readiness wait shows, and fails the run; a rollback that
// finds HEAD at the old revision on another branch says so.
func TestUpdateCheckoutReadAtReport(t *testing.T) {
	r := newRig(t)
	target := r.commit("second")
	once := false
	r.env.crash = func(p string) bool {
		if p == "verify" && !once {
			once = true
			gitIn(t, r.dir, "switch", "-q", "-c", "elsewhere")
		}
		return false
	}
	err := r.env.Run()
	if err == nil || !strings.Contains(r.out.String(), "on elsewhere") || strings.Contains(r.out.String(), "Checkout: at ") {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if r.liveRev() != target {
		t.Error("the binary should be deployed")
	}

	q := newRig(t)
	old := q.head()
	target = q.commit("second")
	q.broken[target] = true
	q.env.crash = func(p string) bool {
		if p == "restarted" {
			gitIn(t, q.dir, "switch", "-q", "-c", "elsewhere", old)
		}
		return false
	}
	if err := q.env.Run(); err == nil {
		t.Fatal("no error")
	}
	o := q.out.String()
	if !strings.Contains(o, "at the old revision, but not on main") || !strings.Contains(o, "NOT at the old revision") || !strings.Contains(o, "on elsewhere") {
		t.Errorf("report:\n%s", o)
	}
	if gitIn(t, q.dir, "rev-parse", "main") != target {
		t.Error("main was moved under concurrent work")
	}
}

// ProbeHTTP against pneu's real Auth middleware: the cookieless 403 names
// this process; anything that isn't pneu's answer is refused.
func TestProbeHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	auth := web.NewAuth("pneu.localhost:"+strconv.Itoa(port), "token")
	hs := &http.Server{Handler: auth.Middleware(http.NotFoundHandler())}
	go hs.Serve(ln)
	defer hs.Close()
	got, err := ProbeHTTP(context.Background(), port)
	if err != nil || got != control.Instance() {
		t.Fatalf("%q %v", got, err)
	}
	// Something else on a port: a plain 403, pneu's headers on a 200, an
	// instance out of shape.
	for _, h := range []http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Pneu-Policy", "data")
			w.Header().Set("Pneu-Instance", "0011223344556677")
		},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Pneu-Policy", "data")
			w.Header().Set("Pneu-Instance", "<b>")
			w.WriteHeader(http.StatusForbidden)
		},
	} {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		s := &http.Server{Handler: h}
		go s.Serve(ln)
		if _, err := ProbeHTTP(context.Background(), ln.Addr().(*net.TCPAddr).Port); err == nil {
			t.Error("a non-pneu answer passed")
		}
		s.Close()
	}
}

// The checkout got dirty while the new build was failing: rolled back
// binary, checkout left where it is, and said so.
func TestUpdateRollbackDirtyCheckout(t *testing.T) {
	r := newRig(t)
	oldSum, _ := r.snapshot()
	target := r.commit("second")
	r.broken[target] = true
	r.env.crash = func(p string) bool {
		if p == "restarted" {
			os.WriteFile(filepath.Join(r.dir, "edit-in-progress"), []byte("x"), 0o644)
		}
		return false
	}
	if err := r.env.Run(); err == nil {
		t.Fatal("no error")
	}
	if got, _ := fileSHA(filepath.Join(r.bin, "pneu")); got != oldSum {
		t.Error("binary not restored")
	}
	if r.head() != target || !exists(filepath.Join(r.dir, "edit-in-progress")) {
		t.Error("the dirty checkout was touched")
	}
	if !strings.Contains(r.out.String(), "LEFT as found") {
		t.Errorf("report:\n%s", r.out)
	}
}

// U6: the checkout switched branch under the merge. The binary is
// deployed and verified, but the run never says the checkout is updated.
func TestUpdateCheckoutMovedConcurrently(t *testing.T) {
	r := newRig(t)
	target := r.commit("second")
	r.env.crash = func(p string) bool {
		if p == "swapped" {
			gitIn(t, r.dir, "switch", "-q", "-c", "elsewhere")
		}
		return false
	}
	err := r.env.Run()
	if err == nil || !strings.Contains(err.Error(), "checkout isn't at") {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if r.liveRev() != target || r.status().Revision != target {
		t.Error("the binary should be deployed")
	}
	if strings.Contains(r.out.String(), "Checkout: at ") || !strings.Contains(r.out.String(), "on elsewhere") {
		t.Errorf("report:\n%s", r.out)
	}
	if exists(filepath.Join(r.state, RecordFile)) {
		t.Error("update.json left")
	}
}

// U3(c): the staged build changed after it was checked: refused at the
// swap, rolled back, nothing live changed.
func TestUpdateStagedTampered(t *testing.T) {
	r := newRig(t)
	oldSum, old := r.snapshot()
	r.commit("second")
	r.env.crash = func(p string) bool {
		if p == "prev" {
			for _, s := range r.stagedLeft() {
				os.WriteFile(s, []byte("#!/bin/sh\necho evil\n"), 0o755)
			}
		}
		return false
	}
	if err := r.env.Run(); err == nil || !strings.Contains(r.out.String(), "isn't the one that was built") {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if got, _ := fileSHA(filepath.Join(r.bin, "pneu")); got != oldSum || r.head() != old {
		t.Error("live changed")
	}
}

// U4: a record that can't be cleared durably (its directory can't be
// synced) is reported, never claimed done.
func TestUpdateRecordNotDurable(t *testing.T) {
	r := newRig(t)
	r.commit("second")
	r.env.crash = func(p string) bool {
		if p == "restarted" {
			os.Chmod(r.state, 0o300) // write and search, no read: opening it to fsync fails
		}
		return false
	}
	t.Cleanup(func() { os.Chmod(r.state, 0o755) })
	err := r.env.Run()
	if err == nil || !strings.Contains(r.out.String(), "couldn't be cleared durably") || strings.Contains(r.out.String(), "Updated:") {
		t.Fatalf("%v\n%s", err, r.out)
	}
}

// An update interrupted at each point is finished, cleaned up or rolled
// back by the next pneu update, from update.json.
func TestUpdateRecovery(t *testing.T) {
	for _, c := range []struct {
		point  string
		broken bool
		want   string // "old" or "new": where everything must end
	}{
		{"recorded", false, "old"},
		{"deploying", false, "old"},
		{"prev", false, "old"},
		{"swapped", false, "new"},
		{"merged", false, "new"},
		{"restarted", false, "new"},
		{"swapped", true, "old"}, // finishing finds the new build broken: rolled back
		{"rollback-binary", true, "old"},
	} {
		t.Run(fmt.Sprintf("%s broken=%v", c.point, c.broken), func(t *testing.T) {
			r := newRig(t)
			oldSum, old := r.snapshot()
			target := r.commit("second")
			r.broken[target] = c.broken
			r.env.crash = func(p string) bool { return p == c.point }
			if err := r.env.Run(); !errors.Is(err, errCrash) {
				t.Fatalf("no crash at %s: %v\n%s", c.point, err, r.out)
			}
			if !exists(filepath.Join(r.state, RecordFile)) {
				t.Fatal("no update.json left by the interruption")
			}
			r.env.crash = nil
			r.out.Reset()
			r.env.Run()
			if exists(filepath.Join(r.state, RecordFile)) {
				t.Fatalf("update.json still there:\n%s", r.out)
			}
			live, _ := fileSHA(filepath.Join(r.bin, "pneu"))
			switch c.want {
			case "old":
				if live != oldSum || r.head() != old {
					t.Errorf("want old: live %s head %s\n%s", r.liveRev(), r.head(), r.out)
				}
			case "new":
				if r.liveRev() != target || r.head() != target || r.status().Revision != target {
					t.Errorf("want new: live %s head %s running %+v\n%s", r.liveRev(), r.head(), r.status(), r.out)
				}
			}
			if len(r.stagedLeft()) > 0 {
				t.Error("a staged build left")
			}
		})
	}
}

// U6 in recovery: interrupted after the swap, then the checkout put on
// another branch at the old HEAD: not fast-forwarded there; rolled back.
func TestUpdateRecoveryWrongBranch(t *testing.T) {
	r := newRig(t)
	oldSum, old := r.snapshot()
	r.commit("second")
	r.env.crash = func(p string) bool { return p == "swapped" }
	if err := r.env.Run(); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	gitIn(t, r.dir, "switch", "-q", "-c", "elsewhere")
	r.env.crash = nil
	r.env.Run()
	if live, _ := fileSHA(filepath.Join(r.bin, "pneu")); live != oldSum {
		t.Errorf("not rolled back:\n%s", r.out)
	}
	if gitIn(t, r.dir, "rev-parse", "elsewhere") != old || gitIn(t, r.dir, "rev-parse", "main") != old {
		t.Errorf("a branch was moved:\n%s", r.out)
	}
}

// update.json out of shape, or ambiguous: refused, never guessed at (U7).
func TestUpdateBadRecord(t *testing.T) {
	a, h := strings.Repeat("a", 40), strings.Repeat("0", 64)
	good := map[string]any{"phase": "deploying", "tx": "0011223344556677", "dir": "/x", "oldHead": a, "oldRevision": a,
		"oldModified": false, "oldLegacy": false, "oldSha256": h, "newRevision": a, "newSha256": h, "started": ""}
	enc := func(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }
	without := func(k string) string {
		m := map[string]any{}
		for kk, v := range good {
			if kk != k {
				m[kk] = v
			}
		}
		return enc(m)
	}
	bads := []string{
		`{"phase":"deploying","oldHead":"x"}`,
		without("tx"),
		strings.Replace(enc(good), `"phase":"deploying"`, `"phase":"deploying","phase":"built"`, 1),
		strings.Replace(enc(good), `"phase":"deploying"`, `"Phase":"deploying"`, 1),
		strings.Replace(enc(good), `"tx":"0011223344556677"`, `"tx":null`, 1),
		enc(good) + `{}`,
		enc(good) + ` x`,
	}
	for _, bad := range bads {
		e := &Env{StateDir: t.TempDir()}
		writePrivate(filepath.Join(e.StateDir, RecordFile), []byte(bad))
		if _, err := e.loadRecord(); err == nil || !strings.Contains(err.Error(), "out of shape") {
			t.Errorf("accepted %s: %v", bad, err)
		}
	}
	e := &Env{StateDir: t.TempDir()}
	writePrivate(filepath.Join(e.StateDir, RecordFile), []byte(enc(good)))
	if rec, err := e.loadRecord(); err != nil || rec.Phase != "deploying" {
		t.Errorf("the good record: %v", err)
	}
	// And a whole run refuses to touch anything.
	g := newRig(t)
	writePrivate(filepath.Join(g.state, RecordFile), []byte(bads[2]))
	sum, head := g.snapshot()
	if err := g.env.Run(); err == nil || !strings.Contains(err.Error(), "out of shape") {
		t.Fatalf("%v", err)
	}
	if got, _ := fileSHA(filepath.Join(g.bin, "pneu")); got != sum || g.head() != head {
		t.Error("changed something")
	}
}

// --check on a client: fetch, then record which build is older for the
// daemon's pair, and tell it.
func TestCheckRecordsSkew(t *testing.T) {
	r := newRig(t)
	r.env.Client = true
	mine := r.liveRev()
	newer := r.commit("second")
	r.serverRev = newer
	if err := r.env.Check(); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	c, err := LoadCache(filepath.Join(r.state, CacheFile))
	r.mu.Lock()
	checked := r.checked
	r.mu.Unlock()
	if err != nil || c.Lookup(mine, newer) != RelClientOlder || checked != 1 {
		t.Fatalf("%+v %v checked %d\n%s", c, err, checked, r.out)
	}
	if !strings.Contains(r.out.String(), "1 commit(s)") {
		t.Errorf("output:\n%s", r.out)
	}
	if err := r.env.Run(); err != nil {
		t.Fatal(err)
	}
	if r.status().Revision != newer {
		t.Fatal("not updated")
	}
}

// Which is older: ancestry in the checkout only. Commit dates are made to
// lie (the older commit dated later), and vcs.time is never read.
func TestRelate(t *testing.T) {
	r := newRig(t)
	t.Setenv("GIT_COMMITTER_DATE", "2030-01-01T00:00:00Z")
	t.Setenv("GIT_AUTHOR_DATE", "2030-01-01T00:00:00Z")
	a := r.commit("a")
	t.Setenv("GIT_COMMITTER_DATE", "2001-01-01T00:00:00Z")
	t.Setenv("GIT_AUTHOR_DATE", "2001-01-01T00:00:00Z")
	b := r.commit("b") // newer in history, older by date
	gitIn(t, r.work, "switch", "-q", "-c", "side", a)
	os.WriteFile(filepath.Join(r.work, "file"), []byte("side\n"), 0o644)
	gitIn(t, r.work, "commit", "-qam", "side")
	gitIn(t, r.work, "push", "-q", "origin", "side")
	side := gitIn(t, r.work, "rev-parse", "HEAD")
	gitIn(t, r.dir, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")

	for _, c := range []struct{ client, server, want string }{
		{a, b, RelClientOlder},
		{b, a, RelServerOlder},
		{b, side, RelDiverged},
		{a, strings.Repeat("f", 40), RelUnknown},
		{a, "not-a-revision", RelUnknown},
		{a, a, RelUnknown},
	} {
		if got := r.env.Relate(c.client, c.server); got != c.want {
			t.Errorf("Relate(%.7s, %.7s) = %s, want %s", c.client, c.server, got, c.want)
		}
	}
	for _, f := range []string{"skew.go", "update.go", "strict.go"} {
		s, _ := os.ReadFile(f)
		if bytes.Contains(s, []byte(`"vcs.time"`)) {
			t.Errorf("%s reads vcs.time", f)
		}
		if bytes.Contains(s, []byte("%ct")) || bytes.Contains(s, []byte("committerdate")) || bytes.Contains(s, []byte("--date")) {
			t.Errorf("%s reads a commit date", f)
		}
	}
}

// U1: in a partial clone, a revision the server named that the checkout
// lacks is never fetched from the promisor remote: it's unknown, and the
// remote's upload-pack never runs.
func TestRelateNoLazyFetch(t *testing.T) {
	gitTestEnv(t)
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", origin)
	gitIn(t, origin, "config", "uploadpack.allowFilter", "true")
	gitIn(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	work := filepath.Join(base, "work")
	gitIn(t, base, "clone", "-q", origin, work)
	os.WriteFile(filepath.Join(work, "f"), []byte("a\n"), 0o644)
	gitIn(t, work, "add", "f")
	gitIn(t, work, "commit", "-qm", "a")
	gitIn(t, work, "push", "-q", "origin", "HEAD:main")
	a := gitIn(t, work, "rev-parse", "HEAD")
	pc := filepath.Join(base, "pc")
	gitIn(t, base, "clone", "-q", "--filter=blob:none", "file://"+origin, pc)
	// A commit the server could name: on the remote, not in the clone.
	os.WriteFile(filepath.Join(work, "f"), []byte("b\n"), 0o644)
	gitIn(t, work, "commit", "-qam", "b")
	gitIn(t, work, "push", "-q", "origin", "HEAD:main")
	x := gitIn(t, work, "rev-parse", "HEAD")
	called := filepath.Join(base, "upload-pack-ran")
	logger := filepath.Join(base, "upload-pack")
	os.WriteFile(logger, []byte("#!/bin/sh\ntouch "+called+"\nexec git-upload-pack \"$@\"\n"), 0o755)
	gitIn(t, pc, "config", "remote.origin.uploadpack", logger)

	e := &Env{Source: config.Source{Dir: pc}, Git: "git"}
	if got := e.Relate(a, x); got != RelUnknown {
		t.Errorf("Relate = %s, want unknown", got)
	}
	if got := e.Relate(x, a); got != RelUnknown {
		t.Errorf("Relate = %s, want unknown", got)
	}
	if _, err := e.ancestor(a, x); err == nil {
		t.Error("ancestry answered for a missing commit")
	}
	if _, err := e.rev(x); err == nil {
		t.Error("rev-parse resolved a missing commit")
	}
	if exists(called) {
		t.Fatal("the promisor remote was asked for the server's revision")
	}
	// An inherited GIT_NO_LAZY_FETCH=0 doesn't turn it back on.
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	e.Relate(a, x)
	if exists(called) {
		t.Fatal("an inherited GIT_NO_LAZY_FETCH=0 turned lazy fetching on")
	}
	// The hazard is real: plain git would have fetched it.
	exec.Command("git", "-C", pc, "cat-file", "-e", x+"^{commit}").Run()
	if !exists(called) {
		t.Fatal("plain git didn't lazy-fetch here, so the checks above prove nothing")
	}
}

// U3: an updater killed mid-build leaves its build running; while that
// lives, no other pneu update can take the lock (the child holds it), and
// once it's gone the next update sweeps the litter and runs.
func TestUpdateKilledMidBuild(t *testing.T) {
	r := newRig(t)
	target := r.commit("second")
	block := filepath.Join(t.TempDir(), "block")
	spec, _ := json.Marshal(helperSpec{Source: r.env.Source, Port: r.port, Go: r.env.Go, Systemctl: r.env.Systemctl,
		BinDir: r.bin, StateDir: r.state, LockDir: r.env.LockDir, Socket: r.sock})
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperUpdate$")
	cmd.Env = append(os.Environ(), "PNEU_UPDATE_HELPER="+string(spec), "FAKEGO_BLOCK="+block)
	// A file, not a pipe: the orphan holds the helper's stdout, and Wait
	// would wait for a pipe's last writer.
	hout, err := os.Create(filepath.Join(t.TempDir(), "helper.out"))
	if err != nil {
		t.Fatal(err)
	}
	defer hout.Close()
	cmd.Stdout, cmd.Stderr = hout, hout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; !exists(block + ".started"); i++ {
		if i > 600 {
			cmd.Process.Kill()
			b, _ := os.ReadFile(hout.Name())
			t.Fatalf("the build never started:\n%s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	// The updater is dead; the build it started still runs, holding the lock.
	if f, err := Lock(r.env.LockDir); !errors.Is(err, ErrLocked) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("lock taken while the orphan runs: %v", err)
	}
	if err := r.env.Run(); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second update ran alongside: %v", err)
	}
	litter := filepath.Join(r.bin, newPrefix+"ffffffffffffffff")
	os.WriteFile(litter, []byte("x"), 0o755)
	os.WriteFile(block+".release", nil, 0o644)
	for i := 0; ; i++ {
		f, err := Lock(r.env.LockDir)
		if err == nil {
			f.Close()
			break
		}
		if i > 200 {
			t.Fatal("the orphan never let go")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := r.env.Run(); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if r.liveRev() != target || exists(litter) {
		t.Errorf("live %s, litter left %v", r.liveRev(), exists(litter))
	}
}

type helperSpec struct {
	Source                            config.Source
	Port                              int
	Go, Systemctl                     string
	BinDir, StateDir, LockDir, Socket string
}

// TestHelperUpdate is the updater TestUpdateKilledMidBuild kills: a real
// process, so its death is the kernel's.
func TestHelperUpdate(t *testing.T) {
	raw := os.Getenv("PNEU_UPDATE_HELPER")
	if raw == "" {
		t.Skip("only as TestUpdateKilledMidBuild's subprocess")
	}
	var s helperSpec
	json.Unmarshal([]byte(raw), &s)
	e := &Env{Source: s.Source, Port: s.Port, Git: "git", Go: s.Go, Systemctl: s.Systemctl, BinDir: s.BinDir,
		StateDir: s.StateDir, LockDir: s.LockDir, Socket: s.Socket, Yes: true, Out: os.Stdout, Err: os.Stdout}
	e.Run()
	os.Exit(0)
}

func TestSkew(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	c := &Cache{}
	c.Add(Pair{Client: a, Server: b, Relation: RelClientOlder})
	c.Add(Pair{Client: b, Server: a, Relation: RelServerOlder})
	for _, x := range []struct {
		client, server Build
		want           string // "" for no nudge
	}{
		{Build{a, false}, Build{a, false}, ""},
		{Build{a, true}, Build{a, false}, Different},
		{Build{a, false}, Build{a, true}, Different},
		{Build{a, false}, Build{b, false}, ClientOlder},
		{Build{b, false}, Build{a, false}, ServerOlder},
		{Build{a, true}, Build{b, false}, Different},
		{Build{a, false}, Build{b, true}, Different},
		{Build{"", false}, Build{"", false}, Different},
		{Build{a, false}, Build{"", false}, Different},
		{Build{strings.ToUpper(a), false}, Build{b, false}, Different},
	} {
		v := Skew(x.client, x.server, c)
		switch {
		case x.want == "" && v != nil:
			t.Errorf("%+v %+v: %+v", x.client, x.server, v)
		case x.want != "" && (v == nil || v.State != x.want):
			t.Errorf("%+v %+v: %+v, want %s", x.client, x.server, v, x.want)
		}
		if v != nil {
			for _, r := range []string{v.Client, v.Server} {
				if r != "unknown" && !ValidRevision(r) {
					t.Errorf("revision %q in the view", r)
				}
			}
		}
	}
	if v := Skew(Build{a, false}, Build{b, false}, nil); v.State != Different {
		t.Errorf("no cache: %+v", v)
	}
	if !slices.Equal(control.UpdateCodes, []string{ClientOlder, ServerOlder, Different}) {
		t.Error("control.UpdateCodes isn't the skew states")
	}
}

func TestCacheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, CacheFile)
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for i := range MaxPairs + 3 {
		if err := SaveCache(p, Pair{Client: a, Server: fmt.Sprintf("%040x", i), Relation: RelDiverged}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	SaveCache(p, Pair{Client: a, Server: b, Relation: RelServerOlder}, time.Now())
	c, err := LoadCache(p)
	if err != nil || len(c.Pairs) != MaxPairs || c.Lookup(a, b) != RelServerOlder {
		t.Fatalf("%+v %v", c, err)
	}
	if err := SaveCache(p, Pair{Client: "x", Server: b, Relation: RelServerOlder}, time.Now()); err == nil {
		t.Error("saved a bad revision")
	}
	pair := `{"client":"` + a + `","server":"` + b + `","relation":"diverged","checked":""}`
	for _, bad := range []string{
		`{"pairs":[{"client":"x","server":"` + b + `","relation":"diverged","checked":""}]}`,
		`{"pairs":[{"client":"` + a + `","server":"` + b + `","relation":"newer","checked":""}]}`,
		`{"pairs":[],"extra":1}`,
		`{"pairs":[],"pairs":[` + pair + `]}`,
		`{"pairs":[` + strings.Replace(pair, `"relation":"diverged"`, `"relation":"diverged","relation":"server-older"`, 1) + `]}`,
		`{"pairs":[` + strings.Replace(pair, `,"checked":""`, ``, 1) + `]}`,
		`{"pairs":null}`,
		`{"Pairs":[]}`,
		`{"pairs":[]} {}`,
	} {
		writePrivate(p, []byte(bad))
		if _, err := LoadCache(p); err == nil {
			t.Errorf("loaded %s", bad)
		}
	}
	writePrivate(p, []byte(`{"pairs":[`+pair+`]}`))
	if c, err := LoadCache(p); err != nil || c.Lookup(a, b) != RelDiverged {
		t.Errorf("the good cache: %+v %v", c, err)
	}
	os.Chmod(p, 0o644)
	if _, err := LoadCache(p); err == nil {
		t.Error("loaded a 0644 cache")
	}
	if c, err := LoadCache(filepath.Join(dir, "none")); c != nil || err != nil {
		t.Error("a missing cache isn't nil, nil")
	}
}

// W1: every exit that speaks of the checkout reads it at that moment and
// fails when it isn't where that exit expects.
func TestShortcutsValidateCheckout(t *testing.T) {
	// Already current, but the checkout switches branch after the fetch.
	r := newRig(t)
	r.commit("second")
	if err := r.env.Run(); err != nil {
		t.Fatal(err)
	}
	r.out.Reset()
	r.env.crash = func(p string) bool {
		if p == "fetched" {
			gitIn(t, r.dir, "switch", "-q", "-c", "elsewhere")
		}
		return false
	}
	if err := r.env.Run(); err == nil || strings.Contains(r.out.String(), "Already up to date") || !strings.Contains(err.Error(), "on elsewhere") {
		t.Fatalf("%v\n%s", err, r.out)
	}

	// --check on another branch: said, and an error.
	c := newRig(t)
	c.commit("second")
	gitIn(t, c.dir, "switch", "-q", "-c", "elsewhere")
	if err := c.env.Check(); err == nil || !strings.Contains(c.out.String(), "on elsewhere") {
		t.Fatalf("%v\n%s", err, c.out)
	}
	// --check reports from a reading after the fetch.
	d := newRig(t)
	d.commit("second")
	d.env.crash = func(p string) bool {
		if p == "fetched" {
			gitIn(t, d.dir, "pull", "-q", "--ff-only")
		}
		return false
	}
	if err := d.env.Check(); err != nil || !strings.Contains(d.out.String(), "The checkout is at origin/main") {
		t.Fatalf("%v\n%s", err, d.out)
	}

	// Recovery's clean-up shortcuts: built, then the branch switched.
	for _, point := range []string{"recorded", "deploying"} {
		q := newRig(t)
		q.commit("second")
		q.env.crash = func(p string) bool { return p == point }
		if err := q.env.Run(); !errors.Is(err, errCrash) {
			t.Fatal(err)
		}
		gitIn(t, q.dir, "switch", "-q", "-c", "elsewhere")
		q.env.crash = nil
		if err := q.env.Run(); err == nil || !strings.Contains(q.out.String(), "cleaned up") || !strings.Contains(err.Error(), "on elsewhere") {
			t.Fatalf("%s: %v\n%s", point, err, q.out)
		}
	}
}

// W2: the verdict and the words come from one reading.
func TestCheckoutReading(t *testing.T) {
	a := strings.Repeat("a", 40)
	for _, c := range []struct {
		c    checkout
		ok   bool
		text string
	}{
		{checkout{"main", a}, true, "the checkout is on main at aaaaaaaaaaaa"},
		{checkout{"other", a}, false, "the checkout is on other at aaaaaaaaaaaa"},
		{checkout{"", a}, false, "the checkout is detached at aaaaaaaaaaaa"},
		{checkout{"main", ""}, false, "the checkout's HEAD is unreadable"},
	} {
		if c.c.at("main", a) != c.ok || c.c.String() != c.text {
			t.Errorf("%+v: %v %q", c.c, c.c.at("main", a), c.c.String())
		}
	}
	// A checkout moving as it's read: the verdict and the words are the
	// same (final) reading, never one of each.
	b := strings.Repeat("b", 40)
	seq := []checkout{{"main", a}, {"other", b}, {"main", a}, {"other", b}, {"other", b}, {"other", b}}
	i := 0
	e := &Env{Source: config.Source{Branch: "main"}, readCheckout: func() checkout { c := seq[i%len(seq)]; i++; return c }}
	where, ok := e.checkoutAt(a)
	if ok != strings.Contains(where, "on main at aaaaaaaaaaaa") {
		t.Errorf("verdict %v, words %q", ok, where)
	}
	if !ok || where != "the checkout is on main at aaaaaaaaaaaa" {
		t.Errorf("want the third reading: %v %q", ok, where)
	}
}

// W3: a fetch that leaves a long-lived helper behind (as
// credential-cache--daemon does) doesn't hand it the update lock: the
// next update isn't locked out.
func TestFetchHelperDoesNotHoldLock(t *testing.T) {
	r := newRig(t)
	target := r.commit("second")
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { os.WriteFile(release, nil, 0o644) })
	wrapper := filepath.Join(r.scripts, "git")
	real, _ := exec.LookPath("git")
	os.WriteFile(wrapper, []byte(`#!/bin/bash
for a in "$@"; do
	if [ "$a" = fetch ]; then
		( while [ ! -e "`+release+`" ]; do sleep 0.05; done ) </dev/null >/dev/null 2>&1 &
		break
	fi
done
exec "`+real+`" "$@"
`), 0o755)
	r.env.Git = wrapper
	if err := r.env.Run(); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	f, err := Lock(r.env.LockDir)
	if err != nil {
		t.Fatalf("the fetch's helper holds the lock: %v", err)
	}
	f.Close()
	r.out.Reset()
	if err := r.env.Run(); err != nil || !strings.Contains(r.out.String(), "Already up to date") || r.liveRev() != target {
		t.Fatalf("%v\n%s", err, r.out)
	}
}
