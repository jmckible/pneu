package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
)

const (
	cabin1    = "CAH7x2Lq-cabin-1@mail.ortega.example"
	cabin3    = "b7e1c0d2-44aa-4c1e-9d3e-cabin3@fastmail.example"
	crossAcct = "CAKv0c-w9-crossacct@mail.gmail.com"
	weekender = "issue-112.a8c1f0@send.weekendreader.example"
)

// form is a parsed compose page: what the browser would submit, plus the
// attributes the browser task codes against.
type composeForm struct {
	values   url.Values
	disabled bool     // the account select
	options  []string // account option values
	labels   []string
	draftKey string
	quote    string // data-quote-from
	err      string
}

var (
	inputRE    = regexp.MustCompile(`<input\b[^>]*>`)
	attrRE     = regexp.MustCompile(`\b([a-z_-]+)="([^"]*)"`)
	optionRE   = regexp.MustCompile(`<option value="([^"]*)"( selected)?>([^<]*)</option>`)
	textareaRE = regexp.MustCompile(`(?s)<textarea\b([^>]*)>\n(.*?)</textarea>`)
	selectRE   = regexp.MustCompile(`<select\b[^>]*name="account"[^>]*>`)
	formRE     = regexp.MustCompile(`<form\b[^>]*action="/send"[^>]*>`)
	errRE      = regexp.MustCompile(`<p class="error"[^>]*>([^<]*)</p>`)
)

func attrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRE.FindAllStringSubmatch(tag, -1) {
		out[m[1]] = html.UnescapeString(m[2])
	}
	return out
}

func parseForm(t *testing.T, body string) composeForm {
	t.Helper()
	if !strings.Contains(body, `<main class="compose">`) {
		t.Fatalf("not a compose page:\n%s", body)
	}
	f := composeForm{values: url.Values{}}
	for _, tag := range inputRE.FindAllString(body, -1) {
		a := attrs(tag)
		if a["name"] != "" && a["name"] != "q" { // q: base.html's search box
			f.values.Add(a["name"], a["value"])
		}
	}
	sel := selectRE.FindString(body)
	f.disabled = strings.Contains(sel, " disabled")
	for _, m := range optionRE.FindAllStringSubmatch(body, -1) {
		f.options = append(f.options, m[1])
		f.labels = append(f.labels, html.UnescapeString(m[3]))
		if m[2] != "" && !f.disabled {
			f.values.Set("account", m[1])
		}
	}
	ta := textareaRE.FindStringSubmatch(body)
	if ta == nil {
		t.Fatalf("no textarea:\n%s", body)
	}
	f.values.Set("body", html.UnescapeString(ta[2]))
	f.quote = attrs(ta[1])["data-quote-from"]
	f.draftKey = attrs(formRE.FindString(body))["data-draft-key"]
	if m := errRE.FindStringSubmatch(body); m != nil {
		f.err = html.UnescapeString(m[1])
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, " on") && regexp.MustCompile(` on[a-z]+=`).MatchString(body) {
		t.Error("inline script in compose page")
	}
	return f
}

func get(t *testing.T, s http.Handler, target string) (int, string) {
	t.Helper()
	w := do(s, "GET", target, withCookie)
	return w.Code, w.Body.String()
}

func postSend(s http.Handler, v url.Values) *httptest.ResponseRecorder {
	return post(s, v, func(r *http.Request) { r.URL.Path = "/send"; r.RequestURI = "/send" })
}

func replyForm(t *testing.T, s http.Handler, account, id string, all bool) composeForm {
	t.Helper()
	target := "/reply/" + account + "/" + url.PathEscape(id)
	if all {
		target += "?all=1"
	}
	code, body := get(t, s, target)
	if code != http.StatusOK {
		t.Fatalf("%s: %d\n%s", target, code, body)
	}
	return parseForm(t, body)
}

