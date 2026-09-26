package gmi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/testmail"
)

const (
	touchKitchen1 = "0100019a7c3e-kitchen-1@delgadobuild.example"
	touchKitchen2 = "0100019a9d11-kitchen-2@delgadobuild.example"
)

// logSink collects Logf lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) find(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

// revision is the database's lastmod revision (`notmuch count --lastmod`).
func revision(t *testing.T, a testmail.Account) int {
	t.Helper()
	f := strings.Fields(string(a.Notmuch(t, "count", "--lastmod", "*")))
	if len(f) != 3 {
		t.Fatalf("count --lastmod: %q", f)
	}
	n, err := strconv.Atoi(f[2])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// changedSince lists messages whose lastmod is above rev: what lieer would
// push if its stored lastmod were rev.
func changedSince(t *testing.T, a testmail.Account, rev int) []string {
	t.Helper()
	var ids []string
	out := a.Notmuch(t, "search", "--format=json", "--output=messages", "--exclude=false", "--", "lastmod:"+strconv.Itoa(rev+1)+"..")
	if err := json.Unmarshal(out, &ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

// touchFixture is a fake-gmi engine over a real notmuch database.
func touchFixture(t *testing.T, sleep string) (*fixture, testmail.Account, *Engine, *logSink) {
	t.Helper()
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", sleep)
	env := testmail.Setup(t)
	p := env.Account(t, "personal")
	a := f.account("personal")
	a.NotmuchConfig = p.NotmuchConfig
	logs := &logSink{}
	o := f.opts(Options{})
	o.Logf = logs.logf
	return f, p, mustNew(t, []Account{a}, o), logs
}

// A tag written while a sync runs sits below the lastmod a full pull stores
// at its end; the engine must bump it (two batches) and push afterwards.
func TestNoteWriteDuringSyncTouchesAndPushes(t *testing.T) {
	f, p, e, logs := touchFixture(t, "0.6")
	start(t, e)
	waitFor(t, 2*time.Second, "sync start", func() bool { return f.count("sync") == 1 })

	// The tag handler's write, then its NoteWrite, while gmi sync runs.
	p.Notmuch(t, "tag", "-inbox", "--", testmail.QuoteID(touchKitchen1))
	e.NoteWrite("personal", []string{touchKitchen1})
	// lieer's full pull ends by storing the revision as of now.
	stored := revision(t, p)

	waitFor(t, 3*time.Second, "push after sync", func() bool { return f.count("push") == 1 })
	rs := f.runs()
	if len(rs) != 2 || rs[0].op != "sync" || rs[1].op != "push" || rs[1].start.Before(rs[0].end) {
		t.Fatalf("runs = %+v, want sync then push", rs)
	}
	if got := changedSince(t, p, stored); !slices.Equal(got, []string{touchKitchen1}) {
		t.Fatalf("above the stored lastmod: %q, want only the written message", got)
	}
	if rev := revision(t, p); rev < stored+2 {
		t.Errorf("revision %d -> %d: want two touch batches", stored, rev)
	}
	var tags []string
	if err := json.Unmarshal(p.Notmuch(t, "search", "--format=json", "--output=tags", "--", testmail.QuoteID(touchKitchen1)), &tags); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(tags, TouchTag) || slices.Contains(tags, "inbox") {
		t.Errorf("tags after touch: %q", tags)
	}
	if l := logs.find("re-marked 1 message(s)"); len(l) != 1 {
		t.Errorf("log lines %q", logs.lines)
	}
}

// Outside a sync a write is already above lastmod: no touch, no extra push.
func TestNoteWriteOutsideSyncIgnored(t *testing.T) {
	saved := noteGrace
	noteGrace = 20 * time.Millisecond
	t.Cleanup(func() { noteGrace = saved })
	f, p, e, logs := touchFixture(t, "0")

	// Before any sync has run.
	e.NoteWrite("personal", []string{touchKitchen2})
	start(t, e)
	waitFor(t, 2*time.Second, "initial sync", func() bool { st, _ := e.Status("personal"); return !st.LastSync.IsZero() })
	time.Sleep(50 * time.Millisecond) // past the grace

	before := revision(t, p)
	e.NoteWrite("personal", []string{touchKitchen1})
	time.Sleep(200 * time.Millisecond)
	if rev := revision(t, p); rev != before {
		t.Errorf("revision moved %d -> %d without a sync", before, rev)
	}
	if n := f.count("push"); n != 0 {
		t.Errorf("%d pushes", n)
	}
	if l := logs.find("re-marked"); len(l) != 0 {
		t.Errorf("touched: %q", l)
	}
}

// A NoteWrite that lands just after gmi exited (the write finished inside
// the sync, the call came after) is still re-marked, via the loop.
func TestNoteWriteJustAfterSyncTouches(t *testing.T) {
	f, p, e, logs := touchFixture(t, "0")
	start(t, e)
	waitFor(t, 2*time.Second, "initial sync", func() bool { st, _ := e.Status("personal"); return !st.LastSync.IsZero() })
	before := revision(t, p)
	e.NoteWrite("personal", []string{touchKitchen2})
	waitFor(t, 3*time.Second, "push", func() bool { return f.count("push") == 1 })
	if got := changedSince(t, p, before); !slices.Equal(got, []string{touchKitchen2}) {
		t.Errorf("changed since: %q", got)
	}
	if l := logs.find("re-marked 1 message(s)"); len(l) != 1 {
		t.Errorf("log lines %q", logs.lines)
	}
}

func TestSendBusyIsDistinguishable(t *testing.T) {
	f := newFixture(t)
	t.Setenv("FAKEGMI_SLEEP", "0.4")
	e := mustNew(t, []Account{f.account("personal")}, f.opts(Options{}))
	start(t, e)
	waitFor(t, 2*time.Second, "sync start", func() bool { return len(f.entries()) > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := e.Send(ctx, "personal", strings.NewReader(rawMsg))
	if !errors.Is(err, ErrBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrBusy wrapping the deadline", err)
	}
}

func TestAccepted(t *testing.T) {
	cases := []struct {
		r    Result
		want bool
	}{
		{Result{Op: OpSend, Output: "sending message, from: a@b..\nreceiving content: 100%|##########| 1/1\nTraceback (most recent call last):"}, true},
		{Result{Op: OpSend, Output: "message sent successfully: 19c8698f3401dced\nerror: lock"}, true},
		{Result{Op: OpSend, Output: "sending message, from: a@b..\ngoogleapiclient.errors.HttpError: 400"}, false},
		{Result{Op: OpSync, Output: "receiving content: 3/3"}, false},
	}
	for _, c := range cases {
		if got := c.r.Accepted(); got != c.want {
			t.Errorf("%q: %v", c.r.Output, got)
		}
	}
}
