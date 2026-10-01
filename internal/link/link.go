package link

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
	"github.com/jmckible/pneu/internal/wake"
	"github.com/jmckible/pneu/internal/web"
)

// Reason is the link's state as a local code (docs/client.md,
// "Unreachable, mismatch, unknown outcomes"). No server text ever goes
// into one: status.json, the error page and agent prompts are built from
// these.
type Reason string

const (
	// Starting: no attempt has finished yet.
	Starting Reason = "starting"
	Up       Reason = "up"
	// TailscaleDown: LocalAPI unreachable, or BackendState isn't Running.
	TailscaleDown Reason = "tailscale-down"
	// NodeOffline: the server's node is offline, or not in the netmap.
	NodeOffline Reason = "node-offline"
	// NodeMismatch: whois at the server's address fails the predicate
	// (another node, tagged, shared in, another user).
	NodeMismatch Reason = "node-mismatch"
	// Refused: the dial was refused or timed out, or pneu answered with
	// something other than a hello.
	Refused Reason = "refused"
	// PinMismatch: the server's key isn't the pinned one. Never retried in
	// the background.
	PinMismatch Reason = "pin-mismatch"
	// NotPaired: the server refused this client's certificate (a TLS
	// alert) or its peer guard refused the request.
	NotPaired Reason = "not-paired"
	// Protocol: hello names another protocol, or breaks its shape.
	Protocol Reason = "protocol"
)

// State is the link's state since Since. Detail is local words for the log
// and the error page, never the server's.
type State struct {
	Reason Reason
	Since  time.Time
	Detail string
	// Asleep is set while the link is starting within Waking of this
	// machine waking (woke): how long it slept. The error page shows a
	// wake page for it, not a failure.
	Asleep time.Duration
}

// Hello is what the server's /peer/hello said on connect, validated.
type Hello struct {
	Name     string // its hostname, printable, ≤ 64 runes; "" if it wasn't
	Revision string // 40 hex digits, or ""
	Modified bool
	Epoch    string
	Gen      uint64
	Accounts []web.HelloAccount
}

// API is the part of tailscaled's LocalAPI the link uses.
type API interface {
	StatusPeers(ctx context.Context) (tailscale.Status, error)
	WhoIs(ctx context.Context, addr netip.AddrPort) (tailscale.WhoIs, error)
}

// Timing (docs/client.md, "Tailscale as the second check"): the same lease
// as the server's, on the client's own pooled connection (N9).
const (
	BackoffMin = time.Second
	BackoffMax = 30 * time.Second
	// HelloWait bounds the connect-time hello; ProbeWait an on-demand one
	// (launch, focus, R), which must answer fast on a laptop just woken.
	HelloWait = 10 * time.Second
	ProbeWait = 3 * time.Second
	// Waking is Timing.Waking outside tests.
	Waking = time.Minute
	// maxHello bounds the hello document.
	maxHello = 64 << 10
	// maxAccounts bounds hello's account set (status.json's limit too).
	maxAccounts = 16
)

// Timing is the link's clock.
type Timing struct {
	Lease, RenewBelow, Sweep time.Duration // the whois lease (peer's constants)
	BackoffMin, BackoffMax   time.Duration
	HelloWait, ProbeWait     time.Duration
	Dial, Handshake          time.Duration
	// Waking is how long after a wake a failure that a network still
	// coming back explains (tailscale-down, node-offline, refused) keeps
	// the link starting, retried every BackoffMin rather than backing
	// off: a resume takes seconds to bring Wi-Fi and the tailnet back.
	Waking time.Duration
}

// DefaultTiming is the link's clock outside tests.
func DefaultTiming() Timing {
	return Timing{
		Lease: peer.Lease, RenewBelow: peer.RenewBelow, Sweep: peer.Sweep,
		BackoffMin: BackoffMin, BackoffMax: BackoffMax,
		HelloWait: HelloWait, ProbeWait: ProbeWait,
		Dial: 5 * time.Second, Handshake: 10 * time.Second,
		Waking: Waking,
	}
}

