package unsub

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicIPv4(t *testing.T) {
	refused := []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "10.255.255.255", "100.64.0.1", "100.127.255.254",
		"127.0.0.1", "127.9.9.9", "169.254.169.254", "172.16.0.1", "172.31.255.255",
		"192.0.0.1", "192.0.2.5", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.255",
		"198.51.100.7", "203.0.113.9", "224.0.0.1", "239.255.255.250", "240.0.0.1",
		"255.255.255.255", "::1", "2001:db8::1", "2606:4700::1111", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
	}
	for _, s := range refused {
		if PublicIPv4(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.0", "172.15.255.255", "172.32.0.0",
		"192.0.1.1", "192.169.0.1", "198.17.255.255", "198.20.0.0", "223.255.255.255", "::ffff:1.1.1.1"}
	for _, s := range allowed {
		if !PublicIPv4(netip.MustParseAddr(s)) {
			t.Errorf("%s refused", s)
		}
	}
}

// testServer is an httptest TLS server reachable as https://example.com
// (its certificate's name) through a dialer that ignores the pinned
// address. The test's policy allows only the addresses it names.
type testServer struct {
	srv    *httptest.Server
	dials  atomic.Int32
	dialed atomic.Value // the address the client pinned
	hits   atomic.Int32
	req    atomic.Value // *http.Request copy
	body   atomic.Value
}

func newTestServer(t *testing.T, h http.HandlerFunc) *testServer {
	ts := &testServer{}
	ts.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		ts.body.Store(string(b))
		ts.req.Store(r.Clone(context.Background()))
		h(w, r)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

// public is the test server's address: loopback, which only the test's
// policy allows.
var public = netip.MustParseAddr("127.0.0.1")

func (ts *testServer) client(answers []netip.Addr) *Client {
	c := NewClient()
	c.Resolve = func(context.Context, string) ([]netip.Addr, error) { return answers, nil }
	c.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		ts.dials.Add(1)
		ts.dialed.Store(network + " " + addr)
		if addr != "127.0.0.1:443" {
			return nil, errors.New("test dialer: unexpected address " + addr)
		}
		// Only the port moves: httptest can't listen on 443.
		var d net.Dialer
		return d.DialContext(ctx, network, ts.srv.Listener.Addr().String())
	}
	// The production policy plus exactly the test server's address.
	c.Allowed = func(a netip.Addr) bool { return a == public || PublicIPv4(a) }
	c.TLS = ts.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return c
}

func TestOneClickOK(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	c := ts.client([]netip.Addr{public})
	if cat := c.OneClick(context.Background(), "https://example.com/u?id=1"); cat != CatOK {
		t.Fatalf("category %q", cat)
	}
	r := ts.req.Load().(*http.Request)
	if r.Method != "POST" || r.URL.RequestURI() != "/u?id=1" || r.Host != "example.com" {
		t.Errorf("request %s %s host %s", r.Method, r.URL, r.Host)
	}
	if ts.body.Load() != OneClickArg || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("body %q type %q", ts.body.Load(), r.Header.Get("Content-Type"))
	}
	if r.Header.Get("User-Agent") != UserAgent || r.Header.Get("Referer") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		t.Errorf("headers %v", r.Header)
	}
	if got := ts.dialed.Load(); got != "tcp4 127.0.0.1:443" {
		t.Errorf("dialed %v", got)
	}
	// Explicit :443 is the same port.
	if cat := c.OneClick(context.Background(), "https://example.com:443/u"); cat != CatOK {
		t.Errorf(":443: %q", cat)
	}
}

