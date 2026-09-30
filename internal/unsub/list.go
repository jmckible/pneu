package unsub

import (
	"errors"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	maxItems    = 8
	maxItemLen  = 2 << 10
	maxHfield   = 1 << 10
	maxLocal    = 64  // octets in an address's local part (RFC 5321)
	maxAddr     = 254 // octets in a whole address
	OneClickArg = "List-Unsubscribe=One-Click"
)

// Kind is what an item can do.
type Kind string

const (
	KindNone   Kind = ""
	KindHTTPS  Kind = "https"
	KindHTTP   Kind = "http"
	KindMailto Kind = "mailto"
)

// Item is one List-Unsubscribe entry, in the sender's order.
type Item struct {
	Index  int
	Raw    string // the URI, whitespace inside the brackets removed
	Kind   Kind   // KindNone: unsupported or rejected
	Web    bool   // scheme is http or https, whether or not it passed
	URL    *url.URL
	Mailto Mailto
}

// ParseList splits an unfolded List-Unsubscribe value per RFC 2369, read
// conservatively: `<URI>` items separated by commas, each optionally
// followed by one parenthesized comment. The first malformed item ends the
// list; at most maxItems items, maxItemLen bytes each. RFC 2047 decoding,
// when the value needs it, is decodeList's, before this.
func ParseList(v string) []Item {
	var items []Item
	i := 0
	skip := func() {
		for i < len(v) && (v[i] == ' ' || v[i] == '\t') {
			i++
		}
	}
	for len(items) < maxItems {
		skip()
		if i >= len(v) || v[i] != '<' {
			break
		}
		end := strings.IndexByte(v[i+1:], '>')
		if end < 0 {
			break
		}
		inner := v[i+1 : i+1+end]
		if strings.IndexByte(inner, '<') >= 0 {
			break
		}
		uri := strings.NewReplacer(" ", "", "\t", "").Replace(inner)
		if uri == "" || len(uri) > maxItemLen {
			break
		}
		j := i + 1 + end + 1
		i = j
		skip()
		if i < len(v) && v[i] == '(' {
			n, ok := comment(v[i:])
			if !ok {
				break
			}
			i += n
			skip()
		}
		if i < len(v) {
			if v[i] != ',' {
				break
			}
			i++
		}
		items = append(items, classify(len(items), uri))
	}
	return items
}

// comment measures an RFC 5322 comment at the start of s: nested parens and
// quoted-pairs, depth-limited.
func comment(s string) (int, bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '(':
			depth++
			if depth > 8 {
				return 0, false
			}
		case ')':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func classify(index int, raw string) Item {
	it := Item{Index: index, Raw: raw}
	lower := strings.ToLower(raw)
	it.Web = strings.HasPrefix(lower, "http:") || strings.HasPrefix(lower, "https:")
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return it // control characters, non-ASCII
		}
	}
	switch {
	case strings.HasPrefix(lower, "mailto:"):
		if m, err := ParseMailto(raw); err == nil {
			it.Kind, it.Mailto = KindMailto, m
		}
	case it.Web:
		u, err := url.Parse(raw)
		if err != nil || !webURLOK(u) {
			return it
		}
		it.URL = u
		it.Kind = Kind(strings.ToLower(u.Scheme))
	}
	return it
}

// webURLOK: an http(s) URL with a plain DNS host: no userinfo, no IP
// literal (in any spelling a resolver might accept), no percent-encoding
// or non-ASCII in the host.
func webURLOK(u *url.URL) bool {
	s := strings.ToLower(u.Scheme)
	if (s != "http" && s != "https") || u.Opaque != "" || u.User != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.HasPrefix(u.Host, "[") || strings.ContainsAny(u.Host, "%@") {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return false
		}
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	// inet_aton accepts 2130706433, 0x7f.1, 127.1: a last label that is
	// numeric (or hex) is no TLD, so it is an address in disguise.
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	last := strings.ToLower(labels[len(labels)-1])
	if last == "" || strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") {
		return false
	}
	if p := u.Port(); p != "" && strings.Trim(p, "0123456789") != "" {
		return false
	}
	return true
}

