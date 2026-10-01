package main

// `pneu agent` and `pneu reset-window`: the bar menu's two actions with
// effects outside the browser (docs/client.md, "The action menu"; R1).
// The widget runs them as fixed argv; nothing from status.json goes into
// either. No page can reach them: they're commands for this user's
// processes, and they talk to the daemon over the control socket.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jmckible/pneu/internal/callout"
	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
)

// agentPrompter is Omarchy's way to start the user's coding agent with a
// prompt, in a terminal of its own.
const agentPrompter = "omarchy-agent-prompt"

// actionEnv is what the two commands touch, swapped out by tests. run
// starts a program from argv, never through a shell; output collects its
// stdout.
type actionEnv struct {
	socket   string // the control socket; "" with sockErr
	sockErr  error
	lookPath func(string) (string, error)
	run      func(name string, args ...string) error
	start    func(name string, args ...string) error
	output   func(name string, args ...string) ([]byte, error)
	sleep    func(time.Duration)
	stdout   io.Writer
	stderr   io.Writer
}

func newActionEnv() *actionEnv {
	e := &actionEnv{lookPath: exec.LookPath, sleep: time.Sleep, stdout: os.Stdout, stderr: os.Stderr}
	e.socket, e.sockErr = control.SocketPath()
	e.run = func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	e.start = func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Dir = "/" // a launcher that evals its line finds nothing to glob
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	e.output = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
	return e
}

// notify shows a desktop notification, if there's a notifier: the bar
// menu has no terminal to print to. Fixed text only.
func (e *actionEnv) notify(urgency, title, body string) {
	if bin, err := e.lookPath("notify-send"); err == nil {
		e.run(bin, "-a", "pneu", "-u", urgency, title, body)
	}
}

// agentCmd is `pneu agent [-print]`: ask the daemon what's wrong, then
// start the user's coding agent on a fixed prompt for that situation.
func agentCmd(args []string) error {
	fs := flag.NewFlagSet("pneu agent", flag.ContinueOnError)
	printOnly := fs.Bool("print", false, "print the prompt instead of starting the agent")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usageError{"usage: pneu agent [-print]"}
	}
	return newActionEnv().agent(cfg, *printOnly)
}

func (e *actionEnv) agent(cfg config.Config, printOnly bool) error {
	f := callout.Facts{Client: cfg.Server != nil, Revision: control.Self().Revision}
	if f.Client {
		f.SSH = cfg.Server.SSH
	}
	var sit control.Situation
	err := control.ErrNotRunning
	if e.sockErr == nil {
		sit, err = control.AskSituation(e.socket)
	}
	switch {
	case err == nil:
		want := control.ModeServer
		if f.Client {
			want = control.ModeClient
		}
		if sit.Mode != want {
			return fmt.Errorf("the running pneu is a %s, but this config makes a %s: restart it (systemctl --user restart pneu)", sit.Mode, want)
		}
		f.Daemon, f.Link, f.ServerRevision, f.Failing, f.More = true, sit.Link, sit.ServerRevision, sit.Failing, sit.More
	case !f.Client:
		return fmt.Errorf("pneu isn't answering here (%v): systemctl --user status pneu", err)
	case !errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(e.stderr, "pneu agent: no usable answer from the daemon (%v); treating it as unreachable.\n", err)
	}
	code := callout.Choose(f)
	if code == callout.None {
		fmt.Fprintln(e.stdout, "pneu agent: nothing for an agent to fix: the link is up and every account syncs.")
		return nil
	}
	prompt, err := callout.Prompt(code, f)
	if err != nil {
		return err
	}
	if printOnly {
		fmt.Fprintln(e.stdout, prompt)
		return nil
	}
	bin, err := e.lookPath(agentPrompter)
	if err != nil {
		fmt.Fprintln(e.stdout, prompt)
		fmt.Fprintf(e.stderr, "\npneu agent: %s isn't installed, so here is the prompt (situation %s). Give it to your coding agent, or run `pneu agent -print` to get it again.\n", agentPrompter, code)
		e.notify("normal", "pneu: Fix with agent", "omarchy-agent-prompt isn't installed. Run pneu agent -print in a terminal for the prompt.")
		return errors.New("no agent launcher")
	}
	// One argument, no shell: the prompt is data to the launcher.
	return e.run(bin, prompt)
}

// The reset (docs/client.md, "Service workers and a poisoned origin"; R4,
// N3).
const (
	// siteSettings is the browser profile's site-data page, filtered to
	// pneu's origin. Chromium-family only, as pneu's window is.
	siteSettings = "chrome://settings/content/all?searchSubpermissions=pneu.localhost"
	// webappLauncher opens a URL as an --app window in the default
	// Chromium-family browser, the profile pneu's own window uses.
	webappLauncher = "omarchy-launch-webapp"
	closeWait      = 5 * time.Second
)

// windowAddress is a Hyprland window address as hyprctl prints it.
var windowAddress = regexp.MustCompile(`^0x[0-9a-f]{1,16}$`)

// hyprClient is the part of `hyprctl clients -j` the reset reads.
type hyprClient struct {
	Address      string `json:"address"`
	Class        string `json:"class"`
	InitialClass string `json:"initialClass"`
	Title        string `json:"title"`
}

// browserClass is a Chromium-family browser's window class: its normal
// windows (chromium, google-chrome, brave-browser, …) and its --app ones
// (chrome-<host>…).
var browserClass = regexp.MustCompile(`(?i)^(chromium|chrome|google-chrome|brave|microsoft-edge|msedge|vivaldi|opera|helium)`)