// Link keeps the one link to the server: a session (transport and its
// connections) while up, none while down.
type Link struct {
	api  API
	id   peer.Identity
	node string
	port int
	pin  string

	// Timing may be changed before Start (tests).
	Timing Timing

	// OnChange sees every state change, outside the lock (a seam for the
	// status file and the event fan-out).
	OnChange func(State)

	mu     sync.Mutex
	state  State
	hello  *Hello
	sess   *session
	user   int64 // this node's tailnet user, from the last attempt
	expiry time.Time
	upc    chan struct{} // closed while up
	// owned is every session from its creation until its close has
	// completed: an attempt's before it's published, the live one, and
	// ones going down. Unpair waits for it to drain. unpaired, once
	// Unpair ran: no session is created or published after.
	owned    map[*session]struct{}
	unpaired bool
	lastID   SessionID // the last session's (newSession)
	// Seams for tests: beforePublish runs between hello and publishing,
	// beforeClose inside each session's close before its connections go.
	beforePublish, beforeClose func()
	afterDial                  func(net.Conn)

	// wake notices this machine slept (wake.go): every timer above
	// stopped with it, so the session goes and an attempt follows.
	wake *wake.Clock
	// wokeAt and asleep are the last wake, until the link is up again
	// (zero after); waking() is within Timing.Waking of it.
	wokeAt time.Time
	asleep time.Duration

	kick chan struct{}
	stop chan struct{}
	wg   sync.WaitGroup
}

// New is a stopped link to the server creds pinned, on port.
func New(api API, creds Creds, port int) *Link {
	return &Link{
		api: api, id: creds.Identity, node: creds.Pin.Node, port: port, pin: creds.Pin.SPKI,
		Timing: DefaultTiming(),
		state:  State{Reason: Starting, Since: time.Now()},
		upc:    make(chan struct{}),
		owned:  map[*session]struct{}{},
		wake:   wake.New(),
		kick:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
	}
}

// Start connects in the background and keeps the link up.
func (l *Link) Start() {
	l.wg.Go(l.run)
	l.wg.Go(func() { l.wake.Watch(l.stop, wake.Every, l.woke) })
}

// Close stops the link and closes its connection.
func (l *Link) Close() {
	select {
	case <-l.stop:
		return
	default:
	}
	close(l.stop)
	l.wg.Wait()
	l.mu.Lock()
	l.sess = nil
	l.mu.Unlock()
	l.drain()
}

// State is the link's current state.
func (l *Link) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state
	if st.Reason == Starting && l.wakingLocked() {
		st.Asleep = l.asleep
	}
	return st
}

// wakingLocked: within Timing.Waking of the last wake, not yet up since.
func (l *Link) wakingLocked() bool {
	return !l.wokeAt.IsZero() && time.Since(l.wokeAt) < l.Timing.Waking
}

func (l *Link) waking() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wakingLocked()
}

// transient: a reason a network still coming back after a wake explains.
// The rest (pin, pairing, protocol) are answers, shown at once.
func transient(r Reason) bool {
	return r == TailscaleDown || r == NodeOffline || r == Refused
}

// failed records an attempt's failure: within the wake window a
// transient one keeps the link starting (logged, as no state change
// is), anything else is the reason.
func (l *Link) failed(reason Reason, detail string) {
	if transient(reason) && l.waking() {
		log.Printf("link: waking: %s: %s", reason, detail)
		reason, detail = Starting, "reconnecting after sleep ("+string(reason)+": "+detail+")"
	}
	l.setDown(nil, reason, detail)
}

// Hello is the last connect's hello; nil while never up.
func (l *Link) Hello() *Hello {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hello
}

