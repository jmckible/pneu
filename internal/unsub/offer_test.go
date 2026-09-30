package unsub

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenContainment(t *testing.T) {
	base := t.TempDir()
	maildir := filepath.Join(base, "gmail", "mail")
	cur := filepath.Join(maildir, "cur")
	if err := os.MkdirAll(cur, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "secret")
	os.WriteFile(secret, []byte("List-Unsubscribe: <https://a.example/>\n\n"), 0o644)
	msg := filepath.Join(cur, "m:2,")
	os.WriteFile(msg, []byte("Subject: hi\n\nbody\n"), 0o644)
	os.WriteFile(filepath.Join(cur, "inside"), []byte("x"), 0o644)
	os.Symlink(secret, filepath.Join(cur, "escape:2,"))
	os.Symlink("../../../secret", filepath.Join(cur, "relescape:2,"))
	os.Symlink("inside", filepath.Join(cur, "within:2,"))
	os.Mkdir(filepath.Join(cur, "dir:2,"), 0o755)
	syscall.Mkfifo(filepath.Join(cur, "fifo:2,"), 0o644)

	if f, err := Open(maildir, msg); err != nil {
		t.Fatalf("plain file: %v", err)
	} else {
		f.Close()
	}
	if f, err := Open(maildir, filepath.Join(cur, "within:2,")); err != nil {
		t.Errorf("symlink inside the maildir: %v", err)
	} else {
		f.Close()
	}
	// Through a symlinked spelling of the maildir itself.
	alias := filepath.Join(base, "alias")
	os.Symlink(maildir, alias)
	if f, err := Open(alias, msg); err != nil {
		t.Errorf("aliased maildir: %v", err)
	} else {
		f.Close()
	}

	refused := map[string]string{
		"absolute outside": secret,
		"dotdot":           filepath.Join(cur, "..", "..", "..", "secret"),
		"symlink escape":   filepath.Join(cur, "escape:2,"),
		"relative symlink": filepath.Join(cur, "relescape:2,"),
		"directory":        filepath.Join(cur, "dir:2,"),
		"fifo":             filepath.Join(cur, "fifo:2,"),
		"relative name":    "cur/m:2,",
		"the maildir":      maildir,
		"missing":          filepath.Join(cur, "nope"),
	}
	for name, p := range refused {
		done := make(chan error, 1)
		go func() {
			f, err := Open(maildir, p)
			if f != nil {
				f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: opened", name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Open blocked", name)
		}
	}
	if _, err := Open("", msg); err == nil {
		t.Error("empty maildir opened")
	}
}

func TestReadHeader(t *testing.T) {
	good := "From: a@b.example\r\nList-Unsubscribe: <https://a.example/1>,\r\n\t<mailto:u@a.example>\r\n\r\nbody"
	br := bufio.NewReader(strings.NewReader(good))
	h, err := ReadHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	if v := h.Values("list-unsubscribe"); len(v) != 1 || v[0] != " <https://a.example/1>,\t<mailto:u@a.example>" {
		t.Errorf("unfolded %q", v)
	}
	if rest, _ := br.ReadString(0); rest != "body" {
		t.Errorf("body %q", rest)
	}
	for name, in := range map[string]string{
		"continuation first": " x\r\nFrom: a\r\n\r\n",
		"no colon":           "From a\r\n\r\n",
		"space in name":      "List-Unsubscribe : <https://a.example/>\r\n\r\n",
		"too large":          "X: " + strings.Repeat("a", MaxHeader) + "\r\n\r\n",
	} {
		if _, err := ReadHeader(bufio.NewReader(strings.NewReader(in))); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// Header only, no blank line.
	if h, err := ReadHeader(bufio.NewReader(strings.NewReader("From: a@b.example\n"))); err != nil || len(h.Fields) != 1 {
		t.Errorf("header-only: %v", err)
	}
}

func choose(t *testing.T, v *Verifier, msg string, after int) Offer {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(msg))
	h, err := ReadHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	return Choose(context.Background(), v, h, br, after)
}

