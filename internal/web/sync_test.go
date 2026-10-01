package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postSync(s http.Handler, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/sync", strings.NewReader(""))
	r.Host = host
	r.Header.Set("Origin", "http://"+host)
	withCookie(r)
	for _, m := range mods {
		m(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestSyncNowQueuesEveryAccount(t *testing.T) {
	f := newTagFixture(t)
	w := postSync(f.s)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("POST /sync = %d %s", w.Code, w.Body)
	}
	if f.sync.syncs != len(f.env.Accounts) {
		t.Fatalf("SyncNow calls = %d, want one per account (%d)", f.sync.syncs, len(f.env.Accounts))
	}
}

func TestSyncNowChecksOriginAndHost(t *testing.T) {
	f := newTagFixture(t)
	for name, mod := range map[string]func(*http.Request){
		"cross-site origin": func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		"no origin":         func(r *http.Request) { r.Header.Del("Origin") },
		"other host":        func(r *http.Request) { r.Host = "127.0.0.1:7317" },
	} {
		if w := postSync(f.s, mod); w.Code == http.StatusOK {
			t.Errorf("%s: POST /sync = %d, want refused", name, w.Code)
		}
	}
	if f.sync.syncs != 0 {
		t.Fatalf("refused requests queued %d syncs", f.sync.syncs)
	}
	if w := postSync(f.s, func(r *http.Request) { r.Method = "GET" }); w.Code == http.StatusOK {
		t.Errorf("GET /sync = %d, want refused", w.Code)
	}
}

func TestSyncNowWithoutSyncer(t *testing.T) {
	f := newTagFixture(t)
	f.s.Syncer = nil
	if w := postSync(f.s); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /sync with no engine = %d, want 503", w.Code)
	}
}

// /open queues no sync, nonce or not: `pneu open` sends the launch over the
// control socket (Server.Launch), which no page can reach. Launch queues
// every account and tells open pages before it returns, and a page rendered
// after it says so from its first paint.
func TestLaunchQueuesEveryAccount(t *testing.T) {
	f := newTagFixture(t)
	_, open := startLaunch(t, f.s)
	for _, u := range []string{open, "/open?nonce=spent"} {
		if w := do(f.s, "GET", u, withCookie); w.Code != http.StatusFound {
			t.Fatalf("open %s: %d", u, w.Code)
		}
	}
	if f.sync.syncs != 0 {
		t.Fatalf("/open queued %d syncs", f.sync.syncs)
	}

	events := f.s.Hub.Subscribe()
	defer f.s.Hub.unsubscribe(events)
	f.s.Launch()
	if f.sync.syncs != len(f.env.Accounts) {
		t.Fatalf("SyncNow calls = %d, want one per account (%d)", f.sync.syncs, len(f.env.Accounts))
	}
	for range f.env.Accounts {
		select {
		case msg := <-events:
			if !strings.HasPrefix(string(msg), "event: account\n") || !strings.Contains(string(msg), `"queued":true`) {
				t.Fatalf("event %q", msg)
			}
		default:
			t.Fatal("Launch returned before telling the page")
		}
	}
	w := do(f.s, "GET", "/", withCookie)
	if body := w.Body.String(); !strings.Contains(body, "&#34;queued&#34;:true") {
		t.Fatalf("page after launch doesn't say queued: %s", body[:min(len(body), 2000)])
	}
}
