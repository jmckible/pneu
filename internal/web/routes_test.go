package web

import (
	"context"
	"github.com/jmckible/pneu/internal/control"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// secHeaders is what the policy tests pin: the security set, the class,
// and the cache rule.
var secHeaders = append(slices.Clone(securityHeaders), "Cache-Control")

func pinned(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range secHeaders {
		if vs := h.Values(k); len(vs) > 0 {
			out[k] = strings.Join(vs, " | ")
		}
	}
	return out
}

// wantHeaders is class p's exact set with cache rule c.
func wantHeaders(p Policy, c CacheRule) map[string]string {
	h := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Pneu-Policy":                  string(p),
		"Pneu-Instance":                control.Instance(),
		"Cache-Control":                string(c),
	}
	switch p {
	case PolicyApp:
		h["Content-Security-Policy"] = "script-src 'self'; object-src 'none'; base-uri 'none'; worker-src 'none'"
	case PolicyCompose:
		h["Content-Security-Policy"] = "script-src 'self'; object-src 'none'; base-uri 'none'; worker-src 'none'"
		h["Referrer-Policy"] = "same-origin"
	case PolicyPart:
		h["Content-Security-Policy"] = "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'"
	case PolicyPDF:
		h["Content-Security-Policy"] = "frame-ancestors 'self'"
		h["X-Frame-Options"] = "SAMEORIGIN"
	case PolicyStaticSVG:
		h["Content-Security-Policy"] = "sandbox; default-src 'none'; style-src 'unsafe-inline'"
	}
	return h
}

