package web

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/testmail"
)

const (
	host  = "pneu.localhost:7317"
	token = "0123456789abcdef0123456789abcdef"
)

// newServer serves the two-account fixture in its own account order.
func newServer(t *testing.T) *Server {
	t.Helper()
	return serverFor(t, testmail.Setup(t).Accounts)
}

func serverFor(t *testing.T, fixture []testmail.Account) *Server {
	t.Helper()
	var accounts []notmuch.Account
	for _, a := range fixture {
		accounts = append(accounts, notmuch.Account{Name: a.Name, Email: a.Email, ConfigPath: a.NotmuchConfig})
	}
	s, err := New(accounts, host, token)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(s http.Handler, method, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func withCookie(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: token}) }

func TestHostMismatch(t *testing.T) {
	s := newServer(t)
	for _, h := range []string{"127.0.0.1:7317", "localhost:7317", "pneu.localhost", "evil.example:7317", ""} {
		w := do(s, "GET", "/", func(r *http.Request) { r.Host = h; withCookie(r) })
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("host %q: %d", h, w.Code)
		}
	}
}

func TestOrigin(t *testing.T) {
	s := newServer(t)
	cases := map[string]int{
		"":                            http.StatusForbidden,
		"null":                        http.StatusForbidden,
		"http://evil.example":         http.StatusForbidden,
		"http://pneu.localhost":       http.StatusForbidden,
		"https://pneu.localhost:7317": http.StatusForbidden,
		"http://pneu.localhost:7317":  http.StatusBadRequest, // passes auth; the empty form is refused by the handler
	}
	for origin, want := range cases {
		w := do(s, "POST", "/tag", func(r *http.Request) {
			withCookie(r)
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
		})
		if w.Code != want {
			t.Errorf("origin %q: got %d want %d", origin, w.Code, want)
		}
	}
	// Right origin, no cookie: still refused.
	w := do(s, "POST", "/tag", func(r *http.Request) { r.Header.Set("Origin", "http://"+host) })
	if w.Code != http.StatusForbidden {
		t.Errorf("no cookie POST: %d", w.Code)
	}
}

// startLaunch points the launch file at a temp dir and returns its path and
// the /open path the launcher would visit.
func startLaunch(t *testing.T, s *Server) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pneu", "launch")
	if err := s.Auth.StartLaunch(p); err != nil {
		t.Fatal(err)
	}
	u, err := LaunchURL(p, "http://"+host)
	if err != nil {
		t.Fatal(err)
	}
	return p, strings.TrimPrefix(u, "http://"+host)
}

func TestCookieFlow(t *testing.T) {
	s := newServer(t)
	// Before StartLaunch nothing opens, not even an empty nonce.
	for _, target := range []string{"/open", "/open?nonce=", "/open?nonce=x", "/open?nonce=" + token} {
		if w := do(s, "GET", target, nil); w.Code == http.StatusFound || len(w.Result().Cookies()) != 0 {
			t.Fatalf("%s before launch: %d", target, w.Code)
		}
	}
	_, open := startLaunch(t, s)
	if !strings.HasPrefix(open, "/open?nonce=") || strings.Contains(open, token) {
		t.Fatalf("open path %q", open)
	}

	w := do(s, "GET", "/", nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "/open") {
		t.Fatalf("no cookie: %d %q", w.Code, w.Body)
	}
	w = do(s, "GET", "/", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: "nope"}) })
	if w.Code != http.StatusForbidden {
		t.Fatalf("bad cookie: %d", w.Code)
	}
	// Neither a wrong nonce nor the install token itself opens.
	for _, target := range []string{"/open?nonce=wrong", "/open?nonce=" + token} {
		if w := do(s, "GET", target, nil); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
			t.Fatalf("%s: %d", target, w.Code)
		}
	}

	w = do(s, "GET", open, nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("open: %d %v", w.Code, w.Header())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}
	c := cookies[0]
	if c.Name != CookieName || c.Value != token || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie attrs %+v", c)
	}

	w = do(s, "GET", "/", func(r *http.Request) { r.AddCookie(c) })
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<title>Inbox · pneu</title>") {
		t.Fatalf("with cookie: %d %q", w.Code, w.Body)
	}
	if w := do(s, "GET", "/static/app.css", withCookie); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "--bg") {
		t.Fatalf("static: %d", w.Code)
	}
}