func TestReplyTextOriginal(t *testing.T) {
	fx := newTagFixture(t)
	f := replyForm(t, fx.s, "personal", cabin3, false)
	v := f.values
	if got := v.Get("to"); got != "Priya Natarajan <priya.n@fastmail.example>" {
		t.Errorf("to = %q", got)
	}
	if v.Get("cc") != "" {
		t.Errorf("reply to sender has cc %q", v.Get("cc"))
	}
	if v.Get("subject") != "Re: Cabin weekend in October?" {
		t.Errorf("subject = %q", v.Get("subject"))
	}
	if v.Get("in_reply_to") != "<"+cabin3+">" {
		t.Errorf("in_reply_to = %q", v.Get("in_reply_to"))
	}
	if refs := v.Get("references"); !strings.HasPrefix(refs, "<"+cabin1+">") || !strings.HasSuffix(refs, "<"+cabin3+">") {
		t.Errorf("references = %q", refs)
	}
	if !f.disabled || v.Get("account") != "personal" || len(v["account"]) != 1 {
		t.Errorf("account not locked to personal: disabled=%v %v", f.disabled, v["account"])
	}
	if f.draftKey != "reply:personal:"+cabin3 || f.quote != "" {
		t.Errorf("draft key %q, quote-from %q", f.draftKey, f.quote)
	}
	if !generatedIDRE.MatchString(v.Get("message_id")) {
		t.Errorf("message_id = %q", v.Get("message_id"))
	}
	body := v.Get("body")
	for _, want := range []string{
		"\n\nOn ",
		" Priya Natarajan <priya.n@fastmail.example> wrote:\n> Yes please on the ride. I'll bring the board games and the good coffee.\n> If we're doing",
		">\n> On 6/13/26 8:15 AM, Robin Hale wrote:\n> > I'm in. Book it and I'll send my third.\n> >\n> > I can drive",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "\n\nOn ") {
		t.Errorf("body should open with room to type:\n%q", body)
	}
}

// The personal database holds a message the owner sent from the work
// address. notmuch reply answers as work; the page must send as personal.
func TestReplyFromIsTheAccount(t *testing.T) {
	fx := newTagFixture(t)
	for _, c := range []struct{ account, email string }{{"personal", "robin@hale.example"}, {"work", "robin@northwind.example"}} {
		f := replyForm(t, fx.s, c.account, crossAcct, false)
		if f.values.Get("account") != c.account {
			t.Fatalf("%s: account %v", c.account, f.values["account"])
		}
		i := indexOf(f.options, c.account)
		if i < 0 || f.labels[i] != "Robin Hale <"+c.email+">" {
			t.Errorf("%s: labels %v", c.account, f.labels)
		}
		w := postSend(fx.s, f.values)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: send %d\n%s", c.account, w.Code, w.Body)
		}
		msg := lastSent(t, fx.sync, c.account)
		if from := msg.Header.Get("From"); from != "Robin Hale <"+c.email+">" {
			t.Errorf("%s: From %q", c.account, from)
		}
	}
}

func TestReplyAll(t *testing.T) {
	fx := newTagFixture(t)
	f := replyForm(t, fx.s, "personal", cabin1, true)
	if f.values.Get("to") != "Sam Ortega <sam@ortega.example>" || f.values.Get("cc") != "Priya Natarajan <priya.n@fastmail.example>" {
		t.Errorf("reply-all to %q cc %q", f.values.Get("to"), f.values.Get("cc"))
	}
	f = replyForm(t, fx.s, "personal", cabin1, false)
	if f.values.Get("cc") != "" {
		t.Errorf("reply to sender cc %q", f.values.Get("cc"))
	}
}

func TestReplyHTMLOnly(t *testing.T) {
	fx := newTagFixture(t)
	f := replyForm(t, fx.s, "personal", weekender, false)
	if f.quote != "/body/personal/"+url.PathEscape(weekender) {
		t.Errorf("data-quote-from = %q", f.quote)
	}
	body := f.values.Get("body")
	if !strings.HasSuffix(body, " The Weekend Reader <issue@weekendreader.example> wrote:\n") || strings.Contains(body, "\n>") {
		t.Errorf("html-only body %q: want attribution and no quote", body)
	}
	// multipart/alternative quotes its text/plain half, server side.
	f = replyForm(t, fx.s, "personal", kitchen2, false)
	if f.quote != "" || !strings.Contains(f.values.Get("body"), "\n> ") {
		t.Errorf("alternative: quote-from %q body %q", f.quote, f.values.Get("body"))
	}
}

func TestReplyNotFound(t *testing.T) {
	fx := newTagFixture(t)
	for _, target := range []string{
		"/reply/personal/" + url.PathEscape("nope@nowhere.example"),
		"/reply/work/" + url.PathEscape(cabin3), // other account's message
		"/reply/nobody/" + url.PathEscape(cabin3),
	} {
		if code, body := get(t, fx.s, target); code != http.StatusNotFound {
			t.Errorf("%s: %d\n%s", target, code, body)
		}
	}
}

