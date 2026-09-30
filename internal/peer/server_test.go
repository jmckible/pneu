package peer

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/tailscale"
)

func get(t *testing.T, c *http.Client, url string) (*http.Response, string, error) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

func TestHandshake(t *testing.T) {
	var served atomic.Int32
	h := newHarness(t, func(s *Server) http.Handler {
		ok := okHandler(s)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Add(1); ok.ServeHTTP(w, r) })
	}, nil)
	paired := newIdentity(t)
	h.pair("macbook", clientNode, paired)

	// No client certificate: the handshake fails; no HTTP is parsed.
	if _, _, err := get(t, h.client(nil, nil), h.url("/")); err == nil {
		t.Fatal("no certificate: request succeeded")
	}
	// A certificate whose key isn't paired.
	stranger := newIdentity(t)
	if _, _, err := get(t, h.client(&stranger, nil), h.url("/")); err == nil {
		t.Fatal("unpaired key: request succeeded")
	}
	// Anything short of TLS 1.3.
	if _, _, err := get(t, h.client(&paired, func(c *tls.Config) { c.MinVersion = tls.VersionTLS12; c.MaxVersion = tls.VersionTLS12 }), h.url("/")); err == nil {
		t.Fatal("TLS 1.2: request succeeded")
	}
	// The paired key: HTTP/2, negotiated, with the peer identified.
	resp, body, err := get(t, h.client(&paired, nil), h.url("/"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || body != "ok macbook http://pneu.localhost:7317" || resp.ProtoMajor != 2 || resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("paired: %d %q %s %q", resp.StatusCode, body, resp.Proto, resp.TLS.NegotiatedProtocol)
	}
	if resp.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("version %x", resp.TLS.Version)
	}
	if served.Load() != 1 {
		t.Fatalf("handler ran %d times; refused handshakes must never reach it", served.Load())
	}
	// A paired key that doesn't offer h2 gets nothing: HTTP/1 is off.
	tr := &http.Transport{TLSClientConfig: h.clientTLS(&paired), Proxy: nil}
	tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	defer tr.CloseIdleConnections()
	if resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(h.url("/")); err == nil {
		resp.Body.Close()
		t.Fatalf("HTTP/1.1 answered %d", resp.StatusCode)
	}
	if served.Load() != 1 {
		t.Fatal("HTTP/1.1 reached the handler")
	}
	// The client's pin: a server with another key is refused by the
	// client's VerifyConnection (the mirror the client daemon builds).
	h.id.SPKI = newIdentity(t).SPKI // what the client pins
	if _, _, err := get(t, h.client(&paired, nil), h.url("/")); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("wrong server key: %v", err)
	}
}

// countingCache records every session a server would let a client resume.
type countingCache struct {
	tls.ClientSessionCache
	puts atomic.Int32
}

func (c *countingCache) Put(key string, cs *tls.ClientSessionState) {
	if cs != nil {
		c.puts.Add(1)
	}
	c.ClientSessionCache.Put(key, cs)
}

