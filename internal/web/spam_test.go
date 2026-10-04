package web

import (
	"context"
	"regexp"
	"strconv"
	"testing"
)

var dataSpamRE = regexp.MustCompile(`<a href="/spam" data-station="5"[^>]* data-spam="(\d+)">`)

func spamAt(t *testing.T, s *Server, path string) int64 {
	t.Helper()
	m := dataSpamRE.FindStringSubmatch(getOK(t, s, path))
	if m == nil {
		t.Fatalf("%s: no data-spam on the Spam station", path)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// The Spam station carries the newest spam's date over every account, on
// every page, and a write that changes what lists show (a new generation)
// is seen by the next page.
func TestSpamStation(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	newest := func() int64 {
		var n int64
		for _, a := range s.Accounts {
			ts, err := a.Newest(ctx, "tag:spam")
			if err != nil {
				t.Fatal(err)
			}
			if !ts.IsZero() {
				n = max(n, ts.Unix())
			}
		}
		return n
	}
	want := newest()
	if want == 0 {
		t.Fatal("the fixture has no spam")
	}
	for _, p := range []string{"/", "/spam", "/compose", rows(t, s, "/")[0].URL} {
		if got := spamAt(t, s, p); got != want {
			t.Errorf("%s: data-spam %d, want %d", p, got, want)
		}
	}
	// The newest inbox thread becomes spam: newer than the fixture's.
	first := rows(t, s, "/")[0]
	tagOK(t, s, form("action", "spam", "account", first.Account, "ids", esc(first.ThreadIDs...)))
	after := newest()
	if after <= want {
		t.Fatalf("spamming the newest inbox thread left newest spam at %d (was %d)", after, want)
	}
	if got := spamAt(t, s, "/"); got != after {
		t.Errorf("after a spam write: data-spam %d, want %d", got, after)
	}
	if s.spam.at != s.viewLabel() {
		t.Errorf("cached at %+v, generation %+v", s.spam.at, s.viewLabel())
	}
}