func TestComposePage(t *testing.T) {
	fx := newTagFixture(t)
	for q, want := range map[string]string{"": "personal", "?account=work": "work", "?account=bogus": "personal"} {
		code, body := get(t, fx.s, "/compose"+q)
		if code != http.StatusOK {
			t.Fatalf("%q: %d", q, code)
		}
		f := parseForm(t, body)
		if strings.Join(f.options, ",") != "personal,work" || f.disabled {
			t.Errorf("%q: options %v disabled %v", q, f.options, f.disabled)
		}
		if f.labels[0] != "Robin Hale <robin@hale.example>" || f.labels[1] != "Robin Hale <robin@northwind.example>" {
			t.Errorf("labels %v", f.labels)
		}
		if f.values.Get("account") != want || f.draftKey != "compose:"+want {
			t.Errorf("%q: account %q draft %q", q, f.values.Get("account"), f.draftKey)
		}
		if f.values.Get("in_reply_to") != "" || f.values.Get("body") != "" {
			t.Errorf("%q: not empty: %v", q, f.values)
		}
		if !strings.Contains(body, `data-addresses="/addresses"`) {
			t.Error("to lacks data-addresses")
		}
	}
}

func lastSent(t *testing.T, f *fakeSyncer, account string) *mail.Message {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("nothing sent")
	}
	s := f.sent[len(f.sent)-1]
	if s.account != account {
		t.Fatalf("sent through %q, want %q", s.account, account)
	}
	if bytes.Contains(bytes.ReplaceAll(s.raw, []byte("\r\n"), nil), []byte("\n")) || bytes.Contains(bytes.ReplaceAll(s.raw, []byte("\r\n"), nil), []byte("\r")) {
		t.Errorf("bare LF or CR in message:\n%q", s.raw)
	}
	for line := range bytes.SplitSeq(s.raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Errorf("line of %d octets", len(line))
		}
	}
	m, err := mail.ReadMessage(bytes.NewReader(s.raw))
	if err != nil {
		t.Fatalf("parse sent message: %v\n%s", err, s.raw)
	}
	return m
}

