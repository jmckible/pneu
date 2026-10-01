package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/remote"
)

// stdinRun runs `pneu account <verb> --stdin` on req and parses every
// line it wrote as a client would. auth's stdin stays open, as a client
// keeps it for the callback.
func stdinRun(t *testing.T, verb, req string) ([]remote.Event, error) {
	t.Helper()
	var out bytes.Buffer
	var in io.Reader = strings.NewReader(req)
	if verb == "auth" {
		pr, pw := io.Pipe()
		go pw.Write([]byte(req + "\n"))
		defer pw.Close()
		in = pr
	}
	err := accountStdin(remote.Verb(verb), in, &out)
	return parseAll(t, out.Bytes()), err
}

func parseAll(t *testing.T, b []byte) []remote.Event {
	t.Helper()
	var evs []remote.Event
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		e, err := remote.ParseEvent([]byte(line))
		if err != nil {
			t.Fatalf("the server wrote a line its client refuses: %q: %v", line, err)
		}
		evs = append(evs, e)
	}
	return evs
}

func lastEvent(evs []remote.Event) remote.Event {
	if len(evs) == 0 {
		return remote.Event{}
	}
	return evs[len(evs)-1]
}

func texts(evs []remote.Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Kind + ":" + e.Text + e.URL + "\n")
	}
	return b.String()
}

