package gmi

// Onboarding: an account's state from lieer's files, the server-run first
// pull, and re-auth after a token dies. docs/onboarding.md is the design.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
)

// State is where an account is between `pneu account add` and a working
// mailbox.
type State string

const (
	StateUnconfigured State = "unconfigured" // no lieer repository: `pneu account add`
	StateUnauthorized State = "unauthorized" // no credentials: `pneu account auth`
	StateNeedsPull    State = "needs-pull"   // authorized, first pull not complete, none running
	StatePulling      State = "pulling"      // the first pull is running
	StateReady        State = "ready"
	StateReauth       State = "reauth" // the last run failed on a dead refresh token
)

var (
	ErrNotConfigured = errors.New("gmi: no lieer repository; run `pneu account add`")
	ErrNotAuthorized = errors.New("gmi: not authorized; run `pneu account auth`")
	// ErrReauth wraps a run that failed with lieer's invalid_grant: the
	// refresh token expired (Testing-mode OAuth apps: 7 days) or was revoked.
	ErrReauth = errors.New("Gmail access expired or was revoked")
	// ErrStalled wraps a first pull the watchdog stopped.
	ErrStalled = errors.New("gmi: first pull stalled")
	// ErrNoReauth is Reauth refusing: the account's token hasn't failed.
	ErrNoReauth = errors.New("gmi: re-auth is offered only after the account's token has failed")
	// ErrAuthing is Reauth refusing a second re-auth while one waits.
	ErrAuthing = errors.New("gmi: a re-auth is already waiting on the consent screen")
	// ErrAlreadyConnected is Reauth finding the token works after all:
	// nothing was deleted, and the account is healthy again.
	ErrAlreadyConnected = errors.New("gmi: the account's credentials work; it's connected")
)

// lieer's files in its repository (lieer/local.py:Local.__init__).
const (
	lieerConfig      = ".gmailieer.json"
	lieerState       = ".state.gmailieer.json"
	lieerCredentials = ".credentials.gmailieer.json"
	lieerResume      = ".resume-pull.gmailieer.json"
)

// FileState is an account's state from its lieer repository alone: whether
// `gmi init` ran, whether credentials exist (a stat; pneu never reads them),
// and whether a first pull completed. It can't see a running pull or a
// dead token; Engine.Status adds those.
func FileState(gmiDir string) State {
	if _, err := os.Stat(filepath.Join(gmiDir, lieerConfig)); err != nil {
		return StateUnconfigured
	}
	if _, err := os.Stat(filepath.Join(gmiDir, lieerCredentials)); err != nil {
		return StateUnauthorized
	}
	if !(&Account{GmiDir: gmiDir}).pulled() {
		return StateNeedsPull
	}
	return StateReady
}

// firstPull runs `gmi pull` for an account lieer has never completed a pull
// for, as a child of the server: no timeout, the stall watchdog instead,
// interrupted (SIGINT) on shutdown, progress parsed from its output. The
// account is read-only meanwhile, so nothing is written for pneu-touch to
// re-mark. ok is false if the server shut down.
func (e *Engine) firstPull(ctx context.Context, a *account) (Result, bool) {
	args := []string{"pull"}
	if _, err := os.Stat(filepath.Join(a.GmiDir, lieerResume)); err == nil {
		// An earlier pull didn't finish. --resume skips the label fetch for
		// what its metadata phase already did; if its history id has
		// expired lieer starts over by itself (gmailieer.py:full_pull).
		args = append(args, "--resume")
	}
	pw := newProgressWriter(time.Now)
	defer func() {
		a.smu.Lock()
		a.pulling = nil
		a.smu.Unlock()
	}()
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	return e.run(ctx, a, OpPull, args, runOpts{
		interrupt: true,
		stall:     e.opts.StallTimeout,
		out:       pw,
		// Holding the slot and flock now, however long they took (a
		// manual `pneu gmi … pull`, a long consent in `pneu account
		// auth`): only from here is the account pulling, and only from
		// here does the watchdog's clock run.
		before: func() {
			pw.restart(time.Now())
			a.smu.Lock()
			a.pulling = pw
			a.smu.Unlock()
			go e.watchPull(wctx, a, pw)
			e.opts.Logf("gmi: [%s] first pull: gmi %s", a.Name, strings.Join(args, " "))
		},
	})
}

