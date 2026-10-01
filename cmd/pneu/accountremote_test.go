package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/remote"
)

const consentURL = "https://accounts.google.com/o/oauth2/auth?client_id=1-x.apps.googleusercontent.com&redirect_uri=http%3A%2F%2Flocalhost%3A8080%2F&response_type=code&state=abc"

// wantArgv is the exact argv each verb's ssh must get: pairing's options
// (no forwarding of any kind, consent included), "--", the target, the
// verb's fixed remote command.
func wantArgv(verb, target string) string {
	return "-T|-o|ForwardAgent=no|-o|ForwardX11=no|-o|ClearAllForwardings=yes|-o|ControlPath=none|-o|PermitLocalCommand=no|--|" + target + "|" + `PATH="$HOME/.local/bin:$PATH" exec pneu account ` + verb + ` --stdin`
}

// acctSSH is an ssh stand-in: argv must be exactly verb's (else exit 99).
// It saves the request line, writes stderr's file to stderr, plays
// stdout's file; then, if stdout2's file isn't empty, reads one more line
// (the callback) into callback and plays stdout2; and exits with exit.
// Each run appends its first argument to the log.
type acctSSH struct {
	bin, dir string
}

func newFakeAccountSSH(t *testing.T, verb, target, stdout, stderr string, exit int) acctSSH {
	return newFakeAuthSSH(t, verb, target, stdout, "", stderr, exit)
}

