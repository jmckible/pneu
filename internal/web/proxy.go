package web

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Admitted is an upstream answer the client proxy accepted (docs/client.md,
// "Responses coming down"): the class it re-applies and the few upstream
// headers it passes on, each validated and rebuilt. Everything else the
// upstream sent is dropped: Set-Cookie, Cache-Control, ETag, Last-Modified,
// its own security headers, trailers.
type Admitted struct {
	Policy Policy
	Header http.Header
	// Bodiless: no Content-Type, so a 204, 304 or redirect (Untyped),
	// which goes on without a body: the proxy discards the upstream's, as
	// anything written would be sniffed.
	Bodiless bool
}

var (
	contentRangeRE = regexp.MustCompile(`^bytes (?:\d{1,19}-\d{1,19}/(?:\d{1,19}|\*)|\*/\d{1,19})$`)
	lengthRE       = regexp.MustCompile(`^\d{1,19}$`)
	charsetRE      = regexp.MustCompile(`^[A-Za-z0-9._:+-]{1,40}$`)
)

// Admit runs Check on an upstream answer to r (the browser's request, as
// the client's mux matched it) on route rt, then builds the headers that
// pass: exactly one Content-Type, parsed and re-emitted with at most a
// charset; Content-Length; Content-Range and Accept-Ranges on part
// routes; Content-Disposition rebuilt from its parsed type and filename;
// Location as location() admits it. The server nominates the class; the
// client accepts it only because Check found it allowed here (N1).
func (c Checker) Admit(rt *Route, status int, up http.Header, r *http.Request) (Admitted, error) {
	if rt == nil {
		return Admitted{}, errors.New("no route")
	}
	if status < 200 || status > 599 {
		return Admitted{}, fmt.Errorf("status %d", status)
	}
	if err := c.Check(rt, status, up, r); err != nil {
		return Admitted{}, err
	}
	for _, ce := range up.Values("Content-Encoding") {
		if !strings.EqualFold(strings.TrimSpace(ce), "identity") {
			// The proxy asks for identity and passes no Content-Encoding.
			return Admitted{}, fmt.Errorf("Content-Encoding %q", ce)
		}
	}
	p := Policy(up.Get(PolicyHeader))
	h := http.Header{}
	if cts := up.Values("Content-Type"); len(cts) == 1 {
		mt, params, _ := mime.ParseMediaType(cts[0]) // Check parsed it
		out := map[string]string{}
		if cs := params["charset"]; charsetRE.MatchString(cs) {
			out["charset"] = cs
		}
		h.Set("Content-Type", mime.FormatMediaType(strings.ToLower(mt), out))
	}
	bodiless := len(up.Values("Content-Type")) == 0
	switch cl := up.Values("Content-Length"); {
	case bodiless:
		// The body is dropped; a redirect says so, a 204 or 304 has none.
		if RedirectStatus(status) {
			h.Set("Content-Length", "0")
		}
	case len(cl) == 1 && lengthRE.MatchString(cl[0]):
		h.Set("Content-Length", cl[0])
	case len(cl) > 0:
		return Admitted{}, fmt.Errorf("Content-Length %q", cl)
	}
	if rt.Disposition == DispPart {
		switch cr := up.Values("Content-Range"); {
		case len(cr) == 1 && contentRangeRE.MatchString(cr[0]):
			h.Set("Content-Range", cr[0])
		case len(cr) > 0:
			return Admitted{}, fmt.Errorf("Content-Range %q", cr)
		}
		switch ar := up.Values("Accept-Ranges"); {
		case len(ar) == 1 && (ar[0] == "bytes" || ar[0] == "none"):
			h.Set("Accept-Ranges", ar[0])
		case len(ar) > 0:
			return Admitted{}, fmt.Errorf("Accept-Ranges %q", ar)
		}
	}
	if ds := up.Values("Content-Disposition"); len(ds) == 1 {
		// Check allowed it: a part, attachment or inline, with a filename.
		disp, params, _ := mime.ParseMediaType(ds[0])
		h.Set("Content-Disposition", disp+"; filename*=UTF-8''"+rfc5987(proxiedFilename(params["filename"])))
	}
	loc, err := c.location(rt, status, up, r)
	if err != nil {
		return Admitted{}, err
	}
	if loc != "" {
		h.Set("Location", loc)
	}
	return Admitted{Policy: p, Header: h, Bodiless: bodiless}, nil
}

// proxiedFilename is the server's filename as the client passes it on:
// valid UTF-8, cleaned as cleanFilename cleans a part's name (no control,
// format or path characters), at most 255 bytes.
func proxiedFilename(s string) string {
	s = cleanFilename(strings.ToValidUTF8(s, ""))
	for len(s) > 255 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	if s == "" {
		s = "file"
	}
	return s
}

// Install makes a the response's headers on w, which must be the Auth
// middleware's writer (or wrap it): the map is cleared, a's headers go in,
// and at the header write the writer applies a's class and rt's cache
// rule, as it does for the server's own handlers.
func (a Admitted) Install(w http.ResponseWriter, rt *Route) error {
	pw := findPolicyWriter(w)
	if pw == nil || pw.wrote {
		return errors.New("web: no policy writer to install on")
	}
	if !a.Policy.valid() {
		return errPolicy
	}
	h := pw.Header()
	clear(h)
	for k, v := range a.Header {
		h[k] = v
	}
	pw.route, pw.policy = rt, a.Policy
	return nil
}
