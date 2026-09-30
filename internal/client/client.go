// Package client is the client daemon's HTTP side (docs/client.md, "The
// client daemon"): pneu.localhost behind the same Auth as a server, a few
// routes answered locally, and every other route in the table proxied to
// the server over the link, with the response boundary enforced here.
package client

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jmckible/pneu/internal/web"
)

// Upstream is the link as the daemon uses it (*link.Link).
type Upstream interface {
	// RoundTrip sends a request to the server; down, it fails with a
	// *link.DownError before anything is dialed.
	http.RoundTripper
	// Retry asks for a connection attempt, or a probe, now.
	Retry()
	// WaitUp waits for the link to be up.
	WaitUp(ctx context.Context) bool
	// Email is an account's address from the server's hello.
	Email(account string) (string, bool)
}

// Config is what New needs.
type Config struct {
	Auth *web.Auth
	Link Upstream
	// ThemePath is this desk's theme file ("": web.ThemePath()).
	ThemePath string
}

// Limits (docs/client.md, "Limits and timeouts").
const (
	// IdleBody cancels one proxied response whose body has sent nothing
	// for this long: that stream only, never the shared connection.
	IdleBody = 60 * time.Second
	// launchWait bounds how long a launch waits for the link before its
	// sync is dropped.
	launchWait = 15 * time.Second
)

// Daemon serves pneu.localhost on a client.
type Daemon struct {
	auth    *web.Auth
	up      Upstream
	checker web.Checker
	theme   string
	rp      *httputil.ReverseProxy
	handler http.Handler

	idle      time.Duration
	launching atomic.Bool
}

// New wires the daemon's routes: the table's, Local ones answered here,
// the rest proxied, anything else a local 404.
func New(cfg Config) *Daemon {
	d := &Daemon{
		auth: cfg.Auth, up: cfg.Link, theme: cfg.ThemePath, idle: IdleBody,
		checker: web.Checker{Origin: cfg.Auth.Origin, Email: cfg.Link.Email},
	}
	d.rp = &httputil.ReverseProxy{
		Rewrite:        d.rewrite,
		Transport:      &roundTripper{d},
		ModifyResponse: d.modify,
		ErrorHandler:   d.fail,
		FlushInterval:  -1, // mail streams as it arrives
		ErrorLog:       log.New(logWriter{}, "", 0),
	}
	mux := web.ClientRoutes(func(rt *web.Route) http.Handler {
		if !rt.Local {
			return d.proxy(rt)
		}
		switch rt.Pattern {
		case "/open":
			return http.HandlerFunc(d.auth.Open)
		case "/theme.css":
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { web.ServeTheme(w, d.theme) })
		case "/events":
			return http.HandlerFunc(d.events)
		}
		return http.NotFoundHandler()
	})
	d.handler = d.auth.Middleware(refuse(mux))
	return d
}

func (d *Daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) { d.handler.ServeHTTP(w, r) }

// refuse turns away what never goes upstream, whatever the route: a
// service worker's script fetch (R4: a 404, so a registered worker's
// update check drops it), CONNECT, and protocol upgrades.
func refuse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Service-Worker") != "":
			http.Error(w, "no service workers", http.StatusNotFound)
		case r.Method == http.MethodConnect:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		case r.Header.Get("Upgrade") != "" || headerHas(r.Header, "Connection", "upgrade"):
			http.Error(w, "no upgrades", http.StatusBadRequest)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// headerHas reports whether any of h's key lines lists token.
func headerHas(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for f := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), token) {
				return true
			}
		}
	}
	return false
}

// events is the browser's /events. The client's fan-out of its one
// upstream stream is the next build step; until then a page here has no
// live updates.
func (d *Daemon) events(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "live updates aren't built on clients yet", http.StatusServiceUnavailable)
}

// Launch is the control socket's `launch` (docs/client.md, R13): retry the
// link now, and once it's up ask the server to sync, without holding up
// the window. Launches while one waits collapse into it.
func (d *Daemon) Launch() {
	d.up.Retry()
	if !d.launching.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.launching.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), launchWait)
		defer cancel()
		if !d.up.WaitUp(ctx) {
			log.Printf("client: launch: the link isn't up; no sync")
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://server/sync?reason=launch", nil)
		resp, err := d.up.RoundTrip(req)
		if err != nil {
			log.Printf("client: launch sync: %v", err)
			return
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("client: launch sync: HTTP %d", resp.StatusCode)
		}
	}()
}

// logWriter sends ReverseProxy's own messages to the log under a prefix.
type logWriter struct{}

func (logWriter) Write(b []byte) (int, error) {
	log.Printf("client: proxy: %s", strings.TrimSpace(string(b)))
	return len(b), nil
}
