package web

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"testing"
)

var dataOldestRE = regexp.MustCompile(`<main class="list" data-view="[a-z]+" data-paged data-oldest="(\d+)">`)

func oldestAt(t *testing.T, s *Server, path string) int64 {
	t.Helper()
	m := dataOldestRE.FindStringSubmatch(getOK(t, s, path))
	if m == nil {
		t.Fatalf("%s: no data-oldest on a paged list", path)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// oldestOf is what viewOldest should say: the oldest match over every
// account, asked of notmuch directly.
func oldestOf(t *testing.T, s *Server, query string) int64 {
	t.Helper()
	var n int64
	for _, a := range s.Accounts {
		ts, err := a.OldestMatch(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if !ts.IsZero() && (n == 0 || ts.Unix() < n) {
			n = ts.Unix()
		}
	}
	return n
}

// A paged list carries its view's oldest date over every account, on every
// page; an unpaged one never asks. A write that changes what lists show (a
// new generation) is seen by the next page.
func TestListOldest(t *testing.T) {
	s := newServer(t)
	// One page: nothing asked, nothing cached.
	getOK(t, s, "/")
	if _, ok := s.oldest.m["tag:inbox"]; ok {
		t.Fatal("an unpaged list asked for its oldest date")
	}
	s.PerPage = 5
	want := oldestOf(t, s, "tag:inbox")
	if want == 0 {
		t.Fatal("the fixture's inbox has no dated mail")
	}
	for _, p := range []string{"/", "/?page=1", "/?page=3"} {
		if got := oldestAt(t, s, p); got != want {
			t.Errorf("%s: data-oldest %d, want %d", p, got, want)
		}
	}
	if s.oldest.at != s.viewLabel() || s.oldest.m["tag:inbox"] != want {
		t.Errorf("cached %+v at %+v, generation %+v", s.oldest.m, s.oldest.at, s.viewLabel())
	}
	// The view's oldest thread leaves the inbox: the far end moves.
	last := rows(t, s, "/?page=3")
	oldest := last[len(last)-1]
	tagOK(t, s, form("action", "archive", "account", oldest.Account, "ids", esc(oldest.ThreadIDs...)))
	after := oldestOf(t, s, "tag:inbox")
	if after <= want {
		t.Fatalf("archiving the oldest inbox thread left the oldest at %d (was %d)", after, want)
	}
	if got := oldestAt(t, s, "/"); got != after {
		t.Errorf("after an archive: data-oldest %d, want %d", got, after)
	}
	// Each view asks its own query.
	if got, want := oldestAt(t, s, "/all"), oldestOf(t, s, "not tag:spam and not tag:trash"); got != want {
		t.Errorf("/all: data-oldest %d, want %d", got, want)
	}
}

// Search results carry the search query's oldest; searches are cached by
// query, at most oldestSearches of them, the fixed views always kept.
func TestSearchOldest(t *testing.T) {
	s := newServer(t)
	s.PerPage = 1
	if got, want := oldestAt(t, s, "/search?q=from%3Aowen"), oldestOf(t, s, "from:owen"); got != want || want == 0 {
		t.Errorf("search: data-oldest %d, want %d", got, want)
	}
	oldestAt(t, s, "/?page=1") // a fixed view, cached alongside
	ctx := context.Background()
	at := s.viewLabel()
	for i := range oldestSearches + 3 {
		s.viewOldest(ctx, at, fmt.Sprintf("tag:inbox and not id:x%d@example.com", i), true)
	}
	if n := len(s.oldest.searches); n != oldestSearches {
		t.Errorf("%d searches kept, want %d", n, oldestSearches)
	}
	if len(s.oldest.m) != oldestSearches+1 {
		t.Errorf("%d queries cached, want %d searches and the inbox", len(s.oldest.m), oldestSearches)
	}
	if _, ok := s.oldest.m["from:owen"]; ok {
		t.Error("the first search outlived the bound")
	}
	if _, ok := s.oldest.m["tag:inbox"]; !ok {
		t.Error("a fixed view was evicted")
	}
}

// A failing account makes the date unknown, and unknown isn't kept: the
// track hides, and the next render asks again.
func TestOldestUnknown(t *testing.T) {
	s := newServer(t)
	at := s.viewLabel()
	if n := s.viewOldest(context.Background(), at, "date:notadate", true); n != 0 {
		t.Errorf("a failing query: %d", n)
	}
	if _, ok := s.oldest.m["date:notadate"]; ok {
		t.Error("an unknown date was cached")
	}
	// Nothing matches: none, which is an answer, and cached.
	if n := s.viewOldest(context.Background(), at, "tag:nosuchtag", false); n != 0 {
		t.Errorf("no match: %d", n)
	}
	if _, ok := s.oldest.m["tag:nosuchtag"]; !ok {
		t.Error("no match wasn't cached")
	}
}

// A request from an older generation that finishes late never replaces
// a newer generation's entries.
func TestOldestKeepsNewer(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	old := s.viewLabel()
	newer := viewLabel{old.Epoch, old.Gen + 1}
	want := s.viewOldest(ctx, newer, "tag:inbox", false)
	s.viewOldest(ctx, old, "tag:sent", false)
	if s.oldest.at != newer {
		t.Fatalf("cache at %+v, want %+v", s.oldest.at, newer)
	}
	if n, ok := s.oldest.m["tag:inbox"]; !ok || n != want {
		t.Error("an older generation's answer replaced the newer entries")
	}
	if _, ok := s.oldest.m["tag:sent"]; ok {
		t.Error("an older generation's answer was cached")
	}
}
