package unsub

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"hash"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// DKIM (RFC 6376, RFC 8463), just enough for RFC 8058 §4: a signature
// counts only if pneu verified it itself, it has no l=, and its h= signs
// every copy of each header in need. x= is parsed but not enforced
// (docs/actions.md, DKIM).

const (
	maxSignatures = 5
	maxSigned     = 64 // h= entries; more and the signature is unusable
	minRSABits    = 1024
	maxRSABits    = 8192
)

// Verifier checks DKIM signatures. LookupTXT is net.DefaultResolver's
// unless a test replaces it.
type Verifier struct {
	LookupTXT func(ctx context.Context, name string) ([]string, error)
}

// NewVerifier uses the system resolver.
func NewVerifier() *Verifier {
	return &Verifier{LookupTXT: net.DefaultResolver.LookupTXT}
}

var (
	errNoSignature = errors.New("dkim: no usable signature")
	errBodyTooLong = errors.New("dkim: body over the cap")
)

type signature struct {
	field                  int // index in Header.Fields
	algo                   string
	headerCanon, bodyCanon string
	domain, selector       string
	auid                   string // i=, or ""
	headers                []string
	bodyHash, sig          []byte
}

// Verify returns the d= of the first DKIM-Signature (in header order) that
// verifies and signs every copy of each name in need: listed in h= at least
// as often as the header occurs. body is read to its end (at most
// MaxBody bytes) only when some signature could qualify. The work is
// linear in the header and honors ctx.
func (v *Verifier) Verify(ctx context.Context, h *Header, body io.Reader, need []string) (string, error) {
	idx := newHeaderIndex(h)
	var sigs []signature
	for _, i := range idx["dkim-signature"] {
		if len(sigs) == maxSignatures {
			break
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		s, err := parseSignature(h.Fields[i].Value())
		if err != nil || !covers(s.headers, idx, need) {
			continue
		}
		s.field = i
		sigs = append(sigs, s)
	}
	if len(sigs) == 0 {
		return "", errNoSignature
	}
	simple, relaxed, err := bodyHashes(ctx, body)
	if err != nil {
		return "", err
	}
	for _, s := range sigs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		bh := simple
		if s.bodyCanon == "relaxed" {
			bh = relaxed
		}
		if !bytes.Equal(bh, s.bodyHash) {
			continue
		}
		key, err := v.key(ctx, s)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(headerData(h, idx, s))
		switch k := key.(type) {
		case *rsa.PublicKey:
			if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], s.sig) == nil {
				return s.domain, nil
			}
		case ed25519.PublicKey:
			if ed25519.Verify(k, sum[:], s.sig) {
				return s.domain, nil
			}
		}
	}
	return "", errNoSignature
}

// covers: h= must list From (RFC 6376 §5.4) and each name in need at least
// as many times as the message carries it, so every copy is signed. Not
// over-signing (count+1): real senders sign List-Unsubscribe{,-Post} once
// (466 of 491 in a 2026-09-29 sample of real mail), and the copy it would
// guard against, one added after signing, is refused anyway: Choose offers
// nothing unless each header occurs exactly once.
func covers(signed []string, idx headerIndex, need []string) bool {
	count := map[string]int{}
	for _, n := range signed {
		count[n]++
	}
	if count["from"] == 0 {
		return false
	}
	for _, n := range need {
		n = strings.ToLower(n)
		if c := len(idx[n]); c == 0 || count[n] < c {
			return false
		}
	}
	return true
}

// headerIndex maps each lowercased field name to its field indexes, in
// header order.
type headerIndex map[string][]int

func newHeaderIndex(h *Header) headerIndex {
	idx := headerIndex{}
	for i, f := range h.Fields {
		n := strings.ToLower(f.Name)
		idx[n] = append(idx[n], i)
	}
	return idx
}