func TestLaunchNonceSingleUse(t *testing.T) {
	s := newServer(t)
	p, open := startLaunch(t, s)
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("launch file %v %v", fi, err)
	}
	if w := do(s, "GET", open, nil); w.Code != http.StatusFound {
		t.Fatalf("first open: %d", w.Code)
	}
	// The URL in history is dead, and the file holds a new nonce that works.
	if w := do(s, "GET", open, nil); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Fatalf("reused nonce: %d", w.Code)
	}
	next, err := LaunchURL(p, "http://"+host)
	if err != nil {
		t.Fatal(err)
	}
	next = strings.TrimPrefix(next, "http://"+host)
	if next == open {
		t.Fatal("launch file not rotated")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("rotated mode %v", fi.Mode())
	}
	if w := do(s, "GET", next, nil); w.Code != http.StatusFound {
		t.Fatalf("rotated nonce: %d", w.Code)
	}
	// A failed attempt does not rotate: a hostile tab can't burn the
	// launcher's nonce by guessing.
	cur, _ := os.ReadFile(p)
	do(s, "GET", "/open?nonce="+strings.Repeat("0", 64), nil)
	if again, _ := os.ReadFile(p); string(again) != string(cur) {
		t.Fatal("failed open rotated the nonce")
	}
}

func TestLaunchURL(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "launch")
	if _, err := LaunchURL(missing, "http://"+host); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if err := os.WriteFile(missing, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LaunchURL(missing, "http://"+host); err == nil {
		t.Fatal("malformed nonce accepted")
	}
	t.Setenv("XDG_STATE_HOME", dir)
	if p, err := LaunchPath(); err != nil || p != filepath.Join(dir, "pneu", "launch") {
		t.Fatal(p, err)
	}
}

// Cookie tossing: a page on another port of pneu.localhost can plant its
// own "pneu" cookie with a longer Path, which the browser sends first.
func TestCookieTossing(t *testing.T) {
	s := newServer(t)
	w := do(s, "GET", "/", func(r *http.Request) {
		r.Header.Add("Cookie", CookieName+"=planted-by-another-port")
		r.Header.Add("Cookie", CookieName+"="+token)
	})
	if w.Code != http.StatusOK {
		t.Fatalf("real cookie behind a tossed one: %d", w.Code)
	}
	w = do(s, "GET", "/", func(r *http.Request) {
		r.Header.Add("Cookie", CookieName+"=planted; "+CookieName+"=also-planted; other="+token)
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("only tossed cookies: %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	s := newServer(t)
	thread := threadURL(t, s, "/", "Cabin weekend")
	for _, tc := range []struct {
		target string
		mod    func(*http.Request)
		html   bool
	}{
		{thread, withCookie, true},
		{"/", withCookie, true},
		{"/static/mailframe.js", withCookie, false},
		{"/body/work/hostile-15-cid@partner-agency.example", withCookie, false},
		{"/", nil, false}, // 403, text/plain
	} {
		w := do(s, "GET", tc.target, tc.mod)
		h := w.Header()
		if h.Get("Cross-Origin-Resource-Policy") != "same-origin" || h.Get("Cross-Origin-Opener-Policy") != "same-origin" {
			t.Errorf("%s: CORP/COOP %v", tc.target, h)
		}
		csp := h.Get("Content-Security-Policy")
		if tc.html && (w.Code != 200 || csp != AppCSP) {
			t.Errorf("%s: %d CSP %q", tc.target, w.Code, csp)
		}
		if !tc.html && csp != "" {
			t.Errorf("%s: non-HTML response got CSP %q", tc.target, csp)
		}
	}
	// The policy the srcdoc frame inherits must leave it everything its own
	// meta policy grants.
	for _, banned := range []string{"default-src", "img-src", "style-src", "font-src"} {
		if strings.Contains(AppCSP, banned) {
			t.Errorf("AppCSP restricts %s, which the mail frame inherits", banned)
		}
	}
	// Written without an explicit Content-Type: sniffed, and still covered.
	h := s.Auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<!DOCTYPE html><p>hi"))
	}))
	if w := do(h, "GET", "/", withCookie); w.Header().Get("Content-Security-Policy") != AppCSP {
		t.Errorf("sniffed HTML: %v", w.Header())
	}
}

