package client

// The one upstream event stream (docs/client.md, "Events"; R14, N10): GET
// /events over the link whenever it's up. The first event must be hello,
// the only place hello is accepted; after it only syncing, sync, account,
// auth, status and view pass, each at most 64 KiB, decoded into its typed
// shape, held to limits and re-encoded: no upstream byte reaches a browser
// as it came. An event naming an account outside the link's hello set is
// dropped, as is a view at or below the stream's generation (the server
// orders its own, but this end can't hold its locks). Nothing the daemon
// says itself (link, theme, hello) is taken from upstream.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/web"
)

const (
	// MaxEvent bounds one upstream event: its lines, comments included.
	MaxEvent = 64 << 10
	// Silence is how long the stream may say nothing at all (the server's
	// Hub pings every 25s) before the link is called down.
	Silence = 60 * time.Second
	// maxAccounts bounds hello's account set and status.json's accounts;
	// maxSenders the status doc's sender names; maxText every string in
	// the status doc, maxEventText the longer ones relayed to pages (an
	// account's last error). maxThreads bounds a view's thread list.
	maxAccounts  = 16
	maxSenders   = 5
	maxText      = 128
	maxEventText = 512
	maxThreads   = 1000
	// StreamMin and StreamMax bound the upstream stream's reconnect
	// backoff; Healthy is how long a stream must last before it resets:
	// a valid hello alone doesn't, or a server could hello, flood, and be
	// reconnected to at once forever.
	StreamMin = time.Second
	StreamMax = 30 * time.Second
	Healthy   = 60 * time.Second
)

// Budget is the upstream stream's work budget (N14), two token buckets
// checked before anything is parsed: every byte off the wire (comments,
// discarded and oversized input included) and every block (an event or a
// comment, each blank line that ends one). Over either, the stream is
// closed and reopened with backoff. A healthy server sends a few events a
// minute, a ping every 25s and one hello of at most 64 KiB per connect; a
// first pull, an `account` per progress report. The defaults leave two
// orders of magnitude of headroom for that and still bound what a hostile
// one costs: 256 KiB and 50 events a second sustained, bursts of 2 MiB
// and 200 events (a hello and a burst of account views on a reconnect).
type Budget struct {
	BytesPerSec, BytesBurst   float64
	EventsPerSec, EventsBurst float64
}

// DefaultBudget is the stream's budget outside tests.
var DefaultBudget = Budget{BytesPerSec: 256 << 10, BytesBurst: 2 << 20, EventsPerSec: 50, EventsBurst: 200}

// bucket is a token bucket: rate a second, holding at most burst.
type bucket struct {
	rate, burst, level float64
	at                 time.Time
}

func newBucket(rate, burst float64) *bucket {
	return &bucket{rate: rate, burst: burst, level: burst, at: time.Now()}
}

func (b *bucket) take(n float64) bool {
	now := time.Now()
	b.level = min(b.burst, b.level+now.Sub(b.at).Seconds()*b.rate)
	b.at = now
	if b.level < n {
		return false
	}
	b.level -= n
	return true
}

// logLimit prints at most one line per its period, counting the rest.
type logLimit struct {
	mu         sync.Mutex
	every      time.Duration
	last       time.Time
	suppressed int
}

