// Command pneu serves the mail UI on 127.0.0.1 for a browser --app window,
// opens that window, and runs lieer by hand for an account.
//
//	pneu [serve] [-config path] [-listen addr]
//	pneu open [-config path]
//	pneu gmi [-config path] <account> <gmi args...>
//	pneu account add|auth|status ...
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/web"
)

// usageError exits 2 rather than 1.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func main() {
	log.SetFlags(0)
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "open":
		err = openWindow(args)
	case "gmi":
		err = runGmi(args)
	case "account":
		err = account(args)
	default:
		err = usageError{fmt.Sprintf("unknown command %q; usage: pneu [serve|open|gmi|account] ...", cmd)}
	}
	if err != nil {
		log.Print("pneu: ", err)
		if errors.As(err, new(usageError)) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// loadConfig parses args with fs, which gets -config, and loads the config.
func loadConfig(fs *flag.FlagSet, args []string) (config.Config, error) {
	cfgPath := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, usageError{err.Error()}
	}
	if *cfgPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return config.Config{}, err
		}
		*cfgPath = p
	}
	return config.Load(*cfgPath)
}

func hostFor(cfg config.Config) string { return "pneu.localhost:" + strconv.Itoa(cfg.Port) }

func serve(args []string) error {
	fs := flag.NewFlagSet("pneu serve", flag.ContinueOnError)
	listen := fs.String("listen", "", "single listen address (default: both loopbacks on the config port)")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}

	host := hostFor(cfg)
	launchPath, err := web.LaunchPath()
	if err != nil {
		return err
	}
	statusPath, err := web.StatusPath()
	if err != nil {
		return err
	}
	tokenPath, err := web.TokenPath()
	if err != nil {
		return err
	}
	token, err := web.LoadOrCreateToken(tokenPath)
	if err != nil {
		return err
	}

	accounts := make([]notmuch.Account, len(cfg.Accounts))
	for i, a := range cfg.Accounts {
		accounts[i] = notmuch.Account{Name: a.Name, Email: a.Email, ConfigPath: a.NotmuchConfig}
	}
	srv, err := web.New(accounts, host, token)
	if err != nil {
		return err
	}

	// pneu.localhost resolves to both loopbacks and clients try ::1 first, so
	// bind both or another local process on [::1]:port would receive the traffic.
	addrs := []string{*listen}
	if *listen == "" {
		port := strconv.Itoa(cfg.Port)
		addrs = []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("::1", port)}
	}
	var lns []net.Listener
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		lns = append(lns, ln)
	}
	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /events streams indefinitely.
	}
	hs.RegisterOnShutdown(srv.Hub.Close)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The server owns every unattended gmi run (PLAN.md item 12). A sync that
	// changed nothing doesn't refresh the view.
	var gmiAccounts []gmi.Account
	for _, a := range cfg.Accounts {
		gmiAccounts = append(gmiAccounts, gmi.Account{Name: a.Name, GmiDir: a.GmiDir, NotmuchConfig: a.NotmuchConfig})
	}
	syncer, err := gmi.New(gmiAccounts, gmi.Options{
		// The header's sync glyph spins between `syncing` and `sync`. Pushes
		// broadcast neither: they change nothing locally, and a keystroke's
		// own push must not reload the list out from under the row it just
		// removed. Every sync's end is broadcast — the glyph must stop on a
		// pull that brought nothing, or failed — but only `changed` makes the
		// client re-render the list.
		OnStart: func(account string, op gmi.Op) {
			if op == gmi.OpSync {
				srv.Hub.Broadcast("syncing", map[string]any{"account": account})
			}
			if op == gmi.OpPull {
				srv.AccountChanged(account)
			}
		},
		// The status file takes every end, pushes included: a failed push
		// counts toward the account's failures. A first pull's end is a
		// `sync` to the page too: the list refreshes.
		OnSynced: func(account string, r gmi.Result) {
			if r.Op == gmi.OpSync || r.Op == gmi.OpPull {
				srv.Hub.Broadcast("sync", map[string]any{"account": account, "op": r.Op, "changed": r.Changed, "at": r.Started.Add(r.Duration)})
			}
			srv.StatusChanged()
			srv.AccountChanged(account)
		},
		// A first pull's progress: SSE `account` every report, the status
		// file when the bar's figure moves.
		OnProgress: func(account string, _ gmi.Progress) { srv.AccountChanged(account) },
		OnAuth: func(account string, err error) {
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			srv.Hub.Broadcast("auth", map[string]any{"account": account, "ok": err == nil, "error": msg})
			srv.AccountChanged(account)
		},
	})
	if err != nil {
		return err
	}
	srv.Syncer = syncer
	// Theme switches restyle open pages (SSE `theme`).
	go srv.WatchTheme(ctx, web.ThemePoll)
	syncDone := make(chan struct{})
	go func() { syncer.Run(ctx); close(syncDone) }()

	// Bound, so a launcher that reads the nonce now will find a server.
	if err := srv.Auth.StartLaunch(launchPath); err != nil {
		return err
	}
	statusDone := make(chan struct{})
	go func() { srv.RunStatus(ctx, statusPath); close(statusDone) }()
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errc <- hs.Serve(ln) }()
		log.Printf("pneu listening on %s", ln.Addr())
	}

	var serveErr error
	select {
	case serveErr = <-errc:
		stop() // the status file still gets its running:false
	case <-ctx.Done():
	}
	<-statusDone
	if serveErr != nil {
		return serveErr
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Wait for the in-flight gmi to finish or be killed so the flock is released.
	// Can take up to the sync timeout; the unit sets TimeoutStopSec to match. A
	// first pull is interrupted instead (SIGINT, then SIGKILL after 30s).
	<-syncDone
	return nil
}

