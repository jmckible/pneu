package testmail

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jmckible/pneu/testdata"
)

// Message-ids referenced by several tests.
const (
	cidMsg      = "5f2e9a10-trip-photos@fastmail.example"
	fwdMsg      = "DW-20260814094820-fwd@whitfield-property.example"
	latin1Msg   = "3f9a.juergen.20260505@mueller.example"
	crossMsg    = "CAKv0c-w9-crossacct@mail.gmail.com"
	cabinReply  = "b7e1c0d2-44aa-4c1e-9d3e-cabin3@fastmail.example"
	roadmapLast = "rm-q3-04-owen@northwind.example"
	hostileMXSS = "hostile-19-mxss@acc0unts-verify.example"
	hostileCID  = "hostile-15-cid@partner-agency.example"
)

func TestManifestApplied(t *testing.T) {
	env := Setup(t)
	manifest, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"personal": 21, "work": 18}
	for _, a := range env.Accounts {
		if got := len(manifest[a.Name]); got != want[a.Name] {
			t.Errorf("%s: manifest has %d messages, want %d", a.Name, got, want[a.Name])
		}
		// dump covers every message regardless of exclude_tags.
		got := map[string][]string{}
		sc := bufio.NewScanner(bytes.NewReader(a.Notmuch(t, "dump", "--format=batch-tag")))
		for sc.Scan() {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			tagPart, query, ok := strings.Cut(line, " -- ")
			if !ok {
				t.Fatalf("unexpected dump line %q", line)
			}
			id := strings.TrimPrefix(query, "id:")
			if unq, err := url.PathUnescape(id); err == nil {
				id = unq
			}
			var tags []string
			for _, f := range strings.Fields(tagPart) {
				if tag := strings.TrimPrefix(f, "+"); tag != "attachment" {
					tags = append(tags, tag)
				}
			}
			got[id] = tags
		}
		if len(got) != len(manifest[a.Name]) {
			t.Errorf("%s: database has %d messages, manifest %d", a.Name, len(got), len(manifest[a.Name]))
		}
		for id, wantTags := range manifest[a.Name] {
			gotTags, ok := got[id]
			if !ok {
				t.Errorf("%s: %s not indexed", a.Name, id)
				continue
			}
			w := slices.Clone(wantTags)
			sort.Strings(w)
			sort.Strings(gotTags)
			if !slices.Equal(w, gotTags) {
				t.Errorf("%s: %s tags %v, want %v", a.Name, id, gotTags, w)
			}
		}
	}
}

type searchThread struct {
	Thread  string   `json:"thread"`
	Subject string   `json:"subject"`
	Matched int      `json:"matched"`
	Total   int      `json:"total"`
	Tags    []string `json:"tags"`
	Query   []any    `json:"query"`
}

