package web

import (
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
)

func pulling(done, total int) gmi.Status {
	return gmi.Status{State: gmi.StatePulling, Progress: &gmi.Progress{Phase: gmi.PhaseContent, Done: done, Total: total, Listed: total,
		Frontier: time.Date(2024, 3, 2, 12, 0, 0, 0, time.UTC)}}
}

// postTo POSTs form to target as the page does: same-origin, with the cookie.
func postTo(s http.Handler, target string, f url.Values, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, strings.NewReader(f.Encode()))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+host)
	withCookie(r)
	for _, m := range mods {
		m(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// While an account's first pull runs, nothing is written to it: tags,
// undo, mark-read and send are all refused or withheld. The other account
// works as usual.
func TestReadOnlyWhilePulling(t *testing.T) {
	f := newTagFixture(t)
	f.sync.setStatus("personal", pulling(10, 100))
	k := find(t, rows(t, f.s, "/"), "Kitchen quote")

	w := post(f.s, form("action", "archive", "account", "personal", "ids", strings.Join(k.MsgIDs, " ")))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "personal is still downloading its mail") {
		t.Fatalf("archive while pulling: %d %s", w.Code, w.Body)
	}
	if f.sync.count("personal") != 0 || len(f.sync.noted("personal")) != 0 {
		t.Fatal("a refused write reached the engine")
	}

	// Thread page: no ids to mark read.
	code, body := get(t, f.s, k.URL)
	if code != 200 {
		t.Fatal(code)
	}
	if m := unreadAttrRE.FindStringSubmatch(body); m == nil || m[4] != "" {
		t.Fatalf("thread page offers mark-read while pulling: %v", m)
	}

	// Compose: the account can't be picked, and a send is refused.
	_, body = get(t, f.s, "/compose")
	if !regexp.MustCompile(`<option value="personal"[^>]* disabled>[^<]*\(still downloading\)</option>`).MatchString(body) {
		t.Errorf("compose offers a pulling account:\n%s", body)
	}
	w = postTo(f.s, "/send", form("account", "personal", "to", "a@example.com", "subject", "x", "body", "y"))
	if w.Code != http.StatusConflict || f.sync.sends() != 0 {
		t.Fatalf("send while pulling: %d, %d sends", w.Code, f.sync.sends())
	}

	// A resumed pull records its history id before its closing partial
	// pull: pulled on disk, still pulling, still read-only.
	resumed := pulling(10, 100)
	resumed.Pulled = true
	f.sync.setStatus("personal", resumed)
	if w := post(f.s, form("action", "archive", "account", "personal", "ids", strings.Join(k.MsgIDs, " "))); w.Code != http.StatusConflict {
		t.Fatalf("archive during a resumed pull's partial pull: %d %s", w.Code, w.Body)
	}
	// Pulled but its credentials gone (an abandoned re-auth): not "downloading".
	f.sync.setStatus("personal", gmi.Status{State: gmi.StateReauth, Pulled: true, LastErr: gmi.ErrReauth})
	if f.s.readOnly("personal") {
		t.Fatal("a pulled account awaiting re-auth is read-only")
	}

	// Undo of an action taken before the pull started is refused too.
	f.sync.setStatus("personal", gmi.Status{State: gmi.StateReady, Pulled: true})
	resp := tagOK(t, f.s, form("action", "archive", "account", "personal", "ids", strings.Join(k.MsgIDs, " ")))
	f.sync.setStatus("personal", pulling(10, 100))
	if w := post(f.s, form("action", "undo", "id", resp.ID)); w.Code != http.StatusConflict {
		t.Fatalf("undo while pulling: %d %s", w.Code, w.Body)
	}
}

// The account's state reaches the page (data-accounts), GET /status and
// the status file, progress included.
func TestAccountStateSurfaces(t *testing.T) {
	f := newTagFixture(t)
	f.sync.setStatus("personal", pulling(250, 1000))
	f.sync.setStatus("work", gmi.Status{State: gmi.StateReauth, Pulled: true, Failures: 3,
		LastErr: errors.New("Gmail access expired or was revoked: gmi sync [work]: exit 1")})

	_, body := get(t, f.s, "/")
	m := regexp.MustCompile(`<div id="accounts" data-accounts="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no #accounts strip:\n%s", body)
	}
	var views []accountView
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].State != gmi.StatePulling || views[0].Progress == nil || *views[0].Progress.Percent != 25 ||
		*views[0].Progress.Frontier != "2024-03-02T12:00:00Z" || views[1].State != gmi.StateReauth || views[1].Error == nil {
		t.Fatalf("data-accounts: %s", html.UnescapeString(m[1]))
	}

	code, body := get(t, f.s, "/status")
	if code != 200 || !strings.Contains(body, `"state":"pulling"`) || !strings.Contains(body, `"percent":25`) || !strings.Contains(body, `"state":"reauth"`) {
		t.Fatalf("GET /status: %d %s", code, body)
	}

	doc := f.s.statusSnapshot(true)
	p, v := doc.Accounts[0], doc.Accounts[1]
	if p.State != gmi.StatePulling || p.Progress == nil || *p.Progress.Percent != 25 || p.Progress.Total != 1000 {
		t.Errorf("status file, personal: %+v", p)
	}
	if v.State != gmi.StateReauth {
		t.Errorf("status file, work: %+v", v)
	}
}

// Re-auth and retry are POSTs behind the app's auth: same-origin only.
func TestAccountEndpoints(t *testing.T) {
	f := newTagFixture(t)
	f.sync.reURL = "https://accounts.google.com/o/oauth2/auth?state=x"

	w := postTo(f.s, "/accounts/work/reauth", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"url":"https://accounts.google.com/o/oauth2/auth?state=x"`) {
		t.Fatalf("reauth: %d %s", w.Code, w.Body)
	}
	f.sync.reErr = gmi.ErrNoReauth
	if w := postTo(f.s, "/accounts/personal/reauth", nil); w.Code != http.StatusConflict {
		t.Fatalf("reauth of a working account: %d %s", w.Code, w.Body)
	}
	f.sync.reErr = gmi.ErrAuthing
	if w := postTo(f.s, "/accounts/work/reauth", nil); w.Code != http.StatusConflict {
		t.Fatalf("second re-auth: %d %s", w.Code, w.Body)
	}
	f.sync.reErr = gmi.ErrAlreadyConnected
	if w := postTo(f.s, "/accounts/work/reauth", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"connected":true`) {
		t.Fatalf("already connected: %d %s", w.Code, w.Body)
	}
	if w := postTo(f.s, "/accounts/nobody/reauth", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown account: %d", w.Code)
	}
	if w := postTo(f.s, "/accounts/work/reauth/cancel", nil); w.Code != 200 || len(f.sync.cancels) != 1 {
		t.Fatalf("cancel: %d %v", w.Code, f.sync.cancels)
	}
	// Another origin, or a GET, gets nowhere.
	if w := postTo(f.s, "/accounts/work/reauth", nil, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin reauth: %d", w.Code)
	}
	if w := do(f.s, "GET", "/accounts/work/reauth", withCookie); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET reauth: %d", w.Code)
	}
	if len(f.sync.reauths) != 4 {
		t.Fatalf("reauth calls: %v", f.sync.reauths)
	}

	// Retry: only a first pull that isn't running.
	if w := postTo(f.s, "/accounts/personal/pull", nil); w.Code != http.StatusConflict {
		t.Fatalf("retry of a pulled account: %d", w.Code)
	}
	f.sync.setStatus("personal", gmi.Status{State: gmi.StateNeedsPull, Failures: 1, LastErr: errors.New("killed")})
	before := f.sync.syncs
	if w := postTo(f.s, "/accounts/personal/pull", nil); w.Code != 200 || f.sync.syncs != before+1 {
		t.Fatalf("retry: %d, syncs %d", w.Code, f.sync.syncs)
	}
}

// AccountChanged broadcasts every report but rewrites the status file only
// when the bar would show something new.
func TestAccountChangedThrottlesStatus(t *testing.T) {
	f := newTagFixture(t)
	drain := func() bool {
		select {
		case <-f.s.statusNow:
			return true
		default:
			return false
		}
	}
	drain()
	f.sync.setStatus("personal", pulling(10, 1000))
	f.s.AccountChanged("personal")
	if !drain() {
		t.Fatal("first report didn't rewrite the status file")
	}
	f.sync.setStatus("personal", pulling(19, 1000)) // still 1%
	f.s.AccountChanged("personal")
	if drain() {
		t.Fatal("rewrote for no visible change")
	}
	f.sync.setStatus("personal", pulling(20, 1000))
	f.s.AccountChanged("personal")
	if !drain() {
		t.Fatal("a new percent didn't rewrite")
	}
}
