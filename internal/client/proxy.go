package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/web"
)

// LinkHeader names, on an answer the daemon wrote itself because the
// server's didn't come through, what happened to the request (R10, N6):
//
//	not-sent  no connection was ever handed to it (the link was down, or
//	          the dial or TLS handshake failed): nothing reached the
//	          server, and pressing again is safe. 502.
//	unknown   it had a connection, so bytes may have reached the server
//	          (a failure while writing counts), or the server answered
//	          outside its route's contract. 504, or 502 for a refused
//	          answer.
//
// Mutations get JSON {"ok":false,"error":…,"link":…} as tagFail's shape;
// everything else text/plain.
const LinkHeader = "Pneu-Link"

// Outcomes in LinkHeader.
const (
	NotSent = "not-sent"
	Unknown = "unknown"
)

// exchange is one proxied request's state, carried in its context from
// Rewrite to the response writer.
type exchange struct {
	rt *web.Route
	in *http.Request // the browser's, as the mux matched it
	// gotConn: a connection was handed to the request, so bytes may have
	// left (httptrace.GotConn, which fires only after the pinned handshake).
	gotConn  atomic.Bool
	admitted *web.Admitted
}

type exchangeKey struct{}

func exchangeOf(r *http.Request) *exchange {
	ex, _ := r.Context().Value(exchangeKey{}).(*exchange)
	return ex
}

// proxy is a proxied route's handler. A request first lets a starting
// link settle (settle), so a page loaded the moment the laptop wakes
// waits for the reconnect rather than failing; a page navigation while
// the link still isn't up gets the error page instead. POST /sync (R, and
// the page's focus return) also asks the link for a probe, or an attempt
// when down: the moment you look is when a dead link must be noticed
// (R12).
func (d *Daemon) proxy(rt *web.Route) http.Handler {
	syncRoute := rt.Method == http.MethodPost && rt.Pattern == "/sync"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.up.CheckWake()
		if syncRoute {
			d.up.Retry()
		}
		d.settle(r.Context())
		if pageNavigation(rt, r) {
			if st := d.up.State(); st.Reason != link.Up {
				d.errorPage(w, rt, st, NotSent)
				return
			}
		}
		ex := &exchange{rt: rt, in: r}
		fw := &finalWriter{w: w, ex: ex, hdr: http.Header{}, d: d}
		d.rp.ServeHTTP(fw, r.WithContext(context.WithValue(r.Context(), exchangeKey{}, ex)))
	})
}

// stripUp are request headers that never go upstream (R3): credentials and
// origin (the server's only credential is the TLS peer), forwarding
// headers, hop-by-hop, upgrades and trailers. Accept-Encoding too: the
// answer comes back identity.
var stripUp = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Cookie", "Authorization", "Origin", "Referer", "Forwarded", "X-Real-Ip", "Accept-Encoding",
}

