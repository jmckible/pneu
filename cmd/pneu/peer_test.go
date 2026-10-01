package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
)

type fakeTS struct{ state string }

func (f fakeTS) Status(context.Context) (tailscale.Status, error) {
	return tailscale.Status{BackendState: f.state, Self: tailscale.Self{StableID: "nSERVER1CNTRL", UserID: 1}}, nil
}

func (fakeTS) WhoIs(context.Context, netip.AddrPort) (tailscale.WhoIs, error) {
	return tailscale.WhoIs{}, errors.New("unused")
}

func clientCertPEM(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "c"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func addRequest(t *testing.T, name, node, cert string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "node": node, "origin": "http://pneu.localhost:7317", "cert": cert})
	return string(b)
}

// testPeerEnv is a peer env in temp dirs; socket is a path with no daemon
// until the test starts one there.
func testPeerEnv(t *testing.T) (*peerEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	state := t.TempDir()
	sockBase, err := os.MkdirTemp("", "pneupeer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockBase) })
	var out, errb bytes.Buffer
	return &peerEnv{
		cfg:   config.Config{Port: 7317, Peer: &config.Peer{Port: 7320}},
		store: peer.Store{Dir: state}, keyDir: filepath.Join(state, "peer"),
		api: fakeTS{tailscale.Running}, socket: filepath.Join(sockBase, "pneu", "control"),
		host: "server", now: time.Now, stdout: &out, stderr: &errb,
	}, &out, &errb
}

type reload struct {
	gen  uint64
	hash string
}

// daemon answers peers-reload on e.socket with h.
func daemon(t *testing.T, e *peerEnv, h func(uint64, string) error) {
	t.Helper()
	ctl, err := control.Listen(e.socket, control.Handler{PeersReload: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
}

func TestPeerAdd(t *testing.T) {
	e, out, errb := testPeerEnv(t)
	got := make(chan reload, 4)
	daemon(t, e, func(g uint64, h string) error { got <- reload{g, h}; return nil })

	cert := clientCertPEM(t)
	e.stdin = strings.NewReader(addRequest(t, "macbook", "nCLIENT1CNTRL", cert))
	if err := e.add(); err != nil {
		t.Fatal(err)
	}
	// stdout is exactly one JSON object, of known fields.
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.DisallowUnknownFields()
	var res peer.AddResult
	if err := dec.Decode(&res); err != nil || dec.More() {
		t.Fatalf("stdout %q: %v", out, err)
	}
	id, err := peer.LoadOrCreateServer(e.keyDir, "server")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := peer.ParseCertPEM(res.Cert)
	if err != nil || peer.SPKI(sc) != id.SPKI || res.Node != "nSERVER1CNTRL" || res.Port != 7320 || res.Protocol != 1 || res.Name != "macbook" || res.Applied != "live" {
		t.Fatalf("result %+v %v", res, err)
	}
	f, _ := e.store.Load()
	if r := <-got; r.gen != 1 || r.hash != f.Hash || f.Generation != 1 || f.Peers[0].Name != "macbook" {
		t.Fatalf("reload %+v, file %+v", r, f)
	}
	if !strings.Contains(errb.String(), "live") {
		t.Fatalf("stderr %q", errb)
	}

	// The same name, node or key again is refused, and nothing is written.
	for _, in := range []string{
		addRequest(t, "macbook", "nOTHER1CNTRL", clientCertPEM(t)),
		addRequest(t, "air", "nCLIENT1CNTRL", clientCertPEM(t)),
		addRequest(t, "air", "nOTHER1CNTRL", cert),
		addRequest(t, "air", "nSERVER1CNTRL", clientCertPEM(t)), // this machine
		addRequest(t, "air", "nOTHER1CNTRL", id.PEM),            // this server's own key
	} {
		out.Reset()
		e.stdin = strings.NewReader(in)
		if err := e.add(); err == nil || out.Len() != 0 {
			t.Errorf("%s: %v, stdout %q", in[:40], err, out)
		}
	}
	if f2, _ := e.store.Load(); f2.Generation != 1 || len(got) != 0 {
		t.Fatalf("refusals wrote generation %d", f2.Generation)
	}
}

func TestPeerAddRefuses(t *testing.T) {
	e, out, _ := testPeerEnv(t)
	e.cfg.Peer = nil
	e.stdin = strings.NewReader(addRequest(t, "macbook", "nCLIENT1CNTRL", clientCertPEM(t)))
	if err := e.add(); err == nil || !strings.Contains(err.Error(), `"peer"`) {
		t.Fatalf("no peer block: %v", err)
	}
	e, out, _ = testPeerEnv(t)
	e.api = fakeTS{"Stopped"}
	e.stdin = strings.NewReader(addRequest(t, "macbook", "nCLIENT1CNTRL", clientCertPEM(t)))
	if err := e.add(); err == nil || out.Len() != 0 {
		t.Fatalf("tailscale stopped: %v", err)
	}
	if f, _ := e.store.Load(); f.Generation != 0 {
		t.Fatal("wrote without tailscale")
	}
	e.stdin = strings.NewReader(`{"name":"macbook"}`)
	e.api = fakeTS{tailscale.Running}
	if err := e.add(); err == nil {
		t.Fatal("partial request accepted")
	}
}

// Without a daemon the file is written and applies at the next start; a
// daemon that doesn't acknowledge leaves it pending, never "live".
func TestPeerApplyStates(t *testing.T) {
	e, out, errb := testPeerEnv(t)
	e.stdin = strings.NewReader(addRequest(t, "macbook", "nCLIENT1CNTRL", clientCertPEM(t)))
	if err := e.add(); err != nil {
		t.Fatal(err)
	}
	var res peer.AddResult
	json.Unmarshal(out.Bytes(), &res)
	if res.Applied != "next-start" || !strings.Contains(errb.String(), "next start") {
		t.Fatalf("no daemon: %+v %q", res, errb)
	}

	// A daemon without a peer listener: next start, too.
	e2, out2, _ := testPeerEnv(t)
	ctl, err := control.Listen(e2.socket, control.Handler{})
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	e2.stdin = strings.NewReader(addRequest(t, "macbook", "nCLIENT1CNTRL", clientCertPEM(t)))
	if err := e2.add(); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(out2.Bytes(), &res)
	if res.Applied != "next-start" {
		t.Fatalf("peers off: %+v", res)
	}

	// A daemon that fails the reload: pending.
	e3, out3, _ := testPeerEnv(t)
	daemon(t, e3, func(uint64, string) error { return errors.New("connection still closing") })
	e3.stdin = strings.NewReader(addRequest(t, "macbook", "nCLIENT1CNTRL", clientCertPEM(t)))
	if err := e3.add(); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(out3.Bytes(), &res)
	if res.Applied != "pending" {
		t.Fatalf("failed reload: %+v", res)
	}
	out3.Reset()
	if err := e3.remove("macbook"); !errors.Is(err, errPending) || !strings.Contains(out3.String(), "hasn't confirmed") {
		t.Fatalf("remove, failed reload: %v %q", err, out3)
	}

	// No control socket at all: pending, since a daemon may be running.
	e4, out4, _ := testPeerEnv(t)
	e4.socket, e4.sockErr = "", control.ErrNoRuntimeDir
	e4.stdin = strings.NewReader(addRequest(t, "macbook", "nCLIENT1CNTRL", clientCertPEM(t)))
	if err := e4.add(); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(out4.Bytes(), &res)
	if res.Applied != "pending" {
		t.Fatalf("no socket: %+v", res)
	}
}

func TestPeerRemoveAndList(t *testing.T) {
	e, out, _ := testPeerEnv(t)
	got := make(chan reload, 4)
	daemon(t, e, func(g uint64, h string) error { got <- reload{g, h}; return nil })
	for _, n := range []string{"macbook", "air"} {
		e.stdin = strings.NewReader(addRequest(t, n, "n"+n, clientCertPEM(t)))
		if err := e.add(); err != nil {
			t.Fatal(err)
		}
		<-got
	}
	out.Reset()
	if err := e.list(); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "macbook") || !strings.Contains(s, "nair") || !strings.Contains(s, "generation 2, 2 peer(s)") || strings.Contains(s, "BEGIN") {
		t.Fatalf("list:\n%s", s)
	}
	out.Reset()
	if err := e.remove("macbook"); err != nil {
		t.Fatal(err)
	}
	f, _ := e.store.Load()
	if r := <-got; r.gen != 3 || r.hash != f.Hash || len(f.Peers) != 1 || !strings.Contains(out.String(), "connections are closed") {
		t.Fatalf("remove: %+v %+v %q", r, f, out)
	}
	for _, bad := range []string{"macbook", "Mac Book", "../x", ""} {
		if err := e.remove(bad); err == nil {
			t.Errorf("remove %q: no error", bad)
		}
	}
	if len(got) != 0 {
		t.Fatal("a refused remove reloaded")
	}
}
