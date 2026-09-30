package web

import (
	"bufio"
	"html/template"
	"mime"
	"net/mail"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jmckible/pneu/internal/notmuch"
)

// attachment is one downloadable part.
type attachment struct {
	Part int
	Name string
	Type string // lower-cased media type as declared
}

// analysis is what the thread view needs from a message's part tree.
type analysis struct {
	Body        *notmuch.Part // chosen body: text/html or text/plain, or nil
	Kind        string        // "html", "text", or ""
	Text        template.HTML // rendered text body (Kind == "text")
	Attachments []attachment
}

func lowerType(p *notmuch.Part) string { return strings.ToLower(strings.TrimSpace(p.ContentType)) }

// isAttachment: an explicit attachment disposition or a filename.
func isAttachment(p *notmuch.Part) bool {
	return p.Filename != "" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.ContentDisposition)), "attachment")
}

// bodyOf picks the displayable body under p: text/html over text/plain in
// multipart/alternative (the later alternative wins ties, per RFC 2046), the
// first body-bearing child of any other multipart, and never a part that is
// an attachment or inside an embedded message.
func bodyOf(p *notmuch.Part) *notmuch.Part {
	ct := lowerType(p)
	switch {
	case ct == "multipart/alternative":
		var html, plain *notmuch.Part
		for i := range p.Children {
			b := bodyOf(&p.Children[i])
			switch {
			case b == nil:
			case lowerType(b) == "text/html":
				html = b
			case plain == nil:
				plain = b
			}
		}
		if html != nil {
			return html
		}
		return plain
	case strings.HasPrefix(ct, "multipart/"):
		for i := range p.Children {
			if b := bodyOf(&p.Children[i]); b != nil {
				return b
			}
		}
	case ct == "text/html" || ct == "text/plain":
		if !isAttachment(p) {
			return p
		}
	}
	return nil
}

func chooseBody(parts []notmuch.Part) *notmuch.Part {
	for i := range parts {
		if b := bodyOf(&parts[i]); b != nil {
			return b
		}
	}
	return nil
}

// analyze chooses the body and lists attachments. For a text body, inline
// text parts outside multipart/alternative and the text bodies of embedded
// (forwarded) messages are appended to the rendered text; an embedded
// message that can't be folded in is offered as a .eml download instead.
func analyze(m *notmuch.Message) analysis {
	var a analysis
	a.Body = chooseBody(m.Body)
	if a.Body != nil {
		if lowerType(a.Body) == "text/html" {
			a.Kind = "html"
		} else {
			a.Kind = "text"
		}
	}
	var text strings.Builder
	if a.Kind == "text" {
		flowed, delsp := false, false
		// notmuch's JSON drops Content-Type parameters, so format=flowed is only
		// knowable from the file itself, and only cheaply for a single-part message.
		if len(m.Body) == 1 && &m.Body[0] == a.Body && len(m.Filename) > 0 {
			flowed, delsp = flowedParams(m.Filename[0])
		}
		text.WriteString(string(RenderText(a.Body.Content, flowed, delsp)))
	}
	htmlLower := ""
	if a.Kind == "html" {
		htmlLower = strings.ToLower(a.Body.Content)
	}
	used := map[*notmuch.Part]bool{a.Body: true}

	var collect func(parts []notmuch.Part, inAlt bool)
	collect = func(parts []notmuch.Part, inAlt bool) {
		for i := range parts {
			p := &parts[i]
			ct := lowerType(p)
			switch {
			case strings.HasPrefix(ct, "multipart/"):
				collect(p.Children, inAlt || ct == "multipart/alternative")
				continue
			case ct == "message/rfc822":
				folded := a.Kind == "text" && len(p.Messages) > 0
				for j := range p.Messages {
					if b := chooseBody(p.Messages[j].Body); b == nil || lowerType(b) != "text/plain" {
						folded = false
					}
				}
				if !folded {
					a.Attachments = append(a.Attachments, attachment{p.ID, emlName(p), lowerType(p)})
					continue
				}
				for j := range p.Messages {
					e := &p.Messages[j]
					b := chooseBody(e.Body)
					used[b] = true
					text.WriteString(string(RenderText(forwardHeader(e.Headers), false, false)))
					text.WriteString(string(RenderText(b.Content, false, false)))
					collect(e.Body, false)
				}
				continue
			}
			if used[p] {
				continue
			}
			if p.ContentID != "" && htmlLower != "" && strings.Contains(htmlLower, "cid:"+strings.ToLower(p.ContentID)) {
				continue // shown inline by the HTML body
			}
			if isAttachment(p) || ct == "text/calendar" || ct == "application/ics" {
				a.Attachments = append(a.Attachments, attachment{p.ID, partName(p), lowerType(p)})
				continue
			}
			if inAlt && (ct == "text/plain" || ct == "text/html") {
				continue // the alternative we didn't choose
			}
			if a.Kind == "text" && ct == "text/plain" && p.HasContent {
				text.WriteString("\n")
				text.WriteString(string(RenderText(p.Content, false, false)))
				continue
			}
			a.Attachments = append(a.Attachments, attachment{p.ID, partName(p), lowerType(p)})
		}
	}
	collect(m.Body, false)
	if a.Kind == "text" {
		a.Text = template.HTML(text.String())
	}
	return a
}

