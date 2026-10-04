package web

import (
	"context"
	"encoding/json"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/testmail"
)

// listRow is one parsed <li class="row"> from a list page.
type listRow struct {
	Class, Account, Thread, URL, Subject string
	MsgIDs                               []string // unescaped
	ThreadIDs                            []string // unescaped
	Gmail                                string
	Count                                bool
}

var rowRE = regexp.MustCompile(`<li class="([^"]*)" data-account="([^"]*)" data-thread="([^"]*)" data-url="([^"]*)" data-msgids="([^"]*)" data-thread-ids="([^"]*)" data-gmail="([^"]*)">(.*?)</li>`)
var subjectRE = regexp.MustCompile(`<span class="subject">(.*?)</span>`)

func getOK(t *testing.T, s http.Handler, target string) string {
	t.Helper()
	w := do(s, "GET", target, withCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
	}
	return w.Body.String()
}

func rows(t *testing.T, s http.Handler, target string) []listRow {
	t.Helper()
	var out []listRow
	for _, m := range rowRE.FindAllStringSubmatch(getOK(t, s, target), -1) {
		r := listRow{Class: m[1], Account: m[2], Thread: m[3], URL: m[4], Gmail: html.UnescapeString(m[7]), Count: strings.Contains(m[8], `class="count"`)}
		if sm := subjectRE.FindStringSubmatch(m[8]); sm != nil {
			r.Subject = html.UnescapeString(sm[1])
		}
		unesc := func(v string) []string {
			var out []string
			for _, id := range strings.Fields(html.UnescapeString(v)) {
				raw, err := url.QueryUnescape(id)
				if err != nil {
					t.Fatalf("msgid %q: %v", id, err)
				}
				out = append(out, raw)
			}
			return out
		}
		r.MsgIDs, r.ThreadIDs = unesc(m[5]), unesc(m[6])
		out = append(out, r)
	}
	return out
}

func keys(rs []listRow) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Account+": "+r.Subject)
	}
	return out
}

func find(t *testing.T, rs []listRow, subject string) listRow {
	t.Helper()
	for _, r := range rs {
		if strings.HasPrefix(r.Subject, subject) {
			return r
		}
	}
	t.Fatalf("no row %q in %q", subject, keys(rs))
	return listRow{}
}

var inboxOrder = []string{
	"personal: Your Quillmate sign-in link",
	"work: [northwind/app] SSO: SAML metadata upload (PR #4821)",
	"work: New logo lockup for review",
	"personal: Action required: verify your mailbox",
	"personal: (no subject)",
	"personal: The Weekend Reader - Issue 112: the case for boring software",
	"work: Deploy summary for 2026-09-18",
	"personal: Invitation: Dental cleaning @ Thu Oct 15, 2026 9am - 10am (PDT)",
	"personal: Photos from the trip + the itinerary",
	"work: Escalation: exports stuck at 99% since this morning",
	"personal: Your trip to Lisbon: confirmation QK7P2M",
	"personal: Fwd: Signed lease addendum",
	"personal: Contractor W-9 for my records",
	"personal: Café à Montréal — dimanche ?", // same Date as the next row
	"work: Übersetzungen für die Oberfläche — Polnisch fertig",
	"work: Q3 roadmap review",
	"personal: Kitchen quote - revised",
	"personal: 🎉 You're invited: Maya turns 40",
	"personal: Cabin weekend in October?",
	"personal: Reminder: the annual homeowners association meeting has been rescheduled from Tuesday the 14th to Thursday the 23rd because the community center is being repainted and the board needs quorum to vote on the landscaping contract, the pool resurfacing, and the new guest parking rules",
}