func (l *logLimit) printf(format string, args ...any) {
	l.mu.Lock()
	if !l.last.IsZero() && time.Since(l.last) < l.every {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	n := l.suppressed
	l.last, l.suppressed = time.Now(), 0
	l.mu.Unlock()
	if n > 0 {
		format += " (%d more like it suppressed)"
		args = append(args, n)
	}
	log.Printf(format, args...)
}

var (
	errHandshake = errors.New("the stream didn't open with a valid hello")
	errSilent    = errors.New("the stream went quiet")
	errStreamHdr = errors.New("no stream headers within the wait")
	errBudget    = errors.New("the stream went over its work budget")
)

// runUpstream keeps the one upstream stream open while the link is up,
// until ctx ends. A stream that fails without the link going down backs
// off, and the backoff resets only after a stream that lasted healthy;
// one that ends with the link is reopened as soon as it's back.
func (d *Daemon) runUpstream(ctx context.Context) {
	delay := time.Duration(0)
	for {
		if !d.up.WaitUp(ctx) {
			return
		}
		start := time.Now()
		err := d.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		d.streamLog.printf("client: events: %v", err)
		if time.Since(start) >= d.healthy {
			delay = 0
		}
		if d.up.State().Reason != link.Up {
			continue // WaitUp: reopened as soon as the link is back
		}
		if !errors.Is(err, errSilent) && !errors.Is(err, errBudget) && !errors.Is(err, errHandshake) {
			// A stream cut with the link still up: a probe says whether
			// the connection under it is dead too.
			d.up.Retry()
		}
		delay = min(max(delay*2, d.streamMin), d.streamMax)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// upstream is one stream's handshake: the account set the link's hello
// named, and the generation its events must climb from.
type upstream struct {
	hello bool
	set   map[string]bool
	epoch string
	gen   uint64
}

func (d *Daemon) drop(name, why string) {
	d.dropLog.printf("client: events: dropped %q (%s)", oneWord(name), why)
}

// stream opens /events upstream and takes its events until it ends.
func (d *Daemon) stream(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://server/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	t := time.AfterFunc(web.DefaultWait, func() { cancel(errStreamHdr) })
	resp, sess, err := d.up.RoundTripOn(req)
	t.Stop()
	if err != nil {
		if errors.Is(context.Cause(ctx), errStreamHdr) {
			return errStreamHdr
		}
		return err
	}
	defer resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	case mt != "text/event-stream":
		return errors.New("not an event stream")
	case resp.Header.Get(web.ProtocolHeader) != strconv.Itoa(web.Protocol):
		return errors.New("another protocol")
	}
	quiet := time.AfterFunc(d.silence, func() { cancel(errSilent) })
	defer quiet.Stop()
	h := &heard{r: resp.Body, quiet: quiet, every: d.silence, bytes: newBucket(d.budget.BytesPerSec, d.budget.BytesBurst)}
	sr := newSSEReader(h, MaxEvent)
	events := newBucket(d.budget.EventsPerSec, d.budget.EventsBurst)
	sr.onBlock = func() error {
		if !events.take(1) {
			return errBudget
		}
		return nil
	}
	u := &upstream{}
	for {
		ev, err := sr.next()
		if err != nil {
			if errors.Is(context.Cause(ctx), errSilent) {
				d.up.Stalled(sess)
				return errSilent
			}
			return err
		}
		if !u.hello {
			if err := d.handshake(u, ev); err != nil {
				return err
			}
			continue
		}
		d.take(u, ev)
	}
}

// heard is the wire as the reader sees it: every byte resets the silence
// timer and is charged to the byte budget before anything parses it.
type heard struct {
	r     io.Reader
	quiet *time.Timer
	every time.Duration
	bytes *bucket
}

func (h *heard) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.quiet.Reset(h.every)
		if !h.bytes.take(float64(n)) {
			return 0, errBudget
		}
	}
	return n, err
}

// ---- the SSE wire -----------------------------------------------------------

type sseEvent struct {
	name string
	data []byte
	over bool // past MaxEvent: its data was discarded
}

// sseReader reads events off the wire holding at most max bytes of any
// one: a longer line or event is read through and discarded, never
// buffered. Lines end in CR, LF or CRLF (a CRLF split across reads is one
// end); one leading UTF-8 BOM is skipped; `id` and `retry` are ignored. A
// block dispatches only if it had a data field (or ran past max): an
// event name alone, or a comment, makes no event. onBlock, if set, is
// charged for every block that ends, comments included.
type sseReader struct {
	br      *bufio.Reader
	max     int
	bom     bool // the stream's start is still to be checked for a BOM
	skipLF  bool // the last line ended in CR: an LF next is its end too
	onBlock func() error
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

func newSSEReader(r io.Reader, max int) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 4096), max: max, bom: true}
}

