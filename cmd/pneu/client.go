package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/client"
	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
	"github.com/jmckible/pneu/internal/web"
)

const clientUsage = "usage: pneu client pair [-name <name>] <ssh-target> | unpair"

// sshOptions are pairing's: every SSH capability it doesn't need off (R7),
// host-key checking left at the user's own setting.
var sshOptions = []string{
	"-T", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes",
	"-o", "ControlPath=none", "-o", "PermitLocalCommand=no",
}

// pairCommand is the one remote command pairing runs; its parameters go on
// stdin, never here (SSH joins arguments into a shell line).
const pairCommand = "pneu peer add --stdin"

// selfStatus is what pairing asks tailscaled: this node's StableID.
type selfStatus interface {
	Status(ctx context.Context) (tailscale.Status, error)
}

// clientEnv is what the client commands touch, swapped out by tests.
type clientEnv struct {
	cfgPath string
	credDir string // $XDG_STATE_HOME/pneu/peer
	api     selfStatus
	ssh     string // the ssh binary
	socket  string // the control socket; "" with sockErr
	sockErr error
	// beforeDelete runs once the daemon has let go (or isn't running),
	// before anything is deleted; midDelete between deleting the
	// credentials and rewriting the config. Tests only.
	beforeDelete, midDelete func()
	host                    string // this machine's hostname, the default peer name
	now                     func() time.Time
	stdout                  io.Writer
	stderr                  io.Writer
}