func TestChoose(t *testing.T) {
	k := newTestKeys(t)
	v := k.verifier()
	signed := k.sign(t, newsletter, signOpts{algo: "ed25519-sha256", canon: "relaxed/relaxed", headers: oversigned})

	o := choose(t, v, signed, -1)
	if o.Method != MethodOneClick || o.Index != 0 || o.URL != "https://news.example/u/abc" || o.SignedBy != "news.example" ||
		o.Origin != "https://news.example" || !o.Fallback || o.Context != "news@news.example" {
		t.Errorf("one-click: %+v", o)
	}
	// After a transport failure: the next item, a mailto.
	o = choose(t, v, signed, 0)
	if o.Method != MethodMailto || o.Index != 1 || o.Mailto.To != "u@news.example" || o.Mailto.Subject != "stop" || o.Fallback {
		t.Errorf("after 0: %+v", o)
	}
	if o = choose(t, v, signed, 1); o.Method != MethodNone {
		t.Errorf("after 1: %+v", o)
	}
	// Unsigned: the first item in the sender's order, opened in the browser.
	if o = choose(t, v, newsletter, -1); o.Method != MethodOpen || o.Index != 0 || o.SignedBy != "" {
		t.Errorf("unsigned: %+v", o)
	}

	hdr := func(lines ...string) string {
		return "From: News <news@news.example>\r\nList-Id: Weekly <weekly.news.example>\r\n" + strings.Join(lines, "\r\n") + "\r\n\r\nhi\r\n"
	}
	cases := []struct {
		name   string
		msg    string
		method Method
		index  int
	}{
		{"none", hdr("Subject: x"), MethodNone, -1},
		{"two headers", hdr("List-Unsubscribe: <mailto:a@news.example>", "List-Unsubscribe: <mailto:b@news.example>"), MethodNone, -1},
		{"two post headers", hdr("List-Unsubscribe: <mailto:a@news.example>", "List-Unsubscribe-Post: List-Unsubscribe=One-Click", "List-Unsubscribe-Post: List-Unsubscribe=One-Click"), MethodNone, -1},
		{"mailto first", hdr("List-Unsubscribe: <mailto:a@news.example>, <https://news.example/u>"), MethodMailto, 0},
		{"skip unsupported", hdr("List-Unsubscribe: <ftp://news.example/>, <mailto:a@news.example?cc=x@y.example>, <https://news.example/u>"), MethodOpen, 2},
		{"http", hdr("List-Unsubscribe: <http://news.example/u>"), MethodOpen, 0},
		{"malformed stops", hdr("List-Unsubscribe: junk, <https://news.example/u>"), MethodNone, -1},
		{"post without https", hdr("List-Unsubscribe: <mailto:a@news.example>", "List-Unsubscribe-Post: List-Unsubscribe=One-Click"), MethodMailto, 0},
	}
	for _, c := range cases {
		o := choose(t, v, c.msg, -1)
		if o.Method != c.method || o.Index != c.index {
			t.Errorf("%s: %+v", c.name, o)
		}
		if c.method != MethodNone && o.Context != "Weekly <weekly.news.example>" {
			t.Errorf("%s: context %q", c.name, o.Context)
		}
	}

	// One-click needs every condition; each broken one falls back to the
	// first supported item.
	type mut struct{ name, msg string }
	base := "From: News <news@news.example>\r\nSubject: s\r\n%LU%\r\n%POST%\r\n\r\nhi\r\n"
	build := func(lu, post string) string {
		return strings.NewReplacer("%LU%", lu, "%POST%", post).Replace(base)
	}
	lu := "List-Unsubscribe: <https://news.example/u>"
	post := "List-Unsubscribe-Post: List-Unsubscribe=One-Click"
	for _, m := range []mut{
		{"post value", build(lu, "List-Unsubscribe-Post: List-Unsubscribe=one-click")},
		{"post extra", build(lu, "List-Unsubscribe-Post: List-Unsubscribe=One-Click&x=1")},
		{"two https", build("List-Unsubscribe: <https://news.example/u>, <https://news.example/v>", post)},
		{"https and http", build("List-Unsubscribe: <https://news.example/u>, <http://news.example/v>", post)},
		{"only http", build("List-Unsubscribe: <http://news.example/u>", post)},
	} {
		msg := k.sign(t, m.msg, signOpts{algo: "rsa-sha256", canon: "relaxed/relaxed", headers: oversigned})
		if o := choose(t, v, msg, -1); o.Method == MethodOneClick {
			t.Errorf("%s: one-click offered", m.name)
		}
	}
	good := k.sign(t, build(lu, post), signOpts{algo: "rsa-sha256", canon: "simple/simple", headers: oversigned})
	if o := choose(t, v, good, -1); o.Method != MethodOneClick {
		t.Errorf("control: %+v", o)
	}
	if o := choose(t, nil, good, -1); o.Method != MethodOpen {
		t.Errorf("no verifier: %+v", o)
	}
	// A port other than 443 stays a one-click (the POST refuses it as a
	// security refusal), never demoted to opening it in the browser.
	port := k.sign(t, build("List-Unsubscribe: <https://news.example:8443/u>", post),
		signOpts{algo: "rsa-sha256", canon: "relaxed/relaxed", headers: oversigned})
	if o := choose(t, v, port, -1); o.Method != MethodOneClick || o.Origin != "https://news.example:8443" {
		t.Errorf("port 8443: %+v", o)
	}
}

