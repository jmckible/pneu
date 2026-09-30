package peer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/tailscale"
)

// The link's timing (docs/client.md, "Tailscale as the second check").
const (
	// Lease is how long a whois check vouches for a connection: the bound
	// on stale access after a node is tagged, shared or re-owned.
	Lease = 60 * time.Second
	// RenewBelow: the sweep renews every connection with less than this left.
	RenewBelow = 20 * time.Second
	// Sweep is the renewal sweep's period.
	Sweep = 10 * time.Second
	// Reconcile is how often the listeners follow tailscaled's addresses.
	Reconcile = 10 * time.Second
	// CloseWait bounds how long a reload waits for a removed peer's
	// connections to close, inside control.ReloadTimeout.
	CloseWait = 4 * time.Second
)

var errPeer = errors.New("peer: not a paired peer")

// Config is what New needs.
type Config struct {
	API      tailscale.API
	Port     int // the peer port; every tailnet address is bound on it
	Identity Identity
	Store    Store
	// Handler builds what serves admitted requests: web's PeerHandler,
	// given this Server to ask (Identify, HostOK) on every request.
	Handler func(*Server) http.Handler
}

// Server is the peer listener: one TLS listener per tailnet address of
// this node, and the registry of live peer connections.
type Server struct {
	api   tailscale.API
	port  int
	id    Identity
	store Store
	hs    *http.Server

	// Timing, the package constants outside tests.
	lease, renewBelow, sweep, reconcile, closeWait time.Duration
	// listen binds an address; net.Listen outside tests.
	listen func(network, addr string) (net.Listener, error)
	// beforeRegister runs between a handshake's whois and its
	// registration; tests only (the remove-versus-handshake race).
	beforeRegister func()

	// mu orders the live generation, the connection registry, the
	// listeners and the node's own identity. A connection is registered
	// under it only if its peer is in the live generation, and a reload
	// swaps the generation and collects that peer's connections under it,
	// so no handshake lands between a removal's check and its close (R2, R8).
	mu     sync.Mutex
	peers  File
	self   *selfInfo // nil unless tailscaled is Running
	lns    map[netip.Addr]*boundListener
	conns  map[*conn]struct{}
	closed bool

	stop chan struct{}
	wg   sync.WaitGroup
}

type selfInfo struct {
	node string
	user int64
}

// boundListener is one address's listener. Its Accept yields only
// connections that finished the handshake and are registered.
type boundListener struct {
	net.Listener
	s        *Server
	addr     netip.Addr
	host     string // ln.Addr() as a Host header names it
	ready    chan net.Conn
	done     chan struct{}
	closeOne sync.Once
}

// New is a stopped Server with no peers; Start loads and serves.
func New(cfg Config) *Server {
	s := &Server{
		api: cfg.API, port: cfg.Port, id: cfg.Identity, store: cfg.Store,
		lease: Lease, renewBelow: RenewBelow, sweep: Sweep, reconcile: Reconcile, closeWait: CloseWait,
		listen: net.Listen,
		peers:  File{Peers: []Record{}},
		lns:    map[netip.Addr]*boundListener{},
		conns:  map[*conn]struct{}{},
		stop:   make(chan struct{}),
	}
	protos := new(http.Protocols)
	protos.SetHTTP2(true) // and only HTTP/2: the client's transport speaks nothing else (R17)
	s.hs = &http.Server{
		Handler:           s.track(cfg.Handler(s)),
		Protocols:         protos,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /events streams; the lease bounds a connection.
		ConnContext: func(ctx context.Context, nc net.Conn) context.Context {
			if c := asConn(nc); c != nil {
				return context.WithValue(ctx, connKey{}, c)
			}
			return ctx
		},
		ConnState: func(nc net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				if c := asConn(nc); c != nil {
					c.markServed()
				}
			}
		},
		ErrorLog: log.New(logWriter{}, "", 0),
	}
	return s
}