func (s *sseReader) next() (sseEvent, error) {
	var ev sseEvent
	size, blank, hasData := 0, true, false
	for {
		line, over, err := s.line()
		if err != nil {
			return sseEvent{}, err
		}
		if len(line) == 0 && !over {
			if blank {
				continue
			}
			if s.onBlock != nil {
				if err := s.onBlock(); err != nil {
					return sseEvent{}, err
				}
			}
			if hasData || ev.over {
				return ev, nil
			}
			ev, size, blank = sseEvent{}, 0, true
			continue
		}
		blank = false
		size += len(line) + 1
		if over || size > s.max {
			ev.over, ev.data = true, nil
			continue
		}
		if ev.over || line[0] == ':' {
			continue // a comment (the Hub's preamble and pings)
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.name = string(value)
		case "data":
			if hasData {
				ev.data = append(ev.data, '\n')
			}
			hasData = true
			ev.data = append(ev.data, value...)
		}
	}
}

// line is the next line without its ending; over when it ran past max and
// was discarded (read through a byte at a time, never held).
func (s *sseReader) line() ([]byte, bool, error) {
	if s.bom {
		s.bom = false
		if b, err := s.br.Peek(len(utf8BOM)); err == nil && bytes.Equal(b, utf8BOM) {
			s.br.Discard(len(utf8BOM))
		}
	}
	var out []byte
	n, over := 0, false
	for {
		c, err := s.br.ReadByte()
		if err != nil {
			return nil, false, err
		}
		if s.skipLF {
			s.skipLF = false
			if c == '\n' {
				continue
			}
		}
		switch c {
		case '\n':
			return out, over, nil
		case '\r':
			s.skipLF = true
			return out, over, nil
		}
		if n++; n > s.max {
			over, out = true, nil
		} else {
			out = append(out, c)
		}
	}
}

// ---- handshake and relay ----------------------------------------------------

// upstreamHello is the server's hello as this end reads it.
type upstreamHello struct {
	Epoch    string       `json:"epoch"`
	Gen      uint64       `json:"gen"`
	Accounts []upAccount  `json:"accounts"`
	Status   *upStatusDoc `json:"status"`
}

// upAccount, upStatusDoc and upStatusAccount are the account view and the
// status doc as this end decodes them: push (D7) and pollEvery are kept
// raw (the outer field shadows the embedded one), so a bad value there,
// of any type, drops that field for the account, never the event.
type upAccount struct {
	web.AccountView
	Push      json.RawMessage `json:"push"`
	PollEvery json.RawMessage `json:"pollEvery"`
}

type upStatusDoc struct {
	web.StatusDoc
	Accounts []upStatusAccount `json:"accounts"`
}

type upStatusAccount struct {
	web.StatusAccount
	Push json.RawMessage `json:"push"`
}

var epochRE = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// handshake takes the stream's first event, which must be a valid hello:
// its accounts all in the link's set (the one /peer/hello named), its
// epoch well formed. It becomes the daemon's state, and every page gets a
// new hello from it: a page that saw a write's outcome as unknown
// reconciles from its generation.
func (d *Daemon) handshake(u *upstream, ev sseEvent) error {
	if ev.over || ev.name != "hello" {
		return errHandshake
	}
	lh := d.up.Hello()
	if lh == nil {
		return errHandshake
	}
	// The link refused a hello naming anything but plain names; checked
	// again here, since everything below shows these as text.
	set := map[string]bool{}
	for _, a := range lh.Accounts {
		if !config.ValidName(a.Name) {
			return errHandshake
		}
		set[a.Name] = true
	}
	var h upstreamHello
	if json.Unmarshal(ev.data, &h) != nil || !epochRE.MatchString(h.Epoch) || len(h.Accounts) > maxAccounts {
		return errHandshake
	}
	accts := make([]web.AccountView, 0, len(h.Accounts))
	seen := map[string]bool{}
	for _, a := range h.Accounts {
		v, ok := cleanAccount(a, set)
		if !ok || seen[v.Name] {
			return errHandshake
		}
		seen[v.Name] = true
		accts = append(accts, v)
	}
	var status *web.StatusDoc
	if h.Status != nil {
		if st, ok := cleanStatus(*h.Status, set); ok {
			status = &st
		}
	}
	u.hello, u.set, u.epoch, u.gen = true, set, h.Epoch, h.Gen
	d.mu.Lock()
	d.live.epoch, d.live.gen, d.live.accounts = h.Epoch, h.Gen, accts
	if status != nil {
		d.live.status, d.live.statusAt = status, time.Now()
	}
	d.hub.Broadcast("hello", d.helloLocked())
	d.mu.Unlock()
	if status != nil {
		d.wakeStatus()
	}
	return nil
}

