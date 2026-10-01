package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
	"github.com/jmckible/pneu/internal/testmail"
	"github.com/jmckible/pneu/internal/web"
)

// serverTS is the server's tailscaled: this node is the server at
// 127.0.0.1, and whoever connects is the paired client's node.
type serverTS struct{}

func (serverTS) Status(context.Context) (tailscale.Status, error) {
	return tailscale.Status{BackendState: tailscale.Running, Self: tailscale.Self{StableID: linktest.ServerNode, UserID: linktest.User,
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}}, nil
}

func (serverTS) WhoIs(_ context.Context, addr netip.AddrPort) (tailscale.WhoIs, error) {
	a := addr.Addr()
	return tailscale.WhoIs{
		Node:        tailscale.Node{StableID: linktest.ClientNode, Addresses: []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}, User: linktest.User},
		UserProfile: tailscale.UserProfile{ID: linktest.User},
	}, nil
}

// realServer is step 4's peer listener in front of the real web server on
// the fixture mail, with this client paired at testOrigin. It returns the
// peer port.
func realServer(t *testing.T, keys linktest.Keys) (int, *web.Server) {
	t.Helper()
	env := testmail.Setup(t)
	var accounts []notmuch.Account
	for _, a := range env.Accounts {
		accounts = append(accounts, notmuch.Account{Name: a.Name, Email: a.Email, ConfigPath: a.NotmuchConfig, Maildir: filepath.Join(a.Root, "gmail", "mail")})
	}
	srv, err := web.New(accounts, "pneu.localhost:7317", strings.Repeat("0123456789abcdef", 2))
	if err != nil {
		t.Fatal(err)
	}
	store := peer.Store{Dir: keys.ServerDir}
	ps := peer.New(peer.Config{API: serverTS{}, Port: 0, Identity: keys.Server, Store: store,
		Handler: func(g *peer.Server) http.Handler { return srv.PeerHandler(g) }})
	ps.Start()
	t.Cleanup(func() { ps.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for len(ps.Addrs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the peer listener never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, err := store.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f, err := l.Add(peer.Record{Name: "mac", Node: linktest.ClientNode, SPKI: keys.Creds.Identity.SPKI, Origin: testOrigin, Added: time.Now().UTC().Truncate(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Reload(f.Generation, f.Hash); err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	_, port, _ := strings.Cut(ps.Addrs()[0], ":")
	n, _ := strconv.Atoi(port)
	return n, srv
}

// The whole path: a browser through the client's Auth, the proxy, the
// pinned link and the real peer listener, to the real handlers.
func TestEndToEnd(t *testing.T) {
	keys := linktest.NewKeys(t)
	port, _ := realServer(t, keys)
	r := newRig(t, keys, port, nil)
	r.waitUp()
	if h := r.link.Hello(); h == nil || len(h.Accounts) != 2 {
		t.Fatalf("hello %+v", h)
	}

	// A page: the server's HTML, rendered for this client's origin, under
	// the client's own class headers.
	resp, body := r.get("/", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `data-origin="`+testOrigin+`"`) ||
		resp.Header.Get(web.PolicyHeader) != "app" || resp.Header.Get("Content-Security-Policy") != web.AppCSP ||
		resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get(web.ProtocolHeader) != "" {
		t.Fatalf("GET /: %d %v\n%.300s", resp.StatusCode, resp.Header, body)
	}
	if resp, _ := r.get("/search?q=tag:inbox", nil); resp.StatusCode != 200 {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	if resp, body := r.get("/status", nil); resp.StatusCode != 200 || !json.Valid([]byte(body)) {
		t.Fatalf("status: %d %q", resp.StatusCode, body)
	}

	// A mutation needs the local Origin and cookie (Auth), and goes up
	// with neither: the empty form is the tag handler's own 400.
	resp, body = r.do(r.req("POST", "/tag", "", nil))
	if resp.StatusCode != 400 || resp.Header.Get(web.PolicyHeader) != "data" {
		t.Fatalf("POST /tag: %d %q", resp.StatusCode, body)
	}
	if resp, _ := r.do(r.req("POST", "/tag", "", map[string]string{"Origin": ""})); resp.StatusCode != 403 {
		t.Fatalf("POST /tag without Origin: %d", resp.StatusCode)
	}

	// Static: If-None-Match: * would get a 304 from the server, so a 200
	// with the body shows it was stripped; no validator comes back.
	resp, body = r.get("/static/app.css", map[string]string{"If-None-Match": "*", "If-Modified-Since": time.Now().UTC().Format(http.TimeFormat)})
	if resp.StatusCode != 200 || len(body) < 100 || resp.Header.Get("ETag") != "" || resp.Header.Get("Last-Modified") != "" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("static: %d %v", resp.StatusCode, resp.Header)
	}
	// The favicon, an SVG, only as static-svg: a sandbox if navigated to.
	if resp, _ := r.get("/static/favicon.svg", nil); resp.StatusCode != 200 || resp.Header.Get(web.PolicyHeader) != "static-svg" ||
		!strings.HasPrefix(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("favicon: %d %v", resp.StatusCode, resp.Header)
	}

	// Local routes: this desk's theme, /open with its nonce.
	if resp, body := r.get("/theme.css", nil); resp.StatusCode != 200 || !strings.Contains(body, "#102030") {
		t.Fatalf("theme: %d %q", resp.StatusCode, body)
	}
	launch := filepath.Join(t.TempDir(), "launch")
	if err := r.d.auth.StartLaunch(launch); err != nil {
		t.Fatal(err)
	}
	u, err := web.LaunchURL(launch, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	_, q, _ := strings.Cut(u, "?")
	resp, _ = r.get("/open?"+q, map[string]string{"Cookie": ""})
	if resp.StatusCode != 302 || len(resp.Header.Values("Set-Cookie")) != 1 {
		t.Fatalf("/open: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := r.get("/peer/hello", nil); resp.StatusCode != 404 {
		t.Fatalf("/peer/hello from the browser: %d", resp.StatusCode)
	}

	// A thread page and its Gmail redirect, from a real row: the redirect's
	// authuser is checked against hello's account address.
	resp, body = r.get("/", nil)
	attr := func(name string) string {
		i := strings.Index(body, name+`="`)
		if i < 0 {
			t.Fatalf("no %s in\n%.500s", name, body)
		}
		v := body[i+len(name)+2:]
		return v[:strings.IndexByte(v, '"')]
	}
	if resp, body := r.get(attr("data-url"), nil); resp.StatusCode != 200 || !strings.Contains(body, `data-origin="`+testOrigin+`"`) {
		t.Fatalf("thread: %d", resp.StatusCode)
	}
	if g := attr("data-gmail"); g != "" {
		resp, _ := r.get(g, nil)
		if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "https://mail.google.com/mail/?authuser=") {
			t.Fatalf("%s: %d %v", g, resp.StatusCode, resp.Header)
		}
	}
}

// Not sent: nothing was ever connected (the link down, a dial refused),
// so pressing again is safe. Unknown: bytes may have reached the server.
func TestMutationOutcome(t *testing.T) {
	t.Run("link down", func(t *testing.T) {
		keys := linktest.NewKeys(t)
		u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
		api := linktest.NewAPI()
		api.Set(func(a *linktest.API) { a.State = "Stopped" })
		r := newRig(t, keys, u.Port, api)
		waitState(t, r.link, link.TailscaleDown)
		assertNotSent(t, r)
		if resp, body := r.get("/", nil); resp.StatusCode != 502 || resp.Header.Get(LinkHeader) != NotSent || !strings.Contains(body, "tailscale-down") {
			t.Fatalf("GET while down: %d %v %q", resp.StatusCode, resp.Header, body)
		}
		if u.Requests.Load() != 0 || u.Conns.Load() != 0 {
			t.Fatal("something reached the server")
		}
	})
	t.Run("dial refused", func(t *testing.T) {
		keys := linktest.NewKeys(t)
		u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
		r := newRig(t, keys, u.Port, nil)
		r.waitUp()
		u.Close() // the link's connection dies; the port refuses
		time.Sleep(200 * time.Millisecond)
		assertNotSent(t, r)
	})
	t.Run("upstream reset after reading", func(t *testing.T) {
		keys := linktest.NewKeys(t)
		got := make(chan string, 1)
		u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/events" {
				http.NotFound(w, r)
				return
			}
			b, _ := io.ReadAll(r.Body)
			got <- string(b)
			panic(http.ErrAbortHandler) // the stream is reset: no answer
		}))
		r := newRig(t, keys, u.Port, nil)
		r.waitUp()
		resp, body := r.do(r.req("POST", "/tag", "archive=1", nil))
		var j struct {
			OK   bool
			Link string
		}
		if resp.StatusCode != 504 || resp.Header.Get(LinkHeader) != Unknown || json.Unmarshal([]byte(body), &j) != nil || j.OK || j.Link != Unknown {
			t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
		}
		if <-got != "archive=1" {
			t.Fatal("the server didn't get the body")
		}
	})
}

func assertNotSent(t *testing.T, r *rig) {
	t.Helper()
	resp, body := r.do(r.req("POST", "/tag", "archive=1", nil))
	var j struct {
		OK   bool
		Link string
	}
	if resp.StatusCode != 502 || resp.Header.Get(LinkHeader) != NotSent || json.Unmarshal([]byte(body), &j) != nil || j.OK || j.Link != NotSent {
		t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
	}
}

func waitState(t *testing.T, l *link.Link, want link.Reason) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.State().Reason != want {
		if time.Now().After(deadline) {
			t.Fatalf("link %+v, want %s", l.State(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// launch retries the link and asks the server to sync, without waiting;
// launches while one waits collapse.
func TestLaunch(t *testing.T) {
	keys := linktest.NewKeys(t)
	syncs := make(chan string, 4)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/sync" {
			syncs <- r.URL.RawQuery
		}
		answer(w, "data", "application/json", 200, `{"ok":true}`)
	}))
	r := newRig(t, keys, u.Port, nil)
	start := time.Now()
	r.d.Launch()
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("launch waited")
	}
	select {
	case q := <-syncs:
		if q != "reason=launch" {
			t.Fatalf("query %q", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no sync")
	}
}

// A request down the link that fails before anything connected is a
// DownError (not sent), the link's doing, not the proxy's guess.
func TestDownErrorIsNotSent(t *testing.T) {
	keys := linktest.NewKeys(t)
	api := linktest.NewAPI()
	api.Set(func(a *linktest.API) { a.Online = false })
	l := link.New(api, keys.Creds, 1)
	l.Start()
	defer l.Close()
	waitState(t, l, link.NodeOffline)
	req, _ := http.NewRequest("POST", "https://server/tag", strings.NewReader("x"))
	if _, err := l.RoundTrip(req); !errors.As(err, new(*link.DownError)) {
		t.Fatalf("%v", err)
	}
}

// The whole event path: the real server's /events over the peer listener
// into the daemon's one stream, out to a page through the local Hub, and
// the server's status into this desk's status.json.
func TestEventsEndToEnd(t *testing.T) {
	keys := linktest.NewKeys(t)
	port, srv := realServer(t, keys)
	r := newRig(t, keys, port, nil)
	r.waitUp()
	p := r.page()
	h := p.handshaken()
	if h.Link.State != "up" || h.Status != nil {
		t.Fatalf("hello %+v", h)
	}
	epoch := h.Epoch

	srv.Hub.Broadcast("theme", web.ThemeEvent{At: 1})
	srv.Hub.Broadcast("syncing", web.SyncingEvent{Account: "personal"})
	if ev := p.until("syncing", ownLinkUp); string(ev.data) != `{"account":"personal"}` {
		t.Fatalf("syncing %s", ev.data)
	}
	srv.ViewChanged()
	ev := p.until("view", ownLinkUp)
	var v web.ViewEvent
	if err := json.Unmarshal(ev.data, &v); err != nil || v.Epoch != epoch || v.Gen != h.Gen+1 {
		t.Fatalf("view %s", ev.data)
	}
	srv.Hub.Broadcast("sync", web.SyncEvent{Account: "work", Op: "sync", At: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)})
	if ev := p.until("sync", ownLinkUp); string(ev.data) != `{"account":"work","op":"sync","changed":false,"at":"2026-09-30T10:00:00Z"}` {
		t.Fatalf("sync %s", ev.data)
	}

	// The server's status file rewrite reaches this desk's status.json.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.RunStatus(ctx, filepath.Join(t.TempDir(), "status.json")); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	ev = p.until("status", ownLinkUp)
	var st web.StatusDoc
	if err := json.Unmarshal(ev.data, &st); err != nil || len(st.Accounts) != 2 {
		t.Fatalf("status %s", ev.data)
	}
	doc := waitV2(t, r.status, "the server's status", func(d StatusV2) bool { return d.Server.StatusAt != nil })
	if len(doc.Accounts) != 2 || doc.Unread != st.Unread || doc.Server.Link != "up" {
		t.Fatalf("status.json %+v", doc)
	}
	// A new page gets it all in its hello; the server's theme never came.
	p2 := r.page()
	h2 := p2.handshaken()
	if h2.Gen != v.Gen || h2.Status == nil || h2.Status.Unread != st.Unread {
		t.Fatalf("later hello %+v", h2)
	}
	p.quiet(100 * time.Millisecond)
}