// launcher is Omarchy's launch-or-focus for web apps: it focuses a window
// whose class or title matches the pattern, else opens the URL as an --app
// window of the default browser.
const launcher = "omarchy-launch-or-focus-webapp"

// windowPattern matches the --app window's class. Chromium builds it from
// host and path (port and query dropped) behind a browser prefix and before
// the profile: chrome-pneu.localhost__open-Default under helium, and the
// prefix and profile differ elsewhere. The launcher tests it as a regex
// between word boundaries, case-insensitively; the unescaped dot is harmless.
const windowPattern = "pneu.localhost__open"

// openWait is how long `pneu open` waits for the server: right after login
// the unit may still be starting.
const openWait = 10 * time.Second

// openWindow focuses the pneu window, or opens one on a fresh launch URL. The URL
// carries only the single-use nonce, never the install token.
func openWindow(args []string) error {
	cfg, err := loadConfig(flag.NewFlagSet("pneu open", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(launcher)
	if err != nil {
		return fmt.Errorf("%s not found on PATH: pneu open needs Omarchy", launcher)
	}
	launchPath, err := web.LaunchPath()
	if err != nil {
		return err
	}
	host := hostFor(cfg)
	u, err := waitForServer(cfg.Port, host, launchPath, openWait)
	if err != nil {
		if n, lerr := exec.LookPath("notify-send"); lerr == nil {
			exec.Command(n, "-u", "critical", "Pneu", "Server not running: systemctl --user status pneu").Run()
		}
		return err
	}
	// The launcher evals its command line, where the URL's '?' is a glob;
	// from / nothing can match it.
	if err := os.Chdir("/"); err != nil {
		return err
	}
	return syscall.Exec(bin, []string{launcher, windowPattern, u}, os.Environ())
}

// waitForServer polls until the server answers HTTP and returns the launch
// URL. An answer means Serve is running, which starts only after the server
// wrote this run's nonce, so a launch file left by an earlier run is never
// read in its place.
func waitForServer(port int, host, launchPath string, wait time.Duration) (string, error) {
	client := &http.Client{
		Timeout:       time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	target := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/"
	deadline := time.Now().Add(wait)
	for {
		req, _ := http.NewRequest(http.MethodHead, target, nil)
		req.Host = host
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			// No cookie, so a pneu answers 403; anything else isn't one.
			if resp.StatusCode != http.StatusForbidden {
				return "", fmt.Errorf("something other than pneu answers on port %d (HTTP %d)", port, resp.StatusCode)
			}
			return web.LaunchURL(launchPath, "http://"+host)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("server not answering on port %d after %v: systemctl --user status pneu", port, wait)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// gmiWait is how long `pneu gmi` waits for a server-run sync to release the
// account's lock; a sync is bounded at 10 minutes.
const gmiWait = 10 * time.Minute

// runGmi runs lieer by hand for one account: its NOTMUCH_CONFIG, its lieer
// dir, and the flock the server's own runs take. It takes the lock and then
// execs gmi with the lock's fd inherited, rather than running gmi as a
// child: gmi then holds the lock for exactly its own life (the kernel drops
// it however gmi exits), signals from the terminal or systemd-run reach gmi
// directly, and the exit status is gmi's own, with no forwarding code.
func runGmi(args []string) error {
	fs := flag.NewFlagSet("pneu gmi", flag.ContinueOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	var names []string
	for _, a := range cfg.Accounts {
		names = append(names, a.Name)
	}
	usage := fmt.Sprintf("usage: pneu gmi <account> <gmi args...>; accounts: %s", strings.Join(names, ", "))
	if fs.NArg() == 0 {
		return usageError{usage}
	}
	a, ok := cfg.Account(fs.Arg(0))
	if !ok {
		return usageError{fmt.Sprintf("unknown account %q; %s", fs.Arg(0), usage)}
	}
	bin, err := exec.LookPath("gmi")
	if err != nil {
		return fmt.Errorf("gmi (lieer) not found on PATH")
	}
	// `gmi init` runs inside the lieer dir, so the first run makes it.
	if err := os.MkdirAll(a.GmiDir, 0o700); err != nil {
		return err
	}
	lockPath := gmi.DefaultLockPath(a.GmiDir)
	if _, err := gmi.LockForExec(lockPath, gmiWait, func() {
		fmt.Fprintf(os.Stderr, "pneu gmi: waiting up to %v for %s (pneu is syncing %s)\n", gmiWait, lockPath, a.Name)
	}); err != nil {
		return err
	}
	if err := os.Chdir(a.GmiDir); err != nil {
		return err
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "NOTMUCH_CONFIG=") })
	env = append(env, "NOTMUCH_CONFIG="+a.NotmuchConfig)
	return syscall.Exec(bin, append([]string{"gmi"}, fs.Args()[1:]...), env)
}