func newFakeAuthSSH(t *testing.T, verb, target, stdout, stdout2, stderr string, exit int) acctSSH {
	t.Helper()
	dir := t.TempDir()
	f := acctSSH{bin: filepath.Join(dir, "ssh"), dir: dir}
	for name, body := range map[string]string{"stdout": stdout, "stdout2": stdout2, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
d='` + dir + `'
echo "$1" >> "$d/log"
got=""
for a in "$@"; do got="$got|$a"; done
got="${got#|}"
if [ "$got" != '` + wantArgv(verb, target) + `' ]; then echo "argv: $got" >&2; exit 99; fi
IFS= read -r req
printf '%s' "$req" > "$d/stdin"
cat "$d/stderr" >&2
cat "$d/stdout"
if [ -s "$d/stdout2" ]; then
  IFS= read -r cb
  printf '%s' "$cb" > "$d/callback"
  cat "$d/stdout2"
fi
exit ` + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(f.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f acctSSH) log() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "log"))
	return strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", ",")
}

func (f acctSSH) stdin() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "stdin"))
	return string(b)
}

func (f acctSSH) callback() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "callback"))
	return string(b)
}

// testRemote is a remoteEnv on a fake ssh, its relay on a free port on
// both loopbacks, recording every URL it would open; browse, when set,
// runs (in its own goroutine) once the consent URL is "opened".
type testRemote struct {
	*remoteEnv
	out, errb bytes.Buffer
	mu        sync.Mutex
	opened    []string
	listened  int
	listenErr error
	port      int
	browse    func()
	browsed   chan struct{}
}

func newTestRemote(t *testing.T, f acctSSH, target string) *testRemote {
	r := &testRemote{browsed: make(chan struct{})}
	r.remoteEnv = &remoteEnv{target: target, ssh: f.bin,
		listen: func() ([]net.Listener, error) {
			r.listened++
			if r.listenErr != nil {
				return nil, r.listenErr
			}
			r.port = freeBothPort(t)
			return listenRelay(r.port)
		},
		open: func(u string) error {
			r.mu.Lock()
			r.opened = append(r.opened, u)
			r.mu.Unlock()
			if r.browse != nil {
				go func() { defer close(r.browsed); r.browse() }()
			}
			return nil
		},
		stdout: &r.out, stderr: &r.errb}
	return r
}

func (r *testRemote) openedN() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opened)
}

// freeBothPort is a port free on 127.0.0.1 and ::1 (skips without IPv6).
func freeBothPort(t *testing.T) int {
	t.Helper()
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		l6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port)))
		if err == nil {
			l6.Close()
			return port
		}
		if strings.Contains(err.Error(), "cannot assign") {
			t.Skip("no IPv6 loopback here")
		}
	}
	t.Fatal("no port free on both loopbacks")
	return 0
}

func lines(evs ...remote.Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.Write(e.Line())
	}
	return b.String()
}

// Non-consent verbs: pairing's options (ClearAllForwardings=yes), no
// ssh -G, no port check; the request on stdin, the events rendered.
func TestRemoteStatus(t *testing.T) {
	f := newFakeAccountSSH(t, "status", "me@dell", lines(
		remote.Event{Kind: remote.Progress, Text: "work         ready"},
		remote.Event{Kind: remote.Progress, Text: "(from the running server)"},
		remote.Event{Kind: remote.Result}), "", 0)
	r := newTestRemote(t, f, "me@dell")
	if err := r.run(remote.Status, remote.StatusRequest{Name: "work"}); err != nil {
		t.Fatalf("%v\n%s%s", err, r.out.String(), r.errb.String())
	}
	if f.stdin() != `{"name":"work"}` || f.log() != "-T" || r.listened != 0 {
		t.Errorf("stdin %q, runs %q, relays %d", f.stdin(), f.log(), r.listened)
	}
	want := "This runs on me@dell: " + f.bin + ` -T -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlPath=none -o PermitLocalCommand=no -- me@dell 'PATH="$HOME/.local/bin:$PATH" exec pneu account status --stdin'` + "\n  work         ready\n  (from the running server)\n"
	if r.out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", r.out.String(), want)
	}
}

// add from a client: the flags parsed and checked here, the OAuth client
// read and cleaned here, only its validated fields sent.
func TestRemoteAddFromClient(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"port":7317,"server":{"ssh":"dell","node":"nSERVER1CNTRL","port":7320}}`), 0o600)
	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(strings.Replace(clientJSON, `"client_secret":"s"`, `"client_secret":"s","redirect_uris":["http://evil.example"]`, 1)), 0o600)
	f := newFakeAccountSSH(t, "add", "dell", lines(remote.Event{Kind: remote.Progress, Text: "Setting up work"}, remote.Event{Kind: remote.Result}), "", 0)
	var r *testRemote
	orig := newRemoteEnv
	newRemoteEnv = func(target string) *remoteEnv { r = newTestRemote(t, f, target); return r.remoteEnv }
	t.Cleanup(func() { newRemoteEnv = orig })
	if err := account([]string{"add", "work", "me@work.example", "--name", "Pat Doe", "--client-secret", secret, "--config", cfg}); err != nil {
		t.Fatalf("%v\n%s", err, r.errb.String())
	}
	clean, _ := gmi.CleanClientSecret([]byte(clientJSON))
	var got remote.AddRequest
	if err := json.Unmarshal([]byte(f.stdin()), &got); err != nil || got != (remote.AddRequest{Name: "work", Address: "me@work.example", FullName: "Pat Doe", ClientSecret: string(clean)}) {
		t.Errorf("request %q: %v", f.stdin(), err)
	}
	if strings.Contains(f.stdin(), "evil") {
		t.Error("an unvalidated field reached the server")
	}
	// Refused here, before ssh: a bad name, address or From name.
	for _, args := range [][]string{
		{"add", "$(id)", "me@work.example"},
		{"add", "work", "me@work.example\nx"},
		{"add", "work", "me@work.example", "--name", "Pat\n[database]"},
		{"status", "work", "home"},
		{"auth", "a;b"},
	} {
		os.Remove(filepath.Join(f.dir, "log"))
		if err := account(append(args, "--config", cfg)); err == nil || f.log() != "" {
			t.Errorf("%q: %v, ssh ran %q", args, err, f.log())
		}
	}
}

