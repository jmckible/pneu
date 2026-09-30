package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	peerHost   = "100.64.0.1:7320"
	peerOrigin = "http://pneu.localhost:7400"
)

// fakeGuard admits every request while ok, counting the checks.
type fakeGuard struct {
	ok     atomic.Bool
	checks atomic.Int32
}

func newGuard() *fakeGuard { g := &fakeGuard{}; g.ok.Store(true); return g }

func (g *fakeGuard) Identify(*http.Request) (string, string, bool) {
	g.checks.Add(1)
	return "macbook", peerOrigin, g.ok.Load()
}

func (g *fakeGuard) HostOK(h string) bool { return h == peerHost }

func doPeer(h http.Handler, method, target, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = peerHost
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPeerHandler(t *testing.T) {
	f := newTagFixture(t)
	s := f.s
	g := newGuard()
	h := s.PeerHandler(g)

	proto := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if got := w.Header().Values(ProtocolHeader); len(got) != 1 || got[0] != "1" {
			t.Errorf("%s: %s %v", name, ProtocolHeader, got)
		}
		if w.Header().Get(PolicyHeader) == "" {
			t.Errorf("%s: no policy class", name)
		}
	}

	// No cookie, no Origin: the TLS peer is the credential. The page runs
	// in the peer's recorded origin, never the loopback's or a header's.
	w := doPeer(h, "GET", "/", "", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	proto("GET /", w)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `data-origin="`+peerOrigin+`"`) {
		t.Fatalf("GET / on peer: %d, origin missing:\n%.400s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/", withCookie); !strings.Contains(w.Body.String(), `data-origin="http://`+host+`"`) {
		t.Fatal("loopback page lost its own origin")
	}

	// Host must be one of the listener's addresses.
	for _, bad := range []string{host, "100.64.0.1", "100.64.0.2:7320", "pneu.localhost:7400", ""} {
		w := doPeer(h, "GET", "/", "", func(r *http.Request) { r.Host = bad })
		proto("host "+bad, w)
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %q: %d", bad, w.Code)
		}
	}

	// Identify runs on every request; a refusal is a 403 with the headers.
	before := g.checks.Load()
	doPeer(h, "GET", "/status", "", nil)
	doPeer(h, "GET", "/status", "", nil)
	if n := g.checks.Load() - before; n != 2 {
		t.Fatalf("Identify ran %d times for 2 requests", n)
	}
	g.ok.Store(false)
	w = doPeer(h, "GET", "/", "", nil)
	proto("refused", w)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unidentified peer: %d", w.Code)
	}
	g.ok.Store(true)

	// The client's own routes don't exist here; /peer/hello exists only here.
	for _, p := range []string{"/open", "/open?nonce=x", "/theme.css"} {
		if w := doPeer(h, "GET", p, "", nil); w.Code != http.StatusNotFound {
			t.Errorf("peer %s: %d", p, w.Code)
		}
	}
	if w := do(s, "GET", "/peer/hello", withCookie); w.Code != http.StatusNotFound {
		t.Errorf("loopback /peer/hello: %d", w.Code)
	}
	w = doPeer(h, "GET", "/peer/hello", "", nil)
	proto("hello", w)
	var hello helloDoc
	if err := json.Unmarshal(w.Body.Bytes(), &hello); err != nil || w.Code != 200 || hello.Protocol != Protocol || hello.Epoch != s.view.epoch || hello.Name == "" {
		t.Fatalf("hello: %d %s %+v %v", w.Code, w.Body, hello, err)
	}

	// Mutations need no Origin: POST /sync (the client's launch) syncs.
	w = doPeer(h, "POST", "/sync", "reason=launch", nil)
	proto("sync", w)
	if w.Code != http.StatusOK || f.sync.syncs != len(f.env.Accounts) {
		t.Fatalf("peer POST /sync: %d %s, %d syncs", w.Code, w.Body, f.sync.syncs)
	}
	// Reauth's consent is this machine's; refused over the link.
	if w := doPeer(h, "POST", "/accounts/personal/reauth", "", nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "reauth-on-server") {
		t.Fatalf("peer reauth: %d %s", w.Code, w.Body)
	}

	// /events streams to a peer (the client's one upstream stream).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		r := httptest.NewRequestWithContext(ctx, "GET", "/events", nil)
		r.Host = peerHost
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case w := <-done:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "event: hello") {
			t.Fatalf("peer /events: %d %.200s", w.Code, w.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer /events didn't end with its request")
	}
}

// The peer mux is the table: Local routes absent unless Upstream, PeerOnly
// routes only there.
func TestPeerRoutes(t *testing.T) {
	s := newServer(t)
	loop, peer := s.serveRoutes(), s.peerRoutes()
	for _, rt := range Routes {
		r := httptest.NewRequest(rt.Method, samplePath(rt.Pattern), nil)
		_, lp := loop.Handler(r)
		_, pp := peer.Handler(r)
		key := rt.Method + " " + rt.Pattern
		wantPeer := !rt.Local || rt.Upstream || rt.PeerOnly
		if (pp == key) != wantPeer || (lp == key) == rt.PeerOnly {
			t.Errorf("%s: loopback %q peer %q", key, lp, pp)
		}
	}
	for _, p := range []string{"/open", "/theme.css"} {
		if _, pat := peer.Handler(httptest.NewRequest("GET", p, nil)); pat != "" {
			t.Errorf("peer serves %s", p)
		}
	}
}
