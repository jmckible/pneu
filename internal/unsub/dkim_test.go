package unsub

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// testKeys is one RSA and one ed25519 key, generated per test run and
// published through a fake resolver: no test touches the network.
type testKeys struct {
	rsa     *rsa.PrivateKey
	ed      ed25519.PrivateKey
	records map[string][]string
}

func newTestKeys(t testing.TB) *testKeys {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, ek, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&rk.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &testKeys{rsa: rk, ed: ek, records: map[string][]string{
		"r1._domainkey.news.example": {"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(spki)},
		"e1._domainkey.news.example": {"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(ek.Public().(ed25519.PublicKey))},
	}}
}

func (k *testKeys) verifier() *Verifier {
	return &Verifier{
		LookupTXT: func(_ context.Context, name string) ([]string, error) {
			if r, ok := k.records[name]; ok {
				return r, nil
			}
			return nil, errors.New("nxdomain")
		},
	}
}

type signOpts struct {
	algo    string // rsa-sha256 | ed25519-sha256
	canon   string // c= value
	headers string // h= value
	extra   string // more tags, "; "-terminated
}

// sign prepends a DKIM-Signature to msg (CRLF or LF line ends).
func (k *testKeys) sign(t testing.TB, msg string, o signOpts) string {
	t.Helper()
	br := bufio.NewReader(strings.NewReader(msg))
	h, err := ReadHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	simple, relaxed, err := bodyHashes(context.Background(), br)
	if err != nil {
		t.Fatal(err)
	}
	_, bc, _ := strings.Cut(o.canon, "/")
	bh := simple
	if bc == "relaxed" {
		bh = relaxed
	}
	sel := "r1"
	if o.algo == "ed25519-sha256" {
		sel = "e1"
	}
	value := fmt.Sprintf(" v=1; a=%s; c=%s; d=news.example; s=%s; %sh=%s;\r\n bh=%s;\r\n b=",
		o.algo, o.canon, sel, o.extra, o.headers, base64.StdEncoding.EncodeToString(bh))
	field := Field{Name: "DKIM-Signature", Raw: []byte("DKIM-Signature:" + value + "\r\n")}
	h2 := &Header{Fields: append([]Field{field}, h.Fields...)}
	hc, _, _ := strings.Cut(o.canon, "/")
	s := signature{field: 0, headerCanon: hc}
	for n := range strings.SplitSeq(o.headers, ":") {
		s.headers = append(s.headers, strings.ToLower(strings.TrimSpace(n)))
	}
	sum := sha256.Sum256(headerData(h2, newHeaderIndex(h2), s))
	var sig []byte
	if o.algo == "ed25519-sha256" {
		sig = ed25519.Sign(k.ed, sum[:])
	} else if sig, err = rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, sum[:]); err != nil {
		t.Fatal(err)
	}
	return "DKIM-Signature:" + value + base64.StdEncoding.EncodeToString(sig) + "\r\n" + msg
}

const newsletter = "From: News <news@news.example>\r\n" +
	"To: robin@hale.example\r\n" +
	"Subject: This week\r\n" +
	"List-Unsubscribe: <https://news.example/u/abc>,\r\n <mailto:u@news.example?subject=stop>\r\n" +
	"List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n" +
	"\r\n" +
	"Hello  there. \r\n\r\nBye.\r\n\r\n\r\n"

const oversigned = "from:subject:list-unsubscribe:list-unsubscribe:list-unsubscribe-post:list-unsubscribe-post"

var need = []string{"List-Unsubscribe", "List-Unsubscribe-Post"}

func verify(v *Verifier, msg string) (string, error) {
	br := bufio.NewReader(strings.NewReader(msg))
	h, err := ReadHeader(br)
	if err != nil {
		return "", err
	}
	return v.Verify(context.Background(), h, br, need)
}

func TestDKIMValid(t *testing.T) {
	k := newTestKeys(t)
	v := k.verifier()
	for _, algo := range []string{"rsa-sha256", "ed25519-sha256"} {
		for _, canon := range []string{"relaxed/relaxed", "simple/simple", "relaxed/simple", "simple/relaxed", "simple", "relaxed"} {
			msg := k.sign(t, newsletter, signOpts{algo: algo, canon: canon, headers: oversigned})
			if d, err := verify(v, msg); err != nil || d != "news.example" {
				t.Errorf("%s %s: %q %v", algo, canon, d, err)
			}
			// The file on disk may hold LF line ends; the wire had CRLF.
			if d, err := verify(v, strings.ReplaceAll(msg, "\r\n", "\n")); err != nil || d != "news.example" {
				t.Errorf("%s %s LF: %q %v", algo, canon, d, err)
			}
		}
	}
}

func TestDKIMRejects(t *testing.T) {
	k := newTestKeys(t)
	now := time.Now()
	v := k.verifier()
	good := signOpts{algo: "rsa-sha256", canon: "relaxed/relaxed", headers: oversigned}
	with := func(f func(*signOpts)) signOpts { o := good; f(&o); return o }
	cases := map[string]string{
		"l= present": k.sign(t, newsletter, with(func(o *signOpts) { o.extra = "l=10; " })),
		"post not signed": k.sign(t, newsletter, with(func(o *signOpts) {
			o.headers = "from:subject:list-unsubscribe:list-unsubscribe"
		})),
		"no from": k.sign(t, newsletter, with(func(o *signOpts) {
			o.headers = "subject:list-unsubscribe:list-unsubscribe:list-unsubscribe-post:list-unsubscribe-post"
		})),
		// x= is not enforced, but a malformed one still rejects.
		"x= before t=": k.sign(t, newsletter, with(func(o *signOpts) {
			o.extra = fmt.Sprintf("t=%d; x=%d; ", now.Unix(), now.Add(-time.Hour).Unix())
		})),
		"x= not a number":    k.sign(t, newsletter, with(func(o *signOpts) { o.extra = "x=soon; " })),
		"q= without dns/txt": k.sign(t, newsletter, with(func(o *signOpts) { o.extra = "q=http/well-known; " })),
		"h= over the cap": k.sign(t, newsletter, with(func(o *signOpts) {
			o.headers = oversigned + strings.Repeat(":x-pad", maxSigned-5)
		})),
		"unknown selector": strings.Replace(k.sign(t, newsletter, good), "s=r1;", "s=r9;", 1),
	}
	for name, msg := range cases {
		if d, err := verify(v, msg); err == nil {
			t.Errorf("%s: verified as %q", name, d)
		}
	}

	signed := k.sign(t, newsletter, good)
	tamper := map[string]string{
		"tampered body":   strings.Replace(signed, "Bye.", "Bye!", 1),
		"appended body":   signed + "Click here.\r\n",
		"tampered header": strings.Replace(signed, "https://news.example/u/abc", "https://evil.example/u/abc", 1),
		// An added copy above the signed one: the signature still covers
		// the bottom instance, but h= no longer covers every copy.
		"added header": strings.Replace(signed, "From: News", "List-Unsubscribe: <https://evil.example/x>\r\nFrom: News", 1),
	}
	for name, msg := range tamper {
		if d, err := verify(v, msg); err == nil {
			t.Errorf("%s: verified as %q", name, d)
		}
	}
	// Still valid: an unsigned header the signature doesn't name.
	if _, err := verify(v, strings.Replace(signed, "From: News", "X-Added: 1\r\nFrom: News", 1)); err != nil {
		t.Errorf("unrelated header: %v", err)
	}
	// x= is parsed, not enforced: expired or not, the signature counts.
	for name, x := range map[string]time.Time{"unexpired": now.Add(time.Hour), "expired": now.Add(-time.Hour)} {
		msg := k.sign(t, newsletter, with(func(o *signOpts) {
			o.extra = fmt.Sprintf("t=%d; x=%d; ", now.Add(-2*time.Hour).Unix(), x.Unix())
		}))
		if _, err := verify(v, msg); err != nil {
			t.Errorf("%s x=: %v", name, err)
		}
	}
	// q= is a list; dns/txt anywhere in it, in any case, will do.
	for _, q := range []string{"dns/txt", "DNS/TXT", "x-other:dns/txt", " dns/txt : x-other "} {
		if _, err := verify(v, k.sign(t, newsletter, with(func(o *signOpts) { o.extra = "q=" + q + "; " }))); err != nil {
			t.Errorf("q=%s: %v", q, err)
		}
	}
	// h= at the cap is fine.
	atCap := k.sign(t, newsletter, with(func(o *signOpts) { o.headers = oversigned + strings.Repeat(":x-pad", maxSigned-6) }))
	if _, err := verify(v, atCap); err != nil {
		t.Errorf("h= at the cap: %v", err)
	}

	// A key in testing mode, a revoked key, a too-small RSA key.
	// crypto/rsa won't generate under 1024 bits; a 768-bit modulus is
	// enough to exercise the size check before any verification.
	n, _ := rand.Prime(rand.Reader, 768)
	smallDER := x509.MarshalPKCS1PublicKey(&rsa.PublicKey{N: n, E: 65537})
	for name, rec := range map[string]string{
		"testing": k.records["r1._domainkey.news.example"][0] + "; t=y",
		"revoked": "v=DKIM1; k=rsa; p=",
		"768 bit": "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(smallDER),
		"ed type": "v=DKIM1; k=ed25519; " + strings.TrimPrefix(k.records["r1._domainkey.news.example"][0], "v=DKIM1; k=rsa; "),
	} {
		k2 := &testKeys{records: map[string][]string{"r1._domainkey.news.example": {rec}}}
		if d, err := verify(k2.verifier(), signed); err == nil {
			t.Errorf("key %s: verified as %q", name, d)
		}
	}
	// Two key records for one selector: ambiguous, refused.
	k2 := &testKeys{records: map[string][]string{"r1._domainkey.news.example": {k.records["r1._domainkey.news.example"][0], "v=DKIM1; p=x"}}}
	if _, err := verify(k2.verifier(), signed); err == nil {
		t.Error("two key records verified")
	}
}

func TestBodyCap(t *testing.T) {
	_, _, err := bodyHashes(context.Background(), io.LimitReader(zeros{}, MaxBody+1))
	if !errors.Is(err, errBodyTooLong) {
		t.Fatalf("over cap: %v", err)
	}
	if _, _, err := bodyHashes(context.Background(), io.LimitReader(zeros{}, 1<<20)); err != nil {
		t.Fatal(err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestBodyCanon(t *testing.T) {
	cases := []struct{ in, simple, relaxed string }{
		{"", "\r\n", ""},
		{"\r\n\r\n", "\r\n", ""},
		{"a  b \t\r\n\r\n", "a  b \t\r\n", "a b\r\n"},
		{"a", "a\r\n", "a\r\n"},
		{" \r\nx\n", " \r\nx\r\n", "\r\nx\r\n"},
		{"a\rb\n", "a\rb\r\n", "a\rb\r\n"},
	}
	for _, c := range cases {
		s, r, _ := bodyHashes(context.Background(), strings.NewReader(c.in))
		ws, wr := sha256.Sum256([]byte(c.simple)), sha256.Sum256([]byte(c.relaxed))
		if !bytes.Equal(s, ws[:]) || !bytes.Equal(r, wr[:]) {
			t.Errorf("%q: simple %v relaxed %v", c.in, bytes.Equal(s, ws[:]), bytes.Equal(r, wr[:]))
		}
	}
}

// Signing each header once is enough (real senders don't over-sign); a copy
// added after signing is refused by Choose's one-header rule, not here.
func TestDKIMSignedOnce(t *testing.T) {
	k := newTestKeys(t)
	msg := k.sign(t, newsletter, signOpts{algo: "rsa-sha256", canon: "relaxed/relaxed",
		headers: "from:subject:list-unsubscribe:list-unsubscribe-post"})
	if d, err := verify(k.verifier(), msg); err != nil || d == "" {
		t.Fatalf("signed once: %q, %v", d, err)
	}
}

// A copy added above a once-signed header: h= no longer covers every copy.
func TestDKIMSignedOnceAddedCopy(t *testing.T) {
	k := newTestKeys(t)
	msg := k.sign(t, newsletter, signOpts{algo: "rsa-sha256", canon: "relaxed/relaxed",
		headers: "from:subject:list-unsubscribe:list-unsubscribe-post"})
	msg = strings.Replace(msg, "From: News", "List-Unsubscribe: <https://evil.example/x>\r\nFrom: News", 1)
	if d, err := verify(k.verifier(), msg); err == nil {
		t.Fatalf("added copy verified as %q", d)
	}
}

// The signature's own field never takes part in h= selection (RFC 6376
// §3.7): an h= naming dkim-signature takes the other signature, even when
// the signature's own field is lower in the header.
func TestHeaderDataSkipsOwnField(t *testing.T) {
	other := "DKIM-Signature: v=1; d=relay.example; s=x; b=AAAA\r\n"
	self := "DKIM-Signature: v=1; d=news.example; h=from:dkim-signature:dkim-signature; b=QUJD\r\n"
	h := &Header{Fields: []Field{
		{Name: "DKIM-Signature", Raw: []byte(other)},
		{Name: "From", Raw: []byte("From: a@news.example\r\n")},
		{Name: "DKIM-Signature", Raw: []byte(self)},
	}}
	s := signature{field: 2, headerCanon: "simple", headers: []string{"from", "dkim-signature", "dkim-signature"}}
	want := "From: a@news.example\r\n" + other +
		"DKIM-Signature: v=1; d=news.example; h=from:dkim-signature:dkim-signature; b="
	if got := string(headerData(h, newHeaderIndex(h), s)); got != want {
		t.Errorf("header data\n got %q\nwant %q", got, want)
	}

	// End to end: a second signature above, signed over by the first.
	k := newTestKeys(t)
	withOther := strings.Replace(newsletter, "From: News", other+"From: News", 1)
	msg := k.sign(t, withOther, signOpts{algo: "ed25519-sha256", canon: "relaxed/relaxed",
		headers: "from:list-unsubscribe:list-unsubscribe-post:dkim-signature"})
	if _, err := verify(k.verifier(), msg); err != nil {
		t.Errorf("signed over another signature: %v", err)
	}
	if _, err := verify(k.verifier(), strings.Replace(msg, "d=relay.example", "d=relay2.example", 1)); err == nil {
		t.Error("the other signature, tampered, still verified")
	}
}

// Header selection is linear and the work bounded: thousands of fields,
// thousands of signatures and an oversized h= finish at once.
func TestDKIMHeaderCost(t *testing.T) {
	k := newTestKeys(t)
	signed := k.sign(t, newsletter, signOpts{algo: "ed25519-sha256", canon: "relaxed/relaxed",
		headers: "from:list-unsubscribe:list-unsubscribe-post"})
	var pad strings.Builder
	for range 20000 {
		pad.WriteString("X:1\r\n")
	}
	// Signatures that parse and then fail covers (Post not signed), each
	// asking for a count of every field under the old code.
	bad := "DKIM-Signature: v=1; a=ed25519-sha256; d=news.example; s=e1; h=from:list-unsubscribe; bh=" +
		base64.StdEncoding.EncodeToString(make([]byte, 32)) + "; b=QQ==\r\n"
	// A signature with the real body hash that covers both headers, and an
	// h= of thousands of absent names: under the old code, each one a scan
	// of every field.
	_, rest, _ := strings.Cut(signed, "bh=")
	bh, _, _ := strings.Cut(rest, ";")
	huge := "DKIM-Signature: v=1; a=ed25519-sha256; c=relaxed/relaxed; d=news.example; s=e1; bh=" + bh +
		"; b=QQ==; h=from:list-unsubscribe:list-unsubscribe-post" + strings.Repeat(":x", 10000) + "\r\n"
	msg := strings.Repeat(bad, 600) + huge + pad.String() + signed
	start := time.Now()
	br := bufio.NewReader(strings.NewReader(msg))
	h, err := ReadHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := k.verifier().Verify(context.Background(), h, br, need); err != nil || d != "news.example" {
		t.Errorf("verify: %q %v", d, err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v", el)
	}
	if _, err := parseSignature(huge[len("DKIM-Signature:") : len(huge)-2]); err == nil {
		t.Error("h= past the cap parsed")
	}
}

// A cancelled request stops verification, in the body hash too.
func TestDKIMCancel(t *testing.T) {
	k := newTestKeys(t)
	msg := k.sign(t, newsletter, signOpts{algo: "ed25519-sha256", canon: "relaxed/relaxed", headers: oversigned})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	br := bufio.NewReader(strings.NewReader(msg))
	h, err := ReadHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.verifier().Verify(ctx, h, br, need); !errors.Is(err, context.Canceled) {
		t.Errorf("verify: %v", err)
	}
	if _, _, err := bodyHashes(ctx, io.LimitReader(zeros{}, 1<<20)); !errors.Is(err, context.Canceled) {
		t.Errorf("body hash: %v", err)
	}
}
