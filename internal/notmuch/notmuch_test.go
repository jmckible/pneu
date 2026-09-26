package notmuch

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// weirdID exercises query quoting: an embedded quote, a space-ish %, parens.
const weirdID = `we"ird%20(id)@example.com`

var fixture = map[string]string{
	"1:2,": `From: Alice <alice@example.com>
To: me@example.com
Subject: Hello
Date: Mon, 01 Jan 2024 10:00:00 +0000
Message-ID: <a1@example.com>

Plain body one.
`,
	"2:2,": `From: Me <me@example.com>
To: alice@example.com
Subject: Re: Hello
Date: Mon, 01 Jan 2024 11:00:00 +0000
Message-ID: <a2@example.com>
In-Reply-To: <a1@example.com>
References: <a1@example.com>

Reply body.
`,
	"3:2,": `From: Bob <bob@example.com>
To: me@example.com
Subject: HTML news
Date: Tue, 02 Jan 2024 10:00:00 +0000
Message-ID: <` + weirdID + `>
MIME-Version: 1.0
Content-Type: multipart/related; boundary="REL"

--REL
Content-Type: multipart/alternative; boundary="ALT"

--ALT
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

caf=E9
--ALT
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: quoted-printable

<html><body><p>Hi =E2=9C=93</p><img src=3D"cid:img1@x"></body></html>
--ALT--
--REL
Content-Type: image/png
Content-ID: <img1@x>
Content-Disposition: inline; filename="dot.png"
Content-Transfer-Encoding: base64

` + pngB64 + `
--REL--
`,
	"4:2,": `From: Carol <carol@example.com>
To: me@example.com, bob@example.com
Subject: Re: HTML news
Date: Wed, 03 Jan 2024 10:00:00 +0000
Message-ID: <a4@example.com>
In-Reply-To: <` + weirdID + `>
References: <` + weirdID + `>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/plain

see attached
--MIX
Content-Type: message/rfc822

From: Dan <dan@example.com>
Subject: inner
Message-ID: <inner@example.com>
Date: Wed, 03 Jan 2024 09:00:00 +0000

inner body
--MIX
Content-Type: application/pdf; name="x.pdf"
Content-Disposition: attachment; filename="x.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--MIX--
`,
}

