package web

// The view generation (docs/client.md "Events" and "Changes from other
// windows"): (epoch, gen) names what the mail database shows. Every write
// that changes a list or a thread bumps gen and broadcasts `view`; every
// page is labeled with the pair it was rendered at; every /events stream
// opens with `hello`, the pair and the account views as of subscribing. A
// page that missed events (a second window, an SSE reconnect, a stale tab)
// compares labels and catches up.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
)

// viewGen is the generation. mu spans each bump and its broadcast, so
// `view` events go out in gen order, and each /events subscription with its
// snapshot, so a stream never carries an event its hello already counts.
type viewGen struct {
	mu    sync.Mutex
	epoch string // random per server start: a restart can't reuse a gen
	gen   uint64
}

func newEpoch() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// viewLabel is a page's or a write's place in the generation.
type viewLabel struct {
	Epoch string `json:"epoch"`
	Gen   uint64 `json:"gen"`
}

// ThreadRef is one thread a write touched.
type ThreadRef struct {
	Account string `json:"account"`
	Thread  string `json:"thread"`
}

// ViewEvent is SSE `view`. No threads means anything may have changed (a
// sync). From is the writing page's X-Pneu-Window: a hint for that page to
// skip its own refresh, never an authority for anything.
type ViewEvent struct {
	Epoch   string      `json:"epoch"`
	Gen     uint64      `json:"gen"`
	From    string      `json:"from"`
	Threads []ThreadRef `json:"threads,omitempty"`
}

// HelloEvent is the first event on every /events stream. Status is the
// last status doc broadcast, null before the first.
type HelloEvent struct {
	Epoch    string        `json:"epoch"`
	Gen      uint64        `json:"gen"`
	Accounts []AccountView `json:"accounts"`
	Status   *StatusDoc    `json:"status"`
}

// viewLabel reads the generation. A page reads it before its notmuch query:
// a write racing the query then makes the page look older than it is, so it
// reloads once too often, never once too few.
func (s *Server) viewLabel() viewLabel {
	s.view.mu.Lock()
	l := viewLabel{s.view.epoch, s.view.gen}
	s.view.mu.Unlock()
	if s.onLabel != nil {
		s.onLabel()
	}
	return l
}

// ViewChanged is a sync that changed the database: every page reloads its
// list and open thread.
func (s *Server) ViewChanged() { s.viewChanged("", nil) }

// viewChanged bumps the generation and broadcasts `view`, returning the new
// label (the writing page's response carries it).
func (s *Server) viewChanged(from string, threads []ThreadRef) viewLabel {
	s.view.mu.Lock()
	defer s.view.mu.Unlock()
	s.view.gen++
	s.Hub.Broadcast("view", ViewEvent{Epoch: s.view.epoch, Gen: s.view.gen, From: from, Threads: threads})
	return viewLabel{s.view.epoch, s.view.gen}
}

// windowIDRE is app.js's window id: 16 random bytes in hex.
var windowIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// windowFrom is the request's X-Pneu-Window if well formed, else "". It is
// echoed as `from` and nothing else: any page can set the header, so it may
// only ever suppress a refresh in the page that claims it.
func windowFrom(r *http.Request) string {
	if v := r.Header.Get("X-Pneu-Window"); windowIDRE.MatchString(v) {
		return v
	}
	return ""
}

// threadLookup bounds threadsOf's query. It runs after the write, which
// has outlived its request, so it doesn't use the request's context either.
const threadLookup = 5 * time.Second

// threadsOf is the threads holding ids, for `view`. On failure it is nil,
// which pages read as "anything may have changed": a missed thread would be
// a stale page, an extra one only a refetch.
func (s *Server) threadsOf(acct notmuch.Account, ids []string) []ThreadRef {
	ctx, cancel := context.WithTimeout(context.Background(), threadLookup)
	defer cancel()
	var out []ThreadRef
	seen := map[string]bool{}
	for _, c := range chunkIDs(ids) {
		got, err := acct.Threads(ctx, notmuch.IDsQuery(c))
		if err != nil {
			return nil
		}
		for _, t := range got {
			if !seen[t] {
				seen[t] = true
				out = append(out, ThreadRef{acct.Name, t})
			}
		}
	}
	return out
}

// subscribe registers a stream and takes its hello. marks.mu (AccountChanged's
// read-and-broadcast), view.mu (every bump) and pubStatus.mu (every status
// broadcast) are held across both, so every `account`, `view` and `status`
// event either went out before the subscription, and hello's snapshot
// includes it, or comes after hello on the stream: none is queued with
// state older than the snapshot. The Hub needs no gen filter.
func (s *Server) subscribe() (chan []byte, HelloEvent) {
	s.marks.mu.Lock()
	defer s.marks.mu.Unlock()
	s.view.mu.Lock()
	defer s.view.mu.Unlock()
	s.pubStatus.mu.Lock()
	defer s.pubStatus.mu.Unlock()
	c := s.Hub.Subscribe()
	accts := s.accountViews()
	if accts == nil {
		accts = []AccountView{}
	}
	return c, HelloEvent{Epoch: s.view.epoch, Gen: s.view.gen, Accounts: accts, Status: s.pubStatus.last}
}

// events handles GET /events: hello, then the Hub's broadcasts.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	c, hello := s.subscribe()
	s.Hub.Serve(w, r, c, EncodeEvent("hello", hello))
}