// conditionals are the validators stripped from /static/ requests (S8):
// the answer is always a fresh 200, never a 304 that would keep whatever
// the browser holds.
var conditionals = []string{"If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "If-Range"}

// rewrite builds the upstream request: a fixed scheme and a placeholder
// authority (the link sets the address it checked), the browser's path and
// query, and its headers less stripUp, anything its Connection named,
// every X-Forwarded-*, and our own Pneu-* names.
func (d *Daemon) rewrite(pr *httputil.ProxyRequest) {
	ex := exchangeOf(pr.In)
	out := pr.Out
	out.URL = &url.URL{Scheme: "https", Host: "server", Path: pr.In.URL.Path, RawPath: pr.In.URL.RawPath, RawQuery: pr.In.URL.RawQuery}
	out.Host = ""
	h := out.Header
	for _, v := range pr.In.Header.Values("Connection") {
		for f := range strings.SplitSeq(v, ",") {
			if f = textproto.TrimString(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, k := range stripUp {
		h.Del(k)
	}
	for k := range h {
		if strings.HasPrefix(k, "X-Forwarded-") || strings.HasPrefix(k, "Pneu-") {
			delete(h, k)
		}
	}
	if ex.rt.Pattern == "/static/" {
		for _, k := range conditionals {
			h.Del(k)
		}
	}
	out.Trailer = nil
	out = out.WithContext(httptrace.WithClientTrace(out.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { ex.gotConn.Store(true) },
	}))
	pr.Out = out
}

// errContract: the server's answer broke its route's contract.
var errContract = errors.New("the server's answer broke its route's contract")

// modify admits the upstream answer or refuses it (a local 502 through
// fail). The upstream's headers go no further than this: the final writer
// installs only what Admit built.
func (d *Daemon) modify(resp *http.Response) error {
	ex := exchangeOf(resp.Request)
	if ex.rt.Pattern == "/static/" && resp.StatusCode == http.StatusNotModified {
		// It was asked without validators (S8): a 304 would keep whatever
		// the browser holds.
		log.Printf("client: %s %s: a 304 to an unconditional request", ex.in.Method, ex.rt.Pattern)
		return errContract
	}
	a, err := d.checker.Admit(ex.rt, resp.StatusCode, resp.Header, ex.in)
	if err != nil {
		log.Printf("client: %s %s: refused the server's answer: %v", ex.in.Method, ex.rt.Pattern, err)
		return fmt.Errorf("%w: %v", errContract, err)
	}
	ex.admitted = &a
	if a.Bodiless {
		// Nothing of an untyped answer's body goes on (C1): net/http would
		// sniff it into a type after admission.
		resp.Body.Close()
		resp.Body = http.NoBody
		resp.ContentLength = 0
	}
	resp.Header = http.Header{}
	resp.Trailer = nil
	return nil
}

// fail answers locally when the server's answer didn't come through, and
// says whether the request may have reached it (LinkHeader).
func (d *Daemon) fail(w http.ResponseWriter, r *http.Request, err error) {
	ex := exchangeOf(r)
	fw, ok := w.(*finalWriter)
	if !ok || fw.wrote {
		return
	}
	fw.wrote = true
	switch {
	case sendNavigation(ex.rt, ex.in):
		// The compose form's own POST: a page that says what happened and
		// takes the user back to the draft, not a bare text answer.
		if ex.gotConn.Load() || errors.Is(err, errContract) {
			d.sendFailed(fw.w, ex.rt, Unknown)
		} else {
			d.sendFailed(fw.w, ex.rt, NotSent)
		}
	case errors.Is(err, errContract):
		writeLinkError(fw.w, ex.rt, ex.in, http.StatusBadGateway, Unknown, "the server's answer was refused")
	case pageNavigation(ex.rt, ex.in):
		// A page load that failed, after its one retry: the link went down
		// between the check and the send, or the connection under it died.
		// A GET changes nothing, so there's no outcome to explain, only
		// the link's state, and the page reloads itself once it's back.
		st := d.up.State()
		if st.Reason == link.Up {
			reason, detail := link.Classify(err)
			st = link.State{Reason: reason, Since: time.Now(), Detail: detail}
		}
		outcome := NotSent
		if ex.gotConn.Load() {
			outcome = Unknown
		}
		d.errorPage(fw.w, ex.rt, st, outcome)
	case !ex.gotConn.Load():
		reason, _ := link.Classify(err)
		writeLinkError(fw.w, ex.rt, ex.in, http.StatusBadGateway, NotSent, "Not sent: can't reach the server ("+string(reason)+").")
	default:
		if r.Context().Err() == nil {
			log.Printf("client: %s %s: %v", ex.in.Method, ex.rt.Pattern, err)
		}
		writeLinkError(fw.w, ex.rt, ex.in, http.StatusGatewayTimeout, Unknown, "The server didn't answer; the outcome is unknown.")
	}
}

// writeLinkError is a local answer through the middleware's writer (data
// class, the route's cache rule): JSON on a mutation whose route answers
// JSON, text/plain otherwise (POST /send's errors are text).
func writeLinkError(w http.ResponseWriter, rt *web.Route, r *http.Request, status int, outcome, msg string) {
	w.Header().Set(LinkHeader, outcome)
	if r.Method == http.MethodGet || r.Method == http.MethodHead || !slices.Contains(rt.Types[web.PolicyData], "application/json") {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Link  string `json:"link"`
	}{false, msg, outcome})
}

// finalWriter owns the response boundary (N5): ReverseProxy writes into a
// scratch header map; a 1xx (its own trace forwarding one) is swallowed and
// the map reset; the first status ≥ 200 installs only the admitted headers
// on the middleware's writer, which applies the class and cache rule; and
// anything written to the map after that, trailers included, lands
// nowhere.
type finalWriter struct {
	w     http.ResponseWriter // the Auth middleware's policy writer
	ex    *exchange
	hdr   http.Header
	wrote bool
	d     *Daemon // for the send page
}

var errRefused = errors.New("client: answer refused")

func (f *finalWriter) Header() http.Header { return f.hdr }

func (f *finalWriter) WriteHeader(code int) {
	if code < 200 {
		clear(f.hdr)
		return
	}
	if f.wrote {
		return
	}
	f.wrote = true
	f.hdr = http.Header{}
	a := f.ex.admitted
	if a == nil || !a.Bodiless && a.Header.Get("Content-Type") == "" || a.Install(f.w, f.ex.rt) != nil {
		// Only an admitted answer gets here; anything else is refused whole.
		f.ex.admitted = nil
		if sendNavigation(f.ex.rt, f.ex.in) {
			f.d.sendFailed(f.w, f.ex.rt, Unknown)
			return
		}
		writeLinkError(f.w, f.ex.rt, f.ex.in, http.StatusBadGateway, Unknown, "the server's answer was refused")
		return
	}
	f.w.WriteHeader(code)
}

func (f *finalWriter) Write(b []byte) (int, error) {
	if !f.wrote {
		f.WriteHeader(http.StatusOK)
	}
	if f.ex.admitted == nil {
		return 0, errRefused
	}
	if len(b) > 0 && (f.ex.admitted.Bodiless || f.w.Header().Get("Content-Type") == "") {
		// Never a body without a type: net/http would sniff one.
		return 0, errRefused
	}
	return f.w.Write(b)
}

// FlushError flushes what's written; before the header write there's
// nothing to flush (a flush must never send headers the boundary hasn't
// set).
func (f *finalWriter) FlushError() error {
	if !f.wrote {
		return nil
	}
	return http.NewResponseController(f.w).Flush()
}

// roundTripper sends a proxied request over the link with its route's
// response-header timeout, and gives the answer's body an idle timer.
type roundTripper struct{ d *Daemon }

var (
	errHeaderTimeout = errors.New("client: no answer within the route's wait")
	errIdle          = errors.New("client: the answer's body stalled")
)

// RoundTrip retries a safe request (GET or HEAD, no body) once when it
// got no answer: on a live session, that's a connection the server
// dropped (most often while this machine slept), so the session is called
// down and the retry waits for a fresh one; on a link that went to
// starting meanwhile, it waits for that. A request that timed out, or
// that the browser cancelled, isn't retried, and a mutation never is
// (docs/client.md, R10).
func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, sess, err := t.try(req)
	if err == nil || !safe(req) || req.Context().Err() != nil || errors.Is(err, errHeaderTimeout) {
		return resp, err
	}
	var down *link.DownError
	switch {
	case sess != 0:
		t.d.up.Reconnect(sess)
	case errors.As(err, &down) && down.Reason == link.Starting:
	default:
		return nil, err
	}
	if !t.d.waitUp(req.Context()) {
		return nil, err
	}
	resp, _, err = t.try(req)
	return resp, err
}

// try is one send with the route's response-header timeout.
func (t *roundTripper) try(req *http.Request) (*http.Response, link.SessionID, error) {
	ex := exchangeOf(req)
	wait := web.DefaultWait
	if ex != nil && ex.rt.Wait > 0 {
		wait = ex.rt.Wait
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(wait, func() { cancel(errHeaderTimeout) })
	resp, sess, err := t.d.up.RoundTripOn(req.WithContext(ctx))
	timer.Stop()
	if err != nil {
		if errors.Is(context.Cause(ctx), errHeaderTimeout) {
			err = fmt.Errorf("%w: %v", errHeaderTimeout, err)
		}
		cancel(nil)
		return nil, sess, err
	}
	resp.Body = &idleBody{rc: resp.Body, idle: t.d.idle, cancel: cancel}
	return resp, sess, nil
}

// safe: a request that changes nothing on the server and has no body to
// send again (net/http/httputil gives a bodiless request a nil Body).
func safe(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && (r.Body == nil || r.Body == http.NoBody)
}

// settle waits up to SettleWait for a link that's starting. Nothing has
// left this machine yet, so waiting is safe for a mutation too.
func (d *Daemon) settle(ctx context.Context) {
	if d.up.State().Reason == link.Starting {
		d.waitUp(ctx)
	}
}

// waitUp waits up to SettleWait for the link to be up.
func (d *Daemon) waitUp(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, d.settleWait)
	defer cancel()
	return d.up.WaitUp(ctx)
}

// idleBody cancels its request, which resets that one HTTP/2 stream, when
// a Read waits idle without a byte. The timer runs only while a Read is
// waiting on the server, so a slow browser isn't counted against it.
type idleBody struct {
	rc     io.ReadCloser
	idle   time.Duration
	cancel context.CancelCauseFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	t := time.AfterFunc(b.idle, func() { b.cancel(errIdle) })
	n, err := b.rc.Read(p)
	t.Stop()
	return n, err
}

func (b *idleBody) Close() error {
	err := b.rc.Close()
	b.cancel(nil)
	return err
}