// logWriter sends net/http's own messages (a failed handshake) to the log
// under the peer prefix.
type logWriter struct{}

func (logWriter) Write(b []byte) (int, error) {
	log.Printf("peer: %s", b)
	return len(b), nil
}

// Start loads the current generation and starts following tailscaled. The
// listeners come up on the first reconcile, in the background.
func (s *Server) Start() {
	f, err := s.store.Load()
	if err != nil {
		// No peers is the closed failure; the next good reload fixes it.
		log.Printf("peer: %v; no peers admitted", err)
	} else if _, err := s.apply(f); err != nil {
		log.Printf("peer: %v", err)
	}
	s.wg.Go(s.reconcileLoop)
	s.wg.Go(s.sweepLoop)
}

// Close stops listening and closes every peer connection.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for a, bl := range s.lns {
		bl.Close()
		delete(s.lns, a)
	}
	drop := s.revokeLocked(func(*conn) bool { return true })
	s.mu.Unlock()
	close(s.stop)
	for _, c := range drop {
		c.Close()
	}
	s.wg.Wait()
	return s.hs.Close()
}

// Reload is the control socket's peers-reload: load peers.json, require it
// to be exactly generation gen with hash hash, make it live, and return
// once every connection of a peer it doesn't pair is finished: closed, and
// every handler admitted on it returned. That includes connections that
// closed before this reload (a client that sent a /tag and hung up: tag
// writes and sends outlive their request's cancellation), and a reload
// after one that timed out waits for the same set again. The caller (pneu
// peer add|remove) holds peers.lock, so the file can't move under it.
func (s *Server) Reload(gen uint64, hash string) error {
	f, err := s.store.Load()
	if err != nil {
		return err
	}
	if f.Generation != gen || f.Hash != hash {
		return fmt.Errorf("peers.json is at generation %d, not %d", f.Generation, gen)
	}
	unpaired, err := s.apply(f)
	if err != nil {
		return err
	}
	return s.awaitFinished(unpaired)
}

// apply makes f live, never going back a generation, closes the
// connections of every peer it no longer pairs, and returns every
// unfinished connection of those peers, closed ones included.
func (s *Server) apply(f File) ([]*conn, error) {
	s.mu.Lock()
	if f.Generation < s.peers.Generation || f.Generation == s.peers.Generation && f.Hash != s.peers.Hash {
		live := s.peers.Generation
		s.mu.Unlock()
		return nil, fmt.Errorf("peers.json generation %d is behind the live %d", f.Generation, live)
	}
	s.peers = f
	gone := func(c *conn) bool {
		r := f.Lookup(c.spki)
		return r == nil || !sameRecord(*r, c.rec)
	}
	drop := s.revokeLocked(gone)
	var unpaired []*conn
	for c := range s.conns {
		if gone(c) {
			unpaired = append(unpaired, c)
		}
	}
	s.mu.Unlock()
	for _, c := range drop {
		log.Printf("peer %s: unpaired; closing its connection from %s", c.rec.Name, c.remote.Addr())
		c.Close()
	}
	return unpaired, nil
}

// revokeLocked marks every registered connection matching drop as revoked
// (no request passes Identify from here on) and returns them to close
// outside the lock.
func (s *Server) revokeLocked(drop func(*conn) bool) []*conn {
	var out []*conn
	for c := range s.conns {
		if !c.revoked && drop(c) {
			c.revoked = true
			out = append(out, c)
		}
	}
	return out
}

// awaitFinished waits until each connection is finished (retire), bounded
// by closeWait. A timeout is an error, never an acknowledgment; the
// connections stay registered, so the next reload waits for them again.
func (s *Server) awaitFinished(cs []*conn) error {
	deadline := time.After(s.closeWait)
	for _, c := range cs {
		select {
		case <-c.finished:
		case <-deadline:
			return fmt.Errorf("peer %s: a connection or request still running after %v", c.rec.Name, s.closeWait)
		}
	}
	return nil
}

