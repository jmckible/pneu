package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
)

func plainMessage(body string) *notmuch.Message {
	return &notmuch.Message{Body: []notmuch.Part{{ContentType: "text/plain", Content: body}}}
}

func TestSnippet(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"greeting and sign-off go",
			"Hi all,\n\nShip it on Friday.\nThe notes are in the doc.\n\nThanks,\nOwen\n",
			"Ship it on Friday. The notes are in the doc."},
		{"trailing quote and its attribution go",
			"Agreed.\n\nOn Wed, Oct 7, 2026 at 7:28 AM, Owen Pryce wrote:\n> Ship it?\n",
			"Agreed."},
		{"signature goes",
			"Numbers attached.\n\n-- \nOwen Pryce · Growth\n",
			"Numbers attached."},
		{"image placeholders go",
			"Two favorites.\n\n[red.png]\n[image: blue.png]\n\nMore soon.\n",
			"Two favorites. More soon."},
		{"a one-line message keeps its only line, greeting or not",
			"Hi Sam!\n",
			"Hi Sam!"},
		{"an inline reply keeps its own lines",
			"> Can you do Friday?\nYes.\n> And Saturday?\nNo.\n",
			"Yes. No."},
		{"all quote is empty",
			"> only a quote\n",
			""},
	} {
		if got := snippet(plainMessage(c.body)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	long := strings.Repeat("word ", 100)
	if got := snippet(plainMessage(long)); len([]rune(got)) != snippetMax {
		t.Errorf("long body: %d runes, want %d", len([]rune(got)), snippetMax)
	}

	// The text half of an alternative; HTML alone has none.
	alt := &notmuch.Message{Body: []notmuch.Part{{ContentType: "multipart/alternative", Children: []notmuch.Part{
		{ContentType: "text/plain", Content: "Plain half."},
		{ContentType: "text/html", Content: "<p>HTML half.</p>"},
	}}}}
	if got := snippet(alt); got != "Plain half." {
		t.Errorf("alternative: %q", got)
	}
	html := &notmuch.Message{Body: []notmuch.Part{{ContentType: "text/html", Content: "<p>Only HTML.</p>"}}}
	if got := snippet(html); got != "" {
		t.Errorf("html only: %q", got)
	}
	// An attached text file isn't the body.
	att := &notmuch.Message{Body: []notmuch.Part{{ContentType: "multipart/mixed", Children: []notmuch.Part{
		{ContentType: "text/plain", ContentDisposition: "attachment", Content: "notes.txt contents"},
	}}}}
	if got := snippet(att); got != "" {
		t.Errorf("attachment: %q", got)
	}
}

func TestContextDates(t *testing.T) {
	loc := time.FixedZone("PDT", -7*3600)
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, loc)
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, loc) }
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{at(10, 13, 1), "Today"},
		{at(10, 12, 23), "Yesterday"},
		{at(10, 11, 12), "Sun Oct 11"}, // a bare weekday read as the coming one
		{time.Date(2025, 12, 31, 12, 0, 0, 0, loc), "Wed Dec 31, 2025"},
	} {
		if got := relDay(c.t, now); got != c.want {
			t.Errorf("relDay(%v) = %q, want %q", c.t, got, c.want)
		}
	}
	for _, c := range []struct {
		a, b time.Time
		want string
	}{
		{at(10, 7, 23), at(10, 8, 1), ""}, // the next day is no silence
		{at(10, 7, 11), at(10, 12, 6), "5 days later"},
		{at(9, 1, 12), at(9, 22, 12), "3 weeks later"},
		{at(3, 1, 12), at(10, 1, 12), "7 months later"},
		{time.Date(2023, 1, 1, 12, 0, 0, 0, loc), at(10, 1, 12), "3 years later"},
	} {
		if got := gapLabel(c.a, c.b); got != c.want {
			t.Errorf("gapLabel(%v, %v) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
	// Across a DST change a day is 23 or 25 hours.
	la, err := time.LoadLocation("America/Los_Angeles")
	if err == nil {
		if n := calendarDays(time.Date(2026, 3, 7, 12, 0, 0, 0, la), time.Date(2026, 3, 9, 0, 30, 0, 0, la)); n != 2 {
			t.Errorf("across spring forward: %d days, want 2", n)
		}
	}
}

func TestShortNames(t *testing.T) {
	for in, want := range map[string]string{
		"Sam Ortega":                          "Sam Ortega",
		"Sam Ortega, me, Priya Natarajan":     "Sam, me, Priya",
		"Tessa Lund (via Google Docs), me":    "Tessa, me",
		"notifications@codehost.example, Kai": "notifications, Kai",
	} {
		if got := shortAuthors(in); got != want {
			t.Errorf("shortAuthors(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"Robin Hale <robin@hale.example>, Sam Ortega <sam@ortega.example>", "me, Sam Ortega"},
		{"ROBIN@HALE.EXAMPLE, kai@fieldwork.example", "me, kai@fieldwork.example"},
		{"", ""},
		{"not <an address", "not <an address"},
	} {
		if got := recipientNames(c.in, "robin@hale.example"); got != c.want {
			t.Errorf("recipientNames(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The fixture's eight-message thread: a divider per day naming the silence
// before the last day, times alone, snippets on the folded rows.
func TestThreadStack(t *testing.T) {
	s := newServer(t)
	s.now = func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local) }
	body := getOK(t, s, threadURL(t, s, "/", "November offsite venue"))
	if n := strings.Count(body, `<div class="day">`); n != 2 {
		t.Errorf("%d day dividers, want 2", n)
	}
	for _, w := range []string{
		`">Wed Oct 7</time></div>`,
		`"><span class="gap">5 days later</span> · Yesterday</time></div>`,
		// The greeting and sign-off are off Owen's opener.
		`<div class="snip">Two venues are still open for the November offsite: the lake lodge and the old mill in town.`,
		// A quoting reply shows only its own line.
		`<div class="snip">ok, let&#39;s book the lodge. Kai, can you send the contract?</div>`,
		`<span class="name">me</span> <span class="addr">&lt;robin@northwind.example&gt;</span>`,
		`title="Wed, Oct 7, 2026 at 7:28 AM">7:28 AM</time>`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("thread missing %s", w)
		}
	}
	if strings.Contains(body, "Owen Pryce wrote:") && strings.Contains(body, `<div class="snip">`+"On ") {
		t.Error("a snippet carries a quote attribution")
	}
}