func TestInbox(t *testing.T) {
	env := testmail.Setup(t)
	s := serverFor(t, env.Accounts)
	body := getOK(t, s, "/")
	for _, want := range []string{
		`<body data-origin="http://pneu.localhost:7317" data-epoch="` + s.view.epoch + `" data-gen="0">`,
		`<link rel="stylesheet" href="/static/app.css">`,
		`<script src="/static/purify.min.js"></script>`,
		`<script src="/static/app.js" defer></script>`,
		// The page's place, for the key bar's counter; one page, so not paged.
		`<main class="list" data-view="inbox" data-start="0" data-rows="20" data-total="20">`,
		`<link rel="expect" href="#tube" blocking="render">`, // tube.js's pagereveal sees the tube
		`<script src="/static/triage.js"></script>
<script src="/static/tube.js"></script>`, // the layout thresholds before the fold
		// The tube: the ghost search station, the main line, the drop, the
		// bins; the capsule only on the active station.
		`<div class="line"><span class="main"><span class="q ghost" data-station="q"><a href="/search" title="Search (/)"><kbd>/</kbd><span class="nm">search</span></a></span>` +
			`<a href="/" data-station="1" title="Inbox (1)" class="active" aria-current="page"><kbd>1</kbd><span class="nm">Inbox</span><span class="cap" aria-hidden="true"></span></a>`,
		`<a href="/starred" data-station="2" title="Starred (2)"><kbd>2</kbd><span class="nm">Starred</span></a>`,
		`<a href="/sent" data-station="3" title="Sent (3)"><kbd>3</kbd><span class="nm">Sent</span></a>`,
		`<a href="/all" data-station="4" title="All (4)"><kbd>4</kbd><span class="nm">All</span></a></span><span class="drop" aria-hidden="true"></span>`,
		`<span class="bins"><a href="/spam" data-station="5" title="Spam (5)" data-spam="`,
		`<a href="/trash" data-station="6" title="Trash (6)"><kbd>6</kbd><span class="nm">Trash</span></a></span></div>`,
		`<button id="mark" type="button"`, `<div class="foot"><button id="sync" type="button" title="Sync details" aria-haspopup="dialog" hidden>`,
		`<div class="wright"><div id="status"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox missing %s", want)
		}
	}
	// No header, no ruler, and an unpaged list has no pager row.
	for _, gone := range []string{"<header>", `id="ruler"`, `class="pager"`, `class="pages"`} {
		if strings.Contains(body, gone) {
			t.Errorf("inbox has %s", gone)
		}
	}
	// No inline script anywhere: every <script> has a src.
	if n, m := strings.Count(body, "<script"), strings.Count(body, `<script src="/static/`); n != m {
		t.Errorf("%d scripts, %d with src", n, m)
	}

	rs := rows(t, s, "/")
	if got := keys(rs); !slices.Equal(got, inboxOrder) {
		t.Fatalf("inbox order:\n got %q\nwant %q", got, inboxOrder)
	}
	// The date tie flips with account order: the tiebreak is account order,
	// not whichever database answered first.
	rev := serverFor(t, []testmail.Account{env.Account(t, "work"), env.Account(t, "personal")})
	got := keys(rows(t, rev, "/"))
	i := slices.Index(got, "work: Übersetzungen für die Oberfläche — Polnisch fertig")
	if i < 0 || got[i+1] != "personal: Café à Montréal — dimanche ?" {
		t.Errorf("reversed account order, tie rows: %q", got)
	}

	// Trash and spam never show.
	for _, r := range rs {
		for _, bad := range []string{"Rewards", "Digest", "SEO", "trial"} {
			if strings.Contains(r.Subject, bad) {
				t.Errorf("excluded message in inbox: %q", r.Subject)
			}
		}
	}

	// data-msgids carries the matched (inbox) messages only.
	roadmap := find(t, rs, "Q3 roadmap review")
	if !slices.Equal(roadmap.MsgIDs, []string{"rm-q3-04-owen@northwind.example"}) || !roadmap.Count || roadmap.Account != "work" {
		t.Errorf("roadmap row %+v", roadmap)
	}
	kitchen := find(t, rs, "Kitchen quote")
	if !slices.Equal(kitchen.MsgIDs, []string{"0100019a7c3e-kitchen-1@delgadobuild.example", "0100019a9d11-kitchen-2@delgadobuild.example"}) ||
		kitchen.Class != "row unread flagged" {
		t.Errorf("kitchen row %+v", kitchen)
	}
	pr := find(t, rs, "[northwind/app]")
	if !slices.Equal(pr.MsgIDs, []string{"northwind/app/pull/4821@codehost.example"}) || pr.Count {
		t.Errorf("pr row %+v", pr)
	}
	if !strings.Contains(body, `data-msgids="northwind%2Fapp%2Fpull%2F4821%40codehost.example"`) {
		t.Error("msgids not QueryEscape'd")
	}
	// notmuch's index-time attachment tag marks the row; a body alone doesn't.
	if lease := find(t, rs, "Fwd: Signed lease addendum"); !strings.HasSuffix(lease.Class, " attach") {
		t.Errorf("lease row class %q, want the attachment mark", lease.Class)
	}
	if w9 := find(t, rs, "Contractor W-9"); strings.Contains(w9.Class, "attach") {
		t.Errorf("w-9 row class %q: no attachment", w9.Class)
	}
	if cafe := find(t, rs, "Café"); cafe.Class != "row" || cafe.URL != "/t/personal/"+cafe.Thread {
		t.Errorf("cafe row %+v", cafe)
	}
}