// Email is an account's address from the server's hello: what a /gmail
// redirect's authuser must be (web.Checker.Email).
func (l *Link) Email(account string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hello == nil {
		return "", false
	}
	for _, a := range l.hello.Accounts {
		if a.Name == account {
			return a.Email, true
		}
	}
	return "", false
}

// Retry asks for an attempt now when down (pin-mismatch included: this is
// the user asking), or a hello probe when up. It doesn't wait.
func (l *Link) Retry() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// WaitUp waits until the link is up, or ctx ends.
func (l *Link) WaitUp(ctx context.Context) bool {
	l.mu.Lock()
	c := l.upc
	l.mu.Unlock()
	select {
	case <-c:
		return true
	case <-ctx.Done():
		return false
	}
}

// DownError: the link wasn't up, so the request never left this machine.
type DownError struct{ Reason Reason }

func (e *DownError) Error() string { return "link: not up (" + string(e.Reason) + ")" }

// SessionID names one up period's session, so a failure seen on a stream
// can be pinned on the session that carried it and not on a later one.
// Never 0.
type SessionID uint64

// RoundTrip sends req to the server over the live session: the scheme and
// authority are the session's (the address it resolved and checked, as
// the peer listener's Host check wants it), whatever req says. Down, it
// fails with a *DownError before anything is dialed.
func (l *Link) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, _, err := l.RoundTripOn(req)
	return resp, err
}

// RoundTripOn is RoundTrip, also naming the session that carried it: the
// only one a failure on that response may be blamed on (Stalled).
func (l *Link) RoundTripOn(req *http.Request) (*http.Response, SessionID, error) {
	l.CheckWake()
	l.mu.Lock()
	s, reason := l.sess, l.state.Reason
	if s != nil && !time.Now().Before(l.expiry) {
		s, reason = nil, TailscaleDown // lapsed; expire closes it
	}
	l.mu.Unlock()
	if s == nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, 0, &DownError{Reason: reason}
	}
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = "https", s.host, s.host
	resp, err := s.tr.RoundTrip(out)
	if err != nil && req.Context().Err() == nil {
		l.Retry() // a failing connection: a probe settles the state
	}
	return resp, s.id, err
}

// Stalled is the event stream's silence watchdog (docs/client.md,
// "Liveness"): nothing from the server, not even its 25s heartbeat, for
// 60s means the connection is dead though nothing has failed on it yet (a
// laptop that slept). id is the session that carried the stream
// (RoundTripOn): if it's still live it goes down with every stream on it,
// and an attempt follows at once, which names the real reason. A session
// already replaced is left alone: the stall was the old one's.
func (l *Link) Stalled(id SessionID) {
	l.mu.Lock()
	s := l.sess
	l.mu.Unlock()
	if s == nil || s.id != id {
		return
	}
	l.setDown(s, Refused, "the server's event stream went quiet")
	l.Retry()
}

// Reconnect: a request on session id got no answer though the session
// was live (a reset, an EOF): a connection the other end dropped, most
// often while this machine slept. If id is still the live session it goes
// down as starting, with every stream on it, and an attempt follows at
// once, so a safe request's one retry (client.roundTripper) goes out on a
// fresh session. A failing attempt names the real reason.
func (l *Link) Reconnect(id SessionID) {
	l.mu.Lock()
	s := l.sess
	l.mu.Unlock()
	if s == nil || s.id != id {
		return
	}
	l.setDown(s, Starting, "a request on the connection failed; reconnecting")
	l.Retry()
}

// CheckWake looks for a sleep now rather than at the next tick: a launch
// or a page load right after waking may arrive first. RoundTripOn checks
// on its own.
func (l *Link) CheckWake() {
	if asleep, ok := l.wake.Slept(); ok {
		l.woke(asleep)
	}
}

