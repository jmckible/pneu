package push

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/google"
)

// RefreshAhead: an access token is refreshed once less than this remains
// of it, by the wall clock (D5).
const RefreshAhead = 5 * time.Minute

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
	expiry time.Time
	epoch  uint64
	dead   bool // invalid_grant: every worker is reauth/owner
	flight *flight
}

type flight struct {
	done chan struct{}
	err  error // set before done closes
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
		if o.access != "" && o.epoch == ep && o.m.clock.Now().Before(o.expiry.Add(-RefreshAhead)) {
			a := o.access
			o.mu.Unlock()
			return a, nil
		}
		fl := o.flight
		if fl == nil {
			fl = &flight{done: make(chan struct{})}
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
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// run is one refresh, bounded by this owner's context and ep's.
func (o *ownerSource) run(fl *flight, ep uint64, ectx context.Context) {
	defer o.m.flights.Done()
	ctx, cancel := context.WithCancel(o.ctx)
	stop := context.AfterFunc(ectx, cancel)
	tok, err := o.m.api.Refresh(ctx, o.creds, google.Owner, o.refresh, o.sub)
	o.m.after(google.OpRefresh)
	stop()
	cancel()
	o.mu.Lock()
	switch {
	case o.m.epochNow() != ep:
		err = errStale
	case err == nil:
		o.access, o.expiry, o.epoch = tok.Access, tok.Expiry, ep
	case google.CodeOf(err) == google.CodeInvalidGrant:
		o.dead = true
		err = errReauth
	}
	fl.err = err
	o.flight = nil
	o.mu.Unlock()
	close(fl.done)
	if err == errReauth {
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
	expiry  time.Time
	epoch   uint64
}

// get is mb's access token good in epoch ep, refreshed if needed;
// errReauth on invalid_grant, errStale when a wake overtook the refresh.
func (mb *mailboxSource) get(ctx context.Context, m *Manager, ep uint64) (string, error) {
	if mb.access != "" && mb.epoch == ep && m.clock.Now().Before(mb.expiry.Add(-RefreshAhead)) {
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
	mb.access, mb.expiry, mb.epoch = tok.Access, tok.Expiry, ep
	return mb.access, nil
}

func (mb *mailboxSource) drop() { mb.access = "" }