func (f *fakeSyncer) sends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func TestSendBuildsMessage(t *testing.T) {
	fx := newTagFixture(t)
	f := replyForm(t, fx.s, "personal", cabin3, false)
	v := f.values
	v.Set("to", "René François <renee@francois.example>, sam@ortega.example,")
	v.Set("cc", `"Natarajan, Priya" <priya.n@fastmail.example>`)
	v.Set("bcc", "robin@northwind.example")
	v.Set("subject", "Re: Café à Montréal — dimanche ?")
	typed := "Sounds good — see you there.\r\n\r\nLine three\r\n" + v.Get("body")
	v.Set("body", typed)
	before := time.Now().Add(-time.Second)

	w := postSend(fx.s, v)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("send: %d\n%s", w.Code, w.Body)
	}
	thread := strings.TrimSpace(string(fx.env.Account(t, "personal").Notmuch(t, "search", "--output=threads", "--", "id:"+cabin3)))
	if loc := w.Header().Get("Location"); loc != "/t/personal/"+strings.TrimPrefix(thread, "thread:")+"#sent" {
		t.Errorf("redirect %q, thread %q", loc, thread)
	}

	m := lastSent(t, fx.sync, "personal")
	h := m.Header
	if h.Get("From") != "Robin Hale <robin@hale.example>" {
		t.Errorf("From %q", h.Get("From"))
	}
	to, err := h.AddressList("To")
	if err != nil || len(to) != 2 || to[0].Name != "René François" || to[0].Address != "renee@francois.example" || to[1].Address != "sam@ortega.example" {
		t.Errorf("To %q: %v %v", h.Get("To"), to, err)
	}
	if !strings.Contains(h.Get("To"), "=?utf-8?q?Ren=C3=A9_Fran=C3=A7ois?=") {
		t.Errorf("To name not Q-encoded: %q", h.Get("To"))
	}
	if cc, err := h.AddressList("Cc"); err != nil || len(cc) != 1 || cc[0].Name != "Natarajan, Priya" {
		t.Errorf("Cc %q: %v %v", h.Get("Cc"), cc, err)
	}
	if bcc, err := h.AddressList("Bcc"); err != nil || len(bcc) != 1 || bcc[0].Address != "robin@northwind.example" {
		t.Errorf("Bcc %q", h.Get("Bcc"))
	}
	raw := h.Get("Subject")
	if !strings.HasPrefix(raw, "=?utf-8?q?") {
		t.Errorf("Subject not Q-encoded: %q", raw)
	}
	if subj, err := new(mime.WordDecoder).DecodeHeader(raw); err != nil || subj != "Re: Café à Montréal — dimanche ?" {
		t.Errorf("Subject decodes to %q (%v)", subj, err)
	}
	if d, err := h.Date(); err != nil || d.Before(before) || d.After(time.Now().Add(time.Second)) {
		t.Errorf("Date %q: %v", h.Get("Date"), err)
	}
	if h.Get("Message-ID") != v.Get("message_id") {
		t.Errorf("Message-ID %q, form had %q", h.Get("Message-ID"), v.Get("message_id"))
	}
	if h.Get("In-Reply-To") != "<"+cabin3+">" || h.Get("References") != v.Get("references") {
		t.Errorf("In-Reply-To %q References %q", h.Get("In-Reply-To"), h.Get("References"))
	}
	// A quote goes out as multipart/alternative: the typed text verbatim,
	// then HTML with the quote in Gmail's foldable markup.
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if h.Get("MIME-Version") != "1.0" || err != nil || mt != "multipart/alternative" {
		t.Fatalf("MIME headers %v", h)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	parts := map[string]string{}
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.Header.Get("Content-Transfer-Encoding") != "8bit" {
			t.Errorf("part CTE %v", p.Header)
		}
		b, _ := io.ReadAll(p)
		parts[p.Header.Get("Content-Type")] = string(b)
	}
	want := strings.ReplaceAll(strings.ReplaceAll(typed, "\r\n", "\n"), "\n", "\r\n")
	if got := parts["text/plain; charset=utf-8"]; got != want {
		t.Errorf("text body\n%q\nwant\n%q", got, want)
	}
	html := parts["text/html; charset=utf-8"]
	for _, frag := range []string{
		"Sounds good — see you there.<br>\r\n<br>\r\nLine three<br>",
		`<div class="gmail_quote"><div dir="ltr" class="gmail_attr">On `,
		` Priya Natarajan &lt;priya.n@fastmail.example&gt; wrote:<br></div><blockquote class="gmail_quote" style="`,
		`">Yes please on the ride. I&#39;ll bring`,
		"On 6/13/26 8:15 AM, Robin Hale wrote:<br>\r\n<blockquote",
		"from Seattle.<br>\r\n</blockquote></blockquote></div>\r\n",
	} {
		if !strings.Contains(html, frag) {
			t.Errorf("html lacks %q:\n%s", frag, html)
		}
	}
	if strings.Count(html, `class="gmail_quote"`) != 3 {
		t.Errorf("want one gmail_quote div and two blockquotes:\n%s", html)
	}

	// A resubmit of the same form (reload, double click, a restored draft
	// reusing the id) doesn't send twice, and still lands as sent.
	w = postSend(fx.s, v)
	if w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "#sent") || fx.sync.sends() != 1 {
		t.Errorf("resubmit: %d, %d sends", w.Code, fx.sync.sends())
	}

	// A new message goes home, ASCII stays unencoded.
	code, page := get(t, fx.s, "/compose?account=work")
	if code != 200 {
		t.Fatal(code)
	}
	c := parseForm(t, page).values
	c.Set("to", "tessa@northwind.example")
	c.Set("subject", "Plain subject")
	c.Set("body", "hi")
	if w := postSend(fx.s, c); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/#sent" {
		t.Fatalf("compose send: %d %q", w.Code, w.Header().Get("Location"))
	}
	m = lastSent(t, fx.sync, "work")
	if m.Header.Get("Subject") != "Plain subject" || m.Header.Get("From") != "Robin Hale <robin@northwind.example>" ||
		m.Header.Get("In-Reply-To") != "" || m.Header.Get("Content-Transfer-Encoding") != "7bit" {
		t.Errorf("compose headers %v", m.Header)
	}
	if b, _ := io.ReadAll(m.Body); string(b) != "hi\r\n" {
		t.Errorf("compose body %q", b)
	}
}

