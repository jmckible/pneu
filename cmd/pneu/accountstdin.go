package main

// The server's half of `pneu account` from a client (docs/client.md, "As
// built: step 8"): `pneu account <verb> --stdin` reads one request line on
// stdin and answers with line-delimited events on stdout (internal/remote).
// For auth, stdin stays open for at most one more line: the consent
// callback the client took from Google's redirect, replayed here to lieer.
// push and push-init (push.go, `pneu push init --stdin`) take the same
// line, checked here against push's own consent and never replayed.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/remote"
)

// acctOut is where an account command's words go: this terminal, or a
// client's event stream. consent runs `gmi auth` (the caller holds the
// account's lock) and gets its consent URL opened: here by xdg-open,
// there by the client. ctx ends when nobody is left to answer (a remote
// client gone); the terminal's never does.
type acctOut interface {
	say(format string, a ...any)
	consent(gmiDir, nmConfig string, env []string, args ...string) error
	ctx() context.Context
}

// termOut is the terminal's.
type termOut struct{}

func (termOut) say(format string, a ...any) { fmt.Printf(format+"\n", a...) }

func (termOut) consent(gmiDir, nmConfig string, env []string, args ...string) error {
	_, err := gmiLocked(context.Background(), gmiDir, nmConfig, &consentOpener{term: os.Stdout, open: openURL}, env, args...)
	return err
}

func (termOut) ctx() context.Context { return context.Background() }

// Remote limits: lieer waits on Google with no timeout of its own, and
// nobody may be at the other end any more, so the server gives up on
// consent after consentWait (the client's relay too), and sends a
// heartbeat every heartbeat for the whole command: a write that fails
// means the client is gone, and whatever is running stops.
var (
	consentWait = 10 * time.Minute
	heartbeat   = 15 * time.Second
)

// lieerCallback is where lieer's consent server listens on this machine:
// run_local_server binds "localhost" with wsgiref's AF_INET server.
var lieerCallback = "127.0.0.1:" + strconv.Itoa(gmi.AuthPort)

// eventOut is a client's event stream. A write that fails, a signal, or
// the client's stdin closing before its callback cancels c.
type eventOut struct {
	mu     sync.Mutex
	w      io.Writer
	n      int
	failed bool
	c      context.Context
	cancel context.CancelFunc
	// callback delivers the client's callback line, parsed; cbErr says
	// why one was refused.
	callback chan url.Values
	cbErr    error
}

func (o *eventOut) ctx() context.Context { return o.c }

// emit writes one event; past the stream's bounds it writes nothing more
// (the client would refuse it).
func (o *eventOut) emit(e remote.Event) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed {
		return errors.New("the client is gone")
	}
	if o.n >= remote.MaxEvents-1 && e.Kind != remote.Result && e.Kind != remote.Error {
		return nil // keep room for the last word
	}
	o.n++
	if _, err := o.w.Write(e.Line()); err != nil {
		o.failed = true
		o.cancel()
		return err
	}
	return nil
}

// say sends each line as its own progress event: an event's text is one
// plain line.
func (o *eventOut) say(format string, a ...any) {
	for _, line := range strings.Split(fmt.Sprintf(format, a...), "\n") {
		if strings.TrimSpace(line) != "" {
			o.emit(remote.Event{Kind: remote.Progress, Text: line})
		}
	}
}

// fail is the closing error event; any lines past the first go before it
// as progress.
func (o *eventOut) fail(err error) {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) != "" {
			o.emit(remote.Event{Kind: remote.Progress, Text: l})
		}
	}
	o.emit(remote.Event{Kind: remote.Error, Text: lines[0]})
}