// clientCmd is `pneu client pair|unpair` (docs/client.md, "Pairing").
func clientCmd(args []string) error {
	if len(args) == 0 {
		return usageError{clientUsage}
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet("pneu client "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgFlag := fs.String("config", "", "config file (default ~/.config/pneu/config.json)")
	name := fs.String("name", "", "this machine's name on the server (pair; default: the hostname)")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	cfgPath, err := configPath(*cfgFlag)
	if err != nil {
		return err
	}
	dir, err := link.Dir()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	env := &clientEnv{cfgPath: cfgPath, credDir: dir, api: tailscale.NewLocal(), ssh: "ssh", host: host,
		now: time.Now, stdout: os.Stdout, stderr: os.Stderr}
	env.socket, env.sockErr = control.SocketPath()
	switch verb {
	case "pair":
		if fs.NArg() != 1 {
			return usageError{clientUsage}
		}
		return env.pair(fs.Arg(0), *name)
	case "unpair":
		if fs.NArg() != 0 || *name != "" {
			return usageError{clientUsage}
		}
		return env.unpair()
	}
	return usageError{clientUsage}
}

// defaultName is the hostname's first label, lowercased: a peer name if it
// passes peer.ValidName.
func defaultName(host string) string {
	first, _, _ := strings.Cut(host, ".")
	return strings.ToLower(first)
}

// pair makes this machine a client of the server at target: a key pair,
// one fixed SSH command carrying the request on stdin, the answer parsed
// strictly, then the pin and the config's server block.
func (e *clientEnv) pair(target, name string) error {
	if !config.ValidSSHTarget(target) {
		return usageError{fmt.Sprintf("bad ssh target %q: a host or user@host, no spaces, not starting with '-'", target)}
	}
	// The pairing transaction, read to commit: an unpair or a starting
	// daemon can't act on a half-written pairing (F2).
	pl, err := link.PairLock(e.credDir, link.PairWait)
	if errors.Is(err, link.ErrLocked) {
		return errors.New("another pneu client pair or unpair, or a pneu starting up, holds the pairing lock. Try again")
	}
	if err != nil {
		return err
	}
	defer pl.Close()
	raw, err := config.ReadRaw(e.cfgPath)
	if err != nil {
		return err
	}
	switch {
	case raw.Server != nil || link.Paired(e.credDir):
		return errors.New("already paired: run `pneu client unpair` first (and remove this machine on the server, as it says)")
	case len(raw.Accounts) > 0 || raw.Peer != nil:
		return fmt.Errorf("%s has accounts or a peer block: this machine is a server, not a client", e.cfgPath)
	}
	if name == "" {
		name = defaultName(e.host)
	}
	if !peer.ValidName(name) {
		return usageError{fmt.Sprintf("%q can't name this machine on the server (a lowercase DNS label of at most 32); pass -name", name)}
	}
	port := raw.Port
	if port == 0 {
		port = config.DefaultPort
	}
	ctx, cancel := context.WithTimeout(context.Background(), tailscale.CallTimeout)
	st, err := e.api.Status(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("asking tailscale who this machine is: %w", err)
	}
	if st.BackendState != tailscale.Running || !peer.ValidNode(st.Self.StableID) {
		return fmt.Errorf("tailscale is %s here; pairing needs it running", st.BackendState)
	}
	id, err := peer.LoadOrCreateIdentity(e.credDir, peer.ClientKeyFile, "pneu "+name)
	if err != nil {
		return err
	}
	req, err := json.Marshal(peer.AddRequest{Name: name, Node: st.Self.StableID, Origin: "http://pneu.localhost:" + strconv.Itoa(port), Cert: id.PEM})
	if err != nil {
		return err
	}
	if len(req) > peer.MaxAddRequest {
		return errors.New("pairing request over its size cap")
	}

	fmt.Fprintf(e.stderr, "pneu client: pairing as %s over ssh %s. If ssh asks about an unknown host key, that's your call to make: it's what vouches for the server.\n", name, target)
	out := &cappedBuffer{max: peer.MaxAddResult}
	cmd := exec.Command(e.ssh, append(append(append([]string{}, sshOptions...), "--", target), pairCommand)...)
	cmd.Stdin = bytes.NewReader(req)
	cmd.Stdout = out
	cmd.Stderr = e.stderr
	runErr := cmd.Run()
	if out.over {
		return fmt.Errorf("the server's answer is over %d bytes; not pairing", peer.MaxAddResult)
	}
	if runErr != nil {
		return fmt.Errorf("pairing over ssh failed (%v); the server's own message, if any, is above", runErr)
	}
	res, cert, err := peer.ParseAddResult(out.Bytes())
	if err != nil {
		return fmt.Errorf("the server's answer won't do: %w", err)
	}
	removeCmd := "ssh " + target + " pneu peer remove " + name
	switch {
	case res.Protocol != web.Protocol:
		return fmt.Errorf("the server speaks link protocol %d and this pneu speaks %d, so nothing is pinned here. The server has recorded the pairing: update the older side, run `%s`, then pair again", res.Protocol, web.Protocol, removeCmd)
	case res.Name != name:
		return fmt.Errorf("the server paired a different name (%s); run `%s` there and pair again", res.Name, removeCmd)
	case res.Node == st.Self.StableID || peer.SPKI(cert) == id.SPKI:
		return errors.New("the server answered with this machine's own node or key; not pairing")
	}
	pin := link.Pin{Name: name, SSH: target, Node: res.Node, Port: res.Port, SPKI: peer.SPKI(cert), Cert: res.Cert,
		Protocol: res.Protocol, Paired: e.now().UTC().Truncate(time.Second)}
	if err := link.WritePin(e.credDir, pin); err != nil {
		return err
	}
	raw.Server = &config.Server{SSH: target, Node: res.Node, Port: res.Port}
	if raw.Port == 0 {
		raw.Port = port
	}
	if err := config.Write(e.cfgPath, raw); err != nil {
		return fmt.Errorf("pinned the server, but writing %s failed: %w", e.cfgPath, err)
	}
	fmt.Fprintf(e.stdout, "paired with %s (node %s, port %d) as %s.\n", target, res.Node, res.Port, name)
	switch res.Applied {
	case "live":
		fmt.Fprintln(e.stdout, "The server's pneu took it live.")
	case "next-start":
		fmt.Fprintln(e.stdout, "pneu isn't running on the server (or runs without its peer block): the pairing applies when it starts there.")
	default:
		fmt.Fprintf(e.stdout, "The server recorded it but didn't confirm it's live: its SSH session had no XDG_RUNTIME_DIR (no pam_systemd session), or its pneu didn't acknowledge in time. It applies at the server's next start at the latest (systemctl --user restart pneu there). Check with `ssh %s pneu peer list`.\n", target)
	}
	fmt.Fprintln(e.stdout, "Next: systemctl --user restart pneu here, then pneu open.")
	return nil
}

// unpair forgets the server here: the pin, this machine's key and the
// config's server block. The server keeps admitting the key until it's
// removed there, so it prints that command rather than running it.
func (e *clientEnv) unpair() error {
	// One transaction from reading the pairing to the last deletion: a
	// daemon (re)starting meanwhile waits for it, then finds no pairing
	// (E1), and a pair in progress finishes before this reads (F2).
	pair, err := link.PairLock(e.credDir, link.PairWait)
	if errors.Is(err, link.ErrLocked) {
		return errors.New("another pneu client pair or unpair, or a pneu starting up, holds the pairing lock; nothing was removed. Try again")
	}
	if err != nil {
		return err
	}
	defer pair.Close()
	raw, err := config.ReadRaw(e.cfgPath)
	if err != nil {
		return err
	}
	if raw.Server == nil && !link.Paired(e.credDir) {
		return errors.New("not paired")
	}
	// The running daemon first: it holds the key and pin in memory and a
	// live connection. Nothing is deleted unless it has let go, or holds
	// no lifecycle lock (it runs only with its socket and its lock).
	err = control.ErrNotRunning
	if e.sockErr == nil {
		err = control.SendUnlink(e.socket)
	}
	switch {
	case err == nil:
		fmt.Fprintln(e.stdout, "the running pneu dropped the link and closed its connections.")
	case errors.Is(err, control.ErrNotRunning):
		// Held across the deletion, so no daemon starts in the middle.
		lock, err := link.Lock(e.credDir)
		if errors.Is(err, link.ErrLocked) {
			return errors.New("a pneu client daemon is running but its control socket doesn't answer here; nothing was removed. Stop it (systemctl --user stop pneu) and run pneu client unpair again")
		}
		if err != nil {
			return err
		}
		defer lock.Close()
	default:
		return fmt.Errorf("the running pneu didn't confirm it dropped the link (%v); nothing was removed. Stop it (systemctl --user stop pneu) and run pneu client unpair again", err)
	}
	target, name := "", ""
	if raw.Server != nil {
		target = raw.Server.SSH
	}
	if c, err := link.LoadCreds(e.credDir); err == nil {
		target, name = c.Pin.SSH, c.Pin.Name
	}
	if e.beforeDelete != nil {
		e.beforeDelete()
	}
	if err := link.RemoveCreds(e.credDir); err != nil {
		return err
	}
	if e.midDelete != nil {
		e.midDelete()
	}
	if raw.Server != nil {
		raw.Server = nil
		if err := config.Write(e.cfgPath, raw); err != nil {
			return err
		}
	}
	fmt.Fprintln(e.stdout, "unpaired here: the pin, this machine's key and the config's server block are gone.")
	if !config.ValidSSHTarget(target) || !peer.ValidName(name) {
		fmt.Fprintln(e.stdout, "The server still admits the old key until you remove this machine there: pneu peer list, then pneu peer remove <name>.")
		return nil
	}
	fmt.Fprintf(e.stdout, "The server still admits the old key until you run:\n\n  ssh %s pneu peer remove %s\n", target, name)
	return nil
}

// cappedBuffer keeps at most max bytes; past that it fails the write,
// which ends exec's copy (and ssh gets EPIPE).
type cappedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.over = true
		return 0, errors.New("answer over its size cap")
	}
	return b.Buffer.Write(p)
}

