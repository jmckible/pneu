package web

import (
	"errors"
	"net/http"
)

// Policy is a response's security class: the complete set of security
// headers it carries. Handlers choose a class; nothing else sets those
// headers. The response names its class in Pneu-Policy, which the client
// proxy (docs/client.md, "Proxy rules") checks against the route table and
// then re-applies with the same Apply, so the two modes can't drift.
type Policy string

const (
	// PolicyApp is an app page, or any HTML the app answers with (a GET
	// redirect's body): AppCSP on top of the base set.
	PolicyApp Policy = "app"
	// PolicyCompose is PolicyApp with Referrer-Policy same-origin: under
	// no-referrer Chromium sends "Origin: null" on the form's POST, which
	// Auth refuses. Only /compose, /reply and /send's re-render are forms.
	PolicyCompose Policy = "compose"
	// PolicyData is everything that isn't a document: JSON, SSE, CSS,
	// scripts, plain-text errors. No CSP: none applies to a subresource,
	// and Check refuses these types as HTML.
	PolicyData Policy = "data"
	// PolicyPart is a message part: a sandbox CSP, so if one is ever
	// navigated to and rendered it runs nothing.
	PolicyPart Policy = "part-sandbox"
	// PolicyPDF is a PDF part, the one frameable response: Chromium's viewer
	// refuses a sandbox CSP, so it gets frame-ancestors 'self' and
	// SAMEORIGIN in place of the sandbox and DENY.
	PolicyPDF Policy = "part-pdf"
	// PolicyStaticSVG is an SVG under /static/ (the favicon): an image to
	// <img> and <link rel=icon>, but a scriptable document if navigated
	// to, so a sandbox with nothing to load. Not PolicyPart: that one is
	// the part route's, with img-src 'self' the favicon doesn't need.
	PolicyStaticSVG Policy = "static-svg"
)

// PolicyHeader names a response's class.
const PolicyHeader = "Pneu-Policy"

// staticSVGCSP is PolicyStaticSVG's policy.
const staticSVGCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'"

// partCSP is PolicyPart's policy.
const partCSP = "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'"

// securityHeaders are the headers Apply owns: whatever a handler (or
// net/http, or an upstream) put in them is replaced.
var securityHeaders = []string{
	"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options",
	"Cross-Origin-Resource-Policy", "Cross-Origin-Opener-Policy",
	"Content-Security-Policy", "Content-Security-Policy-Report-Only", PolicyHeader,
}

var errPolicy = errors.New("unknown policy class")

// Apply replaces h's security headers with p's complete set.
func (p Policy) Apply(h http.Header) error {
	if !p.valid() {
		return errPolicy
	}
	for _, k := range securityHeaders {
		h.Del(k)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	// Other ports of pneu.localhost are same-site: without these a page
	// there could embed our responses (CORP) or keep a handle on our window
	// (COOP).
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	switch p {
	case PolicyApp:
		h.Set("Content-Security-Policy", AppCSP)
	case PolicyCompose:
		h.Set("Content-Security-Policy", AppCSP)
		h.Set("Referrer-Policy", "same-origin")
	case PolicyPart:
		h.Set("Content-Security-Policy", partCSP)
	case PolicyPDF:
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Content-Security-Policy", "frame-ancestors 'self'")
	case PolicyStaticSVG:
		h.Set("Content-Security-Policy", staticSVGCSP)
	}
	h.Set(PolicyHeader, string(p))
	return nil
}

func (p Policy) valid() bool {
	switch p {
	case PolicyApp, PolicyCompose, PolicyData, PolicyPart, PolicyPDF, PolicyStaticSVG:
		return true
	}
	return false
}

// policyWriter applies the response's class and its route's cache rule at
// the one header write. A handler that named no class gets PolicyApp for
// HTML and PolicyData for anything else, decided from the Content-Type as
// written. Unwrap keeps http.ResponseController (SSE flushing) working.
type policyWriter struct {
	http.ResponseWriter
	r      *http.Request
	route  *Route // nil until the table's handler runs (middleware refusals, mux 404s)
	policy Policy // "" until usePolicy
	wrote  bool
	// check sees every response once its headers are final; tests only.
	check func(rt *Route, status int, h http.Header, r *http.Request)
}

func (w *policyWriter) WriteHeader(code int) {
	if code < 200 {
		// An informational response would go out with whatever the map
		// holds, unpoliced, and then read as the one header write. Swallowed:
		// the class applies at the final status (docs/client.md, N5).
		return
	}
	if w.wrote {
		w.ResponseWriter.WriteHeader(code) // net/http's superfluous-call warning
		return
	}
	w.wrote = true
	h := w.Header()
	p := w.policy
	switch {
	case p == "":
		p = PolicyData
		if isHTML(h.Get("Content-Type")) {
			p = PolicyApp
		}
	case code >= 400 && (p == PolicyPart || p == PolicyPDF || p == PolicyStaticSVG):
		// ServeContent's own errors (416, 412) are text/plain messages,
		// not the file: plain data, without the file's disposition.
		p = PolicyData
		h.Del("Content-Disposition")
		h.Set("Content-Type", "text/plain; charset=utf-8") // a 412 has no body, and kept the file's type
	}
	if code == http.StatusNotModified || code == http.StatusNoContent {
		// No representation, so nothing to dispose of (ServeContent's 304
		// drops the part's type and keeps its disposition).
		h.Del("Content-Disposition")
	}
	p.Apply(h)
	cache := CacheNoStore
	if w.route != nil {
		cache = w.route.Cache
	}
	h.Set("Cache-Control", string(cache))
	if w.check != nil {
		w.check(w.route, code, h, w.r)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *policyWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		if w.Header().Get("Content-Type") == "" {
			// What net/http would sniff anyway; decided here so it is seen.
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *policyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FlushError is what http.ResponseController finds before unwrapping: a
// flush ahead of the header write would otherwise send the headers
// without the class.
func (w *policyWriter) FlushError() error {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// finish writes the headers of a response the handler never wrote to.
func (w *policyWriter) finish() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
}

// findPolicyWriter unwraps w to the middleware's writer, if any.
func findPolicyWriter(w http.ResponseWriter) *policyWriter {
	for {
		switch v := w.(type) {
		case *policyWriter:
			return v
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return nil
		}
	}
}

// usePolicy chooses the response's class; call it before the header write.
// Outside the middleware (a handler under test on its own) it applies the
// class at once.
func usePolicy(w http.ResponseWriter, p Policy) {
	if pw := findPolicyWriter(w); pw != nil {
		pw.policy = p
		return
	}
	p.Apply(w.Header())
}
