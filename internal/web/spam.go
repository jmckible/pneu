package web

import (
	"context"
	"log"
	"sync"
)

// The Spam station's light (SPEC.md "Layout"): the tube shows no counts,
// but the Spam stop glows when spam has arrived since the user last
// looked. The server says only when the newest spam is dated (Page.SpamAt,
// the Spam link's data-spam); app.js compares that with the last visit it
// keeps in localStorage. No server state, nothing per window.

// spamCache is the newest spam date as of one view generation. Every
// write that changes what a list shows, a sync that pulled mail included,
// bumps the generation, so a page rendered at the cached label reuses it
// and only the first page after a change asks notmuch: one search per
// account.
type spamCache struct {
	mu sync.Mutex
	at viewLabel
	ok bool
	n  int64
}

// spamNewest is the newest spam message's date over every account, unix
// seconds, 0 for none. at is the page's label, read before this: a write
// racing the search leaves an entry newer than its label, never older. An
// account that fails counts as having none; the dot is a hint, and the
// next generation asks again.
func (s *Server) spamNewest(ctx context.Context, at viewLabel) int64 {
	s.spam.mu.Lock()
	if s.spam.ok && s.spam.at == at {
		n := s.spam.n
		s.spam.mu.Unlock()
		return n
	}
	s.spam.mu.Unlock()
	ns := make([]int64, len(s.Accounts))
	var wg sync.WaitGroup
	for i, a := range s.Accounts {
		wg.Go(func() {
			t, err := a.Newest(ctx, "tag:spam")
			if err != nil {
				log.Printf("newest spam %s: %v", a.Name, err)
				return
			}
			if !t.IsZero() {
				ns[i] = t.Unix()
			}
		})
	}
	wg.Wait()
	var n int64
	for _, v := range ns {
		n = max(n, v)
	}
	if ctx.Err() != nil {
		return n // a cancelled search may have missed an account: don't keep it
	}
	s.spam.mu.Lock()
	s.spam.at, s.spam.ok, s.spam.n = at, true, n
	s.spam.mu.Unlock()
	return n
}
