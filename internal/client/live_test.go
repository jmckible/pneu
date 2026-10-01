package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/web"
)

// page is a browser's /events through the daemon's Auth.
type page struct {
	t   *testing.T
	evs chan sseEvent
}

func (r *rig) page() *page {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	req := r.req("GET", "/events", "", map[string]string{"Accept": "text/event-stream"}).WithContext(ctx)
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get(web.PolicyHeader) != "data" {
		r.t.Fatalf("/events: %d %v", resp.StatusCode, resp.Header)
	}
	p := &page{t: r.t, evs: make(chan sseEvent, 256)}
	go func() {
		defer resp.Body.Close()
		defer close(p.evs)
		sr := newSSEReader(resp.Body, 1<<20)
		for {
			ev, err := sr.next()
			if err != nil {
				return
			}
			p.evs <- ev
		}
	}()
	return p
}

// next is the page's next event.
func (p *page) next() sseEvent {
	p.t.Helper()
	select {
	case ev, ok := <-p.evs:
		if !ok {
			p.t.Fatal("the page's stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		p.t.Fatal("no event")
	}
	return sseEvent{}
}

// until skips to the next event named name, failing on any skipped one
// that skip doesn't allow.
func (p *page) until(name string, skip func(sseEvent) bool) sseEvent {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			p.t.Fatalf("no %s", name)
		}
		ev := p.next()
		if ev.name == name {
			return ev
		}
		if skip == nil || !skip(ev) {
			p.t.Fatalf("got %s %s waiting for %s", ev.name, ev.data, name)
		}
	}
}

// quiet: nothing more arrives for a moment.
func (p *page) quiet(d time.Duration) {
	p.t.Helper()
	select {
	case ev, ok := <-p.evs:
		if ok {
			p.t.Fatalf("unexpected %s %s", ev.name, ev.data)
		}
	case <-time.After(d):
	}
}

// handshaken skips to a hello that carries the server's epoch: the page
// may have subscribed before the daemon's stream opened.
func (p *page) handshaken() localHello {
	p.t.Helper()
	for {
		ev := p.until("hello", ownLinkUp)
		var h localHello
		if err := json.Unmarshal(ev.data, &h); err != nil {
			p.t.Fatal(err)
		}
		if h.Epoch != "" {
			return h
		}
	}
}

// ownLinkUp is the daemon's own `link` saying up: a page may see the
// link's arrival after it subscribed.
func ownLinkUp(ev sseEvent) bool {
	return ev.name == "link" && bytes.Contains(ev.data, []byte(`"state":"up"`)) && bytes.Contains(ev.data, []byte(`"server":"server"`))
}

func block(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

// helloBlock is a valid upstream hello for linktest's link hello (one
// account, personal; epoch abcd at gen 3).
func helloBlock(status string) string {
	if status == "" {
		status = "null"
	}
	return block("hello", `{"epoch":"abcd","gen":3,"accounts":[{"name":"personal","state":"ready","pulled":true,"failures":0,"error":null,"authing":false,"progress":null,"lastSync":"2026-09-30T10:00:00Z","queued":false,"running":false}],"status":`+status+`}`)
}

// script is a server's /events: the preamble, then once release closes
// (nil: at once) each block in turn, then held open. Each request to it
// is counted.
type script struct {
	mu      sync.Mutex
	blocks  []string
	release chan struct{}
	opened  atomic.Int32
	// perOpen, when set, is the blocks for the nth open (from 1); a block
	// sleep(d) pauses, endless writes bytes with no line end until the
	// stream goes. at is when each open came.
	perOpen func(n int) []string
	at      []time.Time
}

const endless = "\x00endless"

func sleep(d time.Duration) string { return "\x00sleep:" + d.String() }

func (s *script) set(release chan struct{}, blocks ...string) *script {
	s.mu.Lock()
	s.blocks, s.release = blocks, release
	s.mu.Unlock()
	return s
}

func newScript(release chan struct{}, blocks ...string) *script {
	return new(script).set(release, blocks...)
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/events" {
		answer(w, "data", "application/json", 200, `{"ok":true}`)
		return
	}
	n := int(s.opened.Add(1))
	s.mu.Lock()
	blocks, release := s.blocks, s.release
	if s.perOpen != nil {
		blocks = s.perOpen(n)
	}
	s.at = append(s.at, time.Now())
	s.mu.Unlock()
	w.Header().Set(web.ProtocolHeader, strconv.Itoa(web.Protocol))
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	io.WriteString(w, ": open\n\n")
	rc.Flush()
	if release != nil {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
	}
	for _, b := range blocks {
		switch {
		case b == endless:
			chunk := strings.Repeat("x", 4096)
			for r.Context().Err() == nil {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
				rc.Flush()
			}
			return
		case strings.HasPrefix(b, "\x00sleep:"):
			d, _ := time.ParseDuration(strings.TrimPrefix(b, "\x00sleep:"))
			time.Sleep(d)
			continue
		}
		if _, err := io.WriteString(w, b); err != nil {
			return
		}
		rc.Flush()
	}
	<-r.Context().Done()
}

