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
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/wake"
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
	// State is the link's state; Hello the last connect's hello (nil
	// before one), whose account set bounds every event.
	State() link.State
	Hello() *link.Hello
	// RoundTripOn is RoundTrip naming the session that carried it;
	// Stalled calls that session down, if it's still the live one: the
	// event stream on it went quiet.
	RoundTripOn(req *http.Request) (*http.Response, link.SessionID, error)
	Stalled(link.SessionID)
	// Reconnect calls that session down as starting, if it's still the
	// live one: a request on it got no answer.
	Reconnect(link.SessionID)
	// CheckWake: if this machine slept since the last look, the session
	// goes and an attempt starts (internal/wake).
	CheckWake()
}

// Config is what New needs.
type Config struct {
	Auth *web.Auth
	Link Upstream
	// Server is the SSH target the user paired with: the server's name in
	// every page, the error page and status.json (never hello's name).
	Server string
	// ThemePath is this desk's theme file ("": web.ThemePath()).
	ThemePath string
	// StatusPath is this desk's status.json ("": none written).
	StatusPath string
	// SkewPath is pneu update's skew.json ("": no ancestry, so any
	// difference is "different").
	SkewPath string
}

// Limits (docs/client.md, "Limits and timeouts").
const (
	// IdleBody cancels one proxied response whose body has sent nothing
	// for this long: that stream only, never the shared connection.
	IdleBody = 60 * time.Second
	// launchWait bounds how long a launch waits for the link before its
	// sync is dropped.
	launchWait = 15 * time.Second
	// SettleWait is how long a request waits for a link that's starting
	// (just started, just woken, or reconnecting after a dropped
	// connection) before it's answered as down: a reconnect on the
	// tailnet takes well under it.
	SettleWait = 5 * time.Second
)

// Daemon serves pneu.localhost on a client.
type Daemon struct {
	auth    *web.Auth
	up      Upstream
	server  string
	checker web.Checker
	theme   string
	rp      *httputil.ReverseProxy
	handler http.Handler

	idle       time.Duration
	settleWait time.Duration
	launching  atomic.Bool

	// The live side (live.go): mu spans every change to live and its
	// broadcast on hub, and each browser's subscription with its hello.
	mu   sync.Mutex
	live live
	hub  *web.Hub

	statusPath string
	statusWake chan struct{}
	skewPath   string
	// self is this binary's build (control.Self; tests play others).
	self func() control.Info
	// Timing and the stream's budget, changed by tests before Run.
	silence, statusGap, themePoll time.Duration
	streamMin, streamMax, healthy time.Duration
	budget                        Budget
	onStatusWrite                 func() // tests only
	// Rate-limited logs: dropped upstream events, and streams ending.
	dropLog, streamLog logLimit
}

// New wires the daemon's routes: the table's, Local ones answered here,
// the rest proxied, anything else a local 404. The link's OnChange must
// call LinkChanged.
func New(cfg Config) *Daemon {
	d := &Daemon{
		auth: cfg.Auth, up: cfg.Link, server: cfg.Server, theme: cfg.ThemePath, idle: IdleBody, settleWait: SettleWait,
		checker:    web.Checker{Origin: cfg.Auth.Origin, Email: cfg.Link.Email},
		hub:        web.NewHub(),
		statusPath: cfg.StatusPath,
		statusWake: make(chan struct{}, 1),
		skewPath:   cfg.SkewPath,
		self:       control.Self,
		silence:    Silence, statusGap: StatusGap, themePoll: web.ThemePoll,
		streamMin: StreamMin, streamMax: StreamMax, healthy: Healthy, budget: DefaultBudget,
		dropLog: logLimit{every: 10 * time.Second}, streamLog: logLimit{every: 10 * time.Second},
	}
	d.live.link = cfg.Link.State()
	d.live.skew = d.loadSkew()
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
		case "/client/link":
			return http.HandlerFunc(d.linkJSON)
		case "/client/retry":
			return http.HandlerFunc(d.retry)
		case "/client/static/":
			return http.HandlerFunc(d.static)
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

// Run is the daemon's live side until ctx ends: the one upstream event
// stream, this desk's theme watcher and status.json. It returns once the
// last status write (running false) is done and every page's stream is
// closed.
func (d *Daemon) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { d.runUpstream(ctx) })
	wg.Go(func() { web.WatchTheme(ctx, d.theme, d.themePoll, d.themeChanged) })
	if d.statusPath != "" {
		wg.Go(func() { d.runStatus(ctx) })
		// The heartbeat's timer stopped while asleep, and status.json's
		// updated with it: the widget would call it stale until the next
		// tick. The link's own wake check rewrites it only when the state
		// changes.
		wg.Go(func() { wake.New().Watch(ctx.Done(), wake.Every, func(time.Duration) { d.wakeStatus() }) })
	}
	wg.Wait()
	d.hub.Close()
}

// Launch is the control socket's `launch` (docs/client.md, R13): retry the
// link now, and once it's up ask the server to sync, without holding up
// the window. Launches while one waits collapse into it.
func (d *Daemon) Launch() {
	d.up.CheckWake()
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