// cleanTmp empties the maildir's tmp/. lieer writes each message there and
// renames it into cur/ (local.py:Local.store), and refuses to store a
// message whose tmp file already exists, so one left by a killed gmi would
// fail every run after it that stores mail: a first pull, but also a sync,
// since a resumed pull records its history id before its closing partial
// pull, and a send, which stores the sent copy. run calls it before each.
//
// With the flock held and no gmi of ours running, everything in tmp/ is an
// orphan, unless a bare `gmi` (not through `pneu gmi`, so without the
// flock) is writing there. So the cleanup holds lieer's own repository
// lock throughout, and skips if another process has it; it is released
// before gmi starts.
func (e *Engine) cleanTmp(a *account) {
	unlock, ok := lockLieer(a.GmiDir)
	if !ok {
		e.opts.Logf("gmi: [%s] lieer's .lock is held by another gmi (or can't be taken); mail/tmp left alone", a.Name)
		return
	}
	defer unlock()
	dir := filepath.Join(a.GmiDir, "mail", "tmp")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		if ent.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, ent.Name())); err == nil {
				e.opts.Logf("gmi: [%s] removed %s, left in mail/tmp by an interrupted gmi", a.Name, ent.Name())
			}
		}
	}
}

// lockLieer takes lieer's repository lock without waiting.
// local.py:Local.load_repository takes fcntl.lockf(LOCK_EX|LOCK_NB) on
// .lock: a POSIX record lock over the whole file, which flock(2) doesn't
// see, so this takes the same kind (F_SETLK, whole file). ok is false if
// another process holds it or it can't be taken. unlock releases it; a
// POSIX lock belongs to the process, and a gmi started while it's held
// would fail "failed to lock repository".
func lockLieer(gmiDir string) (unlock func(), ok bool) {
	f, err := os.OpenFile(filepath.Join(gmiDir, ".lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &syscall.Flock_t{Type: syscall.F_WRLCK}); err != nil {
		f.Close()
		return nil, false
	}
	return func() {
		syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &syscall.Flock_t{Type: syscall.F_UNLCK})
		f.Close()
	}, true
}

// watchStall stops the run (cause errStalled) once it has been idle for d:
// no output (pw) and no bytes read by any process in its group (pgid). The
// reads matter: lieer fetches 50 messages in one batch request and prints
// their dots only when the whole response is in, which for big messages
// over a slow link can take longer than d.
func watchStall(ctx context.Context, cancel context.CancelCauseFunc, pw *progressWriter, d time.Duration, pgid int) {
	t := time.NewTicker(min(d/4, time.Second))
	defer t.Stop()
	lastIO, _ := groupRead(pgid)
	ioAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, ok := groupRead(pgid); ok && n != lastIO {
			lastIO, ioAt = n, time.Now()
		}
		idle := min(time.Since(pw.snapshot().Updated), time.Since(ioAt))
		if idle > d {
			cancel(errStalled)
			return
		}
	}
}

// groupRead sums rchar (bytes read, sockets included) from /proc/<pid>/io
// over the processes in process group pgid. ok is false if none was found.
func groupRead(pgid int) (total uint64, ok bool) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + ent.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...; comm may hold spaces and parens.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat[i+1:]))
		if len(f) < 3 || f[2] != strconv.Itoa(pgid) {
			continue
		}
		io, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(io), "\n") {
			if v, ok0 := strings.CutPrefix(line, "rchar: "); ok0 {
				if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
					total, ok = total+n, true
				}
			}
		}
	}
	return total, ok
}

// frontierEvery is how often a running pull's frontier is re-read.
var frontierEvery = 30 * time.Second

// watchPull reports progress (OnProgress) once a second when it changed,
// and keeps the frontier: the oldest message stored so far, which is how
// far back the mail is complete because lieer stores newest first.
func (e *Engine) watchPull(ctx context.Context, a *account, pw *progressWriter) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last Progress
	var lastFrontier time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		p := pw.snapshot()
		if p.Phase == PhaseContent && time.Since(lastFrontier) >= frontierEvery {
			lastFrontier = time.Now()
			if t, err := e.frontier(ctx, a); err == nil && !t.IsZero() {
				pw.setFrontier(t)
				p = pw.snapshot()
			}
		}
		if e.opts.OnProgress != nil && (p.Phase != last.Phase || p.Done != last.Done || p.Total != last.Total || !p.Frontier.Equal(last.Frontier)) {
			e.opts.OnProgress(a.Name, p)
		}
		last = p
	}
}

