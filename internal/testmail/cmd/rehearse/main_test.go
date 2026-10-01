package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// INSTALL.md must stay rehearsable: every placeholder has a value, every
// file block names its path, and the steps come out in order.
func TestInstallScript(t *testing.T) {
	var b strings.Builder
	if err := script("../../../../INSTALL.md", false, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "\nnote ") {
		t.Errorf("INSTALL.md has a block the rehearsal can't run:\n%s", out)
	}
	last := -1
	for _, want := range []string{"step '1.", "cmd 'go build", "human '4.", "cmd 'pneu account add $ACCT $ADDRESS", "cmd 'pneu account auth $ACCT'", "human '**(human)**", "cmd 'systemctl --user enable --now pneu.service'", "step '7."} {
		i := strings.Index(out, want)
		if i < 0 || i < last {
			t.Fatalf("script lacks %q in order:\n%s", want, out)
		}
		last = i
	}
	if strings.Contains(out, "Uninstall") || strings.Contains(out, "rm -r") {
		t.Error("the Uninstall section leaked into the rehearsal")
	}
	// The rehearsal is a server's install: the client path and the peer
	// block are sections of their own, never run by it.
	for _, leak := range []string{"pneu client pair", "packages client", ".peer = ", "step 'Client path'", "step 'Let other machines in'"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked into the rehearsal:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, `cmd 'pkexec "$PWD/install/packages" server "$USER" ~/.cache/pneu/lieer'`) {
		t.Errorf("step 1 doesn't install a server:\n%s", out)
	}
}

// The Uninstall section is rehearsed on its own, after the install.
func TestUninstallScript(t *testing.T) {
	var b strings.Builder
	if err := script("../../../../INSTALL.md", true, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "\nnote ") {
		t.Errorf("the Uninstall section has a block the rehearsal can't run:\n%s", out)
	}
	if strings.Contains(out, "go build") || !strings.Contains(out, "step 'Uninstall'") {
		t.Errorf("script --uninstall isn't just the Uninstall section:\n%s", out)
	}
	for _, want := range []string{"cmd 'omarchy plugin remove pneu --yes", "cmd 'systemctl --user disable --now pneu.service'", "cmd 'rm -r ~/mail/$ACCT"} {
		if !strings.Contains(out, want) {
			t.Errorf("script lacks %q:\n%s", want, out)
		}
	}
}

func TestSubstituteUnknownPlaceholder(t *testing.T) {
	if _, err := substitute("pneu gmi <acct> <mystery>", 7); err == nil || !strings.Contains(err.Error(), "INSTALL.md:7: placeholder <mystery>") {
		t.Errorf("err = %v", err)
	}
}

// scripts/rehearse deletes its sandbox, so a REHEARSE_DIR that resolves to
// anything but its own temp directory is refused before anything runs:
// `..` back into HOME, a temp root itself, a path outside the temp roots,
// and a populated directory it didn't make.
func TestRehearseDirGuard(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, "rehearsal"), 0o755); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(home, "canary")
	os.WriteFile(canary, []byte("keep me"), 0o644)
	for _, dir := range []string{
		filepath.Join(home, "rehearsal", ".."),
		"/tmp",
		"/var/tmp/rehearsal",
		home,
	} {
		cmd := exec.Command("../../../../scripts/rehearse")
		cmd.Env = append(os.Environ(), "REHEARSE_DIR="+dir, "HOME="+home)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "rehearse: ") {
			t.Errorf("REHEARSE_DIR=%s: %v\n%s", dir, err, out)
		}
		if _, err := os.Stat(canary); err != nil {
			t.Fatalf("REHEARSE_DIR=%s deleted HOME", dir)
		}
	}
}
