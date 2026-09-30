package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/unsub"
	"github.com/jmckible/pneu/internal/unsub/unsubtest"
)

// unsubFixture is the tag fixture plus a signing key and a one-click
// endpoint, both wired into the server in place of DNS and the network.
type unsubFixture struct {
	tagFixture
	key    ed25519.PrivateKey
	status atomic.Int32 // what the one-click endpoint answers
	hits   atomic.Int32
	answer atomic.Value // netip.Addr the fake DNS gives
}

var unsubServerAddr = netip.MustParseAddr("127.0.0.1")

func newUnsubFixture(t *testing.T) *unsubFixture {
	t.Helper()
	f := &unsubFixture{tagFixture: newTagFixture(t)}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = key
	f.status.Store(http.StatusOK)
	f.answer.Store(unsubServerAddr)
	record := unsubtest.KeyRecord(key.Public().(ed25519.PublicKey))
	f.s.unsubDKIM = &unsub.Verifier{
		LookupTXT: func(_ context.Context, name string) ([]string, error) {
			if name == "k1._domainkey.news.example" {
				return []string{record}, nil
			}
			return nil, errors.New("nxdomain")
		},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		w.WriteHeader(int(f.status.Load()))
	}))
	t.Cleanup(srv.Close)
	c := unsub.NewClient()
	c.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{f.answer.Load().(netip.Addr)}, nil
	}
	c.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	// The production policy plus exactly the test server's loopback.
	c.Allowed = func(a netip.Addr) bool { return a == unsubServerAddr || unsub.PublicIPv4(a) }
	c.TLS = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	f.s.unsubHTTP = c
	return f
}

