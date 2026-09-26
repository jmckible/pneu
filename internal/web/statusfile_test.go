package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFirstName(t *testing.T) {
	for in, want := range map[string]string{
		"Ada Lovelace, Charles Babbage| Me": "Ada",
		`"Grace Hopper"| Ada`:               "Grace",
		"billing@example.com":               "billing",
		" | Unmatched Only":                 "",
		"":                                  "",
	} {
		if got := firstName(in); got != want {
			t.Errorf("firstName(%q) = %q, want %q", in, got, want)
		}
	}
}

func readStatus(t *testing.T, path string) (statusDoc, os.FileInfo, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc statusDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("status.json: %v\n%s", err, b)
	}
	return doc, fi, string(b)
}

func waitStatus(t *testing.T, path, what string, ok func(statusDoc) bool) statusDoc {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			var doc statusDoc
			if json.Unmarshal(b, &doc) == nil && ok(doc) {
				return doc
			}
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(path)
			t.Fatalf("status.json never showed %s:\n%s", what, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStatusSnapshot(t *testing.T) {
	f := newTagFixture(t)
	f.s.now = func() time.Time { return time.Date(2026, 9, 25, 9, 30, 0, 0, time.UTC) }
	doc := f.s.statusSnapshot(true)

	if doc.Version != 1 || !doc.Running || doc.Updated != "2026-09-25T09:30:00Z" {
		t.Fatalf("header: %+v", doc)
	}
	want := 0
	for i, a := range f.s.Accounts {
		n, err := a.Count(context.Background(), unreadQuery, true)
		if err != nil {
			t.Fatal(err)
		}
		if doc.Accounts[i].Name != a.Name || doc.Accounts[i].Unread != n {
			t.Errorf("account %d: %+v, want %s with %d unread", i, doc.Accounts[i], a.Name, n)
		}
		want += n
	}
	if want == 0 || doc.Unread != want {
		t.Fatalf("unread = %d, want %d (the fixture needs unread inbox mail)", doc.Unread, want)
	}
	if len(doc.Senders) == 0 || len(doc.Senders) > statusSenders {
		t.Fatalf("senders = %q", doc.Senders)
	}
	for i, n := range doc.Senders {
		if n == "" || strings.ContainsAny(n, " @,|") || slices.Index(doc.Senders, n) != i {
			t.Fatalf("senders = %q: want distinct first names", doc.Senders)
		}
	}

	// fakeSyncer: personal healthy, work failing.
	p, v := doc.Accounts[0], doc.Accounts[1]
	if !p.Pulled || p.LastSync == nil || *p.LastSync != "2026-09-23T10:00:00Z" || p.Failures != 0 || p.Error != nil {
		t.Errorf("personal: %+v", p)
	}
	if v.Failures != 2 || v.Error == nil || *v.Error != "gmi exited 1" || v.LastSync != nil {
		t.Errorf("work: %+v", v)
	}

	f.s.Syncer = nil
	doc = f.s.statusSnapshot(false)
	if doc.Running || doc.Accounts[0].Error != nil || doc.Accounts[0].Pulled || doc.Unread != want {
		t.Errorf("no engine: %+v", doc)
	}
}

// The file's lifecycle: written at start, rewritten after a tag write, and
// left with running false at shutdown. Private, and never the token or nonce.
func TestRunStatus(t *testing.T) {
	f := newTagFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	launch := filepath.Join(dir, "launch")
	if err := f.s.Auth.StartLaunch(launch); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.s.RunStatus(ctx, path); close(done) }()
	defer func() { cancel(); <-done }()

	first := waitStatus(t, path, "a first write", func(d statusDoc) bool { return d.Running })
	_, fi, raw := readStatus(t, path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, want 0600", fi.Mode().Perm())
	}
	nonce, _ := os.ReadFile(launch)
	if strings.Contains(raw, token) || strings.Contains(raw, strings.TrimSpace(string(nonce))) {
		t.Fatalf("status.json carries the token or nonce:\n%s", raw)
	}

	p := f.s.Accounts[0]
	ids, err := p.MessageIDs(context.Background(), unreadQuery)
	if err != nil || len(ids) == 0 || first.Accounts[0].Unread == 0 {
		t.Fatalf("fixture: %d unread ids in %s (%v), status %+v", len(ids), p.Name, err, first.Accounts[0])
	}
	tagOK(t, f.s, form("action", "read", "account", p.Name, "ids", esc(ids...)))
	waitStatus(t, path, "the read thread", func(d statusDoc) bool {
		return d.Accounts[0].Unread == 0 && d.Unread == first.Unread-first.Accounts[0].Unread
	})

	f.s.StatusChanged() // a sync end: an immediate rewrite, same content
	cancel()
	<-done
	if last, _, _ := readStatus(t, path); last.Running {
		t.Fatalf("after shutdown: %+v", last)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".status.json-*")); len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}