// The server's half end to end: add with an OAuth client sent as JSON,
// status, and auth whose consent URL comes back as an event and is never
// opened on the server (consentOpen "print").
func TestAccountStdin(t *testing.T) {
	home := sandbox(t)
	marker := filepath.Join(home, "xdg-open-ran")
	bin := filepath.Dir(must(exec.LookPath("gmi")))
	os.WriteFile(filepath.Join(bin, "xdg-open"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755)

	clean, _ := gmi.CleanClientSecret([]byte(clientJSON))
	req := `{"name":"work","address":"me@work.example","fullName":"Pat Doe","clientSecret":` + strconv.Quote(string(clean)) + `}`
	evs, err := stdinRun(t, "add", req)
	if err != nil || lastEvent(evs).Kind != remote.Result {
		t.Fatalf("add: %v\n%s", err, texts(evs))
	}
	if fi, err := os.Stat(filepath.Join(home, ".config", "pneu", "work", "client_secret.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("client secret: %v %v", fi, err)
	}
	if !strings.Contains(texts(evs), "progress:Next: pneu account auth work") {
		t.Errorf("add said:\n%s", texts(evs))
	}

	evs, err = stdinRun(t, "auth", `{"name":"work","force":false,"consentOpen":"print"}`)
	if err != nil || lastEvent(evs).Kind != remote.Result {
		t.Fatalf("auth: %v\n%s", err, texts(evs))
	}
	var urls []string
	for _, e := range evs {
		if e.Kind == remote.Consent {
			urls = append(urls, e.URL)
		}
	}
	if len(urls) != 1 || !gmi.ValidConsentURL(urls[0]) {
		t.Errorf("consent URLs %q", urls)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("xdg-open ran on the server")
	}
	if gmi.FileState(filepath.Join(home, "mail", "work", "gmail")) != gmi.StateNeedsPull {
		t.Error("auth didn't authorize")
	}

	evs, err = stdinRun(t, "status", `{"name":"work"}`)
	if err != nil || lastEvent(evs).Kind != remote.Result || !strings.Contains(texts(evs), "progress:work") {
		t.Errorf("status: %v\n%s", err, texts(evs))
	}
	evs, err = stdinRun(t, "status", `{"name":"nope"}`)
	if !errors.As(err, new(reported)) || lastEvent(evs) != (remote.Event{Kind: remote.Error, Text: "no such account"}) {
		t.Errorf("status of no account: %v\n%s", err, texts(evs))
	}
}

// Requests that aren't exactly a verb's shape are refused before anything
// is done, with one error event.
func TestAccountStdinStrict(t *testing.T) {
	home := sandbox(t)
	cfg := filepath.Join(home, ".config", "pneu", "config.json")
	ok := `{"name":"work","address":"me@work.example","fullName":"","clientSecret":""}`
	for name, req := range map[string]string{
		"oversize":     `{"name":"work","address":"me@work.example","fullName":"","clientSecret":"` + strings.Repeat("a", remote.MaxRequest) + `"}`,
		"unknown key":  strings.Replace(ok, `"fullName"`, `"fullname":"","fullName"`, 1),
		"duplicate":    strings.Replace(ok, `"name":"work"`, `"name":"work","name":"home"`, 1),
		"case variant": strings.Replace(ok, `"name"`, `"Name"`, 1),
		"trailing":     ok + `{}`,
		"trailing txt": ok + "\nrm -rf ~",
		"missing":      `{"name":"work","address":"me@work.example","fullName":""}`,
		"not json":     `name=work`,
		"bad name":     strings.Replace(ok, `"work"`, `"../x"`, 1),
		"bad secret":   strings.Replace(ok, `"clientSecret":""`, `"clientSecret":"{\"web\":{}}"`, 1),
		"newline name": strings.Replace(ok, `"fullName":""`, `"fullName":"x\n[database]\npath=/"`, 1),
	} {
		evs, err := stdinRun(t, "add", req)
		if !errors.As(err, new(reported)) || len(evs) == 0 || lastEvent(evs).Kind != remote.Error {
			t.Errorf("%s: %v\n%s", name, err, texts(evs))
		}
		if _, err := os.Stat(cfg); err == nil {
			t.Fatalf("%s: a refused request wrote the config", name)
		}
	}
	for verb, req := range map[string]string{
		"auth":   `{"name":"work","force":false,"consentOpen":"xdg-open"}`,
		"gmi":    `{}`,
		"peer":   `{}`,
		"status": `{"name":"work","extra":1}`,
	} {
		if evs, err := stdinRun(t, verb, req); !errors.As(err, new(reported)) || lastEvent(evs).Kind != remote.Error {
			t.Errorf("%s: %v\n%s", verb, err, texts(evs))
		}
	}
	// A client's config refuses the server's side outright.
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	os.WriteFile(cfg, []byte(`{"port":7317,"server":{"ssh":"dell","node":"nSERVER1CNTRL","port":7320}}`), 0o600)
	evs, err := stdinRun(t, "status", `{"name":""}`)
	if !errors.As(err, new(reported)) || !strings.Contains(lastEvent(evs).Text, "client of dell") {
		t.Errorf("on a client: %v\n%s", err, texts(evs))
	}
}

// A client that goes away while lieer waits on consent: the heartbeat's
// write fails, or its stdin ends before a callback came, and lieer's whole
// group is interrupted, so the account's lock and port 8080 aren't held
// for the full wait. lieer here is a stand-in that waits without a port,
// so no other test's 8080 gets in the way.
func TestAccountStdinClientGone(t *testing.T) {
	home := sandbox(t)
	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(clientJSON), 0o644)
	if _, err := capture(t, func() error { return account([]string{"add", "work", "me@work.example", "--client-secret", secret}) }); err != nil {
		t.Fatal(err)
	}
	pids := filepath.Join(home, "pids")
	bin := filepath.Dir(must(exec.LookPath("gmi")))
	os.Remove(filepath.Join(bin, "gmi"))
	// lieer, and a descendant of it that ignores SIGINT (Z1).
	kidFile := filepath.Join(home, "kid")
	os.WriteFile(filepath.Join(bin, "gmi"), []byte(`#!/bin/sh
echo $$ >> '`+pids+`'
sh -c 'trap "" INT; echo $$ > "$0.tmp"; mv "$0.tmp" "$0"; exec sleep 60' '`+kidFile+`' &
while [ ! -s '`+kidFile+`' ]; do sleep 0.01; done
echo "Please visit this URL to authorize this application: https://accounts.google.com/o/oauth2/auth?state=abc"
trap 'exit 1' INT
while :; do sleep 0.05; done
`), 0o755)
	oldBeat, oldWait, oldGrace := heartbeat, consentWait, groupGrace
	// The wait's own limit is far off, so only the client's going ends it.
	heartbeat, consentWait, groupGrace = 20*time.Millisecond, 8*time.Second, 300*time.Millisecond
	t.Cleanup(func() { heartbeat, consentWait, groupGrace = oldBeat, oldWait, oldGrace })
	req := `{"name":"work","force":false,"consentOpen":"print"}` + "\n"
	gmiDir := filepath.Join(home, "mail", "work", "gmail")
	for _, how := range []string{"heartbeat", "stdin"} {
		os.Remove(pids)
		pr, pw := io.Pipe()
		go pw.Write([]byte(req))
		w := &goneAfter{}
		if how == "stdin" {
			w.onConsent = func() { pw.Close() }
			w.keep = true
		}
		start := time.Now()
		err := accountStdin(remote.Auth, pr, w)
		pw.Close()
		if !errors.As(err, new(reported)) || !strings.Contains(err.Error(), "went away") {
			t.Errorf("%s: err = %v\n%s", how, err, w.String())
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: took %v", how, d)
		}
		b, _ := os.ReadFile(pids)
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid == 0 || syscall.Kill(pid, 0) == nil {
			t.Errorf("%s: lieer (pid %d) still running", how, pid)
		}
		b, _ = os.ReadFile(kidFile)
		if kid, _ := strconv.Atoi(strings.TrimSpace(string(b))); kid == 0 || syscall.Kill(kid, 0) == nil {
			t.Errorf("%s: lieer's descendant (pid %d) still running", how, kid)
		}
		os.Remove(kidFile)
		release, err := gmi.Lock(gmi.DefaultLockPath(gmiDir), 100*time.Millisecond, nil)
		if err != nil {
			t.Errorf("%s: the account's lock is still held: %v", how, err)
		} else {
			release()
		}
	}
}

// goneAfter takes writes until the consent URL's, then fails as a closed
// pipe would; keep: it goes on taking them, and onConsent runs instead.
type goneAfter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	gone      bool
	keep      bool
	onConsent func()
}

