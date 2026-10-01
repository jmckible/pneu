package client

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/web"
)

// A page load while the link is starting (just started, just woken) waits
// for it rather than failing; past SettleWait it gets the error page,
// branded, saying it's connecting.
func TestSettle(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer(w, "app", "text/html; charset=utf-8", 200, "<!doctype html><p>inbox")
	}))
	gate := make(chan struct{})
	u.SetHello(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}
		linktest.DefaultHello(w, r)
	})
	r := newRig(t, keys, u.Port, nil, func(d *Daemon) { d.settleWait = 200 * time.Millisecond })
	if st := r.link.State(); st.Reason != link.Starting {
		t.Fatalf("link %+v before its first hello", st)
	}
	resp, body := r.get("/", navigate)
	if resp.StatusCode != 503 || resp.Header.Get(LinkHeader) != NotSent || !strings.Contains(body, "Connecting to server") ||
		!strings.Contains(body, `<svg class="mark"`) || !strings.Contains(body, "data-retrying") {
		t.Fatalf("past SettleWait: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}

	r.d.settleWait = 5 * time.Second
	time.AfterFunc(300*time.Millisecond, func() { close(gate) })
	start := time.Now()
	if resp, body := r.get("/", navigate); resp.StatusCode != 200 || body != "<!doctype html><p>inbox" {
		t.Fatalf("within SettleWait: %d %q", resp.StatusCode, body)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("answered before the link was up")
	}
}

// A safe request that dies on a live session (a connection the server
// dropped while this machine slept) is sent once more on a fresh session;
// a mutation never is. A page load that fails again gets the error page,
// never a bare text answer.
func TestSafeRetry(t *testing.T) {
	keys := linktest.NewKeys(t)
	var pages, tags, fail atomic.Int32
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			http.NotFound(w, r)
			return
		case "/tag":
			io.ReadAll(r.Body)
			tags.Add(1)
			panic(http.ErrAbortHandler)
		}
		pages.Add(1)
		if fail.Add(-1) >= 0 {
			panic(http.ErrAbortHandler) // the stream is reset: no answer
		}
		answer(w, "app", "text/html; charset=utf-8", 200, "<!doctype html><p>inbox")
	}))
	r := newRig(t, keys, u.Port, nil)
	r.waitUp()
	conns := u.Conns.Load()

	fail.Store(1)
	if resp, body := r.get("/", navigate); resp.StatusCode != 200 || body != "<!doctype html><p>inbox" {
		t.Fatalf("retried page load: %d %q", resp.StatusCode, body)
	}
	if pages.Load() != 2 || u.Conns.Load() <= conns {
		t.Fatalf("%d sends, %d connections (was %d): want a second send on a new connection", pages.Load(), u.Conns.Load(), conns)
	}

	r.waitUp()
	resp, _ := r.do(r.req("POST", "/tag", "archive=1", nil))
	if resp.StatusCode != 504 || resp.Header.Get(LinkHeader) != Unknown || tags.Load() != 1 {
		t.Fatalf("mutation: %d %v, sent %d times", resp.StatusCode, resp.Header, tags.Load())
	}

	r.waitUp()
	pages.Store(0)
	fail.Store(2)
	resp, body := r.get("/", navigate)
	if resp.StatusCode != 503 || resp.Header.Get(LinkHeader) != Unknown || resp.Header.Get(web.PolicyHeader) != "app" ||
		resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(body, `<svg class="mark"`) {
		t.Fatalf("page load failing twice: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}
	if pages.Load() != 2 {
		t.Fatalf("sent %d times, want 2", pages.Load())
	}
	// A script's fetch that fails twice still gets the plain answer.
	r.waitUp()
	fail.Store(2)
	if resp, body := r.get("/", map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}); resp.StatusCode != 504 ||
		strings.Contains(body, "<html") {
		t.Fatalf("fetch failing twice: %d %q", resp.StatusCode, body)
	}
}

// Starting after a wake is a welcome-back page with a progress bar: no
// steps, no reason code, no button. Starting otherwise is the usual page.
func TestWakePage(t *testing.T) {
	var b strings.Builder
	st := link.State{Reason: link.Starting, Since: time.Now(), Asleep: 55 * time.Minute}
	if err := errorTmpl.Execute(&b, explain(st, "dell")); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, want := range []string{"Welcome back", "reconnecting to dell", `class="progress"`, "Asleep for 55 minutes", `<svg class="mark"`} {
		if !strings.Contains(page, want) {
			t.Errorf("wake page lacks %q", want)
		}
	}
	for _, not := range []string{"<ul>", `id="retry"`, "<code>starting</code>"} {
		if strings.Contains(page, not) {
			t.Errorf("wake page has %q", not)
		}
	}
	b.Reset()
	st.Asleep = 0
	errorTmpl.Execute(&b, explain(st, "dell"))
	if p := b.String(); strings.Contains(p, "Welcome back") || strings.Contains(p, "progress") || !strings.Contains(p, "Connecting to dell") {
		t.Errorf("starting without a wake:\n%s", p)
	}
	for d, want := range map[time.Duration]string{
		30 * time.Second: "a minute", 90 * time.Second: "a minute", 2 * time.Minute: "2 minutes",
		55 * time.Minute: "55 minutes", 2 * time.Hour: "2 hours", 5*time.Hour + 40*time.Minute: "5 hours",
		47 * time.Hour: "47 hours", 49 * time.Hour: "2 days",
	} {
		if got := asleepText(d); got != want {
			t.Errorf("asleepText(%v) = %q, want %q", d, got, want)
		}
	}
}