func sameRecord(a, b Record) bool {
	return a.Name == b.Name && a.Node == b.Node && a.SPKI == b.SPKI && a.Origin == b.Origin && a.Added.Equal(b.Added)
}

// ---- listeners ------------------------------------------------------------

func (s *Server) reconcileLoop() {
	t := time.NewTicker(s.reconcile)
	defer t.Stop()
	var last string
	for {
		if msg := s.reconcileOnce(); msg != last {
			if msg != "" {
				log.Print(msg)
			}
			last = msg
		}
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
	}
}

// reconcileOnce binds this node's tailnet addresses that aren't bound,
// closes listeners (and connections) on addresses it no longer has, and
// closes everything when tailscaled isn't Running (R17). It returns a
// problem worth logging once, "" when all is well.
func (s *Server) reconcileOnce() string {
	ctx, cancel := context.WithTimeout(context.Background(), tailscale.CallTimeout)
	st, err := s.api.Status(ctx)
	cancel()
	want := map[netip.Addr]bool{}
	var problem string
	switch {
	case err != nil:
		problem = fmt.Sprintf("peer: tailscale unavailable (%v); not listening", err)
	case st.BackendState != tailscale.Running:
		problem = fmt.Sprintf("peer: tailscale is %s; not listening", st.BackendState)
	case st.Self.StableID == "" || st.Self.UserID == 0:
		problem = "peer: tailscale status names no node or user; not listening"
	default:
		for _, a := range st.Self.TailscaleIPs {
			a = a.Unmap()
			// Never a wildcard: only this node's own addresses.
			if a.IsValid() && !a.IsUnspecified() && !a.IsMulticast() {
				want[a] = true
			}
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ""
	}
	if problem == "" {
		s.self = &selfInfo{node: st.Self.StableID, user: st.Self.UserID}
	} else {
		s.self = nil
	}
	for a, bl := range s.lns {
		if !want[a] {
			bl.Close()
			delete(s.lns, a)
			log.Printf("peer: stopped listening on %s", bl.host)
		}
	}
	drop := s.revokeLocked(func(c *conn) bool { return !want[c.local] })
	var bind []netip.Addr
	for a := range want {
		if s.lns[a] == nil {
			bind = append(bind, a)
		}
	}
	s.mu.Unlock()
	for _, c := range drop {
		log.Printf("peer %s: address %s gone; closing its connection", c.rec.Name, c.local)
		c.Close()
	}

	for _, a := range bind {
		ln, err := s.listen("tcp", net.JoinHostPort(a.String(), strconv.Itoa(s.port)))
		if err != nil {
			// A fresh address may not be assigned yet; the next pass retries.
			if problem == "" {
				problem = fmt.Sprintf("peer: %v; retrying", err)
			}
			continue
		}
		bl := &boundListener{Listener: ln, s: s, addr: a, host: ln.Addr().String(), ready: make(chan net.Conn), done: make(chan struct{})}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			ln.Close()
			continue
		}
		s.lns[a] = bl
		s.mu.Unlock()
		log.Printf("peer: listening on %s", bl.host)
		s.wg.Go(bl.run)
		s.wg.Go(func() { s.hs.Serve(bl) })
	}
	return problem
}

