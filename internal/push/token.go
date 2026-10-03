package push

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/google"
)

// RefreshAhead: an access token is refreshed once less than this remains
// of it, by the wall clock (D5); a token given for less than twice that
// is refreshed halfway, so a short one is still used.
const RefreshAhead = 5 * time.Minute

// renewAt is when a token fetched at now, good until expiry, is refreshed.
func renewAt(now, expiry time.Time) time.Time {
	return expiry.Add(-min(RefreshAhead, expiry.Sub(now)/2))
}

var (
	// errReauth: the token's grant is dead (invalid_grant on refresh); a
	// consent is the only fix.
	errReauth = errors.New("push: the grant needs a new consent")
	// errStale: a wake came while this was in flight; its answer is
	// discarded.
	errStale = errors.New("push: answer from before a wake")
)

// ownerSource is the owner's access token, shared by every worker's
// pulls. One refresh runs at a time, whoever asked first; the rest wait
// for it. A token is good only in the epoch it was fetched in (a wake
// drops it), and a refresh's answer that lands after a wake is discarded.
type ownerSource struct {
	m       *Manager
	creds   google.Credentials
	refresh string
	sub     string
	ctx     context.Context // ends when this owner is replaced or the manager stops
	cancel  context.CancelFunc

	mu     sync.Mutex
	access string
	renew  time.Time // renewAt
	epoch  uint64
	dead   bool // invalid_grant: every worker is reauth/owner
	flight *flight
}

type flight struct {
	done   chan struct{}
	ep     uint64
	access string // set before done closes, or err
	err    error
}

func newOwnerSource(m *Manager, creds google.Credentials, refresh, sub string) *ownerSource {
	ctx, cancel := context.WithCancel(m.ctx)
	return &ownerSource{m: m, creds: creds, refresh: refresh, sub: sub, ctx: ctx, cancel: cancel}
}

// get is an access token good in epoch ep for at least RefreshAhead,
// refreshing (once, for everyone waiting) if needed. errReauth once the
// owner's grant is dead; errStale when a wake overtook the refresh.
func (o *ownerSource) get(ctx context.Context, ep uint64, ectx context.Context) (string, error) {
	for {
		if o.m.epochNow() != ep {
			return "", errStale
		}
		o.mu.Lock()
		if o.dead {
			o.mu.Unlock()
			return "", errReauth
		}
		if o.access != "" && o.epoch == ep && o.m.clock.Now().Before(o.renew) {
			a := o.access
			o.mu.Unlock()
			return a, nil
		}
		fl := o.flight
		if fl == nil {
			fl = &flight{done: make(chan struct{}), ep: ep}
			o.flight = fl
			o.m.flights.Add(1)
			go o.run(fl, ep, ectx)
		}
		o.mu.Unlock()
		select {
		case <-fl.done:
			if fl.err != nil {
				return "", fl.err
			}
			if fl.ep == ep {
				return fl.access, nil // however short its life
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// run is one refresh, bounded by this owner's context and ep's.
func (o *ownerSource) run(fl *flight, ep uint64, ectx context.Context) {
	defer o.m.flights.Done()
	ctx, done := ctxIn(o.ctx, ectx)
	tok, err := o.m.api.Refresh(ctx, o.creds, google.Owner, o.refresh, o.sub)
	o.m.after(google.OpRefresh)
	done()
	// Stored, or the grant marked dead, only with no wake since the
	// refresh began.
	dead := false
	if !o.m.inEpoch(ep, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		switch {
		case err == nil:
			o.access, o.renew, o.epoch = tok.Access, renewAt(o.m.clock.Now(), tok.Expiry), ep
			fl.access = tok.Access
		case google.CodeOf(err) == google.CodeInvalidGrant:
			o.dead, dead = true, true
			err = errReauth
		}
	}) {
		err = errStale
	}
	o.mu.Lock()
	fl.err = err
	o.flight = nil
	o.mu.Unlock()
	close(fl.done)
	if dead {
		o.m.ownerReauth(o)
	}
}

// drop forgets access if it's still the token held: the API refused it.
func (o *ownerSource) drop(access string) {
	o.mu.Lock()
	if o.access == access {
		o.access = ""
	}
	o.mu.Unlock()
}

// forget drops the token held (a wake).
func (o *ownerSource) forget() {
	o.mu.Lock()
	o.access = ""
	o.mu.Unlock()
}

func (o *ownerSource) isDead() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dead
}

// mailboxSource is one worker's mailbox token. Only its watch loop uses
// it, so it needs no lock and no shared refresh.
type mailboxSource struct {
	creds   google.Credentials
	refresh string
	access  string
	renew   time.Time // renewAt
	epoch   uint64
}

// get is mb's access token good in epoch ep, refreshed if needed;
// errReauth on invalid_grant, errStale when a wake overtook the refresh.
func (mb *mailboxSource) get(ctx context.Context, m *Manager, ep uint64) (string, error) {
	if mb.access != "" && mb.epoch == ep && m.clock.Now().Before(mb.renew) {
		return mb.access, nil
	}
	mb.access = ""
	tok, err := m.api.Refresh(ctx, mb.creds, google.Mailbox, mb.refresh, "")
	m.after(google.OpRefresh)
	switch {
	case m.epochNow() != ep:
		return "", errStale
	case google.CodeOf(err) == google.CodeInvalidGrant:
		return "", errReauth
	case err != nil:
		return "", err
	}
	mb.access, mb.renew, mb.epoch = tok.Access, renewAt(m.clock.Now(), tok.Expiry), ep
	return mb.access, nil
}

func (mb *mailboxSource) drop() { mb.access = "" }