func TestPagination(t *testing.T) {
	s := newServer(t)
	s.PerPage = 5
	var all []string
	for page := 0; ; page++ {
		target := "/"
		if page > 0 {
			target += "?page=" + string(rune('0'+page))
		}
		body := getOK(t, s, target)
		got := keys(rows(t, s, target))
		all = append(all, got...)
		hasNext := strings.Contains(body, `rel="next"`)
		if page < 3 && (len(got) != 5 || !hasNext) || page == 3 && (len(got) != 5 || hasNext) {
			t.Fatalf("page %d: %d rows, next=%v", page, len(got), hasNext)
		}
		if (page > 0) != strings.Contains(body, `rel="prev"`) {
			t.Fatalf("page %d prev link", page)
		}
		if page == 3 {
			break
		}
	}
	if !slices.Equal(all, inboxOrder) {
		t.Fatalf("pages concatenated:\n got %q\nwant %q", all, inboxOrder)
	}
	// main.list says where the page sits. Nothing is counted during a
	// render: the first look has no total and the page asks for it.
	if body := getOK(t, s, "/?page=1"); !strings.Contains(body, `data-start="5" data-rows="5" data-total="-1" data-paged>`) {
		t.Errorf("uncounted position: %s", body)
	}
	// The pager row: both ends live in the middle, the first page's newer
	// end dimmed, the last's older; the old bottom links are gone.
	mid := getOK(t, s, "/?page=1")
	for _, want := range []string{
		`<nav class="pager" aria-label="Pages"><a class="pg prev" rel="prev" href="/"><kbd>&lt;</kbd>newer</a>`,
		`<span class="range">6–10</span>`,
		`<a class="pg next" rel="next" href="/?page=2">older<kbd>&gt;</kbd></a></nav>`,
	} {
		if !strings.Contains(mid, want) {
			t.Errorf("page 1 pager missing %s", want)
		}
	}
	if first := getOK(t, s, "/"); !strings.Contains(first, `<span class="pg prev" aria-disabled="true"><kbd>&lt;</kbd>newer</span>`) || strings.Contains(first, `class="pages"`) {
		t.Errorf("first page's pager: %s", first)
	}
	if last := getOK(t, s, "/?page=3"); !strings.Contains(last, `<span class="pg next" aria-disabled="true">older<kbd>&gt;</kbd></span>`) {
		t.Error("last page's older end isn't dimmed")
	}
	w := do(s, "GET", "/?page=1&total=1", withCookie)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"total":20}` {
		t.Fatalf("?total=1: %d %s", w.Code, w.Body)
	}
	// Counted once, cached at the databases' revisions: the next render has it.
	if body := getOK(t, s, "/?page=3"); !strings.Contains(body, `data-start="15" data-rows="5" data-total="20" data-paged>`) {
		t.Errorf("cached position: %s", body)
	}
	// A tag write moves the revision; the stale count is not shown.
	first := rows(t, s, "/")[0]
	tagOK(t, s, form("action", "archive", "account", first.Account, "ids", esc(first.MsgIDs...)))
	if body := getOK(t, s, "/?page=1"); !strings.Contains(body, `data-total="-1"`) {
		t.Error("stale total survived a tag write")
	}
	if w := do(s, "GET", "/?total=1", withCookie); strings.TrimSpace(w.Body.String()) != `{"total":19}` {
		t.Errorf("recount after archive: %s", w.Body)
	}
	// Search pages keep the query.
	s.PerPage = 1
	if body := getOK(t, s, "/search?q=from%3Aowen"); !strings.Contains(body, `href="/search?page=1&amp;q=from%3Aowen"`) {
		t.Errorf("search next link: %s", body)
	}
}

func TestStarredAndSearch(t *testing.T) {
	s := newServer(t)
	starred := rows(t, s, "/starred")
	want := []string{
		"personal: Photos from the trip + the itinerary",
		"work: Escalation: exports stuck at 99% since this morning",
		"personal: Your trip to Lisbon: confirmation QK7P2M",
		"personal: Kitchen quote - revised",
		"work: Notes from the offsite",
		"work: Board deck draft",
	}
	got := keys(starred)
	if len(got) != len(want) {
		t.Fatalf("starred: %q", got)
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Errorf("starred[%d] = %q, want prefix %q", i, got[i], w)
		}
	}
	if k := find(t, starred, "Kitchen"); !slices.Equal(k.MsgIDs, []string{"0100019a9d11-kitchen-2@delgadobuild.example"}) {
		t.Errorf("starred kitchen carries %v", k.MsgIDs)
	}
	if !strings.Contains(getOK(t, s, "/starred"), `data-view="starred"`) {
		t.Error("starred view")
	}

	// Search passes notmuch syntax straight through, across both databases.
	body := getOK(t, s, "/search?q="+url.QueryEscape("from:owen@northwind.example"))
	// The search station shows the query, with the capsule.
	if !strings.Contains(body, `data-view="search"`) ||
		!strings.Contains(body, `<span class="q active" data-station="q"><a href="/search?q=from%3aowen%40northwind.example" aria-current="page" title="Search: from:owen@northwind.example"><kbd>/</kbd><span class="qt">from:owen@northwind.example</span></a><a class="x" href="/"`) ||
		!strings.Contains(body, `×</a><span class="cap" aria-hidden="true"></span></span>`) {
		t.Errorf("search view / query on the stop: %s", body)
	}
	if strings.Contains(body, `class="active" aria-current="page">`) {
		t.Error("a view's station is active on a search")
	}
	// An empty search leaves the stop a ghost.
	if body := getOK(t, s, "/search"); !strings.Contains(body, `<span class="q ghost" data-station="q">`) {
		t.Error("empty search: no ghost stop")
	}
	got = keys(rows(t, s, "/search?q="+url.QueryEscape("from:owen@northwind.example")))
	if !slices.Equal(got, []string{"work: Q3 roadmap review", "work: Board deck draft"}) {
		t.Errorf("search from:owen = %q", got)
	}
	both := keys(rows(t, s, "/search?q="+url.QueryEscape(`id:CAKv0c-w9-crossacct@mail.gmail.com`)))
	if !slices.Equal(both, []string{"personal: Contractor W-9 for my records", "work: Contractor W-9 for my records"}) {
		t.Errorf("cross-account message shows once per database: %q", both)
	}
	// search.exclude_tags applies to search too.
	for _, r := range rows(t, s, "/search?q=*") {
		if strings.Contains(r.Subject, "Rewards") || strings.Contains(r.Subject, "trial") {
			t.Errorf("excluded in search: %q", r.Subject)
		}
	}
	// Explicitly asking for trash works: the query is verbatim.
	if n := len(rows(t, s, "/search?q=tag%3Atrash")); n != 2 {
		t.Errorf("tag:trash: %d rows", n)
	}
	// A bad query reports instead of crashing; an empty one lists nothing.
	if w := do(s, "GET", "/search?q=date%3Anotadate", withCookie); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `class="error"`) {
		t.Errorf("bad query: %d", w.Code)
	}
	if rs := rows(t, s, "/search"); len(rs) != 0 {
		t.Errorf("empty search: %d rows", len(rs))
	}
}

// Spam, Trash and All Mail are fixed queries; spam and trash name their
// excluded tag, so notmuch lifts the exclusion; All Mail leaves both out.
// A thread opened from Spam or Trash shows its messages (not a 404) and
// says where it is, for app.js's trash no-op.
func TestSpamTrashAll(t *testing.T) {
	s := newServer(t)
	for _, c := range []struct{ path, view, key string }{{"/sent", "sent", "3"}, {"/spam", "spam", "5"}, {"/trash", "trash", "6"}, {"/all", "all", "4"}} {
		body := getOK(t, s, c.path)
		if !strings.Contains(body, `data-view="`+c.view+`"`) || !strings.Contains(body, `<a href="`+c.path+`" data-station="`+c.key+`" title="`) ||
			!strings.Contains(body, `)" class="active" aria-current="page"`) {
			t.Errorf("%s: view or active nav missing", c.path)
		}
	}
	if n := len(rows(t, s, "/trash")); n != 2 {
		t.Errorf("trash: %d rows", n)
	}
	spam := rows(t, s, "/spam")
	if len(spam) == 0 {
		t.Fatal("spam: no rows")
	}
	all := rows(t, s, "/all")
	if len(all) <= len(rows(t, s, "/")) {
		t.Errorf("all mail (%d) should hold more than the inbox", len(all))
	}
	for _, r := range all {
		if strings.Contains(r.Subject, "Rewards") || strings.Contains(r.Subject, "trial") {
			t.Errorf("excluded in all mail: %q", r.Subject)
		}
	}
	for _, c := range []struct{ list, in string }{{"/spam", "spam"}, {"/trash", "trash"}} {
		for _, r := range rows(t, s, c.list) {
			body := getOK(t, s, r.URL)
			if !strings.Contains(body, ` data-in="`+c.in+`"`) || !strings.Contains(body, "<article") {
				t.Errorf("%s thread %q: no data-in or no messages", c.list, r.Subject)
			}
		}
	}
	if strings.Contains(getOK(t, s, threadURL(t, s, "/", "Cabin weekend")), "data-in=") {
		t.Error("live thread carries data-in")
	}
}