// Every kind of response carries exactly its class's headers. A change
// here is a change to what the browser enforces: it goes through the
// hostile corpus (AGENTS.md).
func TestPolicyHeaderSets(t *testing.T) {
	fx := newTagFixture(t)
	s := fx.s
	thread := threadURL(t, s, "/", "Cabin weekend")
	image := func(r *http.Request) { withCookie(r); r.Header.Set("Sec-Fetch-Dest", "image") }
	ifNoneMatch := func(r *http.Request) { withCookie(r); r.Header.Set("If-None-Match", "*") }
	rangeReq := func(v string) func(*http.Request) {
		return func(r *http.Request) { withCookie(r); r.Header.Set("Range", v) }
	}
	bad := replyForm(t, s, "personal", cabin3, false).values
	bad.Set("to", "not an address")

	cases := []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		ctype  string
		want   map[string]string
	}{
		{"list page", do(s, "GET", "/", withCookie), 200, "text/html; charset=utf-8", wantHeaders(PolicyApp, CacheNoStore)},
		{"thread page", do(s, "GET", thread, withCookie), 200, "text/html; charset=utf-8", wantHeaders(PolicyApp, CacheNoStore)},
		{"JSON endpoint", do(s, "GET", "/status", withCookie), 200, "application/json", wantHeaders(PolicyData, CacheNoStore)},
		{"body JSON", do(s, "GET", "/body/work/hostile-15-cid@partner-agency.example", withCookie), 200, "application/json", wantHeaders(PolicyData, CacheNoStore)},
		{"PDF part", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/8", withCookie), 200, "application/pdf", wantHeaders(PolicyPDF, CacheNoStore)},
		{"image part", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/6", withCookie), 200, "image/png", wantHeaders(PolicyPart, CacheNoStore)},
		{"HTML attachment", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/5", withCookie), 200, "text/plain; charset=utf-8", wantHeaders(PolicyPart, CacheNoStore)},
		{"SVG part as an image", do(s, "GET", survey1+"4", image), 200, "image/svg+xml", wantHeaders(PolicyPart, CacheNoStore)},
		{"SVG part navigated to", do(s, "GET", survey1+"4", withCookie), 200, "text/plain", wantHeaders(PolicyPart, CacheNoStore)},
		{"missing part", do(s, "GET", "/part/personal/nope@example.com/1", withCookie), 404, "text/plain; charset=utf-8", wantHeaders(PolicyData, CacheNoStore)},
		{"PDF part, unsatisfiable range", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/8", rangeReq("bytes=99999-")), 416, "text/plain; charset=utf-8", wantHeaders(PolicyData, CacheNoStore)},
		{"PDF part, failed precondition", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/8", func(r *http.Request) { withCookie(r); r.Header.Set("If-Match", `"x"`) }), 412, "text/plain; charset=utf-8", wantHeaders(PolicyData, CacheNoStore)},
		{"PDF part, conditional GET", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/8", ifNoneMatch), 304, "", wantHeaders(PolicyPDF, CacheNoStore)},
		{"PDF part, conditional HEAD", do(s, "HEAD", "/part/personal/5f2e9a10-trip-photos@fastmail.example/8", ifNoneMatch), 304, "", wantHeaders(PolicyPDF, CacheNoStore)},
		{"image part, conditional GET", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/6", ifNoneMatch), 304, "", wantHeaders(PolicyPart, CacheNoStore)},
		{"image part, conditional HEAD", do(s, "HEAD", "/part/personal/5f2e9a10-trip-photos@fastmail.example/6", ifNoneMatch), 304, "", wantHeaders(PolicyPart, CacheNoStore)},
		{"PDF part, multi-range: the whole file", do(s, "GET", "/part/personal/5f2e9a10-trip-photos@fastmail.example/8", rangeReq("bytes=0-9,20-29")), 200, "application/pdf", wantHeaders(PolicyPDF, CacheNoStore)},
		{"static, multi-range: the whole file", do(s, "GET", "/static/app.css", rangeReq("bytes=0-9,20-29")), 200, "text/css; charset=utf-8", wantHeaders(PolicyData, CacheRevalidate)},
		{"static SVG", do(s, "GET", "/static/favicon.svg", withCookie), 200, "image/svg+xml", wantHeaders(PolicyStaticSVG, CacheRevalidate)},
		{"compose GET", do(s, "GET", "/compose", withCookie), 200, "text/html; charset=utf-8", wantHeaders(PolicyCompose, CacheNoStore)},
		{"reply GET", do(s, "GET", "/reply/personal/"+url.PathEscape(cabin3), withCookie), 200, "text/html; charset=utf-8", wantHeaders(PolicyCompose, CacheNoStore)},
		{"/send error render", postSend(s, bad), 400, "text/html; charset=utf-8", wantHeaders(PolicyCompose, CacheNoStore)},
		{"/gmail redirect", do(s, "GET", strings.Replace(thread, "/t/", "/gmail/", 1), withCookie), 303, "text/html; charset=utf-8", wantHeaders(PolicyApp, CacheNoStore)},
		{"theme", do(s, "GET", "/theme.css", withCookie), 200, "text/css; charset=utf-8", wantHeaders(PolicyData, CacheNoStore)},
		{"static", do(s, "GET", "/static/app.js", withCookie), 200, "", wantHeaders(PolicyData, CacheRevalidate)},
		{"no session", do(s, "GET", "/", nil), 403, "text/plain; charset=utf-8", wantHeaders(PolicyData, CacheNoStore)},
		{"no route", do(s, "GET", "/nope", withCookie), 404, "text/plain; charset=utf-8", wantHeaders(PolicyData, CacheNoStore)},
	}
	// /send's success: a bodiless redirect.
	good := replyForm(t, s, "personal", cabin3, false).values
	cases = append(cases, struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		ctype  string
		want   map[string]string
	}{"/send redirect", postSend(s, good), 303, "", wantHeaders(PolicyData, CacheNoStore)})

	for _, c := range cases {
		h := c.w.Header()
		if c.w.Code != c.status || (c.ctype != "" && h.Get("Content-Type") != c.ctype) {
			t.Errorf("%s: %d %q", c.name, c.w.Code, h.Get("Content-Type"))
		}
		if c.status == http.StatusNotModified && h.Get("Content-Disposition") != "" {
			t.Errorf("%s: a 304 with a disposition", c.name)
		}
		if got := pinned(h); !maps.Equal(got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
	}

	// /events, a stream: its headers once it has started.
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, "GET", "/events", nil)
	r.Host = host
	withCookie(r)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.ServeHTTP(w, r); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if got := pinned(w.Header()); w.Header().Get("Content-Type") != "text/event-stream" || !maps.Equal(got, wantHeaders(PolicyData, CacheNoStore)) {
		t.Errorf("/events: %v", w.Header())
	}
}

// A flush before any write still sends the class.
func TestPolicyFlushFirst(t *testing.T) {
	a := NewAuth(host, token)
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).Flush()
	}))
	w := do(h, "GET", "/", withCookie)
	if w.Header().Get(PolicyHeader) != "data" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("%v", w.Header())
	}
}