func TestSendRejects(t *testing.T) {
	fx := newTagFixture(t)
	base := replyForm(t, fx.s, "personal", cabin3, false).values
	typed := "my carefully typed reply <b>& stuff</b>\n\n" + base.Get("body")
	cases := []struct {
		name   string
		mod    func(url.Values)
		status int
		err    string
	}{
		{"bad address", func(v url.Values) { v.Set("to", "Sam Ortega <sam@") }, 400, "To:"},
		{"no recipients", func(v url.Values) { v.Set("to", " , ") }, 400, "No recipients"},
		{"bad cc", func(v url.Values) { v.Set("cc", "not an address") }, 400, "Cc:"},
		{"header injection", func(v url.Values) { v.Set("in_reply_to", "<a@b>\r\nBcc: evil@example.com") }, 400, "In-Reply-To"},
		{"bad references", func(v url.Values) { v.Set("references", "<a@b> junk") }, 400, "References"},
		{"unknown account", func(v url.Values) { v.Set("account", "nobody") }, 400, "Unknown account"},
	}
	for _, c := range cases {
		v := url.Values{}
		for k, vs := range base {
			v[k] = append([]string(nil), vs...)
		}
		v.Set("body", typed)
		c.mod(v)
		w := postSend(fx.s, v)
		if w.Code != c.status {
			t.Errorf("%s: %d\n%s", c.name, w.Code, w.Body)
			continue
		}
		f := parseForm(t, w.Body.String())
		if !strings.Contains(f.err, c.err) {
			t.Errorf("%s: error %q", c.name, f.err)
		}
		if f.values.Get("body") != typed || f.values.Get("subject") != base.Get("subject") {
			t.Errorf("%s: typed text lost: %q", c.name, f.values.Get("body"))
		}
		if c.name != "unknown account" && (f.values.Get("to") != v.Get("to") || f.values.Get("in_reply_to") != v.Get("in_reply_to")) {
			t.Errorf("%s: fields not preserved: %v", c.name, f.values)
		}
	}
	if n := fx.sync.sends(); n != 0 {
		t.Errorf("%d sends", n)
	}
	// Mutations need the Origin like every POST.
	w := post(fx.s, base, func(r *http.Request) { r.URL.Path = "/send"; r.Header.Set("Origin", "http://evil.example") })
	if w.Code != http.StatusForbidden || fx.sync.sends() != 0 {
		t.Errorf("cross-origin send: %d", w.Code)
	}
}

func TestSendGmiFailure(t *testing.T) {
	fx := newTagFixture(t)
	v := replyForm(t, fx.s, "personal", cabin3, true).values
	typed := "Don't lose me.\n\n" + v.Get("body")
	v.Set("body", typed)
	fx.sync.sendErr = errors.New("gmi send [personal]: exit 1: HttpError 400 invalid To header")
	w := postSend(fx.s, v)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("gmi failure: %d", w.Code)
	}
	f := parseForm(t, w.Body.String())
	if f.values.Get("body") != typed || f.values.Get("to") != v.Get("to") || f.values.Get("message_id") != v.Get("message_id") {
		t.Errorf("form not preserved: %v", f.values)
	}
	if !strings.Contains(f.err, "invalid To header") || !f.disabled || f.draftKey != "reply:personal:"+cabin3 {
		t.Errorf("err %q disabled %v draft %q", f.err, f.disabled, f.draftKey)
	}
	// The failed id is released: the retry sends.
	fx.sync.mu.Lock()
	fx.sync.sendErr = nil
	fx.sync.mu.Unlock()
	if w := postSend(fx.s, f.values); w.Code != http.StatusSeeOther || fx.sync.sends() != 2 {
		t.Errorf("retry: %d, %d sends", w.Code, fx.sync.sends())
	}
}

