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
	var lines []textLine
	if flowed {
		lines = flowedLines(s, delsp)
	} else {
		lines = plainLines(s)
	}
	var b strings.Builder
	depth := 0
	for _, l := range lines {
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
	return template.HTML(b.String())
}

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