// woke: this machine slept, so nothing the link knows is current. Its
// timers stopped (wake.go): the lease still has its time and the session
// still looks live, though the server dropped it long ago. The session
// goes, the state says starting (a down reason from before the sleep is
// stale too), and an attempt follows at once. A pin mismatch stays: it's
// never retried in the background.
func (l *Link) woke(asleep time.Duration) {
	l.mu.Lock()
	s, pin := l.sess, l.state.Reason == PinMismatch
	if !pin {
		l.wokeAt, l.asleep = time.Now(), asleep
	}
	l.mu.Unlock()
	if pin {
		return
	}
	l.setDown(s, Starting, "woke from "+asleep.Round(time.Second).String()+" asleep; reconnecting")
	l.Retry()
}

// ---- the loop ---------------------------------------------------------------

func (l *Link) run() {
	delay := time.Duration(0)
	for {
		st := l.State()
		var wait <-chan time.Time
		switch {
		case l.isUnpaired():
			<-l.stop // unlinked: nothing more, not even on demand
			return
		case st.Reason == Up:
			wait = time.After(l.Timing.Sweep)
		case st.Reason == PinMismatch:
			// Never in the background: only Retry.
		default:
			wait = time.After(delay)
		}
		kicked := false
		select {
		case <-l.stop:
			return
		case <-l.kick:
			kicked = true
		case <-wait:
		}
		if l.State().Reason == Up {
			if kicked {
				l.probe()
			} else {
				l.renew()
			}
			delay = 0 // gone down: straight back to attempting
			continue
		}
		switch {
		case l.attempt():
			delay = l.Timing.BackoffMin
		case kicked || delay == 0 || l.waking():
			delay = l.Timing.BackoffMin
		default:
			delay = min(delay*2, l.Timing.BackoffMax)
		}
	}
}

// attempt runs the pre-dial checks, dials, and says hello. It reports
// whether the link is up.
func (l *Link) attempt() bool {
	// The lease runs from before the pre-dial whois, as the server's runs
	// from its handshake's.
	checked := time.Now()
	target, user, reason, detail := l.resolve()
	if reason != "" {
		l.failed(reason, detail)
		return false
	}
	s, ok := l.newSession(target)
	if !ok {
		return false // unpaired meanwhile
	}
	h, reason, detail := l.sayHello(s, l.Timing.HelloWait)
	if reason != "" {
		l.retire(s)
		l.failed(reason, detail)
		return false
	}
	if l.beforePublish != nil {
		l.beforePublish()
	}
	expiry := checked.Add(l.Timing.Lease)
	// Pending to live is one step under the lock; s stays owned throughout.
	l.mu.Lock()
	if l.stopped() || l.unpaired {
		l.mu.Unlock()
		l.retire(s)
		return false
	}
	if !time.Now().Before(expiry) {
		// A slow hello outlasted the whois that vouched for it.
		l.mu.Unlock()
		l.retire(s)
		l.failed(TailscaleDown, "the whois lease ran out before hello finished")
		return false
	}
	old := l.sess
	l.sess, l.hello, l.user = s, h, user
	l.wokeAt, l.asleep = time.Time{}, 0
	l.expiry = expiry
	s.timer = time.AfterFunc(time.Until(expiry), func() { l.expire(s) })
	changed := l.setLocked(State{Reason: Up, Since: time.Now(), Detail: "connected to " + target.String()})
	close(l.upc)
	l.mu.Unlock()
	if old != nil {
		l.retire(old)
	}
	l.changed(changed)
	return true
}

// Unpair is the control socket's `unlink` (pneu client unpair): the link
// forgets its identity and pin, closes every connection of its session
// (busy ones included) and in-flight attempt's, and never reconnects.
// When it returns the connections are closed.
//
// Every caller returns only once every session the link owned has
// finished closing: one being published, one going down in setDown, or
// one another Unpair is closing (a session's close is once, and a second
// caller waits for the first).
func (l *Link) Unpair() {
	l.mu.Lock()
	l.unpaired = true
	l.id, l.pin = peer.Identity{}, ""
	l.sess = nil
	if l.state.Reason == Up {
		l.upc = make(chan struct{})
	}
	changed := l.setLocked(State{Reason: NotPaired, Since: time.Now(), Detail: "unpaired here"})
	l.mu.Unlock()
	l.changed(changed)
	l.drain()
}

