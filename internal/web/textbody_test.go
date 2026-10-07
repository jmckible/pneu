package web

import (
	"slices"
	"strings"
	"testing"

	"github.com/jmckible/pneu/internal/notmuch"
)

func TestRenderTextEscaping(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<script>alert(1)</script> & "q" 'a'`, "&lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;q&#34; &#39;a&#39;\n"},
		{`</pre><img src=x onerror=alert(1)>`, "&lt;/pre&gt;&lt;img src=x onerror=alert(1)&gt;\n"},
		{"<blockquote class=\"q\">", "&lt;blockquote class=&#34;q&#34;&gt;\n"},
		{"", ""},
		{"a\r\nb\r\n", "a\nb\n"},
	}
	for _, c := range cases {
		if got := string(RenderText(c.in, false, false)); got != c.want {
			t.Errorf("%q:\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRenderTextLinks(t *testing.T) {
	a := func(u string) string { return `<a href="` + u + `" target="_blank" rel="noopener">` + u + `</a>` }
	cases := []struct{ in, want string }{
		{"see https://example.com/x.", "see " + a("https://example.com/x") + ".\n"},
		{"(http://example.com/a_(b))", "(" + a("http://example.com/a_(b)") + ")\n"},
		{"q https://e.com/?a=1&b=<2>", "q " + a("https://e.com/?a=1&amp;b=") + "&lt;2&gt;\n"},
		{`x https://e.com/"onmouseover="alert(1)`, "x " + a("https://e.com/") + "&#34;onmouseover=&#34;alert(1)\n"},
		{"javascript:alert(1) and ftp://x", "javascript:alert(1) and ftp://x\n"},
		{"bare https:// alone", "bare https:// alone\n"},
		{"HTTPS://E.COM, then", a("HTTPS://E.COM") + ", then\n"},
	}
	for _, c := range cases {
		if got := string(RenderText(c.in, false, false)); got != c.want {
			t.Errorf("%q:\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRenderTextQuotes(t *testing.T) {
	in := "Top\n> one\n> > two <b>\n>> two again\n> back\nend\n"
	want := "Top\n" +
		`<blockquote class="q">one` + "\n" +
		`<blockquote class="q">two &lt;b&gt;` + "\n" + "two again\n" + `</blockquote>` +
		"back\n" + `</blockquote>` +
		"end\n"
	if got := string(RenderText(in, false, false)); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// Unclosed quote at the end still balances.
	if got := string(RenderText(">> deep", false, false)); got != `<blockquote class="q"><blockquote class="q">deep`+"\n</blockquote></blockquote>" {
		t.Errorf("%q", got)
	}
	// Leading spaces before '>' are not a quote.
	if got := string(RenderText("  > code", false, false)); got != "  &gt; code\n" {
		t.Errorf("%q", got)
	}
}

func TestRenderTextFlowed(t *testing.T) {
	in := "This is a soft \nwrapped line.\n>quoted soft \n>continues\n>> deeper\n-- \nsig \nline\n \nx"
	want := "This is a soft wrapped line.\n" +
		`<blockquote class="q">quoted soft continues` + "\n" +
		`<blockquote class="q">deeper` + "\n" + `</blockquote></blockquote>` +
		"-- \nsig line\n" + "\nx\n"
	// A lone " " is space-stuffing around an empty line, not a soft break.
	if got := string(RenderText(in, true, false)); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	// delsp=yes removes the space at the soft break.
	if got := string(RenderText("Zusammen \ngeschrieben", true, true)); got != "Zusammengeschrieben\n" {
		t.Errorf("delsp: %q", got)
	}
	// A soft line followed by a different quote depth doesn't join.
	if got := string(RenderText("a \n>b", true, false)); got != "a \n"+`<blockquote class="q">b`+"\n</blockquote>" {
		t.Errorf("depth change: %q", got)
	}
}

func TestMatchedIDs(t *testing.T) {
	s := func(v string) *string { return &v }
	cases := []struct {
		in   *string
		want []string
	}{
		{nil, nil},
		{s("id:a@b"), []string{"a@b"}},
		{s("id:a@b id:c/d@e"), []string{"a@b", "c/d@e"}},
		{s(`id:"we""ird (x)@y" id:plain@z`), []string{`we"ird (x)@y`, "plain@z"}},
		{s(`thread:0001`), nil},
		{s(`id:"unterminated`), nil},
	}
	for _, c := range cases {
		if got := matchedIDs(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%v: %q", c.in, got)
		}
	}
}

func TestGmailURL(t *testing.T) {
	got := gmailURL("robin@hale.example", []string{"/mail/personal/gmail/mail/cur/19c8698f3401dced:2,S"})
	if got != "https://mail.google.com/mail/?authuser=robin%40hale.example#all/19c8698f3401dced" {
		t.Error(got)
	}
	// Not a lieer filename: no link rather than a wrong one.
	if got := gmailURL("x@y", []string{"/mail/cur/1700000000.M1P2.host:2,S"}); got != "" {
		t.Error(got)
	}
	if got := gmailURL("x@y", nil); got != "" {
		t.Error(got)
	}
}

func TestAnalyzeMultipartText(t *testing.T) {
	// mixed(text/plain body, inline text/plain footer, image without cid) -> footer folded, image attached.
	m := &notmuch.Message{Body: []notmuch.Part{{
		ID: 1, ContentType: "multipart/mixed", Children: []notmuch.Part{
			{ID: 2, ContentType: "text/plain", Content: "body", HasContent: true},
			{ID: 3, ContentType: "text/plain", Content: "footer <x>", HasContent: true},
			{ID: 4, ContentType: "image/png", ContentID: "unused@x"},
		},
	}}}
	a := analyze(m)
	if a.Kind != "text" || string(a.Text) != "body\n\nfooter &lt;x&gt;\n" {
		t.Errorf("text %q", a.Text)
	}
	if len(a.Attachments) != 1 || a.Attachments[0] != (attachment{4, "part-4.png", "image/png"}) {
		t.Errorf("attachments %v", a.Attachments)
	}
	// alternative(plain, related(html, cid image)) -> html body, referenced image hidden.
	m = &notmuch.Message{Body: []notmuch.Part{{
		ID: 1, ContentType: "multipart/alternative", Children: []notmuch.Part{
			{ID: 2, ContentType: "text/plain", Content: "plain", HasContent: true},
			{ID: 3, ContentType: "multipart/related", Children: []notmuch.Part{
				{ID: 4, ContentType: "text/html", Content: `<img src="CID:Logo@x">`, HasContent: true},
				{ID: 5, ContentType: "image/png", ContentID: "logo@x", Filename: "logo.png"},
			}},
		},
	}}}
	a = analyze(m)
	if a.Kind != "html" || a.Body.ID != 4 || len(a.Attachments) != 0 {
		t.Errorf("html analysis %+v", a)
	}
}

func TestRenderTextFold(t *testing.T) {
	const open = `<details class="qfold"><summary title="Show quoted text">···</summary>`
	cases := []struct {
		name, in, want string
	}{
		{"trailing quote with attribution",
			"Sounds good.\n\nOn Tue, Oct 7, 2026 at 3:00 PM Ann <a@x.example> wrote:\n> Lunch?\n>\n> Ann\n\n",
			"Sounds good.\n" + open + "\nOn Tue, Oct 7, 2026 at 3:00 PM Ann &lt;a@x.example&gt; wrote:\n" +
				`<blockquote class="q">Lunch?` + "\n\nAnn\n</blockquote>\n</details>"},
		{"wrapped attribution",
			"Yes.\nOn Tue, Oct 7, 2026 at 3:00 PM Ann <\na@x.example> wrote:\n> Lunch?\n",
			"Yes.\n" + open + "On Tue, Oct 7, 2026 at 3:00 PM Ann &lt;\na@x.example&gt; wrote:\n" +
				`<blockquote class="q">Lunch?` + "\n</blockquote></details>"},
		{"no attribution, nested",
			"Yes.\n> > deep\n> top\n",
			"Yes.\n" + open + `<blockquote class="q"><blockquote class="q">deep` + "\n</blockquote>top\n</blockquote></details>"},
		{"outlook copy",
			"Fine by me.\n\n-----Original Message-----\nFrom: Ann <a@x.example>\nSent: Tuesday\nSubject: RE: lunch\n\nLunch?\n",
			"Fine by me.\n" + open + "\n-----Original Message-----\nFrom: Ann &lt;a@x.example&gt;\nSent: Tuesday\nSubject: RE: lunch\n\nLunch?\n</details>"},
		{"outlook web copy",
			"Fine.\n________________________________\nFrom: Ann\nSent: Tuesday\n> x\n",
			"Fine.\n" + open + "________________________________\nFrom: Ann\nSent: Tuesday\n" + `<blockquote class="q">x` + "\n</blockquote></details>"},
		{"own colon line stays out",
			"Hi.\nHere is the list:\n> a\n> b\n",
			"Hi.\nHere is the list:\n" + open + `<blockquote class="q">a` + "\nb\n</blockquote></details>"},
		{"inline reply stays",
			"> Lunch?\nYes.\n> Where?\nThere.\n",
			`<blockquote class="q">Lunch?` + "\n</blockquote>Yes.\n" + `<blockquote class="q">Where?` + "\n</blockquote>There.\n"},
		{"all quote stays",
			"On Tue Ann wrote:\n> Lunch?\n",
			"On Tue Ann wrote:\n" + `<blockquote class="q">Lunch?` + "\n</blockquote>"},
		{"forward stays",
			"FYI\n\n---------- Forwarded message ---------\nFrom: Ann\nDate: Tue\nSubject: lunch\n> x\n",
			"FYI\n\n---------- Forwarded message ---------\nFrom: Ann\nDate: Tue\nSubject: lunch\n" + `<blockquote class="q">x` + "\n</blockquote>"},
		{"outlook forward stays",
			"FYI\n-----Original Message-----\nFrom: Ann\nSent: Tue\nSubject: FW: lunch\nbody\n",
			"FYI\n-----Original Message-----\nFrom: Ann\nSent: Tue\nSubject: FW: lunch\nbody\n"},
		{"apple forward stays",
			"FYI\nBegin forwarded message:\n> From: Ann\n",
			"FYI\nBegin forwarded message:\n" + `<blockquote class="q">From: Ann` + "\n</blockquote>"},
	}
	for _, c := range cases {
		if got := string(renderText(c.in, false, false, true)); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	// Without fold, nothing changes.
	in := cases[0].in
	if got, want := string(renderText(in, false, false, false)), string(RenderText(in, false, false)); got != want || strings.Contains(got, "details") {
		t.Errorf("unfolded: %q", got)
	}
}
