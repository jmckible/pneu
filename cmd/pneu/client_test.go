package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
	"github.com/jmckible/pneu/internal/web"
)

type clientTS struct{ state string }

func (c clientTS) Status(context.Context) (tailscale.Status, error) {
	return tailscale.Status{BackendState: c.state, Self: tailscale.Self{StableID: "nCLIENT1CNTRL", UserID: 1}}, nil
}

// fakeSSH writes an ssh stand-in that insists on exactly pairing's argv
// (options, "--", the target, the fixed remote command; exit 99
// otherwise), saves its stdin, says something on stderr, and prints
// stdout's file.
func fakeSSH(t *testing.T, target, stdout string, exit int) (bin, stdin, ran string) {
	t.Helper()
	dir := t.TempDir()
	bin, stdin, ran = filepath.Join(dir, "ssh"), filepath.Join(dir, "stdin"), filepath.Join(dir, "ran")
	out := filepath.Join(dir, "stdout")
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "-T|-o|ForwardAgent=no|-o|ForwardX11=no|-o|ClearAllForwardings=yes|-o|ControlPath=none|-o|PermitLocalCommand=no|--|" + target + "|" + `PATH="$HOME/.local/bin:$PATH" exec pneu peer add --stdin`
	script := `#!/bin/sh
touch '` + ran + `'
got=""
for a in "$@"; do got="$got|$a"; done
got="${got#|}"
if [ "$got" != '` + want + `' ]; then echo "argv: $got" >&2; exit 99; fi
cat > '` + stdin + `'
echo "pneu peer: paired (remote diagnostics)" >&2
cat '` + out + `'
exit ` + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, stdin, ran
}