// consent runs `gmi auth` with the consent URL sent as an event (never
// opened here: consentOpen "print") and lieer's other lines as progress.
// The client's callback, when it comes after the URL, is replayed to
// lieer. The wait's end, a client gone, or a callback refused interrupts
// lieer's whole process group.
func (o *eventOut) consent(gmiDir, nmConfig string, env []string, args ...string) error {
	bin, err := exec.LookPath("gmi")
	if err != nil {
		return errors.New("gmi (lieer) not found on PATH")
	}
	ctx, cancel := context.WithTimeout(o.c, consentWait)
	defer cancel()
	cmd := exec.Command(bin, args...)
	cmd.Dir = gmiDir
	cmd.Env = gmiEnvFor(nmConfig, env)
	var sent atomic.Bool // the consent URL is out: a callback may follow
	w := &consentOpener{
		lines: func(l string) { o.emit(remote.Event{Kind: remote.Progress, Text: l}) },
		open: func(u string) error {
			err := o.emit(remote.Event{Kind: remote.Consent, URL: u})
			sent.Store(true)
			return err
		},
	}
	cmd.Stdout, cmd.Stderr = w, w
	g, err := startGroup(cmd)
	if err != nil {
		return err
	}
	var relayErr error
wait:
	for {
		select {
		case err = <-g.exited:
			break wait
		case <-ctx.Done():
			err = g.stop()
			break wait
		case q := <-o.callback:
			if !sent.Load() {
				relayErr = errors.New("a callback came before the consent URL")
				cancel()
				continue
			}
			if rerr := replayCallback(ctx, q); rerr != nil {
				relayErr = fmt.Errorf("passing Google's answer to lieer: %w", rerr)
				cancel()
			}
		}
	}
	// The lock is the caller's until the whole group is gone.
	if rerr := g.reap(); rerr != nil && err == nil {
		err = rerr
	}
	w.flush()
	o.mu.Lock()
	cbErr := o.cbErr
	o.mu.Unlock()
	switch {
	case relayErr != nil:
		return relayErr
	case err == nil:
		return nil
	case cbErr != nil:
		return cbErr
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("consent wasn't given within %v", consentWait)
	case o.c.Err() != nil:
		return errors.New("the client went away")
	}
	return err
}

// replayCallback is Google's redirect as lieer would have had it: GET / on
// lieer's loopback with the callback's query, rebuilt from its parsed,
// allowlisted keys. lieer's server answers one request and ends.
func replayCallback(ctx context.Context, q url.Values) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+lieerCallback+"/?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Host = "localhost:" + strconv.Itoa(gmi.AuthPort) // as the browser's redirect names it
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{}).DialContext, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return nil
}

// gmiEnvFor is gmiLocked's environment for a run.
func gmiEnvFor(nmConfig string, env []string) []string {
	return withEnv(os.Environ(), append(append([]string{"NOTMUCH_CONFIG=" + nmConfig}, gmi.PythonEnv...), env...)...)
}

// reported is an error already sent as an error event: main exits 1
// without logging it again.
type reported struct{ error }

// accountStdin is `pneu account <verb> --stdin`: one request, the verb's
// work, its words as events, and a closing result or error event.
func accountStdin(verb remote.Verb, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A client that hangs up turns the next write into EPIPE (rather than a
	// SIGPIPE that kills pneu and leaves lieer waiting), and a hangup or
	// interrupt ends whatever is running.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGPIPE, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	o := &eventOut{w: out, c: ctx, cancel: cancel, callback: make(chan url.Values, 1)}
	// The heartbeat runs for the whole command: lock waits, consent and
	// the token check alike.
	stop := make(chan struct{})
	beat := make(chan struct{})
	go func() {
		defer close(beat)
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if o.emit(remote.Event{Kind: remote.Waiting}) != nil {
					return
				}
			}
		}
	}()
	err := runStdin(verb, bufio.NewReaderSize(in, 4096), o)
	close(stop)
	<-beat
	if err != nil {
		o.fail(err)
		return reported{err}
	}
	o.emit(remote.Event{Kind: remote.Result})
	return nil
}