func TestAddresses(t *testing.T) {
	fx := newTagFixture(t)
	lookup := func(q string) []string {
		t.Helper()
		code, body := get(t, fx.s, "/addresses?q="+url.QueryEscape(q))
		if code != http.StatusOK {
			t.Fatalf("q=%q: %d", q, code)
		}
		var out []string
		if err := json.Unmarshal([]byte(body), &out); err != nil || out == nil {
			t.Fatalf("q=%q: %q %v", q, body, err)
		}
		return out
	}
	eq := func(got []string, want ...string) bool { return strings.Join(got, "|") == strings.Join(want, "|") }

	if got := lookup(""); len(got) != 6 {
		t.Errorf("all: %v", got) // sent recipients, deduplicated across accounts
	}
	if got := lookup("te"); !eq(got, "Tessa Lund <tessa@northwind.example>") {
		t.Errorf("te: %v", got)
	}
	if got := lookup("LUND"); !eq(got, "Tessa Lund <tessa@northwind.example>") {
		t.Errorf("surname: %v", got)
	}
	if got := lookup("rob"); !eq(got, "Robin Hale <robin@northwind.example>", "Robin Hale <robin@hale.example>") {
		t.Errorf("rob: %v", got)
	}
	if got := lookup("priya.n@"); !eq(got, "Priya Natarajan <priya.n@fastmail.example>") {
		t.Errorf("address prefix: %v", got)
	}
	if got := lookup("renee"); len(got) != 0 { // received from, never sent to
		t.Errorf("renee: %v", got)
	}

	// The cache answers inside its TTL and caps the answer.
	now := time.Now()
	fx.s.now = func() time.Time { return now }
	var many []notmuch.AddressEntry
	for i := range 30 {
		many = append(many, notmuch.AddressEntry{Name: fmt.Sprintf("Zed %02d", i), Address: fmt.Sprintf("zed%02d@example.com", i)})
	}
	fx.s.outbox.mu.Lock()
	fx.s.outbox.addrs["personal"] = addrCache{at: now, list: many}
	fx.s.outbox.mu.Unlock()
	got := lookup("zed")
	if len(got) != maxAddresses || got[0] != "Zed 00 <zed00@example.com>" {
		t.Errorf("cap: %d %v", len(got), got)
	}
	fx.s.now = func() time.Time { return now.Add(addressTTL + time.Second) }
	if got := lookup("zed"); len(got) != 0 {
		t.Errorf("stale cache served: %v", got)
	}

	if w := do(fx.s, "GET", "/addresses?q=ha", nil); w.Code != http.StatusForbidden {
		t.Errorf("no cookie: %d", w.Code)
	}
}

func TestEncodeBodyLongLine(t *testing.T) {
	long := strings.Repeat("abcdéfghij", 150) // 1650 octets
	body, cte := encodeBody("short\n" + long + "\n")
	if cte != "quoted-printable" {
		t.Fatalf("cte %q", cte)
	}
	dec, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if err != nil || string(dec) != "short\r\n"+long+"\r\n" {
		t.Errorf("round trip %q %v", dec, err)
	}
	for line := range strings.SplitSeq(body, "\r\n") {
		if len(line) > 78 {
			t.Errorf("qp line %d", len(line))
		}
	}
}

func TestFoldHeader(t *testing.T) {
	refs := strings.Repeat("<0123456789abcdef@mail.example> ", 8)
	got := foldHeader("References", strings.TrimSpace(refs))
	for line := range strings.SplitSeq(strings.TrimSuffix(got, "\r\n"), "\r\n") {
		if len(line) > 78 {
			t.Errorf("line %d: %q", len(line), line)
		}
	}
	if unfolded := strings.ReplaceAll(got, "\r\n ", " "); unfolded != "References: "+strings.TrimSpace(refs)+"\r\n" {
		t.Errorf("unfold %q", unfolded)
	}
}

func TestComposeReferrerPolicy(t *testing.T) {
	s := newServer(t)
	w := do(s, "GET", "/compose", withCookie)
	if w.Code != http.StatusOK || w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("compose: %d Referrer-Policy=%q (no-referrer makes the form POST arrive with Origin: null)", w.Code, w.Header().Get("Referrer-Policy"))
	}
	w = do(s, "GET", "/", withCookie)
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("list page must keep no-referrer, got %q", w.Header().Get("Referrer-Policy"))
	}
}

func TestReplyRefs(t *testing.T) {
	var many []string
	for i := range 25 {
		many = append(many, fmt.Sprintf("<r%02d@x>", i))
	}
	cases := []struct {
		name          string
		h             notmuch.ReplyHeaders
		wantIRT, want string
	}{
		{"junk dropped", notmuch.ReplyHeaders{InReplyTo: "<o@x>", References: "<a@b><c@d> junk <o@x>"}, "<o@x>", "<a@b> <c@d> <o@x>"},
		{"last 20 kept", notmuch.ReplyHeaders{InReplyTo: "<r24@x>", References: strings.Join(many, "\n ")}, "<r24@x>", strings.Join(many[5:], " ")},
		{"broken in-reply-to", notmuch.ReplyHeaders{InReplyTo: "o@x (lost brackets)", References: ""}, "<o@x>", ""},
		{"no header injection", notmuch.ReplyHeaders{InReplyTo: "<a\r\nBcc: e@v>", References: "<a@b>\r\nBcc: evil@example.com"}, "<o@x>", "<a@b>"},
	}
	for _, c := range cases {
		irt, refs := replyRefs(c.h, "o@x")
		if irt != c.wantIRT || refs != c.want {
			t.Errorf("%s: %q %q", c.name, irt, refs)
		}
	}
}