// take is one event after the handshake: allowlisted, decoded, held to
// shape, then applied and broadcast under the lock, or dropped.
func (d *Daemon) take(u *upstream, ev sseEvent) {
	if ev.over {
		d.drop(ev.name, "over its size cap")
		return
	}
	var payload any
	ok := false
	switch ev.name {
	case "syncing":
		var e web.SyncingEvent
		ok = decode(ev.data, &e) && u.set[e.Account]
		payload = web.SyncingEvent{Account: e.Account}
	case "sync":
		var e web.SyncEvent
		ok = decode(ev.data, &e) && u.set[e.Account] && validOp(e.Op) && sane(e.At)
		payload = web.SyncEvent{Account: e.Account, Op: e.Op, Changed: e.Changed, At: e.At.UTC()}
	case "auth":
		var e web.AuthEvent
		ok = decode(ev.data, &e) && u.set[e.Account]
		payload = web.AuthEvent{Account: e.Account, OK: e.OK, Error: plain(e.Error, maxEventText)}
	case "account":
		var e upAccount
		if decode(ev.data, &e) {
			payload, ok = cleanAccount(e, u.set)
		}
	case "status":
		var e upStatusDoc
		if decode(ev.data, &e) {
			payload, ok = cleanStatus(e, u.set)
		}
	case "view":
		var e web.ViewEvent
		if decode(ev.data, &e) && e.Epoch == u.epoch && e.Gen > u.gen {
			payload, ok = cleanView(e, u.set)
		}
	default:
		d.drop(ev.name, "not allowed")
		return
	}
	if !ok {
		d.drop(ev.name, "out of shape")
		return
	}
	if v, isView := payload.(web.ViewEvent); isView {
		u.gen = v.Gen
	}
	d.relay(ev.name, payload)
}

// relay applies an accepted event to the daemon's state and broadcasts it,
// together, under the lock /events subscribes under.
func (d *Daemon) relay(name string, payload any) {
	d.mu.Lock()
	status := false
	switch e := payload.(type) {
	case web.AccountView:
		d.setAccountLocked(e)
	case web.SyncingEvent:
		d.runningLocked(e.Account, true)
	case web.SyncEvent:
		if e.Op != string(gmi.OpPush) {
			d.runningLocked(e.Account, false)
		}
	case web.ViewEvent:
		d.live.gen = e.Gen
	case web.StatusDoc:
		d.live.status, d.live.statusAt = &e, time.Now()
		status = true
	}
	d.hub.Broadcast(name, payload)
	d.mu.Unlock()
	if status {
		d.wakeStatus()
	}
}

// setAccountLocked replaces an account's view, keeping hello's order.
func (d *Daemon) setAccountLocked(v web.AccountView) {
	accts := make([]web.AccountView, 0, len(d.live.accounts)+1)
	found := false
	for _, a := range d.live.accounts {
		if a.Name == v.Name {
			a, found = v, true
		}
		accts = append(accts, a)
	}
	if !found {
		accts = append(accts, v)
	}
	d.live.accounts = accts
}

// runningLocked applies `syncing` or a sync's end to the account's view,
// as app.js does, so a hello taken between them says what's running.
func (d *Daemon) runningLocked(account string, running bool) {
	for _, a := range d.live.accounts {
		if a.Name == account {
			a.Running = running
			if running {
				a.Queued = false
			}
			d.setAccountLocked(a)
			return
		}
	}
}

// ---- shapes -----------------------------------------------------------------

