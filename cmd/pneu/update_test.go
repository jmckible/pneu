package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
)

func TestVersionLine(t *testing.T) {
	rev := strings.Repeat("ab", 20)
	for in, want := range map[control.Info]string{
		{Revision: rev}:                 "pneu " + rev,
		{Revision: rev, Modified: true}: "pneu " + rev + " modified",
		{}:                              "pneu unknown",
		{Revision: "not hex; rm -rf ~"}: "pneu unknown",
	} {
		if got := versionLine(in); got != want {
			t.Errorf("%+v: %q", in, got)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pneu source set reads the checkout: its top level, its branch and that
// branch's upstream; one remote and no upstream is that remote; anything
// ambiguous is refused.
func TestFindSource(t *testing.T) {
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(gcfg, []byte("[init]\n\tdefaultBranch = main\n[user]\n\tname = T\n\temail = t@example.com\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	git(t, base, "init", "-q", "--bare", origin)
	seed := filepath.Join(base, "seed")
	git(t, base, "clone", "-q", origin, seed)
	os.WriteFile(filepath.Join(seed, "f"), []byte("x"), 0o644)
	git(t, seed, "add", "f")
	git(t, seed, "commit", "-qm", "x")
	git(t, seed, "push", "-q", "origin", "main")
	co := filepath.Join(base, "co")
	git(t, base, "clone", "-q", origin, co)
	os.MkdirAll(filepath.Join(co, "sub"), 0o755)

	src, note, err := findSource("git", filepath.Join(co, "sub"), "", "")
	if err != nil || src != (config.Source{Dir: co, Remote: "origin", URL: origin, Branch: "main"}) || note != "" {
		t.Fatalf("%+v %q %v", src, note, err)
	}
	// No upstream, one remote: that remote and this branch, said.
	git(t, co, "switch", "-q", "-c", "feature")
	src, note, err = findSource("git", co, "", "")
	if err != nil || src.Remote != "origin" || src.Branch != "feature" || note == "" {
		t.Fatalf("%+v %q %v", src, note, err)
	}
	// Two remotes and no upstream: refused.
	git(t, co, "remote", "add", "other", seed)
	if _, _, err := findSource("git", co, "", ""); err == nil {
		t.Error("ambiguous remote accepted")
	}
	if src, _, err := findSource("git", co, "other", ""); err != nil || src.URL != seed {
		t.Errorf("-remote: %+v %v", src, err)
	}
	// A branch other than the one checked out: refused.
	if _, _, err := findSource("git", co, "origin", "main"); err == nil {
		t.Error("branch mismatch accepted")
	}
	// Detached: refused.
	git(t, co, "switch", "-q", "--detach")
	if _, _, err := findSource("git", co, "origin", ""); err == nil {
		t.Error("detached HEAD accepted")
	}
	if _, _, err := findSource("git", base, "", ""); err == nil {
		t.Error("not a checkout accepted")
	}
	// An option-looking remote never gets recorded.
	if err := config.ValidSource(config.Source{Dir: co, Remote: "-oProxy", URL: "x", Branch: "main"}); err == nil {
		t.Error("a remote starting with - is valid")
	}
}

// Without a recorded source pneu update refuses and says how to record it.
func TestUpdateNeedsSource(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"port":7317,"server":{"ssh":"server","node":"nSERVER1CNTRL","port":7320}}`), 0o644)
	err := updateCmd([]string{"-config", cfg})
	if err == nil || !strings.Contains(err.Error(), "pneu source set") {
		t.Fatalf("%v", err)
	}
}

// With stdin not a terminal, nobody can confirm: refused, not read.
func TestTerminalConfirmRefusesPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w.WriteString("y\n")
	w.Close()
	var out strings.Builder
	ok, err := terminalConfirm(r, &out)("Update? ")
	if ok || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("%v %v", ok, err)
	}
}