var articleRE = regexp.MustCompile(`<article class="([^"]*)" data-account="([^"]*)" data-msgid="([^"]*)" data-depth="(\d+)"[^>]*>`)

func threadURL(t *testing.T, s http.Handler, target, subject string) string {
	t.Helper()
	return find(t, rows(t, s, target), subject).URL
}

func TestThread(t *testing.T) {
	s := newServer(t)
	body := getOK(t, s, threadURL(t, s, "/", "Cabin weekend"))
	if !strings.Contains(body, `<main class="thread" data-account="personal" data-thread="`) || !strings.Contains(body, "<h1>Cabin weekend in October?</h1>") {
		t.Fatalf("thread header: %s", body)
	}
	var got [][]string
	for _, m := range articleRE.FindAllStringSubmatch(body, -1) {
		got = append(got, m[1:])
	}
	want := [][]string{
		{"message collapsed", "personal", "CAH7x2Lq-cabin-1@mail.ortega.example", "0"},
		{"message collapsed", "personal", "CAJm4kR9-cabin-2@mail.gmail.com", "1"},
		{"message unread", "personal", "b7e1c0d2-44aa-4c1e-9d3e-cabin3@fastmail.example", "2"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("articles:\n got %q\nwant %q", got, want)
	}
	for _, s := range []string{
		`<span class="name">Priya Natarajan</span> <span class="addr">&lt;priya.n@fastmail.example&gt;</span>`,
		`<div class="cc">Cc: Priya Natarajan &lt;priya.n@fastmail.example&gt;</div>`,
		`data-gmail="https://mail.google.com/mail/?authuser=robin%40hale.example#all/19c8698f3401dced"`,
		// format=flowed joined, quote wrapped, both server-side.
		`<div class="body" data-kind="text"><pre>Yes please on the ride. I&#39;ll bring the board games and the good coffee. If we&#39;re doing`,
		"wrote:\n<blockquote class=\"q\">I&#39;m in. Book it",
		`<span class="pos">1 of 3</span>`,
		`<span class="pos">3 of 3</span>`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("cabin thread missing %s", s)
		}
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "<iframe") {
		t.Error("thread page carries script or iframe")
	}

	// Message-IDs with '/' are one path segment.
	body = getOK(t, s, threadURL(t, s, "/", "[northwind/app]"))
	if !strings.Contains(body, `data-msgid="northwind%2Fapp%2Fpull%2F4821@codehost.example"`) ||
		!strings.Contains(body, `data-gmail="https://mail.google.com/mail/?authuser=robin%40northwind.example#all/190c275d554c8086"`) {
		t.Errorf("pr thread: %s", body)
	}

	// HTML body is a placeholder; inline cid images aren't attachments, the rest are.
	body = getOK(t, s, threadURL(t, s, "/", "Photos from the trip"))
	for _, s := range []string{
		`<article class="message flagged collapsed" data-account="personal" data-msgid="5f2e9a10-trip-photos@fastmail.example" data-depth="0"`,
		`<div class="body" data-kind="html" data-body-url="/body/personal/5f2e9a10-trip-photos@fastmail.example"></div>`,
		// PDF and images open inline in a tab (the part endpoint serves them
		// inline); everything else downloads.
		`<li><a href="/part/personal/5f2e9a10-trip-photos@fastmail.example/8" target="_blank" rel="noopener" data-view="pdf">itinerary.pdf</a></li>`,
		`<li><a href="/part/personal/5f2e9a10-trip-photos@fastmail.example/9" download="packing-list.txt" data-view="text">packing-list.txt</a></li>`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("photos thread missing %s", s)
		}
	}
	if strings.Contains(body, "red.png") || strings.Contains(body, "blue.png") {
		t.Error("cid images listed as attachments")
	}

	// Calendar parts are attachments, even inside multipart/alternative.
	body = getOK(t, s, threadURL(t, s, "/", "Invitation: Dental"))
	if !strings.Contains(body, `data-kind="html"`) || !strings.Contains(body, `/5" download="part-5.ics" data-view="ics"`) || !strings.Contains(body, `/6" download="invite.ics" data-view="ics"`) {
		t.Errorf("dental thread: %s", body)
	}

	// A forward's text is folded in; its attachment is listed.
	body = getOK(t, s, threadURL(t, s, "/", "Fwd: Signed lease"))
	if !strings.Contains(body, "---------- Forwarded message ----------\nFrom: Graham Ellis &lt;graham.ellis@example.org&gt;") ||
		!strings.Contains(body, "Signed and attached.") || !strings.Contains(body, `/6" target="_blank" rel="noopener" data-view="pdf">addendum-signed.pdf`) {
		t.Errorf("fwd thread: %s", body)
	}

	// delsp=yes drops the soft-break space.
	body = getOK(t, s, threadURL(t, s, "/starred", "Notes from the offsite"))
	if !strings.Contains(body, "The big decision was to treat enterprise SSO as a Q3 commitment rather than a stretch goal, which means pulling") {
		t.Errorf("offsite not unwrapped: %s", body)
	}
}

