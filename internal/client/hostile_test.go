package client

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/web"
)

// seen records what the upstream received.
type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (s *seen) add(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, r)
	s.body = append(s.body, string(b))
	s.mu.Unlock()
}

func (s *seen) last() (*http.Request, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return nil, ""
	}
	return s.reqs[len(s.reqs)-1], s.body[len(s.body)-1]
}

func (s *seen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// hostileRig is a daemon whose link reaches an upstream that holds the
// pinned server key and answers as each test says: a compromised server
// (T6), within what the link lets it do.
func hostileRig(t *testing.T) (*rig, *linktest.Upstream, *seen) {
	t.Helper()
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	r := newRig(t, keys, u.Port, nil)
	r.waitUp()
	return r, u, &seen{}
}

// answer sets a class and a type, then the status.
func answer(w http.ResponseWriter, policy, ctype string, status int, body string) {
	w.Header().Set(web.PolicyHeader, policy)
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func TestHostileUpstream(t *testing.T) {
	r, u, s := hostileRig(t)
	type tc struct {
		method, path string
		hdr          map[string]string
		up           http.HandlerFunc
		check        func(t *testing.T, resp *http.Response, body string)
	}
	refused := func(t *testing.T, resp *http.Response, body string) {
		t.Helper()
		if resp.StatusCode != http.StatusBadGateway || resp.Header.Get(LinkHeader) != Unknown || resp.Header.Get(web.PolicyHeader) != "data" || strings.Contains(body, "evil") {
			t.Fatalf("want a local 502: %d %v %q", resp.StatusCode, resp.Header, body)
		}
	}
	cases := map[string]tc{
		// N5: a 103's headers and the 1xx itself never reach the browser,
		// and the 200 still goes through.
		"103 then 200": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", "</evil.js>; rel=preload; as=script")
			w.Header().Set("Set-Cookie", "evil=1")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Del("Link")
			w.Header().Del("Set-Cookie")
			answer(w, "data", "application/json", 200, "{}")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 200 || body != "{}" || resp.Header.Get("Link") != "" || r.got1xx.Load() != 0 {
				t.Fatalf("%d %v %q, %d 1xx", resp.StatusCode, resp.Header, body, r.got1xx.Load())
			}
		}},
		"100 then 200": {"POST", "/tag", nil, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusContinue)
			answer(w, "data", "application/json", 200, `{"ok":true}`)
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 200 || r.got1xx.Load() != 0 {
				t.Fatalf("%d %q, %d 1xx", resp.StatusCode, body, r.got1xx.Load())
			}
		}},
		"duplicate Content-Type": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = []string{"application/json", "text/html"}
			answer(w, "data", "", 200, "<script>evil()</script>")
		}, refused},
		"duplicate Pneu-Policy": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header()[web.PolicyHeader] = []string{"data", "app"}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "evil")
		}, refused},
		"HTML as part-pdf": {"GET", "/part/personal/m1/2", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Disposition", "inline; filename=x.pdf")
			answer(w, "part-pdf", "text/html", 200, "<script>evil()</script>")
		}, refused},
		"HTML as a page's data": {"GET", "/", nil, func(w http.ResponseWriter, r *http.Request) {
			answer(w, "data", "text/html", 200, "<script>evil()</script>")
		}, refused},
		"app on a JSON route": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			answer(w, "app", "text/html", 200, "<script>evil()</script>")
		}, refused},
		"unknown class": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			answer(w, "trusted", "application/json", 200, "evil")
		}, refused},
		// Only the allowlist passes, and the class's set is ours.
		"Set-Cookie and friends": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Set-Cookie", "pneu=evil; Path=/")
			h.Add("Set-Cookie", "x=1")
			h.Set("Cache-Control", "public, max-age=31536000")
			h.Set("X-Frame-Options", "ALLOWALL")
			h.Set("Content-Security-Policy", "script-src *")
			h.Set("Access-Control-Allow-Origin", "*")
			h.Set("Clear-Site-Data", `"*"`)
			h.Set("Refresh", "0; url=https://evil.example/")
			h.Set("Service-Worker-Allowed", "/")
			answer(w, "data", "application/json; charset=utf-8; evil=1", 200, "{}")
		}, func(t *testing.T, resp *http.Response, body string) {
			h := resp.Header
			if resp.StatusCode != 200 || h.Get("Cache-Control") != "no-store" || h.Get("X-Frame-Options") != "DENY" ||
				h.Get("Content-Security-Policy") != "" || h.Get("Access-Control-Allow-Origin") != "" || h.Get("Clear-Site-Data") != "" ||
				h.Get("Refresh") != "" || h.Get("Service-Worker-Allowed") != "" || h.Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("%d %v", resp.StatusCode, h)
			}
		}},
		// S8: validators never go up, and never come down.
		"static validators": {"GET", "/static/app.js", map[string]string{"If-None-Match": `"abc"`, "If-Modified-Since": "Wed, 30 Sep 2026 10:00:00 GMT", "If-Range": `"abc"`, "Range": "bytes=0-1"}, func(w http.ResponseWriter, r *http.Request) {
			s.add(r)
			h := w.Header()
			h.Set("ETag", `"evil"`)
			h.Set("Last-Modified", "Wed, 30 Sep 2026 10:00:00 GMT")
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			h.Set("Expires", "Thu, 01 Jan 2099 00:00:00 GMT")
			answer(w, "data", "text/javascript", 200, "ok()")
		}, func(t *testing.T, resp *http.Response, body string) {
			up, _ := s.last()
			for _, k := range conditionals {
				if up.Header.Get(k) != "" {
					t.Errorf("%s went upstream", k)
				}
			}
			if up.Header.Get("Range") == "" {
				t.Error("Range was dropped")
			}
			h := resp.Header
			if resp.StatusCode != 200 || body != "ok()" || h.Get("ETag") != "" || h.Get("Last-Modified") != "" || h.Get("Expires") != "" || h.Get("Cache-Control") != "no-cache" {
				t.Fatalf("%d %v", resp.StatusCode, h)
			}
		}},
		"static 304": {"GET", "/static/app.js", map[string]string{"If-None-Match": `"abc"`}, func(w http.ResponseWriter, r *http.Request) {
			answer(w, "data", "", http.StatusNotModified, "")
		}, refused},
		"SVG navigated as data": {"GET", "/static/logo.svg", nil, func(w http.ResponseWriter, r *http.Request) {
			answer(w, "data", "image/svg+xml", 200, "<svg onload=evil()>")
		}, refused},
		"SVG part outside an image": {"GET", "/part/personal/m1/1", map[string]string{"Sec-Fetch-Dest": "document"}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Disposition", "attachment; filename=a.svg")
			answer(w, "part-sandbox", "image/svg+xml", 200, "<svg onload=evil()>")
		}, refused},
		"SVG part as an image": {"GET", "/part/personal/m1/1", map[string]string{"Sec-Fetch-Dest": "image"}, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Sec-Fetch-Dest") != "image" {
				http.Error(w, "no dest", 400)
				return
			}
			w.Header().Set("Content-Disposition", "attachment; filename=a.svg")
			answer(w, "part-sandbox", "image/svg+xml", 200, "<svg/>")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 200 || resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get(web.PolicyHeader) != "part-sandbox" {
				t.Fatalf("%d %v", resp.StatusCode, resp.Header)
			}
		}},
		// Content-Disposition is rebuilt: a format character (U+202E, which
		// shows "gnp.exe" as "exe.png") and the raw parameters are gone.
		"disposition rebuilt": {"GET", "/part/personal/m1/3", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Disposition", `inline; filename*=UTF-8''a%E2%80%AEgnp.exe; x="evil"`)
			w.Header().Set("Content-Range", "bytes 0-1/2")
			w.Header().Set("Accept-Ranges", "bytes")
			answer(w, "part-sandbox", "image/png", 200, "\x89P")
		}, func(t *testing.T, resp *http.Response, body string) {
			h := resp.Header
			if resp.StatusCode != 200 || h.Get("Content-Disposition") != "inline; filename*=UTF-8''agnp.exe" || h.Get("Content-Range") != "bytes 0-1/2" || h.Get("Accept-Ranges") != "bytes" {
				t.Fatalf("%d %v", resp.StatusCode, h)
			}
		}},
		"Content-Range off a part": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-1/2")
			answer(w, "data", "application/json", 200, "{}")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 200 || resp.Header.Get("Content-Range") != "" {
				t.Fatalf("%d %v", resp.StatusCode, resp.Header)
			}
		}},
		"gzip": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			answer(w, "data", "application/json", 200, "evil")
		}, refused},
		// N4: redirects stay on the local origin, or are exactly the
		// account's Gmail.
		"absolute Location": {"GET", "/gmail/personal/t1", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://evil.example/")
			answer(w, "data", "", http.StatusFound, "")
		}, refused},
		"Gmail as another user": {"GET", "/gmail/personal/t1", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://mail.google.com/mail/?authuser=evil%40example.com#all/18c0ffee")
			answer(w, "data", "", http.StatusFound, "")
		}, refused},
		"Gmail": {"GET", "/gmail/personal/t1", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://mail.google.com/mail/?authuser=me%40example.com#all/18c0ffee")
			answer(w, "app", "text/html; charset=utf-8", http.StatusFound, "<a>Found</a>")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 302 || resp.Header.Get("Location") != "https://mail.google.com/mail/?authuser=me%40example.com#all/18c0ffee" {
				t.Fatalf("%d %v", resp.StatusCode, resp.Header)
			}
		}},
		"Gmail for an unknown account": {"GET", "/gmail/work/t1", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://mail.google.com/mail/?authuser=me%40example.com#all/18c0ffee")
			answer(w, "data", "", http.StatusFound, "")
		}, refused},
		"//evil Location": {"POST", "/send", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "//evil.example/x")
			answer(w, "compose", "", http.StatusSeeOther, "")
		}, func(t *testing.T, resp *http.Response, body string) {
			// /send's errors are text (its route answers no JSON).
			if resp.StatusCode != 502 || resp.Header.Get(LinkHeader) != Unknown || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || resp.Header.Get("Location") != "" {
				t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
			}
		}},
		`\evil Location`: {"POST", "/send", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", `/\evil.example/x`)
			answer(w, "compose", "", http.StatusSeeOther, "")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 502 || resp.Header.Get("Location") != "" {
				t.Fatalf("%d %v", resp.StatusCode, resp.Header)
			}
		}},
		"local Location": {"POST", "/send", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/t/personal/abc")
			answer(w, "compose", "", http.StatusSeeOther, "")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 303 || resp.Header.Get("Location") != testOrigin+"/t/personal/abc" {
				t.Fatalf("%d %v", resp.StatusCode, resp.Header)
			}
		}},
		"Location on a 200": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/x")
			answer(w, "data", "application/json", 200, "{}")
		}, refused},
		// C1: untyped answers. net/http sniffs a type into any body that
		// has none, nosniff or not, so an untyped answer carries no body,
		// and only statuses Fetch treats as redirects (or 204, 304) may go
		// untyped at all.
		"300 untyped with HTML": {"GET", "/gmail/personal/t1", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://mail.google.com/mail/?authuser=me%40example.com#all/18c0ffee")
			w.Header()["Content-Type"] = nil // no sniffing upstream either
			answer(w, "data", "", http.StatusMultipleChoices, "<html><script>evil()</script>")
		}, refused},
		"302 untyped with HTML": {"GET", "/gmail/personal/t1", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://mail.google.com/mail/?authuser=me%40example.com#all/18c0ffee")
			w.Header()["Content-Type"] = nil
			answer(w, "data", "", http.StatusFound, "<html><script>evil()</script>")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 302 || body != "" || resp.Header.Get("Content-Type") != "" || resp.Header.Get("Content-Length") != "0" || resp.Header.Get("Location") == "" {
				t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
			}
		}},
		"303 untyped with HTML": {"POST", "/send", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/t/personal/abc")
			w.Header()["Content-Type"] = nil
			answer(w, "compose", "", http.StatusSeeOther, "<html><script>evil()</script>")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 303 || body != "" || resp.Header.Get("Content-Type") != "" {
				t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
			}
		}},
		"200 untyped": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			answer(w, "data", "", 200, "<html><script>evil()</script>")
		}, refused},
		"204 untyped with a body": {"POST", "/tag", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			answer(w, "data", "", http.StatusNoContent, "")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 204 || body != "" || resp.Header.Get("Content-Type") != "" {
				t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
			}
		}},
		"redirect on a route that has none": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/")
			answer(w, "data", "text/plain", http.StatusMovedPermanently, "moved")
		}, refused},
		"307 typed HTML redirect": {"GET", "/gmail/personal/t1", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://mail.google.com/mail/?authuser=me%40example.com#all/18c0ffee")
			answer(w, "app", "text/html; charset=utf-8", http.StatusTemporaryRedirect, "<a>moved</a>")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 307 || body != "<a>moved</a>" || resp.Header.Get("Content-Security-Policy") != web.AppCSP {
				t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
			}
		}},
		"huge headers": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Big", strings.Repeat("a", 256<<10))
			answer(w, "data", "application/json", 200, "{}")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode < 500 || resp.Header.Get("X-Big") != "" {
				t.Fatalf("%d %.200v", resp.StatusCode, resp.Header)
			}
		}},
		"trailers": {"GET", "/status", nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Trailer", "X-Evil")
			answer(w, "data", "application/json", 200, "{}")
			http.NewResponseController(w).Flush() // streamed: no length, so the browser's answer is chunked
			w.Header().Set("X-Evil", "1")
			w.Header().Set(http.TrailerPrefix+"Set-Cookie", "pneu=evil")
		}, func(t *testing.T, resp *http.Response, body string) {
			if resp.StatusCode != 200 || body != "{}" || len(resp.Trailer) != 0 || resp.Header.Get("Trailer") != "" {
				t.Fatalf("%d %v trailer %v", resp.StatusCode, resp.Header, resp.Trailer)
			}
		}},
		// R3: what goes up.
		"request headers": {"POST", "/tag", map[string]string{
			"Referer": testOrigin + "/", "Authorization": "Basic eDp5", "X-Forwarded-For": "1.2.3.4", "X-Forwarded-Host": "evil",
			"Forwarded": "for=1.2.3.4", "X-Real-Ip": "1.2.3.4", "Connection": "X-Secret", "X-Secret": "1", "Te": "trailers",
			"Accept-Encoding": "gzip", "X-Pneu-Window": "w1", "Pneu-Protocol": "9", "Sec-Fetch-Site": "same-origin",
		}, func(w http.ResponseWriter, r *http.Request) {
			s.add(r)
			answer(w, "data", "application/json", 200, `{"ok":true}`)
		}, func(t *testing.T, resp *http.Response, body string) {
			up, got := s.last()
			if resp.StatusCode != 200 || up.Method != "POST" || got != "a=1" || up.Host != u.Host() || up.URL.Path != "/tag" {
				t.Fatalf("%d %s %q %q", resp.StatusCode, up.Method, got, up.Host)
			}
			for _, k := range []string{"Cookie", "Origin", "Referer", "Authorization", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip", "X-Secret", "Te", "Pneu-Protocol"} {
				if v := up.Header.Get(k); v != "" {
					t.Errorf("%s: %q went upstream", k, v)
				}
			}
			if up.Header.Get("X-Pneu-Window") != "w1" || up.Header.Get("Sec-Fetch-Site") != "same-origin" || up.Header.Get("Accept-Encoding") == "gzip" {
				t.Errorf("headers up %v", up.Header)
			}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r.got1xx.Store(0)
			u.SetHandler(c.up)
			body := ""
			if c.method == "POST" {
				body = "a=1"
			}
			resp, got := r.do(r.req(c.method, c.path, body, c.hdr))
			c.check(t, resp, got)
		})
	}
}

