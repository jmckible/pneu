package main

// The client's half of `pneu account` (docs/client.md, "Server work from a
// client"; as built: step 8): one fixed remote command per verb over SSH,
// the parameters as JSON on stdin, the server's events read strictly and
// rendered as plain text, and for auth Google's consent in this machine's
// browser, its redirect answered here and relayed (relay.go).

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/remote"
)

// remoteEnv is what `pneu account` on a client touches; tests swap it.
type remoteEnv struct {
	target string
	ssh    string // the ssh binary
	// listen binds the consent relay: lieer's callback port on both
	// loopbacks here (listenRelay).
	listen func() ([]net.Listener, error)
	open   func(string) error // the consent URL's opener (openURL)
	stdout io.Writer
	stderr io.Writer
}

// newRemoteEnv builds the real one; a variable so tests can replace it.
var newRemoteEnv = func(target string) *remoteEnv {
	return &remoteEnv{target: target, ssh: "ssh",
		listen: func() ([]net.Listener, error) { return listenRelay(gmi.AuthPort) },
		open:   openURL, stdout: os.Stdout, stderr: os.Stderr}
}

// argv is ssh's arguments for verb: the fixed options, "--", the target as
// its own argument, and the verb's fixed remote command.
func (e *remoteEnv) argv(verb remote.Verb) ([]string, error) {
	cmd, ok := remote.Command(verb)
	if !ok {
		return nil, fmt.Errorf("no remote command for %q", verb)
	}
	// Every verb, consent included, with pairing's options: no forwarding
	// of any kind, whatever the user's ssh config says (Y1).
	return append(append(append([]string{}, sshOptions...), "--", e.target), cmd), nil
}

// shownPlain is what shown leaves unquoted: ShellWord's set plus '=',
// '/' and ',', so options read as typed.
var shownPlain = regexp.MustCompile(`^[A-Za-z0-9@._:=/,-]+$`)

// shown is argv as a command line for the user to read (never run).
func shown(ssh string, argv []string) string {
	var words []string
	for _, a := range append([]string{ssh}, argv...) {
		if shownPlain.MatchString(a) {
			words = append(words, a)
		} else {
			words = append(words, config.ShellWord(a))
		}
	}
	return strings.Join(words, " ")
}