// drain closes every owned session and returns once none is left. With
// unpaired set (or the loop stopped) none can be added meanwhile.
func (l *Link) drain() {
	for {
		l.mu.Lock()
		all := make([]*session, 0, len(l.owned))
		for s := range l.owned {
			all = append(all, s)
		}
		l.mu.Unlock()
		if len(all) == 0 {
			return
		}
		for _, s := range all {
			l.retire(s)
		}
	}
}

// retire closes s, waiting for a close already running, and only then
// stops owning it.
func (l *Link) retire(s *session) {
	s.close()
	l.mu.Lock()
	delete(l.owned, s)
	l.mu.Unlock()
}

func (l *Link) isUnpaired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unpaired
}

func (l *Link) stopped() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

// resolve is the mirror of the server's check (docs/client.md): tailscaled
// Running, the server's node found by StableID (never MagicDNS) and
// online, one of its own addresses chosen, and whois about that address
// passing the predicate against this node's user.
func (l *Link) resolve() (netip.AddrPort, int64, Reason, string) {
	ctx, cancel := context.WithTimeout(context.Background(), tailscale.CallTimeout)
	st, err := l.api.StatusPeers(ctx)
	cancel()
	switch {
	case err != nil:
		return netip.AddrPort{}, 0, TailscaleDown, fmt.Sprintf("tailscale unavailable: %v", err)
	case st.BackendState != tailscale.Running:
		return netip.AddrPort{}, 0, TailscaleDown, "tailscale is " + oneWord(st.BackendState)
	case st.Self.UserID == 0:
		return netip.AddrPort{}, 0, TailscaleDown, "tailscale names no user for this node"
	}
	var ps *tailscale.PeerStatus
	for _, p := range st.Peer {
		if p.StableID == l.node {
			ps = &p
			break
		}
	}
	switch {
	case ps == nil:
		return netip.AddrPort{}, 0, NodeOffline, "the server's node " + l.node + " isn't in this tailnet's map"
	case !ps.Online:
		d := "the server's node is offline"
		if !ps.LastSeen.IsZero() {
			d += " (last seen " + ps.LastSeen.UTC().Format(time.RFC3339) + ")"
		}
		return netip.AddrPort{}, 0, NodeOffline, d
	}
	addr, ok := pickAddr(ps.TailscaleIPs)
	if !ok {
		return netip.AddrPort{}, 0, NodeOffline, "the server's node has no address"
	}
	target := netip.AddrPortFrom(addr, uint16(l.port))
	if err := l.whois(target, st.Self.UserID); err != nil {
		if errors.Is(err, errNoAnswer) {
			return netip.AddrPort{}, 0, TailscaleDown, err.Error()
		}
		return netip.AddrPort{}, 0, NodeMismatch, err.Error()
	}
	return target, st.Self.UserID, "", ""
}

// pickAddr prefers the node's IPv4 address; the listener binds both.
func pickAddr(ips []netip.Addr) (netip.Addr, bool) {
	var v6 netip.Addr
	for _, a := range ips {
		a = a.Unmap()
		switch {
		case !a.IsValid() || a.IsUnspecified() || a.IsMulticast():
		case a.Is4():
			return a, true
		case !v6.IsValid():
			v6 = a
		}
	}
	return v6, v6.IsValid()
}

var errNoAnswer = errors.New("whois didn't answer")

