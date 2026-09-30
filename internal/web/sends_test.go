package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
)

// sendRecordOf reads the send log's record for the form's draft id.
func sendRecordOf(t *testing.T, s *Server, v url.Values) (sendRecord, bool) {
	t.Helper()
	rec, found, err := s.Sends.read(v.Get("message_id"))
	if err != nil {
		t.Fatal(err)
	}
	return rec, found
}

func setSend(fx tagFixture, out string, err error) {
	fx.sync.mu.Lock()
	fx.sync.sendOut, fx.sync.sendErr = out, err
	fx.sync.mu.Unlock()
}

// The reservation is on disk, durably, before gmi is handed the message.
func TestSendReservesFirst(t *testing.T) {
	fx := newTagFixture(t)
	v := replyForm(t, fx.s, "personal", cabin3, false).values
	var during sendRecord
	var found bool
	fx.sync.onSend = func() { during, found = sendRecordOf(t, fx.s, v) }
	if w := postSend(fx.s, v); w.Code != http.StatusSeeOther {
		t.Fatalf("send: %d", w.Code)
	}
	if !found || during.Result != "" || during.Account != "personal" || during.Hash == "" || during.Reserved.IsZero() {
		t.Fatalf("record during Send: %+v (found %v)", during, found)
	}
	after, _ := sendRecordOf(t, fx.s, v)
	if after.Result != sendAccepted || !strings.HasPrefix(after.Dest, "/t/personal/") || after.Hash != during.Hash {
		t.Errorf("record after: %+v", after)
	}
	// Same id, same content: the recorded answer, not a second send.
	w := postSend(fx.s, v)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != after.Dest+sentFragment || fx.sync.sends() != 1 {
		t.Errorf("replay: %d %q, %d sends", w.Code, w.Header().Get("Location"), fx.sync.sends())
	}
	// Same id, different content: refused.
	v.Set("body", "an edited body\n"+v.Get("body"))
	w = postSend(fx.s, v)
	if w.Code != http.StatusConflict || parseForm(t, w.Body.String()).err != msgChanged || fx.sync.sends() != 1 {
		t.Errorf("edited replay: %d, %d sends", w.Code, fx.sync.sends())
	}
	// So is the same content from the other account.
	v.Set("account", "work")
	if w := postSend(fx.s, v); w.Code != http.StatusConflict || fx.sync.sends() != 1 {
		t.Errorf("other account: %d, %d sends", w.Code, fx.sync.sends())
	}
}