func (g *goneAfter) Write(b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gone && !g.keep {
		return 0, io.ErrClosedPipe
	}
	if !g.gone && bytes.Contains(b, []byte(`"consent-url"`)) {
		g.gone = true
		if g.onConsent != nil {
			g.onConsent()
		}
	}
	return g.buf.Write(b)
}

func (g *goneAfter) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

// fakeConsentGmi is a gmi that announces a consent URL (after delay) and
// waits for done to exist, as lieer waits on its callback.
func fakeConsentGmi(t *testing.T, delay string) (done string) {
	t.Helper()
	bin := t.TempDir()
	done = filepath.Join(t.TempDir(), "done")
	os.WriteFile(filepath.Join(bin, "gmi"), []byte(`#!/bin/sh
sleep `+delay+`
echo "Please visit this URL to authorize this application: https://accounts.google.com/o/oauth2/auth?state=abc&redirect_uri=http%3A%2F%2Flocalhost%3A8080%2F"
while [ ! -e '`+done+`' ]; do sleep 0.02; done
`), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return done
}

// fakeLieer is lieer's consent server: it records the one request it gets
// and lets the fake gmi finish.
func fakeLieer(t *testing.T, done string) (got func() (string, string)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var uri, host string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uri, host = r.RequestURI, r.Host
		mu.Unlock()
		io.WriteString(w, "The authentication flow has completed.")
		os.WriteFile(done, nil, 0o600)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	old := lieerCallback
	lieerCallback = ln.Addr().String()
	t.Cleanup(func() { lieerCallback = old })
	return func() (string, string) { mu.Lock(); defer mu.Unlock(); return uri, host }
}

// consentWith runs eventOut.consent with the client's stdin fed after the
// consent URL is out (or at once): the callback line, or anything else.
func consentWith(t *testing.T, line string, atOnce bool) (error, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	feed := func() { go pw.Write([]byte(line)) }
	w := &goneAfter{keep: true}
	if !atOnce {
		w.onConsent = feed
	}
	o := &eventOut{w: w, c: ctx, cancel: cancel, callback: make(chan url.Values, 1)}
	go o.readCallback(bufio.NewReader(pr))
	if atOnce {
		feed()
	}
	err := o.consent(t.TempDir(), "", nil, "auth", "-c", "x")
	return err, w.String()
}

// The callback line is replayed to lieer's loopback as GET /?<query>
// rebuilt from its allowlisted keys, Host as the browser's redirect had
// it; anything else ends the consent and lieer with it.
func TestConsentReplay(t *testing.T) {
	done := fakeConsentGmi(t, "0")
	got := fakeLieer(t, done)
	raw := "state=abc&code=4%2F0Ad&scope=https%3A%2F%2Fmail.google.com%2F&authuser=0"
	err, out := consentWith(t, string(remote.CallbackLine(raw)), false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	q, _ := url.ParseQuery(raw)
	if uri, host := got(); uri != "/?"+q.Encode() || host != "localhost:8080" {
		t.Errorf("lieer got %q (Host %q)", uri, host)
	}
	if !strings.Contains(out, `"consent-url"`) {
		t.Errorf("no consent URL event:\n%s", out)
	}
}

func TestConsentReplayRefusals(t *testing.T) {
	for name, line := range map[string]string{
		"extra key":        `{"callback":"state=abc&code=c&next=https://evil.example"}` + "\n",
		"extra field":      `{"callback":"state=abc&code=c","x":""}` + "\n",
		"not json":         "state=abc&code=c\n",
		"no state":         `{"callback":"code=c"}` + "\n",
		"oversized":        `{"callback":"state=abc&code=` + strings.Repeat("a", remote.MaxCallback) + `"}` + "\n",
		"no newline, huge": strings.Repeat("a", 3*remote.MaxCallback),
	} {
		done := fakeConsentGmi(t, "0")
		got := fakeLieer(t, done)
		start := time.Now()
		err, out := consentWith(t, line, false)
		if err == nil || !strings.Contains(err.Error(), "callback") || time.Since(start) > 7*time.Second {
			t.Errorf("%s: %v (%v)\n%s", name, err, time.Since(start), out)
		}
		if uri, _ := got(); uri != "" {
			t.Errorf("%s: relayed %q", name, uri)
		}
	}
	// A callback before the consent URL isn't this consent's.
	done := fakeConsentGmi(t, "0.3")
	got := fakeLieer(t, done)
	if err, out := consentWith(t, string(remote.CallbackLine("state=abc&code=c")), true); err == nil || !strings.Contains(err.Error(), "before the consent URL") {
		t.Errorf("early callback: %v\n%s", err, out)
	}
	if uri, _ := got(); uri != "" {
		t.Errorf("early callback relayed %q", uri)
	}
}

// Y3: the token check (`gmi pull -t`) holds the account's lock, so it runs
// under the client's context in a group of its own, with its own bound: a
// client gone, or no answer in verifyWait, kills it and frees the lock.
func TestAccountStdinVerifyStalled(t *testing.T) {
	home := sandbox(t)
	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(clientJSON), 0o644)
	if _, err := capture(t, func() error { return account([]string{"add", "work", "me@work.example", "--client-secret", secret}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, func() error { return account([]string{"auth", "work"}) }); err != nil {
		t.Fatal(err)
	}
	// Now a gmi whose label listing never answers; it records its pid.
	pids := filepath.Join(home, "pids")
	bin := filepath.Dir(must(exec.LookPath("gmi")))
	// A new file, not a write over the stub: its old inode may still be
	// mapped (ETXTBSY) by a gmi that's just exiting.
	if err := os.Remove(filepath.Join(bin, "gmi")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gmi"), []byte("#!/bin/sh\necho $$ >> '"+pids+"'\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBeat, oldVerify := heartbeat, verifyWait
	t.Cleanup(func() { heartbeat, verifyWait = oldBeat, oldVerify })
	gmiDir := filepath.Join(home, "mail", "work", "gmail")
	req := `{"name":"work","force":false,"consentOpen":"print"}` + "\n"
	for _, c := range []struct {
		how    string
		verify time.Duration
		want   string
	}{
		{"client gone", time.Minute, "canceled"},
		{"no answer", 300 * time.Millisecond, "no answer from Gmail"},
	} {
		os.Remove(pids)
		heartbeat, verifyWait = 50*time.Millisecond, c.verify
		pr, pw := io.Pipe()
		go pw.Write([]byte(req))
		var out io.Writer = &bytes.Buffer{}
		if c.how == "client gone" {
			// Gone once the check has started: only the heartbeat can tell.
			out = &failWhen{pids: pids, log: &bytes.Buffer{}}
		}
		start := time.Now()
		err := accountStdin(remote.Auth, pr, out)
		pw.Close()
		if !errors.As(err, new(reported)) || !strings.Contains(err.Error(), c.want) || time.Since(start) > 10*time.Second {
			t.Errorf("%s: %v after %v\n%s", c.how, err, time.Since(start), fmt.Sprint(out.(fmt.Stringer)))
		}
		b, _ := os.ReadFile(pids)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid == 0 {
			t.Fatalf("%s: gmi never ran", c.how)
		}
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("%s: gmi pull -t (pid %d) still running", c.how, pid)
			syscall.Kill(pid, syscall.SIGKILL)
		}
		release, err := gmi.Lock(gmi.DefaultLockPath(gmiDir), 100*time.Millisecond, nil)
		if err != nil {
			t.Errorf("%s: the account's lock is still held: %v", c.how, err)
		} else {
			release()
		}
	}
}

// failWhen fails every write once the pids file exists: the client went
// away after gmi started.
type failWhen struct {
	pids string
	log  *bytes.Buffer
}

func (f *failWhen) String() string { return f.log.String() }

func (f *failWhen) Write(b []byte) (int, error) {
	if _, err := os.Stat(f.pids); err == nil {
		return 0, io.ErrClosedPipe
	}
	return f.log.Write(b)
}

// Z1: a descendant of gmi that ignores SIGINT outlives the leader; the
// group is killed whole, and the account's lock is free only once it's
// gone.
func TestGroupOutlivedByDescendant(t *testing.T) {
	home := sandbox(t)
	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(clientJSON), 0o644)
	if _, err := capture(t, func() error { return account([]string{"add", "work", "me@work.example", "--client-secret", secret}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, func() error { return account([]string{"auth", "work"}) }); err != nil {
		t.Fatal(err)
	}
	kidFile := filepath.Join(home, "kid")
	bin := filepath.Dir(must(exec.LookPath("gmi")))
	os.Remove(filepath.Join(bin, "gmi"))
	// The leader exits on SIGINT; its background child ignores it (and a
	// non-interactive sh starts it with SIGINT ignored anyway).
	os.WriteFile(filepath.Join(bin, "gmi"), []byte(`#!/bin/sh
sh -c 'trap "" INT; echo $$ > "$0.tmp"; mv "$0.tmp" "$0"; exec sleep 60' '`+kidFile+`' &
trap 'exit 1' INT
while :; do sleep 0.05; done
`), 0o755)
	oldBeat, oldGrace := heartbeat, groupGrace
	heartbeat, groupGrace = 50*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { heartbeat, groupGrace = oldBeat, oldGrace })

	// Another taker of the lock notes whether the descendant still lives
	// when it gets it.
	lockPath := gmi.DefaultLockPath(filepath.Join(home, "mail", "work", "gmail"))
	kidAlive := make(chan bool, 1)
	go func() {
		for {
			if _, err := os.Stat(kidFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		b, _ := os.ReadFile(kidFile)
		kid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		release, err := gmi.Lock(lockPath, 30*time.Second, nil)
		if err != nil {
			kidAlive <- true
			return
		}
		kidAlive <- syscall.Kill(kid, 0) == nil
		release()
	}()
	pr, pw := io.Pipe()
	go pw.Write([]byte(`{"name":"work","force":false,"consentOpen":"print"}` + "\n"))
	out := &failWhen{pids: kidFile, log: &bytes.Buffer{}} // gone once the kid runs
	err := accountStdin(remote.Auth, pr, out)
	pw.Close()
	if !errors.As(err, new(reported)) {
		t.Errorf("err = %v\n%s", err, out)
	}
	select {
	case alive := <-kidAlive:
		if alive {
			t.Error("the lock was free while gmi's descendant still ran")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lock never came free")
	}
	b, _ := os.ReadFile(kidFile)
	if kid, _ := strconv.Atoi(strings.TrimSpace(string(b))); kid == 0 || syscall.Kill(kid, 0) == nil {
		t.Errorf("descendant %d still running", kid)
		syscall.Kill(kid, syscall.SIGKILL)
	}
}
