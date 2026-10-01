// Package linktest has the link's test fixtures: a client's tailscaled, a
// paired key set in temp dirs, and an upstream that holds the server's
// pinned key but answers however a test wants (a hostile or broken
// server). Tests only.
package linktest

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
	"github.com/jmckible/pneu/internal/web"
)

const (
	ServerNode = "nSERVER1CNTRL"
	ClientNode = "nCLIENT1CNTRL"
	User       = int64(131792011117475)
)

// API is the client's tailscaled: Running, the server's node online at
// Addrs, and a whois that vouches for the server node there.
type API struct {
	mu        sync.Mutex
	State     string
	Online    bool
	Missing   bool // the server's node isn't in the map
	Addrs     []netip.Addr
	StatusErr error
	// WhoIsFn, when set, answers instead of the default.
	WhoIsFn func(addr netip.AddrPort) (tailscale.WhoIs, error)
	WhoIsN  atomic.Int32
}

// NewAPI has the server at 127.0.0.1.
func NewAPI() *API {
	return &API{State: tailscale.Running, Online: true, Addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
}

// Set changes the fake under its lock.
func (a *API) Set(f func(*API)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

func (a *API) StatusPeers(context.Context) (tailscale.Status, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.StatusErr != nil {
		return tailscale.Status{}, a.StatusErr
	}
	st := tailscale.Status{BackendState: a.State, Self: tailscale.Self{StableID: ClientNode, UserID: User}, Peer: map[string]tailscale.PeerStatus{
		"nodekey:other": {StableID: "nOTHER1CNTRL", Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.99")}},
	}}
	if !a.Missing {
		st.Peer["nodekey:server"] = tailscale.PeerStatus{StableID: ServerNode, Online: a.Online, TailscaleIPs: a.Addrs,
			LastSeen: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	}
	return st, nil
}

func (a *API) WhoIs(_ context.Context, addr netip.AddrPort) (tailscale.WhoIs, error) {
	a.WhoIsN.Add(1)
	a.mu.Lock()
	f, addrs := a.WhoIsFn, a.Addrs
	a.mu.Unlock()
	if f != nil {
		return f(addr)
	}
	return ServerWhoIs(addrs...), nil
}

// ServerWhoIs is whois's answer for the server node at addrs.
func ServerWhoIs(addrs ...netip.Addr) tailscale.WhoIs {
	var ps []netip.Prefix
	for _, a := range addrs {
		ps = append(ps, netip.PrefixFrom(a, a.BitLen()))
	}
	return tailscale.WhoIs{
		Node:        tailscale.Node{StableID: ServerNode, Addresses: ps, User: User},
		UserProfile: tailscale.UserProfile{ID: User, LoginName: "a@b.example"},
	}
}

// Keys is a paired key set: the server's identity, and the client's
// credentials pinning it, in their own temp dirs.
type Keys struct {
	ServerDir string // the server's $XDG_STATE_HOME/pneu
	Server    peer.Identity
	ClientDir string // the client's peer dir
	Creds     link.Creds
}

// NewKeys makes both key pairs and writes the client's pin.
func NewKeys(t testing.TB) Keys {
	t.Helper()
	sdir := t.TempDir()
	srv, err := peer.LoadOrCreateServer(filepath.Join(sdir, "peer"), "server")
	if err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(t.TempDir(), "peer")
	if _, err := peer.LoadOrCreateIdentity(cdir, peer.ClientKeyFile, "pneu mac"); err != nil {
		t.Fatal(err)
	}
	pin := link.Pin{Name: "mac", SSH: "server", Node: ServerNode, Port: 7320, SPKI: srv.SPKI, Cert: srv.PEM, Protocol: web.Protocol, Paired: time.Now().UTC().Truncate(time.Second)}
	if err := link.WritePin(cdir, pin); err != nil {
		t.Fatal(err)
	}
	creds, err := link.LoadCreds(cdir)
	if err != nil {
		t.Fatal(err)
	}
	return Keys{ServerDir: sdir, Server: srv, ClientDir: cdir, Creds: creds}
}

// Upstream is a TLS 1.3, h2-only server holding a server key: pinned or
// not, it requires a client certificate whose SPKI is ClientPin (when
// set), answers /peer/hello (Hello), and hands everything else to Handler.
type Upstream struct {
	Port      int
	Requests  atomic.Int32 // requests past the handshake, hello included
	Conns     atomic.Int32 // connections accepted
	ClientPin string

	mu      sync.Mutex
	handler http.Handler
	hello   http.HandlerFunc
	srv     *http.Server
	ln      net.Listener
}

// DefaultHello is a well-formed hello with one account, personal at
// me@example.com.
func DefaultHello(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(web.ProtocolHeader, strconv.Itoa(web.Protocol))
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"protocol":` + strconv.Itoa(web.Protocol) + `,"name":"server","revision":"0123456789abcdef0123456789abcdef01234567","modified":false,"epoch":"abcd","gen":3,"accounts":[{"name":"personal","email":"me@example.com"}]}`))
}

// StartUpstream serves on 127.0.0.1 with id's key.
func StartUpstream(t testing.TB, id peer.Identity, clientPin string, h http.Handler) *Upstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &Upstream{Port: ln.Addr().(*net.TCPAddr).Port, ClientPin: clientPin, handler: h, hello: DefaultHello, ln: ln}
	protos := new(http.Protocols)
	protos.SetHTTP2(true)
	u.srv = &http.Server{
		Protocols: protos,
		TLSConfig: &tls.Config{
			Certificates:           []tls.Certificate{id.Cert},
			ClientAuth:             tls.RequireAnyClientCert,
			SessionTicketsDisabled: true,
			MinVersion:             tls.VersionTLS13,
			NextProtos:             []string{"h2"},
			VerifyConnection: func(cs tls.ConnectionState) error {
				if u.ClientPin != "" && (len(cs.PeerCertificates) != 1 || peer.SPKI(cs.PeerCertificates[0]) != u.ClientPin) {
					return errors.New("unpaired")
				}
				return nil
			},
		},
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				u.Conns.Add(1)
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u.Requests.Add(1)
			u.mu.Lock()
			h, hello := u.handler, u.hello
			u.mu.Unlock()
			if r.URL.Path == "/peer/hello" {
				hello(w, r)
				return
			}
			if h == nil {
				http.NotFound(w, r)
				return
			}
			h.ServeHTTP(w, r)
		}),
	}
	go u.srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { u.srv.Close() })
	return u
}

// SetHandler replaces what answers everything but hello.
func (u *Upstream) SetHandler(h http.Handler) {
	u.mu.Lock()
	u.handler = h
	u.mu.Unlock()
}

// SetHello replaces hello's answer.
func (u *Upstream) SetHello(h http.HandlerFunc) {
	u.mu.Lock()
	u.hello = h
	u.mu.Unlock()
}

// Close stops listening and closes every connection.
func (u *Upstream) Close() { u.srv.Close() }

// Host is the upstream's address as a Host header names it.
func (u *Upstream) Host() string { return "127.0.0.1:" + strconv.Itoa(u.Port) }