// auth: the relay bound on both loopbacks before ssh, pairing's options
// (ClearAllForwardings=yes, no -L), the consent URL opened here once, and
// Google's redirect answered here: everything that isn't this consent's
// well-formed callback gets a fixed 400 and the relay keeps waiting; the
// one that is gets a fixed page and goes to the server as one line.
func TestRemoteAuthRelay(t *testing.T) {
	const value = "c0dev4lue"
	query := "state=abc&code=" + value + "&scope=https%3A%2F%2Fmail.google.com%2F&authuser=0"
	f := newFakeAuthSSH(t, "auth", "dell", lines(
		remote.Event{Kind: remote.Progress, Text: "Opening Google's consent screen for work."},
		remote.Event{Kind: remote.Consent, URL: consentURL}),
		lines(remote.Event{Kind: remote.Progress, Text: "Authorized: Gmail answers for me@work.example."}, remote.Event{Kind: remote.Result}), "", 0)
	r := newTestRemote(t, f, "dell")
	var problems []string
	r.browse = func() {
		v4 := "http://127.0.0.1:" + strconv.Itoa(r.port)
		v6 := "http://[::1]:" + strconv.Itoa(r.port)
		host := "localhost:" + strconv.Itoa(r.port)
		get := func(method, u, host string, hdr map[string]string) (int, string) {
			req, _ := http.NewRequest(method, u, nil)
			req.Host = host
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err.Error()
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		nav := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "cross-site"}
		for name, c := range map[string]struct {
			method, url, host string
			hdr               map[string]string
		}{
			"POST":            {"POST", v4 + "/?" + query, host, nav},
			"path":            {"GET", v4 + "/callback?" + query, host, nav},
			"encoded path":    {"GET", v4 + "/%2F?" + query, host, nav},
			"extra key":       {"GET", v4 + "/?" + query + "&next=https://evil.example", host, nav},
			"wrong state":     {"GET", v4 + "/?state=guess&code=" + value, host, nav},
			"no state":        {"GET", v4 + "/?code=" + value, host, nav},
			"oversized":       {"GET", v4 + "/?" + query + "&error_description=" + strings.Repeat("a", remote.MaxCallback), host, nav},
			"duplicate":       {"GET", v4 + "/?" + query + "&code=other", host, nav},
			"fetch":           {"GET", v4 + "/?" + query, host, map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}},
			"image":           {"GET", v6 + "/?" + query, host, map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"}},
			"rebound host":    {"GET", v4 + "/?" + query, "evil.example:" + strconv.Itoa(r.port), nav},
			"other port host": {"GET", v4 + "/?" + query, "localhost:1", nav},
		} {
			code, body := get(c.method, c.url, c.host, c.hdr)
			if code != http.StatusBadRequest || body != relayRefused {
				problems = append(problems, fmt.Sprintf("%s: %d %q", name, code, body))
			}
		}
		// The real redirect, on the other family.
		if code, body := get("GET", v6+"/?"+query, host, nav); code != http.StatusOK || body != relayDone {
			problems = append(problems, fmt.Sprintf("callback: %d %q", code, body))
		}
		// The one-shot is spent.
		if code, body := get("GET", v4+"/?"+query, host, nav); code != 0 && (code != http.StatusBadRequest || body != relayRefused) {
			problems = append(problems, fmt.Sprintf("second callback: %d %q", code, body))
		}
	}
	if err := r.run(remote.Auth, remote.AuthRequest{Name: "work", Force: true, ConsentOpen: remote.ConsentPrint}); err != nil {
		t.Fatalf("%v\n%s%s", err, r.out.String(), r.errb.String())
	}
	<-r.browsed
	for _, p := range problems {
		t.Error(p)
	}
	if r.listened != 1 || f.log() != "-T" {
		t.Errorf("relays %d, runs %q", r.listened, f.log())
	}
	if f.stdin() != `{"name":"work","force":true,"consentOpen":"print"}` {
		t.Errorf("stdin %q", f.stdin())
	}
	if want := strings.TrimSuffix(string(remote.CallbackLine(query)), "\n"); f.callback() != want {
		t.Errorf("callback line %q, want %q", f.callback(), want)
	}
	if r.openedN() != 1 || r.opened[0] != consentURL {
		t.Errorf("opened %q", r.opened)
	}
	if strings.Contains(r.out.String(), "-L") || strings.Contains(r.out.String(), "ClearAllForwardings=no") {
		t.Errorf("printed command:\n%s", r.out.String())
	}
	// The relay is gone with the command, on both families.
	for _, a := range []string{"127.0.0.1", "::1"} {
		if c, err := net.Dial("tcp", net.JoinHostPort(a, strconv.Itoa(r.port))); err == nil {
			c.Close()
			t.Errorf("%s still listening", a)
		}
	}
}