// A real reply to a message whose References header is malformed: the page
// renders sendable ids and the send goes through.
func TestReplyMalformedReferences(t *testing.T) {
	fx := newTagFixture(t)
	p := fx.env.Account(t, "personal")
	msg := "From: Sam <sam@ortega.example>\nTo: robin@hale.example\nSubject: refs\nDate: Mon, 1 Jun 2026 10:00:00 +0000\n" +
		"Message-ID: <badrefs@ortega.example>\nIn-Reply-To: <c@d>\nReferences: <a@b><c@d> junk\n\nhello\n"
	if err := os.WriteFile(filepath.Join(p.Root, "gmail", "mail", "cur", "badrefs:2,S"), []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Notmuch(t, "new", "--quiet")

	f := replyForm(t, fx.s, "personal", "badrefs@ortega.example", false)
	if got := f.values.Get("references"); got != "<a@b> <c@d> <badrefs@ortega.example>" {
		t.Fatalf("references %q", got)
	}
	if got := f.values.Get("in_reply_to"); got != "<badrefs@ortega.example>" {
		t.Fatalf("in_reply_to %q", got)
	}
	if w := postSend(fx.s, f.values); w.Code != http.StatusSeeOther {
		t.Fatalf("send: %d\n%s", w.Code, w.Body)
	}
	if got := lastSent(t, fx.sync, "personal").Header.Get("References"); got != "<a@b> <c@d> <badrefs@ortega.example>" {
		t.Errorf("sent References %q", got)
	}
}

func TestHeaderAddrQuotedLocalPart(t *testing.T) {
	for _, c := range []struct{ in, local string }{
		{`"x>, evil@attacker.com, <y"@example.com`, `x>, evil@attacker.com, <y`},
		{`"john smith"@example.com`, `john smith`},
		{`Jo <"john smith"@example.com>`, `john smith`},
		{`=?utf-8?q?Ren=C3=A9?= <"a b"@example.com>`, `a b`},
	} {
		d := composePage{To: c.in, MessageID: newMessageID()}
		raw, err := buildMessage(mail.Address{Address: "robin@hale.example"}, d, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		m, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		to, err := m.Header.AddressList("To")
		if err != nil || len(to) != 1 || to[0].Address != c.local+"@example.com" {
			t.Errorf("%s: To %q parses as %v (%v)", c.in, m.Header.Get("To"), to, err)
		}
		if !strings.Contains(m.Header.Get("To"), `"`+c.local+`"@example.com`) {
			t.Errorf("%s: local part not quoted: %q", c.in, m.Header.Get("To"))
		}
	}
	// Plain addresses are untouched.
	if got := headerAddr(&mail.Address{Address: "a@b.example"}); got != "a@b.example" {
		t.Error(got)
	}
	if got := headerAddr(&mail.Address{Name: "A B", Address: "a@b.example"}); got != "A B <a@b.example>" {
		t.Error(got)
	}
}

func TestSendBusy(t *testing.T) {
	fx := newTagFixture(t)
	fx.s.SendWait = 3 * time.Second
	v := replyForm(t, fx.s, "personal", cabin3, false).values
	typed := "Keep me.\n\n" + v.Get("body")
	v.Set("body", typed)
	fx.sync.sendErr = fmt.Errorf("%w: %w", gmi.ErrBusy, context.DeadlineExceeded)
	start := time.Now()
	w := postSend(fx.s, v)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("busy: %d %v", w.Code, w.Header())
	}
	if dl, ok := fx.sync.sendCtx.Deadline(); !ok || dl.Before(start) || dl.After(start.Add(3*time.Second+time.Second)) {
		t.Errorf("send ctx deadline %v (ok %v), want about SendWait from now", dl, ok)
	}
	f := parseForm(t, w.Body.String())
	if f.err != msgBusy || f.values.Get("body") != typed || f.values.Get("message_id") != v.Get("message_id") {
		t.Errorf("err %q, form %v", f.err, f.values)
	}
	// Released: the retry sends.
	fx.sync.mu.Lock()
	fx.sync.sendErr = nil
	fx.sync.mu.Unlock()
	if w := postSend(fx.s, f.values); w.Code != http.StatusSeeOther {
		t.Errorf("retry: %d", w.Code)
	}
}

func TestSendAcceptedButNoLocalCopy(t *testing.T) {
	fx := newTagFixture(t)
	v := replyForm(t, fx.s, "personal", cabin3, false).values
	fx.sync.sendOut = "sending message, from: Robin Hale <robin@hale.example>..\nreceiving content: 0%|          | 0/1\nnotmuch2._errors.XapianError: lock"
	fx.sync.sendErr = errors.New("gmi send [personal]: exit 1: notmuch2._errors.XapianError: lock")
	w := postSend(fx.s, v)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d", w.Code)
	}
	if f := parseForm(t, w.Body.String()); f.err != msgSentNoCopy || f.values.Get("message_id") != v.Get("message_id") {
		t.Fatalf("err %q", f.err)
	}
	if fx.sync.syncs != 1 {
		t.Errorf("SyncNow calls %d", fx.sync.syncs)
	}
	// Even once gmi would succeed, the same message isn't sent again.
	fx.sync.mu.Lock()
	fx.sync.sendErr, fx.sync.sendOut = nil, ""
	fx.sync.mu.Unlock()
	w = postSend(fx.s, v)
	if w.Code != http.StatusConflict || parseForm(t, w.Body.String()).err != msgSentNoCopy || fx.sync.sends() != 1 {
		t.Errorf("resubmit: %d, %d sends", w.Code, fx.sync.sends())
	}
	// A failure before lieer's send line proves nothing went out: "Not sent".
	v2 := replyForm(t, fx.s, "personal", cabin3, false).values
	fx.sync.sendOut = "Traceback (most recent call last):\nValueError: Recipients passed via sendmail(1) arguments"
	fx.sync.sendErr = errors.New("gmi send [personal]: exit 1: ValueError")
	if w := postSend(fx.s, v2); w.Code != http.StatusBadGateway || !strings.HasPrefix(parseForm(t, w.Body.String()).err, "Not sent:") {
		t.Errorf("plain failure: %d", w.Code)
	}
}