// serveClient is `pneu serve` on a client: the same Auth on both
// loopbacks, local routes and the proxy (internal/client), the link, and
// the control socket for launch, status and unlink.
func serveClient(cfg config.Config, listen string) error {
	host := hostFor(cfg)
	launchPath, err := web.LaunchPath()
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
	dir, err := link.Dir()
	if err != nil {
		return err
	}
	auth := web.NewAuth(host, token)
	sock, sockErr := control.SocketPath()
	c, err := startClient(cfg, dir, sock, sockErr, tailscale.NewLocal(), auth)
	if err != nil {
		return err
	}
	defer c.close()

	lns, err := listenLoopbacks(cfg.Port, listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: c.d, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := auth.StartLaunch(launchPath); err != nil {
		return err
	}
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errc <- hs.Serve(ln) }()
		log.Printf("pneu (client of %s) listening on %s", cfg.Server.SSH, ln.Addr())
	}
	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	c.ctl.Close()
	if serveErr != nil {
		return serveErr
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// clientRun is a started client daemon: its lifecycle lock, its control
// socket, and the link and daemon once the credentials are loaded.
type clientRun struct {
	lock *os.File
	ctl  *control.Server

	mu       sync.Mutex
	lk       *link.Link
	d        *client.Daemon
	unlinked bool // unlink came before the link existed
}

// startClient takes the lifecycle lock, then the control socket, both
// before loading any credentials: `pneu client unpair` can only reach a
// daemon through the socket, and finds one without it by the lock (D1).
// Without either there's no daemon.
func startClient(cfg config.Config, dir, sock string, sockErr error, api link.API, auth *web.Auth) (*clientRun, error) {
	// Held from taking the lifecycle lock until the credentials are
	// loaded: an unpair in progress finishes first (E1).
	pair, err := link.PairLock(dir, link.PairWait)
	if errors.Is(err, link.ErrLocked) {
		return nil, errors.New("pneu client unpair is running; start pneu again once it's done")
	}
	if err != nil {
		return nil, err
	}
	defer pair.Close()
	lock, err := link.Lock(dir)
	if errors.Is(err, link.ErrLocked) {
		return nil, errors.New("another pneu client is running (or pneu client unpair is removing its pairing)")
	}
	if err != nil {
		return nil, err
	}
	c := &clientRun{lock: lock}
	fail := func(err error) (*clientRun, error) {
		c.close()
		return nil, err
	}
	if sockErr != nil {
		return fail(fmt.Errorf("client mode needs its control socket (%v): pneu client unpair reaches the daemon through it", sockErr))
	}
	if c.ctl, err = control.Listen(sock, control.Handler{Launch: c.launch, Client: true, Unlink: c.unlink}); err != nil {
		return fail(fmt.Errorf("client mode needs its control socket: %w", err))
	}
	creds, err := link.LoadCreds(dir)
	if err != nil {
		return fail(fmt.Errorf("%w (pair with pneu client pair <ssh-target>)", err))
	}
	if creds.Pin.Node != cfg.Server.Node {
		return fail(fmt.Errorf("config.json names server node %s but the pin is for %s: pneu client unpair, then pair again", cfg.Server.Node, creds.Pin.Node))
	}
	lk := link.New(api, creds, cfg.Server.Port)
	d := client.New(client.Config{Auth: auth, Link: lk})
	c.mu.Lock()
	unlinked := c.unlinked
	if !unlinked {
		c.lk, c.d = lk, d
		lk.Start()
	}
	c.mu.Unlock()
	if unlinked {
		return fail(errors.New("unpaired while starting"))
	}
	return c, nil
}

func (c *clientRun) launch() {
	c.mu.Lock()
	d := c.d
	c.mu.Unlock()
	if d != nil {
		d.Launch()
	}
}

// unlink is the control socket's: the link drops its pairing and returns
// once its connections are closed; before there's a link, none starts.
func (c *clientRun) unlink() error {
	c.mu.Lock()
	c.unlinked = true
	lk := c.lk
	c.mu.Unlock()
	if lk != nil {
		lk.Unpair()
	}
	return nil
}

func (c *clientRun) close() {
	if c.ctl != nil {
		c.ctl.Close()
	}
	c.mu.Lock()
	lk := c.lk
	c.mu.Unlock()
	if lk != nil {
		lk.Close()
	}
	c.lock.Close()
}
