package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/web"
)

const (
	testHost   = "pneu.localhost:7400"
	testOrigin = "http://" + testHost
	testToken  = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
)

// rig is a client daemon on a link to port, and a browser for it that
// speaks through the daemon's Auth like the --app window.
type rig struct {
	t      *testing.T
	keys   linktest.Keys
	api    *linktest.API
	link   *link.Link
	d      *Daemon
	srv    *httptest.Server
	hc     *http.Client
	got1xx atomic.Int32 // informational responses the browser saw
}

func newRig(t *testing.T, keys linktest.Keys, port int, api *linktest.API) *rig {
	t.Helper()
	if api == nil {
		api = linktest.NewAPI()
	}
	l := link.New(api, keys.Creds, port)
	l.Timing.BackoffMin, l.Timing.BackoffMax = 20*time.Millisecond, 100*time.Millisecond
	l.Timing.ProbeWait = time.Second
	l.Start()
	t.Cleanup(l.Close)
	auth := web.NewAuth(testHost, testToken)
	theme := filepath.Join(t.TempDir(), "theme.css")
	os.WriteFile(theme, []byte(":root { --bg: #102030; }"), 0o600)
	d := New(Config{Auth: auth, Link: l, ThemePath: theme})
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	r := &rig{t: t, keys: keys, api: api, link: l, d: d, srv: srv}
	r.hc = &http.Client{
		Transport:     &http.Transport{DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	t.Cleanup(r.hc.CloseIdleConnections)
	return r
}

func (r *rig) waitUp() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !r.link.WaitUp(ctx) {
		r.t.Fatalf("link never came up: %+v", r.link.State())
	}
}

// req is a browser request: the daemon's Host, the session cookie, and for
// anything but GET the page's Origin; hdr adds or (with "") removes.
func (r *rig) req(method, path, body string, hdr map[string]string) *http.Request {
	r.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.srv.URL+path, rd)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("Cookie", web.CookieName+"="+testToken)
	if method != "GET" && method != "HEAD" {
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(int, textproto.MIMEHeader) error { r.got1xx.Add(1); return nil },
	}))
}

// do sends req and reads the whole answer. Every answer, local or proxied,
// must carry a class and the base set, and never a cookie but /open's.
func (r *rig) do(req *http.Request) (*http.Response, string) {
	r.t.Helper()
	resp, err := r.hc.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		// A cut or short body is a failure wherever do is used: tests of
		// cut streams read for themselves.
		r.t.Fatalf("%s %s: body after %d bytes: %v", req.Method, req.URL.Path, len(b), err)
	}
	if resp.ContentLength >= 0 && int64(len(b)) != resp.ContentLength && req.Method != "HEAD" {
		r.t.Fatalf("%s %s: %d bytes of %d", req.Method, req.URL.Path, len(b), resp.ContentLength)
	}
	h := resp.Header
	if h.Get(web.PolicyHeader) == "" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") == "" {
		r.t.Errorf("%s %s: unpoliced answer %v", req.Method, req.URL.Path, h)
	}
	if req.URL.Path != "/open" && len(h.Values("Set-Cookie")) > 0 {
		r.t.Errorf("%s %s: Set-Cookie passed: %v", req.Method, req.URL.Path, h.Values("Set-Cookie"))
	}
	return resp, string(b)
}

func (r *rig) get(path string, hdr map[string]string) (*http.Response, string) {
	r.t.Helper()
	return r.do(r.req("GET", path, "", hdr))
}