func runStdin(verb remote.Verb, in *bufio.Reader, o *eventOut) error {
	if _, ok := remote.Command(verb); !ok {
		return fmt.Errorf("pneu account %s has no --stdin form", config.Plain(string(verb), 32))
	}
	line, err := remote.ReadLine(in, remote.MaxRequest)
	switch {
	case errors.Is(err, remote.ErrLongLine):
		return fmt.Errorf("request over %d bytes", remote.MaxRequest)
	case err != nil:
		return fmt.Errorf("request: %w", err)
	}
	// add and status take nothing after the request; auth's stdin stays
	// open for its callback.
	end := func() error {
		if _, err := in.ReadByte(); err != io.EOF {
			return errors.New("request: data after the request")
		}
		return nil
	}
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	if target, ok, err := clientOf(cfgPath); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("this machine is a client of %s: --stdin is its server's side", target)
	}
	switch verb {
	case remote.Add:
		q, err := remote.ParseAdd(line)
		if err == nil {
			err = end()
		}
		if err != nil {
			return err
		}
		p := addParams{name: q.Name, address: q.Address, fullName: q.FullName}
		if q.ClientSecret != "" {
			p.secret, p.secretFrom = []byte(q.ClientSecret), "the OAuth client sent"
		}
		return doAdd(cfgPath, p, o)
	case remote.Auth:
		q, err := remote.ParseAuth(line)
		if err != nil {
			return err
		}
		go o.readCallback(in)
		return doAuth(cfgPath, q.Name, q.Force, o)
	case remote.Status:
		q, err := remote.ParseStatus(line)
		if err == nil {
			err = end()
		}
		if err != nil {
			return err
		}
		var names []string
		if q.Name != "" {
			names = []string{q.Name}
		}
		return doStatus(cfgPath, names, o)
	case remote.Push:
		q, err := remote.ParsePush(line)
		if err != nil {
			return err
		}
		go o.readCallback(in)
		e, err := newPushEnv(cfgPath, o)
		if err != nil {
			return err
		}
		return e.on(q.Name, q.Reconsent)
	case remote.PushOff:
		q, err := remote.ParsePushOff(line)
		if err == nil {
			err = end()
		}
		if err != nil {
			return err
		}
		e, err := newPushEnv(cfgPath, o)
		if err != nil {
			return err
		}
		return e.off(q.Name)
	case remote.PushInit:
		q, err := remote.ParsePushInit(line)
		if err != nil {
			return err
		}
		go o.readCallback(in)
		e, err := newPushEnv(cfgPath, o)
		if err != nil {
			return err
		}
		p := initParams{project: q.Project, replace: q.Replace, reconsent: q.Reconsent}
		if q.ClientSecret != "" {
			p.client = []byte(q.ClientSecret)
		}
		return e.init(p)
	}
	return errors.New("unreachable")
}

// pushConsent is push's consent from a client (docs/push.md D3, K8): the
// consent URL goes out as an event, the client's relay answers Google's
// redirect, and its query comes back as one line on stdin (readCallback).
// That line is only a candidate: the caller checks its state against the
// consent's own, here, and exchanges only its code. Nothing listens on
// this machine's callback port. stdin's end before the line cancels the
// command (readCallback); its end after the line is the client's normal
// close.
func (o *eventOut) pushConsent(c *google.Consent) (url.Values, error) {
	u := c.URL()
	if !gmi.ValidConsentURL(u) {
		return nil, errors.New("the consent URL isn't Google's; not sending it")
	}
	if err := o.emit(remote.Event{Kind: remote.Consent, URL: u}); err != nil {
		return nil, errors.New("the client went away")
	}
	t := time.NewTimer(consentWait)
	defer t.Stop()
	select {
	case q := <-o.callback:
		return q, nil
	case <-o.c.Done():
		o.mu.Lock()
		cbErr := o.cbErr
		o.mu.Unlock()
		if cbErr != nil {
			return nil, cbErr
		}
		return nil, errors.New("the client went away before Google's answer; nothing changed")
	case <-t.C:
		return nil, fmt.Errorf("consent wasn't given within %v; nothing changed", consentWait)
	}
}

// readCallback reads a consent's one more line (auth's, push's). The
// client keeps stdin open until it has sent it, so an end before then
// means it's gone; a line that isn't a callback (remote.ParseCallbackLine)
// ends the command.
func (o *eventOut) readCallback(in *bufio.Reader) {
	line, err := remote.ReadLine(in, remote.MaxCallback+64)
	if err == nil {
		var q url.Values
		if q, err = remote.ParseCallbackLine(line); err == nil {
			o.callback <- q
			return
		}
	}
	if !errors.Is(err, io.EOF) {
		o.mu.Lock()
		o.cbErr = fmt.Errorf("the client's callback: %w", err)
		o.mu.Unlock()
	}
	o.cancel()
}
