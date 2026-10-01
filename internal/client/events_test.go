package client

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/web"
)

// The wire reader: comments make no event, data lines join, CRLF is a
// line end, and a line or event past the cap is read through and marked,
// never held, while the stream goes on.
func TestSSEReader(t *testing.T) {
	long := strings.Repeat("x", 300)
	in := ": open\n\n" +
		"event: a\r\ndata: 1\r\ndata: 2\r\n\r\n" +
		strings.Repeat(": ping\n\n", 100) + // pings never add up to an oversized event
		"event: b\ndata: " + long + "\n\n" +
		"event: c\ndata: " + long[:100] + "\ndata: " + long[:100] + "\n\n" +
		"event: d\ndata:\n\n" +
		"data: unnamed\n\n" +
		"event: e\n: a comment inside\ndata: {}\n\n"
	sr := newSSEReader(strings.NewReader(in), 200)
	want := []sseEvent{
		{name: "a", data: []byte("1\n2")},
		{name: "b", over: true},
		{name: "c", over: true},
		{name: "d"},
		{data: []byte("unnamed")},
		{name: "e", data: []byte("{}")},
	}
	for i, w := range want {
		ev, err := sr.next()
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if ev.name != w.name || string(ev.data) != string(w.data) || ev.over != w.over {
			t.Fatalf("%d: %+v, want %+v", i, ev, w)
		}
	}
	if _, err := sr.next(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
}

func TestPlain(t *testing.T) {
	for in, want := range map[string]string{
		"Ann":                   "Ann",
		"<b>Ann</b>":            "<b>Ann</b>",
		"a\x00b\x07c\nd\re\tf":  "abcdef",
		"x\u202ey\u2066z\u200f": "xyz",
		"l\u2028m\u2029n":       "lmn",
		"bad\xffutf":            "bad\ufffdutf",
	} {
		if got := plain(in, 100); got != want {
			t.Errorf("plain(%q) = %q, want %q", in, got, want)
		}
	}
	if got := plain(strings.Repeat("é", 10), 4); got != "éééé" {
		t.Errorf("not cut to 4 runes: %q", got)
	}
}

// A hostile account name never gets as far as the stream: the link
// refuses the hello that names it (protocol), so nothing opens, nothing is
// relayed, and status.json has the link down and no counts.
func TestHostileAccountName(t *testing.T) {
	keys := linktest.NewKeys(t)
	s := newScript(nil, helloBlock(`{"version":1,"unread":5,"senders":[],"accounts":[]}`))
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, s)
	u.SetHello(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(web.ProtocolHeader, strconv.Itoa(web.Protocol))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"protocol":1,"name":"dell","epoch":"abcd","gen":3,"accounts":[{"name":"personal","email":"me@example.com"},{"name":"pay‮latot","email":"x@example.com"}]}`)
	})
	r := newRig(t, keys, u.Port, nil)
	waitState(t, r.link, link.Protocol)
	doc := waitV2(t, r.status, "the link refused", func(d StatusV2) bool { return d.Server.Reason != nil && *d.Server.Reason == "protocol" })
	if doc.Server.StatusAt != nil || doc.Unread != 0 || len(doc.Accounts) != 0 || s.opened.Load() != 0 {
		t.Fatalf("%+v, %d streams", doc, s.opened.Load())
	}
	b, _ := os.ReadFile(r.status)
	if strings.Contains(string(b), "‮") || strings.Contains(string(b), "latot") {
		t.Fatalf("the name reached status.json: %s", b)
	}
}

// fakeHello is an Upstream whose link hello names whatever a test wants:
// the handshake's own check, behind the link's.
type fakeHello struct {
	Upstream
	h *link.Hello
}

func (f fakeHello) Hello() *link.Hello          { return f.h }
func (f fakeHello) State() link.State           { return link.State{Reason: link.Up} }
func (f fakeHello) Email(string) (string, bool) { return "", false }

func TestHandshakeRefusesNames(t *testing.T) {
	for _, name := range []string{"a‮b", "a\nb", "a b", "a b"} {
		d := New(Config{Auth: web.NewAuth(testHost, testToken), Link: fakeHello{h: &link.Hello{Accounts: []web.HelloAccount{{Name: name, Email: "a@b.c"}}}}})
		ev := sseEvent{name: "hello", data: []byte(`{"epoch":"abcd","gen":1,"accounts":[]}`)}
		if err := d.handshake(&upstream{}, ev); err != errHandshake {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func flood(n int) []string {
	out := []string{helloBlock("")}
	for range n {
		out = append(out, block("syncing", `{"account":"personal"}`))
	}
	return out
}

func waitOpens(t *testing.T, s *script, n int, within time.Duration) []time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for len(s.opens()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d opens, want %d", len(s.opens()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.opens()
}

// A server flooding valid events goes over the budget: the stream is
// closed long before the flood ends, and each reopen waits longer, a
// valid hello notwithstanding.
func TestStreamBudgetEvents(t *testing.T) {
	s := &script{perOpen: func(int) []string { return flood(5000) }}
	r, _, _ := scriptRig(t, s, func(d *Daemon) { d.streamMin, d.streamMax = 50*time.Millisecond, 2*time.Second })
	p := r.page()
	at := waitOpens(t, s, 5, 10*time.Second)
	for i := 2; i < len(at)-1; i++ {
		if a, b := at[i].Sub(at[i-1]), at[i+1].Sub(at[i]); b < a*3/2 {
			t.Fatalf("backoff didn't grow: %v then %v (%v)", a, b, at)
		}
	}
	// What reached a page is bounded by the budget per stream: its burst,
	// plus what refills while it lasts (well under a second each).
	n := 0
	deadline := time.After(time.Second)
drain:
	for {
		select {
		case ev := <-p.evs:
			if ev.name == "syncing" {
				n++
			}
		case <-deadline:
			break drain
		}
	}
	streams := len(s.opens())
	if n == 0 || n > streams*int(DefaultBudget.EventsBurst+DefaultBudget.EventsPerSec) {
		t.Fatalf("%d syncing relayed over %d streams", n, streams)
	}
}

// A line that never ends is charged byte by byte and closes the stream.
func TestStreamBudgetBytes(t *testing.T) {
	s := &script{perOpen: func(int) []string { return []string{helloBlock(""), endless} }}
	scriptRig(t, s, func(d *Daemon) {
		d.streamMin = 50 * time.Millisecond
		d.budget.BytesPerSec, d.budget.BytesBurst = 64<<10, 512<<10
	})
	waitOpens(t, s, 3, 10*time.Second)
}

// The backoff resets only after a stream that lasted Healthy: three
// streams that hello and then flood back off further each time; one that
// stays quiet past Healthy before its flood is reopened at the minimum.
func TestStreamBackoffResets(t *testing.T) {
	s := &script{perOpen: func(n int) []string {
		if n == 4 {
			return append([]string{helloBlock(""), sleep(700 * time.Millisecond)}, flood(5000)[1:]...)
		}
		return flood(5000)
	}}
	scriptRig(t, s, func(d *Daemon) {
		d.streamMin, d.streamMax, d.healthy = 100*time.Millisecond, 5*time.Second, 500*time.Millisecond
	})
	at := waitOpens(t, s, 5, 10*time.Second)
	g2, g3, g5 := at[2].Sub(at[1]), at[3].Sub(at[2]), at[4].Sub(at[3])
	// 2→3 waited 200ms, 3→4 400ms; 4 lasted ~700ms, then only the minimum
	// (100ms), where without the reset it would wait 800ms.
	if g3 < g2*3/2 || g5 > 1300*time.Millisecond {
		t.Fatalf("gaps %v %v %v", g2, g3, g5)
	}
}

// The wire, in fragments: CRLF split across reads is one line end, a BOM
// split across reads is skipped, a multibyte rune split across reads
// comes through whole, and a lone CR ends a line.
func TestSSEReaderFragments(t *testing.T) {
	in := "\ufeffevent: a\rdata: é€😀\r\n\r\n: open\r\n\r\nid: 7\nretry: 10\n\nevent: only-a-name\n\nevent: b\ndata: x\n\n"
	sr := newSSEReader(iotest.OneByteReader(strings.NewReader(in)), 200)
	for _, w := range []sseEvent{{name: "a", data: []byte("é€😀")}, {name: "b", data: []byte("x")}} {
		ev, err := sr.next()
		if err != nil || ev.name != w.name || string(ev.data) != string(w.data) || ev.over {
			t.Fatalf("%+v %v, want %+v", ev, err, w)
		}
	}
	if _, err := sr.next(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
	// Only one BOM is skipped: a second is part of the field's name.
	sr = newSSEReader(strings.NewReader("\ufeff\ufeffevent: a\ndata: 1\n\n"), 200)
	if ev, err := sr.next(); err != nil || ev.name != "" || string(ev.data) != "1" {
		t.Fatalf("a second BOM: %+v %v", ev, err)
	}
}

// An unterminated oversized line holds nothing past the cap and ends when
// the stream does.
func TestSSEReaderEndsMidLine(t *testing.T) {
	pr, pw := io.Pipe()
	sr := newSSEReader(pr, 100)
	done := make(chan error, 1)
	go func() {
		_, err := sr.next()
		done <- err
	}()
	for range 100 {
		pw.Write([]byte(strings.Repeat("x", 1000)))
	}
	pw.CloseWithError(io.ErrClosedPipe)
	select {
	case err := <-done:
		if err != io.ErrClosedPipe {
			t.Fatalf("%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still reading")
	}
}