func setup(t *testing.T) Account {
	t.Helper()
	if _, err := exec.LookPath(Binary); err != nil {
		t.Skip("notmuch not on PATH")
	}
	dir := t.TempDir()
	mail := filepath.Join(dir, "mail")
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(mail, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range fixture {
		if err := os.WriteFile(filepath.Join(mail, "cur", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "notmuch-config")
	conf := fmt.Sprintf("[database]\npath=%s\n[user]\nname=Me\nprimary_email=me@example.com\n[new]\ntags=\n[search]\nexclude_tags=spam;trash\n[maildir]\nsynchronize_flags=false\n", mail)
	if err := os.WriteFile(cfg, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Account{Name: "test", Email: "me@example.com", ConfigPath: cfg}
	ctx := context.Background()
	if _, err := a.run(ctx, nil, "new", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if err := a.Tag(ctx, []string{"+inbox", "+unread"}, []string{"a1@example.com", "a2@example.com", weirdID, "a4@example.com"}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestIDQuery(t *testing.T) {
	if got := idQuery(`a"b@c`); got != `id:"a""b@c"` {
		t.Fatal(got)
	}
}

func TestReadPath(t *testing.T) {
	a := setup(t)
	ctx := context.Background()

	t.Run("Search", func(t *testing.T) {
		res, err := a.Search(ctx, "tag:inbox", SearchOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("want 2 threads, got %d", len(res))
		}
		r := res[0]
		if r.Account != "test" || r.Subject != "HTML news" || r.Matched != 2 || r.Total != 2 || r.Timestamp != 1704276000 || r.Query[0] == nil || r.Query[1] != nil || !slices.Contains(r.Tags, "attachment") {
			t.Fatalf("newest thread: %+v", r)
		}
		page, err := a.Search(ctx, "tag:inbox", SearchOpts{Limit: 1, Offset: 1})
		if err != nil || len(page) != 1 || page[0].Subject != "Hello" {
			t.Fatalf("paged: %+v %v", page, err)
		}
	})

	t.Run("Count", func(t *testing.T) {
		if n, err := a.Count(ctx, "tag:unread", false); err != nil || n != 4 {
			t.Fatalf("messages %d %v", n, err)
		}
		if n, err := a.Count(ctx, "tag:unread", true); err != nil || n != 2 {
			t.Fatalf("threads %d %v", n, err)
		}
	})

	t.Run("Show", func(t *testing.T) {
		msgs, err := a.Show(ctx, idQuery("a4@example.com"))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 || msgs[0].ID != weirdID || msgs[1].ID != "a4@example.com" || msgs[0].Depth != 0 || msgs[1].Depth != 1 {
			t.Fatalf("thread shape: %+v", msgs)
		}
		if msgs[0].Match || !msgs[1].Match || msgs[0].Account != "test" {
			t.Fatal("match flags / account")
		}

		html := msgs[0]
		if got := html.CIDs(); got["img1@x"] != 5 {
			t.Fatalf("cid map %v", got)
		}
		var sawHTML, sawLatin bool
		Walk(html.Body, func(p *Part) bool {
			switch p.ContentType {
			case "text/html":
				sawHTML = p.HasContent && strings.Contains(p.Content, "Hi ✓") && p.ID == 4
			case "text/plain":
				sawLatin = p.Content == "café"
			case "image/png":
				if p.HasContent || p.ContentLength == 0 || p.Filename != "dot.png" || p.ContentDisposition != "inline" || p.ContentTransferEncoding != "base64" {
					t.Errorf("png part %+v", p)
				}
			}
			return true
		})
		if !sawHTML || !sawLatin {
			t.Fatalf("html %v latin %v: %+v", sawHTML, sawLatin, html.Body)
		}

		fwd := msgs[1].Body[0]
		if fwd.ContentType != "multipart/mixed" || len(fwd.Children) != 3 {
			t.Fatalf("mixed: %+v", fwd)
		}
		rfc := fwd.Children[1]
		if rfc.ContentType != "message/rfc822" || len(rfc.Messages) != 1 || rfc.Messages[0].Headers["Subject"] != "inner" || rfc.Messages[0].Body[0].ID != 4 {
			t.Fatalf("rfc822: %+v", rfc)
		}
		if pdf := fwd.Children[2]; pdf.ID != 5 || pdf.Filename != "x.pdf" || pdf.HasContent {
			t.Fatalf("pdf: %+v", pdf)
		}

		// Date order across the other thread.
		msgs, err = a.Show(ctx, "subject:Hello and not subject:news")
		if err != nil || len(msgs) != 2 || msgs[0].ID != "a1@example.com" || msgs[1].Depth != 1 {
			t.Fatalf("thread A: %+v %v", msgs, err)
		}
	})

	t.Run("Message", func(t *testing.T) {
		m, err := a.Message(ctx, weirdID)
		if err != nil || m.ID != weirdID || m.Account != "test" || m.CIDs()["img1@x"] != 5 {
			t.Fatalf("message: %+v %v", m, err)
		}
		if _, err := a.Message(ctx, "nope@example.com"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})

	t.Run("Part", func(t *testing.T) {
		want, _ := base64.StdEncoding.DecodeString(pngB64)
		b, ct, err := a.Part(ctx, weirdID, 5)
		if err != nil || ct != "image/png" || !bytes.Equal(b, want) {
			t.Fatalf("png: ct=%q len=%d err=%v", ct, len(b), err)
		}
		b, ct, err = a.Part(ctx, "a4@example.com", 5)
		if err != nil || ct != "application/pdf" || string(b) != "%PDF-1.4\n" {
			t.Fatalf("pdf: ct=%q %q err=%v", ct, b, err)
		}
		// Text parts come back as UTF-8 whatever the source charset.
		b, ct, err = a.Part(ctx, weirdID, 3)
		if err != nil || string(b) != "café" || ct != "text/plain; charset=utf-8" {
			t.Fatalf("latin1: ct=%q %q err=%v", ct, b, err)
		}
		if _, _, err := a.Part(ctx, weirdID, 99); err == nil {
			t.Fatal("missing part should error")
		}
	})

	t.Run("Reply", func(t *testing.T) {
		r, err := a.Reply(ctx, "a4@example.com", true)
		if err != nil {
			t.Fatal(err)
		}
		h := r.Headers
		if h.Subject != "Re: HTML news" || h.From != "Me <me@example.com>" || h.InReplyTo != "<a4@example.com>" ||
			!strings.Contains(h.References, "<a4@example.com>") || !strings.Contains(h.To, "carol@example.com") ||
			!strings.Contains(h.To+h.Cc, "bob@example.com") || r.Original.ID != "a4@example.com" {
			t.Fatalf("reply-all: %+v", h)
		}
		r, err = a.Reply(ctx, "a4@example.com", false)
		if err != nil || strings.Contains(r.Headers.To+r.Headers.Cc, "bob@") {
			t.Fatalf("reply-sender: %+v %v", r.Headers, err)
		}
	})

	t.Run("Address", func(t *testing.T) {
		got, err := a.Address(ctx, "from:alice or from:bob")
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		if !slices.Equal(got, []string{"Alice <alice@example.com>", "Bob <bob@example.com>"}) {
			t.Fatalf("%q", got)
		}
	})

	t.Run("Errors", func(t *testing.T) {
		_, err := a.Search(ctx, "date:garbage..", SearchOpts{})
		var ne *Error
		if !errors.As(err, &ne) || ne.Stderr == "" {
			t.Fatalf("want *Error with stderr, got %v", err)
		}
	})
}

func TestTag(t *testing.T) {
	a := setup(t)
	ctx := context.Background()

	if err := a.Tag(ctx, []string{"-inbox", "+needs reply", "+a%b"}, []string{weirdID}); err != nil {
		t.Fatal(err)
	}
	msgs, err := a.Show(ctx, idQuery(weirdID))
	if err != nil {
		t.Fatal(err)
	}
	tags := msgs[0].Tags
	if slices.Contains(tags, "inbox") || !slices.Contains(tags, "needs reply") || !slices.Contains(tags, "a%b") {
		t.Fatalf("tags %v", tags)
	}
	// The other message in the thread is untouched: ids, never thread:.
	if !slices.Contains(msgs[1].Tags, "inbox") {
		t.Fatal("tag leaked to sibling")
	}
	for _, bad := range [][]string{{"inbox"}, {"+"}} {
		if err := a.Tag(ctx, bad, []string{"a1@example.com"}); err == nil {
			t.Fatalf("change %q should be rejected", bad)
		}
	}
	if err := a.Tag(ctx, []string{"+x"}, []string{"a\nb"}); err == nil {
		t.Fatal("newline in id should be rejected")
	}
}

// holdLock keeps a `notmuch tag --batch` open (it holds the Xapian write lock
// for as long as its stdin is open) and returns the release func.
func holdLock(t *testing.T, a Account) func() {
	t.Helper()
	cmd := exec.Command(Binary, "tag", "--batch")
	cmd.Env = append(os.Environ(), "NOTMUCH_CONFIG="+a.ConfigPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(w, "+holder -- "+idQuery("a1@example.com"))
	// Wait until the holder has the database open for writing.
	for i := 0; ; i++ {
		probe := Account{Name: a.Name, ConfigPath: a.ConfigPath}
		c, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, err := probe.run(c, strings.NewReader("+probe -- id:none\n"), "tag", "--batch")
		cancel()
		if err != nil {
			break
		}
		if i > 50 {
			t.Fatal("holder never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {
		w.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("holder: %v: %s", err, stderr.String())
		}
	}
}

func TestTagLocked(t *testing.T) {
	a := setup(t)

	release := holdLock(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := a.Tag(ctx, []string{"+late"}, []string{"a2@example.com"})
	cancel()
	if !errors.Is(err, ErrLocked) {
		release()
		t.Fatalf("want ErrLocked, got %v", err)
	}
	t.Logf("locked error: %v", err)

	// Released mid-wait: Tag succeeds once the writer goes away.
	time.AfterFunc(300*time.Millisecond, release)
	start := time.Now()
	if err := a.Tag(context.Background(), []string{"+late"}, []string{"a2@example.com"}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("Tag did not wait for the lock")
	}
	if n, _ := a.Count(context.Background(), "tag:late", false); n != 1 {
		t.Fatal("tag not applied")
	}
}

func TestMessageIDs(t *testing.T) {
	a := setup(t)
	ctx := context.Background()
	got, err := a.MessageIDs(ctx, IDsQuery([]string{"a1@example.com", weirdID, "a4@example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a4@example.com", weirdID, "a1@example.com"}; !slices.Equal(got, want) {
		t.Fatalf("got %q want %q (newest first)", got, want)
	}
	if got, err := a.MessageIDs(ctx, "tag:nothing"); err != nil || len(got) != 0 {
		t.Fatalf("empty: %q %v", got, err)
	}
}

func TestIsLockErr(t *testing.T) {
	for _, s := range []string{
		"A Xapian exception occurred opening database: Unable to get write lock on /x/.notmuch/xapian: already locked",
		"Unable to acquire database write lock",
	} {
		if !isLockErr(s) {
			t.Error(s)
		}
	}
	if isLockErr("Syntax error in query") {
		t.Error("false positive")
	}
}