// Both loopbacks or no consent: either one taken refuses before ssh.
func TestRemoteAuthRefusals(t *testing.T) {
	port := freeBothPort(t)
	for _, host := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		lns, err := listenRelay(port)
		ln.Close()
		if err == nil || !strings.Contains(err.Error(), net.JoinHostPort(host, strconv.Itoa(port))) {
			for _, l := range lns {
				l.Close()
			}
			t.Errorf("%s taken: %v", host, err)
		}
	}
	lns, err := listenRelay(port)
	if err != nil || len(lns) != 2 {
		t.Fatalf("free: %v", err)
	}
	for _, l := range lns {
		l.Close()
	}
	f := newFakeAccountSSH(t, "auth", "dell", lines(remote.Event{Kind: remote.Result}), "", 0)
	r := newTestRemote(t, f, "dell")
	r.listenErr = errors.New("[::1]:8080 is in use here")
	if err := r.run(remote.Auth, remote.AuthRequest{Name: "work", ConsentOpen: remote.ConsentPrint}); err == nil || !strings.Contains(err.Error(), "[::1]:8080") || f.log() != "" {
		t.Errorf("port taken: %v, runs %q", err, f.log())
	}
}

// Every ssh pneu runs, checked by the real ssh's own resolution (-G) with
// a config that adds every kind of forward, ClearAllForwardings no, and a
// Match on the remote command: the command line's ClearAllForwardings=yes
// leaves none (Y1).
func TestSSHClearsConfiguredForwards(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh")
	}
	cfg := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfg, []byte(`Host dell
  LocalForward 5555 localhost:22
  RemoteForward 9000 localhost:9000
  DynamicForward 1080
  ClearAllForwardings no
  ForwardAgent yes
  ControlMaster auto
  ControlPath /tmp/pneu-test-%C
Match command "*pneu*"
  RemoteForward 9001 localhost:9001
  LocalForward 9002 localhost:9002
`), 0o600)
	e := &remoteEnv{target: "dell"}
	var argvs [][]string
	for _, v := range []remote.Verb{remote.Add, remote.Auth, remote.Status} {
		a, _ := e.argv(v)
		argvs = append(argvs, a)
	}
	argvs = append(argvs, append(append(append([]string{}, sshOptions...), "--", "dell"), pairCommand))
	resolve := func(argv []string) string {
		out, err := exec.Command(ssh, append([]string{"-G", "-F", cfg}, argv...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh -G: %v %s", err, out)
		}
		return string(out)
	}
	forward := regexp.MustCompile(`(?m)^(localforward|remoteforward|dynamicforward) `)
	has := func(out, line string) bool { return slices.Contains(strings.Split(out, "\n"), line) }
	for _, argv := range argvs {
		got := resolve(argv)
		if forward.MatchString(got) || !has(got, "clearallforwardings yes") || !has(got, "forwardagent no") || regexp.MustCompile(`(?m)^controlpath `).MatchString(got) || !has(got, "permitlocalcommand no") {
			t.Errorf("%q resolves with:\n%s", argv[len(argv)-1], got)
		}
		// The config does add them: without our option they're there,
		// the Match on the command included.
		plain := slices.Clone(argv)
		for i, a := range plain {
			if a == "ClearAllForwardings=yes" {
				plain[i] = "ClearAllForwardings=no"
			}
		}
		if got := resolve(plain); len(forward.FindAllString(got, -1)) != 5 {
			t.Errorf("control: the config's forwards didn't resolve:\n%s", got)
		}
	}
}

