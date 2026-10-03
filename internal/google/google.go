// Package google is pneu's one boundary with Google for push sync
// (docs/push.md D6): OAuth for the push project's own client, the three
// Gmail calls a mailbox token makes (getProfile, watch, stop), and the
// Pub/Sub calls the owner's token makes (topic, IAM policy, subscription,
// pull, acknowledge).
//
// Everything goes through one http.Client that reaches three fixed hosts
// and nothing else: no redirects, no proxy from the environment, normal
// certificate checks. Answers are capped at MaxBody, read into fixed
// shapes, and every failure becomes an *Error naming only the operation
// and a local Code. Nothing Google wrote is ever returned as text, so
// nothing it wrote can reach a log, a page, a command or status.json.
// The package never logs. Every call takes a context.
package google

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/jmckible/pneu/internal/google/internal/testhook"
)

// The three hosts pneu calls, and the consent screen's URL, which is only
// ever handed to a browser.
const (
	TokenHost  = "oauth2.googleapis.com"
	GmailHost  = "gmail.googleapis.com"
	PubSubHost = "pubsub.googleapis.com"
	AuthURL    = "https://accounts.google.com/o/oauth2/v2/auth"
)

// MaxBody caps every answer Google sends, error bodies included.
const MaxBody = 64 << 10

// Default per-call bounds: Timeout for every call but a pull, PullTimeout
// for a pull, which Google holds open until a message or its own limit.
const (
	DefaultTimeout     = 30 * time.Second
	DefaultPullTimeout = 90 * time.Second
)

// Options tune an API. Zero values take the defaults.
type Options struct {
	// Now is the clock for token expiry and ID-token checks; default
	// time.Now. Tests pass a fake clock's.
	Now         func() time.Time
	Timeout     time.Duration // default DefaultTimeout
	PullTimeout time.Duration // default DefaultPullTimeout
}

// API calls Google. Safe for concurrent use.
type API struct {
	hc    *http.Client
	tr    *http.Transport
	bases map[string]string // host -> scheme://host; only testhook changes it
	now   func() time.Time
	opts  Options
}

// New makes an API with its own transport.
func New(opts Options) *API {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.PullTimeout <= 0 {
		opts.PullTimeout = DefaultPullTimeout
	}
	tr := &http.Transport{
		Proxy:               nil, // never from the environment (C10)
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 4,
	}
	a := &API{
		tr: tr,
		hc: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a 3xx is an answer we don't accept, never followed
		}},
		bases: map[string]string{},
		now:   opts.Now,
		opts:  opts,
	}
	for _, h := range []string{TokenHost, GmailHost, PubSubHost} {
		a.bases[h] = "https://" + h
	}
	return a
}

func init() {
	testhook.WithBase = func(api any, base string) {
		a := api.(*API)
		for h := range a.bases {
			a.bases[h] = base + "/" + h
		}
	}
}

// CloseIdle drops pooled connections (after a wake: they died in suspend).
func (a *API) CloseIdle() { a.tr.CloseIdleConnections() }

// Now is the API's clock.
func (a *API) Now() time.Time { return a.now() }

// request is one call.
type request struct {
	op      Op
	host    string
	method  string
	path    string     // starts with /; built only from validated parts
	query   url.Values // nil: none
	bearer  string     // "": no Authorization
	form    url.Values // a form body (the token endpoint)
	json    []byte     // a JSON body
	timeout time.Duration
}

// call runs r and returns a 2xx answer's body. A context that ended is
// returned as its own error; everything else as an *Error.
func (a *API) call(ctx context.Context, r request) ([]byte, error) {
	base, ok := a.bases[r.host]
	if !ok {
		panic("google: host " + r.host + " isn't one of the three")
	}
	if r.timeout <= 0 {
		r.timeout = a.opts.Timeout
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	u := base + r.path
	if r.query != nil {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	ctype := ""
	switch {
	case r.form != nil:
		body, ctype = bytes.NewReader([]byte(r.form.Encode())), "application/x-www-form-urlencoded"
	case r.json != nil:
		body, ctype = bytes.NewReader(r.json), "application/json"
	}
	req, err := http.NewRequestWithContext(cctx, r.method, u, body)
	if err != nil {
		return nil, fail(r.op, CodeUnknown)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pneu")
	resp, err := a.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail(r.op, CodeNetwork)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail(r.op, CodeNetwork)
	}
	if len(b) > MaxBody {
		return nil, fail(r.op, CodeUnknown)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fail(r.op, classify(resp.StatusCode, b, r.host == TokenHost))
	}
	return b, nil
}

// errShape marks an answer pneu wouldn't accept; callers turn it into
// fail(op, CodeUnknown).
var errShape = errors.New("google: answer out of shape")