// run sends req to the server's `pneu account <verb> --stdin` and renders
// its events. Nothing is opened unless the event stream has been valid up
// to and including the consent URL, which must pass the same rule a local
// consent's does (gmi.ValidConsentURL). For auth, this machine answers
// Google's redirect itself (relay.go) and sends its query on, one line.
func (e *remoteEnv) run(verb remote.Verb, req any) error {
	if !config.ValidSSHTarget(e.target) {
		return fmt.Errorf("server.ssh %q isn't a usable ssh target", e.target)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(body) > remote.MaxRequest {
		return errors.New("the request is over its size cap")
	}
	argv, err := e.argv(verb)
	if err != nil {
		return err
	}
	out := &lockedWriter{w: e.stdout}
	var rl *relay
	if verb.Consent() {
		// Before ssh: both loopbacks, or no consent.
		lns, err := e.listen()
		if err != nil {
			return err
		}
		rl = startRelay(lns)
		defer rl.close()
	}
	server := config.Plain(e.target, 64)
	fmt.Fprintf(out, "This runs on %s: %s\n", server, shown(e.ssh, argv))
	if rl != nil {
		fmt.Fprintf(out, "Google's answer comes back to localhost:%d here; this command takes it and passes it on.\n", gmi.AuthPort)
	}

	cmd := exec.Command(e.ssh, argv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	pr, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// The server's stderr is its text too: plain lines, bounded.
	se := &plainLines{w: e.stderr, prefix: server + ": ", max: 64 << 10}
	cmd.Stderr = se
	if err := cmd.Start(); err != nil {
		return err
	}
	stdin.Write(append(body, '\n'))
	finished := make(chan struct{})
	relayed := make(chan struct{})
	// A closing event ends the session's use (Z2): the relay goes and
	// stdin closes at once, and ssh gets lingerWait to finish before it's
	// killed.
	terminal := make(chan struct{})
	var termOnce sync.Once
	var lingered atomic.Bool
	onTerminal := func() {
		termOnce.Do(func() {
			close(terminal)
			if rl != nil {
				rl.close()
			}
			time.AfterFunc(lingerWait, func() {
				select {
				case <-finished:
				default:
					lingered.Store(true)
					cmd.Process.Kill()
				}
			})
		})
	}
	var expired atomic.Value // why the relay gave up, a string
	if rl == nil {
		stdin.Close()
		close(relayed)
	} else {
		// The relay waits relayStartWait for the consent URL, then
		// consentWait from that URL for Google's answer; stdin stays open
		// for the callback until then. Running out ends the session.
		go func() {
			defer close(relayed)
			defer stdin.Close()
			defer rl.close()
			start := time.NewTimer(relayStartWait)
			defer start.Stop()
			var consent <-chan time.Time
			armed := rl.armed
			abort := func(why string) {
				expired.Store(why)
				cmd.Process.Kill()
			}
			for {
				select {
				case <-armed:
					start.Stop()
					armed = nil
					t := time.NewTimer(consentWait)
					defer t.Stop()
					consent = t.C
				case <-start.C:
					abort(fmt.Sprintf("%s sent no consent URL within %v; stopped", server, relayStartWait))
					return
				case <-consent:
					abort(fmt.Sprintf("Google's answer didn't come within %v of the consent screen; stopped", consentWait))
					return
				case q := <-rl.got:
					rl.close()
					if _, err := stdin.Write(remote.CallbackLine(q)); err != nil {
						fmt.Fprintf(out, "Couldn't pass Google's answer to %s (%v).\n", server, err)
						return
					}
					fmt.Fprintf(out, "Google answered; passed it on to %s.\n", server)
					return
				case <-terminal:
					return
				case <-finished:
					return
				}
			}
		}()
	}
	last, perr := e.events(pr, out, rl, onTerminal)
	if perr != nil {
		// A broken stream ends the session: whatever the server says next
		// isn't read, let alone acted on.
		cmd.Process.Kill()
	}
	io.Copy(io.Discard, pr)
	close(finished)
	<-relayed
	werr := cmd.Wait()
	se.flush()
	if why, _ := expired.Load().(string); why != "" && last.Kind == "" && perr == nil {
		return errors.New(why)
	}
	if lingered.Load() && last.Kind == remote.Result && perr == nil {
		fmt.Fprintf(e.stderr, "%s: (its session didn't end after the result; closed it)\n", server)
		return nil
	}
	switch {
	case perr != nil:
		return fmt.Errorf("%s's answer won't do (%v); stopped", server, perr)
	case last.Kind == remote.Error:
		return fmt.Errorf("on %s: %s", server, last.Text)
	case last.Kind == remote.Result && werr == nil:
		return nil
	case last.Kind == remote.Result:
		return fmt.Errorf("%s said it was done, but ssh failed (%v)", server, werr)
	}
	var ee *exec.ExitError
	if errors.As(werr, &ee) && ee.ExitCode() == notFound {
		return fmt.Errorf("the server's shell couldn't find pneu (exit 127): build it into ~/.local/bin on %s (INSTALL.md step 2)", server)
	}
	if errors.As(werr, &ee) && ee.ExitCode() == 255 {
		return fmt.Errorf("ssh to %s failed (exit 255); its message, if any, is above", server)
	}
	return fmt.Errorf("%s's pneu ended without a result (%v). If it's older than this one, update it (Update %s in the bar menu)", server, werr, server)
}

// The client's bounds on a session (Z2): how long ssh may run on after
// the closing event, and how long the relay waits for the consent URL
// before its own consentWait starts.
var (
	lingerWait     = 5 * time.Second
	relayStartWait = 3 * time.Minute
)

// lockedWriter serializes writes: the event reader and the relay both
// print.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// events reads and renders the stream to its end, and returns the closing
// event (Kind "" if none came). A line over remote.MaxLine, a session over
// its bounds, any event that doesn't parse, a consent URL without a relay
// or a state, a second one, or anything after the closing event is an
// error.
func (e *remoteEnv) events(r io.Reader, out io.Writer, rl *relay, onTerminal func()) (remote.Event, error) {
	br := bufio.NewReaderSize(r, 4096)
	var last remote.Event
	total, n, opened := 0, 0, false
	for {
		line, err := remote.ReadLine(br, remote.MaxLine)
		if err == io.EOF {
			return last, nil
		}
		if err != nil {
			return last, err
		}
		total += len(line) + 1
		n++
		switch {
		case total > remote.MaxStream:
			return last, fmt.Errorf("over %d bytes", remote.MaxStream)
		case n > remote.MaxEvents:
			return last, fmt.Errorf("over %d events", remote.MaxEvents)
		case last.Kind != "":
			return last, errors.New("data after its last word")
		}
		ev, err := remote.ParseEvent(line)
		if err != nil {
			return last, err
		}
		switch ev.Kind {
		case remote.Progress:
			fmt.Fprintf(out, "  %s\n", ev.Text)
		case remote.Waiting:
		case remote.Consent:
			if opened {
				return last, errors.New("a second consent URL")
			}
			if rl == nil {
				return last, errors.New("a consent URL from a command that runs no consent")
			}
			state, ok := remote.ConsentState(ev.URL)
			if !ok {
				return last, errors.New("the consent URL carries no state")
			}
			opened = true
			rl.arm(state)
			fmt.Fprintf(out, "Opening Google's consent screen in this machine's browser:\n  %s\n", ev.URL)
			if err := e.open(ev.URL); err != nil {
				fmt.Fprintf(out, "Couldn't open a browser (%v): open the URL above yourself, here.\n", err)
			}
		case remote.Result:
			last = ev
			if ev.Text != "" {
				fmt.Fprintln(out, ev.Text)
			}
			onTerminal()
		case remote.Error:
			last = ev
			onTerminal()
		}
	}
}

// plainLines writes each line it's given as plain text with a prefix, up
// to max bytes in all; then one note, and nothing more.
type plainLines struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	max    int
	n      int
	line   []byte
	over   bool
}

func (p *plainLines) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range b {
		if c == '\n' {
			p.emit()
			continue
		}
		if len(p.line) < 1024 {
			p.line = append(p.line, c)
		}
	}
	return len(b), nil
}

func (p *plainLines) emit() {
	text := config.Plain(string(p.line), 500)
	p.line = p.line[:0]
	if text == "" || p.over {
		return
	}
	p.n += len(text)
	if p.n > p.max {
		p.over = true
		fmt.Fprintf(p.w, "%s(more on stderr, not shown)\n", p.prefix)
		return
	}
	fmt.Fprintf(p.w, "%s%s\n", p.prefix, text)
}

func (p *plainLines) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.line) > 0 {
		p.emit()
	}
}