// Resumption is impossible: the server issues no tickets, so a client with
// a session cache still makes a full handshake, and admit (pin, whois,
// registration) runs on each connection.
func TestNoResumption(t *testing.T) {
	var admits atomic.Int32
	h := newHarness(t, okHandler, func(s *Server) { s.beforeRegister = func() { admits.Add(1) } })
	paired := newIdentity(t)
	h.pair("macbook", clientNode, paired)
	cache := &countingCache{ClientSessionCache: tls.NewLRUClientSessionCache(8)}
	c := h.client(&paired, func(cfg *tls.Config) { cfg.ClientSessionCache = cache })
	for i := range 3 {
		resp, _, err := get(t, c, h.url("/"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.TLS.DidResume {
			t.Fatalf("connection %d resumed", i)
		}
		c.Transport.(*http.Transport).CloseIdleConnections()
	}
	if admits.Load() != 3 || cache.puts.Load() != 0 {
		t.Fatalf("%d admits for 3 connections, %d tickets issued", admits.Load(), cache.puts.Load())
	}
}

// streamHandler serves /sse (an event every 20ms until the stream ends)
// and /big (an endless body), recording each handler's return, and
// captures each identified request for Identify afterwards.
type streamHandler struct {
	s        *Server
	sseDone  chan struct{}
	bigDone  chan struct{}
	mu       sync.Mutex
	lastReq  *http.Request
	sseOnce  sync.Once
	bigOnce  sync.Once
	requests atomic.Int32
}

func newStreamHandler() *streamHandler {
	return &streamHandler{sseDone: make(chan struct{}), bigDone: make(chan struct{})}
}

func (sh *streamHandler) build(s *Server) http.Handler {
	sh.s = s
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sh.requests.Add(1)
		if _, _, ok := s.Identify(r); !ok {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		sh.mu.Lock()
		sh.lastReq = r
		sh.mu.Unlock()
		rc := http.NewResponseController(w)
		switch r.URL.Path {
		case "/sse":
			defer sh.sseOnce.Do(func() { close(sh.sseDone) })
			w.Header().Set("Content-Type", "text/event-stream")
			for {
				if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
					return
				}
				if rc.Flush() != nil {
					return
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		case "/big":
			defer sh.bigOnce.Do(func() { close(sh.bigDone) })
			chunk := make([]byte, 32<<10)
			for {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				rc.Flush()
			}
		default:
			io.WriteString(w, "ok")
		}
	})
}

// open starts a streaming GET and returns its body once the first bytes
// have arrived.
func open(t *testing.T, c *http.Client, url string) io.ReadCloser {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d", url, resp.StatusCode)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	return resp.Body
}

// drainErr reads body until it fails, returning the error (nil if it just
// kept going for d).
func drainErr(body io.Reader, d time.Duration) error {
	errc := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		if err == nil {
			err = io.EOF
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-time.After(d):
		return nil
	}
}

// Removing a peer closes its live connection, every stream on it (an SSE
// stream and a download the client isn't reading), and Reload answers
// only after that connection's handlers have returned. Another peer's
// connection stays up.
func TestRemoveClosesConnections(t *testing.T) {
	sh := newStreamHandler()
	h := newHarness(t, sh.build, nil)
	two := netip.MustParseAddr("127.0.0.2")
	h.api.setWhois(func(_ context.Context, a netip.AddrPort) (tailscale.WhoIs, error) {
		w := goodWhoIs(a.Addr())
		if a.Addr() == two {
			w.Node.StableID = "nOTHER1CNTRL"
		}
		return w, nil
	})
	mac, other := newIdentity(t), newIdentity(t)
	h.pair("macbook", clientNode, mac)
	h.pair("other", "nOTHER1CNTRL", other)

	c := h.client(&mac, nil)
	sse := open(t, c, h.url("/sse"))
	defer sse.Close()
	big := open(t, c, h.url("/big")) // never read further: the server blocks on flow control
	defer big.Close()

	oc := h.client(&other, nil)
	oc.Transport.(*http.Transport).DialContext = (&net.Dialer{LocalAddr: &net.TCPAddr{IP: two.AsSlice()}}).DialContext
	osse := open(t, oc, h.url("/"))
	osse.Close()
	oc2 := open(t, oc, h.url("/sse")) // the other peer's stream, which must keep going
	defer oc2.Close()
	if n := h.registered(); n != 2 {
		t.Fatalf("%d connections registered, want 2", n)
	}

	// Capture a request of macbook's for Identify after removal.
	resp, _, err := get(t, c, h.url("/"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("macbook before removal: %v", err)
	}
	sh.mu.Lock()
	macReq := sh.lastReq
	sh.mu.Unlock()

	if err := h.unpair("macbook"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Answered only once the handlers are gone.
	for name, ch := range map[string]chan struct{}{"sse": sh.sseDone, "big": sh.bigDone} {
		select {
		case <-ch:
		default:
			t.Fatalf("reload answered while the %s handler still ran", name)
		}
	}
	if err := drainErr(sse, 2*time.Second); err == nil || err == io.EOF {
		t.Fatalf("sse after removal: %v, want a broken stream", err)
	}
	if err := drainErr(big, 2*time.Second); err == nil || err == io.EOF {
		t.Fatalf("download after removal: %v, want a broken stream", err)
	}
	// The per-request check refuses the old connection's requests too.
	if _, _, ok := h.s.Identify(macReq); ok {
		t.Fatal("Identify admitted a removed peer's connection")
	}
	// And the key no longer connects.
	if _, _, err := get(t, h.client(&mac, nil), h.url("/")); err == nil {
		t.Fatal("removed key connected")
	}
	// The other peer is untouched.
	if n := h.registered(); n != 1 {
		t.Fatalf("%d connections after removal, want the other peer's 1", n)
	}
	if err := drainErr(oc2, 200*time.Millisecond); err != nil {
		t.Fatalf("other peer's stream: %v", err)
	}
	if resp, body, err := get(t, oc, h.url("/")); err != nil || body != "ok" || resp.StatusCode != 200 {
		t.Fatalf("other peer after removal: %v %q", err, body)
	}
}

// A removal that lands between a handshake's whois and its registration
// wins: registration re-checks under the lock the removal takes (R2, R8).
func TestRemoveRacesHandshake(t *testing.T) {
	var h *harness
	var raced atomic.Bool
	h = newHarness(t, okHandler, func(s *Server) {
		s.beforeRegister = func() {
			if raced.CompareAndSwap(false, true) {
				if err := h.unpair("macbook"); err != nil {
					t.Errorf("unpair in the seam: %v", err)
				}
			}
		}
	})
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	if _, _, err := get(t, h.client(&mac, nil), h.url("/")); err == nil {
		t.Fatal("handshake registered after its peer was removed")
	}
	if !raced.Load() || h.registered() != 0 {
		t.Fatalf("raced %v, %d registered", raced.Load(), h.registered())
	}
}

func leaseTimes(s *Server) {
	s.lease = 400 * time.Millisecond
	s.renewBelow = 200 * time.Millisecond
	s.sweep = 50 * time.Millisecond
}

// A good whois renews the lease whatever the connection carries: a stream
// outlives many leases.
func TestLeaseRenews(t *testing.T) {
	sh := newStreamHandler()
	h := newHarness(t, sh.build, leaseTimes)
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	sse := open(t, h.client(&mac, nil), h.url("/sse"))
	defer sse.Close()
	before := h.api.whoisN.Load()
	if err := drainErr(sse, 1500*time.Millisecond); err != nil {
		t.Fatalf("stream ended under renewals: %v", err)
	}
	if n := h.api.whoisN.Load() - before; n < 3 {
		t.Fatalf("%d renewals in ~4 leases", n)
	}
}

// Without a renewal the lease runs out, and the connection closes with
// every stream on it, idle ones too. A whois error and a whois that hangs
// past its deadline are both "no renewal".
func TestLeaseExpires(t *testing.T) {
	for name, whois := range map[string]func(ctx context.Context, a netip.AddrPort) (tailscale.WhoIs, error){
		"error": func(context.Context, netip.AddrPort) (tailscale.WhoIs, error) {
			return tailscale.WhoIs{}, errors.New("tailscaled: connection refused")
		},
		"timeout": func(ctx context.Context, _ netip.AddrPort) (tailscale.WhoIs, error) {
			<-ctx.Done()
			return tailscale.WhoIs{}, ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			sh := newStreamHandler()
			h := newHarness(t, sh.build, leaseTimes)
			mac := newIdentity(t)
			h.pair("macbook", clientNode, mac)
			sse := open(t, h.client(&mac, nil), h.url("/sse"))
			defer sse.Close()
			// A second connection, idle after one request.
			idle := h.client(&mac, nil)
			if _, _, err := get(t, idle, h.url("/")); err != nil {
				t.Fatal(err)
			}
			if h.registered() != 2 {
				t.Fatalf("%d registered", h.registered())
			}
			h.api.setWhois(whois)
			start := time.Now()
			if err := drainErr(sse, 3*time.Second); err == nil || err == io.EOF {
				t.Fatalf("stream after the lease: %v", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("closed %v after renewals stopped; lease is 400ms", d)
			}
			waitFor(t, "every connection closed", time.Second, func() bool { return h.registered() == 0 })
			select {
			case <-sh.sseDone:
			case <-time.After(time.Second):
				t.Fatal("sse handler still running")
			}
		})
	}
}

// A renewal whois that answers but refuses closes at once, not at expiry.
func TestLeaseRefusalCloses(t *testing.T) {
	sh := newStreamHandler()
	h := newHarness(t, sh.build, func(s *Server) {
		s.lease = 10 * time.Second
		s.renewBelow = 10 * time.Second // every sweep renews
		s.sweep = 50 * time.Millisecond
	})
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	sse := open(t, h.client(&mac, nil), h.url("/sse"))
	defer sse.Close()
	h.api.setWhois(func(_ context.Context, a netip.AddrPort) (tailscale.WhoIs, error) {
		w := goodWhoIs(a.Addr())
		w.Node.Tags = []string{"tag:server"}
		return w, nil
	})
	start := time.Now()
	if err := drainErr(sse, 3*time.Second); err == nil || err == io.EOF {
		t.Fatalf("stream after a refusing whois: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("closed after %v", d)
	}
}

// Each clause of the predicate refuses on its own (R5).
func TestCheckWhoIs(t *testing.T) {
	a := netip.MustParseAddr("100.91.195.0")
	for name, tc := range map[string]struct {
		mod  func(*tailscale.WhoIs)
		addr netip.Addr
		ok   bool
	}{
		"good":             {func(*tailscale.WhoIs) {}, a, true},
		"good v4-mapped":   {func(*tailscale.WhoIs) {}, netip.AddrFrom16(a.As16()), true},
		"tagged":           {func(w *tailscale.WhoIs) { w.Node.Tags = []string{"tag:x"} }, a, false},
		"shared in":        {func(w *tailscale.WhoIs) { w.Node.Sharer = 99 }, a, false},
		"other user":       {func(w *tailscale.WhoIs) { w.UserProfile.ID = 5 }, a, false},
		"no user":          {func(w *tailscale.WhoIs) { w.UserProfile.ID = 0 }, a, false},
		"other node":       {func(w *tailscale.WhoIs) { w.Node.StableID = "nELSE" }, a, false},
		"not its address":  {func(w *tailscale.WhoIs) { w.Node.Addresses = []netip.Prefix{netip.MustParsePrefix("100.64.0.7/32")} }, a, false},
		"subnet route":     {func(w *tailscale.WhoIs) { w.Node.Addresses = []netip.Prefix{netip.MustParsePrefix("100.91.195.0/24")} }, a, false},
		"no addresses":     {func(w *tailscale.WhoIs) { w.Node.Addresses = nil }, a, false},
		"empty whois":      {func(w *tailscale.WhoIs) { *w = tailscale.WhoIs{} }, a, false},
		"only node's user": {func(w *tailscale.WhoIs) { w.Node.User = ourUser; w.UserProfile.ID = 7 }, a, false},
	} {
		w := goodWhoIs(a)
		tc.mod(&w)
		err := CheckWhoIs(w, tc.addr, clientNode, ourUser)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && !errors.Is(err, errRefused) {
			t.Errorf("%s: %v isn't a refusal", name, err)
		}
	}
}

// The predicate at the handshake: every failing whois refuses the
// connection before any request.
func TestHandshakeWhois(t *testing.T) {
	var served atomic.Int32
	h := newHarness(t, func(s *Server) http.Handler {
		ok := okHandler(s)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Add(1); ok.ServeHTTP(w, r) })
	}, nil)
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	mod := func(f func(*tailscale.WhoIs)) func(context.Context, netip.AddrPort) (tailscale.WhoIs, error) {
		return func(_ context.Context, a netip.AddrPort) (tailscale.WhoIs, error) {
			w := goodWhoIs(a.Addr())
			f(&w)
			return w, nil
		}
	}
	cases := map[string]func(context.Context, netip.AddrPort) (tailscale.WhoIs, error){
		"tagged":     mod(func(w *tailscale.WhoIs) { w.Node.Tags = []string{"tag:x"} }),
		"shared":     mod(func(w *tailscale.WhoIs) { w.Node.Sharer = 3 }),
		"wrong user": mod(func(w *tailscale.WhoIs) { w.UserProfile.ID = 3 }),
		"wrong node": mod(func(w *tailscale.WhoIs) { w.Node.StableID = "nELSE" }),
		"not its ip": mod(func(w *tailscale.WhoIs) { w.Node.Addresses = []netip.Prefix{netip.MustParsePrefix("100.64.0.7/32")} }),
		"no match": func(context.Context, netip.AddrPort) (tailscale.WhoIs, error) {
			return tailscale.WhoIs{}, tailscale.ErrNoMatch
		},
		"whois hangs": func(ctx context.Context, _ netip.AddrPort) (tailscale.WhoIs, error) {
			<-ctx.Done()
			return tailscale.WhoIs{}, ctx.Err()
		},
	}
	for name, whois := range cases {
		h.api.setWhois(whois)
		if _, _, err := get(t, h.client(&mac, nil), h.url("/")); err == nil {
			t.Errorf("%s: connected", name)
		}
	}
	if served.Load() != 0 || h.registered() != 0 {
		t.Fatalf("%d requests served, %d registered", served.Load(), h.registered())
	}
	h.api.setWhois(nil)
	if _, body, err := get(t, h.client(&mac, nil), h.url("/")); err != nil || !strings.HasPrefix(body, "ok macbook") {
		t.Fatalf("good whois: %v %q", err, body)
	}
}

// recordingListen records every address bound.
type recordingListen struct {
	mu    sync.Mutex
	addrs []string
}

func (r *recordingListen) listen(network, addr string) (net.Listener, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, addr)
	r.mu.Unlock()
	return net.Listen(network, addr)
}

// The listeners follow tailscaled: only its addresses, never a wildcard;
// vanished addresses and a stopped backend close listeners and the
// connections on them.
func TestReconcile(t *testing.T) {
	rec := &recordingListen{}
	sh := newStreamHandler()
	h := newHarness(t, sh.build, func(s *Server) { s.listen = rec.listen })
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	first := h.addr()
	if !h.s.HostOK(first) || !strings.HasPrefix(first, "127.0.0.1:") {
		t.Fatalf("first listener %q", first)
	}
	for _, bad := range []string{"127.0.0.1:1", "localhost" + first[len("127.0.0.1"):], "[::1]" + first[len("127.0.0.1"):], "", "pneu.localhost:7317"} {
		if h.s.HostOK(bad) {
			t.Errorf("HostOK(%q)", bad)
		}
	}
	sse := open(t, h.client(&mac, nil), h.url("/sse"))
	defer sse.Close()

	// The address moves; wildcards in the status are never bound.
	h.api.set(tailscale.Running, "127.0.0.2", "0.0.0.0", "::")
	waitFor(t, "the move", 3*time.Second, func() bool { return strings.HasPrefix(h.addr(), "127.0.0.2:") })
	if h.s.HostOK(first) {
		t.Fatal("old address still accepted as Host")
	}
	if err := drainErr(sse, 2*time.Second); err == nil || err == io.EOF {
		t.Fatalf("stream on a vanished address: %v", err)
	}
	waitFor(t, "connections closed", time.Second, func() bool { return h.registered() == 0 })
	if _, _, err := get(t, h.client(&mac, nil), "https://"+first+"/"); err == nil {
		t.Fatal("old address still answers")
	}
	if _, body, err := get(t, h.client(&mac, nil), h.url("/sse-not")); err != nil || body != "ok" {
		t.Fatalf("new address: %v %q", err, body)
	}

	// Stopped, then unreachable: nothing listens, nothing stays connected.
	sse = open(t, h.client(&mac, nil), h.url("/sse"))
	h.api.set("Stopped")
	h.waitListening(false)
	if err := drainErr(sse, 2*time.Second); err == nil || err == io.EOF {
		t.Fatalf("stream with tailscale stopped: %v", err)
	}
	h.api.mu.Lock()
	h.api.statusErr = errors.New("no tailscaled")
	h.api.mu.Unlock()
	time.Sleep(150 * time.Millisecond)
	if h.addr() != "" {
		t.Fatal("listening without tailscaled")
	}
	h.api.mu.Lock()
	h.api.statusErr = nil
	h.api.mu.Unlock()
	h.api.set(tailscale.Running, "127.0.0.1")
	h.waitListening(true)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, a := range rec.addrs {
		host, _, _ := net.SplitHostPort(a)
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
			t.Errorf("bound %q", a)
		}
	}
}

// Logs name peers, never certificate bytes or pins.
func TestLogsNameNoKeys(t *testing.T) {
	var buf lockedBuf
	restore := captureLog(&buf)
	defer restore()
	h := newHarness(t, okHandler, nil)
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	get(t, h.client(&mac, nil), h.url("/"))
	stranger := newIdentity(t)
	get(t, h.client(&stranger, nil), h.url("/"))
	h.unpair("macbook")
	out := buf.String()
	if !strings.Contains(out, "peer macbook: connected") || !strings.Contains(out, "unpaired key") {
		t.Fatalf("logs:\n%s", out)
	}
	for _, secret := range []string{mac.SPKI, stranger.SPKI, h.id.SPKI, "BEGIN", mac.SPKI[:16]} {
		if strings.Contains(out, secret) {
			t.Fatalf("logs carry key material %q:\n%s", secret, out)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "peer") {
			t.Errorf("unprefixed log line %q", sc.Text())
		}
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(w io.Writer) func() {
	flags, out := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(w)
	return func() { log.SetFlags(flags); log.SetOutput(out) }
}

// The pinned certificate is public; only its key admits. Go runs
// VerifyConnection before the client's CertificateVerify, so registration
// waits for the whole handshake: a client showing the paired certificate
// without its key is never registered, even for a moment.
func TestProofOfPossession(t *testing.T) {
	var registers atomic.Int32
	h := newHarness(t, okHandler, func(s *Server) { s.beforeRegister = func() { registers.Add(1) } })
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	impostor := newIdentity(t)
	forged := Identity{Cert: tls.Certificate{Certificate: mac.Cert.Certificate, PrivateKey: impostor.Cert.PrivateKey}}
	if _, _, err := get(t, h.client(&forged, nil), h.url("/")); err == nil {
		t.Fatal("paired certificate without its key was served")
	}
	if registers.Load() != 0 || h.registered() != 0 {
		t.Fatalf("%d registrations for a forged handshake", registers.Load())
	}
	if _, body, err := get(t, h.client(&mac, nil), h.url("/")); err != nil || !strings.HasPrefix(body, "ok macbook") || registers.Load() != 1 {
		t.Fatalf("real key: %v %q", err, body)
	}
}

// blockHandler serves /block by entering, then waiting for release while
// ignoring its request's cancellation, as a tag write or a started send
// does.
type blockHandler struct {
	entered, release, exited chan struct{}
}

func newBlockHandler() *blockHandler {
	return &blockHandler{entered: make(chan struct{}, 1), release: make(chan struct{}), exited: make(chan struct{})}
}

func (b *blockHandler) build(s *Server) http.Handler {
	ok := okHandler(s)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/block" {
			ok.ServeHTTP(w, r)
			return
		}
		if _, _, ok := s.Identify(r); !ok {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		b.entered <- struct{}{}
		<-b.release // not r.Context(): the work outlives the request
		close(b.exited)
	})
}

// startBlocked opens a connection of mac's, starts /block on it, and once
// the handler runs, drops the connection from the client side, waiting
// until net/http on the server is done with it.
func startBlocked(t *testing.T, h *harness, b *blockHandler, mac Identity) {
	t.Helper()
	var nc net.Conn
	var mu sync.Mutex
	c := h.client(&mac, nil)
	c.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		mu.Lock()
		nc = conn
		mu.Unlock()
		return conn, err
	}
	go c.Get(h.url("/block"))
	<-b.entered
	mu.Lock()
	nc.Close()
	mu.Unlock()
	waitFor(t, "the server's serve loop to end", 3*time.Second, func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		for c := range h.s.conns {
			if !c.servedDone {
				return false
			}
		}
		return true
	})
}

