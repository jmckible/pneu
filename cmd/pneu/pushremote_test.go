package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmckible/pneu/internal/push/state"
	"github.com/jmckible/pneu/internal/remote"
)

// remotePush runs `pneu account push|push-off --stdin` (or `pneu push init
// --stdin`) as the server's half, against pt's env: req as stdin's first
// line, then client, called on every event, plays the client (writing
// the callback line, closing stdin). It returns every event and the
// command's error.
func (pt *pushTest) remotePush(verb remote.Verb, req string, client func(ev remote.Event, stdin io.WriteCloser)) ([]remote.Event, error) {
	t := pt.t
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "pneu"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(pt.cfg)
	os.WriteFile(filepath.Join(home, "pneu", "config.json"), b, 0o600)
	t.Setenv("XDG_CONFIG_HOME", home)
	orig := newPushEnv
	newPushEnv = func(cfgPath string, out pushOut) (*pushEnv, error) {
		if cfgPath != filepath.Join(home, "pneu", "config.json") {
			t.Errorf("config %s", cfgPath)
		}
		e := *pt.e
		e.out = out
		return &e, nil
	}
	defer func() { newPushEnv = orig }()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := accountStdin(verb, inR, outW)
		outW.Close()
		done <- err
	}()
	inW.Write([]byte(req + "\n"))
	if !verb.Consent() {
		inW.Close() // as the client does: nothing more comes
	}
	var evs []remote.Event
	br := bufio.NewReader(outR)
	for {
		line, err := remote.ReadLine(br, remote.MaxLine)
		if err != nil {
			break
		}
		ev, err := remote.ParseEvent(line)
		if err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		evs = append(evs, ev)
		if client != nil {
			client(ev, inW)
		}
	}
	inW.Close()
	return evs, <-done
}

func eventText(evs []remote.Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Kind + ": " + e.Text + e.URL + "\n")
	}
	return b.String()
}

// From a client: the consent URL as an event, the callback back on stdin
// (then stdin closed, as the client does: a normal end), the state
// checked here, the result.
func TestPushRemoteOn(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	evs, err := pt.remotePush(remote.Push, `{"name":"personal","reconsent":false}`, func(ev remote.Event, stdin io.WriteCloser) {
		if ev.Kind == remote.Consent {
			q, err := pt.f.Approve(ev.URL, mailAddr)
			if err != nil {
				t.Error(err)
			}
			stdin.Write(remote.CallbackLine(q))
			stdin.Close()
		}
	})
	if err != nil || lastEvent(evs).Kind != remote.Result {
		t.Fatalf("%v\n%s", err, eventText(evs))
	}
	text := eventText(evs)
	for _, want := range []string{"Sign in as me@example.com", "topic pneu-personal: created", "pneu isn't running; instant mail for personal starts when it does."} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if pt.load().Accounts["personal"].State != state.On {
		t.Fatal("not committed")
	}
	for _, s := range []string{"Google's words", "ya29.", "1//"} {
		if strings.Contains(text, s) {
			t.Errorf("events carry %q", s)
		}
	}
	// Again: the stored grant works, so no consent URL; stdin stays open
	// until the result, as the client keeps it.
	evs, err = pt.remotePush(remote.Push, `{"name":"personal","reconsent":false}`, nil)
	if err != nil || strings.Contains(eventText(evs), "consent-url") {
		t.Fatalf("again: %v\n%s", err, eventText(evs))
	}
}

// stdin's end before the callback cancels the consent; a callback with
// another consent's state, a malformed line or a denial ends it; none
// commits anything.
func TestPushRemoteConsentRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		play func(pt *pushTest, u string, stdin io.WriteCloser)
		want string
	}{
		"eof before callback": {func(pt *pushTest, u string, stdin io.WriteCloser) { stdin.Close() },
			"the client went away before Google's answer; nothing changed"},
		"another state": {func(pt *pushTest, u string, stdin io.WriteCloser) {
			q, _ := pt.f.Approve(u, mailAddr)
			v, _ := url.ParseQuery(q)
			v.Set("state", v.Get("state")+"x")
			stdin.Write(remote.CallbackLine(v.Encode()))
		}, "the answer that came back isn't this consent's; nothing changed"},
		"malformed": {func(pt *pushTest, u string, stdin io.WriteCloser) {
			io.WriteString(stdin, `{"callback":"state=a&code=b","x":""}`+"\n")
		}, "the client's callback: callback line: unknown field"},
		"denied": {func(pt *pushTest, u string, stdin io.WriteCloser) {
			q, _ := pt.f.Deny(u)
			stdin.Write(remote.CallbackLine(q))
		}, "consent wasn't given"},
	} {
		t.Run(name, func(t *testing.T) {
			pt := newPushTest(t)
			pt.firstInit()
			evs, err := pt.remotePush(remote.Push, `{"name":"personal","reconsent":false}`, func(ev remote.Event, stdin io.WriteCloser) {
				if ev.Kind == remote.Consent {
					tc.play(pt, ev.URL, stdin)
				}
			})
			if err == nil || lastEvent(evs).Kind != remote.Error || !strings.Contains(lastEvent(evs).Text, tc.want) {
				t.Fatalf("%v\n%s", err, eventText(evs))
			}
			if len(pt.load().Accounts) != 0 || pt.f.HasTopic("pneu-personal") {
				t.Fatal("went on after a refused callback")
			}
		})
	}
}