// Informational responses never reach the wire, and the final status is
// the one the class is chosen at. Over a real connection: a recorder
// doesn't send 1xx the way net/http does.
func TestPolicyInformational(t *testing.T) {
	for _, code := range []int{http.StatusContinue, http.StatusEarlyHints} {
		a := NewAuth(host, token)
		srv := httptest.NewServer(a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", "</static/app.css>; rel=preload")
			w.WriteHeader(code)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("<p>hi"))
		})))
		var got1xx []int
		trace := &httptrace.ClientTrace{Got1xxResponse: func(c int, _ textproto.MIMEHeader) error {
			got1xx = append(got1xx, c)
			return nil
		}}
		r, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "GET", srv.URL+"/", nil)
		r.Host = host
		withCookie(r)
		res, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		srv.Close()
		if len(got1xx) != 0 || res.StatusCode != 200 || res.Header.Get(PolicyHeader) != "app" || res.Header.Get("Content-Security-Policy") != AppCSP {
			t.Errorf("%d then 200: 1xx seen %v, final %d %v", code, got1xx, res.StatusCode, res.Header)
		}
	}
}

// Static files revalidate against a content ETag; no directory listings.
func TestStaticRevalidates(t *testing.T) {
	s := newServer(t)
	w := do(s, "GET", "/static/app.js", withCookie)
	tag := w.Header().Get("ETag")
	if w.Code != 200 || !regexp.MustCompile(`^"[0-9a-f]{32}"$`).MatchString(tag) || w.Header().Get("Last-Modified") != "" {
		t.Fatalf("%d ETag %q", w.Code, tag)
	}
	w = do(s, "GET", "/static/app.js", func(r *http.Request) { withCookie(r); r.Header.Set("If-None-Match", tag) })
	if w.Code != http.StatusNotModified || w.Header().Get("Pneu-Policy") != "data" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("revalidation: %d %v", w.Code, w.Header())
	}
	if w := do(s, "GET", "/static/mailframe.js", withCookie); w.Header().Get("ETag") == tag {
		t.Error("two files, one ETag")
	}
	for _, p := range []string{"/static/", "/static/index.html", "/static/nope.js", "/static/app.js/"} {
		if w := do(s, "GET", p, withCookie); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

// samplePath fills a pattern's wildcards.
func samplePath(pattern string) string {
	p := strings.ReplaceAll(pattern, "{$}", "")
	p = regexp.MustCompile(`\{[a-z]+\}`).ReplaceAllString(p, "x")
	if strings.HasSuffix(p, "/static/") {
		p += "app.css"
	}
	return p
}

// The table is the mux: every entry matches as itself, nothing else
// matches, and nothing registers a route any other way.
func TestRouteTable(t *testing.T) {
	s := newServer(t)
	mux := s.serveRoutes()
	seen := map[string]bool{}
	for _, rt := range Routes {
		key := rt.Method + " " + rt.Pattern
		if seen[key] {
			t.Errorf("%s twice", key)
		}
		seen[key] = true
		r := httptest.NewRequest(rt.Method, samplePath(rt.Pattern), nil)
		m := mux
		if rt.PeerOnly {
			m = s.peerRoutes() // TestPeerRoutes: only there
		}
		if rt.ClientOnly {
			// Neither of a server's listeners has them; a client answers.
			if _, pat := m.Handler(r); pat == key {
				t.Errorf("%s served by the server", key)
			}
			if !rt.Local || rt.Handler != nil || !strings.HasPrefix(rt.Pattern, "/client/") {
				t.Errorf("%s: a client route is Local, under /client/, with no server handler", key)
			}
		} else if _, pat := m.Handler(r); pat != key {
			t.Errorf("%s: %s matched %q", key, r.URL.Path, pat)
		}
		if rt.Handler == nil && !rt.ClientOnly || len(rt.Types) == 0 || rt.Cache == "" {
			t.Errorf("%s: incomplete entry", key)
		}
		if got := RouteOf(r); got == nil && !rt.PeerOnly || got != nil && got.Method+" "+got.Pattern != key {
			t.Errorf("%s: RouteOf %v", key, got)
		}
		for p, types := range rt.Types {
			if !p.valid() || len(types) == 0 {
				t.Errorf("%s: class %q", key, p)
			}
			if slices.Contains(types, "text/html") != (p == PolicyApp || p == PolicyCompose) {
				t.Errorf("%s: class %q with %v: HTML is app or compose, and those are only HTML", key, p, types)
			}
		}
		if _, ok := rt.Types[PolicyCompose]; ok && !slices.Contains([]string{"/compose", "/reply/{account}/{msgid}", "/send"}, rt.Pattern) {
			t.Errorf("%s: compose class off the form's routes", key)
		}
		_, part := rt.Types[PolicyPart]
		_, pdf := rt.Types[PolicyPDF]
		if _, ok := rt.Types[PolicyStaticSVG]; ok != (rt.Pattern == "/static/") {
			t.Errorf("%s: static-svg off /static/", key)
		}
		if (part || pdf || rt.Disposition == DispPart || rt.Dest == DestSVGImage) != (rt.Pattern == "/part/{account}/{msgid}/{n}") {
			t.Errorf("%s: part rules off the part route", key)
		}
		if rt.Local != (slices.Contains([]string{"/open", "/theme.css", "/events"}, rt.Pattern) || rt.ClientOnly) {
			t.Errorf("%s: Local %v", key, rt.Local)
		}
		if rt.ClientOnly != strings.HasPrefix(rt.Pattern, "/client/") {
			t.Errorf("%s: ClientOnly %v", key, rt.ClientOnly)
		}
		if rt.Upstream != (rt.Pattern == "/events") || rt.PeerOnly != (rt.Pattern == "/peer/hello") {
			t.Errorf("%s: Upstream %v, PeerOnly %v", key, rt.Upstream, rt.PeerOnly)
		}
	}
	for _, p := range []string{"/sw.js", "/client/x", "/t/personal", "/part/a/b", "/favicon.ico"} {
		if _, pat := mux.Handler(httptest.NewRequest("GET", p, nil)); pat != "" {
			t.Errorf("%s matched %q", p, pat)
		}
	}
	// The mux's own trailing-slash redirect, which unrouted allows.
	if h, _ := mux.Handler(httptest.NewRequest("GET", "/static", nil)); h == nil {
		t.Error("no handler for /static")
	}
	if w := do(s, "GET", "/static", withCookie); w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/static/" {
		t.Errorf("/static: %d %v", w.Code, w.Header())
	}
	if _, pat := mux.Handler(httptest.NewRequest("DELETE", "/tag", nil)); pat != "" {
		t.Errorf("DELETE /tag matched %q", pat)
	}

	// No Handle or HandleFunc anywhere in the package but routeMux.
	fset := gotoken.NewFileSet()
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name == "routeMux" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") {
					t.Errorf("%s: %s registers a route outside the table", fset.Position(sel.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
}

func TestCheck(t *testing.T) {
	c := Checker{Origin: "http://" + host, Email: func(a string) (string, bool) {
		return "robin@hale.example", a == "personal"
	}}
	route := func(pattern string) *Route {
		for i := range Routes {
			if Routes[i].Pattern == pattern {
				return &Routes[i]
			}
		}
		t.Fatalf("no route %s", pattern)
		return nil
	}
	part, list, jsonR := route("/part/{account}/{msgid}/{n}"), route("/{$}"), route("/status")
	gmail, send, static := route("/gmail/{account}/{thread}"), route("/send"), route("/static/")
	hdr := func(p Policy, kv ...string) http.Header {
		h := http.Header{}
		p.Apply(h)
		for i := 0; i+1 < len(kv); i += 2 {
			h.Add(kv[i], kv[i+1])
		}
		return h
	}
	req := func(target string, dest string) *http.Request {
		r := httptest.NewRequest("GET", target, nil)
		if dest != "" {
			r.Header.Set("Sec-Fetch-Dest", dest)
		}
		// PathValue, as the matching mux would set it.
		if parts := strings.Split(target, "/"); len(parts) > 2 {
			r.SetPathValue("account", parts[2])
		}
		return r
	}
	partReq := req("/part/personal/m@x/2", "")
	gm := "https://mail.google.com/mail/?authuser=robin%40hale.example#all/19c8698f3401dced"
	cases := []struct {
		name   string
		rt     *Route
		status int
		h      http.Header
		r      *http.Request
		ok     bool
	}{
		{"pdf part", part, 200, hdr(PolicyPDF, "Content-Type", "application/pdf", "Content-Disposition", "inline; filename*=UTF-8''a.pdf"), partReq, true},
		{"part-pdf as text/html", part, 200, hdr(PolicyPDF, "Content-Type", "text/html", "Content-Disposition", "inline; filename=a.pdf"), partReq, false},
		{"part-pdf as a png", part, 200, hdr(PolicyPDF, "Content-Type", "image/png", "Content-Disposition", "inline; filename=a.png"), partReq, false},
		{"part-pdf on a page route", list, 200, hdr(PolicyPDF, "Content-Type", "application/pdf"), req("/", ""), false},
		{"app on a part route", part, 200, hdr(PolicyApp, "Content-Type", "text/html"), partReq, false},
		{"sandbox part as html", part, 200, hdr(PolicyPart, "Content-Type", "text/html", "Content-Disposition", "attachment; filename=a"), partReq, false},
		{"sandbox part as xhtml", part, 200, hdr(PolicyPart, "Content-Type", "application/xhtml+xml", "Content-Disposition", "attachment; filename=a"), partReq, false},
		{"sandbox part as javascript", part, 200, hdr(PolicyPart, "Content-Type", "text/javascript", "Content-Disposition", "attachment; filename=a"), partReq, false},
		{"svg as an image", part, 200, hdr(PolicyPart, "Content-Type", "image/svg+xml", "Content-Disposition", "attachment; filename=a.svg"), req("/part/personal/m@x/2", "image"), true},
		{"svg navigated to", part, 200, hdr(PolicyPart, "Content-Type", "image/svg+xml", "Content-Disposition", "attachment; filename=a.svg"), req("/part/personal/m@x/2", "document"), false},
		{"svg to an iframe", part, 200, hdr(PolicyPart, "Content-Type", "image/svg+xml", "Content-Disposition", "attachment; filename=a.svg"), req("/part/personal/m@x/2", "iframe"), false},
		{"part without a disposition", part, 200, hdr(PolicyPart, "Content-Type", "image/png"), partReq, false},
		{"text shown inline", part, 200, hdr(PolicyPart, "Content-Type", "text/plain", "Content-Disposition", "inline; filename=a.txt"), partReq, false},
		{"disposition with no filename", part, 200, hdr(PolicyPart, "Content-Type", "image/png", "Content-Disposition", "inline"), partReq, false},
		{"two dispositions", part, 200, hdr(PolicyPart, "Content-Type", "image/png", "Content-Disposition", "inline; filename=a", "Content-Disposition", "attachment; filename=b"), partReq, false},
		{"part error", part, 404, hdr(PolicyData, "Content-Type", "text/plain; charset=utf-8"), partReq, true},
		{"list page", list, 200, hdr(PolicyApp, "Content-Type", "text/html; charset=utf-8"), req("/", ""), true},
		{"page as data", list, 200, hdr(PolicyData, "Content-Type", "text/html"), req("/", ""), false},
		{"compose class on a list", list, 200, hdr(PolicyCompose, "Content-Type", "text/html"), req("/", ""), false},
		{"html on a JSON route", jsonR, 200, hdr(PolicyApp, "Content-Type", "text/html"), req("/status", ""), false},
		{"json", jsonR, 200, hdr(PolicyData, "Content-Type", "application/json"), req("/status", ""), true},
		{"json with a disposition", jsonR, 200, hdr(PolicyData, "Content-Type", "application/json", "Content-Disposition", "attachment; filename=x.html"), req("/status", ""), false},
		{"two content types", jsonR, 200, hdr(PolicyData, "Content-Type", "application/json", "Content-Type", "text/html"), req("/status", ""), false},
		{"no content type", jsonR, 200, hdr(PolicyData), req("/status", ""), false},
		{"malformed content type", jsonR, 200, hdr(PolicyData, "Content-Type", "application/json; ="), req("/status", ""), false},
		{"no class", jsonR, 200, http.Header{"Content-Type": {"application/json"}}, req("/status", ""), false},
		{"two classes", jsonR, 200, hdr(PolicyData, "Content-Type", "application/json", PolicyHeader, "app"), req("/status", ""), false},
		{"unknown class", jsonR, 200, http.Header{"Content-Type": {"application/json"}, PolicyHeader: {"trusted"}}, req("/status", ""), false},
		{"redirect off a redirect route", jsonR, 302, hdr(PolicyData, "Location", "/"), req("/status", ""), false},
		{"location on a 200", send, 200, hdr(PolicyCompose, "Content-Type", "text/html", "Location", "/"), req("/send", ""), false},
		{"send redirect", send, 303, hdr(PolicyData, "Location", "/t/personal/00ab#sent"), req("/send", ""), true},
		{"send redirect off-origin", send, 303, hdr(PolicyData, "Location", "//evil.example/"), req("/send", ""), false},
		{"send redirect without Location", send, 303, hdr(PolicyData), req("/send", ""), false},
		{"gmail", gmail, 303, hdr(PolicyApp, "Content-Type", "text/html; charset=utf-8", "Location", gm), req("/gmail/personal/00ab", ""), true},
		{"gmail, other account's address", gmail, 303, hdr(PolicyApp, "Content-Type", "text/html", "Location", strings.Replace(gm, "robin%40hale", "eve%40hale", 1)), req("/gmail/personal/00ab", ""), false},
		{"gmail, unknown account", gmail, 303, hdr(PolicyApp, "Content-Type", "text/html", "Location", gm), req("/gmail/nobody/00ab", ""), false},
		{"gmail, somewhere else", gmail, 303, hdr(PolicyApp, "Content-Type", "text/html", "Location", "https://evil.example/"), req("/gmail/personal/00ab", ""), false},
		{"static 304", static, 304, hdr(PolicyData, "ETag", `"x"`), req("/static/app.js", ""), true},
		{"static svg, sandboxed, navigated to", static, 200, hdr(PolicyStaticSVG, "Content-Type", "image/svg+xml"), req("/static/favicon.svg", "document"), true},
		{"static svg as data, navigated to", static, 200, hdr(PolicyData, "Content-Type", "image/svg+xml"), req("/static/favicon.svg", "document"), false},
		{"static svg as data, as an image", static, 200, hdr(PolicyData, "Content-Type", "image/svg+xml"), req("/static/favicon.svg", "image"), false},
		{"svg as data on a part", part, 200, hdr(PolicyData, "Content-Type", "image/svg+xml"), req("/part/personal/m@x/2", "image"), false},
		{"static-svg as css", static, 200, hdr(PolicyStaticSVG, "Content-Type", "text/css"), req("/static/app.css", ""), false},
		{"static-svg on a part", part, 200, hdr(PolicyStaticSVG, "Content-Type", "image/svg+xml", "Content-Disposition", "attachment; filename=a.svg"), req("/part/personal/m@x/2", "image"), false},
		{"part 304", part, 304, hdr(PolicyPDF), partReq, true},
		{"part 304 with a disposition", part, 304, hdr(PolicyPart, "Content-Disposition", "inline; filename=a.png"), partReq, false},
		{"part 200 inline text still refused", part, 200, hdr(PolicyPart, "Content-Type", "text/plain", "Content-Disposition", "inline; filename=a.txt"), partReq, false},
		{"part 416 as data", part, 416, hdr(PolicyData, "Content-Type", "text/plain; charset=utf-8"), partReq, true},
		{"part 416 as part-pdf", part, 416, hdr(PolicyPDF, "Content-Type", "text/plain; charset=utf-8", "Content-Disposition", "inline; filename=a.pdf"), partReq, false},
		{"part body as data", part, 200, hdr(PolicyData, "Content-Type", "text/plain"), partReq, false},
		{"multipart byteranges", part, 206, hdr(PolicyPDF, "Content-Type", "multipart/byteranges; boundary=x", "Content-Disposition", "inline; filename=a.pdf"), partReq, false},
		{"static html", static, 200, hdr(PolicyData, "Content-Type", "text/html"), req("/static/x.html", ""), false},
		{"unrouted", nil, 404, hdr(PolicyData, "Content-Type", "text/plain"), req("/nope", ""), true},
		{"unrouted html", nil, 200, hdr(PolicyApp, "Content-Type", "text/html"), req("/nope", ""), false},
		{"unrouted redirect", nil, 307, hdr(PolicyApp, "Content-Type", "text/html", "Location", "/static/"), req("/static", ""), true},
		{"unrouted redirect away", nil, 307, hdr(PolicyApp, "Content-Type", "text/html", "Location", "//evil.example/"), req("/static", ""), false},
	}
	for _, tc := range cases {
		err := c.Check(tc.rt, tc.status, tc.h, tc.r)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestValidGmailURL(t *testing.T) {
	const email = "robin+pneu@hale.example"
	good := gmailURL(email, []string{"/m/cur/19c8698f3401dced:2,S"})
	if !validGmailURL(good, email) {
		t.Fatalf("gmailURL's own output refused: %s", good)
	}
	for _, u := range []string{
		"",
		strings.Replace(good, "https:", "http:", 1),
		strings.Replace(good, "mail.google.com", "MAIL.google.com", 1),
		strings.Replace(good, "mail.google.com", "mail.google.com.evil.example", 1),
		strings.Replace(good, "mail.google.com", "mail.google.com:443", 1),
		strings.Replace(good, "https://", "https://user@", 1),
		strings.Replace(good, "https://", "https://evil.example\\@", 1),
		strings.Replace(good, "/mail/?", "/mail/u/1/?", 1),
		strings.Replace(good, "/mail/?", "/mail?", 1),
		strings.Replace(good, "?authuser=", "?x=1&authuser=", 1),
		strings.Replace(good, "#all/", "&next=https://evil.example#all/", 1),
		strings.Replace(good, "robin%2Bpneu%40hale.example", "eve%40hale.example", 1),
		strings.Replace(good, "robin%2Bpneu%40hale.example", "robin+pneu@hale.example", 1),
		strings.Replace(good, "#all/", "#inbox/", 1),
		good + "/x",
		good + "?x",
		good + "\n",
		good + "%0a",
		strings.Replace(good, "19c8698f3401dced", "19C8698F3401DCED", 1),
		strings.Replace(good, "19c8698f3401dced", "19c8", 1),
		strings.Replace(good, "19c8698f3401dced", "javascript:alert(1)", 1),
		" " + good,
		"\t" + good,
	} {
		if validGmailURL(u, email) {
			t.Errorf("accepted %q", u)
		}
	}
	if validGmailURL(gmailURL("", []string{"19c8698f3401dced"}), "") {
		t.Error("accepted an empty address")
	}
}

func TestLocalLocation(t *testing.T) {
	base, _ := url.Parse("http://" + host + "/send")
	for loc, want := range map[string]string{
		"/":                     "http://" + host + "/",
		"/#sent":                "http://" + host + "/#sent",
		"/t/personal/00ab#sent": "http://" + host + "/t/personal/00ab#sent",
		"./":                    "http://" + host + "/",
		"t/x":                   "http://" + host + "/t/x",
		"/../../etc":            "http://" + host + "/etc",
		"/search?q=a%20b":       "http://" + host + "/search?q=a%20b",
	} {
		got, err := localLocation(loc, base)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", loc, got, err, want)
		}
	}
	for _, loc := range []string{
		"", "//evil.example", "//evil.example/x", "///evil.example", "/\\evil.example", "\\\\evil.example",
		"https:evil.example", "https://evil.example/", "HTTP://pneu.localhost:7317/", "http://" + host + "/",
		"javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,x", "mailto:a@b", "a:b",
		"%2f%2fevil.example", "/%2F%2Fevil.example", "/%5cevil.example", "/%5Cevil.example",
		"/\tevil", "\t//evil.example", "/x\ny", "/x\r\nSet-Cookie: a=b", "/x\x00", "/x\x7f", " /x", "/x y",
		"//user@evil.example", "http://user:pass@" + host + "/", "/é", "//" + host + "/",
	} {
		if got, err := localLocation(loc, base); err == nil {
			t.Errorf("accepted %q as %q", loc, got)
		}
	}
}

// reset-window (Auth.ArmClearSite): the next authenticated top-level
// navigation this browser made itself carries Clear-Site-Data, once. A
// fetch, a frame, another Host, a request without the session, another
// site's navigation and a failed nonce all leave it armed; a good nonce
// takes it. A handler can't set it.
func TestClearSiteData(t *testing.T) {
	s := newServer(t)
	navNoCookie := func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	nav := func(r *http.Request) { navNoCookie(r); withCookie(r) }
	with := func(f func(*http.Request), k, v string) func(*http.Request) {
		return func(r *http.Request) { f(r); r.Header.Set(k, v) }
	}
	s.Auth.ArmClearSite()
	for _, c := range []struct {
		why, path string
		mod       func(*http.Request)
	}{
		{"a subresource", "/static/app.css", withCookie},
		{"a fetch", "/status", with(withCookie, "Sec-Fetch-Mode", "cors")},
		{"a frame", "/", with(nav, "Sec-Fetch-Dest", "iframe")},
		{"another Host", "/", with(nav, "Host", "x")},
		{"no session", "/", navNoCookie},
		{"another site's navigation", "/", with(nav, "Sec-Fetch-Site", "cross-site")},
		{"a same-site page's navigation", "/", with(nav, "Sec-Fetch-Site", "same-site")},
		{"no fetch metadata", "/", with(nav, "Sec-Fetch-Site", "")},
		{"a failed nonce", "/open?nonce=bad", navNoCookie},
	} {
		mod := c.mod
		if c.why == "another Host" {
			mod = func(r *http.Request) { nav(r); r.Host = "evil.example:7317" }
		}
		if w := do(s, "GET", c.path, mod); w.Header().Get("Clear-Site-Data") != "" {
			t.Fatalf("%s took it", c.why)
		}
	}
	if w := do(s, "GET", "/", nav); w.Code != 200 || w.Header().Get("Clear-Site-Data") != ClearSiteData {
		t.Fatalf("navigation: %d %v", w.Code, w.Header())
	}
	if w := do(s, "GET", "/", nav); w.Header().Get("Clear-Site-Data") != "" {
		t.Fatal("twice")
	}

	// The launcher's open: a good nonce takes it (Sec-Fetch-Site none).
	launch := filepath.Join(t.TempDir(), "launch")
	if err := s.Auth.StartLaunch(launch); err != nil {
		t.Fatal(err)
	}
	u, _ := LaunchURL(launch, s.Auth.Origin)
	_, q, _ := strings.Cut(u, "?")
	s.Auth.ArmClearSite()
	w := do(s, "GET", "/open?"+q, with(navNoCookie, "Sec-Fetch-Site", "none"))
	if w.Code != 302 || w.Header().Get("Clear-Site-Data") != ClearSiteData {
		t.Fatalf("/open: %d %v", w.Code, w.Header())
	}

	h := http.Header{"Clear-Site-Data": {`"*"`}}
	PolicyData.Apply(h)
	if h.Get("Clear-Site-Data") != "" {
		t.Fatal("a class kept a handler's Clear-Site-Data")
	}
}