// windows reads Hyprland's clients: the addresses of pneu's --app windows
// (class holds windowPattern: chrome-pneu.localhost__open-Default and the
// like), and how many other browser windows have "pneu" in their title.
// Titles only ever count toward that warning; only an app window's
// validated address is ever handed to hyprctl.
func (e *actionEnv) windows() (app []string, others int, err error) {
	out, err := e.output("hyprctl", "clients", "-j")
	if err != nil {
		return nil, 0, fmt.Errorf("hyprctl clients: %w", err)
	}
	var cs []hyprClient
	if err := json.Unmarshal(out, &cs); err != nil {
		return nil, 0, fmt.Errorf("hyprctl clients: %w", err)
	}
	for _, c := range cs {
		isApp := strings.Contains(strings.ToLower(c.Class), windowPattern) || strings.Contains(strings.ToLower(c.InitialClass), windowPattern)
		switch {
		case isApp && windowAddress.MatchString(c.Address):
			app = append(app, c.Address)
		case !isApp && browserClass.MatchString(c.Class) && strings.Contains(strings.ToLower(c.Title), "pneu"):
			others++
		}
	}
	return app, others, nil
}

// closeWindows asks Hyprland to close pneu's app windows and waits for
// them to go. It returns how many were open, and an error if any stayed
// (a page asking to leave, say).
func (e *actionEnv) closeWindows() (int, error) {
	addrs, _, err := e.windows()
	if err != nil {
		return 0, err
	}
	for _, a := range addrs {
		// address:<hex> is validated above; argv, never a shell line.
		if err := e.run("hyprctl", "dispatch", "closewindow", "address:"+a); err != nil {
			return len(addrs), fmt.Errorf("hyprctl closewindow: %w", err)
		}
	}
	for waited := time.Duration(0); ; waited += 250 * time.Millisecond {
		left, _, err := e.windows()
		if err != nil {
			return len(addrs), err
		}
		if len(left) == 0 {
			return len(addrs), nil
		}
		if waited >= closeWait {
			return len(addrs), fmt.Errorf("%d of pneu's app windows still open", len(left))
		}
		e.sleep(250 * time.Millisecond)
	}
}

// What the reset says. It knows only about pneu's app windows: a pneu tab
// or popup in the same browser profile keeps running whatever it loaded,
// and only quitting the browser ends it.
const (
	quitBrowser = "If pneu is also open anywhere else in this browser (a tab, a popup, another window), quit the browser entirely now, then choose Reset window data again; make the click below only after that."
	resetSteps  = "pneu's app windows are closed. " + quitBrowser + " In the browser settings window that just opened (Site settings, filtered to pneu.localhost), delete pneu.localhost's data: its trash icon, then Delete. That also deletes compose drafts saved in this browser. Then choose Reopen pneu in pneu's bar menu, or run pneu open."
	othersOpen  = "pneu looks open in %d other browser window(s) (by their titles). Quit the browser entirely (every window), then choose Reset window data again."
)

// resetWindowCmd is `pneu reset-window`.
func resetWindowCmd(args []string) error {
	fs := flag.NewFlagSet("pneu reset-window", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() != 0 {
		return usageError{"usage: pneu reset-window"}
	}
	return newActionEnv().resetWindow()
}

// resetWindow: (1) close pneu's app windows, and stop if a browser window
// still looks like pneu by its title (only quitting the browser closes a
// tab), so no page it knows of holds code in memory; (2) arm Clear-Site-Data for the next authenticated navigation
// (best effort: a worker could answer it); (3) open the browser's own
// site-data page for the origin, the one reliable clear; (4) tell the
// user the one click to make, and that Reopen pneu comes after.
func (e *actionEnv) resetWindow() error {
	n, err := e.closeWindows()
	if err != nil {
		e.notify("critical", "pneu: Reset window data stopped", "Not every one of pneu's app windows closed. Close them (or quit the browser), then choose Reset window data again.")
		return fmt.Errorf("%w; close them (a page may be asking to leave), then run pneu reset-window again", err)
	}
	fmt.Fprintf(e.stdout, "closed %d of pneu's app windows.\n", n)
	// A pneu tab or popup in an ordinary browser window: the reset can't
	// close it, and the clear means nothing while it runs. Stop and say so.
	if _, others, err := e.windows(); err == nil && others > 0 {
		msg := fmt.Sprintf(othersOpen, others)
		e.notify("critical", "pneu: quit the browser first", msg)
		return errors.New(msg)
	}
	armed := errors.New("no control socket")
	if e.sockErr == nil {
		_, armed = control.Send(e.socket, control.ResetWindow, control.Timeout)
	}
	if armed != nil {
		fmt.Fprintf(e.stderr, "pneu reset-window: the daemon didn't arm Clear-Site-Data (%v); the browser's own clear below is what counts.\n", armed)
	} else {
		fmt.Fprintln(e.stdout, "the next pneu page load also asks the browser to clear the origin's cache and storage.")
	}
	opened := false
	if bin, err := e.lookPath(webappLauncher); err == nil {
		opened = e.start(bin, siteSettings) == nil
	}
	if !opened {
		fmt.Fprintf(e.stdout, "Open %s in pneu's browser (the address bar takes it).\n", siteSettings)
	}
	fmt.Fprintln(e.stdout, resetSteps)
	e.notify("critical", "pneu: one click to finish the reset", resetSteps)
	return nil
}