// decode is one event's JSON into its typed shape. Fields it doesn't
// know are dropped by the re-encode.
func decode(data []byte, v any) bool { return json.Unmarshal(data, v) == nil }

func validOp(op string) bool {
	switch gmi.Op(op) {
	case gmi.OpSync, gmi.OpPull, gmi.OpPush:
		return true
	}
	return false
}

func validState(s gmi.State) bool {
	switch s {
	case "", gmi.StateUnconfigured, gmi.StateUnauthorized, gmi.StateNeedsPull, gmi.StatePulling, gmi.StateReady, gmi.StateReauth:
		return true
	}
	return false
}

func validPhase(p gmi.Phase) bool {
	switch p {
	case "", gmi.PhaseListing, gmi.PhaseRemoving, gmi.PhaseContent, gmi.PhaseMetadata:
		return true
	}
	return false
}

var (
	windowRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	threadRE = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
)

// cleanAccount holds an account view to shape: a name in the set, enum
// states, counts not below zero, times re-formatted, its error plain, its
// push health and poll delay each held to shape or dropped.
func cleanAccount(u upAccount, set map[string]bool) (web.AccountView, bool) {
	a := u.AccountView
	if !set[a.Name] || !validState(a.State) {
		return web.AccountView{}, false
	}
	v := web.AccountView{Name: a.Name, State: a.State, Pulled: a.Pulled, Failures: max(a.Failures, 0),
		Authing: a.Authing, Queued: a.Queued, Running: a.Running, LastSync: cleanTime(a.LastSync),
		Push: cleanPush(u.Push, time.Now()), PollEvery: cleanPoll(u.PollEvery)}
	if a.Error != nil {
		e := plain(*a.Error, maxEventText)
		v.Error = &e
	}
	if p := a.Progress; p != nil {
		if !validPhase(p.Phase) {
			return web.AccountView{}, false
		}
		v.Progress = cleanProgress(p)
	}
	return v, true
}

func cleanProgress(p *web.ProgressView) *web.ProgressView {
	pv := &web.ProgressView{Phase: p.Phase, Done: max(p.Done, 0), Total: max(p.Total, 0), Listed: max(p.Listed, 0), Frontier: cleanTime(p.Frontier)}
	if p.Percent != nil && *p.Percent >= 0 && *p.Percent <= 100 {
		pct := *p.Percent
		pv.Percent = &pct
	}
	return pv
}

// cleanStatus holds a status doc to status.json's limits (N14): at most
// maxAccounts accounts, all in the set, maxSenders senders, every string
// plain and at most maxText runes.
func cleanStatus(s upStatusDoc, set map[string]bool) (web.StatusDoc, bool) {
	if len(s.Accounts) > maxAccounts || len(s.Senders) > maxSenders {
		return web.StatusDoc{}, false
	}
	out := web.StatusDoc{Version: 1, Updated: plain(s.Updated, maxText), Running: s.Running, Unread: max(s.Unread, 0),
		Senders: []string{}, Accounts: []web.StatusAccount{}}
	for _, n := range s.Senders {
		if n = plain(n, maxText); n != "" {
			out.Senders = append(out.Senders, n)
		}
	}
	now := time.Now()
	for _, u := range s.Accounts {
		a := u.StatusAccount
		if !set[a.Name] || !validState(a.State) {
			return web.StatusDoc{}, false
		}
		sa := web.StatusAccount{Name: a.Name, Unread: max(a.Unread, 0), Pulled: a.Pulled, LastSync: cleanTime(a.LastSync),
			Failures: max(a.Failures, 0), State: a.State, Push: cleanPush(u.Push, now)}
		if a.Error != nil {
			e := plain(*a.Error, maxText)
			sa.Error = &e
		}
		if a.Progress != nil {
			if !validPhase(a.Progress.Phase) {
				return web.StatusDoc{}, false
			}
			sa.Progress = cleanProgress(a.Progress)
		}
		out.Accounts = append(out.Accounts, sa)
	}
	return out, true
}

// MaxPollEvery bounds an account's poll delay in seconds: the engine's
// backoff caps at 15 minutes; a day is past anything it says.
const MaxPollEvery = 24 * 60 * 60