func TestOneClickRefused(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	cases := []struct {
		name    string
		url     string
		answers []netip.Addr
	}{
		{"private", "https://example.com/u", []netip.Addr{netip.MustParseAddr("192.168.1.1")}},
		{"other loopback", "https://example.com/u", []netip.Addr{netip.MustParseAddr("127.0.0.2")}},
		{"metadata", "https://example.com/u", []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
		{"one public one private", "https://example.com/u", []netip.Addr{public, netip.MustParseAddr("10.0.0.1")}},
		{"private first", "https://example.com/u", []netip.Addr{netip.MustParseAddr("10.0.0.1"), public}},
		{"mapped private", "https://example.com/u", []netip.Addr{netip.MustParseAddr("::ffff:10.0.0.1")}},
		{"ipv6", "https://example.com/u", []netip.Addr{netip.MustParseAddr("2606:4700::1111")}},
		{"http", "http://example.com/u", []netip.Addr{public}},
		{"ip literal", "https://93.184.216.34/u", []netip.Addr{public}},
		{"userinfo", "https://a@example.com/u", []netip.Addr{public}},
	}
	for _, c := range cases {
		cl := ts.client(c.answers)
		if cat := cl.OneClick(context.Background(), c.url); cat != CatRefusedAddress {
			t.Errorf("%s: %q", c.name, cat)
		}
	}
	if cat := ts.client([]netip.Addr{public}).OneClick(context.Background(), "https://example.com:8443/u"); cat != CatRefusedPort || !Refused(cat) || Transport(cat) {
		t.Errorf("port: %q", cat)
	}
	if n := ts.dials.Load(); n != 0 {
		t.Errorf("%d dials for refused destinations", n)
	}
}

func TestOneClickFailures(t *testing.T) {
	redirect := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/elsewhere", http.StatusFound)
	})
	if cat := redirect.client([]netip.Addr{public}).OneClick(context.Background(), "https://example.com/u"); cat != "http-3xx" {
		t.Errorf("redirect: %q", cat)
	}
	if n := redirect.hits.Load(); n != 1 {
		t.Errorf("redirect followed: %d hits", n)
	}

	gone := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) })
	if cat := gone.client([]netip.Addr{public}).OneClick(context.Background(), "https://example.com/u"); cat != "http-4xx" {
		t.Errorf("410: %q", cat)
	}
	broken := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if cat := broken.client([]netip.Addr{public}).OneClick(context.Background(), "https://example.com/u"); cat != "http-5xx" {
		t.Errorf("502: %q", cat)
	}

	big := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("x", 20<<10))
	})
	if cat := big.client([]netip.Addr{public}).OneClick(context.Background(), "https://example.com/u"); cat == CatOK || !Transport(cat) {
		t.Errorf("oversized headers: %q", cat)
	}

	// The certificate names example.com, not this host.
	ok := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {})
	if cat := ok.client([]netip.Addr{public}).OneClick(context.Background(), "https://other.example/u"); cat != CatTLS {
		t.Errorf("wrong name: %q", cat)
	}
	// An untrusted certificate.
	cl := ok.client([]netip.Addr{public})
	cl.TLS = &tls.Config{}
	if cat := cl.OneClick(context.Background(), "https://example.com/u"); cat != CatTLS {
		t.Errorf("untrusted: %q", cat)
	}

	nx := ok.client(nil)
	nx.Resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	if cat := nx.OneClick(context.Background(), "https://example.com/u"); cat != CatDNS {
		t.Errorf("dns: %q", cat)
	}
	if cat := ok.client(nil).OneClick(context.Background(), "https://example.com/u"); cat != CatDNS {
		t.Errorf("no A records: %q", cat)
	}
}

func TestOneClickTimeout(t *testing.T) {
	defer func(d time.Duration) { oneClickWait = d }(oneClickWait)
	oneClickWait = 300 * time.Millisecond
	release := make(chan struct{})
	slow := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	start := time.Now()
	if cat := slow.client([]netip.Addr{public}).OneClick(context.Background(), "https://example.com/u"); cat != CatTimeout {
		t.Errorf("slow: %q", cat)
	}
	if d := time.Since(start); d > oneClickWait+time.Second {
		t.Errorf("took %v", d)
	}
}

func TestOneClickSlots(t *testing.T) {
	release := make(chan struct{})
	slow := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	c := slow.client([]netip.Addr{public})
	done := make(chan string, 3)
	for range 2 {
		go func() { done <- c.OneClick(context.Background(), "https://example.com/u") }()
	}
	for slow.hits.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if cat := c.OneClick(ctx, "https://example.com/u"); cat != CatBusy {
		t.Errorf("third in flight: %q", cat)
	}
	if n := slow.hits.Load(); n != 2 {
		t.Errorf("%d requests reached the server", n)
	}
	close(release)
	<-done
	<-done
}
