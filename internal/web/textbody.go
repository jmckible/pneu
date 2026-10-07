package web

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// textLine is one logical line of a text/plain body: format=flowed soft
// breaks already joined, quote markers stripped into depth.
type textLine struct {
	depth int
	text  string
}

// RenderText turns a text/plain body into HTML for the inside of a <pre>:
// every byte of message text goes through html.EscapeString, URLs become
// target=_blank links, and quoted lines nest in one <blockquote class="q">
// per level. The result is balanced, so segments can be concatenated.
//
// This is the only place message content becomes template.HTML.
func RenderText(s string, flowed, delsp bool) template.HTML {
	return renderText(s, flowed, delsp, false)
}

// renderText is RenderText, and with fold the body's trailing quote
// (foldStart) goes inside <details class="qfold">, closed, behind a "···"
// summary: Gmail's trimmed content, for text.
func renderText(s string, flowed, delsp, fold bool) template.HTML {
	var lines []textLine
	if flowed {
		lines = flowedLines(s, delsp)
	} else {
		lines = plainLines(s)
	}
	f := -1
	if fold {
		f = foldStart(lines)
	}
	var b strings.Builder
	depth := 0
	for i, l := range lines {
		if i == f {
			for ; depth > 0; depth-- {
				b.WriteString(`</blockquote>`)
			}
			b.WriteString(`<details class="qfold"><summary title="Show quoted text">···</summary>`)
		}
		for ; depth < l.depth; depth++ {
			b.WriteString(`<blockquote class="q">`)
		}
		for ; depth > l.depth; depth-- {
			b.WriteString(`</blockquote>`)
		}
		linkify(&b, l.text)
		b.WriteByte('\n')
	}
	for ; depth > 0; depth-- {
		b.WriteString(`</blockquote>`)
	}
	if f >= 0 {
		b.WriteString(`</details>`)
	}
	return template.HTML(b.String())
}

var (
	// replyHeaderRE is the line an Outlook-style reply starts its unquoted
	// copy of the original with, the From:/Sent: block right under it.
	replyHeaderRE = regexp.MustCompile(`^(?:-{3,} ?Original Message ?-{3,}|_{10,})\s*$`)
	fromRE        = regexp.MustCompile(`(?i)^(?:From|Von|De|Van|Da):\s`)
	sentRE        = regexp.MustCompile(`(?i)^(?:Sent|Date|Gesendet|Envoyé|Enviado|Verzonden|Inviato):\s`)
	// forwardRE marks a quote that is the message (a forward), not a copy
	// of the one it answers: it never folds.
	forwardRE = regexp.MustCompile(`(?i)forwarded message|begin forwarded|weitergeleitete nachricht|message transféré|mensaje reenviado|^(?:subject|betreff|objet|asunto):\s*(?:fwd?|wg|tr|rv)\s*:`)
)

// foldStart is the index of the first line of lines' trailing quote, or -1.
// The quote is either the last run of '>' lines with nothing after it but
// blank lines, together with the "... wrote:" line right above it, or an
// Outlook-style copy: a "-----Original Message-----" or underscore rule, or
// a From: line, followed by From:/Sent:, and everything after it. An inline
// reply (text after the quote) doesn't fold, nor a body that is all quote,
// nor a forward.
func foldStart(lines []textLine) int {
	blank := func(i int) bool { return strings.TrimSpace(lines[i].text) == "" }
	header := func(i int) bool {
		if lines[i].depth != 0 {
			return false
		}
		t := strings.TrimSpace(lines[i].text)
		at := func(j int, re *regexp.Regexp) bool {
			return j < len(lines) && lines[j].depth == 0 && re.MatchString(strings.TrimSpace(lines[j].text))
		}
		if replyHeaderRE.MatchString(t) {
			return at(i+1, fromRE) || at(i+2, fromRE)
		}
		return fromRE.MatchString(t) && at(i+1, sentRE)
	}
	f := -1
	for i := range lines {
		if header(i) {
			f = i
			break
		}
	}
	if f < 0 {
		last := len(lines) - 1
		for last >= 0 && blank(last) {
			last--
		}
		if last < 0 || lines[last].depth == 0 {
			return -1
		}
		q := last
		for q > 0 && (lines[q-1].depth > 0 || blank(q-1)) {
			q--
		}
		for blank(q) {
			q++
		}
		f = q
		a := q - 1
		for a >= 0 && blank(a) {
			a--
		}
		if a >= 0 && lines[a].depth == 0 && attributionRE.MatchString(strings.TrimSpace(lines[a].text)) {
			f = a
			// A wrapped "On ..., Name <\naddr> wrote:" is two lines.
			if a > 0 && !blank(a-1) && lines[a-1].depth == 0 && attrOpenRE.MatchString(lines[a-1].text) && !attrOpenRE.MatchString(lines[a].text) {
				f = a - 1
			}
		}
	}
	before := false
	for i := 0; i < f; i++ {
		if !blank(i) {
			before = true
			break
		}
	}
	if !before {
		return -1
	}
	// The forward's own marker may sit just above a From: block.
	for i := max(f-2, 0); i < len(lines) && i < f+8; i++ {
		if forwardRE.MatchString(strings.TrimSpace(lines[i].text)) {
			return -1
		}
	}
	// The blank lines above it go in too, so the pill sits right under
	// the reply.
	for f > 0 && blank(f-1) {
		f--
	}
	return f
}

