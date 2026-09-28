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

// A launch (/open with the nonce) syncs every account, so mail the phone just
// announced is on its way as the window opens. The session branch of /open, a
// GET any localhost page can fire, and a refused nonce queue nothing.
func TestLaunchSyncsEveryAccount(t *testing.T) {
	f := newTagFixture(t)
	_, open := startLaunch(t, f.s)
	if w := do(f.s, "GET", "/open?nonce=spent", withCookie); w.Code != http.StatusFound {
		t.Fatalf("open with session: %d", w.Code)
	}
	if w := do(f.s, "GET", "/open?nonce=wrong", nil); w.Code != http.StatusForbidden {
		t.Fatalf("bad nonce: %d", w.Code)
	}
	if f.sync.syncs != 0 {
		t.Fatalf("non-launch opens queued %d syncs", f.sync.syncs)
	}
	if w := do(f.s, "GET", open, nil); w.Code != http.StatusFound {
		t.Fatalf("launch: %d", w.Code)
	}
	if f.sync.syncs != len(f.env.Accounts) {
		t.Fatalf("SyncNow calls = %d, want one per account (%d)", f.sync.syncs, len(f.env.Accounts))
	}
}
