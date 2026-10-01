package link

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/peer"
)

// errPinMismatch: the server presented a key other than the pinned one.
var errPinMismatch = errors.New("link: the server's key isn't the pinned one")

// PinnedTLS is the client's TLS config, and the only place
// InsecureSkipVerify is set: chain verification has to be off for a
// self-signed certificate to reach any callback, so VerifyConnection, which
// runs on every handshake (resumed or not, R2), requires exactly one
// certificate whose SPKI is pin. No ClientSessionCache: every connection
// is a full handshake. TLS 1.3, h2, and this client's certificate.
func PinnedTLS(cert tls.Certificate, pin string) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"h2"},
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if pin == "" || len(cs.PeerCertificates) != 1 || peer.SPKI(cs.PeerCertificates[0]) != pin {
				return errPinMismatch
			}
			return nil
		},
	}
}

// dialError: no TCP connection could be made.
type dialError struct{ err error }

func (e *dialError) Error() string { return "link: dial: " + e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// session is one up period of the link: a transport of its own that dials
// only target, and the connections it made, so that going down closes
// every one of them with all their streams. CloseIdleConnections alone
// would leave a busy connection, and the authorization it was checked
// under, running (N9).
type session struct {
	id     SessionID
	target netip.AddrPort
	host   string // target as a Host header names it, as the peer listener checks it
	tls    *tls.Config
	tr     *http.Transport
	dialTO time.Duration
	hsTO   time.Duration
	timer  *time.Timer // the lease's expiry, under Link.mu

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	// closeOnce: a second close waits for the first to finish.
	closeOnce   sync.Once
	beforeClose func() // tests
	// dialing counts dials between the closed check and registration;
	// ctx ends at close, cancelling them.
	dialing   sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
	afterDial func(net.Conn) // tests: between the TCP dial and registration
}

// Transport limits.
const (
	// headerBackstop is the transport's own response-header timeout: the
	// longest any route waits (web.Route.Wait). The client proxy applies
	// each route's own, DefaultWait unless the route says otherwise.
	headerBackstop = 5 * time.Minute
	// HTTP/2 pings find a connection that died under a sleeping laptop.
	pingIdle    = 30 * time.Second
	pingTimeout = 10 * time.Second
	// maxHeaderBytes bounds an answer's header block.
	maxHeaderBytes = 64 << 10
)

// newSession is a session to target with the current credentials, owned
// by the link from here until retired; false once unpaired.
func (l *Link) newSession(target netip.AddrPort) (*session, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.unpaired {
		return nil, false
	}
	l.lastID++
	s := &session{
		id:     l.lastID,
		target: target, host: target.String(),
		tls:    PinnedTLS(l.id.Cert, l.pin),
		dialTO: l.Timing.Dial, hsTO: l.Timing.Handshake,
		conns: map[net.Conn]struct{}{},
	}
	protos := new(http.Protocols)
	protos.SetHTTP2(true) // and only HTTP/2 (R17)
	s.tr = &http.Transport{
		Protocols:              protos,
		ForceAttemptHTTP2:      true,
		Proxy:                  nil, // never an environment proxy
		DialTLSContext:         s.dial,
		TLSClientConfig:        s.tls,
		MaxIdleConns:           1,
		MaxIdleConnsPerHost:    1,
		ResponseHeaderTimeout:  headerBackstop,
		MaxResponseHeaderBytes: maxHeaderBytes, // an answer's headers are a handful of short lines
		DisableCompression:     true,           // identity only: the proxy passes no Content-Encoding
		HTTP2:                  &http.HTTP2Config{SendPingTimeout: pingIdle, PingTimeout: pingTimeout},
	}
	s.beforeClose, s.afterDial = l.beforeClose, l.afterDial
	s.ctx, s.cancel = context.WithCancel(context.Background())
	l.owned[s] = struct{}{}
	return s, true
}

// dial connects to the session's target, whatever address the transport
// asks for (a mismatch is refused), and completes the pinned handshake.
// The TCP connection is tracked, so close ends it under its TLS.
func (s *session) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	if addr != s.host {
		return nil, &dialError{fmt.Errorf("asked for %s, the link's server is %s", addr, s.host)}
	}
	// A dial is counted from here until its socket is registered or
	// closed, and close waits for the count: a socket established while
	// the session closes is never left open behind it (E2).
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, &dialError{errors.New("link went down")}
	}
	s.dialing.Add(1)
	s.mu.Unlock()
	dctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel) // close cancels a dial in progress
	d := net.Dialer{Timeout: s.dialTO}
	nc, err := d.DialContext(dctx, "tcp", s.host)
	stop()
	cancel()
	if err != nil {
		s.dialing.Done()
		return nil, &dialError{err}
	}
	if s.afterDial != nil {
		s.afterDial(nc)
	}
	tc := &trackedConn{Conn: nc, s: s}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		nc.Close()
		s.dialing.Done()
		return nil, &dialError{errors.New("link went down")}
	}
	s.conns[tc] = struct{}{}
	s.mu.Unlock()
	s.dialing.Done()
	c := tls.Client(tc, s.tls)
	hctx, cancel := context.WithTimeout(ctx, s.hsTO)
	defer cancel()
	if err := c.HandshakeContext(hctx); err != nil {
		c.Close()
		return nil, err
	}
	if c.ConnectionState().NegotiatedProtocol != "h2" {
		c.Close()
		return nil, errors.New("link: the server didn't negotiate h2")
	}
	return c, nil
}

// close ends the session: no more dials, every connection closed. Any
// caller returns only once that's done.
func (s *session) close() { s.closeOnce.Do(s.doClose) }

func (s *session) doClose() {
	s.mu.Lock()
	s.closed = true // no new dials while the hook runs
	s.mu.Unlock()
	s.cancel()
	if s.beforeClose != nil {
		s.beforeClose()
	}
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = map[net.Conn]struct{}{}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	for c := range conns {
		c.Close()
	}
	s.tr.CloseIdleConnections()
	// No Add after closed was set, so this waits only for dials already
	// past the check: each registers (and was closed above, or is by its
	// own closed check) or closes its socket.
	s.dialing.Wait()
}

// trackedConn leaves the session's set when closed.
type trackedConn struct {
	net.Conn
	s    *session
	once sync.Once
	err  error
}

// Close closes the socket and leaves the set as one step: a second caller
// (Unpair racing a failed handshake's close) waits for the first close to
// finish, since Go's own second Close returns before the descriptor is
// gone (F1).
func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.s.mu.Lock()
		delete(c.s.conns, c)
		c.s.mu.Unlock()
	})
	return c.err
}