// Some ESPs send List-Unsubscribe as RFC 2047 encoded-words, and sign it
// that way: decoded (us-ascii, utf-8; ASCII result) before the grammar,
// verified over the raw form.
func TestChooseEncodedList(t *testing.T) {
	k := newTestKeys(t)
	v := k.verifier()
	enc := "List-Unsubscribe: =?us-ascii?Q?=3Chttps=3A=2F=2Fexample.com?=\r\n" +
		" =?us-ascii?Q?=2Fu=3Fa=3D1=3E=2C?=\r\n" +
		"\t=?us-ascii?Q?_=3Cmailto=3Ax=40?=\r\n" +
		" =?us-ascii?Q?example.com=3E?="
	msg := "From: News <news@news.example>\r\nSubject: s\r\n" + enc +
		"\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nhi\r\n"
	signed := k.sign(t, msg, signOpts{algo: "rsa-sha256", canon: "relaxed/relaxed",
		headers: "from:subject:list-unsubscribe:list-unsubscribe-post"})
	o := choose(t, v, signed, -1)
	if o.Method != MethodOneClick || o.URL != "https://example.com/u?a=1" || o.SignedBy != "news.example" || !o.Fallback {
		t.Fatalf("encoded one-click: %+v", o)
	}
	if o := choose(t, v, signed, 0); o.Method != MethodMailto || o.Mailto.To != "x@example.com" {
		t.Errorf("encoded, after 0: %+v", o)
	}
	// Tampering with the encoded form breaks the signature, decoded or not.
	if o := choose(t, v, strings.Replace(signed, "example.com?=", "evil.test?=", 1), -1); o.Method == MethodOneClick {
		t.Errorf("tampered: %+v", o)
	}

	for name, lu := range map[string]string{
		"latin-1":       "=?iso-8859-1?Q?=3Cmailto=3Ax=40example.com=3E?=",
		"other charset": "=?windows-1252?Q?=3Cmailto=3Ax=40example.com=3E?=",
		"non-ascii":     "=?utf-8?Q?=3Cmailto=3Ax=40ex=C3=A4mple.com=3E?=",
		"bad encoding":  "=?utf-8?B?!!!?=",
	} {
		o := choose(t, v, "From: a@news.example\r\nList-Unsubscribe: "+lu+"\r\n\r\nhi\r\n", -1)
		if o.Method != MethodNone {
			t.Errorf("%s: %+v", name, o)
		}
	}
	// utf-8 whose result is ASCII is fine, B-encoded too.
	o = choose(t, v, "From: a@news.example\r\nList-Unsubscribe: =?UTF-8?B?PG1haWx0bzp4QGV4YW1wbGUuY29tPg==?=\r\n\r\nhi\r\n", -1)
	if o.Method != MethodMailto || o.Mailto.To != "x@example.com" {
		t.Errorf("utf-8 B: %+v", o)
	}
}

