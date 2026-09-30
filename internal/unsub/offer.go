package unsub

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Method is what an offer does.
type Method string

const (
	MethodNone     Method = "none"
	MethodOneClick Method = "one-click"
	MethodMailto   Method = "mailto"
	MethodOpen     Method = "open"
)

// Offer is the action one message's headers lead to. It is comparable: the
// copies of a message must produce equal offers.
type Offer struct {
	Method   Method
	Reason   string // why none
	Index    int    // the item's position in the sender's list; -1 for none
	URL      string // the item as written (brackets' whitespace removed)
	Origin   string // http(s): scheme://host[:port]
	SignedBy string // one-click: the verified DKIM d=
	Mailto   Mailto
	// Fallback: a supported item follows Index, for a transport failure.
	Fallback bool
	Context  string // List-Id, else the From address
}

func noOffer(reason string) Offer { return Offer{Method: MethodNone, Reason: reason, Index: -1} }

const maxFiles = 8

// Inspect reads every file notmuch reported for the message (inside
// maildir) and returns their offer, or none unless they all agree.
// after skips items with Index <= after (-1 skips nothing).
func Inspect(ctx context.Context, v *Verifier, maildir string, files []string, after int) Offer {
	if len(files) == 0 {
		return noOffer("The message file is missing.")
	}
	if len(files) > maxFiles {
		return noOffer("The message has too many copies to check.")
	}
	var first Offer
	for i, name := range files {
		o, err := inspectFile(ctx, v, maildir, name, after)
		if err != nil {
			return noOffer("The message file couldn't be read.")
		}
		if i == 0 {
			first = o
		} else if o != first {
			return noOffer("The message's copies disagree about unsubscribing.")
		}
	}
	return first
}

func inspectFile(ctx context.Context, v *Verifier, maildir, name string, after int) (Offer, error) {
	f, err := Open(maildir, name)
	if err != nil {
		return Offer{}, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 32<<10)
	h, err := ReadHeader(br)
	if err != nil {
		return Offer{}, err
	}
	return Choose(ctx, v, h, br, after), nil
}

// Choose picks the method (docs/actions.md, Choosing the method): one-click
// only when List-Unsubscribe-Post is exactly the RFC 8058 argument, the
// list has exactly one http(s) item and it is https, and a DKIM signature
// v verified covers both headers; otherwise the first supported item in
// the sender's order. A port other than 443 doesn't demote a one-click:
// the POST refuses it, as a security refusal (Client.OneClick). body is
// read only for DKIM, which runs over the raw (still encoded) header.
func Choose(ctx context.Context, v *Verifier, h *Header, body io.Reader, after int) Offer {
	lu := h.Values("List-Unsubscribe")
	switch len(lu) {
	case 0:
		return noOffer("This message has no List-Unsubscribe header.")
	case 1:
	default:
		return noOffer("This message has more than one List-Unsubscribe header.")
	}
	post := h.Values("List-Unsubscribe-Post")
	if len(post) > 1 {
		return noOffer("This message has more than one List-Unsubscribe-Post header.")
	}
	list, ok := decodeList(lu[0])
	if !ok {
		return noOffer("This message's List-Unsubscribe header is encoded in a way pneu doesn't read.")
	}
	items := ParseList(list)
	ctxt := contextOf(h)

	var web []Item
	for _, it := range items {
		if it.Web {
			web = append(web, it)
		}
	}
	offer := func(it Item, m Method) Offer {
		o := Offer{Method: m, Index: it.Index, URL: it.Raw, Context: ctxt}
		if it.URL != nil {
			o.Origin = Origin(it.URL)
		}
		if m == MethodMailto {
			o.Mailto = it.Mailto
		}
		for _, later := range items[it.Index+1:] {
			if later.Kind != KindNone {
				o.Fallback = true
			}
		}
		return o
	}
	if len(post) == 1 && strings.Trim(post[0], " \t") == OneClickArg && len(web) == 1 &&
		web[0].Kind == KindHTTPS && web[0].Index > after && v != nil {
		if d, err := v.Verify(ctx, h, body, []string{"List-Unsubscribe", "List-Unsubscribe-Post"}); err == nil {
			o := offer(web[0], MethodOneClick)
			o.SignedBy = d
			return o
		}
	}
	for _, it := range items {
		if it.Index <= after {
			continue
		}
		switch it.Kind {
		case KindMailto:
			return offer(it, MethodMailto)
		case KindHTTP, KindHTTPS:
			return offer(it, MethodOpen)
		}
	}
	if after >= 0 {
		return noOffer("No other unsubscribe address follows.")
	}
	return noOffer("This message has no usable unsubscribe address.")
}

// encodedList matches a value made entirely of RFC 2047 encoded-words in
// the two charsets allowed, separated by whitespace. Only such a value is
// decoded: a literal <URI> with "=?" inside it stays literal, so decoding
// can't turn part of one signed URL into a second list item.
var encodedList = regexp.MustCompile(`^[ \t\r\n]*(?:=\?(?i:us-ascii|utf-8)\?[bBqQ]\?[^?\s]*\?=[ \t\r\n]*)+$`)

// decodeList returns a List-Unsubscribe value ready for ParseList. A value
// that is wholly encoded-words (some ESPs send, and sign, it that way) is
// decoded first, charsets us-ascii and utf-8 only, and the result must be
// ASCII. Decoding is deterministic, so what the signature covered is what's
// read. Any other value with "=?" in it is taken literally.
func decodeList(v string) (string, bool) {
	if !encodedList.MatchString(v) {
		return v, true
	}
	dec := &mime.WordDecoder{CharsetReader: func(string, io.Reader) (io.Reader, error) {
		return nil, errors.New("unsub: charset")
	}}
	out, err := dec.DecodeHeader(v)
	if err != nil || !ascii(out) {
		return "", false
	}
	return out, true
}

const maxContext = 200

// contextOf is List-Id if present, else the From address, for display.
func contextOf(h *Header) string {
	var s string
	if ids := h.Values("List-Id"); len(ids) > 0 {
		s = strings.TrimSpace(ids[0])
		if d, err := (&mime.WordDecoder{}).DecodeHeader(s); err == nil {
			s = d
		}
	} else if from := h.Values("From"); len(from) > 0 {
		s = strings.TrimSpace(from[0])
		if a, err := mail.ParseAddress(s); err == nil {
			s = a.Address
		}
	}
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) > maxContext {
		s = string([]rune(s)[:maxContext]) + "…"
	}
	return s
}

// HasList reports whether the message (the first of files that opens, inside
// maildir) carries exactly one List-Unsubscribe header: the thread page's
// cue that X has something to offer. Headers only; no DKIM, no parse of the
// list itself, so a message it marks may still turn out to offer nothing.
func HasList(maildir string, files []string) bool {
	for _, name := range files {
		f, err := Open(maildir, name)
		if err != nil {
			continue
		}
		h, err := ReadHeader(bufio.NewReader(f))
		f.Close()
		return err == nil && h.Count("List-Unsubscribe") == 1
	}
	return false
}