// run accepts connections and hands each to its own handshake, so a slow
// or silent client never holds up the next; at most maxHandshakes run at
// once.
func (bl *boundListener) run() {
	sem := make(chan struct{}, maxHandshakes)
	for {
		nc, err := bl.Listener.Accept()
		if err != nil {
			select {
			case <-bl.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("peer: accept on %s: %v", bl.host, err)
			time.Sleep(100 * time.Millisecond) // EMFILE and the like
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-bl.done:
			nc.Close()
			return
		}
		go func() {
			defer func() { <-sem }()
			bl.handshake(nc)
		}()
	}
}

// maxHandshakes bounds concurrent handshakes per listener; handshakeTimeout
// each one.
const (
	maxHandshakes    = 32
	handshakeTimeout = 10 * time.Second
)

// handshake completes TLS on nc and registers the connection, then offers
// it to net/http. Registration waits for the complete handshake: Go runs
// VerifyConnection before the client's CertificateVerify, so only now has
// the client proved it holds the pinned key.
func (bl *boundListener) handshake(nc net.Conn) {
	s := bl.s
	c := &conn{Conn: nc, s: s, local: bl.addr, finished: make(chan struct{})}
	if ap, err := netip.ParseAddrPort(nc.RemoteAddr().String()); err == nil {
		c.remote = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	tc := tls.Server(c, s.tlsConfig(c))
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	err := tc.HandshakeContext(ctx)
	cancel()
	if err == nil {
		err = s.register(c)
	}
	if err != nil {
		if !errors.Is(err, errPeer) { // admit and register log their own refusals
			log.Printf("peer: handshake with %s: %v", c.remote.Addr(), err)
		}
		tc.Close()
		return
	}
	select {
	case bl.ready <- tc:
	case <-bl.done:
		tc.Close()
		c.markServed() // net/http never saw it; nothing else will
	}
}

// Accept hands net/http connections whose handshake is done and whose peer
// is registered.
func (bl *boundListener) Accept() (net.Conn, error) {
	select {
	case c := <-bl.ready:
		return c, nil
	case <-bl.done:
		return nil, net.ErrClosed
	}
}

// Close stops the listener; handshakes in flight end unregistered.
func (bl *boundListener) Close() error {
	var err error
	bl.closeOne.Do(func() {
		close(bl.done)
		err = bl.Listener.Close()
	})
	return err
}

// tlsConfig is one connection's: mutual TLS 1.3, HTTP/2 only, no session
// tickets (so every connection is a full handshake), and the pin and whois
// in VerifyConnection, which runs on every handshake, resumed or not (R2).
func (s *Server) tlsConfig(c *conn) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{s.id.Cert},
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true,
		MinVersion:             tls.VersionTLS13,
		NextProtos:             []string{"h2"},
		VerifyConnection:       func(cs tls.ConnectionState) error { return s.admit(c, cs) },
	}
}

// admit checks a handshake's certificate against the live generation and
// asks whois about the remote address, noting what it vouched for on c for
// register.
func (s *Server) admit(c *conn, cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) != 1 {
		return errPeer
	}
	spki := SPKI(cs.PeerCertificates[0])
	s.mu.Lock()
	var rec Record
	r := s.peers.Lookup(spki)
	if r != nil {
		rec = *r
	}
	self := s.self
	s.mu.Unlock()
	if r == nil {
		log.Printf("peer: refused %s: unpaired key", c.remote.Addr())
		return errPeer
	}
	if self == nil {
		log.Printf("peer %s: refused %s: tailscale isn't running", rec.Name, c.remote.Addr())
		return errPeer
	}
	if err := s.whois(c.remote, rec.Node, self.user); err != nil {
		log.Printf("peer %s: refused %s: %v", rec.Name, c.remote.Addr(), err)
		return errPeer
	}
	// The lease runs from this check, not from registration: a client
	// holding the key can stall the rest of its handshake.
	c.rec, c.spki, c.user = rec, spki, self.user // not yet shared: only this handshake has c
	c.expiry = time.Now().Add(s.lease)
	return nil
}

