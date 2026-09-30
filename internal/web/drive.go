package web

import (
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jmckible/pneu/internal/notmuch"
)

// A shared Doc, Sheet or folder is not a MIME part, only a link in the body.
// The thread page lists each one under the message as a chip, like an
// attachment. The link is rebuilt from kind and file id, never the sender's
// URL, with authuser set so the account the mail came to opens it. The
// thread's files are listed together on its newest message.

// driveKind is one kind of Drive file: its data-kind, label, and link.
type driveKind struct {
	Name, Label string
	link        func(id string) string
}

var (
	kindDoc     = &driveKind{"doc", "Google Doc", docsLink("document", "edit")}
	kindSheet   = &driveKind{"sheet", "Google Sheet", docsLink("spreadsheets", "edit")}
	kindSlides  = &driveKind{"slides", "Google Slides", docsLink("presentation", "edit")}
	kindForm    = &driveKind{"form", "Google Form", docsLink("forms", "edit")}
	kindFormE   = &driveKind{"form", "Google Form", func(id string) string { return "https://docs.google.com/forms/d/e/" + id + "/viewform" }}
	kindDrawing = &driveKind{"drawing", "Google Drawing", docsLink("drawings", "edit")}
	kindFile    = &driveKind{"file", "Drive file", func(id string) string { return "https://drive.google.com/file/d/" + id + "/view" }}
	kindFolder  = &driveKind{"folder", "Drive folder", func(id string) string { return "https://drive.google.com/drive/folders/" + id }}
)

func docsLink(app, action string) func(string) string {
	return func(id string) string { return "https://docs.google.com/" + app + "/d/" + id + "/" + action }
}

var docsApps = map[string]*driveKind{
	"document": kindDoc, "spreadsheets": kindSheet, "presentation": kindSlides,
	"forms": kindForm, "drawings": kindDrawing, "file": kindFile,
}

// driveIDRE is strict: real ids are 25+ characters, and a short one is more
// likely a mangled link than a file.
var driveIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{15,}$`)

// driveURLRE finds candidate links in text or raw HTML. The scheme must
// directly precede the exact host and a path or query must follow, so
// docs.google.com.evil.com and evil.com/docs.google.com/… never match.
var driveURLRE = regexp.MustCompile(`\bhttps?://(?:docs|drive)\.google\.com[/?][^\s"'<>]*`)

// anchorRE is an HTML anchor: its href (any quoting) and inner HTML.
var anchorRE = regexp.MustCompile(`(?is)<a\s(?:[^>]*?\s)?href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))[^>]*>(.*?)</a\s*>`)

var tagRE = regexp.MustCompile(`(?s)<[^>]*>`)

// driveRef is one Drive file a message links to.
type driveRef struct {
	Kind  *driveKind
	ID    string
	Title string // anchor text, "" when none was usable
}

func (r driveRef) key() string { return r.Kind.Name + "/" + r.ID }

// parseDrive reads a Drive link. raw may still carry HTML entities (an
// href's &amp;) and trailing prose punctuation.
func parseDrive(raw string) (driveRef, bool) {
	raw = strings.TrimRight(html.UnescapeString(raw), ".,;:!?)]}'\"")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" {
		return driveRef{}, false
	}
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	// /document/u/0/d/<id>: drop the account selector.
	if len(seg) >= 3 && seg[1] == "u" {
		seg = append(seg[:1:1], seg[3:]...)
	}
	ref := driveRef{}
	switch u.Host {
	case "docs.google.com":
		switch {
		case len(seg) >= 4 && seg[0] == "forms" && seg[1] == "d" && seg[2] == "e":
			ref.Kind, ref.ID = kindFormE, seg[3] // a form's responder link
		case len(seg) >= 3 && docsApps[seg[0]] != nil && seg[1] == "d":
			ref.Kind, ref.ID = docsApps[seg[0]], seg[2]
		case len(seg) == 1 && seg[0] == "open":
			ref.Kind, ref.ID = kindFile, u.Query().Get("id")
		}
	case "drive.google.com":
		switch {
		case len(seg) >= 3 && seg[0] == "file" && seg[1] == "d":
			ref.Kind, ref.ID = kindFile, seg[2]
		case len(seg) >= 3 && seg[0] == "drive" && seg[1] == "folders":
			ref.Kind, ref.ID = kindFolder, seg[2]
		case len(seg) == 1 && (seg[0] == "open" || seg[0] == "uc"):
			ref.Kind, ref.ID = kindFile, u.Query().Get("id")
		}
	}
	if ref.Kind == nil || !driveIDRE.MatchString(ref.ID) {
		return driveRef{}, false
	}
	return ref, true
}