// serverAnswer is `pneu peer add`'s stdout for a server key.
func serverAnswer(t *testing.T, id peer.Identity, name, applied string, protocol int) string {
	t.Helper()
	b, err := json.Marshal(peer.AddResult{Cert: id.PEM, Node: "nSERVER1CNTRL", Port: 7320, Protocol: protocol, Name: name, Applied: applied})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func serverID(t *testing.T) peer.Identity {
	t.Helper()
	id, err := peer.LoadOrCreateServer(filepath.Join(t.TempDir(), "peer"), "server")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testClientEnv(t *testing.T, ssh string) (*clientEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	sockBase, err := os.MkdirTemp("", "pneucl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockBase) })
	return &clientEnv{
		cfgPath: filepath.Join(t.TempDir(), "config.json"), credDir: filepath.Join(t.TempDir(), "peer"),
		api: clientTS{tailscale.Running}, ssh: ssh, host: "mac.local", now: time.Now, stdout: &out, stderr: &errb,
		socket: filepath.Join(sockBase, "pneu", "control"), // no daemon until a test starts one
	}, &out, &errb
}

func TestClientPair(t *testing.T) {
	srv := serverID(t)
	bin, stdin, _ := fakeSSH(t, "server", serverAnswer(t, srv, "mac", "live", web.Protocol), 0)
	e, out, errb := testClientEnv(t, bin)
	if err := e.pair("server", ""); err != nil {
		t.Fatalf("%v\nstderr: %s", err, errb)
	}
	// What went over stdin is the request the server parses.
	raw, _ := os.ReadFile(stdin)
	req, rec, err := peer.ParseAddRequest(bytes.NewReader(raw))
	if err != nil || req.Name != "mac" || req.Node != "nCLIENT1CNTRL" || req.Origin != "http://pneu.localhost:7317" {
		t.Fatalf("request %s: %v", raw, err)
	}
	creds, err := link.LoadCreds(e.credDir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SPKI != creds.Identity.SPKI || creds.Pin.SPKI != srv.SPKI || creds.Pin.Node != "nSERVER1CNTRL" || creds.Pin.SSH != "server" || creds.Pin.Name != "mac" {
		t.Fatalf("creds %+v", creds.Pin)
	}
	cfg, err := config.Load(e.cfgPath)
	if err != nil || cfg.Server == nil || *cfg.Server != (config.Server{SSH: "server", Node: "nSERVER1CNTRL", Port: 7320}) {
		t.Fatalf("config %+v %v", cfg.Server, err)
	}
	if !strings.Contains(out.String(), "took it live") || !strings.Contains(errb.String(), "remote diagnostics") || !strings.Contains(errb.String(), "host key") {
		t.Fatalf("stdout %q stderr %q", out, errb)
	}
	for _, f := range []string{link.PinFile, peer.ClientKeyFile} {
		if fi, err := os.Stat(filepath.Join(e.credDir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, fi.Mode(), err)
		}
	}

	// Paired: refused until unpaired.
	if err := e.pair("server", ""); err == nil || !strings.Contains(err.Error(), "unpair") {
		t.Fatalf("second pair: %v", err)
	}
	out.Reset()
	if err := e.unpair(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ssh -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlPath=none -o PermitLocalCommand=no server '~/.local/bin/pneu peer remove mac'") || link.Paired(e.credDir) {
		t.Fatalf("unpair: %q", out)
	}
	if raw, _ := config.ReadRaw(e.cfgPath); raw.Server != nil || raw.Port != 7317 {
		t.Fatalf("config after unpair %+v", raw)
	}
	if _, err := os.Stat(filepath.Join(e.credDir, peer.ClientKeyFile)); !os.IsNotExist(err) {
		t.Fatal("unpair kept the key")
	}
	if err := e.unpair(); err == nil {
		t.Fatal("unpair twice")
	}
	// A new pairing has a new key.
	if err := e.pair("server", ""); err != nil {
		t.Fatal(err)
	}
	if c2, _ := link.LoadCreds(e.credDir); c2.Identity.SPKI == creds.Identity.SPKI {
		t.Fatal("re-pairing kept the old key")
	}
}

func TestClientPairApplied(t *testing.T) {
	srv := serverID(t)
	for applied, want := range map[string]string{
		"next-start": "applies when it starts",
		"pending":    "XDG_RUNTIME_DIR",
	} {
		bin, _, _ := fakeSSH(t, "me@server", serverAnswer(t, srv, "air", applied, web.Protocol), 0)
		e, out, _ := testClientEnv(t, bin)
		if err := e.pair("me@server", "air"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), want) || (applied == "pending" && !strings.Contains(out.String(), "ssh -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlPath=none -o PermitLocalCommand=no me@server '~/.local/bin/pneu peer list'")) {
			t.Errorf("%s: %q", applied, out)
		}
	}
}

// The answer is parsed strictly, and a refusal leaves nothing behind.
func TestClientPairRefusals(t *testing.T) {
	srv := serverID(t)
	good := serverAnswer(t, srv, "mac", "live", web.Protocol)
	var fields map[string]any
	json.Unmarshal([]byte(good), &fields)
	with := func(k string, v any) string {
		m := map[string]any{}
		for a, b := range fields {
			m[a] = b
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	rsa := "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIUQ==\n-----END CERTIFICATE-----\n"
	for name, c := range map[string]struct {
		stdout string
		exit   int
		want   string
	}{
		"oversize":          {strings.Repeat(" ", peer.MaxAddResult+1), 0, "over"},
		"unknown field":     {with("extra", "x"), 0, "unknown field"},
		"case variant":      {strings.Replace(good, `"cert"`, `"Cert"`, 1), 0, "unknown field"},
		"duplicate":         {strings.Replace(good, `{`, `{"name":"mac",`, 1), 0, "twice"},
		"missing":           {with("applied", nil), 0, "required"},
		"bad cert":          {with("cert", rsa), 0, "certificate"},
		"two certs":         {with("cert", srv.PEM+srv.PEM), 0, "certificate"},
		"no node":           {with("node", ""), 0, "node"},
		"port":              {with("port", 0), 0, "port"},
		"applied":           {with("applied", "done"), 0, "applied"},
		"trailing":          {good + "{}", 0, "trailing"},
		"protocol mismatch": {with("protocol", web.Protocol+1), 0, "protocol"},
		"other name":        {with("name", "air"), 0, "different name"},
		"own node":          {with("node", "nCLIENT1CNTRL"), 0, "own node"},
		"ssh failed":        {"", 255, "ssh failed"},
		"no pneu there":     {"", 127, "couldn't find pneu (exit 127): build it into ~/.local/bin on server"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, _, _ := fakeSSH(t, "server", c.stdout, c.exit)
			e, _, _ := testClientEnv(t, bin)
			err := e.pair("server", "")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			if name == "protocol mismatch" && !strings.Contains(err.Error(), "pneu peer remove mac") {
				t.Errorf("mismatch doesn't say how to undo: %v", err)
			}
			if link.Paired(e.credDir) {
				t.Fatal("pinned after a refusal")
			}
			if raw, _ := config.ReadRaw(e.cfgPath); raw.Server != nil {
				t.Fatal("config written after a refusal")
			}
		})
	}
}

// Before ssh runs at all: the target, the name, tailscale, and a machine
// that is a server.
func TestClientPairPreflight(t *testing.T) {
	srv := serverID(t)
	for name, c := range map[string]struct {
		target, peerName string
		setup            func(e *clientEnv)
		want             string
	}{
		"option target":  {target: "-oProxyCommand=touch /tmp/x", want: "bad ssh target"},
		"space":          {target: "server ls", want: "bad ssh target"},
		"newline":        {target: "server\nls", want: "bad ssh target"},
		"empty":          {target: "", want: "bad ssh target"},
		"bad hostname":   {target: "server", setup: func(e *clientEnv) { e.host = "My_Laptop" }, want: "-name"},
		"bad name":       {target: "server", peerName: "Air", want: "-name"},
		"tailscale down": {target: "server", setup: func(e *clientEnv) { e.api = clientTS{"Stopped"} }, want: "tailscale is Stopped"},
		"a server": {target: "server", setup: func(e *clientEnv) {
			config.Write(e.cfgPath, config.Config{Port: 7317, Accounts: []config.Account{{Name: "a"}}})
		}, want: "is a server"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, _, ran := fakeSSH(t, "server", serverAnswer(t, srv, "mac", "live", web.Protocol), 0)
			e, _, _ := testClientEnv(t, bin)
			if c.setup != nil {
				c.setup(e)
			}
			if err := e.pair(c.target, c.peerName); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			if _, err := os.Stat(ran); err == nil {
				t.Fatal("ssh ran")
			}
		})
	}
}

// C3: unpair asks the running daemon to drop the link first. With it
// running, the link's open stream is cut, a request after is not sent
// (not-paired), and nothing reconnects; only then do the files go.
func TestClientUnpairRunning(t *testing.T) {
	srv := serverID(t)
	bin, _, _ := fakeSSH(t, "server", serverAnswer(t, srv, "mac", "live", web.Protocol), 0)
	e, out, _ := testClientEnv(t, bin)
	if err := e.pair("server", ""); err != nil {
		t.Fatal(err)
	}
	creds, err := link.LoadCreds(e.credDir)
	if err != nil {
		t.Fatal(err)
	}
	u := linktest.StartUpstream(t, srv, creds.Identity.SPKI, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			http.NewResponseController(w).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	lk := link.New(linktest.NewAPI(), creds, u.Port)
	lk.Start()
	defer lk.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !lk.WaitUp(ctx) {
		t.Fatalf("link %+v", lk.State())
	}
	req, _ := http.NewRequest("GET", "https://server/stream", nil)
	resp, err := lk.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cut := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); cut <- err }()

	// A daemon that won't confirm: nothing is removed.
	ctl, err := control.Listen(e.socket, control.Handler{Client: true, Unlink: func() error { return errors.New("busy") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.unpair(); err == nil || !strings.Contains(err.Error(), "systemctl --user stop pneu") || !link.Paired(e.credDir) {
		t.Fatalf("unconfirmed unlink: %v, paired %v", err, link.Paired(e.credDir))
	}
	ctl.Close()

	ctl, err = control.Listen(e.socket, control.Handler{Client: true, Unlink: func() error { lk.Unpair(); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	conns := u.Conns.Load()
	if err := e.unpair(); err != nil {
		t.Fatal(err)
	}
	// The ack came after the connections closed: the stream is already cut.
	select {
	case err := <-cut:
		if err == nil {
			t.Fatal("stream ended cleanly")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream still running after unlink")
	}
	var down *link.DownError
	if _, err := lk.RoundTrip(req); !errors.As(err, &down) || down.Reason != link.NotPaired {
		t.Fatalf("request after unlink: %v", err)
	}
	lk.Retry()
	time.Sleep(300 * time.Millisecond)
	if u.Conns.Load() != conns {
		t.Fatalf("reconnected after unlink: %d -> %d", conns, u.Conns.Load())
	}
	if link.Paired(e.credDir) || !strings.Contains(out.String(), "dropped the link") || !strings.Contains(out.String(), "ssh -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlPath=none -o PermitLocalCommand=no server '~/.local/bin/pneu peer remove mac'") {
		t.Fatalf("paired %v, out %q", link.Paired(e.credDir), out)
	}
}

// D1: with no socket to ask (no runtime dir here, or none answering),
// unpair goes by the lifecycle lock: held means a daemon runs, so nothing
// is removed; free, unpair holds it while deleting, so no daemon can start
// in the middle.
func TestClientUnpairLock(t *testing.T) {
	srv := serverID(t)
	bin, _, _ := fakeSSH(t, "server", serverAnswer(t, srv, "mac", "live", web.Protocol), 0)
	for _, noRuntime := range []bool{true, false} {
		e, _, _ := testClientEnv(t, bin)
		if err := e.pair("server", ""); err != nil {
			t.Fatal(err)
		}
		if noRuntime {
			e.socket, e.sockErr = "", control.ErrNoRuntimeDir
		}
		daemon, err := link.Lock(e.credDir) // a daemon without a reachable socket
		if err != nil {
			t.Fatal(err)
		}
		if err := e.unpair(); err == nil || !strings.Contains(err.Error(), "systemctl --user stop pneu") || !link.Paired(e.credDir) {
			t.Fatalf("with a daemon's lock held: %v, paired %v", err, link.Paired(e.credDir))
		}
		daemon.Close()
		var midErr error
		e.midDelete = func() { _, midErr = link.Lock(e.credDir) }
		if err := e.unpair(); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(midErr, link.ErrLocked) {
			t.Fatalf("a daemon could start mid-delete: %v", midErr)
		}
		if raw, _ := config.ReadRaw(e.cfgPath); link.Paired(e.credDir) || raw.Server != nil {
			t.Fatal("not unpaired")
		}
		if l, err := link.Lock(e.credDir); err != nil {
			t.Fatalf("unpair kept the lock: %v", err)
		} else {
			l.Close()
		}
	}
}

// D1: the daemon takes its lock and its socket before loading any
// credentials, and won't run without either.
func TestStartClient(t *testing.T) {
	keys := linktest.NewKeys(t)
	cfg := config.Config{Port: 7317, Server: &config.Server{SSH: "server", Node: linktest.ServerNode, Port: 1}}
	auth := web.NewAuth("pneu.localhost:7317", strings.Repeat("ab", 32))
	sockBase, _ := os.MkdirTemp("", "pneusc")
	defer os.RemoveAll(sockBase)
	sock := filepath.Join(sockBase, "pneu", "control")

	// No runtime dir: refused, and the credentials weren't the reason.
	if _, err := startClient(cfg, keys.ClientDir, "", control.ErrNoRuntimeDir, linktest.NewAPI(), auth, ""); err == nil || !strings.Contains(err.Error(), "control socket") {
		t.Fatalf("without a runtime dir: %v", err)
	}
	// Unpaired: the socket and lock come first, then the refusal, and
	// neither is left behind.
	empty := filepath.Join(t.TempDir(), "peer")
	if _, err := startClient(cfg, empty, sock, nil, linktest.NewAPI(), auth, ""); err == nil || !strings.Contains(err.Error(), "pair") {
		t.Fatalf("unpaired: %v", err)
	}
	c, err := startClient(cfg, keys.ClientDir, sock, nil, linktest.NewAPI(), auth, "")
	if err != nil {
		t.Fatal(err)
	}
	// A second daemon exits.
	sock2 := filepath.Join(sockBase, "two", "control")
	if _, err := startClient(cfg, keys.ClientDir, sock2, nil, linktest.NewAPI(), auth, ""); err == nil || !strings.Contains(err.Error(), "another pneu client") {
		t.Fatalf("second daemon: %v", err)
	}
	// unlink over the real socket.
	if err := control.SendUnlink(sock); err != nil {
		t.Fatal(err)
	}
	if st := c.lk.State(); st.Reason != link.NotPaired {
		t.Fatalf("after unlink: %+v", st)
	}
	c.close()
	if l, err := link.Lock(keys.ClientDir); err != nil {
		t.Fatalf("lock after close: %v", err)
	} else {
		l.Close()
	}
}

// E1: a daemon (re)starting while unpair runs, after the old daemon's ack
// and before the deletion, waits for the transaction and then finds no
// pairing to load.
func TestUnpairTransaction(t *testing.T) {
	srv := serverID(t)
	bin, _, _ := fakeSSH(t, "server", serverAnswer(t, srv, "mac", "live", web.Protocol), 0)
	e, _, _ := testClientEnv(t, bin)
	if err := e.pair("server", ""); err != nil {
		t.Fatal(err)
	}
	// The old daemon: acks unlink (then, say, crashes and is restarted).
	ctl, err := control.Listen(e.socket, control.Handler{Client: true, Unlink: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	cfg := config.Config{Port: 7317, Server: &config.Server{SSH: "server", Node: "nSERVER1CNTRL", Port: 7320}}
	auth := web.NewAuth("pneu.localhost:7317", strings.Repeat("ab", 32))
	sockBase, _ := os.MkdirTemp("", "pneutx")
	defer os.RemoveAll(sockBase)
	started := make(chan error, 1)
	e.beforeDelete = func() {
		go func() {
			c, err := startClient(cfg, e.credDir, filepath.Join(sockBase, "pneu", "control"), nil, linktest.NewAPI(), auth, "")
			if c != nil {
				c.close()
			}
			started <- err
		}()
		select {
		case err := <-started:
			t.Errorf("a daemon started inside the unpair transaction: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
	}
	if err := e.unpair(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if err == nil || !strings.Contains(err.Error(), "pneu client pair") {
			t.Fatalf("the restarted daemon: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the restarted daemon never got past the transaction")
	}
}

// pair runs inside the pairing transaction (F2): while another holds
// pair.lock it writes nothing, and it commits once the lock is free.
func TestPairTakesPairLock(t *testing.T) {
	srv := serverID(t)
	bin, _, _ := fakeSSH(t, "server", serverAnswer(t, srv, "mac", "live", web.Protocol), 0)
	e, _, _ := testClientEnv(t, bin)
	held, err := link.PairLock(e.credDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.pair("server", "") }()
	select {
	case err := <-done:
		t.Fatalf("pair ran while pair.lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if link.Paired(e.credDir) {
		t.Fatal("pair wrote its pin while pair.lock was held")
	}
	held.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pair never finished after the lock was released")
	}
	if !link.Paired(e.credDir) {
		t.Error("pair didn't commit")
	}
}

// sshd hands the remote command to the user's shell as `-c <command>`,
// with a PATH that may lack ~/.local/bin. The fixed command still finds
// pneu there, with exactly its arguments, under sh and bash alike.
func TestPairCommandFindsLocalBin(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "pneu"), []byte("#!/bin/sh\nprintf '%s|' \"$@\"\n"), 0o755)
	for _, shell := range []string{"sh", "bash"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, "-c", pairCommand)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil || string(out) != "peer|add|--stdin|" {
			t.Errorf("%s: %q %v", shell, out, err)
		}
	}
}

// A target outside [A-Za-z0-9@._:-] is quoted in commands printed to copy.
func TestRemoteRemoveQuotes(t *testing.T) {
	if got := remoteRemove("server", "mac"); got != "ssh -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlPath=none -o PermitLocalCommand=no server '~/.local/bin/pneu peer remove mac'" {
		t.Error(got)
	}
	if got := remoteRemove("me@server;x", "mac"); got != "ssh -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlPath=none -o PermitLocalCommand=no 'me@server;x' '~/.local/bin/pneu peer remove mac'" {
		t.Error(got)
	}
}