// A reservation with no result (the server stopped mid-send) is unknown
// after a restart: never sent again, and said so.
func TestSendCrashIsUnknown(t *testing.T) {
	fx := newTagFixture(t)
	v := replyForm(t, fx.s, "personal", cabin3, false).values
	fx.sync.onSend = func() { panic(http.ErrAbortHandler) } // the process dies inside gmi send
	func() {
		defer func() { recover() }()
		postSend(fx.s, v)
	}()
	fx.sync.onSend = nil
	if rec, found := sendRecordOf(t, fx.s, v); !found || rec.Result != "" {
		t.Fatalf("after the crash: %+v (found %v)", rec, found)
	}
	// A new server on the same log.
	s2 := serverFor(t, fx.env.Accounts)
	s2.Syncer = fx.sync
	log, err := OpenSendLog(fx.s.Sends.dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.Sends = log
	w := postSend(s2, v)
	if w.Code != http.StatusConflict || parseForm(t, w.Body.String()).err != msgMaybeSent {
		t.Fatalf("after restart: %d %q", w.Code, w.Body)
	}
	if n := fx.sync.sends(); n != 0 { // the crash came before the fake counted one
		t.Errorf("%d sends after the restart", n)
	}
}

// Which failures free the draft id for another try (with any content),
// and which leave it unknown.
func TestSendFailureOutcomes(t *testing.T) {
	http400 := `sending message, from: x..` + "\n" + `googleapiclient.errors.HttpError: <HttpError 400 when requesting https://gmail.googleapis.com/gmail/v1/users/me/messages/send?alt=json returned "Invalid To header">`
	cases := []struct {
		name   string
		out    string
		err    error
		status int
		msg    string
		result string
		free   bool
	}{
		{"busy: gmi never started", "", fmt.Errorf("%w: %w", gmi.ErrBusy, context.DeadlineExceeded), 503, msgBusy, sendRejected, true},
		{"failed before the send line", "Traceback\ngoogle.auth.exceptions.RefreshError: invalid_grant", errors.New("gmi send [personal]: exit 1: invalid_grant"), 502, "Not sent:", sendRejected, true},
		// httplib2 resends a POST whose connection dropped, and lieer makes
		// more requests after the send: a 4xx after the send line proves nothing.
		{"4xx after the send line", http400, errors.New("gmi send [personal]: exit 1: HttpError 400"), 502, msgMaybeSent, sendUnknown, false},
		{"timed out mid-call", "sending message, from: x..\nTimeoutError: The read operation timed out", errors.New("gmi send [personal]: exit 1: TimeoutError"), 502, msgMaybeSent, sendUnknown, false},
		{"accepted, no local copy", "sending message, from: x..\nreceiving content (1) ...\nXapianError", errors.New("gmi send [personal]: exit 1: XapianError"), 502, msgSentNoCopy, sendNoCopy, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newTagFixture(t)
			v := replyForm(t, fx.s, "personal", cabin3, false).values
			setSend(fx, c.out, c.err)
			w := postSend(fx.s, v)
			if got := parseForm(t, w.Body.String()).err; w.Code != c.status || !strings.HasPrefix(got, c.msg) {
				t.Fatalf("%d %q", w.Code, got)
			}
			if rec, _ := sendRecordOf(t, fx.s, v); rec.Result != c.result {
				t.Errorf("recorded %q", rec.Result)
			}
			// The user edits and presses Send again; gmi would succeed now.
			setSend(fx, "", nil)
			v.Set("body", "edited\n"+v.Get("body"))
			w = postSend(fx.s, v)
			if c.free {
				if w.Code != http.StatusSeeOther || fx.sync.sends() != 2 {
					t.Errorf("retry: %d, %d sends", w.Code, fx.sync.sends())
				}
				return
			}
			if w.Code != http.StatusConflict || fx.sync.sends() != 1 {
				t.Errorf("retry: %d, %d sends", w.Code, fx.sync.sends())
			}
		})
	}
}

// A second submit while the first is still in gmi is refused at once.
func TestSendInFlight(t *testing.T) {
	fx := newTagFixture(t)
	v := replyForm(t, fx.s, "personal", cabin3, false).values
	started, release := make(chan struct{}), make(chan struct{})
	fx.sync.onSend = func() { close(started); <-release }
	done := make(chan int)
	go func() { done <- postSend(fx.s, v).Code }()
	<-started
	w := postSend(fx.s, v)
	if w.Code != http.StatusConflict || parseForm(t, w.Body.String()).err != msgSending {
		t.Errorf("second submit: %d", w.Code)
	}
	close(release)
	if code := <-done; code != http.StatusSeeOther {
		t.Errorf("first submit: %d", code)
	}
	if n := fx.sync.sends(); n != 1 {
		t.Errorf("%d sends", n)
	}
}

// No send log, no send: never a send nothing can deduplicate.
func TestSendNeedsLog(t *testing.T) {
	fx := newTagFixture(t)
	fx.s.Sends = nil
	v := replyForm(t, fx.s, "personal", cabin3, false).values
	if w := postSend(fx.s, v); w.Code != http.StatusServiceUnavailable || fx.sync.sends() != 0 {
		t.Errorf("%d, %d sends", w.Code, fx.sync.sends())
	}
}

func TestSendLogFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sends")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := OpenSendLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	id := "<0123456789abcdef0123456789abcdef@pneu.test>"
	rec, fresh, err := l.reserve(id, "personal", "h1")
	if err != nil || !fresh {
		t.Fatalf("reserve: %v %v", fresh, err)
	}
	l.record(rec, sendAccepted, "/")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("files %v", entries)
	}
	fi, _ := entries[0].Info()
	if fi.Mode().Perm() != 0o600 || strings.Contains(entries[0].Name(), "pneu.test") {
		t.Errorf("%s: mode %v", entries[0].Name(), fi.Mode().Perm())
	}
	// A record nobody can read is not a free id.
	if err := os.WriteFile(l.path(id), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.reserve(id, "personal", "h1"); err == nil {
		t.Error("reserved over an unreadable record")
	}
	// A symlink where the directory should be is refused.
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(dir, link)
	if _, err := OpenSendLog(link); err == nil {
		t.Error("opened a symlinked log")
	}
}

func TestSendLogPrune(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenSendLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ids := map[string]time.Duration{
		"<00000000000000000000000000000001@pneu.test>": SendKeep + time.Hour,   // expired
		"<00000000000000000000000000000002@pneu.test>": SendKeep - time.Hour,   // kept
		"<00000000000000000000000000000003@pneu.test>": 2*SendKeep + time.Hour, // expired, unknown
	}
	for id, age := range ids {
		l.now = func() time.Time { return now.Add(-age) }
		if _, _, err := l.reserve(id, "personal", "h"); err != nil {
			t.Fatal(err)
		}
		l.release(id)
	}
	oldTmp := filepath.Join(dir, ".tmp-old")
	newTmp := filepath.Join(dir, ".tmp-new")
	for _, p := range []string{oldTmp, newTmp} {
		os.WriteFile(p, nil, 0o600)
	}
	os.Chtimes(oldTmp, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	l.now = func() time.Time { return now }
	l.Prune()
	for id, age := range ids {
		_, found, _ := l.read(id)
		if found != (age < SendKeep) {
			t.Errorf("%s (age %v): found %v", id, age, found)
		}
	}
	if _, err := os.Stat(oldTmp); err == nil {
		t.Error("stale temp file kept")
	}
	if _, err := os.Stat(newTmp); err != nil {
		t.Error("a write in progress was pruned")
	}
	// reserve prunes on its way past once a day.
	l.now = func() time.Time { return now.Add(SendKeep + 2*time.Hour) }
	if _, _, err := l.reserve("<00000000000000000000000000000004@pneu.test>", "personal", "h"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := l.read("<00000000000000000000000000000002@pneu.test>"); found {
		t.Error("reserve didn't prune")
	}
}

// Prune's check and removal hold the files lock: a reserve that replaces
// an expired record in between waits, and its fresh reservation survives.
func TestSendLogPruneRace(t *testing.T) {
	l, err := OpenSendLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "<00000000000000000000000000000009@pneu.test>"
	now := time.Now()
	l.now = func() time.Time { return now.Add(-SendKeep - time.Hour) }
	rec, _, err := l.reserve(id, "personal", "h")
	if err != nil {
		t.Fatal(err)
	}
	l.record(rec, sendRejected, "") // expired, and free
	l.now = func() time.Time { return now }
	l.lastPrune = now // reserve doesn't prune on its own here

	done := make(chan bool)
	l.beforeRemove = func(string) {
		go func() {
			_, fresh, err := l.reserve(id, "personal", "h2")
			done <- fresh && err == nil
		}()
		select {
		case <-done:
			t.Error("reserve ran inside Prune's check-and-remove")
		case <-time.After(50 * time.Millisecond):
		}
	}
	l.Prune()
	if !<-done {
		t.Fatal("reserve after the prune failed")
	}
	got, found, err := l.read(id)
	if err != nil || !found || got.Hash != "h2" || got.Result != "" {
		t.Errorf("fresh reservation lost: %+v %v %v", got, found, err)
	}
}

// Retention runs on the last submit: a replay refreshes it on disk.
func TestSendLogReplayRefreshes(t *testing.T) {
	l, err := OpenSendLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "<0000000000000000000000000000000a@pneu.test>"
	now := time.Now()
	l.now = func() time.Time { return now.Add(-SendKeep + time.Hour) }
	rec, _, _ := l.reserve(id, "personal", "h")
	l.record(rec, sendUnknown, "")
	l.now = func() time.Time { return now }
	if _, fresh, err := l.reserve(id, "personal", "h"); fresh || err != nil {
		t.Fatalf("replay: fresh %v %v", fresh, err)
	}
	l.release(id)
	l.now = func() time.Time { return now.Add(2 * time.Hour) } // past the first submit's 30 days
	l.Prune()
	got, found, _ := l.read(id)
	if !found || !got.Submitted.Equal(now.UTC()) || got.Result != sendUnknown {
		t.Errorf("record %+v found %v", got, found)
	}
}

// A replay whose refresh can't be written still answers from the record,
// and Prune keeps the record and retries the refresh until it lands.
func TestSendLogRefreshFailure(t *testing.T) {
	l, err := OpenSendLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "<0000000000000000000000000000000b@pneu.test>"
	now := time.Now()
	l.now = func() time.Time { return now.Add(-SendKeep + time.Hour) }
	rec, _, _ := l.reserve(id, "personal", "h")
	l.record(rec, sendUnknown, "")

	l.now = func() time.Time { return now }
	l.writeErr = func(sendRecord) error { return errors.New("no space left on device") }
	got, fresh, err := l.reserve(id, "personal", "h")
	if fresh || err != nil || got.Result != sendUnknown {
		t.Fatalf("replay: %+v fresh %v %v", got, fresh, err)
	}
	l.release(id)

	// Past the old deadline, the write still failing: kept.
	l.now = func() time.Time { return now.Add(2 * time.Hour) }
	l.Prune()
	if _, found, _ := l.read(id); !found {
		t.Fatal("pinned record pruned")
	}
	// The disk recovers: the next pass writes the refresh and unpins.
	l.writeErr = nil
	l.Prune()
	disk, found, _ := l.read(id)
	if !found || !disk.Submitted.Equal(now.UTC()) {
		t.Errorf("refresh not written: %+v", disk)
	}
	if len(l.pinned) != 0 {
		t.Errorf("still pinned: %v", l.pinned)
	}
}

// A refresh that failed after its rename is visible but not durable: the
// retry must write it again rather than take the visible time as done.
func TestSendLogRefreshSyncFailure(t *testing.T) {
	l, err := OpenSendLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "<0000000000000000000000000000000c@pneu.test>"
	now := time.Now()
	l.now = func() time.Time { return now.Add(-SendKeep + time.Hour) }
	rec, _, _ := l.reserve(id, "personal", "h")
	l.record(rec, sendUnknown, "")

	l.now = func() time.Time { return now }
	l.syncErr = func() error { return errors.New("input/output error") }
	if _, fresh, err := l.reserve(id, "personal", "h"); fresh || err != nil {
		t.Fatalf("replay: fresh %v %v", fresh, err)
	}
	l.release(id)
	if disk, _, _ := l.read(id); !disk.Submitted.Equal(now.UTC()) {
		t.Fatalf("the rename should have landed: %+v", disk)
	}
	if len(l.pinned) != 1 {
		t.Fatalf("not pinned: %v", l.pinned)
	}

	// Still failing: the retry writes, fails, stays pinned.
	l.Prune()
	if len(l.pinned) != 1 {
		t.Fatalf("unpinned without a durable write: %v", l.pinned)
	}
	// Recovered: one more write, then unpinned.
	writes := 0
	l.syncErr = func() error { writes++; return nil }
	l.Prune()
	if writes == 0 || len(l.pinned) != 0 {
		t.Errorf("writes %d pinned %v", writes, l.pinned)
	}
}
