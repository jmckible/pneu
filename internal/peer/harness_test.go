package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/tailscale"
)

const (
	serverNode = "nSERVER1CNTRL"
	clientNode = "nCLIENT1CNTRL"
	ourUser    = int64(131792011117475)
)

// fakeAPI is tailscaled: a status, and a whois that by default vouches for
// clientNode at whatever address asks.
type fakeAPI struct {
	mu        sync.Mutex
	state     string
	ips       []netip.Addr
	statusErr error
	// whois, when set, answers instead of the default.
	whois  func(ctx context.Context, addr netip.AddrPort) (tailscale.WhoIs, error)
	whoisN atomic.Int32
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{state: tailscale.Running, ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
}

func (f *fakeAPI) Status(context.Context) (tailscale.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return tailscale.Status{}, f.statusErr
	}
	return tailscale.Status{BackendState: f.state, Self: tailscale.Self{StableID: serverNode, UserID: ourUser, TailscaleIPs: f.ips}}, nil
}

func (f *fakeAPI) WhoIs(ctx context.Context, addr netip.AddrPort) (tailscale.WhoIs, error) {
	f.whoisN.Add(1)
	f.mu.Lock()
	h := f.whois
	f.mu.Unlock()
	if h != nil {
		return h(ctx, addr)
	}
	return goodWhoIs(addr.Addr()), nil
}

func (f *fakeAPI) setWhois(h func(ctx context.Context, addr netip.AddrPort) (tailscale.WhoIs, error)) {
	f.mu.Lock()
	f.whois = h
	f.mu.Unlock()
}

func (f *fakeAPI) set(state string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
	f.ips = nil
	for _, ip := range ips {
		f.ips = append(f.ips, netip.MustParseAddr(ip))
	}
}

func goodWhoIs(a netip.Addr) tailscale.WhoIs {
	return tailscale.WhoIs{
		Node:        tailscale.Node{StableID: clientNode, Addresses: []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}, User: ourUser},
		UserProfile: tailscale.UserProfile{ID: ourUser, LoginName: "a@b.example"},
	}
}

// newIdentity is a fresh key pair as a client would make one.
func newIdentity(t *testing.T) Identity {
	t.Helper()
	b, err := newIdentityPEM("pneu test")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	cert.Leaf = leaf
	return Identity{Cert: cert, SPKI: SPKI(leaf)}
}

type harness struct {
	t     *testing.T
	s     *Server
	api   *fakeAPI
	store Store
	id    Identity // the server's
}

// newHarness starts a peer Server on loopback (the fake's "tailnet"
// address), port 0, with short timings; h serves admitted requests.
func newHarness(t *testing.T, h func(*Server) http.Handler, tune func(*Server)) *harness {
	t.Helper()
	dir := t.TempDir()
	id, err := LoadOrCreateServer(filepath.Join(dir, "peer"), "server")
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI()
	store := Store{Dir: dir}
	s := New(Config{API: api, Port: 0, Identity: id, Store: store, Handler: h})
	s.reconcile = 50 * time.Millisecond
	if tune != nil {
		tune(s)
	}
	s.Start()
	t.Cleanup(func() { s.Close() })
	hs := &harness{t: t, s: s, api: api, store: store, id: id}
	hs.waitListening(true)
	return hs
}

// addr is the one listener's host:port.
func (h *harness) addr() string {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	for _, bl := range h.s.lns {
		return bl.host
	}
	return ""
}

func (h *harness) waitListening(want bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for (h.addr() != "") != want {
		if time.Now().After(deadline) {
			h.t.Fatalf("listening never became %v", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pair adds a peer for id and makes it live, as `pneu peer add` does.
func (h *harness) pair(name, node string, id Identity) {
	h.t.Helper()
	h.pairOrigin(name, node, id, "http://pneu.localhost:7317")
}

func (h *harness) pairOrigin(name, node string, id Identity, origin string) {
	h.t.Helper()
	l, err := h.store.Lock(time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	defer l.Unlock()
	f, err := l.Add(Record{Name: name, Node: node, SPKI: id.SPKI, Origin: origin, Added: time.Now().UTC().Truncate(time.Second)})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.s.Reload(f.Generation, f.Hash); err != nil {
		h.t.Fatal(err)
	}
}

// unpair removes name, as `pneu peer remove` does, returning Reload's answer.
func (h *harness) unpair(name string) error {
	h.t.Helper()
	l, err := h.store.Lock(time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	defer l.Unlock()
	f, _, err := l.Remove(name)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.s.Reload(f.Generation, f.Hash)
}

// clientTLS is the client's side: its certificate, the server's key pinned
// in VerifyConnection (chain verification off: the certificate is
// self-signed), TLS 1.3 and h2.
func (h *harness) clientTLS(id *Identity) *tls.Config {
	pin := h.id.SPKI
	cfg := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"h2"},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) != 1 || SPKI(cs.PeerCertificates[0]) != pin {
				return errors.New("server pin mismatch")
			}
			return nil
		},
	}
	if id != nil {
		cfg.Certificates = []tls.Certificate{id.Cert}
	}
	return cfg
}

// client is an HTTP/2-only client presenting id (nil: no certificate).
func (h *harness) client(id *Identity, mod func(*tls.Config)) *http.Client {
	cfg := h.clientTLS(id)
	if mod != nil {
		mod(cfg)
	}
	protos := new(http.Protocols)
	protos.SetHTTP2(true)
	tr := &http.Transport{TLSClientConfig: cfg, Protocols: protos, Proxy: nil, MaxIdleConnsPerHost: 1}
	h.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func (h *harness) url(path string) string { return "https://" + h.addr() + path }

// registered is how many peer connections are live.
func (h *harness) registered() int {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return len(h.s.conns)
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// okHandler answers "ok <peer> <origin>" to an identified request, 403
// otherwise, like web's guard.
func okHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, origin, ok := s.Identify(r)
		if !ok || !s.HostOK(r.Host) {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, "ok %s %s", name, origin)
	})
}