// P1: a client that started a request and hung up before `peer remove`:
// its connection is closed and revoked already, but its handler still
// runs. The reload waits for it.
func TestRemoveWaitsForClosedConnections(t *testing.T) {
	b := newBlockHandler()
	h := newHarness(t, b.build, func(s *Server) { s.closeWait = 3 * time.Second })
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	startBlocked(t, h, b, mac)

	done := make(chan error, 1)
	go func() { done <- h.unpair("macbook") }()
	select {
	case err := <-done:
		t.Fatalf("reload answered (%v) while a handler of the removed peer still ran", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(b.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reload never answered")
	}
	select {
	case <-b.exited:
	default:
		t.Fatal("acknowledged before the handler returned")
	}
	if h.registered() != 0 {
		t.Fatalf("%d still registered", h.registered())
	}
}

// P1: a reload that timed out waiting is an error, and the next reload of
// the same generation waits for the same connections again.
func TestReloadAfterTimeoutWaitsAgain(t *testing.T) {
	b := newBlockHandler()
	h := newHarness(t, b.build, func(s *Server) { s.closeWait = 200 * time.Millisecond })
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	startBlocked(t, h, b, mac)
	if err := h.unpair("macbook"); err == nil {
		t.Fatal("reload acknowledged with a handler still running")
	}
	f, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.Reload(f.Generation, f.Hash); err == nil {
		t.Fatal("second reload acknowledged with the handler still running")
	}
	close(b.release)
	<-b.exited
	if err := h.s.Reload(f.Generation, f.Hash); err != nil {
		t.Fatalf("reload once finished: %v", err)
	}
}

// P2: the lease runs from the handshake's whois, not from registration: a
// handshake stalled after its whois gets no extra time.
func TestLeaseFromWhois(t *testing.T) {
	const lease, stall = 400 * time.Millisecond, 300 * time.Millisecond
	sh := newStreamHandler()
	var h *harness
	var whoisAt atomic.Int64
	h = newHarness(t, sh.build, func(s *Server) {
		s.lease, s.renewBelow, s.sweep = lease, 200*time.Millisecond, 50*time.Millisecond
		s.beforeRegister = func() { time.Sleep(stall) }
	})
	mac := newIdentity(t)
	h.pair("macbook", clientNode, mac)
	var failing atomic.Bool
	h.api.setWhois(func(_ context.Context, a netip.AddrPort) (tailscale.WhoIs, error) {
		if failing.Load() {
			return tailscale.WhoIs{}, errors.New("tailscaled away")
		}
		whoisAt.Store(time.Now().UnixNano())
		return goodWhoIs(a.Addr()), nil
	})
	sse := open(t, h.client(&mac, nil), h.url("/sse"))
	defer sse.Close()
	failing.Store(true)
	if err := drainErr(sse, 3*time.Second); err == nil || err == io.EOF {
		t.Fatalf("stream after the lease: %v", err)
	}
	if d := time.Since(time.Unix(0, whoisAt.Load())); d > lease+150*time.Millisecond {
		t.Fatalf("closed %v after the last good whois; the lease is %v", d, lease)
	}
}