// Requests that never go upstream: routes the table doesn't have, the
// server-only hello, a wrong method, a service worker's script, upgrades.
func TestRefusedLocally(t *testing.T) {
	r, u, _ := hostileRig(t)
	var upstream atomic.Int32 // the daemon's own /events aside
	u.SetHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			upstream.Add(1)
		}
		answer(w, "data", "text/plain", 200, "upstream")
	}))
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
		status       int
	}{
		{"GET", "/nope", nil, 404},
		{"GET", "/sw.js", nil, 404},
		{"GET", "/peer/hello", nil, 404},
		{"GET", "/t/a/b/c", nil, 404},
		{"GET", "/tag", nil, 405},
		{"DELETE", "/t/a/b", nil, 405},
		{"GET", "/static/app.js", map[string]string{"Service-Worker": "script"}, 404},
		{"GET", "/", map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}, 400},
		// Auth first: no cookie, or a POST without our Origin.
		{"GET", "/", map[string]string{"Cookie": ""}, 403},
		{"POST", "/tag", map[string]string{"Origin": "http://evil.example"}, 403},
		{"POST", "/tag", map[string]string{"Origin": ""}, 403},
	} {
		resp, body := r.do(r.req(c.method, c.path, "", c.hdr))
		if resp.StatusCode != c.status || strings.Contains(body, "upstream") {
			t.Errorf("%s %s %v: %d %q", c.method, c.path, c.hdr, resp.StatusCode, body)
		}
	}
	if n := upstream.Load(); n != 0 {
		t.Fatalf("%d requests went upstream", n)
	}
}