// opens is when each stream was opened.
func (s *script) opens() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time{}, s.at...)
}

// scriptRig is a daemon whose server answers /events from s, which is
// set before the link (and so the stream) comes up.
func scriptRig(t *testing.T, s *script, opts ...func(*Daemon)) (*rig, *linktest.Upstream, *script) {
	t.Helper()
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, s)
	r := newRig(t, keys, u.Port, nil, opts...)
	r.waitUp()
	return r, u, s
}

// The handshake, then the allowlist: every event is decoded, held to
// shape and re-encoded, or dropped; nothing the daemon says itself is
// taken from upstream.
// lockedBuf is a log sink safe for concurrent writers.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
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

func TestUpstreamAllowlist(t *testing.T) {
	// An event the relay admits must encode: a failure there would be
	// logged per event, outside the drop path's limiter (H1).
	logs := &lockedBuf{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	t.Cleanup(func() {
		if strings.Contains(logs.String(), "sse: marshal") {
			t.Errorf("an admitted event failed to encode:\n%s", logs.String())
		}
	})
	release := make(chan struct{})
	pad := strings.Repeat("x", MaxEvent)
	hostileAccount := `{"name":"personal","state":"ready","pulled":true,"failures":-3,"lastSync":"yesterday",` +
		`"error":"<img src=x onerror=alert(1)>\u202e\u0007tail","evil":"<script>alert(1)</script>","progress":{"phase":"content","done":5,"total":10,"percent":250}}`
	r, _, _ := scriptRig(t, newScript(release,
		helloBlock(""),
		block("link", `{"state":"down","reason":"pin-mismatch","server":"evil"}`),
		block("theme", `{"at":1}`),
		helloBlock(""),
		block("message", `{"account":"personal"}`),
		"data: {\"account\":\"personal\"}\n\n", // unnamed
		block("syncing", `{"account":"work"}`),
		block("syncing", `{"account":"personal","pad":"`+pad+`"}`),
		block("view", `{"epoch":"abcd","gen":3,"from":""}`),
		block("view", `{"epoch":"abcd","gen":2,"from":""}`),
		block("view", `{"epoch":"ffff","gen":9,"from":""}`),
		block("view", `{"epoch":"abcd","gen":9,"from":"<b>"}`),
		block("view", `{"epoch":"abcd","gen":9,"threads":[{"account":"work","thread":"0000000000000abc"}]}`),
		block("account", hostileAccount),
		block("account", `{"name":"work","state":"ready"}`),
		block("account", `{"name":"personal","state":"<b>owned</b>"}`),
		block("view", `{"epoch":"abcd","gen":4,"from":"0123456789abcdef0123456789abcdef","threads":[{"account":"personal","thread":"0000000000000abc"}],"x":1}`),
		block("view", `{"epoch":"abcd","gen":4,"from":""}`),
		block("sync", `{"account":"personal","op":"rm -rf","changed":true,"at":"2026-09-30T10:00:00Z"}`),
		block("sync", `{"account":"personal","op":"sync","changed":false,"at":"0000-01-01T00:00:00+01:00"}`), // UTC year -1 (H1)
		block("sync", `{"account":"personal","op":"sync","changed":false,"at":"2026-09-30T11:00:00+01:00"}`),
		block("auth", `{"account":"work","ok":true,"error":""}`),
		block("syncing", `{"account":"personal"}`),
	))
	p := r.page()
	if h := p.until("hello", ownLinkUp); !bytes.Contains(h.data, []byte(`"epoch":""`)) {
		t.Fatalf("a page's hello before the handshake: %s", h.data)
	}
	close(release)
	h := p.until("hello", ownLinkUp)
	var hello localHello
	if err := json.Unmarshal(h.data, &hello); err != nil || hello.Epoch != "abcd" || hello.Gen != 3 || len(hello.Accounts) != 1 || hello.Link.State != "up" {
		t.Fatalf("handshake hello %s", h.data)
	}
	acct := p.until("account", ownLinkUp)
	for _, bad := range []string{"evil", "script", "<", "\u202e", "\u0007", "yesterday", "250"} {
		if bytes.Contains(acct.data, []byte(bad)) {
			t.Errorf("account passed %q: %s", bad, acct.data)
		}
	}
	var av web.AccountView
	if err := json.Unmarshal(acct.data, &av); err != nil || av.Failures != 0 || av.LastSync != nil || av.Error == nil ||
		*av.Error != "<img src=x onerror=alert(1)>tail" || av.Progress == nil || av.Progress.Percent != nil || av.Progress.Done != 5 {
		t.Fatalf("account %s", acct.data)
	}
	if !bytes.Contains(acct.data, []byte(`\u003cimg`)) {
		t.Errorf("markup went on unescaped: %s", acct.data)
	}
	view := p.until("view", ownLinkUp)
	if string(view.data) != `{"epoch":"abcd","gen":4,"from":"0123456789abcdef0123456789abcdef","threads":[{"account":"personal","thread":"0000000000000abc"}]}` {
		t.Fatalf("view %s", view.data)
	}
	if ev := p.until("sync", ownLinkUp); string(ev.data) != `{"account":"personal","op":"sync","changed":false,"at":"2026-09-30T10:00:00Z"}` {
		t.Fatalf("sync %s", ev.data)
	}
	if ev := p.until("syncing", ownLinkUp); string(ev.data) != `{"account":"personal"}` {
		t.Fatalf("syncing %s", ev.data)
	}
	p.quiet(200 * time.Millisecond)

	// A page that subscribes now gets the daemon's state: gen 4, and
	// personal running with the `syncing` applied.
	p2 := r.page()
	h = p2.until("hello", ownLinkUp)
	if err := json.Unmarshal(h.data, &hello); err != nil || hello.Gen != 4 || len(hello.Accounts) != 1 || !hello.Accounts[0].Running {
		t.Fatalf("later hello %s", h.data)
	}
}

// The first event must be hello. A stream that opens with anything else,
// or with a hello naming accounts outside the link's set, is closed and
// opened again; nothing on it reaches a page.
func TestUpstreamHelloFirst(t *testing.T) {
	for name, first := range map[string]string{
		"syncing first":   block("syncing", `{"account":"personal"}`),
		"oversized hello": block("hello", `{"epoch":"abcd","gen":3,"accounts":[],"pad":"`+strings.Repeat("x", MaxEvent)+`"}`),
		"foreign account": block("hello", `{"epoch":"abcd","gen":3,"accounts":[{"name":"work","state":"ready"}]}`),
		"bad epoch":       block("hello", `{"epoch":"<b>","gen":3,"accounts":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, _, s := scriptRig(t, newScript(nil, first, helloBlock(""), block("syncing", `{"account":"personal"}`)))
			p := r.page()
			p.until("hello", ownLinkUp)
			deadline := time.Now().Add(5 * time.Second)
			for s.opened.Load() < 2 {
				if time.Now().After(deadline) {
					t.Fatal("the stream was never opened again")
				}
				time.Sleep(10 * time.Millisecond)
			}
			for {
				select {
				case ev := <-p.evs:
					if !ownLinkUp(ev) {
						t.Fatalf("%s reached the page: %s", ev.name, ev.data)
					}
					continue
				case <-time.After(100 * time.Millisecond):
				}
				break
			}
			r.d.mu.Lock()
			epoch := r.d.live.epoch
			r.d.mu.Unlock()
			if epoch != "" {
				t.Fatalf("handshake on %s", name)
			}
		})
	}
}

// A page's hello is taken under the lock relays broadcast under: the
// first `view` after it is exactly the next generation, never one the
// hello already counted, never a skipped one. Many short subscriptions
// race a relay loop.
func TestLocalHelloOrdering(t *testing.T) {
	keys := linktest.NewKeys(t)
	r := newRig(t, keys, 1, nil)
	const subscribers, each = 8, 300
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := uint64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r.d.relay("view", web.ViewEvent{Epoch: "abcd", Gen: i})
		}
	})
	genRE := regexp.MustCompile(`"gen":(\d+)`)
	gen := func(b []byte) uint64 {
		g, _ := strconv.ParseUint(string(genRE.FindSubmatch(b)[1]), 10, 64)
		return g
	}
	errs := make(chan string, subscribers*each)
	var observed atomic.Int32 // subscriptions that saw their first view
	var subs sync.WaitGroup
	for range subscribers {
		subs.Go(func() {
			for range each {
				c, hello := r.d.subscribe()
				want := gen(hello) + 1
			read:
				for {
					select {
					case msg, ok := <-c:
						if !ok {
							break read // dropped as slow
						}
						if !bytes.HasPrefix(msg, []byte("event: view\n")) {
							continue // the link's own changes
						}
						if got := gen(msg); got != want {
							errs <- fmt.Sprintf("hello at %d, then %d", want-1, got)
						}
						observed.Add(1)
						break read
					case <-time.After(time.Second):
						break read
					}
				}
				// Left subscribed: the Hub drops it once its buffer fills.
			}
		})
	}
	subs.Wait()
	close(stop)
	wg.Wait()
	close(errs)
	n := 0
	for e := range errs {
		if n++; n <= 5 {
			t.Error(e)
		}
	}
	// A pass must come from observations, not timeouts or drops.
	if o := int(observed.Load()); o < subscribers*each*9/10 {
		t.Fatalf("only %d of %d subscriptions saw a view", o, subscribers*each)
	}
}

// `link` on every change, from the daemon's own state: down with a local
// reason, then up again, and a new hello once the stream is back.
func TestLinkEvents(t *testing.T) {
	r, u, _ := scriptRig(t, newScript(nil, helloBlock("")))
	p := r.page()
	p.until("hello", ownLinkUp)
	u.SetHello(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) })
	if resp, _ := r.do(r.req("POST", "/client/retry", "", nil)); resp.StatusCode != 200 {
		t.Fatalf("retry: %d", resp.StatusCode)
	}
	down := p.until("link", func(ev sseEvent) bool { return ev.name == "hello" || ownLinkUp(ev) })
	var lv LinkView
	if err := json.Unmarshal(down.data, &lv); err != nil || lv.State != "down" || lv.Reason == nil || *lv.Reason != "refused" || lv.Server != "server" {
		t.Fatalf("down %s", down.data)
	}
	u.SetHello(linktest.DefaultHello)
	up := p.until("link", nil)
	if err := json.Unmarshal(up.data, &lv); err != nil || lv.State != "up" || lv.Reason != nil || lv.Revision.Server != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("up %s", up.data)
	}
	h := p.until("hello", nil)
	if !bytes.Contains(h.data, []byte(`"epoch":"abcd"`)) || !bytes.Contains(h.data, []byte(`"state":"up"`)) {
		t.Fatalf("hello after reconnect %s", h.data)
	}
}

// `theme` comes from this desk's file only: an upstream one never passes
// (TestUpstreamAllowlist), and a change here reaches every page.
func TestThemeEvents(t *testing.T) {
	r, _, _ := scriptRig(t, newScript(nil, helloBlock(""), block("theme", `{"at":1}`)))
	p := r.page()
	p.handshaken()
	p.quiet(150 * time.Millisecond)
	if err := os.WriteFile(r.theme+".new", []byte(":root { --bg: #000000; }"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Rename(r.theme+".new", r.theme)
	ev := p.until("theme", func(ev sseEvent) bool { return ev.name == "hello" || ownLinkUp(ev) })
	if !regexp.MustCompile(`^\{"at":\d{10}\}$`).Match(ev.data) {
		t.Fatalf("theme %s", ev.data)
	}
	if _, body := r.get("/theme.css", nil); !strings.Contains(body, "#000000") {
		t.Fatalf("theme.css %q", body)
	}
}

// Nothing from the server for the silence limit, not even a ping: the
// link is called down at once (a laptop that slept), then comes back.
func TestSilence(t *testing.T) {
	r, _, _ := scriptRig(t, newScript(nil, helloBlock("")), func(d *Daemon) { d.silence = 300 * time.Millisecond })
	p := r.page()
	p.until("hello", ownLinkUp)
	down := p.until("link", func(ev sseEvent) bool { return ev.name == "hello" })
	if !bytes.Contains(down.data, []byte(`"state":"down"`)) {
		t.Fatalf("after silence: %s", down.data)
	}
	if up := p.until("link", nil); !bytes.Contains(up.data, []byte(`"state":"up"`)) {
		t.Fatalf("after: %s", up.data)
	}
}

// launch, and a page's POST /sync, ask for a hello probe: on a dead link
// that's noticed within the probe's wait, not a heartbeat's.
func TestLaunchProbe(t *testing.T) {
	r, u, _ := scriptRig(t, newScript(nil, helloBlock("")))
	var probes atomic.Int32
	u.SetHello(func(w http.ResponseWriter, req *http.Request) { probes.Add(1); linktest.DefaultHello(w, req) })
	r.d.Launch()
	deadline := time.Now().Add(3 * time.Second)
	for probes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("launch sent no hello probe")
		}
		time.Sleep(5 * time.Millisecond)
	}
	n := probes.Load()
	r.do(r.req("POST", "/sync", "", nil))
	for probes.Load() == n {
		if time.Now().After(deadline) {
			t.Fatal("POST /sync sent no hello probe")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A hello that hangs: the link is down within the probe's wait (1s in
	// the rig), not the 60s silence.
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	u.SetHello(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-hang:
		case <-req.Context().Done():
		}
	})
	start := time.Now()
	r.d.Launch()
	waitState(t, r.link, link.Refused)
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("down after %v", d)
	}
}

// GET /client/link and POST /client/retry: behind Auth (cookie; Origin on
// the POST), data class, read-only and input-free.
func TestClientEndpoints(t *testing.T) {
	r, u, _ := scriptRig(t, newScript(nil, helloBlock("")))
	resp, body := r.get("/client/link", nil)
	var lv LinkView
	if resp.StatusCode != 200 || resp.Header.Get(web.PolicyHeader) != "data" || json.Unmarshal([]byte(body), &lv) != nil ||
		lv.State != "up" || lv.Reason != nil || lv.Server != "server" || lv.Revision.Server == "" || lv.Since == "" {
		t.Fatalf("/client/link: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	before := u.Requests.Load()
	for _, c := range []struct {
		method, path, body string
		hdr                map[string]string
		status             int
	}{
		{"GET", "/client/link", "", map[string]string{"Cookie": ""}, 403},
		{"POST", "/client/retry", "", map[string]string{"Cookie": ""}, 403},
		{"POST", "/client/retry", "", map[string]string{"Origin": ""}, 403},
		{"POST", "/client/retry", "", map[string]string{"Origin": "http://evil.example"}, 403},
		{"POST", "/client/retry", "x=1", nil, 400},
		{"POST", "/client/retry?reason=x", "", nil, 400},
		{"GET", "/client/retry", "", nil, 405},
		{"POST", "/client/link", "", nil, 405},
		{"GET", "/client/launch", "", nil, 404},
		{"GET", "/client/static/", "", nil, 404},
		{"GET", "/client/static/error.html", "", nil, 404},
		{"GET", "/client/static/../page/error.html", "", nil, 307}, // the mux's own, to:
		{"GET", "/client/page/error.html", "", nil, 404},
	} {
		if resp, body := r.do(r.req(c.method, c.path, c.body, c.hdr)); resp.StatusCode != c.status {
			t.Errorf("%s %s %v: %d %q", c.method, c.path, c.hdr, resp.StatusCode, body)
		}
	}
	chunked := r.req("POST", "/client/retry", "", nil)
	chunked.Body, chunked.ContentLength = io.NopCloser(strings.NewReader("x")), -1
	if resp, _ := r.do(chunked); resp.StatusCode != 400 {
		t.Errorf("chunked body: %d", resp.StatusCode)
	}
	if n := u.Requests.Load() - before; n != 0 {
		t.Fatalf("%d requests went upstream (a refused retry probed)", n)
	}
	if resp, body := r.do(r.req("POST", "/client/retry", "", nil)); resp.StatusCode != 200 || body != "{\"ok\":true}\n" {
		t.Fatalf("retry: %d %q", resp.StatusCode, body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for u.Requests.Load() == before { // the probe's hello
		if time.Now().After(deadline) {
			t.Fatal("retry didn't probe")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for name, ctype := range map[string]string{"error.js": "text/javascript; charset=utf-8", "error.css": "text/css; charset=utf-8"} {
		resp, body := r.get("/client/static/"+name, nil)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctype || resp.Header.Get(web.PolicyHeader) != "data" || len(body) < 100 {
			t.Errorf("%s: %d %v", name, resp.StatusCode, resp.Header)
		}
	}
}

// reset-window arms Clear-Site-Data for the next page navigation the
// daemon answers, once; subresources and fetches don't take it, and a
// service worker's script fetch stays refused.
func TestResetWindow(t *testing.T) {
	r, _, _ := scriptRig(t, newScript(nil, helloBlock("")))
	nav := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "same-origin"}
	r.d.auth.ArmClearSite()
	if resp, _ := r.get("/client/link", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "same-origin", "Cookie": ""}); resp.StatusCode != 403 || resp.Header.Get("Clear-Site-Data") != "" {
		t.Fatal("a navigation without the session took it")
	}
	if resp, _ := r.get("/client/link", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "cross-site"}); resp.Header.Get("Clear-Site-Data") != "" {
		t.Fatal("another site's navigation took it")
	}
	for _, c := range []struct {
		path string
		hdr  map[string]string
	}{
		{"/client/static/error.js", map[string]string{"Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"}},
		{"/client/link", map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}},
		{"/client/static/error.js", map[string]string{"Service-Worker": "script"}},
		{"/client/link", nil},
	} {
		if resp, _ := r.get(c.path, c.hdr); resp.Header.Get("Clear-Site-Data") != "" {
			t.Fatalf("%s %v took it", c.path, c.hdr)
		}
	}
	if resp, _ := r.get("/client/static/sw.js", map[string]string{"Service-Worker": "script"}); resp.StatusCode != 404 {
		t.Fatalf("worker script: %d", resp.StatusCode)
	}
	resp, _ := r.get("/client/link", nav)
	if resp.Header.Get("Clear-Site-Data") != `"cache", "storage"` {
		t.Fatalf("navigation: %v", resp.Header)
	}
	if resp, _ := r.get("/client/link", nav); resp.Header.Get("Clear-Site-Data") != "" {
		t.Fatal("twice")
	}
}

// The daemon's half of an agent callout: the link's own reason code, and
// failing accounts from the last valid status. Nothing else.
func TestSituation(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	api := linktest.NewAPI()
	api.Set(func(a *linktest.API) { a.State = "Stopped" })
	r := newRig(t, keys, u.Port, api)
	waitState(t, r.link, link.TailscaleDown)
	sit := r.d.Situation()
	if sit.Mode != control.ModeClient || sit.Link != "tailscale-down" || len(sit.Failing) != 0 {
		t.Fatalf("%+v", sit)
	}
	bad := "gmi: boom"
	r.d.mu.Lock()
	r.d.live.status = &web.StatusDoc{Version: 1, Accounts: []web.StatusAccount{
		{Name: "work", Failures: 2, Error: &bad}, {Name: "home"}, {Name: "new", State: "reauth"},
	}}
	r.d.mu.Unlock()
	if sit := r.d.Situation(); strings.Join(sit.Failing, ",") != "work,new" {
		t.Fatalf("failing %v", sit.Failing)
	}
}