func TestInspectCopies(t *testing.T) {
	maildir := t.TempDir()
	write := func(name, lu string) string {
		p := filepath.Join(maildir, name)
		os.WriteFile(p, []byte("From: a@news.example\nList-Unsubscribe: "+lu+"\n\nhi\n"), 0o644)
		return p
	}
	a := write("a", "<mailto:u@news.example>")
	b := write("b", "<mailto:u@news.example>")
	c := write("c", "<mailto:evil@news.example>")
	if o := Inspect(context.Background(), nil, maildir, []string{a, b}, -1); o.Method != MethodMailto {
		t.Errorf("agreeing copies: %+v", o)
	}
	if o := Inspect(context.Background(), nil, maildir, []string{a, c}, -1); o.Method != MethodNone {
		t.Errorf("disagreeing copies: %+v", o)
	}
	if o := Inspect(context.Background(), nil, maildir, []string{a, "/etc/passwd"}, -1); o.Method != MethodNone {
		t.Errorf("copy outside: %+v", o)
	}
	if o := Inspect(context.Background(), nil, maildir, nil, -1); o.Method != MethodNone {
		t.Errorf("no files: %+v", o)
	}
}

func TestStore(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Store[string, int]{Now: func() time.Time { return now }}
	tok, err := s.Issue("act")
	if err != nil || len(tok) != 32 {
		t.Fatalf("token %q %v", tok, err)
	}
	tok2, _ := s.Issue("other")
	if tok2 == tok {
		t.Fatal("repeated token")
	}
	if _, _, found := s.Result(tok); found {
		t.Error("result before take")
	}
	a, ok := s.Take(tok)
	if !ok || a != "act" {
		t.Fatalf("take: %q %v", a, ok)
	}
	if _, ok := s.Take(tok); ok {
		t.Error("second take")
	}
	if _, done, found := s.Result(tok); !found || done {
		t.Errorf("running: done=%v found=%v", done, found)
	}
	s.Finish(tok, 7)
	if r, done, found := s.Result(tok); r != 7 || !done || !found {
		t.Errorf("result %v %v %v", r, done, found)
	}
	now = now.Add(ResultTTL + time.Second)
	if _, _, found := s.Result(tok); found {
		t.Error("result outlived ResultTTL")
	}
	// tok2 expired long ago.
	if _, ok := s.Take(tok2); ok {
		t.Error("expired token taken")
	}
	tok3, _ := s.Issue("x")
	now = now.Add(TokenTTL - time.Second)
	if _, ok := s.Take(tok3); !ok {
		t.Error("token expired early")
	}
	if _, ok := s.Take("nope"); ok {
		t.Error("unknown token taken")
	}

	// Concurrent takes: exactly one wins.
	tok4, _ := s.Issue("race")
	wins := make(chan bool, 16)
	for range 16 {
		go func() { _, ok := s.Take(tok4); wins <- ok }()
	}
	n := 0
	for range 16 {
		if <-wins {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d winners", n)
	}
}

// Only a value made wholly of encoded-words is decoded: "=?" inside a
// literal <URI> stays literal, so it can't split one signed URL into a
// different URL plus a second item (round-2 review).
func TestDecodeListLiteral(t *testing.T) {
	for _, v := range []string{
		"<https://example.com/u?token==?utf-8?Q?=3E=2C=20=3Cmailto:other@example.net?=>",
		"<https://example.com/u> =?utf-8?Q?=2C_=3Cmailto=3Ax=40example.net=3E?=",
		"=?utf-8?Q?=3Chttps=3A=2F=2Fexample.com=2Fu=3E?= <mailto:x@example.net>",
	} {
		got, ok := decodeList(v)
		if !ok || got != v {
			t.Errorf("decodeList(%q) = %q, %v; want it unchanged", v, got, ok)
		}
	}
	items := ParseList("<https://example.com/u?token==?utf-8?Q?=3E=2C=20=3Cmailto:other@example.net?=>")
	for _, it := range items {
		if it.Kind == KindMailto {
			t.Errorf("literal =? produced a mailto item: %+v", items)
		}
	}
	if _, ok := decodeList("=?iso-8859-1?Q?=3Chttps=3A=2F=2Fexample.com=3E?="); ok {
		if got, _ := decodeList("=?iso-8859-1?Q?=3Chttps=3A=2F=2Fexample.com=3E?="); got != "=?iso-8859-1?Q?=3Chttps=3A=2F=2Fexample.com=3E?=" {
			t.Errorf("latin-1 decoded: %q", got)
		}
	}
}
