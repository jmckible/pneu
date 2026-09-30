package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
	"github.com/jmckible/pneu/internal/web"
)

const peerUsage = "usage: pneu peer add --stdin | list | remove <name>"

// peerEnv is what the peer commands touch, swapped out by tests.
type peerEnv struct {
	cfg     config.Config
	store   peer.Store
	keyDir  string // the server's identity ($XDG_STATE_HOME/pneu/peer)
	api     tailscale.API
	socket  string // the control socket; "" when there is none (socketErr)
	sockErr error
	host    string // this machine's hostname, for the certificate's name
	now     func() time.Time
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
}

// peerCmd is `pneu peer add|list|remove`, run on the server (docs/client.md,
// "Pairing"). stdout carries only the result; diagnostics go to stderr.
func peerCmd(args []string) error {
	if len(args) == 0 {
		return usageError{peerUsage}
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet("pneu peer "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stdin := fs.Bool("stdin", false, "read the pairing request from stdin (add)")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	env, err := newPeerEnv(cfg)
	if err != nil {
		return err
	}
	switch verb {
	case "add":
		if !*stdin || fs.NArg() != 0 {
			return usageError{"usage: pneu peer add --stdin (the request is JSON on stdin; `pneu client pair` sends it)"}
		}
		return env.add()
	case "list":
		if fs.NArg() != 0 {
			return usageError{peerUsage}
		}
		return env.list()
	case "remove":
		if fs.NArg() != 1 {
			return usageError{peerUsage}
		}
		return env.remove(fs.Arg(0))
	}
	return usageError{peerUsage}
}

func newPeerEnv(cfg config.Config) (*peerEnv, error) {
	state, err := web.StateDir()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	env := &peerEnv{
		cfg: cfg, store: peer.Store{Dir: state}, keyDir: filepath.Join(state, "peer"),
		api: tailscale.NewLocal(), host: host, now: time.Now,
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
	}
	env.socket, env.sockErr = control.SocketPath()
	return env, nil
}

// add pairs a client: validate the request, write the next generation
// under peers.lock, have the running server take it, and print what the
// client pins.
func (e *peerEnv) add() error {
	if e.cfg.Peer == nil {
		return errors.New(`no "peer" block in config.json: add "peer": {"port": 7320} and restart pneu (INSTALL.md)`)
	}
	_, rec, err := peer.ParseAddRequest(e.stdin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tailscale.CallTimeout)
	st, err := e.api.Status(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("asking tailscale who this machine is: %w", err)
	}
	if st.BackendState != tailscale.Running || st.Self.StableID == "" {
		return fmt.Errorf("tailscale is %s here; pairing needs it running", st.BackendState)
	}
	if rec.Node == st.Self.StableID {
		return errors.New("that node is this machine")
	}
	id, err := peer.LoadOrCreateServer(e.keyDir, e.host)
	if err != nil {
		return err
	}
	if rec.SPKI == id.SPKI {
		return errors.New("that key is this server's own")
	}
	lock, err := e.store.Lock(peer.LockWait)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	rec.Added = e.now().UTC().Truncate(time.Second)
	f, err := lock.Add(rec)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stderr, "pneu peer: paired %s (node %s) as generation %d\n", rec.Name, rec.Node, f.Generation)
	applied := e.apply(f)
	out, err := json.Marshal(peer.AddResult{
		Cert: id.PEM, Node: st.Self.StableID, Port: e.cfg.Peer.Port, Protocol: web.Protocol, Name: rec.Name, Applied: applied,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "%s\n", out)
	return err
}

// errPending: the running server didn't acknowledge; the outcome is unknown.
var errPending = errors.New("not acknowledged: pending")

// remove unpairs name: the next generation without it, and, from a
// running server, an acknowledgment that it's live and every connection
// from that peer is closed.
func (e *peerEnv) remove(name string) error {
	if !peer.ValidName(name) {
		return fmt.Errorf("%q isn't a peer name", name)
	}
	lock, err := e.store.Lock(peer.LockWait)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	f, rec, err := lock.Remove(name)
	if err != nil {
		return err
	}
	switch e.apply(f) {
	case "live":
		fmt.Fprintf(e.stdout, "removed %s (node %s): generation %d is live and its connections are closed\n", rec.Name, rec.Node, f.Generation)
	case "next-start":
		fmt.Fprintf(e.stdout, "removed %s (node %s): generation %d applies when pneu next starts\n", rec.Name, rec.Node, f.Generation)
	default:
		fmt.Fprintf(e.stdout, "removed %s from peers.json (generation %d), but the running pneu hasn't confirmed it: restart it (systemctl --user restart pneu) to be sure\n", rec.Name, f.Generation)
		return errPending
	}
	return nil
}

// apply asks the running server to load f, holding the lock throughout
// (R8), and says what came of it: "live", "next-start" or "pending",
// never claiming more than the acknowledgment says.
func (e *peerEnv) apply(f peer.File) string {
	if e.sockErr != nil {
		fmt.Fprintf(e.stderr, "pneu peer: no control socket (%v); a running pneu takes generation %d only when restarted: pending\n", e.sockErr, f.Generation)
		return "pending"
	}
	err := control.ReloadPeers(e.socket, f.Generation, f.Hash)
	switch {
	case err == nil:
		fmt.Fprintf(e.stderr, "pneu peer: generation %d is live\n", f.Generation)
		return "live"
	case errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(e.stderr, "pneu peer: pneu isn't running; generation %d applies at next start\n", f.Generation)
		return "next-start"
	case errors.Is(err, control.ErrPeersOff):
		fmt.Fprintf(e.stderr, "pneu peer: the running pneu has no peer listener; generation %d applies when it restarts with the peer block\n", f.Generation)
		return "next-start"
	default:
		fmt.Fprintf(e.stderr, "pneu peer: %v; generation %d is pending, not confirmed\n", err, f.Generation)
		return "pending"
	}
}

func (e *peerEnv) list() error {
	f, err := e.store.Load()
	if err != nil {
		return err
	}
	if len(f.Peers) == 0 {
		fmt.Fprintf(e.stdout, "no peers (generation %d)\n", f.Generation)
		return nil
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tNODE\tORIGIN\tADDED\tKEY")
	for _, p := range f.Peers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s…\n", p.Name, p.Node, p.Origin, p.Added.Local().Format("2006-01-02 15:04"), p.SPKI[:16])
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "generation %d, %d peer(s)\n", f.Generation, len(f.Peers))
	return nil
}