func TestRoutes(t *testing.T) {
	s := newServer(t)
	// Every read route refuses without the cookie.
	for _, target := range []string{"/", "/starred", "/search?q=x", "/t/personal/0000000000000001",
		"/body/personal/a%2Fb%40c", "/part/personal/a%2Fb%40c/3"} {
		if w := do(s, "GET", target, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s without cookie: %d", target, w.Code)
		}
	}
	for _, target := range []string{"/t/nobody/0000000000000001", "/t/personal/not-hex", "/t/personal/ffffffffffffffff",
		"/body/nobody/x@y", "/body/personal/nope@example.com", "/part/personal/nope@example.com/1"} {
		if w := do(s, "GET", target, withCookie); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	if w := do(s, "GET", "/tag", withCookie); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /tag: %d", w.Code)
	}
}

func TestSSE(t *testing.T) {
	s := newServer(t)
	s.Hub.Heartbeat = 50 * time.Millisecond
	ts := httptest.NewServer(s)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events", nil)
	req.Host = host
	withCookie(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	rd := bufio.NewReader(resp.Body)
	readBlock := func() string {
		var b strings.Builder
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line == "\n" {
				return b.String()
			}
			b.WriteString(line)
		}
	}
	if got := readBlock(); got != ": open\n" {
		t.Fatalf("preamble %q", got)
	}
	// Subscribed before the preamble flushed, so this broadcast is not lost.
	s.Hub.Broadcast("sync", map[string]string{"account": "personal"})
	var sawPing, sawSync bool
	for !(sawPing && sawSync) {
		switch got := readBlock(); got {
		case ": ping\n":
			sawPing = true
		case "event: sync\ndata: {\"account\":\"personal\"}\n":
			sawSync = true
		default:
			t.Fatalf("unexpected %q", got)
		}
	}

	// Disconnect cleans the client up.
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.Hub.mu.Lock()
		n := len(s.Hub.clients)
		s.Hub.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d clients left after disconnect", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestToken(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	p, err := TokenPath()
	if err != nil || p != filepath.Join(state, "pneu", "token") {
		t.Fatal(p, err)
	}
	tok, err := LoadOrCreateToken(p)
	if err != nil || len(tok) != 64 {
		t.Fatal(tok, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	again, err := LoadOrCreateToken(p)
	if err != nil || again != tok {
		t.Fatal("token not stable")
	}

	// Readable by group or others: refused, with the fix in the message.
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateToken(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
	os.Chmod(p, 0o400)
	if got, err := LoadOrCreateToken(p); err != nil || got != tok {
		t.Errorf("mode 0400: %v", err)
	}
}

func TestCleanFilename(t *testing.T) {
	for in, want := range map[string]string{
		"invoice\u202Egpj.exe":          "invoicegpj.exe", // RIGHT-TO-LEFT OVERRIDE
		"a\u200Bb\u200Dc\u2066d\u2069e": "abcde",          // zero-width space/joiner, isolates
		"\uFEFFreport.pdf":              "report.pdf",     // BOM
		"../etc/passwd":                 "..etcpasswd",
		" tab\there.txt ":               "tabhere.txt",
		"café résumé.pdf":               "café résumé.pdf",
	} {
		if got := cleanFilename(in); got != want {
			t.Errorf("cleanFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenWithSessionGoesHome(t *testing.T) {
	s := newServer(t)
	p, open := startLaunch(t, s)
	live, _ := os.ReadFile(p)
	// A crash-restored window reloads a spent nonce with the cookie present.
	w := do(s, "GET", "/open?nonce=spent", withCookie)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("open with session: %d %q", w.Code, w.Header().Get("Location"))
	}
	if now, _ := os.ReadFile(p); string(now) != string(live) {
		t.Fatal("a session request must not rotate the launch nonce")
	}
	// The real launch still works afterwards.
	if w := do(s, "GET", open, nil); w.Code != http.StatusFound {
		t.Fatalf("launch after session open: %d", w.Code)
	}
}

func TestEmptyListShowsMark(t *testing.T) {
	s := newServer(t)
	w := do(s, "GET", "/search?q=subject:no-such-message-anywhere", withCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %q", w.Code, w.Body)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="row`) || !strings.Contains(body, `<svg class="empty-mark" aria-hidden="true" viewBox="0 0 15 15"><g fill="none" stroke="currentColor"`) {
		t.Fatalf("empty list without the mark: %q", body)
	}
}
