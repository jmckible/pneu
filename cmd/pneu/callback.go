package main

// The consent callback listener, shared by the client relay (relay.go)
// and push's own consent on the server (push.go; docs/push.md D3). Google
// sends the browser to http://localhost:8080/ with the consent's answer;
// whichever command is waiting binds 127.0.0.1:8080 and [::1]:8080 and
// takes one well-formed callback for its consent.
//
// Any page in this machine's browser can reach the listener while it's
// up. It takes only GET / on a loopback Host:port, a top-level navigation
// when the browser says (Sec-Fetch-*), a query of Google's callback keys
// only (remote.ParseCallback) whose state is this consent's, compared in
// constant time: a page that doesn't know the state gets a 400 and the
// listener keeps waiting, so it can't use up the one-shot. Each request
// is bounded in time and size, and so is how many connections it holds.
// The answer is fixed text; nothing from the request is reflected.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/remote"
)

// bindPolicy is how a callback listener binds its port.
type bindPolicy int

const (
	// bindReuse is Go's default, SO_REUSEADDR: a TIME_WAIT from a recent
	// consent doesn't refuse. The client relay's: nothing else on the
	// client binds the port the way lieer does.
	bindReuse bindPolicy = iota
	// bindExclusive is lieer's bind, no SO_REUSEADDR (gmi.CheckAuthPort's
	// rule), so it fails exactly when lieer's would: a consent waiting on
	// the port, or a connection to it still closing. The server's push
	// consent binds this way and holds the port until the callback, so it
	// and a lieer consent can't both think they own Google's redirect.
	bindExclusive
)

// errPortBusy: the port is taken on one of the loopbacks (or, with
// bindExclusive, a connection to it is still closing).
var errPortBusy = errors.New("the consent port is in use or still closing")

// listenLoopback binds port on both loopbacks, both required: the browser
// resolves localhost to either. A port in use is errPortBusy, wrapped
// with the address that refused; any other failure (no IPv6 loopback)
// is that failure.
func listenLoopback(port int, policy bindPolicy) ([]net.Listener, error) {
	lc := net.ListenConfig{}
	if policy == bindExclusive {
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
			}); err != nil {
				return err
			}
			return serr
		}
	}
	var lns []net.Listener
	for _, host := range []string{"127.0.0.1", "::1"} {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			if errors.Is(err, syscall.EADDRINUSE) {
				return nil, fmt.Errorf("%s: %w", addr, errPortBusy)
			}
			return nil, fmt.Errorf("%s can't be bound: %w", addr, err)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// The listener's bounds: a whole request (headers and body) and its
// answer in relayTimeout each, at most relayConns connections at once.
var (
	relayTimeout = 5 * time.Second
	relayConns   = 16
)

// limitListener holds at most cap(sem) connections; one past that is
// closed as soon as it's accepted.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
			c.Close()
		}
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// callbackServer serves one consent's callback.
type callbackServer struct {
	srv     *http.Server
	hosts   map[string]bool
	done    string      // the fixed answer to the accepted callback
	got     chan string // the one accepted callback's raw query
	armed   chan struct{}
	armOnce sync.Once
	mu      sync.Mutex
	state   string // this consent's, once known
	taken   bool
	once    sync.Once
}

// The listener's fixed refusal: no request value is ever echoed.
const relayRefused = "pneu: not a consent callback this command is waiting for.\n"

// startCallback serves lns until close; done is the accepted callback's
// fixed answer.
func startCallback(lns []net.Listener, done string) *callbackServer {
	cb := &callbackServer{hosts: map[string]bool{}, done: done, got: make(chan string, 1), armed: make(chan struct{})}
	for _, ln := range lns {
		if a, ok := ln.Addr().(*net.TCPAddr); ok {
			p := strconv.Itoa(a.Port)
			for _, h := range []string{"localhost", "127.0.0.1", "[::1]"} {
				cb.hosts[h+":"+p] = true
			}
		}
	}
	cb.srv = &http.Server{Handler: cb, ReadHeaderTimeout: relayTimeout, ReadTimeout: relayTimeout,
		WriteTimeout: relayTimeout, MaxHeaderBytes: 8 << 10}
	cb.srv.SetKeepAlivesEnabled(false)     // one answer per connection; close doesn't wait on idle ones
	sem := make(chan struct{}, relayConns) // shared by both families
	for _, ln := range lns {
		go cb.srv.Serve(&limitListener{Listener: ln, sem: sem})
	}
	return cb
}

// arm sets the state a callback must carry; until then, none is taken.
func (cb *callbackServer) arm(state string) {
	cb.mu.Lock()
	cb.state = state
	cb.mu.Unlock()
	cb.armOnce.Do(func() { close(cb.armed) })
}

// close stops listening, letting a callback being answered finish.
func (cb *callbackServer) close() {
	cb.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if cb.srv.Shutdown(ctx) != nil {
			cb.srv.Close()
		}
	})
}

func (cb *callbackServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	if !cb.accept(r) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, relayRefused)
		return
	}
	fmt.Fprint(w, cb.done)
}

// accept reports whether r is this consent's callback, and if so takes it:
// once.
func (cb *callbackServer) accept(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/" || r.URL.RawPath != "" || !cb.hosts[r.Host] {
		return false
	}
	// Google's redirect is a top-level navigation; a browser that labels
	// requests must label it so.
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" && m != "navigate" {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
		return false
	}
	q, err := remote.ParseCallback(r.URL.RawQuery)
	if err != nil {
		return false
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.taken || cb.state == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(cb.state)) != 1 {
		return false
	}
	cb.taken = true
	cb.got <- r.URL.RawQuery
	return true
}