// driveRefs lists the Drive files a message's own text links to, once each
// by (kind, id), in order of first appearance. Every inline text/plain and
// text/html part is read (the chosen body and the alternative it beat);
// forwarded messages are not. Titles come from HTML anchor text.
func driveRefs(m *notmuch.Message) []driveRef {
	var out []driveRef
	at := map[string]int{}
	var walk func(parts []notmuch.Part)
	walk = func(parts []notmuch.Part) {
		for i := range parts {
			p := &parts[i]
			walk(p.Children)
			ct := lowerType(p)
			if !p.HasContent || isAttachment(p) || (ct != "text/plain" && ct != "text/html") {
				continue
			}
			for _, raw := range driveURLRE.FindAllString(p.Content, -1) {
				if r, ok := parseDrive(raw); ok {
					if _, dup := at[r.key()]; !dup {
						at[r.key()] = len(out)
						out = append(out, r)
					}
				}
			}
			if ct != "text/html" {
				continue
			}
			for _, a := range anchorRE.FindAllStringSubmatch(p.Content, -1) {
				r, ok := parseDrive(a[1] + a[2] + a[3])
				if !ok {
					continue
				}
				i, seen := at[r.key()]
				if !seen || out[i].Title != "" {
					continue
				}
				out[i].Title = anchorTitle(a[4])
			}
		}
	}
	walk(m.Body)
	return out
}

// anchorTitle is an anchor's text as a file title, or "" when it names
// nothing: empty, a URL, or a button ("Open", "Open in Docs").
func anchorTitle(inner string) string {
	t := strings.Join(strings.Fields(html.UnescapeString(tagRE.ReplaceAllString(inner, " "))), " ")
	lower := strings.ToLower(t)
	switch {
	case t == "", strings.Contains(lower, "://"), strings.Contains(lower, "google.com/"),
		lower == "open", strings.HasPrefix(lower, "open in "), lower == "view", lower == "edit",
		lower == "here", lower == "click here", lower == "link":
		return ""
	}
	if utf8.RuneCountInString(t) > 80 {
		t = strings.TrimSpace(string([]rune(t)[:79])) + "…"
	}
	return t
}

// driveView is one chip on the thread page.
type driveView struct {
	Key   string // kind/id: a file gets a chip only on its first message
	Href  string
	Kind  string // data-kind
	Label string // "Google Doc"
	Title string // the anchor text, else Label
	Named bool   // Title is the file's own, so the label shows beside it
}

// driveViews builds the chips, each link opening as email's Google account.
func driveViews(refs []driveRef, email string) []driveView {
	var out []driveView
	for _, r := range refs {
		href := r.Kind.link(r.ID)
		if email != "" {
			href += "?" + url.Values{"authuser": {email}}.Encode()
		}
		v := driveView{Key: r.key(), Href: href, Kind: r.Kind.Name, Label: r.Kind.Label, Title: r.Title, Named: r.Title != ""}
		if !v.Named {
			v.Title = r.Kind.Label
		}
		out = append(out, v)
	}
	return out
}

// mergeDrive adds a message's files to the thread's, once each in order of
// first mention (a quoted reply repeats its links). A later mention's title
// fills in for an untitled earlier one.
func mergeDrive(all, add []driveView) []driveView {
	for _, v := range add {
		i := slices.IndexFunc(all, func(o driveView) bool { return o.Key == v.Key })
		switch {
		case i < 0:
			all = append(all, v)
		case !all[i].Named && v.Named:
			all[i] = v
		}
	}
	return all
}
