package client

import (
	"encoding/json"
	"fmt"
	"github.com/jmckible/pneu/internal/control"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func statusBlock(unread int, senders, accounts string) string {
	return block("status", fmt.Sprintf(`{"version":1,"updated":"x","running":true,"unread":%d,"senders":%s,"accounts":%s}`, unread, senders, accounts))
}

func readV2(t *testing.T, path string) (StatusV2, map[string]any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc StatusV2
	var raw map[string]any
	if json.Unmarshal(b, &doc) != nil || json.Unmarshal(b, &raw) != nil {
		t.Fatalf("status.json: %s", b)
	}
	return doc, raw
}

func waitV2(t *testing.T, path, what string, ok func(StatusV2) bool) StatusV2 {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			var doc StatusV2
			if json.Unmarshal(b, &doc) == nil && ok(doc) {
				return doc
			}
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(path)
			t.Fatalf("status.json never showed %s:\n%s", what, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// status.json v2: this daemon's clock and running, the server's last
// valid status held to its limits, the link in local codes; written at
// most once per StatusGap however fast valid statuses come, the latest
// winning.
func TestStatusFileV2(t *testing.T) {
	long := strings.Repeat("é", 200)
	first := `{"version":1,"updated":"x","running":true,"unread":3,"senders":["<b>Ann</b>","Bob\u0007\u202e","` + long + `"],` +
		`"accounts":[{"name":"personal","unread":3,"pulled":true,"lastSync":"2026-09-30T10:00:00+02:00","failures":1,"error":"line\nbreak","state":"ready","progress":null,"x":1}],"evil":true}`
	blocks := []string{helloBlock(first)}
	for i := range 300 {
		blocks = append(blocks, statusBlock(100+i, `["Cy"]`, `[{"name":"personal","unread":1,"state":"ready"}]`))
	}
	// After the last valid one, each over a limit or naming an account
	// outside the set: dropped, so 399 stays.
	blocks = append(blocks,
		statusBlock(901, `[]`, `[`+strings.Repeat(`{"name":"personal","state":"ready"},`, 16)+`{"name":"personal","state":"ready"}]`),
		statusBlock(902, `["a","b","c","d","e","f"]`, `[]`),
		statusBlock(903, `[]`, `[{"name":"work","state":"ready"}]`),
		statusBlock(904, `[]`, `[{"name":"personal","state":"<b>"}]`),
		block("syncing", `{"account":"personal"}`),
	)
	release := make(chan struct{})
	var mu sync.Mutex
	var writes []time.Time
	r, u, _ := scriptRig(t, newScript(release, blocks...), func(d *Daemon) {
		// Valid events faster than a second's writes, within the stream's
		// budget (TestStreamBudget floods past it).
		d.budget.EventsBurst = 1000
		// The server's build: no nudge (skew_test.go has the others).
		d.self = func() control.Info { return control.Info{Revision: linktestRev} }
		d.onStatusWrite = func() {
			mu.Lock()
			writes = append(writes, time.Now())
			mu.Unlock()
		}
	})
	// Before any status: the link, no counts, statusAt null.
	doc := waitV2(t, r.status, "the link up", func(d StatusV2) bool { return d.Server.Link == "up" })
	if doc.Version != 2 || !doc.Running || doc.Server.StatusAt != nil || doc.Server.Name != "dell" || doc.Unread != 0 || len(doc.Accounts) != 0 {
		t.Fatalf("before a status: %+v", doc)
	}
	close(release)

	got := waitV2(t, r.status, "the flood's last", func(d StatusV2) bool { return d.Unread == 399 })
	if got.Version != 2 || !got.Running || got.Server.Link != "up" || got.Server.Reason != nil || got.Server.Update != nil ||
		got.Server.StatusAt == nil || len(got.Senders) != 1 || got.Senders[0] != "Cy" {
		t.Fatalf("after the flood: %+v", got)
	}
	time.Sleep(1200 * time.Millisecond) // anything still pending lands
	if doc, _ := readV2(t, r.status); doc.Unread != 399 {
		t.Fatalf("a status past its limits was written: %+v", doc)
	}
	mu.Lock()
	ws := append([]time.Time{}, writes...)
	mu.Unlock()
	for i := 1; i < len(ws); i++ {
		if gap := ws[i].Sub(ws[i-1]); gap < StatusGap-50*time.Millisecond {
			t.Fatalf("writes %v apart (%d writes)", gap, len(ws))
		}
	}
	if len(ws) > 6 {
		t.Fatalf("%d writes for a flood of 300", len(ws))
	}

	// statusAt is when the last valid status arrived; updated keeps
	// moving without one.
	before := got
	r.d.wakeStatus()
	after := waitV2(t, r.status, "a later updated", func(d StatusV2) bool { return d.Updated != before.Updated })
	if *after.Server.StatusAt != *before.Server.StatusAt || after.Updated <= *after.Server.StatusAt {
		t.Fatalf("statusAt %s updated %s", *after.Server.StatusAt, after.Updated)
	}

	// The link going down: local codes, the counts kept as last known.
	u.SetHello(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) })
	r.d.up.Retry()
	down := waitV2(t, r.status, "the link down", func(d StatusV2) bool { return d.Server.Link == "down" })
	if down.Server.Reason == nil || *down.Server.Reason != "refused" || down.Unread != 399 || *down.Server.StatusAt != *before.Server.StatusAt {
		t.Fatalf("down: %+v", down)
	}

	// A clean stop says the daemon isn't running.
	r.stop()
	if doc, _ := readV2(t, r.status); doc.Running {
		t.Fatal("running after stop")
	}
	fi, err := os.Stat(r.status)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
}

// The first status, from hello: strings plain and bounded, markup kept
// as text (the widget renders it as text), unknown fields gone.
func TestStatusFileLimits(t *testing.T) {
	long := strings.Repeat("é", 200)
	first := `{"version":1,"updated":"x","running":true,"unread":-4,"senders":["<b>Ann</b>","Bob\u0007\u202e\u2028","` + long + `",""],` +
		`"accounts":[{"name":"personal","unread":3,"pulled":true,"lastSync":"2026-09-30T10:00:00+02:00","failures":1,"error":"line\nbreak` + long + `","state":"ready","progress":{"phase":"content","done":1,"total":2,"percent":50,"frontier":"bad"},"x":1}],"evil":true}`
	r, _, _ := scriptRig(t, newScript(nil, helloBlock(first)))
	doc := waitV2(t, r.status, "the hello's status", func(d StatusV2) bool { return d.Server.StatusAt != nil })
	_, raw := readV2(t, r.status)
	if _, ok := raw["evil"]; ok {
		t.Fatal("an unknown field passed")
	}
	if doc.Unread != 0 || len(doc.Senders) != 3 || doc.Senders[0] != "<b>Ann</b>" || doc.Senders[1] != "Bob" ||
		utf8.RuneCountInString(doc.Senders[2]) != maxText {
		t.Fatalf("senders %q unread %d", doc.Senders, doc.Unread)
	}
	a := doc.Accounts
	if len(a) != 1 || a[0].Name != "personal" || *a[0].LastSync != "2026-09-30T08:00:00Z" || a[0].Error == nil ||
		!strings.HasPrefix(*a[0].Error, "linebreak") || utf8.RuneCountInString(*a[0].Error) != maxText ||
		a[0].Progress == nil || a[0].Progress.Frontier != nil || *a[0].Progress.Percent != 50 {
		t.Fatalf("accounts %+v", a)
	}
	b, _ := os.ReadFile(r.status)
	if strings.Contains(string(b), `\u003c`) || !strings.Contains(string(b), "<b>Ann</b>") {
		t.Fatalf("status.json escapes markup (QML reads it, as plain text): %s", b)
	}
}