func (e *Engine) frontier(ctx context.Context, a *account) (time.Time, error) {
	if a.NotmuchConfig == "" {
		return time.Time{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return notmuch.Account{Name: a.Name, ConfigPath: a.NotmuchConfig}.Oldest(ctx)
}

// consentPrompt is how google_auth_oauthlib's run_local_server announces
// the consent URL on stdout.
const consentPrompt = "Please visit this URL to authorize this application: "

// AuthPort is where lieer's consent flow waits for Google's redirect:
// lieer calls run_local_server() with no arguments, so port 8080, fixed.
const AuthPort = 8080

// ErrAuthPort means something else is listening on AuthPort.
var ErrAuthPort = fmt.Errorf("gmi: localhost:%d is in use; the consent flow needs it for Google's redirect", AuthPort)

// CheckAuthPort fails with ErrAuthPort when localhost:8080 is taken.
func CheckAuthPort() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", AuthPort))
	if err != nil {
		return ErrAuthPort
	}
	return ln.Close()
}

// Reauth reconnects an account whose token has failed: it returns the
// consent URL for the app to open, from `gmi auth -f -c <client secret>`.
//
// It claims the account first: one re-auth at a time (ErrAuthing), and
// the account's slot without waiting (ErrBusy: a run holds it; try again),
// then the flock. Holding both, it lists labels (`gmi pull -t`) with the
// credentials lieer has: if they work after all (fixed in a terminal
// meanwhile), the account is healthy again and it returns
// ErrAlreadyConnected rather than delete them, which -f does before the
// flow starts (remote.py:Remote.authorize). Only invalid_grant goes on to
// the consent flow.
//
// That run keeps the slot and flock until consent completes, fails, is
// cancelled (CancelReauth), AuthTimeout passes or Run ends; then the new
// token is checked the same way and OnAuth reports. On success the account
// syncs at once. BROWSER=true keeps lieer from opening a browser itself
// (the app does), and from failing when the server's environment has none
// (webbrowser.Error comes before the URL is printed). The consent URL must
// be Google's.
func (e *Engine) Reauth(ctx context.Context, account string) (string, error) {
	a, ok := e.accts[account]
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	if st, _ := e.Status(account); st.State != StateReauth {
		return "", ErrNoReauth
	}
	// The file could have changed since `pneu account add` cleaned it:
	// check it again before anything reads it.
	b, err := os.ReadFile(a.ClientSecret)
	if err != nil {
		return "", fmt.Errorf("gmi: no OAuth client JSON at %s (INSTALL.md step 4)", a.ClientSecret)
	}
	if _, err := CleanClientSecret(b); err != nil {
		return "", fmt.Errorf("gmi: %s: %w (rerun `pneu account add` with --client-secret)", a.ClientSecret, err)
	}
	a.smu.Lock()
	if a.authing {
		a.smu.Unlock()
		return "", ErrAuthing
	}
	a.authing = true
	a.smu.Unlock()
	fail := func(err error) (string, error) {
		a.smu.Lock()
		a.authing, a.authCancel = false, nil
		a.smu.Unlock()
		return "", err
	}
	if err := CheckAuthPort(); err != nil {
		return fail(err)
	}
	select {
	case a.run <- struct{}{}:
	default:
		return fail(fmt.Errorf("%w: a sync is running; try again in a moment", ErrBusy))
	}
	unlock, err := flockFile(ctx, a.LockPath, nil)
	if err != nil {
		<-a.run
		return fail(fmt.Errorf("%w: %w", ErrBusy, err))
	}
	release := func() { unlock(); <-a.run }
	base, ok := e.startAux()
	if !ok {
		release()
		return fail(errors.New("gmi: shutting down"))
	}
	done := false
	defer func() {
		if !done {
			e.aux.Done()
		}
	}()

	if _, err := os.Stat(filepath.Join(a.GmiDir, lieerCredentials)); err == nil {
		// Without credentials `gmi pull -t` would start a consent flow of
		// its own, with lieer's shared client; only check what exists.
		err := e.checkAuth(base, a)
		if err == nil {
			a.smu.Lock()
			a.authing, a.authCancel = false, nil
			a.status.LastErr, a.status.Failures = nil, 0
			a.smu.Unlock()
			release()
			e.opts.Logf("gmi: [%s] credentials work again; nothing to re-authorize", a.Name)
			signal(a.syncReq)
			return "", ErrAlreadyConnected
		}
		if !errors.Is(err, ErrReauth) {
			release()
			return fail(err)
		}
	}

	actx, cancel := context.WithTimeout(base, e.opts.AuthTimeout)
	cmd := exec.CommandContext(actx, e.opts.GmiPath, "auth", "-f", "-c", a.ClientSecret)
	cmd.Dir = a.GmiDir
	cmd.Env = append(os.Environ(), "NOTMUCH_CONFIG="+a.NotmuchConfig, "PYTHONUNBUFFERED=1", "BROWSER=true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }
	cmd.WaitDelay = 5 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		release()
		return fail(err)
	}
	a.smu.Lock()
	a.authCancel = cancel
	a.smu.Unlock()

	urls := make(chan string, 1)
	tail := &tailBuffer{max: outputLimit}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			line := sc.Text()
			tail.Write([]byte(line + "\n"))
			if u, ok := strings.CutPrefix(line, consentPrompt); ok {
				select {
				case urls <- strings.TrimSpace(u):
				default:
				}
			}
		}
		io.Copy(io.Discard, pr)
	}()
	ended := make(chan error, 1)
	done = true // the wait goroutine owns the aux slot now
	go func() {
		defer e.aux.Done()
		err := cmd.Wait()
		pw.Close()
		<-scanned
		cancel()
		if err != nil {
			msg := lastLine(tail.String())
			if msg == "" {
				msg = err.Error()
			}
			err = fmt.Errorf("gmi auth [%s]: %s", a.Name, msg)
		} else if err = PrivateCredentials(a.GmiDir); err == nil {
			err = e.checkAuth(base, a)
		}
		a.smu.Lock()
		a.authing, a.authCancel = false, nil
		if err == nil {
			a.status.LastErr, a.status.Failures = nil, 0
		}
		a.smu.Unlock()
		release() // after the status is right: the next run must see it
		if err == nil {
			e.opts.Logf("gmi: [%s] re-authorized", a.Name)
			signal(a.syncReq)
		} else {
			e.opts.Logf("gmi: [%s] re-auth failed: %v", a.Name, err)
		}
		if e.opts.OnAuth != nil {
			e.opts.OnAuth(a.Name, err)
		}
		ended <- err
	}()

	select {
	case u := <-urls:
		if !strings.HasPrefix(u, "https://accounts.google.com/") {
			cancel()
			return "", fmt.Errorf("gmi auth [%s]: consent URL isn't Google's: %q", a.Name, u)
		}
		return u, nil
	case err := <-ended:
		if err == nil {
			return "", errors.New("gmi auth finished without asking for consent")
		}
		return "", err
	case <-time.After(30 * time.Second):
		cancel()
		return "", errors.New("gmi auth printed no consent URL")
	}
}

