package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Admit passes only the allowlist, rebuilt; the rest of the boundary is
// covered end to end in internal/client.
func TestAdmit(t *testing.T) {
	c := Checker{Origin: "http://pneu.localhost:7317", Email: func(string) (string, bool) { return "me@example.com", true }}
	part := routeFor(t, "GET", "/part/{account}/{msgid}/{n}")
	status := routeFor(t, "GET", "/status")
	req := httptest.NewRequest("GET", "/part/personal/m/1", nil)
	req.SetPathValue("account", "personal")
	up := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			h.Add(kv[i], kv[i+1])
		}
		return h
	}
	a, err := c.Admit(part, 206, up(PolicyHeader, "part-sandbox", "Content-Type", `image/PNG; charset="ev il"; x=1`,
		"Content-Disposition", "attachment; filename=\"a\u202eb.png\"", "Content-Length", "2", "Content-Range", "bytes 0-1/9",
		"Accept-Ranges", "bytes", "ETag", `"x"`, "Set-Cookie", "a=b", "X-Other", "1"), req)
	if err != nil {
		t.Fatal(err)
	}
	want := http.Header{"Content-Type": {"image/png"}, "Content-Length": {"2"}, "Content-Range": {"bytes 0-1/9"},
		"Accept-Ranges": {"bytes"}, "Content-Disposition": {"attachment; filename*=UTF-8''ab.png"}}
	if a.Policy != PolicyPart || len(a.Header) != len(want) {
		t.Fatalf("admitted %+v", a)
	}
	for k, v := range want {
		if a.Header.Get(k) != v[0] {
			t.Errorf("%s: %q, want %q", k, a.Header.Get(k), v[0])
		}
	}
	for name, h := range map[string]http.Header{
		"two lengths":    up(PolicyHeader, "data", "Content-Type", "application/json", "Content-Length", "1", "Content-Length", "2"),
		"signed length":  up(PolicyHeader, "data", "Content-Type", "application/json", "Content-Length", "-1"),
		"deflate":        up(PolicyHeader, "data", "Content-Type", "application/json", "Content-Encoding", "deflate"),
		"no class":       up("Content-Type", "application/json"),
		"html as data":   up(PolicyHeader, "data", "Content-Type", "text/html"),
		"redirect on it": up(PolicyHeader, "data", "Content-Type", "application/json", "Location", "/"),
	} {
		if _, err := c.Admit(status, 200, h, req); err == nil {
			t.Errorf("%s admitted", name)
		}
	}
	bad := up(PolicyHeader, "part-sandbox", "Content-Type", "image/png", "Content-Disposition", "inline; filename=a.png", "Content-Range", "bytes 0-1/9, 2-3/9")
	if _, err := c.Admit(part, 206, bad, req); err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Errorf("multi-range Content-Range: %v", err)
	}
	// Install needs the middleware's writer: nothing else can apply a class.
	if err := a.Install(httptest.NewRecorder(), part); err == nil {
		t.Error("installed without a policy writer")
	}
}

func routeFor(t *testing.T, method, pattern string) *Route {
	t.Helper()
	for i := range Routes {
		if Routes[i].Method == method && Routes[i].Pattern == pattern {
			return &Routes[i]
		}
	}
	t.Fatalf("no route %s %s", method, pattern)
	return nil
}

// C1 on the server's own writer: a body never goes out without a type
// net/http could sniff, and only Fetch's redirect statuses redirect.
func TestUntypedResponses(t *testing.T) {
	c := Checker{Origin: "http://pneu.localhost:7317"}
	gmail := routeFor(t, "GET", "/gmail/{account}/{thread}")
	req := httptest.NewRequest("GET", "/gmail/a/t", nil)
	h := http.Header{PolicyHeader: {"data"}, "Location": {"/"}}
	if err := c.Check(gmail, 300, h, req); err == nil {
		t.Error("300 passed Check")
	}
	for _, st := range []int{305, 399} {
		if err := c.Check(routeFor(t, "GET", "/status"), st, http.Header{PolicyHeader: {"data"}, "Content-Type": {"text/plain"}}, req); err == nil {
			t.Errorf("%d passed Check", st)
		}
	}
	if err := c.Check(routeFor(t, "GET", "/status"), 200, http.Header{PolicyHeader: {"data"}}, req); err == nil {
		t.Error("an untyped 200 passed Check")
	}

	run := func(f func(w http.ResponseWriter)) (*httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		pw := &policyWriter{ResponseWriter: rec, r: req}
		var err error
		f(writerFunc{pw, &err})
		return rec, err
	}
	// A status first, then a body with no type: typed text/plain at the
	// header write, never sniffed.
	rec, _ := run(func(w http.ResponseWriter) { w.WriteHeader(200); w.Write([]byte("<html><script>x</script>")) })
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("untyped 200: %q", ct)
	}
	// An untyped redirect takes no body.
	rec, err := run(func(w http.ResponseWriter) {
		w.Header().Set("Location", "/")
		w.WriteHeader(302)
		w.Write([]byte("<html><script>x</script>"))
	})
	if err == nil || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Errorf("untyped 302 with a body: %v %q %v", err, rec.Body, rec.Header())
	}
}

// writerFunc records the last Write error.
type writerFunc struct {
	http.ResponseWriter
	err *error
}

func (w writerFunc) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	*w.err = err
	return n, err
}