// deliver writes a message into the personal maildir and indexes it.
func (f *unsubFixture) deliver(t *testing.T, name, msg string) string {
	t.Helper()
	a := f.env.Account(t, "personal")
	p := filepath.Join(a.Root, "gmail", "mail", "cur", name+":2,")
	if err := os.WriteFile(p, []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Notmuch(t, "new", "--quiet")
	return p
}

func newsMsg(id, lu, post string) string {
	m := "From: News <news@news.example>\n" +
		"To: robin@hale.example\n" +
		"Subject: This week\n" +
		"Date: Mon, 28 Sep 2026 10:00:00 +0000\n" +
		"Message-ID: <" + id + ">\n" +
		"List-Id: Weekly <weekly.news.example>\n" +
		"List-Unsubscribe: " + lu + "\n"
	if post != "" {
		m += "List-Unsubscribe-Post: " + post + "\n"
	}
	return m + "\nHello.\n"
}

const unsubSigned = "from:subject:list-unsubscribe:list-unsubscribe:list-unsubscribe-post:list-unsubscribe-post"

type previewJSON struct {
	Method   string  `json:"method"`
	Reason   string  `json:"reason"`
	Index    *int    `json:"index"`
	Token    string  `json:"token"`
	URL      string  `json:"url"`
	Origin   string  `json:"origin"`
	SignedBy string  `json:"signedBy"`
	Account  string  `json:"account"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Subject  string  `json:"subject"`
	Body     *string `json:"body"`
	Context  string  `json:"context"`
}

func preview(t *testing.T, s http.Handler, account, id, query string) (int, previewJSON) {
	t.Helper()
	w := do(s, "GET", "/unsubscribe/"+url.PathEscape(account)+"/"+url.PathEscape(id)+query, withCookie)
	var p previewJSON
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("preview %s: %d %q", id, w.Code, w.Body)
	}
	return w.Code, p
}

func execute(t *testing.T, s http.Handler, token string, mods ...func(*http.Request)) (int, unsubResult) {
	t.Helper()
	w := postTo(s, "/unsubscribe", url.Values{"token": {token}}, mods...)
	var r unsubResult
	json.Unmarshal(w.Body.Bytes(), &r)
	return w.Code, r
}

func TestUnsubscribeMailto(t *testing.T) {
	f := newUnsubFixture(t)
	f.deliver(t, "mailto1", newsMsg("m1@news.example", "<mailto:leave+robin@news.example?subject=stop%20please&body=bye%0D%0Anow>, <https://news.example/u>", ""))

	code, p := preview(t, f.s, "personal", "m1@news.example", "")
	if code != http.StatusOK || p.Method != "mailto" || p.Token == "" || *p.Index != 0 {
		t.Fatalf("preview %d %+v", code, p)
	}
	if p.To != "leave+robin@news.example" || p.Subject != "stop please" || p.Body == nil || *p.Body != "bye\nnow" ||
		p.From != "Robin Hale <robin@hale.example>" || p.Account != "personal" || p.Context != "Weekly <weekly.news.example>" {
		t.Errorf("preview fields %+v", p)
	}

	// Cross-origin POST: refused before the handler, token intact.
	if code, _ := execute(t, f.s, p.Token, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", code)
	}
	if code, _ := execute(t, f.s, p.Token, func(r *http.Request) { r.Header.Del("Origin") }); code != http.StatusForbidden {
		t.Fatalf("no origin: %d", code)
	}
	if f.sync.sends() != 0 {
		t.Fatal("sent on a refused POST")
	}

	code, res := execute(t, f.s, p.Token)
	if code != http.StatusOK || res.State != "ok" {
		t.Fatalf("execute %d %+v", code, res)
	}
	if g := genOf(f.s); g != 1 { // the sent copy is in Sent
		t.Errorf("gen %d after a sent unsubscribe", g)
	}
	m := lastSent(t, f.sync, "personal")
	if got := m.Header.Get("To"); got != "leave+robin@news.example" {
		t.Errorf("To %q", got)
	}
	if m.Header.Get("Cc") != "" || m.Header.Get("Bcc") != "" {
		t.Error("extra recipients")
	}
	if m.Header.Get("Subject") != "stop please" || !strings.Contains(m.Header.Get("From"), "robin@hale.example") {
		t.Errorf("headers %v", m.Header)
	}

	// Single use; the result stays readable.
	if code, _ := execute(t, f.s, p.Token); code != http.StatusConflict {
		t.Errorf("reuse: %d", code)
	}
	if f.sync.sends() != 1 {
		t.Errorf("%d sends", f.sync.sends())
	}
	w := do(f.s, "GET", "/unsubscribe-result/"+p.Token, withCookie)
	var stored unsubResult
	json.Unmarshal(w.Body.Bytes(), &stored)
	if w.Code != http.StatusOK || stored.State != "ok" {
		t.Errorf("result %d %q", w.Code, w.Body)
	}
	if w := do(f.s, "GET", "/unsubscribe-result/"+strings.Repeat("0", 32), withCookie); w.Code != http.StatusNotFound {
		t.Errorf("unknown result: %d", w.Code)
	}
	if code, _ := execute(t, f.s, "nope"); code != http.StatusConflict {
		t.Errorf("unknown token: %d", code)
	}

	// gmi failing before Gmail accepted it: failed, not resent.
	_, p2 := preview(t, f.s, "personal", "m1@news.example", "")
	f.sync.mu.Lock()
	f.sync.sendErr = errors.New("gmi exited 1")
	f.sync.mu.Unlock()
	if _, res := execute(t, f.s, p2.Token); res.State != "failed" || res.Fallback {
		t.Errorf("failed send: %+v", res)
	}
	if g := genOf(f.s); g != 1 {
		t.Errorf("gen %d after a failed unsubscribe", g)
	}
}

func TestUnsubscribeReadOnly(t *testing.T) {
	f := newUnsubFixture(t)
	f.deliver(t, "ro1", newsMsg("ro1@news.example", "<mailto:leave@news.example>", ""))
	f.sync.setStatus("personal", pulling(10, 100))
	code, p := preview(t, f.s, "personal", "ro1@news.example", "")
	if code != http.StatusOK || p.Method != "none" || p.Token != "" {
		t.Errorf("read-only mailto: %d %+v", code, p)
	}
	// A token issued before the pull began still can't send.
	f.sync.setStatus("personal", gmi.Status{State: gmi.StateReady, Pulled: true})
	_, p = preview(t, f.s, "personal", "ro1@news.example", "")
	f.sync.setStatus("personal", pulling(10, 100))
	if _, res := execute(t, f.s, p.Token); res.State != "failed" || f.sync.sends() != 0 {
		t.Errorf("read-only execute: %+v, %d sends", res, f.sync.sends())
	}
}

func TestUnsubscribeOneClick(t *testing.T) {
	f := newUnsubFixture(t)
	msg := newsMsg("oc1@news.example", "<https://example.com/u/abc>, <mailto:leave@news.example>", "List-Unsubscribe=One-Click")
	f.deliver(t, "oc1", unsubtest.Sign(msg, f.key, "news.example", "k1", unsubSigned))

	code, p := preview(t, f.s, "personal", "oc1@news.example", "")
	if code != http.StatusOK || p.Method != "one-click" || p.SignedBy != "news.example" || p.Origin != "https://example.com" ||
		p.URL != "https://example.com/u/abc" || p.Token == "" || *p.Index != 0 {
		t.Fatalf("preview %d %+v", code, p)
	}
	if _, res := execute(t, f.s, p.Token); res.State != "ok" || res.Category != "ok" || f.hits.Load() != 1 {
		t.Fatalf("execute %+v hits %d", res, f.hits.Load())
	}
	if g := genOf(f.s); g != 0 { // nothing local changed
		t.Errorf("gen %d after a one-click", g)
	}

	// A transport failure offers the next item as a new preview.
	f.status.Store(http.StatusInternalServerError)
	_, p = preview(t, f.s, "personal", "oc1@news.example", "")
	if _, res := execute(t, f.s, p.Token); res.State != "failed" || res.Category != "http-5xx" || !res.Fallback {
		t.Fatalf("5xx %+v", res)
	}
	_, next := preview(t, f.s, "personal", "oc1@news.example", "?after=0")
	if next.Method != "mailto" || *next.Index != 1 || next.To != "leave@news.example" {
		t.Errorf("fallback preview %+v", next)
	}

	// An address the check refuses: refused, and no fallback.
	f.answer.Store(netip.MustParseAddr("192.168.1.1"))
	_, p = preview(t, f.s, "personal", "oc1@news.example", "")
	before := f.hits.Load()
	if _, res := execute(t, f.s, p.Token); res.State != "refused" || res.Category != "refused-address" || res.Fallback {
		t.Errorf("refused %+v", res)
	}
	if f.hits.Load() != before {
		t.Error("refused destination was contacted")
	}

	// Unsigned: opened in the browser.
	f.deliver(t, "oc2", newsMsg("oc2@news.example", "<https://example.com/u/abc>", "List-Unsubscribe=One-Click"))
	if _, p := preview(t, f.s, "personal", "oc2@news.example", ""); p.Method != "open" || p.Token != "" || p.Origin != "https://example.com" {
		t.Errorf("unsigned %+v", p)
	}
	// Signing each header once is enough (every copy is signed).
	msg3 := newsMsg("oc3@news.example", "<https://example.com/u/abc>", "List-Unsubscribe=One-Click")
	f.deliver(t, "oc3", unsubtest.Sign(msg3, f.key, "news.example", "k1", "from:subject:list-unsubscribe:list-unsubscribe-post"))
	if _, p := preview(t, f.s, "personal", "oc3@news.example", ""); p.Method != "one-click" || p.Token == "" {
		t.Errorf("signed once %+v", p)
	}
}

// A qualifying one-click on a port other than 443 is offered as one-click
// and refused when it runs: a security refusal, no fallback, nothing sent.
// Never quietly offered as open-in-tab.
func TestUnsubscribeOneClickPort(t *testing.T) {
	f := newUnsubFixture(t)
	msg := newsMsg("port1@news.example", "<https://example.com:8443/u/abc>, <mailto:leave@news.example>", "List-Unsubscribe=One-Click")
	f.deliver(t, "port1", unsubtest.Sign(msg, f.key, "news.example", "k1", unsubSigned))
	_, p := preview(t, f.s, "personal", "port1@news.example", "")
	if p.Method != "one-click" || p.Origin != "https://example.com:8443" || p.Token == "" {
		t.Fatalf("preview %+v", p)
	}
	if _, res := execute(t, f.s, p.Token); res.State != "refused" || res.Category != "refused-port" || res.Fallback {
		t.Errorf("execute %+v", res)
	}
	if f.hits.Load() != 0 {
		t.Error("contacted")
	}
}

// Previews run at most unsubPreviews at once; one that can't get a slot
// before its request's deadline answers busy.
func TestUnsubscribePreviewBusy(t *testing.T) {
	f := newUnsubFixture(t)
	f.deliver(t, "busy1", newsMsg("busy1@news.example", "<mailto:leave@news.example>", ""))
	for range unsubPreviews {
		f.s.unsubSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w := do(f.s, "GET", "/unsubscribe/personal/"+url.PathEscape("busy1@news.example"), func(r *http.Request) {
		*r = *r.WithContext(ctx)
		withCookie(r)
	})
	var p previewJSON
	json.Unmarshal(w.Body.Bytes(), &p)
	if w.Code != http.StatusServiceUnavailable || p.Method != "none" || p.Reason != "busy" {
		t.Errorf("busy: %d %q", w.Code, w.Body)
	}
	// A slot frees: the next preview runs.
	<-f.s.unsubSlots
	if _, p := preview(t, f.s, "personal", "busy1@news.example", ""); p.Method != "mailto" {
		t.Errorf("after a slot freed: %+v", p)
	}
	if len(f.s.unsubSlots) != unsubPreviews-1 {
		t.Errorf("slot not returned: %d held", len(f.s.unsubSlots))
	}
}

func TestUnsubscribePreviewErrors(t *testing.T) {
	f := newUnsubFixture(t)
	if code, p := preview(t, f.s, "personal", "no-such@news.example", ""); code != http.StatusNotFound || p.Method != "none" {
		t.Errorf("unknown message: %d %+v", code, p)
	}
	if code, _ := preview(t, f.s, "nobody", "x@y", ""); code != http.StatusNotFound {
		t.Errorf("unknown account: %d", code)
	}
	if code, _ := preview(t, f.s, "personal", kitchen1, "?after=x"); code != http.StatusBadRequest {
		t.Errorf("bad after: %d", code)
	}
	// A fixture message without the header.
	if code, p := preview(t, f.s, "personal", kitchen1, ""); code != http.StatusOK || p.Method != "none" || p.Reason == "" {
		t.Errorf("no header: %d %+v", code, p)
	}
	// No session: refused.
	if w := do(f.s, "GET", "/unsubscribe/personal/"+url.PathEscape(kitchen1), nil); w.Code != http.StatusForbidden {
		t.Errorf("no cookie: %d", w.Code)
	}
}

// A message file that is a symlink out of the maildir is never read, even
// though notmuch indexed it.
func TestUnsubscribeSymlinkEscape(t *testing.T) {
	f := newUnsubFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte(newsMsg("esc1@news.example", "<mailto:leave@news.example>", "")), 0o644)
	a := f.env.Account(t, "personal")
	if err := os.Symlink(outside, filepath.Join(a.Root, "gmail", "mail", "cur", "esc1:2,")); err != nil {
		t.Fatal(err)
	}
	a.Notmuch(t, "new", "--quiet")
	code, p := preview(t, f.s, "personal", "esc1@news.example", "")
	if code == http.StatusNotFound {
		t.Skip("notmuch didn't index the symlink")
	}
	if p.Method != "none" || p.Token != "" {
		t.Errorf("escaped file read: %d %+v", code, p)
	}
}

func TestBuildUnsubMessage(t *testing.T) {
	from := mail.Address{Name: "Robin Hale", Address: "robin@hale.example"}
	id := newMessageID()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, to := range []string{"", "a@b.example, c@d.example", "a@b.example\r\nBcc: e@f.example", "Name <a@b.example>"} {
		if _, err := buildUnsubMessage(from, unsub.Mailto{To: to, Subject: "s"}, id, now); err == nil {
			t.Errorf("to %q built", to)
		}
	}
	if _, err := buildUnsubMessage(from, unsub.Mailto{To: "a@b.example"}, "<x@y>", now); err == nil {
		t.Error("foreign Message-ID built")
	}
	raw, err := buildUnsubMessage(from, unsub.Mailto{To: "a@b.example", Subject: "unsubscribe", Body: "x"}, id, now)
	if err != nil || !strings.Contains(string(raw), "\r\nTo: a@b.example\r\n") || !strings.Contains(string(raw), "Message-ID: "+id+"\r\n") {
		t.Errorf("built %q %v", raw, err)
	}
	if !strings.Contains(string(raw), "\r\nSubject: unsubscribe\r\n") {
		t.Errorf("plain subject encoded: %q", raw)
	}
}

// Whatever the subject, a MIME decoder reading the built message gets
// exactly the text the confirmation showed.
func TestUnsubSubjectRoundTrip(t *testing.T) {
	from := mail.Address{Name: "Robin Hale", Address: "robin@hale.example"}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, subj := range []string{
		"unsubscribe",
		"stop please",
		"=?utf-8?q?Unsubscribe_me?=",
		"=?utf-8?b?c3RvcA==?= now",
		"a =? b",
		"  leading and trailing  ",
		"two  spaces",
		"tab\there",
		"Zoë — 東京",
		strings.Repeat("=?x?q?y?= ", 60) + "ü",
		strings.Repeat("x", 1000),
		strings.Repeat("é", 400),
	} {
		raw, err := buildUnsubMessage(from, unsub.Mailto{To: "a@b.example", Subject: subj}, newMessageID(), now)
		if err != nil {
			t.Fatalf("%q: %v", subj, err)
		}
		for line := range strings.SplitSeq(string(raw), "\r\n") {
			if len(line) > 998 {
				t.Errorf("%q: line of %d octets", subj, len(line))
			}
		}
		m, err := mail.ReadMessage(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("%q: %v", subj, err)
		}
		got, err := (&mime.WordDecoder{}).DecodeHeader(m.Header.Get("Subject"))
		if err != nil || got != subj {
			t.Errorf("subject %q decoded as %q (%v); header %q", subj, got, err, m.Header.Get("Subject"))
		}
		if strings.Contains(subj, "=?") && !strings.HasPrefix(m.Header.Get("Subject"), "=?utf-8?b?") {
			t.Errorf("%q: not encoded: %q", subj, m.Header.Get("Subject"))
		}
	}
}