// parseTags reads a DKIM tag-list: tag=value pairs split by ';', FWS
// around names and values dropped, a trailing ';' allowed, duplicates not.
func parseTags(v string) (map[string]string, error) {
	out := map[string]string{}
	parts := strings.Split(v, ";")
	for i, p := range parts {
		p = strings.Trim(p, " \t\r\n")
		if p == "" {
			if i == len(parts)-1 {
				break
			}
			return nil, errors.New("dkim: empty tag")
		}
		name, val, ok := strings.Cut(p, "=")
		if !ok {
			return nil, errors.New("dkim: tag without value")
		}
		name = strings.Trim(name, " \t\r\n")
		if name == "" || !isAlpha(name[0]) {
			return nil, errors.New("dkim: bad tag name")
		}
		for j := 1; j < len(name); j++ {
			if !isAlpha(name[j]) && !(name[j] >= '0' && name[j] <= '9') && name[j] != '_' {
				return nil, errors.New("dkim: bad tag name")
			}
		}
		if _, dup := out[name]; dup {
			return nil, errors.New("dkim: duplicate tag")
		}
		out[name] = strings.Trim(val, " \t\r\n")
	}
	return out, nil
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func stripWS(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

func parseSignature(v string) (signature, error) {
	var s signature
	bad := func(what string) (signature, error) { return signature{}, errors.New("dkim: " + what) }
	t, err := parseTags(v)
	if err != nil {
		return s, err
	}
	for _, req := range []string{"v", "a", "b", "bh", "d", "h", "s"} {
		if _, ok := t[req]; !ok {
			return bad("missing " + req + "=")
		}
	}
	if t["v"] != "1" {
		return bad("version")
	}
	if _, ok := t["l"]; ok {
		return bad("l= present") // a body-length limit lets appended content ride
	}
	s.algo = strings.ToLower(t["a"])
	if s.algo != "rsa-sha256" && s.algo != "ed25519-sha256" {
		return bad("algorithm")
	}
	if q, ok := t["q"]; ok && !listHas(stripWS(q), "dns/txt") {
		return bad("query method")
	}
	s.headerCanon, s.bodyCanon = "simple", "simple"
	if c, ok := t["c"]; ok {
		hc, bc, slash := strings.Cut(strings.ToLower(c), "/")
		s.headerCanon = hc
		if slash {
			s.bodyCanon = bc
		}
	}
	for _, c := range []string{s.headerCanon, s.bodyCanon} {
		if c != "simple" && c != "relaxed" {
			return bad("canonicalization")
		}
	}
	s.domain = strings.ToLower(t["d"])
	s.selector = strings.ToLower(t["s"])
	if !dnsName(s.domain) || !dnsName(s.selector) || !strings.Contains(s.domain, ".") {
		return bad("d= or s=")
	}
	if i, ok := t["i"]; ok {
		at := strings.LastIndexByte(i, '@')
		if at < 0 {
			return bad("i=")
		}
		dom := strings.ToLower(i[at+1:])
		if dom != s.domain && !strings.HasSuffix(dom, "."+s.domain) {
			return bad("i= outside d=")
		}
		s.auid = dom
	}
	if strings.Count(t["h"], ":") >= maxSigned {
		return bad("h= too long")
	}
	for _, n := range strings.Split(t["h"], ":") {
		n = strings.ToLower(strings.Trim(n, " \t\r\n"))
		if n == "" {
			return bad("h=")
		}
		s.headers = append(s.headers, n)
	}
	var signed int64 = -1
	if ts, ok := t["t"]; ok {
		if signed, err = strconv.ParseInt(stripWS(ts), 10, 64); err != nil || signed < 0 {
			return bad("t=")
		}
	}
	// x= is read but not enforced: ESPs set it days out, and a replay gets
	// an attacker only the genuine sender's endpoint (docs/actions.md).
	if xs, ok := t["x"]; ok {
		x, err := strconv.ParseInt(stripWS(xs), 10, 64)
		if err != nil || x < 0 || (signed >= 0 && x < signed) {
			return bad("x=")
		}
	}
	if s.bodyHash, err = base64.StdEncoding.DecodeString(stripWS(t["bh"])); err != nil || len(s.bodyHash) != sha256.Size {
		return bad("bh=")
	}
	if s.sig, err = base64.StdEncoding.DecodeString(stripWS(t["b"])); err != nil || len(s.sig) == 0 {
		return bad("b=")
	}
	return s, nil
}

// dnsName: dot-separated labels of letters, digits, '-' and '_'.
func dnsName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for l := range strings.SplitSeq(s, ".") {
		if l == "" || len(l) > 63 {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// key fetches and checks the selector's public key record.
func (v *Verifier) key(ctx context.Context, s signature) (crypto.PublicKey, error) {
	if v.LookupTXT == nil {
		return nil, errors.New("dkim: no resolver")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	recs, err := v.LookupTXT(ctx, s.selector+"._domainkey."+s.domain)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, r := range recs {
		if strings.TrimSpace(r) != "" {
			keys = append(keys, r)
		}
	}
	if len(keys) != 1 {
		return nil, errors.New("dkim: want exactly one key record")
	}
	t, err := parseTags(keys[0])
	if err != nil {
		return nil, err
	}
	if kv, ok := t["v"]; ok && kv != "DKIM1" {
		return nil, errors.New("dkim: key version")
	}
	kt := "rsa"
	if k, ok := t["k"]; ok {
		kt = strings.ToLower(k)
	}
	if (s.algo == "rsa-sha256") != (kt == "rsa") || (s.algo == "ed25519-sha256") != (kt == "ed25519") {
		return nil, errors.New("dkim: key type")
	}
	if hs, ok := t["h"]; ok && !listHas(hs, "sha256") {
		return nil, errors.New("dkim: key hash")
	}
	if st, ok := t["s"]; ok && !listHas(st, "*") && !listHas(st, "email") {
		return nil, errors.New("dkim: key service")
	}
	if fl, ok := t["t"]; ok {
		if listHas(fl, "y") {
			return nil, errors.New("dkim: key in testing mode")
		}
		if listHas(fl, "s") && s.auid != "" && s.auid != s.domain {
			return nil, errors.New("dkim: i= subdomain under t=s")
		}
	}
	raw, err := base64.StdEncoding.DecodeString(stripWS(t["p"]))
	if err != nil || len(raw) == 0 {
		return nil, errors.New("dkim: key revoked or malformed")
	}
	if kt == "ed25519" {
		if len(raw) != ed25519.PublicKeySize {
			return nil, errors.New("dkim: ed25519 key size")
		}
		return ed25519.PublicKey(raw), nil
	}
	var pub *rsa.PublicKey
	if k, err := x509.ParsePKIXPublicKey(raw); err == nil {
		rk, ok := k.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("dkim: not an RSA key")
		}
		pub = rk
	} else if pub, err = x509.ParsePKCS1PublicKey(raw); err != nil {
		return nil, errors.New("dkim: RSA key malformed")
	}
	if bits := pub.N.BitLen(); bits < minRSABits || bits > maxRSABits {
		return nil, errors.New("dkim: RSA key size")
	}
	return pub, nil
}

func listHas(list, want string) bool {
	for it := range strings.SplitSeq(list, ":") {
		if strings.EqualFold(strings.Trim(it, " \t\r\n"), want) {
			return true
		}
	}
	return false
}

// headerData is what the signature signs: each h= header, taken from the
// bottom up (a name listed more often than it occurs contributes nothing
// for the extras), then the signature's own field with b= emptied and no
// trailing CRLF. The signature's own field never takes part in the h=
// selection (RFC 6376 §3.7). One cursor per name walks idx upwards, so
// the work is linear in h=.
func headerData(h *Header, idx headerIndex, s signature) []byte {
	next := map[string]int{} // per name: how many of idx[name] remain unused
	var b bytes.Buffer
	for _, name := range s.headers {
		c, ok := next[name]
		if !ok {
			c = len(idx[name])
		}
		for c > 0 && idx[name][c-1] == s.field {
			c--
		}
		if c > 0 {
			c--
			b.Write(canonHeader(h.Fields[idx[name][c]].Raw, s.headerCanon))
		}
		next[name] = c
	}
	self := canonHeader(emptyB(h.Fields[s.field].Raw), s.headerCanon)
	b.Write(bytes.TrimSuffix(self, []byte("\r\n")))
	return b.Bytes()
}

// emptyB removes the value of the b= tag (and the whitespace around it)
// from a raw DKIM-Signature field.
func emptyB(raw []byte) []byte {
	colon := bytes.IndexByte(raw, ':')
	if colon < 0 {
		return raw
	}
	start := colon + 1
	for start <= len(raw) {
		end := bytes.IndexByte(raw[start:], ';')
		seg := raw[start:]
		if end >= 0 {
			seg = raw[start : start+end]
		}
		if eq := bytes.IndexByte(seg, '='); eq >= 0 && string(bytes.Trim(seg[:eq], " \t\r\n")) == "b" {
			out := append([]byte{}, raw[:start+eq+1]...)
			if end >= 0 {
				return append(out, raw[start+end:]...)
			}
			// b= is the last tag: keep the field's final CRLF.
			return append(out, '\r', '\n')
		}
		if end < 0 {
			break
		}
		start += end + 1
	}
	return raw
}

// canonHeader canonicalizes one raw field (CRLF line ends).
func canonHeader(raw []byte, c string) []byte {
	if c == "simple" {
		return raw
	}
	name, value, _ := bytes.Cut(raw, []byte(":"))
	var b bytes.Buffer
	b.WriteString(strings.ToLower(strings.TrimRight(string(name), " \t")))
	b.WriteByte(':')
	value = bytes.ReplaceAll(value, []byte("\r\n"), nil)
	wsp := false
	var v bytes.Buffer
	for _, ch := range value {
		if ch == ' ' || ch == '\t' {
			wsp = true
			continue
		}
		if wsp && v.Len() > 0 {
			v.WriteByte(' ')
		}
		wsp = false
		v.WriteByte(ch)
	}
	b.Write(v.Bytes())
	b.WriteString("\r\n")
	return b.Bytes()
}

// bodyHashes streams the body once into both canonicalizations' SHA-256,
// checking ctx every 64 KiB.
func bodyHashes(ctx context.Context, body io.Reader) (simple, relaxed []byte, err error) {
	sc := newBodyCanon(false)
	rc := newBodyCanon(true)
	r := bufio.NewReaderSize(io.LimitReader(body, MaxBody+1), 64<<10)
	n := 0
	cr := false
	for {
		c, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		n++
		if n > MaxBody {
			return nil, nil, errBodyTooLong
		}
		if n&(64<<10-1) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		if cr {
			cr = false
			if c == '\n' {
				sc.eol()
				rc.eol()
				continue
			}
			sc.content('\r')
			rc.content('\r')
		}
		switch c {
		case '\r':
			cr = true
		case '\n': // a file's bare LF stands for the wire's CRLF
			sc.eol()
			rc.eol()
		default:
			sc.content(c)
			rc.content(c)
		}
	}
	if cr {
		sc.content('\r')
		rc.content('\r')
	}
	return sc.sum(), rc.sum(), nil
}

// bodyCanon is RFC 6376 §3.4.3/§3.4.4 as a byte stream: trailing empty
// lines are held back until content follows them.
type bodyCanon struct {
	relaxed bool
	h       hash.Hash
	w       *bufio.Writer
	empties int  // empty lines not yet written
	inLine  bool // the current line has written content
	wsp     bool // relaxed: whitespace pending in the current line
	any     bool // some non-empty line was written
}

func newBodyCanon(relaxed bool) *bodyCanon {
	h := sha256.New()
	return &bodyCanon{relaxed: relaxed, h: h, w: bufio.NewWriterSize(h, 32<<10)}
}

func (c *bodyCanon) content(b byte) {
	if c.relaxed && (b == ' ' || b == '\t') {
		c.wsp = true
		return
	}
	if !c.inLine {
		for ; c.empties > 0; c.empties-- {
			c.w.WriteString("\r\n")
		}
		c.inLine = true
	}
	if c.wsp {
		c.w.WriteByte(' ')
		c.wsp = false
	}
	c.w.WriteByte(b)
}

func (c *bodyCanon) eol() {
	if c.inLine {
		c.w.WriteString("\r\n")
		c.any = true
	} else {
		c.empties++
	}
	c.inLine, c.wsp = false, false
}

func (c *bodyCanon) sum() []byte {
	if c.inLine {
		c.eol() // an unterminated last line gets its CRLF
	}
	if !c.relaxed && !c.any {
		c.w.WriteString("\r\n") // simple: an empty body is one CRLF
	}
	c.w.Flush()
	return c.h.Sum(nil)
}