// whois applies peer.CheckWhoIs to whatever tailscaled says is at target:
// the recorded server node, untagged, not shared in, our user. An error
// wrapping errNoAnswer means no answer, anything else a refusal.
func (l *Link) whois(target netip.AddrPort, user int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), tailscale.CallTimeout)
	defer cancel()
	w, err := l.api.WhoIs(ctx, target)
	switch {
	case errors.Is(err, tailscale.ErrNoMatch):
		return fmt.Errorf("whois: no node at %s", target.Addr())
	case err != nil:
		return fmt.Errorf("%w: %v", errNoAnswer, err)
	}
	return peer.CheckWhoIs(w, target.Addr(), l.node, user)
}

// renew is the client-side lease (N9): with less than renewBelow left,
// whois the connected address again. A refusal closes the session at once,
// with every stream on it; no answer leaves it to expire.
func (l *Link) renew() {
	l.mu.Lock()
	s, user, left := l.sess, l.user, time.Until(l.expiry)
	l.mu.Unlock()
	if s == nil || left >= l.Timing.RenewBelow {
		return
	}
	err := l.whois(s.target, user)
	switch {
	case err == nil:
		l.mu.Lock()
		if l.sess == s {
			l.expiry = time.Now().Add(l.Timing.Lease)
			s.timer.Reset(l.Timing.Lease)
		}
		l.mu.Unlock()
	case errors.Is(err, errNoAnswer):
		log.Printf("link: lease not renewed: %v", err)
	default:
		l.setDown(s, NodeMismatch, err.Error())
	}
}

// expire closes s if its lease ran out without a renewal.
func (l *Link) expire(s *session) {
	l.mu.Lock()
	if l.sess != s {
		l.mu.Unlock()
		return
	}
	if left := time.Until(l.expiry); left > 0 {
		s.timer.Reset(left)
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	l.setDown(s, TailscaleDown, "whois lease expired without a renewal")
}

// probe says hello on the live session, fast: a laptop just woken can't
// wait for a heartbeat to learn its connection died.
func (l *Link) probe() {
	l.mu.Lock()
	s := l.sess
	l.mu.Unlock()
	if s == nil {
		return
	}
	h, reason, detail := l.sayHello(s, l.Timing.ProbeWait)
	if reason != "" {
		l.setDown(s, reason, detail)
		return
	}
	l.mu.Lock()
	if l.sess == s {
		l.hello = h
	}
	l.mu.Unlock()
}

// setDown records a down state; with s, only if s is still the session
// (closing it and every stream on it).
func (l *Link) setDown(s *session, reason Reason, detail string) {
	l.mu.Lock()
	if s != nil && l.sess != s || l.unpaired {
		l.mu.Unlock()
		return
	}
	old := l.sess
	l.sess = nil // no longer live, still owned until retired
	if l.state.Reason == Up {
		l.upc = make(chan struct{})
	}
	changed := l.setLocked(State{Reason: reason, Since: time.Now(), Detail: detail})
	l.mu.Unlock()
	if old != nil {
		l.retire(old)
	}
	l.changed(changed)
}

// setLocked sets the state, keeping Since across a repeat of the same
// reason, and returns the new state when it changed.
func (l *Link) setLocked(st State) *State {
	if st.Reason == l.state.Reason {
		l.state.Detail = st.Detail
		return nil
	}
	l.state = st
	return &st
}

func (l *Link) changed(st *State) {
	if st == nil {
		return
	}
	log.Printf("link: %s: %s", st.Reason, st.Detail)
	if l.OnChange != nil {
		l.OnChange(*st)
	}
}

// ---- hello ------------------------------------------------------------------

var (
	revisionRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	epochRE    = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
)

// sayHello is GET /peer/hello on s within wait, classified.
func (l *Link) sayHello(s *session, wait time.Duration) (*Hello, Reason, string) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s.host+"/peer/hello", nil)
	req.Host = s.host
	resp, err := s.tr.RoundTrip(req)
	if err != nil {
		reason, detail := Classify(err)
		return nil, reason, detail
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxHello+1))
	switch {
	case err != nil:
		reason, detail := Classify(err)
		return nil, reason, detail
	case resp.StatusCode == http.StatusForbidden:
		return nil, NotPaired, "the server's peer guard refused this client"
	case resp.StatusCode != http.StatusOK:
		return nil, Refused, "hello answered HTTP " + strconv.Itoa(resp.StatusCode)
	case resp.ProtoMajor != 2:
		return nil, Refused, "hello over " + resp.Proto
	case len(b) > maxHello:
		return nil, Protocol, "hello over its size cap"
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, Protocol, "hello isn't JSON"
	}
	if p := resp.Header.Get(web.ProtocolHeader); p != strconv.Itoa(web.Protocol) {
		return nil, Protocol, fmt.Sprintf("server protocol %q, this client's %d", oneWord(p), web.Protocol)
	}
	return parseHello(b)
}