func search(t *testing.T, a Account, args ...string) []searchThread {
	t.Helper()
	var out []searchThread
	if err := json.Unmarshal(a.Notmuch(t, append([]string{"search", "--format=json"}, args...)...), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInboxThreads(t *testing.T) {
	env := Setup(t)
	want := map[string][]string{
		"personal": {
			"Action required: verify your mailbox",
			"",
			"The Weekend Reader - Issue 112: the case for boring software",
			"Invitation: Dental cleaning @ Thu Oct 15, 2026 9am - 10am (PDT)",
			"Photos from the trip + the itinerary",
			"Your trip to Lisbon: confirmation QK7P2M",
			"Fwd: Signed lease addendum",
			"Contractor W-9 for my records",
			"Café à Montréal — dimanche ?",
			"Kitchen quote - revised",
			"🎉 You're invited: Maya turns 40",
			"Cabin weekend in October?",
			"Reminder: the annual homeowners association meeting has been rescheduled from Tuesday the 14th to Thursday the 23rd because the community center is being repainted and the board needs quorum to vote on the landscaping contract, the pool resurfacing, and the new guest parking rules",
		},
		"work": {
			"[northwind/app] SSO: SAML metadata upload (PR #4821)",
			"New logo lockup for review",
			"Deploy summary for 2026-09-18",
			"Escalation: exports stuck at 99% since this morning",
			"Übersetzungen für die Oberfläche — Polnisch fertig",
			"Q3 roadmap review",
		},
	}
	for _, a := range env.Accounts {
		threads := search(t, a, "tag:inbox")
		var got []string
		for _, th := range threads {
			got = append(got, th.Subject)
		}
		// Newest first; the thread subject is the oldest message's.
		if !slices.Equal(got, want[a.Name]) {
			t.Errorf("%s inbox threads:\n got %q\nwant %q", a.Name, got, want[a.Name])
		}
	}
	// Threads span archived and sent messages: matched counts inbox messages only.
	for _, th := range search(t, env.Account(t, "personal"), "tag:inbox", "and", "subject:Cabin") {
		if th.Matched != 1 || th.Total != 3 {
			t.Errorf("cabin thread matched/total = %d/%d, want 1/3", th.Matched, th.Total)
		}
	}
}

func TestExcludeTags(t *testing.T) {
	env := Setup(t)
	excluded := map[string][]string{
		"personal": {"x9q7w.rewards.20260920@rewards-center.example", "digest-2026-37-8812@neighbors.example"},
		"work":     {"seo-outreach-99812@seo-growth.example", "trial-ending-5521@formstack-trials.example"},
	}
	wantThreads := map[string]int{"personal": 16, "work": 11}
	for _, a := range env.Accounts {
		ids := strings.Fields(string(a.Notmuch(t, "search", "--output=messages", "*")))
		for _, id := range excluded[a.Name] {
			if slices.Contains(ids, "id:"+id) {
				t.Errorf("%s: %s not excluded from *", a.Name, id)
			}
		}
		if n := len(search(t, a, "*")); n != wantThreads[a.Name] {
			t.Errorf("%s: * has %d threads, want %d", a.Name, n, wantThreads[a.Name])
		}
		for _, tag := range []string{"trash", "spam"} {
			got := strings.Fields(string(a.Notmuch(t, "search", "--output=messages", "tag:"+tag)))
			if len(got) != 1 {
				t.Errorf("%s: tag:%s = %v, want one message", a.Name, tag, got)
			}
		}
	}
}

func TestCrossAccountAndDates(t *testing.T) {
	env := Setup(t)
	p, v := env.Account(t, "personal"), env.Account(t, "work")
	pt := search(t, p, QuoteID(crossMsg))
	vt := search(t, v, QuoteID(crossMsg))
	if len(pt) != 1 || len(vt) != 1 {
		t.Fatalf("cross-account message: personal %d, work %d threads", len(pt), len(vt))
	}
	if !slices.Contains(pt[0].Tags, "inbox") || slices.Contains(vt[0].Tags, "inbox") || !slices.Contains(vt[0].Tags, "sent") {
		t.Errorf("cross-account tags: personal %v, work %v", pt[0].Tags, vt[0].Tags)
	}
	// Two messages share a Date across accounts, so a merged sort needs a tiebreak.
	pm := showMessage(t, p, "renee-cafe-20260714@francois.example", "--body=false")
	vm := showMessage(t, v, "lw-translations-pl-20260714@tlumacze.example", "--body=false")
	if pm["timestamp"] != vm["timestamp"] {
		t.Errorf("identical-Date pair has timestamps %v and %v", pm["timestamp"], vm["timestamp"])
	}
}

// showMessage returns the single message object for id.
func showMessage(t *testing.T, a Account, id string, extra ...string) map[string]any {
	t.Helper()
	args := append([]string{"show", "--format=json", "--entire-thread=false"}, extra...)
	var out [][][]any // [thread][node][message, replies]
	if err := json.Unmarshal(a.Notmuch(t, append(args, QuoteID(id))...), &out); err != nil {
		t.Fatal(err)
	}
	return out[0][0][0].(map[string]any)
}

// parts flattens a body tree depth-first, descending into message/rfc822.
func parts(body []any) []map[string]any {
	var out []map[string]any
	for _, n := range body {
		p := n.(map[string]any)
		out = append(out, p)
		kids, _ := p["content"].([]any)
		if p["content-type"] == "message/rfc822" {
			for _, m := range kids {
				out = append(out, parts(m.(map[string]any)["body"].([]any))...)
			}
		} else {
			out = append(out, parts(kids)...)
		}
	}
	return out
}

func partID(p map[string]any) int { return int(p["id"].(float64)) }

func TestShowCIDParts(t *testing.T) {
	env := Setup(t)
	a := env.Account(t, "personal")

	msg := showMessage(t, a, cidMsg, "--include-html")
	var html map[string]any
	var cids []string
	for _, p := range parts(msg["body"].([]any)) {
		if p["content-type"] == "text/html" {
			html = p
		}
		if cid, ok := p["content-id"].(string); ok {
			cids = append(cids, strconv.Itoa(partID(p))+"="+cid)
		}
	}
	if html == nil {
		t.Fatal("no text/html part")
	}
	if s, _ := html["content"].(string); !strings.Contains(s, `src="cid:ii_red_01@fastmail.example"`) {
		t.Errorf("html content missing cid reference: %q", s)
	}
	// content-id is exposed without angle brackets.
	want := []string{"6=ii_red_01@fastmail.example", "7=ii_blue_02@fastmail.example"}
	if !slices.Equal(cids, want) {
		t.Errorf("content-id parts %v, want %v", cids, want)
	}

	// Without --include-html the html part carries no content, only metadata.
	for _, p := range parts(showMessage(t, a, cidMsg)["body"].([]any)) {
		if p["content-type"] == "text/html" {
			if _, ok := p["content"]; ok {
				t.Error("html content present without --include-html")
			}
			if p["content-charset"] != "utf-8" {
				t.Errorf("html part without content: charset %v", p["content-charset"])
			}
		}
	}
}

func TestRawPartIsTransferDecoded(t *testing.T) {
	env := Setup(t)
	a := env.Account(t, "personal")

	check := func(id string, wantPart int, wantName string) {
		t.Helper()
		var pdf map[string]any
		for _, p := range parts(showMessage(t, a, id)["body"].([]any)) {
			if p["content-type"] == "application/pdf" {
				pdf = p
			}
		}
		if pdf == nil || partID(pdf) != wantPart || pdf["filename"] != wantName {
			t.Fatalf("%s: pdf part %v, want id %d %s", id, pdf, wantPart, wantName)
		}
		raw := a.Notmuch(t, "show", "--format=raw", "--part="+strconv.Itoa(wantPart), QuoteID(id))
		if !bytes.HasPrefix(raw, []byte("%PDF-")) {
			t.Fatalf("%s part %d: raw starts %q, want %%PDF- (transfer-decoded)", id, wantPart, raw[:min(len(raw), 16)])
		}
		if !bytes.HasSuffix(raw, []byte("%%EOF\n")) {
			t.Errorf("%s part %d: raw is not the whole PDF", id, wantPart)
		}
		// content-length is the ENCODED (base64) size, not the decoded size.
		if cl := int(pdf["content-length"].(float64)); cl <= len(raw) {
			t.Errorf("content-length %d <= decoded %d; expected encoded size", cl, len(raw))
		}
	}
	check(cidMsg, 8, "itinerary.pdf")
	// Inside message/rfc822: numbering continues through the embedded message.
	check(fwdMsg, 6, "addendum-signed.pdf")

	// Raw text parts are transfer-decoded but NOT charset-converted; JSON content is UTF-8.
	raw := a.Notmuch(t, "show", "--format=raw", "--part=1", QuoteID(latin1Msg))
	if !bytes.Contains(raw, []byte("Gr\xfc\xdfe")) {
		t.Errorf("latin1 raw part: want ISO-8859-1 bytes, got %q", raw[:40])
	}
	body := showMessage(t, a, latin1Msg)["body"].([]any)
	if s := body[0].(map[string]any)["content"].(string); !strings.Contains(s, "Grüße") {
		t.Errorf("latin1 JSON content not UTF-8: %q", s[:40])
	}
}

func TestReplyIdentity(t *testing.T) {
	env := Setup(t)
	type reply struct {
		Headers map[string]string `json:"reply-headers"`
	}
	get := func(a Account, id string) map[string]string {
		var r reply
		if err := json.Unmarshal(a.Notmuch(t, "reply", "--format=json", QuoteID(id)), &r); err != nil {
			t.Fatal(err)
		}
		return r.Headers
	}
	cases := []struct{ account, id, from string }{
		{"personal", cabinReply, "Robin Hale <robin@hale.example>"},
		{"work", roadmapLast, "Robin Hale <robin@northwind.example>"},
		// A message the owner sent from the *other* address: notmuch prefers the
		// user address it finds in From, so the personal database answers with the
		// work identity. Callers that must send from the answering account have to
		// override From themselves.
		{"personal", crossMsg, "Robin Hale <robin@northwind.example>"},
	}
	for _, c := range cases {
		h := get(env.Account(t, c.account), c.id)
		if h["From"] != c.from {
			t.Errorf("%s %s: reply From %q, want %q", c.account, c.id, h["From"], c.from)
		}
		// Header key is "In-reply-to", not "In-Reply-To".
		if h["In-reply-to"] != "<"+c.id+">" {
			t.Errorf("%s %s: In-reply-to %q", c.account, c.id, h["In-reply-to"])
		}
	}
}

func TestHostileMessagesCarryCorpus(t *testing.T) {
	env := Setup(t)
	cases := []struct{ account, id, file string }{
		{"personal", hostileMXSS, "19-mxss-form-math.html"},
		{"work", hostileCID, "15-cid-image.html"},
	}
	for _, c := range cases {
		want, err := testdata.FS.ReadFile("hostile/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		msg := showMessage(t, env.Account(t, c.account), c.id, "--include-html")
		var got string
		for _, p := range parts(msg["body"].([]any)) {
			if p["content-type"] == "text/html" {
				got, _ = p["content"].(string)
			}
		}
		if strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
			t.Errorf("%s: html part differs from hostile/%s", c.id, c.file)
		}
	}
}

// Every corpus file is documented in the README table, and vice versa.
func TestHostileReadmeCoversCorpus(t *testing.T) {
	readme, err := testdata.FS.ReadFile("hostile/README.md")
	if err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(testdata.FS, "hostile/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 30 {
		t.Errorf("only %d hostile cases", len(files))
	}
	for _, f := range files {
		if !bytes.Contains(readme, []byte("`"+path.Base(f)+"`")) {
			t.Errorf("README.md does not document %s", path.Base(f))
		}
	}
}