// cleanPoll is an account's pollEvery: whole seconds in [1, MaxPollEvery],
// else 0 (absent).
func cleanPoll(raw json.RawMessage) int {
	var n int
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || n < 1 || n > MaxPollEvery {
		return 0
	}
	return n
}

// deliveryRE is task 2's lastDelivery exactly: RFC 3339, UTC, to the second.
var deliveryRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// DeliverySkew is how far ahead of this machine's clock a push's last
// delivery may be: the server's clock may run ahead, not by a day.
const DeliverySkew = 24 * time.Hour

// cleanPush holds an account's push health to D7's shape, field by field:
// absent or null is off (nil); state a string from control.PushStates
// but off (absent is off); reason present exactly when the state takes
// one, from that state's closed list; lastDelivery, when present, RFC
// 3339 UTC to the second, from 2000 to a day past now. Anything else, of
// any type, null where a value belongs included, is nil: push dropped for
// the account, the rest of its view kept.
func cleanPush(raw json.RawMessage, now time.Time) *web.PushView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var p struct{ State, Reason, LastDelivery json.RawMessage }
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	state, ok := rawString(p.State)
	if !ok || state == control.PushOff || !slices.Contains(control.PushStates, state) {
		return nil
	}
	v := &web.PushView{State: state}
	var reasons []string
	switch state {
	case control.PushReauth:
		reasons = control.ReauthReasons
	case control.PushFailing:
		reasons = control.FailingReasons
	}
	if reasons == nil && p.Reason != nil {
		return nil
	}
	if reasons != nil {
		if v.Reason, ok = rawString(p.Reason); !ok || !slices.Contains(reasons, v.Reason) {
			return nil
		}
	}
	if p.LastDelivery != nil {
		s, ok := rawString(p.LastDelivery)
		t, err := time.Parse(time.RFC3339, s)
		if !ok || !deliveryRE.MatchString(s) || err != nil || t.Year() < 2000 || t.After(now.Add(DeliverySkew)) {
			return nil
		}
		at := t.UTC().Format(time.RFC3339)
		v.LastDelivery = &at
	}
	return v
}

// rawString is a JSON string's value; ok only for a string (never null).
func rawString(raw json.RawMessage) (string, bool) {
	var s *string
	if raw == nil || json.Unmarshal(raw, &s) != nil || s == nil {
		return "", false
	}
	return *s, true
}

// cleanView holds a view to shape: from a window id or nothing, threads
// bounded, each in a set account with a notmuch thread id.
func cleanView(e web.ViewEvent, set map[string]bool) (web.ViewEvent, bool) {
	if e.From != "" && !windowRE.MatchString(e.From) || len(e.Threads) > maxThreads {
		return web.ViewEvent{}, false
	}
	v := web.ViewEvent{Epoch: e.Epoch, Gen: e.Gen, From: e.From}
	for _, t := range e.Threads {
		if !set[t.Account] || !threadRE.MatchString(t.Thread) {
			return web.ViewEvent{}, false
		}
		v.Threads = append(v.Threads, web.ThreadRef{Account: t.Account, Thread: t.Thread})
	}
	return v, true
}

// cleanTime keeps an RFC 3339 time, re-formatted; anything else is null.
func cleanTime(s *string) *string {
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil || !sane(t) {
		return nil
	}
	f := t.UTC().Format(time.RFC3339)
	return &f
}

// sane is a time that stays a four-digit RFC 3339 year once in UTC: Go
// parses "0000-01-01T00:00:00+01:00", whose UTC is year -1, which
// MarshalJSON then refuses, failing the relay's encode outside the drop
// path's log limiter (H1).
func sane(t time.Time) bool {
	y := t.UTC().Year()
	return y >= 1 && y <= 9999
}

// plain is config.Plain: server text made safe to show as text.
func plain(s string, n int) string { return config.Plain(s, n) }

// oneWord keeps an upstream event name short and plain in the log.
func oneWord(s string) string {
	if len(s) > 32 || !utf8.ValidString(s) || strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return "?"
	}
	return s
}