func TestBody(t *testing.T) {
	s := newServer(t)
	w := do(s, "GET", "/body/personal/5f2e9a10-trip-photos@fastmail.example", withCookie)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	var got struct {
		HTML string            `json:"html"`
		CIDs map[string]string `json:"cids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.HTML, `src="cid:ii_red_01@fastmail.example"`) {
		t.Errorf("html %q", got.HTML)
	}
	want := map[string]string{
		"ii_red_01@fastmail.example":  "/part/personal/5f2e9a10-trip-photos@fastmail.example/6",
		"ii_blue_02@fastmail.example": "/part/personal/5f2e9a10-trip-photos@fastmail.example/7",
	}
	if len(got.CIDs) != 2 || got.CIDs["ii_red_01@fastmail.example"] != want["ii_red_01@fastmail.example"] || got.CIDs["ii_blue_02@fastmail.example"] != want["ii_blue_02@fastmail.example"] {
		t.Errorf("cids %v", got.CIDs)
	}

	// The hostile cid message, from the other database.
	w = do(s, "GET", "/body/work/hostile-15-cid@partner-agency.example", withCookie)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.CIDs["logo@hostile.test"] != "/part/work/hostile-15-cid@partner-agency.example/3" || !strings.Contains(got.HTML, "tracker.invalid") {
		t.Errorf("hostile-15: %d %v %v", w.Code, err, got.CIDs)
	}
	// Wrong database, text-only message: 404.
	for _, target := range []string{"/body/work/5f2e9a10-trip-photos@fastmail.example", "/body/personal/renee-cafe-20260714@francois.example"} {
		if w := do(s, "GET", target, withCookie); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}

func TestPart(t *testing.T) {
	s := newServer(t)
	const photos = "/part/personal/5f2e9a10-trip-photos@fastmail.example/"
	check := func(target, ctype, disp string, prefix string) {
		t.Helper()
		w := do(s, "GET", target, withCookie)
		h := w.Header()
		if w.Code != 200 || h.Get("Content-Type") != ctype || h.Get("Content-Disposition") != disp ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" ||
			!strings.HasPrefix(w.Body.String(), prefix) {
			t.Errorf("%s: %d %v %.20q", target, w.Code, h, w.Body.String())
		}
	}
	check(photos+"8", "application/pdf", "inline; filename*=UTF-8''itinerary.pdf", "%PDF-")
	check(photos+"6", "image/png", "inline; filename*=UTF-8''red.png", "\x89PNG")
	check(photos+"9", "text/plain; charset=utf-8", "attachment; filename*=UTF-8''packing-list.txt", "")
	check("/part/personal/appt-88421-invite@brightsmile.example/5", "text/calendar; charset=utf-8", "attachment; filename*=UTF-8''part-5.ics", "BEGIN:VCALENDAR")
	// Inside an embedded message/rfc822.
	check("/part/personal/DW-20260814094820-fwd@whitfield-property.example/6", "application/pdf", "inline; filename*=UTF-8''addendum-signed.pdf", "%PDF-")

	// Never text/html, whatever the message says.
	check(photos+"5", "text/plain; charset=utf-8", "attachment; filename*=UTF-8''part-5.html", "")
	check("/part/personal/hostile-19-mxss@acc0unts-verify.example/1", "text/plain; charset=utf-8", "attachment; filename*=UTF-8''part-1.html", "")
	if w := do(s, "GET", photos+"5", withCookie); !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Error("non-PDF part without sandbox CSP")
	}

	for _, n := range []string{"0", "99", "-1", "x", "2" /* multipart container */} {
		if w := do(s, "GET", photos+n, withCookie); w.Code != http.StatusNotFound {
			t.Errorf("part %s: %d", n, w.Code)
		}
	}
	// Right message, wrong database.
	if w := do(s, "GET", "/part/work/5f2e9a10-trip-photos@fastmail.example/8", withCookie); w.Code != http.StatusNotFound {
		t.Errorf("wrong account: %d", w.Code)
	}
}

func TestPartHeaders(t *testing.T) {
	cases := []struct {
		in, name, ct string
		inline       bool
	}{
		{"image/png", "a.png", "image/png", true},
		{"IMAGE/JPEG", "a.jpg", "image/jpeg", true},
		{"image/jpg", "a.jpg", "image/jpeg", true},
		{"image/svg+xml", "a.svg", "text/plain", false},
		{"text/html; charset=utf-8", "a.html", "text/plain; charset=utf-8", false},
		{"application/xhtml+xml", "a.xhtml", "text/plain", false},
		{"text/javascript", "a.js", "text/plain", false},
		{"application/pdf", "a.pdf", "application/pdf", true},
		{"application/zip; name=\"x.zip\"", "x.zip", "application/zip", false},
		{"garbage", "part-3", "application/octet-stream", false},
		{"", "part-3", "application/octet-stream", false},
		{"audio/mp3", "a.mp3", "audio/mpeg", true},
		{"video/mp4", "a.mp4", "video/mp4", true},
		// A generic type takes the name's; an active one is still neutered.
		{"application/octet-stream", "scan.PDF", "application/pdf", true},
		{"application/octet-stream", "x.svg", "text/plain", false},
		{"application/octet-stream", "x.html", "text/plain", false},
		{"application/octet-stream", "data.json", "application/json", false},
		{"application/octet-stream", "blob.bin", "application/octet-stream", false},
		// A specific type is never overridden by a name, except text/plain
		// into a text format.
		{"image/png", "evil.html", "image/png", true},
		{"text/plain; charset=iso-8859-1", "t.csv", "text/csv; charset=iso-8859-1", false},
		{"text/plain", "x.pdf", "text/plain", false},
	}
	for _, c := range cases {
		if ct, inline := partHeaders(c.in, c.name); ct != c.ct || inline != c.inline {
			t.Errorf("%q %q: %q %v", c.in, c.name, ct, inline)
		}
	}
	if got := rfc5987(`naïve "résumé"/x.pdf`); got != `na%C3%AFve%20%22r%C3%A9sum%C3%A9%22%2Fx.pdf` {
		t.Error(got)
	}
}

func TestGmailRedirect(t *testing.T) {
	s := newServer(t)
	r := find(t, rows(t, s, "/"), "Photos from the trip")
	if r.Gmail != "/gmail/personal/"+r.Thread {
		t.Fatalf("data-gmail %q", r.Gmail)
	}
	w := do(s, "GET", r.Gmail, withCookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "https://mail.google.com/mail/?authuser=robin%40hale.example#all/1902ba559a010deb" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	// Newest message of a multi-message thread.
	k := find(t, rows(t, s, "/"), "Kitchen quote")
	w = do(s, "GET", k.Gmail, withCookie)
	body := getOK(t, s, k.URL)
	links := regexp.MustCompile(`data-gmail="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if w.Code != http.StatusSeeOther || len(links) != 2 || w.Header().Get("Location") != links[1][1] {
		t.Fatalf("kitchen: %d %q, page links %q", w.Code, w.Header().Get("Location"), links)
	}
	for _, bad := range []string{"/gmail/personal/zz", "/gmail/nope/" + r.Thread, "/gmail/personal/00000000deadbeef"} {
		if w := do(s, "GET", bad, withCookie); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := do(s, "GET", r.Gmail, nil); w.Code == http.StatusSeeOther {
		t.Error("redirect without the cookie")
	}
}

func TestAttachmentInline(t *testing.T) {
	s := newServer(t)
	m := &notmuch.Message{ID: "a@b", Body: []notmuch.Part{{
		ID: 1, ContentType: "multipart/mixed", Children: []notmuch.Part{
			{ID: 2, ContentType: "text/plain", Content: "hi", HasContent: true},
			{ID: 3, ContentType: "image/jpeg", Filename: "photo.jpg"},
			{ID: 4, ContentType: "Application/PDF", Filename: "doc.pdf"},
			{ID: 5, ContentType: "image/svg+xml", Filename: "logo.svg"},
			{ID: 6, ContentType: "application/zip", Filename: "x.zip"},
		},
	}}}
	v := s.messageView(s.Accounts[0], m)
	got := map[string]bool{}
	for _, a := range v.Attach {
		got[a.Name] = a.Inline
	}
	want := map[string]bool{"photo.jpg": true, "doc.pdf": true, "logo.svg": false, "x.zip": false}
	if !maps.Equal(got, want) {
		t.Errorf("inline %v", got)
	}
}

// Remote images load by default; spam and trash keep click-to-load. The
// thread page never shows those today (excluded), so the view is built
// directly.
func TestHoldImages(t *testing.T) {
	s := newServer(t)
	acct := s.Accounts[0]
	for _, c := range []struct {
		tags []string
		hold bool
	}{{[]string{"inbox"}, false}, {[]string{"spam"}, true}, {[]string{"trash", "unread"}, true}} {
		m := notmuch.Message{ID: "hold@hostile.test", Tags: c.tags, Headers: map[string]string{},
			Body: []notmuch.Part{{ID: 1, ContentType: "text/html", Content: "<p>x</p>"}}}
		v := s.messageView(acct, &m)
		if v.HoldImages != c.hold {
			t.Errorf("%v: HoldImages = %v", c.tags, v.HoldImages)
		}
		w := httptest.NewRecorder()
		s.render(w, http.StatusOK, "thread", threadPage{Page: s.page(httptest.NewRequest("GET", "/", nil), "x", "", viewLabel{}), Account: acct.Name, Messages: []messageView{v}})
		if got := strings.Contains(w.Body.String(), ` data-remote-images="click"></div>`); got != c.hold {
			t.Errorf("%v: data-remote-images present = %v", c.tags, got)
		}
	}
	// /body/ answers with the tags as of now: a message tagged spam after
	// the thread page rendered must not load on the page's stale flag.
	id := "5f2e9a10-trip-photos@fastmail.example"
	hold := func() bool {
		w := do(s, "GET", "/body/personal/"+id, withCookie)
		var got struct {
			Hold bool `json:"hold"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(w.Code, err)
		}
		return got.Hold
	}
	if hold() {
		t.Error("inbox message: hold = true")
	}
	if err := acct.Tag(context.Background(), []string{"+spam", "-inbox"}, []string{id}); err != nil {
		t.Fatal(err)
	}
	if !hold() {
		t.Error("spam message: hold = false")
	}
}

// Sent names each thread's recipients (the newest sent message's To), and
// every list shows the account's own name as "me", once.
func TestSentAndMe(t *testing.T) {
	s := newServer(t)
	for path, want := range map[string][]string{
		"/sent": {
			`<span class="authors">To: Sam Ortega</span> <span class="subject">Cabin weekend`,
			`<span class="authors">To: me, Tessa Lund, Owen Pryce</span> <span class="subject">Q3 roadmap`,
			// work's W-9 went to the personal address: not this account's me.
			`<span class="authors">To: Robin Hale</span> <span class="subject">Contractor W-9`,
		},
		"/all": {
			`<span class="authors">Sam Ortega, me, Priya Natarajan</span>`,
			`<span class="authors">Owen Pryce, Tessa Lund, me</span>`,
		},
	} {
		body := getOK(t, s, path)
		if !strings.Contains(body, `<main class="list" data-view="`+path[1:]+`" `) {
			t.Errorf("%s: view missing", path)
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s missing %s", path, w)
			}
		}
	}
}

func TestAuthorsAndRecipients(t *testing.T) {
	me := self{"Robin Hale", "robin@hale.example"}
	for in, want := range map[string]string{
		"Robin Hale, Sam| Robin Hale, Priya": "me, Sam, Priya",
		"robin@hale.example| Sam":            "me, Sam",
		"Sam":                                "Sam",
		"":                                   "",
	} {
		if got := authors(in, me); got != want {
			t.Errorf("authors(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		`Sam <sam@x.example>, ROBIN@hale.example, bare@x.example`: "Sam, me, bare@x.example",
		"undisclosed-recipients:;":                                "undisclosed-recipients:;",
		"  ":                                                      "(no recipients)",
	} {
		if got := recipients(in, me); got != want {
			t.Errorf("recipients(%q) = %q, want %q", in, got, want)
		}
	}
}

// data-unsub marks a message with a List-Unsubscribe header, the key bar's
// cue for X; mail without one has no mark.
func TestThreadUnsubMark(t *testing.T) {
	s := newServer(t)
	if body := getOK(t, s, threadURL(t, s, "/", "The Weekend Reader")); !strings.Contains(body, ` data-unsub>`) {
		t.Errorf("newsletter thread: no data-unsub")
	}
	if body := getOK(t, s, threadURL(t, s, "/", "Cabin weekend")); strings.Contains(body, "data-unsub") {
		t.Errorf("cabin thread marked data-unsub")
	}
}