// push-off from a client: the request, then the end of input; anything
// after the request is refused before anything runs.
func TestPushRemoteOff(t *testing.T) {
	pt := newPushTest(t)
	pt.firstInit()
	if err := pt.e.on("personal", false); err != nil {
		t.Fatal(err)
	}
	evs, err := pt.remotePush(remote.PushOff, `{"name":"personal"}`+"\n"+`{"callback":"x"}`, nil)
	if err == nil || !strings.Contains(lastEvent(evs).Text, "data after the request") {
		t.Fatalf("trailing data: %v\n%s", err, eventText(evs))
	}
	if pt.load().Accounts["personal"].State != state.On {
		t.Fatal("acted on a refused request")
	}
	evs, err = pt.remotePush(remote.PushOff, `{"name":"personal"}`, func(ev remote.Event, stdin io.WriteCloser) {})
	if err != nil || !strings.Contains(eventText(evs), "Instant mail is off for personal.") {
		t.Fatalf("%v\n%s", err, eventText(evs))
	}
	if _, ok := pt.load().Accounts["personal"]; ok {
		t.Fatal("not removed")
	}
}

// push-init from a client: the client JSON in the request (its three
// fields), the owner's consent through the client.
func TestPushRemoteInit(t *testing.T) {
	pt := newPushTest(t)
	c, _ := state.ParseClient(pushClientJSON(pt.f))
	req, _ := json.Marshal(remote.PushInitRequest{Project: pt.f.Project, ClientSecret: string(cleanPushClient(c))})
	evs, err := pt.remotePush(remote.PushInit, string(req), func(ev remote.Event, stdin io.WriteCloser) {
		if ev.Kind == remote.Consent {
			q, _ := pt.f.Approve(ev.URL, ownerAddr)
			stdin.Write(remote.CallbackLine(q))
			stdin.Close()
		}
	})
	if err != nil || !strings.Contains(eventText(evs), "Push is set up.") {
		t.Fatalf("%v\n%s", err, eventText(evs))
	}
	if s := pt.load(); s.Owner.Email != ownerAddr || s.Client != pt.f.ClientID {
		t.Fatalf("%+v", s.File)
	}
	// A bad request is refused before anything runs.
	for _, bad := range []string{
		`{"project":"x","clientSecret":"","replace":false,"reconsent":false}`,
		`{"project":"","clientSecret":"","replace":false}`,
		`{"project":"","clientSecret":"","replace":false,"reconsent":false,"reconsent":true}`,
		`{"project":"","clientSecret":"{}","replace":false,"reconsent":false}`,
		`{"project":"","clientSecret":"","replace":"no","reconsent":false}`,
	} {
		evs, err := pt.remotePush(remote.PushInit, bad, nil)
		if err == nil || !strings.Contains(lastEvent(evs).Text, "request") {
			t.Errorf("%s: %v %s", bad, err, eventText(evs))
		}
	}
	if pt.load().Generation != 1 {
		t.Fatal("a refused request committed")
	}
}