// attributionRE is how an attribution line ends: "... wrote:", "... a
// écrit :", "... schrieb ...:". Any other line ending in a colon is the
// reply's own ("the list:"), and stays out of the fold.
var attributionRE = regexp.MustCompile(`(?i)(?:wrote|[ée]crit|schrieb[^:]*|escribi[óo]|scritto|schreef|escreveu)\s*:$`)

// attrOpenRE is how an attribution line opens: "On Tue, ... wrote:",
// "Le ... a écrit :", "Am ... schrieb ...:".
var attrOpenRE = regexp.MustCompile(`^(?:On|Le|Am|El|Il|Op|Em)\s`)

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// plainLines reads quote depth loosely: "> > x", ">>x" and "> x" all count.
func plainLines(s string) []textLine {
	raw := splitLines(s)
	out := make([]textLine, 0, len(raw))
	for _, l := range raw {
		d, i := 0, 0
		for {
			j := i
			for d > 0 && j < len(l) && l[j] == ' ' {
				j++
			}
			if j < len(l) && l[j] == '>' {
				d++
				i = j + 1
				continue
			}
			break
		}
		rest := l[i:]
		if d > 0 {
			rest = strings.TrimPrefix(rest, " ")
		}
		out = append(out, textLine{d, rest})
	}
	return out
}

// flowedLines decodes RFC 3676: quote depth is the run of '>', then one
// stuffed space is removed, and a line ending in a space (other than the
// "-- " signature separator) joins the next line at the same depth.
func flowedLines(s string, delsp bool) []textLine {
	var out []textLine
	var cur *textLine
	for _, l := range splitLines(s) {
		d := 0
		for d < len(l) && l[d] == '>' {
			d++
		}
		l = strings.TrimPrefix(l[d:], " ")
		soft := strings.HasSuffix(l, " ") && l != "-- "
		if soft && delsp {
			l = l[:len(l)-1]
		}
		if cur != nil && cur.depth == d {
			cur.text += l
		} else {
			out = append(out, textLine{d, l})
			cur = &out[len(out)-1]
		}
		if !soft {
			cur = nil
		}
	}
	return out
}

var urlRE = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"'\x60\x00-\x1f\x7f]+`)

// linkify escapes s and wraps http(s) URLs in links. Trailing sentence
// punctuation and an unbalanced closing paren stay outside the link.
func linkify(b *strings.Builder, s string) {
	last := 0
	for _, m := range urlRE.FindAllStringIndex(s, -1) {
		start, end := m[0], m[1]
		for end > start {
			c := s[end-1]
			if strings.IndexByte(".,;:!?*", c) >= 0 ||
				c == ')' && strings.Count(s[start:end], "(") < strings.Count(s[start:end], ")") ||
				c == ']' && strings.Count(s[start:end], "[") < strings.Count(s[start:end], "]") {
				end--
				continue
			}
			break
		}
		if u := s[start:end]; strings.HasSuffix(u, "://") {
			continue
		}
		b.WriteString(html.EscapeString(s[last:start]))
		u := html.EscapeString(s[start:end])
		b.WriteString(`<a href="` + u + `" target="_blank" rel="noopener">` + u + `</a>`)
		last = end
	}
	b.WriteString(html.EscapeString(s[last:]))
}