// A hostile server's stream: refused, nothing opened after the fault, and
// never raw text in the terminal.
func TestRemoteHostileStreams(t *testing.T) {
	good := string((remote.Event{Kind: remote.Consent, URL: consentURL}).Line())
	result := string((remote.Event{Kind: remote.Result}).Line())
	cases := map[string]struct {
		stdout string
		exit   int
		want   string // in the error
		opened int
	}{
		"oversized line":     {`{"event":"progress","text":"` + strings.Repeat("a", remote.MaxLine) + `"}` + "\n" + good + result, 0, "size cap", 0},
		"oversized no eol":   {strings.Repeat("a", remote.MaxLine*3), 0, "size cap", 0},
		"unknown event":      {`{"event":"prompt","text":"your password?"}` + "\n" + good + result, 0, "unknown event", 0},
		"extra fields":       {`{"event":"consent-url","url":"` + consentURL + `","open":"now"}` + "\n" + result, 0, "extra fields", 0},
		"not google":         {`{"event":"consent-url","url":"https://evil.example/o/oauth2/auth"}` + "\n" + result, 0, "isn't Google's", 0},
		"javascript":         {`{"event":"consent-url","url":"javascript:alert(1)"}` + "\n" + result, 0, "isn't Google's", 0},
		"file":               {`{"event":"consent-url","url":"file:///etc/passwd"}` + "\n" + result, 0, "isn't Google's", 0},
		"credentials":        {`{"event":"consent-url","url":"https://me:pw@accounts.google.com/o/oauth2/auth"}` + "\n" + result, 0, "isn't Google's", 0},
		"long url":           {`{"event":"consent-url","url":"https://accounts.google.com/` + strings.Repeat("a", 5000) + `"}` + "\n" + result, 0, "isn't Google's", 0},
		"second consent":     {good + good + result, 0, "second consent", 1},
		"no state":           {`{"event":"consent-url","url":"https://accounts.google.com/o/oauth2/auth?client_id=x"}` + "\n" + result, 0, "no state", 0},
		"after result":       {result + good, 0, "after its last word", 0},
		"after error":        {`{"event":"error","text":"x"}` + "\n" + good, 0, "after its last word", 0},
		"no result":          {good, 0, "without a result", 1},
		"nothing":            {"", 0, "without a result", 0},
		"not found":          {"", 127, "exit 127", 0},
		"ssh failed":         {"", 255, "exit 255", 0},
		"result then failed": {result, 3, "ssh failed", 0},
		"error event":        {`{"event":"error","text":"no account \"x\"\u001b[2J‮"}` + "\n", 1, `on dell: no account "x"[2J`, 0},
	}
	for name, c := range cases {
		f := newFakeAccountSSH(t, "auth", "dell", c.stdout, "", c.exit)
		r := newTestRemote(t, f, "dell")
		err := r.run(remote.Auth, remote.AuthRequest{Name: "work", ConsentOpen: remote.ConsentPrint})
		if err == nil || !strings.Contains(err.Error(), c.want) || r.openedN() != c.opened {
			t.Errorf("%s: %v (want %q), opened %d", name, err, c.want, r.openedN())
		}
		if strings.ContainsAny(err.Error()+r.out.String(), "\x1b‮") {
			t.Errorf("%s: raw control or bidi in the output", name)
		}
	}
}

// A consent URL is taken only from auth, which has a relay.
func TestRemoteConsentOnlyForAuth(t *testing.T) {
	f := newFakeAccountSSH(t, "status", "dell", lines(remote.Event{Kind: remote.Consent, URL: consentURL}, remote.Event{Kind: remote.Result}), "", 0)
	r := newTestRemote(t, f, "dell")
	if err := r.run(remote.Status, remote.StatusRequest{}); err == nil || !strings.Contains(err.Error(), "runs no consent") || r.openedN() != 0 {
		t.Errorf("%v, opened %d", err, r.openedN())
	}
}

// Display text from the server, stdout or stderr, reaches the terminal
// only plain.
func TestRemoteCleansText(t *testing.T) {
	f := newFakeAccountSSH(t, "status", "dell",
		`{"event":"progress","text":"a\u001b]0;pwned\u0007b‮c"}`+"\n"+string((remote.Event{Kind: remote.Result, Text: "ok x"}).Line()),
		"warning\x1b[2J from ⁦dell\nsecond\r line\n", 0)
	r := newTestRemote(t, f, "dell")
	if err := r.run(remote.Status, remote.StatusRequest{}); err != nil {
		t.Fatal(err)
	}
	all := r.out.String() + r.errb.String()
	if strings.ContainsAny(all, "\x1b\x07‮⁦ \r") {
		t.Errorf("raw text reached the terminal: %q", all)
	}
	if !strings.Contains(r.out.String(), "  a]0;pwnedbc\n") || !strings.Contains(r.out.String(), "okx\n") || r.errb.String() != "dell: warning[2J from dell\ndell: second line\n" {
		t.Errorf("out %q err %q", r.out.String(), r.errb.String())
	}
}