// register makes a handshaken connection live, with the peer checked again
// under mu, which a reload also holds while it collects a removed peer's
// connections: a removal that landed since admit wins (R2, R8).
func (s *Server) register(c *conn) error {
	if s.beforeRegister != nil {
		s.beforeRegister()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.peers.Lookup(c.spki)
	switch {
	case c.spki == "", s.closed, r == nil, !sameRecord(*r, c.rec), s.self == nil, s.self.user != c.user, s.lns[c.local] == nil:
		log.Printf("peer %s: refused %s: unpaired or unlistened during its handshake", c.rec.Name, c.remote.Addr())
		return errPeer
	}
	left := time.Until(c.expiry)
	if left <= 0 {
		log.Printf("peer %s: refused %s: its whois lease ran out during the handshake", c.rec.Name, c.remote.Addr())
		return errPeer
	}
	c.timer = time.AfterFunc(left, func() { s.expire(c) })
	c.registered = true
	s.conns[c] = struct{}{}
	log.Printf("peer %s: connected from %s", c.rec.Name, c.remote.Addr())
	return nil
}

// whois applies the predicate (R5) to whatever tailscaled says is at addr.
func (s *Server) whois(addr netip.AddrPort, node string, user int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), tailscale.CallTimeout)
	defer cancel()
	w, err := s.api.WhoIs(ctx, addr)
	if err != nil {
		return fmt.Errorf("whois: %w", err)
	}
	return CheckWhoIs(w, addr.Addr(), node, user)
}

// errRefused wraps a whois answer that fails the predicate, as opposed to
// no answer.
var errRefused = errors.New("whois refuses")

// CheckWhoIs is the predicate, exactly (docs/client.md, R5): addr is one of
// the node's own addresses, the node is the paired one, untagged, not
// shared in, and its user is this node's user.
func CheckWhoIs(w tailscale.WhoIs, addr netip.Addr, node string, user int64) error {
	addr = addr.Unmap()
	own := false
	for _, p := range w.Node.Addresses {
		if p.IsSingleIP() && p.Addr().Unmap() == addr {
			own = true
		}
	}
	switch {
	case !own:
		return fmt.Errorf("%w: %s isn't one of the node's own addresses", errRefused, addr)
	case w.Node.StableID != node:
		return fmt.Errorf("%w: node %s, paired with %s", errRefused, w.Node.StableID, node)
	case len(w.Node.Tags) != 0:
		return fmt.Errorf("%w: node %s is tagged", errRefused, w.Node.StableID)
	case w.Node.Sharer != 0:
		return fmt.Errorf("%w: node %s is shared in", errRefused, w.Node.StableID)
	case w.UserProfile.ID == 0 || w.UserProfile.ID != user:
		return fmt.Errorf("%w: node %s belongs to user %d, not %d", errRefused, w.Node.StableID, w.UserProfile.ID, user)
	}
	return nil
}

// ---- leases ---------------------------------------------------------------

// expire closes c when its lease has run out without a renewal.
func (s *Server) expire(c *conn) {
	s.mu.Lock()
	if !c.registered || c.revoked {
		s.mu.Unlock()
		return
	}
	if left := time.Until(c.expiry); left > 0 {
		c.timer.Reset(left) // renewed meanwhile
		s.mu.Unlock()
		return
	}
	c.revoked = true
	s.mu.Unlock()
	log.Printf("peer %s: lease expired; closing its connection from %s", c.rec.Name, c.remote.Addr())
	c.Close()
}

func (s *Server) sweepLoop() {
	t := time.NewTicker(s.sweep)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sweepOnce()
		}
	}
}

// sweepOnce renews every connection with less than renewBelow left,
// whatever it carries (R5, N9). A whois that refuses closes at once; one
// that fails leaves the lease to run out.
func (s *Server) sweepOnce() {
	s.mu.Lock()
	var due []*conn
	for c := range s.conns {
		if !c.revoked && time.Until(c.expiry) < s.renewBelow {
			due = append(due, c)
		}
	}
	self := s.self
	s.mu.Unlock()
	if self == nil {
		return // leases run out; reconcile closes the rest
	}
	var wg sync.WaitGroup
	for _, c := range due {
		wg.Go(func() {
			err := s.whois(c.remote, c.rec.Node, self.user)
			s.mu.Lock()
			if err == nil && !c.revoked && s.self != nil && s.self.user == self.user {
				c.expiry = time.Now().Add(s.lease)
				c.timer.Reset(s.lease)
				s.mu.Unlock()
				return
			}
			refused := errors.Is(err, errRefused) && !c.revoked
			if refused {
				c.revoked = true
			}
			s.mu.Unlock()
			if refused {
				log.Printf("peer %s: %v; closing its connection from %s", c.rec.Name, err, c.remote.Addr())
				c.Close()
			} else if err != nil {
				log.Printf("peer %s: lease not renewed: %v", c.rec.Name, err)
			}
		})
	}
	wg.Wait()
}