func TestDropPhoneNotes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// Gmail's text part, after two round trips through a pneu quote.
		{"Direct: 651-379-2240 <(651)%20379-2240> <(651)%20379-2240>", "Direct: 651-379-2240"},
		{"call +1 651 379 2240 <tel:651-379-2240> today", "call +1 651 379 2240 today"},
		// A note for a different number, or that isn't a phone, stays.
		{"Direct: 651-379-2240 <(612)%20555-0100>", "Direct: 651-379-2240 <(612)%20555-0100>"},
		{"*kbruins@bushfound.org* <kbruins@bushfound.org>", "*kbruins@bushfound.org* <kbruins@bushfound.org>"},
		{"see <123> and 2024 <2024>", "see <123> and 2024 <2024>"},
		{"pin <6513792240>", "pin <6513792240>"},
	} {
		if got := dropPhoneNotes(c.in); got != c.want {
			t.Errorf("dropPhoneNotes(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestQuoteHTML(t *testing.T) {
	bq := `<blockquote class="gmail_quote" style="` + quoteStyle + `">`
	for _, c := range []struct{ in, want string }{
		// A quote with no attribution (a forward pasted in) still folds.
		{"see below\n> a <b>\n\nbye", "see below<br>\n" +
			`<div class="gmail_quote">` + bq + "a &lt;b&gt;<br>\n</blockquote></div>\n<br>\nbye<br>\n"},
		// Two quoted runs, each with its own attribution.
		{"On x wrote:\n> one\nmid\nOn y wrote:\n>> two", `<div class="gmail_quote"><div dir="ltr" class="gmail_attr">On x wrote:<br></div>` +
			bq + "one<br>\n</blockquote></div>\nmid<br>\n" +
			`<div class="gmail_quote"><div dir="ltr" class="gmail_attr">On y wrote:<br></div>` + bq + bq + "two<br>\n</blockquote></blockquote></div>\n"},
		// Spacing and links survive.
		{"  a  b https://x.example/", "&nbsp;&nbsp;a &nbsp;b " + `<a href="https://x.example/" target="_blank" rel="noopener">https://x.example/</a><br>` + "\n"},
	} {
		if got := quoteHTML(c.in); got != c.want {
			t.Errorf("quoteHTML(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	if hasQuote("plain\nnot > a quote") || !hasQuote("x\r\n> y") {
		t.Error("hasQuote")
	}
}