// The remote command never comes from what the user typed: only a Verb
// has one, and the target is a separate argument after "--".
func TestRemoteCommandFixed(t *testing.T) {
	f := newFakeAccountSSH(t, "status", "dell", lines(remote.Event{Kind: remote.Result}), "", 0)
	r := newTestRemote(t, f, "dell")
	for _, v := range []remote.Verb{"status; touch /tmp/pwned", "gmi", "status --config x", ""} {
		if err := r.run(v, remote.StatusRequest{}); err == nil || f.log() != "" {
			t.Errorf("%q: %v, runs %q", v, err, f.log())
		}
	}
	for _, target := range []string{"-oProxyCommand=touch /tmp/x", "dell ls", "dell\nls", ""} {
		r := newTestRemote(t, f, target)
		if err := r.run(remote.Status, remote.StatusRequest{}); err == nil || f.log() != "" {
			t.Errorf("target %q: %v", target, err)
		}
	}
	// Under the remote shell, each fixed command runs pneu with exactly
	// `account <verb> --stdin`.
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "pneu"), []byte("#!/bin/sh\nprintf '%s|' \"$@\"\n"), 0o755)
	for _, v := range []remote.Verb{remote.Add, remote.Auth, remote.Status} {
		c, _ := remote.Command(v)
		cmd := exec.Command("sh", "-c", c)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		if out, err := cmd.Output(); err != nil || string(out) != "account|"+string(v)+"|--stdin|" {
			t.Errorf("%s: %q %v", v, out, err)
		}
	}
}

// On a client, lieer's own runs aren't forwarded.
func TestGmiOnClientRefused(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte(`{"port":7317,"server":{"ssh":"me@dell","node":"nSERVER1CNTRL","port":7320}}`), 0o600)
	if err := runGmi([]string{"-config", cfg, "work", "pull"}); err == nil || !strings.Contains(err.Error(), "client of me@dell") {
		t.Errorf("%v", err)
	}
}

// The relay takes one callback even while it's still listening, and none
// before its consent URL has armed it.
func TestRelayOneShot(t *testing.T) {
	lns, err := listenRelay(freeBothPort(t))
	if err != nil {
		t.Fatal(err)
	}
	rl := startRelay(lns)
	defer rl.close()
	port := lns[0].Addr().(*net.TCPAddr).Port
	u := "http://127.0.0.1:" + strconv.Itoa(port) + "/?state=abc&code=c"
	get := func() int {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code != http.StatusBadRequest {
		t.Errorf("before arming: %d", code)
	}
	rl.arm("abc")
	if code := get(); code != http.StatusOK || <-rl.got != "state=abc&code=c" {
		t.Errorf("first: %d", code)
	}
	if code := get(); code != http.StatusBadRequest {
		t.Errorf("second: %d", code)
	}
	select {
	case q := <-rl.got:
		t.Errorf("a second callback was taken: %q", q)
	default:
	}
}

// Z3: connections a page can open to the relay are bounded in number and
// time; once they've gone, the real callback is still taken.
func TestRelayBounded(t *testing.T) {
	oldT := relayTimeout
	relayTimeout = 500 * time.Millisecond
	t.Cleanup(func() { relayTimeout = oldT })
	lns, err := listenRelay(freeBothPort(t))
	if err != nil {
		t.Fatal(err)
	}
	rl := startRelay(lns)
	defer rl.close()
	addr := lns[0].Addr().String()
	host := "localhost:" + strconv.Itoa(lns[0].Addr().(*net.TCPAddr).Port)
	// How long until the server lets go of a connection that sent head.
	held := func(head string) time.Duration {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		start := time.Now()
		c.Write([]byte(head))
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, c)
		return time.Since(start)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var times []time.Duration
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := held("GET /?state=abc&code=c HTTP/1.1\r\nHost: " + host + "\r\n") // headers never end
			mu.Lock()
			times = append(times, d)
			mu.Unlock()
		}()
	}
	wg.Wait()
	quick := 0
	for _, d := range times {
		if d > 3*time.Second {
			t.Errorf("a connection held for %v", d)
		}
		if d < 250*time.Millisecond {
			quick++
		}
	}
	if quick < 32-relayConns {
		t.Errorf("only %d of 32 closed at once; at most %d may be held", quick, relayConns)
	}
	// A body that never comes.
	if d := held("POST / HTTP/1.1\r\nHost: " + host + "\r\nContent-Length: 1\r\n\r\n"); d > 3*time.Second {
		t.Errorf("slow body held for %v", d)
	}
	rl.arm("abc")
	req, _ := http.NewRequest("GET", "http://"+addr+"/?state=abc&code=c", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("callback after: %v %v", resp, err)
	}
	resp.Body.Close()
}

