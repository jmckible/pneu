package install

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// packages runs install/packages against stub pacman, makepkg, runuser and
// id, which log to pacman.log. Installed packages are files under db/.
// makepkg builds lieer-1.6-5-any.pkg.tar.zst but, as Arch's default debug
// option has it, lists a lieer-debug package it never builds. failBuild
// makes the build fail.
func packages(t *testing.T, failBuild bool, installed ...string) (log string, db string, err error) {
	t.Helper()
	me, uerr := user.Current()
	if uerr != nil {
		t.Fatal(uerr)
	}
	tmp := t.TempDir()
	bin, dir := filepath.Join(tmp, "bin"), filepath.Join(tmp, "lieer")
	db = filepath.Join(tmp, "db")
	for _, d := range []string{bin, dir, db} {
		os.MkdirAll(d, 0o755)
	}
	for _, p := range installed {
		os.WriteFile(filepath.Join(db, p), nil, 0o644)
	}
	os.WriteFile(filepath.Join(dir, ".SRCINFO"), []byte("pkgbase = lieer\n\tdepends = python-google-api-python-client\n\tmakedepends = python-setuptools\n\tcheckdepends = python-pytest\n"), 0o644)
	fail := ""
	if failBuild {
		fail = "exit 1"
	}
	stubs := map[string]string{
		"id":      `echo 0`,
		"runuser": `shift 3; exec "$@"`,
		"makepkg": `if [ "$1" = --packagelist ]; then echo "$3/lieer-1.6-5-any.pkg.tar.zst"; echo "$3/lieer-debug-1.6-5-any.pkg.tar.zst"; exit; fi
` + fail + `
touch "$3/lieer-1.6-5-any.pkg.tar.zst"`,
		"pacman": `echo "pacman $*" >>"$LOG"
case $1 in
-Q) [ -f "$DB/$2" ] ;;
-S) shift; for p; do case $p in -*) ;; *) touch "$DB/$p" ;; esac; done ;;
-U) shift; for p; do case $p in -*) ;; *) [ -f "$p" ] || { echo "error: '$p': could not find or read package" >&2; exit 1; }; touch "$DB/$(basename "$p" | sed 's/-[0-9].*//')" ;; esac; done ;;
-Rns) shift; for p; do case $p in -*) ;; *) rm "$DB/$p" ;; esac; done ;;
esac`,
	}
	for name, body := range stubs {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	}
	logf := filepath.Join(tmp, "pacman.log")
	cmd := exec.Command("sh", "packages", "server", me.Username, dir)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "LOG="+logf, "DB="+db)
	out, err := cmd.CombinedOutput()
	b, _ := os.ReadFile(logf)
	t.Logf("output:\n%s\npacman calls:\n%s", out, b)
	return string(b), db, err
}

func has(db, p string) bool {
	_, err := os.Stat(filepath.Join(db, p))
	return err == nil
}

// Arch's makepkg.conf enables debug, so --packagelist names a -debug
// package that an arch=any build never makes: only what was built goes to
// pacman -U. The build-only packages this run added are removed.
func TestPackagesInstallsWhatWasBuilt(t *testing.T) {
	log, db, err := packages(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log, "lieer-debug") {
		t.Error("pacman -U was given the -debug package makepkg never built")
	}
	if !has(db, "lieer") || !has(db, "go") || !has(db, "notmuch") {
		t.Error("lieer, go or notmuch not installed")
	}
	if has(db, "python-setuptools") || has(db, "python-pytest") {
		t.Error("the build-only packages were left installed")
	}
}

// A failed build still removes the build-only packages this run added, and
// leaves ones that were already installed.
func TestPackagesCleansUpAfterAFailure(t *testing.T) {
	_, db, err := packages(t, true, "python-pytest")
	if err == nil {
		t.Fatal("a failed build exited 0")
	}
	if has(db, "python-setuptools") {
		t.Error("the build-only package this run added was left installed")
	}
	if !has(db, "python-pytest") {
		t.Error("a package installed before the run was removed")
	}
}

// run runs install/packages with args against a pacman stub that logs.
func run(t *testing.T, args ...string) (log string, err error) {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "pacman"), []byte("#!/bin/sh\necho \"pacman $*\" >>\"$LOG\"\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "makepkg"), []byte("#!/bin/sh\necho \"makepkg $*\" >>\"$LOG\"\n"), 0o755)
	logf := filepath.Join(tmp, "log")
	cmd := exec.Command("sh", append([]string{"packages"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "LOG="+logf)
	out, err := cmd.CombinedOutput()
	b, _ := os.ReadFile(logf)
	t.Logf("output:\n%s\ncalls:\n%s", out, b)
	return string(b), err
}

// A client keeps no mail: go only, no lieer, no notmuch, nothing built.
func TestPackagesClient(t *testing.T) {
	log, err := run(t, "client")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(log) != "pacman -S --needed --noconfirm go" {
		t.Errorf("client installs:\n%s", log)
	}
}

// The mode comes first and takes exactly its own arguments; anything else
// is a usage error before pacman runs.
func TestPackagesUsage(t *testing.T) {
	for _, args := range [][]string{
		nil, {"desktop"}, {"client", "me"}, {"server"}, {"server", "me"}, {"server", "me", "dir", "extra"}, {"me", "dir"},
	} {
		log, err := run(t, args...)
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 || log != "" {
			t.Errorf("%q: %v, calls %q", args, err, log)
		}
	}
}