// A stalled body is cancelled on its own after the idle limit; another
// stream on the same connection carries on, and the connection stays.
func TestStalledBody(t *testing.T) {
	r, u, _ := hostileRig(t)
	r.d.idle = 300 * time.Millisecond
	u.SetHandler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/body/personal/stall":
			answer(w, "data", "application/json", 200, `{"html":"`)
			http.NewResponseController(w).Flush()
			<-req.Context().Done()
		case "/body/personal/slow":
			// Slow but moving: a byte every 100ms for a second.
			answer(w, "data", "application/json", 200, "")
			for range 10 {
				w.Write([]byte(" "))
				http.NewResponseController(w).Flush()
				time.Sleep(100 * time.Millisecond)
			}
		default:
			answer(w, "data", "application/json", 200, "{}")
		}
	}))
	conns := u.Conns.Load()
	stalled := make(chan error, 1)
	go func() {
		resp, err := r.hc.Do(r.req("GET", "/body/personal/stall", "", nil))
		if err != nil {
			stalled <- err
			return
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		stalled <- err
	}()
	slow := make(chan int, 1)
	go func() {
		resp, err := r.hc.Do(r.req("GET", "/body/personal/slow", "", nil))
		if err != nil {
			slow <- -1
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			slow <- -2
			return
		}
		slow <- len(b)
	}()
	select {
	case err := <-stalled:
		if err == nil {
			t.Fatal("the stalled body ended cleanly; want it cut")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled body was never cancelled")
	}
	if n := <-slow; n != 10 {
		t.Fatalf("the moving stream got %d bytes", n)
	}
	if resp, body := r.get("/status", nil); resp.StatusCode != 200 || body != "{}" {
		t.Fatalf("after the stall: %d %q", resp.StatusCode, body)
	}
	if u.Conns.Load() != conns {
		t.Fatalf("connections %d -> %d: the stall took the connection down", conns, u.Conns.Load())
	}
}