// HTMLBody is the HTML body the thread view renders for m (analyze's
// choice, what /body serves), or false when it renders none. For tools
// that must see exactly what the app shows (internal/testmail/cmd/ctaeval).
func HTMLBody(m *notmuch.Message) (string, bool) {
	a := analyze(m)
	if a.Kind != "html" {
		return "", false
	}
	return a.Body.Content, true
}

func forwardHeader(h map[string]string) string {
	var b strings.Builder
	b.WriteString("\n---------- Forwarded message ----------\n")
	for _, k := range []string{"From", "Date", "Subject", "To", "Cc"} {
		if v := h[k]; v != "" {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

var extByType = map[string]string{
	"text/plain":       ".txt",
	"text/html":        ".html",
	"text/calendar":    ".ics",
	"application/ics":  ".ics",
	"application/pdf":  ".pdf",
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/gif":        ".gif",
	"image/webp":       ".webp",
	"message/rfc822":   ".eml",
	"application/zip":  ".zip",
	"text/csv":         ".csv",
	"application/json": ".json",
}

// partName is the part's filename, or a stable made-up one.
func partName(p *notmuch.Part) string {
	if name := cleanFilename(p.Filename); name != "" {
		return name
	}
	return "part-" + strconv.Itoa(p.ID) + extByType[lowerType(p)]
}

func emlName(p *notmuch.Part) string {
	if name := cleanFilename(p.Filename); name != "" {
		return name
	}
	if len(p.Messages) > 0 {
		if s := cleanFilename(p.Messages[0].Headers["Subject"]); s != "" {
			return s + ".eml"
		}
	}
	return "message-" + strconv.Itoa(p.ID) + ".eml"
}

// cleanFilename drops control characters, format characters (bidi overrides
// such as U+202E turn "gpj.exe" into what reads as "exe.jpg"; zero-width
// joiners hide text) and path separators.
func cleanFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// findPart returns part n of the message tree, or nil.
func findPart(m *notmuch.Message, n int) *notmuch.Part {
	var found *notmuch.Part
	notmuch.Walk(m.Body, func(p *notmuch.Part) bool {
		if p.ID == n {
			found = p
			return false
		}
		return true
	})
	return found
}

// flowedParams reads the message file's top-level Content-Type.
func flowedParams(filename string) (flowed, delsp bool) {
	f, err := os.Open(filename)
	if err != nil {
		return false, false
	}
	defer f.Close()
	h, err := textproto.NewReader(bufio.NewReader(f)).ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return false, false
	}
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mt != "text/plain" {
		return false, false
	}
	return strings.EqualFold(params["format"], "flowed"), strings.EqualFold(params["delsp"], "yes")
}

var gmailIDRE = regexp.MustCompile(`^[0-9a-f]{8,}$`)

// gmailURL deep-links a message: lieer names files "<gmail id>:2,<flags>".
func gmailURL(email string, filenames []string) string {
	for _, f := range filenames {
		id, _, _ := strings.Cut(filepath.Base(f), ":")
		if gmailIDRE.MatchString(id) {
			return "https://mail.google.com/mail/u/" + url.PathEscape(email) + "/#all/" + id
		}
	}
	return ""
}

// splitAddress returns display name and address; unparseable input comes
// back whole as the name.
func splitAddress(s string) (name, addr string) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return strings.TrimSpace(s), ""
	}
	return a.Name, a.Address
}

// matchedIDs parses the matched half of a search summary's "query" field:
// space-separated id: terms, quoted with "" doubling when needed.
func matchedIDs(q *string) []string {
	if q == nil {
		return nil
	}
	s := *q
	var ids []string
	for {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			return ids
		}
		if !strings.HasPrefix(s, "id:") {
			return nil // not the shape notmuch prints; refuse rather than guess
		}
		s = s[3:]
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for {
				if i >= len(s) {
					return nil
				}
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			ids = append(ids, b.String())
			s = s[i:]
			continue
		}
		end := strings.IndexByte(s, ' ')
		if end < 0 {
			end = len(s)
		}
		ids = append(ids, s[:end])
		s = s[end:]
	}
}