// scriptSSH is an ssh stand-in that checks verb's argv and then runs body.
func scriptSSH(t *testing.T, verb, target, body string) acctSSH {
	t.Helper()
	dir := t.TempDir()
	f := acctSSH{bin: filepath.Join(dir, "ssh"), dir: dir}
	script := `#!/bin/sh
got=""
for a in "$@"; do got="$got|$a"; done
got="${got#|}"
if [ "$got" != '` + wantArgv(verb, target) + `' ]; then echo "argv: $got" >&2; exit 99; fi
IFS= read -r req
` + body + "\n"
	if err := os.WriteFile(f.bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func relayClosed(port int) bool {
	for _, a := range []string{"127.0.0.1", "::1"} {
		if c, err := net.Dial("tcp", net.JoinHostPort(a, strconv.Itoa(port))); err == nil {
			c.Close()
			return false
		}
	}
	return true
}

// Z2: a closing event ends the session's use at once (the relay closed,
// stdin closed), and a server that keeps its stdout open after it is cut
// off after lingerWait.
func TestRemoteResultEndsSession(t *testing.T) {
	oldL := lingerWait
	lingerWait = time.Second
	t.Cleanup(func() { lingerWait = oldL })
	evs := lines(remote.Event{Kind: remote.Consent, URL: consentURL}, remote.Event{Kind: remote.Result})
	f := scriptSSH(t, "auth", "dell", "printf '%s' '"+evs+"'\nexec sleep 30")
	r := newTestRemote(t, f, "dell")
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- r.run(remote.Auth, remote.AuthRequest{Name: "work", ConsentOpen: remote.ConsentPrint}) }()
	// The relay goes with the result, well before the linger is up.
	for r.openedN() == 0 || !relayClosed(r.port) {
		if time.Since(start) > 700*time.Millisecond {
			t.Fatal("the relay outlived the result")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%v", err)
		}
		if d := time.Since(start); d > 4*time.Second {
			t.Errorf("took %v", d)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the command hung on a server that kept its stdout open")
	}
}

// Z2: no consent URL within relayStartWait ends it; the consent wait runs
// from the URL's arrival, not from the start.
func TestRemoteRelayTimers(t *testing.T) {
	oldS, oldC := relayStartWait, consentWait
	t.Cleanup(func() { relayStartWait, consentWait = oldS, oldC })

	relayStartWait, consentWait = 300*time.Millisecond, time.Minute
	f := scriptSSH(t, "auth", "dell", "exec sleep 30")
	r := newTestRemote(t, f, "dell")
	start := time.Now()
	err := r.run(remote.Auth, remote.AuthRequest{Name: "work", ConsentOpen: remote.ConsentPrint})
	if err == nil || !strings.Contains(err.Error(), "sent no consent URL within") || time.Since(start) > 3*time.Second || !relayClosed(r.port) {
		t.Errorf("startup bound: %v after %v", err, time.Since(start))
	}

	relayStartWait, consentWait = time.Second, 1500*time.Millisecond
	f = scriptSSH(t, "auth", "dell", "sleep 0.8\nprintf '%s' '"+lines(remote.Event{Kind: remote.Consent, URL: consentURL})+"'\nexec sleep 30")
	r = newTestRemote(t, f, "dell")
	start = time.Now()
	err = r.run(remote.Auth, remote.AuthRequest{Name: "work", ConsentOpen: remote.ConsentPrint})
	d := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "didn't come within") || d < 2200*time.Millisecond || d > 5*time.Second || r.openedN() != 1 || !relayClosed(r.port) {
		t.Errorf("consent bound: %v after %v", err, d)
	}
}