// Origin is scheme://host[:port], the port only when not the scheme's default.
func Origin(u *url.URL) string {
	s := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	p := u.Port()
	if p == "" || (s == "https" && p == "443") || (s == "http" && p == "80") {
		return s + "://" + host
	}
	return s + "://" + host + ":" + p
}

// Mailto is a parsed mailto: exactly one recipient, the subject and body
// as they will be sent.
type Mailto struct {
	To      string // addr-spec
	Subject string
	Body    string // LF line ends
}

// DefaultSubject is used when the mailto has no subject hfield.
const DefaultSubject = "unsubscribe"

var errMailto = errors.New("unsub: unusable mailto")

// ParseMailto parses per RFC 6068, not as a form: '+' is literal and each
// %XX is decoded exactly once. The path must be exactly one mailbox; `to`
// and every hfield but subject and body reject the URI, as does a
// duplicate hfield. The address is at most maxAddr octets, its local part
// maxLocal. Subject and body are capped at maxHfield bytes after decoding;
// the subject loses CR/LF/NUL (nothing else: a present subject, even
// empty, is kept), the body's line ends become LF.
func ParseMailto(raw string) (Mailto, error) {
	if len(raw) < 7 || !strings.EqualFold(raw[:7], "mailto:") || strings.ContainsAny(raw, "#") {
		return Mailto{}, errMailto
	}
	rest := raw[7:]
	path, query, _ := strings.Cut(rest, "?")
	to, err := pctDecode(path)
	if err != nil || to == "" || hasCtl(to) {
		return Mailto{}, errMailto
	}
	addr, err := mail.ParseAddress(to)
	if err != nil || addr.Address == "" || hasCtl(addr.Address) || !ascii(addr.Address) ||
		(&mail.Address{Address: addr.Address}).String() != "<"+addr.Address+">" {
		// Only a plain ASCII dot-atom addr-spec: it round-trips unchanged
		// into the To header. Quoted local parts are refused, deliberately.
		return Mailto{}, errMailto
	}
	if at := strings.LastIndexByte(addr.Address, '@'); at < 0 || at > maxLocal || len(addr.Address) > maxAddr {
		return Mailto{}, errMailto
	}
	m := Mailto{To: addr.Address, Subject: DefaultSubject}
	seen := map[string]bool{}
	if query != "" {
		for hf := range strings.SplitSeq(query, "&") {
			k, v, ok := strings.Cut(hf, "=")
			if !ok {
				return Mailto{}, errMailto
			}
			name, err := pctDecode(k)
			if err != nil {
				return Mailto{}, errMailto
			}
			name = strings.ToLower(name)
			if seen[name] {
				return Mailto{}, errMailto
			}
			seen[name] = true
			val, err := pctDecode(v)
			if err != nil || len(val) > maxHfield || !utf8.ValidString(val) {
				return Mailto{}, errMailto
			}
			switch name {
			case "subject":
				// Present, even empty, is kept as written (less CR/LF/NUL).
				m.Subject = strings.Map(func(r rune) rune {
					if r == '\r' || r == '\n' || r == 0 {
						return -1
					}
					return r
				}, val)
			case "body":
				val = strings.ReplaceAll(val, "\r\n", "\n")
				val = strings.ReplaceAll(val, "\r", "\n")
				m.Body = strings.ReplaceAll(val, "\x00", "")
			default: // to, cc, bcc, in-reply-to, anything
				return Mailto{}, errMailto
			}
		}
	}
	return m, nil
}

// pctDecode decodes each %XX once and nothing else ('+' stays '+').
func pctDecode(s string) (string, error) {
	if strings.IndexByte(s, '%') < 0 {
		return s, nil
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b = append(b, s[i])
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return "", errMailto
		}
		b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
		i += 2
	}
	return string(b), nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

func hasCtl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return true
		}
	}
	return false
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
