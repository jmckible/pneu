package main

// The consent relay (docs/client.md, "As built: step 8", Y1). lieer on the
// server waits for Google's redirect on its own localhost:8080, but the
// browser is here. No SSH forward carries it: `pneu account auth` on a
// client listens on this machine's 127.0.0.1:8080 and [::1]:8080 itself,
// takes one well-formed callback for this consent, and hands its query to
// the session, which sends it to the server as one line on stdin.
//
// Any page in this machine's browser can reach the listener while it's
// up. It takes only GET / on a loopback Host:port, a top-level navigation
// when the browser says (Sec-Fetch-*), a query of Google's callback keys
// only (remote.ParseCallback) whose state is this consent's: a page that
// doesn't know the state gets a 400 and the relay keeps waiting, so it
// can't use up the one-shot. The answer is fixed text; nothing from the
// request is reflected.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/remote"
)

// listenRelay binds port on both loopbacks, both required: the browser
// resolves localhost to either. A Go listener sets SO_REUSEADDR, so a
// TIME_WAIT from a recent consent doesn't refuse.
func listenRelay(port int) ([]net.Listener, error) {
	var lns []net.Listener
	for _, host := range []string{"127.0.0.1", "::1"} {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return nil, fmt.Errorf("%s is in use here (or can't be bound): Google's consent comes back to it, so pneu account auth needs it free. `ss -ltnp 'sport = :%d'` shows what holds it", addr, port)
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// The relay's bounds: a whole request (headers and body) and its answer
// in relayTimeout each, at most relayConns connections at once.
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

// relay serves one consent's callback.
type relay struct {
	srv     *http.Server
	hosts   map[string]bool
	got     chan string // the one accepted callback's raw query
	armed   chan struct{}
	armOnce sync.Once
	mu      sync.Mutex
	state   string // this consent's, once its URL has arrived
	done    bool
	once    sync.Once
}

// The relay's fixed answers: no request value is ever echoed.
const (
	relayDone    = "pneu: Google's answer is on its way to the server. You can close this tab; the terminal shows the result.\n"
	relayRefused = "pneu: not a consent callback this command is waiting for.\n"
)

func startRelay(lns []net.Listener) *relay {
	rl := &relay{hosts: map[string]bool{}, got: make(chan string, 1), armed: make(chan struct{})}
	for _, ln := range lns {
		if a, ok := ln.Addr().(*net.TCPAddr); ok {
			p := strconv.Itoa(a.Port)
			for _, h := range []string{"localhost", "127.0.0.1", "[::1]"} {
				rl.hosts[h+":"+p] = true
			}
		}
	}
	// Any page here can open connections to it: each request is bounded in
	// time and size, and so is how many connections it holds (Z3).
	rl.srv = &http.Server{Handler: rl, ReadHeaderTimeout: relayTimeout, ReadTimeout: relayTimeout,
		WriteTimeout: relayTimeout, MaxHeaderBytes: 8 << 10}
	rl.srv.SetKeepAlivesEnabled(false)     // one answer per connection; close doesn't wait on idle ones
	sem := make(chan struct{}, relayConns) // shared by both families
	for _, ln := range lns {
		go rl.srv.Serve(&limitListener{Listener: ln, sem: sem})
	}
	return rl
}

// arm sets the state a callback must carry; until then, none is taken.
func (rl *relay) arm(state string) {
	rl.mu.Lock()
	rl.state = state
	rl.mu.Unlock()
	rl.armOnce.Do(func() { close(rl.armed) })
}

// close stops listening, letting a callback being answered finish.
func (rl *relay) close() {
	rl.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if rl.srv.Shutdown(ctx) != nil {
			rl.srv.Close()
		}
	})
}

func (rl *relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	if !rl.accept(r) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, relayRefused)
		return
	}
	fmt.Fprint(w, relayDone)
}

// accept reports whether r is this consent's callback, and if so takes it:
// once.
func (rl *relay) accept(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/" || r.URL.RawPath != "" || !rl.hosts[r.Host] {
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
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.done || rl.state == "" || q.Get("state") != rl.state {
		return false
	}
	rl.done = true
	rl.got <- r.URL.RawQuery
	return true
}