// ---- per-request -----------------------------------------------------------

// Identify is the per-request check (R2): the request's connection must be
// registered and unrevoked, within its lease, and its certificate's key
// paired, as the same record, in the live generation. It returns the
// peer's name and its origin, the pages' data-origin (never a header).
func (s *Server) Identify(r *http.Request) (name, origin string, ok bool) {
	c, _ := r.Context().Value(connKey{}).(*conn)
	if c == nil || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		return "", "", false
	}
	spki := SPKI(r.TLS.PeerCertificates[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.registered || c.revoked || c.spki != spki || !time.Now().Before(c.expiry) {
		return "", "", false
	}
	rec := s.peers.Lookup(spki)
	if rec == nil || !sameRecord(*rec, c.rec) {
		return "", "", false
	}
	return rec.Name, rec.Origin, true
}

// HostOK reports whether host names one of the addresses the listener is
// on, with its port.
func (s *Server) HostOK(host string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bl := range s.lns {
		if bl.host == host {
			return true
		}
	}
	return false
}

// track admits each request against its connection, so a reload can wait
// for the last one to return. net/http's HTTP/2 serve loop doesn't wait for
// its handler goroutines, so one can start after the connection is
// finished: it's refused before doing anything.
func (s *Server) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Context().Value(connKey{}).(*conn)
		if c == nil || !c.enter() {
			http.Error(w, "connection closed", http.StatusForbidden)
			return
		}
		defer c.exit()
		next.ServeHTTP(w, r)
	})
}

// ---- connections ------------------------------------------------------------

type connKey struct{}

// conn is one accepted TCP connection under its TLS.
type conn struct {
	net.Conn
	s      *Server
	local  netip.Addr     // the listener's address
	remote netip.AddrPort // the peer's, unmapped

	// Under s.mu.
	registered bool
	revoked    bool
	rec        Record // set by admit, during the handshake
	spki       string
	user       int64 // the tailnet user admit checked against
	expiry     time.Time
	timer      *time.Timer

	// A registered connection stays in s.conns until finished: net/http is
	// done with it (served) and no admitted handler is running (active).
	servedDone bool
	active     int
	retired    bool
	finished   chan struct{} // closed at retirement

	closeOne sync.Once
}

func asConn(nc net.Conn) *conn {
	tc, ok := nc.(*tls.Conn)
	if !ok {
		return nil
	}
	c, _ := tc.NetConn().(*conn)
	return c
}

// Close closes the socket, which ends every stream on it, and revokes the
// connection. It stays registered until finished (retireLocked).
func (c *conn) Close() error {
	var err error
	c.closeOne.Do(func() {
		err = c.Conn.Close()
		c.s.mu.Lock()
		c.revoked = true
		if c.timer != nil {
			c.timer.Stop()
		}
		c.s.mu.Unlock()
	})
	return err
}

// markServed: net/http is done with the connection.
func (c *conn) markServed() {
	c.s.mu.Lock()
	c.servedDone = true
	c.s.retireLocked(c)
	c.s.mu.Unlock()
}

// enter admits a handler, unless the connection is finished.
func (c *conn) enter() bool {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.retired || c.servedDone {
		return false
	}
	c.active++
	return true
}

func (c *conn) exit() {
	c.s.mu.Lock()
	c.active--
	c.s.retireLocked(c)
	c.s.mu.Unlock()
}

// retireLocked drops a finished connection from the registry.
func (s *Server) retireLocked(c *conn) {
	if c.retired || !c.servedDone || c.active > 0 {
		return
	}
	c.retired = true
	c.revoked = true
	delete(s.conns, c)
	close(c.finished)
}
