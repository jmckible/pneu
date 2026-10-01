package main

// `pneu version`, `pneu source set` and `pneu update` (docs/client.md,
// "Versions and updates"). The transaction is internal/update's; this
// wires it to the config, the user's directories and the terminal.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/update"
	"github.com/jmckible/pneu/internal/web"
)

// versionLine is `pneu version`'s output, and what pneu update's smoke
// check expects of a fresh build: "pneu <revision>", "pneu unknown"
// without a VCS stamp, " modified" after a dirty build's.
func versionLine(in control.Info) string {
	s := "pneu " + update.Shown(in.Revision)
	if in.Modified {
		s += " modified"
	}
	return s
}

func versionCmd(args []string) error {
	if len(args) != 0 {
		return usageError{"usage: pneu version"}
	}
	fmt.Println(versionLine(control.Self()))
	return nil
}

const sourceUsage = "usage: pneu source set [-remote <remote>] [-branch <branch>] [<checkout>]"

// sourceCmd is `pneu source set`: record the checkout pneu is built from,
// and the remote and branch `pneu update` takes code from (R15).
func sourceCmd(args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return usageError{sourceUsage}
	}
	fs := flag.NewFlagSet("pneu source set", flag.ContinueOnError)
	remote := fs.String("remote", "", "the remote to update from (default: the branch's upstream remote)")
	branch := fs.String("branch", "", "the branch (default: the checkout's current branch)")
	cfgPath := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	if err := fs.Parse(args[1:]); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 1 {
		return usageError{sourceUsage}
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	if *cfgPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		*cfgPath = p
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return errors.New("git not found on PATH")
	}
	src, note, err := findSource(gitBin, dir, *remote, *branch)
	if err != nil {
		return err
	}
	raw, err := config.ReadRaw(*cfgPath)
	if err != nil {
		return err
	}
	raw.Source = &src
	if err := config.Write(*cfgPath, raw); err != nil {
		return err
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, "pneu source set: "+note)
	}
	fmt.Printf("pneu update will build %s from %s (%s), branch %s.\n", src.Dir, src.Remote, src.URL, src.Branch)
	return nil
}

// findSource reads the checkout at dir: its top level, current branch, and
// that branch's upstream remote. With no upstream and exactly one remote,
// that remote and the current branch (said in note). The local branch
// must have its remote branch's name: HEAD on it is what update checks.
func findSource(gitBin, dir, remote, branch string) (config.Source, string, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command(gitBin, append([]string{"-C", dir}, args...)...)
		cmd.Env = os.Environ()
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil || !filepath.IsAbs(top) {
		return config.Source{}, "", fmt.Errorf("%s isn't in a git checkout", dir)
	}
	dir = filepath.Clean(top)
	cur, err := git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || cur == "" {
		return config.Source{}, "", errors.New("the checkout isn't on a branch (detached HEAD): git switch to the branch pneu updates from")
	}
	if branch == "" {
		branch = cur
		if merge, _ := git("config", "--get", "branch."+cur+".merge"); merge != "" {
			branch = strings.TrimPrefix(merge, "refs/heads/")
		}
	}
	if branch != cur {
		return config.Source{}, "", fmt.Errorf("the checkout is on %s, but would update from branch %s: switch to a local branch named %s first", cur, branch, branch)
	}
	note := ""
	if remote == "" {
		remote, _ = git("config", "--get", "branch."+cur+".remote")
	}
	if remote == "" {
		list, _ := git("remote")
		names := strings.Fields(list)
		if len(names) != 1 {
			return config.Source{}, "", fmt.Errorf("the branch %s has no upstream and the checkout has %d remotes: name one with -remote", cur, len(names))
		}
		remote = names[0]
		note = fmt.Sprintf("%s has no upstream; using the checkout's one remote, %s (-remote and -branch override)", cur, remote)
	}
	url, err := git("remote", "get-url", "--", remote)
	if err != nil || url == "" {
		return config.Source{}, "", fmt.Errorf("no remote %q in %s", remote, dir)
	}
	src := config.Source{Dir: dir, Remote: remote, URL: url, Branch: branch}
	if err := config.ValidSource(src); err != nil {
		return config.Source{}, "", err
	}
	return src, note, nil
}

// updateCmd is `pneu update [--check] [--yes]`.
func updateCmd(args []string) error {
	fs := flag.NewFlagSet("pneu update", flag.ContinueOnError)
	check := fs.Bool("check", false, "fetch and say what's new (and, on a client, which build is older); change nothing")
	yes := fs.Bool("yes", false, "don't ask before updating")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usageError{"usage: pneu update [--check] [--yes]"}
	}
	if cfg.Source == nil {
		return errors.New("no source recorded, so pneu update doesn't know where code may come from: in the checkout pneu is built from, run pneu source set (INSTALL.md step 2)")
	}
	e, err := newUpdateEnv(cfg, *yes, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if *check {
		return e.Check()
	}
	return e.Run()
}

func newUpdateEnv(cfg config.Config, yes bool, in *os.File, out, errw io.Writer) (*update.Env, error) {
	e := &update.Env{Source: *cfg.Source, Port: cfg.Port, Client: cfg.Server != nil, Yes: yes, Out: out, Err: errw}
	for _, b := range []struct {
		dst  *string
		name string
	}{{&e.Git, "git"}, {&e.Go, "go"}, {&e.Systemctl, "systemctl"}} {
		p, err := exec.LookPath(b.name)
		if err != nil {
			return nil, fmt.Errorf("%s not found on PATH", b.name)
		}
		*b.dst = p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	e.BinDir = filepath.Join(home, ".local", "bin")
	if e.StateDir, err = web.StateDir(); err != nil {
		return nil, err
	}
	sock, err := control.SocketPath()
	if err != nil {
		return nil, fmt.Errorf("%w: pneu update needs the session's runtime dir for its lock and to check the restarted pneu", err)
	}
	e.Socket, e.LockDir = sock, filepath.Dir(sock)
	e.Confirm = terminalConfirm(in, out)
	return e, nil
}

// terminalConfirm asks on the terminal; with stdin not a terminal it
// refuses rather than reading an answer from a pipe.
func terminalConfirm(in *os.File, out io.Writer) func(string) (bool, error) {
	return func(q string) (bool, error) {
		fi, err := in.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false, errors.New("stdin isn't a terminal, so nobody can confirm: run pneu update in a terminal, or pass --yes")
		}
		fmt.Fprint(out, q)
		line, _ := bufio.NewReader(io.LimitReader(in, 64)).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		return a == "y" || a == "yes", nil
	}
}
