package web

import (
	"context"
	"log"
	"slices"
	"sync"
)

// The pager row's timeline (SPEC.md "Index views"): its track runs from
// now back to the view's oldest message, so a paged list carries that date
// (listPage.Oldest, main.list's data-oldest). Only paged lists ask; an
// unpaged one has no track.

// oldestSearches bounds the search queries the cache keeps: the six fixed
// views are always kept, the searches first in, first out.
const oldestSearches = 16

// oldestCache is each list query's oldest message date as of one view
// generation, the way spamCache keeps the newest spam: every write that
// changes what a list shows, a sync that pulled mail included, bumps the
// generation, so only the first paged render after a change asks notmuch,
// once per account.
type oldestCache struct {
	mu       sync.Mutex
	at       viewLabel
	m        map[string]int64 // query -> unix seconds, 0 for none
	searches []string         // the search queries in m, oldest first
}

// viewOldest is the oldest message query matches over every account, unix
// seconds, 0 for none or unknown. at is the page's label, read before
// this: a write racing the search leaves an entry newer than its label,
// never older. An account that fails makes the whole answer unknown (and
// uncached): the far end of the track would be a guess, and the next
// render asks again. search marks a search query, which the bound applies
// to.
func (s *Server) viewOldest(ctx context.Context, at viewLabel, query string, search bool) int64 {
	c := &s.oldest
	c.mu.Lock()
	if c.at == at {
		if n, ok := c.m[query]; ok {
			c.mu.Unlock()
			return n
		}
	}
	c.mu.Unlock()
	ns := make([]int64, len(s.Accounts))
	failed := make([]bool, len(s.Accounts))
	var wg sync.WaitGroup
	for i, a := range s.Accounts {
		wg.Go(func() {
			t, err := a.OldestMatch(ctx, query)
			if err != nil {
				log.Printf("oldest %q %s: %v", query, a.Name, err)
				failed[i] = true
				return
			}
			if !t.IsZero() {
				ns[i] = t.Unix()
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil || slices.Contains(failed, true) {
		return 0
	}
	var n int64
	for _, v := range ns {
		if v > 0 && (n == 0 || v < n) {
			n = v
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at != at || c.m == nil {
		c.at, c.m, c.searches = at, map[string]int64{}, nil
	}
	if _, had := c.m[query]; !had && search {
		c.searches = append(c.searches, query)
		if len(c.searches) > oldestSearches {
			delete(c.m, c.searches[0])
			c.searches = slices.Delete(c.searches, 0, 1)
		}
	}
	c.m[query] = n
	return n
}