// parseHello checks the protocol before anything else (a different
// protocol may shape the rest differently), then holds every field to a
// shape: the account set is what /gmail redirects are checked against.
func parseHello(b []byte) (*Hello, Reason, string) {
	var head struct {
		Protocol int `json:"protocol"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, Protocol, "hello doesn't parse"
	}
	if head.Protocol != web.Protocol {
		return nil, Protocol, fmt.Sprintf("server protocol %d, this client's %d", head.Protocol, web.Protocol)
	}
	var doc struct {
		Name     string             `json:"name"`
		Revision string             `json:"revision"`
		Modified bool               `json:"modified"`
		Epoch    string             `json:"epoch"`
		Gen      uint64             `json:"gen"`
		Accounts []web.HelloAccount `json:"accounts"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, Protocol, "hello doesn't parse"
	}
	if !epochRE.MatchString(doc.Epoch) || len(doc.Accounts) > maxAccounts {
		return nil, Protocol, "hello's epoch or account set is out of shape"
	}
	h := &Hello{Modified: doc.Modified, Epoch: doc.Epoch, Gen: doc.Gen}
	if printable(doc.Name, 64) {
		h.Name = doc.Name
	}
	if revisionRE.MatchString(doc.Revision) {
		h.Revision = doc.Revision
	}
	seen := map[string]bool{}
	for _, a := range doc.Accounts {
		if !config.ValidName(a.Name) || seen[a.Name] || !validEmail(a.Email) {
			return nil, Protocol, "hello's account set is out of shape"
		}
		seen[a.Name] = true
		h.Accounts = append(h.Accounts, a)
	}
	return h, "", ""
}

// validEmail is an address shaped enough to compare: one '@', at most 254
// bytes, printable ASCII without space or angle brackets.
func validEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || c == '<' || c == '>' {
			return false
		}
		if c == '@' {
			at++
		}
	}
	return at == 1 && s[0] != '@' && s[len(s)-1] != '@'
}

func printable(s string, max int) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// oneWord keeps a value from tailscaled or the wire short and plain in a
// local detail.
func oneWord(s string) string {
	if len(s) > 32 || !printable(s, 32) {
		return "unknown"
	}
	return s
}

// Classify turns a dial, handshake or request error into a reason code.
func Classify(err error) (Reason, string) {
	var down *DownError
	var op *net.OpError
	switch {
	case errors.As(err, &down):
		return down.Reason, "link down"
	case errors.Is(err, errPinMismatch):
		return PinMismatch, "the server's key isn't the pinned one"
	case errors.As(err, &op) && op.Op == "remote error":
		// A TLS alert: TLS 1.3 servers judge the client's certificate after
		// the client's handshake is done, so it arrives on the first read.
		return NotPaired, "the server refused this client's certificate"
	case errors.As(err, new(*dialError)):
		return Refused, "nothing answered on the server's peer port"
	case errors.As(err, new(tls.RecordHeaderError)), errors.As(err, new(*tls.CertificateVerificationError)):
		return Refused, "the peer port isn't speaking pneu's TLS"
	}
	return Refused, "the connection failed"
}
