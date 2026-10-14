package web

import (
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmckible/pneu/internal/notmuch"
)

// The thread page's folded messages show one line of what each one says
// (snippet), and the stack reads by day: a divider where the day changes,
// naming a silence before it (gapLabel), and each message's time alone.

var (
	greetingRE = regexp.MustCompile(`(?i)^(?:hi|hey|hello|dear|morning|good morning|afternoon|all|team|folks)\b[^.!?]{0,40}[,!:]?$`)
	// placeholderRE is a line that only stands in for an inline image or
	// file ("[image: logo.png]", Gmail's text part).
	placeholderRE = regexp.MustCompile(`^\[[^\]]{1,200}\]$`)
	signoffRE     = regexp.MustCompile(`^(?:(?i:thanks|thank you|best|cheers|regards|best regards|kind regards|thx)[,!.]?|-?\s*\p{Lu}[\p{L}'-]*(?: \p{Lu}[\p{L}.'-]*){0,2},?)$`)
)

// snippetMax bounds the snippet in runes; the row's ellipsis cuts it to fit.
const snippetMax = 240

// plainPart is the message's first inline text/plain part, inside an
// alternative or not.
func plainPart(parts []notmuch.Part) *notmuch.Part {
	for i := range parts {
		p := &parts[i]
		if lowerType(p) == "text/plain" && p.Content != "" && !strings.HasPrefix(strings.ToLower(p.ContentDisposition), "attachment") {
			return p
		}
		if c := plainPart(p.Children); c != nil {
			return c
		}
	}
	return nil
}

// snippet is the message's own new text on one line: its plain body above
// the trailing quote (foldStart) and the signature, quoted lines dropped,
// and an opening greeting and a closing sign-off when there is more. Mail
// with no text/plain part has none: stdlib has no HTML parser, and a crude
// strip shows CSS and boilerplate. Image placeholder lines are dropped.
func snippet(m *notmuch.Message) string {
	p := plainPart(m.Body)
	if p == nil {
		return ""
	}
	lines := plainLines(p.Content)
	if f := foldStart(lines); f >= 0 {
		lines = lines[:f]
	}
	var keep []string
	for _, l := range lines {
		if l.text == "-- " || l.text == "--" {
			break
		}
		if l.depth > 0 {
			continue
		}
		if t := strings.TrimSpace(l.text); t != "" && !placeholderRE.MatchString(t) {
			keep = append(keep, t)
		}
	}
	if len(keep) > 1 && greetingRE.MatchString(keep[0]) {
		keep = keep[1:]
	}
	for len(keep) > 1 && utf8.RuneCountInString(keep[len(keep)-1]) <= 24 && signoffRE.MatchString(keep[len(keep)-1]) {
		keep = keep[:len(keep)-1]
	}
	s := strings.Join(strings.Fields(strings.Join(keep, " ")), " ")
	if utf8.RuneCountInString(s) > snippetMax {
		s = string([]rune(s)[:snippetMax])
	}
	return s
}

// recipientNames is an address list as display names (the address when
// unnamed), "me" for the account's own; unparseable input comes back whole.
func recipientNames(list, me string) string {
	if strings.TrimSpace(list) == "" {
		return ""
	}
	as, err := mail.ParseAddressList(list)
	if err != nil {
		return list
	}
	out := make([]string, 0, len(as))
	for _, a := range as {
		switch {
		case me != "" && strings.EqualFold(a.Address, me):
			out = append(out, "me")
		case a.Name != "":
			out = append(out, a.Name)
		default:
			out = append(out, a.Address)
		}
	}
	return strings.Join(out, ", ")
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// calendarDays is the number of midnights from a to b (rounded, so a DST
// change doesn't shift it).
func calendarDays(a, b time.Time) int {
	return int((midnight(b).Sub(midnight(a)).Hours() + 12) / 24)
}

// relDay names t's day: Today, Yesterday, else the date, with the year
// when it isn't now's. A bare weekday was tried and read as the coming one.
func relDay(t, now time.Time) string {
	switch calendarDays(t, now) {
	case 0:
		return "Today"
	case 1:
		return "Yesterday"
	}
	if t.Year() != now.Year() {
		return t.Format("Mon Jan 2, 2006")
	}
	return t.Format("Mon Jan 2")
}

// gapLabel names a silence of two calendar days or more between messages.
func gapLabel(prev, t time.Time) string {
	n := calendarDays(prev, t)
	switch {
	case n < 2:
		return ""
	case n < 14:
		return strconv.Itoa(n) + " days later"
	case n < 60:
		return strconv.Itoa(n/7) + " weeks later"
	case n < 730:
		return strconv.Itoa(n/30) + " months later"
	}
	return strconv.Itoa(n/365) + " years later"
}

func clock(t time.Time) string { return t.Format("3:04 PM") }

// shortAuthors is a list of several authors as first names (firstName);
// a single author stays whole.
func shortAuthors(list string) string {
	parts := strings.Split(list, ", ")
	if len(parts) < 2 {
		return list
	}
	for i, p := range parts {
		if f := firstName(p); f != "" {
			parts[i] = f
		}
	}
	return strings.Join(parts, ", ")
}