// checkAuth lists labels (`gmi pull -t`): proof the token works for the
// repository's address. The caller holds the slot and flock. A dead token
// wraps ErrReauth.
func (e *Engine) checkAuth(ctx context.Context, a *account) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.opts.GmiPath, "pull", "-t")
	cmd.Dir = a.GmiDir
	cmd.Env = append(os.Environ(), "NOTMUCH_CONFIG="+a.NotmuchConfig, "PYTHONUNBUFFERED=1", "BROWSER=true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := lastLine(string(out))
		if msg == "" {
			msg = err.Error()
		}
		err = fmt.Errorf("gmi pull -t [%s]: %s", a.Name, msg)
		if strings.Contains(string(out), "invalid_grant") {
			err = fmt.Errorf("%w: %w", ErrReauth, err)
		}
		return err
	}
	return nil
}

// PrivateCredentials makes lieer's credentials file 0600. lieer writes it
// with a plain open("w") (remote.py:Remote.__store_credentials__), so its
// mode is whatever the umask leaves: 0644 under the usual 022. A chmod,
// not a read: pneu never reads the token.
func PrivateCredentials(gmiDir string) error {
	if err := os.Chmod(filepath.Join(gmiDir, lieerCredentials), 0o600); err != nil {
		return fmt.Errorf("gmi: making the credentials private: %w", err)
	}
	return nil
}

// CancelReauth stops a re-auth waiting on the consent screen.
func (e *Engine) CancelReauth(account string) error {
	a, ok := e.accts[account]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownAccount, account)
	}
	a.smu.Lock()
	c := a.authCancel
	a.smu.Unlock()
	if c != nil {
		c()
	}
	return nil
}