// The client's half: the fixed remote command per verb, pairing's ssh
// options, the request on stdin; the relay bound only for the verbs that
// may consent.
func TestPushRemoteClient(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"port":7317,"server":{"ssh":"server","node":"nSERVER1CNTRL","port":7320}}`), 0o600)
	for _, tc := range []struct {
		verb    string
		args    []string
		req     string
		relayed int
	}{
		{"push", []string{"push", "personal"}, `{"name":"personal","reconsent":false}`, 1},
		{"push", []string{"push", "--reconsent", "personal"}, `{"name":"personal","reconsent":true}`, 1},
		{"push-off", []string{"push", "personal", "--off"}, `{"name":"personal"}`, 0},
	} {
		f := newFakeAccountSSH(t, tc.verb, "server", lines(remote.Event{Kind: remote.Result}), "", 0)
		var r *testRemote
		orig := newRemoteEnv
		newRemoteEnv = func(target string) *remoteEnv { r = newTestRemote(t, f, target); return r.remoteEnv }
		err := account(append(tc.args, "--config", cfg))
		newRemoteEnv = orig
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, r.errb.String())
		}
		if f.stdin() != tc.req || r.listened != tc.relayed {
			t.Errorf("%v: request %q, relays %d", tc.args, f.stdin(), r.listened)
		}
	}
	// Refused here, before ssh.
	for _, args := range [][]string{{"push", "a;b"}, {"push", "personal", "--off", "--reconsent"}, {"push"}} {
		ran := false
		orig := newRemoteEnv
		newRemoteEnv = func(target string) *remoteEnv { ran = true; return orig(target) }
		if err := account(append(args, "--config", cfg)); err == nil || ran {
			t.Errorf("%q: %v, ssh ran %v", args, err, ran)
		}
		newRemoteEnv = orig
	}
}

// push init from a client: its own remote command, the client JSON read
// and checked here, only its three fields sent.
func TestPushInitRemoteClient(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"port":7317,"server":{"ssh":"server","node":"nSERVER1CNTRL","port":7320}}`), 0o600)
	dir := t.TempDir()
	want := "-T|-o|ForwardAgent=no|-o|ForwardX11=no|-o|ClearAllForwardings=yes|-o|ControlPath=none|-o|PermitLocalCommand=no|--|server|" + `PATH="$HOME/.local/bin:$PATH" exec pneu push init --stdin`
	script := `#!/bin/sh
got=""
for a in "$@"; do got="$got|$a"; done
got="${got#|}"
if [ "$got" != '` + want + `' ]; then echo "argv: $got" >&2; exit 99; fi
IFS= read -r req
printf '%s' "$req" > '` + dir + `/stdin'
printf '%s\n' '{"event":"result","text":""}'
`
	bin := filepath.Join(dir, "ssh")
	os.WriteFile(bin, []byte(script), 0o700)
	var r *testRemote
	orig := newRemoteEnv
	newRemoteEnv = func(target string) *remoteEnv {
		r = newTestRemote(t, acctSSH{bin: bin, dir: dir}, target)
		return r.remoteEnv
	}
	t.Cleanup(func() { newRemoteEnv = orig })
	secret := filepath.Join(dir, "client.json")
	os.WriteFile(secret, []byte(`{"installed":{"client_id":"123-abc.apps.googleusercontent.com","project_id":"pneu-push-9","client_secret":"GOCSPX-x","redirect_uris":["http://evil.example"]}}`), 0o600)
	if err := pushCmd([]string{"init", "--project", "pneu-push-9", "--client-secret", secret, "--config", cfg}); err != nil {
		t.Fatalf("%v\n%s", err, r.errb.String())
	}
	got, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	q, err := remote.ParsePushInit(got)
	if err != nil || q.Project != "pneu-push-9" || strings.Contains(string(got), "evil") || r.listened != 1 {
		t.Fatalf("request %s: %v, relays %d", got, err, r.listened)
	}
	c, _ := state.ParseClient([]byte(q.ClientSecret))
	if c.ID != "123-abc.apps.googleusercontent.com" || c.Secret != "GOCSPX-x" {
		t.Fatalf("client %v", c)
	}
	// Refused here: a client of another project, a bad project ID.
	for _, args := range [][]string{
		{"init", "--project", "pneu-push-8", "--client-secret", secret},
		{"init", "--project", "Bad_Project"},
		{"init", "extra"},
		{"nope"},
	} {
		os.Remove(filepath.Join(dir, "stdin"))
		if err := pushCmd(append(args, "--config", cfg)); err == nil {
			t.Errorf("%q accepted", args)
		}
		if _, err := os.Stat(filepath.Join(dir, "stdin")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%q: ssh ran", args)
		}
	}
}

// pneu account push-init --stdin isn't a way in.
func TestPushInitStdinOnlyUnderPush(t *testing.T) {
	if err := account([]string{"push-init", "--stdin"}); err == nil || !errors.As(err, new(usageError)) {
		t.Fatalf("%v", err)
	}
	if _, ok := remote.Command(remote.PushInit); !ok {
		t.Fatal("no command")
	}
}
