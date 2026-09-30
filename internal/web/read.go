package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jmckible/pneu/internal/unsub"
	"html/template"
	"log"
	"maps"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
)

// PerPage is the default threads per list page.
const PerPage = 50

// ---- list ------------------------------------------------------------------

type row struct {
	Account string
	Thread  string
	URL     string
	MsgIDs  string // matched ids, url.QueryEscape'd, space-separated: star, unread
	// ThreadIDs is every message of the thread (matched and unmatched, the
	// excluded left out), same form: archive and trash act on all of them.
	ThreadIDs string
	Gmail     string // /gmail/{account}/{thread}: redirects to the newest message in Gmail
	Class     string
	Authors   string
	Subject   string
	Total     int
	Date      string
	ISO       string
	Title     string // absolute local date, for the tooltip
}

type listPage struct {
	Page
	Rows  []row
	Prev  string
	Next  string
	Err   string
	Empty bool // show the mark as a watermark once the list is empty
	// The title row's position (positionText): Start is the page's offset,
	// Total the query's thread count or -1 while unknown (app.js asks for
	// it with ?total=1).
	Start    int
	Total    int
	Position string
}

// positionText is the list title's count: the row count on a single page,
// else "51–100 of 1,234" (without " of …" while the total is unknown).
// triage.js position is the same rule; they must agree.
func positionText(start, rows, total int, paged bool) string {
	if !paged {
		return commas(rows)
	}
	if rows == 0 {
		return "0"
	}
	out := commas(start+1) + "–" + commas(start+rows)
	if total >= 0 {
		out += " of " + commas(total)
	}
	return out
}

// list serves a merged view. An empty fixed query means "use ?q=" (search).
func (s *Server) list(view, title, fixed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		query, userQuery, title := fixed, "", title
		if fixed == "" {
			userQuery = q.Get("q")
			query = userQuery
			if strings.TrimSpace(query) != "" {
				title = query
			}
		}
		page, _ := strconv.Atoi(q.Get("page"))
		page = max(page, 0)
		if q.Get("total") == "1" {
			s.listTotal(w, r, query)
			return
		}

		data := listPage{Page: s.page(r, title, userQuery, s.viewLabel()), Total: -1}
		data.View = view
		status := http.StatusOK
		if strings.TrimSpace(query) != "" {
			threads, more, errs := s.merged(r, query, page)
			selves := s.selves(r.Context())
			var to map[string]string
			if view == "sent" {
				to = s.sentTo(r.Context(), threads, selves)
			}
			for _, t := range threads {
				rw := s.row(t, selves[t.Account])
				if v, ok := to[t.Account+"/"+t.Thread]; ok {
					rw.Authors = v
				}
				data.Rows = append(data.Rows, rw)
			}
			if len(errs) > 0 {
				data.Err = errors.Join(errs...).Error()
				if len(threads) == 0 {
					status = http.StatusBadRequest
					if view != "search" {
						status = http.StatusInternalServerError
					}
				}
			}
			pageURL := func(n int) string {
				v := url.Values{}
				if userQuery != "" {
					v.Set("q", userQuery)
				}
				if n > 0 {
					v.Set("page", strconv.Itoa(n))
				}
				path := r.URL.Path
				if len(v) == 0 {
					return path
				}
				return path + "?" + v.Encode()
			}
			if page > 0 {
				data.Prev = pageURL(page - 1)
			}
			if more {
				data.Next = pageURL(page + 1)
			}
			data.Start = page * s.PerPage
			if data.Prev != "" || data.Next != "" {
				// Only a cached count here: an uncached one can take a
				// second, and the page asks for it once it is showing.
				if n, ok := s.total(r.Context(), query, false); ok {
					data.Total = n
				}
			} else {
				data.Total = len(data.Rows)
			}
		}
		data.Position = positionText(data.Start, len(data.Rows), data.Total, data.Prev != "" || data.Next != "")
		// Rendered with rows too: triage can empty the list in place, and
		// app.css shows the mark only once no row is left.
		data.Empty = data.Err == "" && strings.TrimSpace(query) != ""
		s.render(w, status, "list", data)
	}
}

type totalKey struct{ account, query string }

type totalVal struct {
	rev string
	n   int
}

// total is query's thread count summed over the accounts, as the list's
// rows count them (search.exclude_tags applies). Each account's count is
// cached at its database revision; a revision check is a few milliseconds.
// With count false only cached counts answer; ok is false on any miss or
// error.
func (s *Server) total(ctx context.Context, query string, count bool) (int, bool) {
	ns := make([]int, len(s.Accounts))
	oks := make([]bool, len(s.Accounts))
	var wg sync.WaitGroup
	for i, a := range s.Accounts {
		wg.Go(func() {
			rev, err := a.Revision(ctx)
			if err != nil {
				log.Printf("total %q: %v", query, err)
				return
			}
			key := totalKey{a.Name, query}
			if v, ok := s.totals.Load(key); ok && v.(totalVal).rev == rev {
				ns[i], oks[i] = v.(totalVal).n, true
				return
			}
			if !count {
				return
			}
			// rev was read first: a write during the count leaves an entry
			// whose revision is already stale, so the next look recounts.
			n, err := a.Count(ctx, query, true)
			if err != nil {
				log.Printf("total %q: %v", query, err)
				return
			}
			s.totals.Store(key, totalVal{rev, n})
			ns[i], oks[i] = n, true
		})
	}
	wg.Wait()
	sum := 0
	for i := range s.Accounts {
		if !oks[i] {
			return 0, false
		}
		sum += ns[i]
	}
	return sum, true
}

// listTotal answers ?total=1 on a list URL: {"total": n}, counted if need
// be. The list page asks for it when it rendered without one.
func (s *Server) listTotal(w http.ResponseWriter, r *http.Request, query string) {
	n, ok := 0, false
	if strings.TrimSpace(query) != "" {
		n, ok = s.total(r.Context(), query, true)
	}
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"count failed"}`))
		return
	}
	fmt.Fprintf(w, `{"total":%d}`, n)
}

// merged runs query on every account, newest first. Each account returns
// its first (page+1)*PerPage+1 threads, so the merged slice for this page is
// exact and one extra says whether a next page exists. Ties break on account
// order, then thread id.
func (s *Server) merged(r *http.Request, query string, page int) ([]notmuch.ThreadSummary, bool, []error) {
	per := s.PerPage
	limit := (page+1)*per + 1
	results := make([][]notmuch.ThreadSummary, len(s.Accounts))
	errs := make([]error, len(s.Accounts))
	var wg sync.WaitGroup
	for i, a := range s.Accounts {
		wg.Go(func() {
			results[i], errs[i] = a.Search(r.Context(), query, notmuch.SearchOpts{Limit: limit})
		})
	}
	wg.Wait()
	var all []notmuch.ThreadSummary
	var failed []error
	for i := range s.Accounts {
		if errs[i] != nil {
			log.Printf("search %q: %v", query, errs[i])
			failed = append(failed, errs[i])
			continue
		}
		all = append(all, results[i]...)
	}
	slices.SortStableFunc(all, func(a, b notmuch.ThreadSummary) int {
		return cmp.Or(
			cmp.Compare(b.Timestamp, a.Timestamp),
			cmp.Compare(s.order[a.Account], s.order[b.Account]),
			strings.Compare(a.Thread, b.Thread),
		)
	})
	start, end := page*per, (page+1)*per
	more := len(all) > end
	all = all[min(start, len(all)):min(end, len(all))]
	return all, more, failed
}

func (s *Server) row(t notmuch.ThreadSummary, me self) row {
	matched := escapeIDs(matchedIDs(t.Query[0]))
	thread := append(slices.Clone(matched), escapeIDs(matchedIDs(t.Query[1]))...)
	class := "row"
	if slices.Contains(t.Tags, "unread") {
		class += " unread"
	}
	if slices.Contains(t.Tags, "flagged") {
		class += " flagged"
	}
	// notmuch's own index-time tag, on any message of the thread.
	if slices.Contains(t.Tags, "attachment") {
		class += " attach"
	}
	subject := t.Subject
	if strings.TrimSpace(subject) == "" {
		subject = "(no subject)"
	}
	when := time.Unix(t.Timestamp, 0).Local()
	return row{
		Account:   t.Account,
		Thread:    t.Thread,
		URL:       "/t/" + url.PathEscape(t.Account) + "/" + url.PathEscape(t.Thread),
		MsgIDs:    strings.Join(matched, " "),
		ThreadIDs: strings.Join(thread, " "),
		Gmail:     "/gmail/" + url.PathEscape(t.Account) + "/" + url.PathEscape(t.Thread),
		Class:     class,
		Authors:   authors(t.Authors, me),
		Subject:   subject,
		Total:     t.Total,
		Date:      shortDate(when, s.now()),
		ISO:       when.Format(time.RFC3339),
		Title:     longDate(when),
	}
}

// self is who "me" is in one account: its user.name and address.
type self struct{ Name, Email string }

// author says whether an author-list entry (a name, or a bare address) is me.
func (m self) author(n string) bool { return (m.Name != "" && n == m.Name) || m.address(n) }

func (m self) address(a string) bool { return m.Email != "" && strings.EqualFold(a, m.Email) }

// selves reads each account's user.name once and keeps it; a failed read
// is retried on the next list (the row then shows the name, not "me").
func (s *Server) selves(ctx context.Context) map[string]self {
	out := make(map[string]self, len(s.Accounts))
	for _, a := range s.Accounts {
		if v, ok := s.userNames.Load(a.Name); ok {
			out[a.Name] = self{v.(string), a.Email}
			continue
		}
		name, err := a.UserName(ctx)
		if err != nil {
			log.Printf("list %s: user.name: %v", a.Name, err)
		} else {
			s.userNames.Store(a.Name, name)
		}
		out[a.Name] = self{name, a.Email}
	}
	return out
}

// authors is notmuch's author list (matched, then "| " and unmatched;
// names only, or the address when a sender gave none) with the account's
// own name shown as "me", once, as Gmail does.
func authors(list string, me self) string {
	var out []string
	seenMe := false
	for _, half := range strings.Split(list, "| ") {
		for _, n := range strings.Split(half, ", ") {
			if n == "" {
				continue
			}
			if me.author(n) {
				if seenMe {
					continue
				}
				n, seenMe = "me", true
			}
			out = append(out, n)
		}
	}
	return strings.Join(out, ", ")
}

// sentTo is the Sent view's name column, "To: …" from each thread's newest
// matched (sent) message, keyed account/thread. One headers-only show per
// account; a failure leaves that account's rows on their authors.
func (s *Server) sentTo(ctx context.Context, threads []notmuch.ThreadSummary, selves map[string]self) map[string]string {
	owner := map[string]map[string]string{} // account → msgid → thread
	for _, t := range threads {
		if owner[t.Account] == nil {
			owner[t.Account] = map[string]string{}
		}
		for _, id := range matchedIDs(t.Query[0]) {
			owner[t.Account][id] = t.Thread
		}
	}
	out := map[string]string{}
	for _, a := range s.Accounts {
		ids := owner[a.Name]
		if len(ids) == 0 {
			continue
		}
		msgs, err := a.Headers(ctx, notmuch.IDsQuery(slices.Collect(maps.Keys(ids))))
		if err != nil {
			log.Printf("sent %s: headers: %v", a.Name, err)
			continue
		}
		newest := map[string]notmuch.Message{}
		for _, m := range msgs {
			th, ok := ids[m.ID]
			if !ok {
				continue
			}
			if cur, seen := newest[th]; !seen || m.Timestamp > cur.Timestamp {
				newest[th] = m
			}
		}
		for th, m := range newest {
			out[a.Name+"/"+th] = "To: " + recipients(m.Headers["To"], selves[a.Name])
		}
	}
	return out
}

// recipients names a To header's addresses for the list: display name,
// else address, and "me" for the account's own address.
func recipients(to string, me self) string {
	to = strings.TrimSpace(to)
	if to == "" {
		return "(no recipients)"
	}
	list, err := mail.ParseAddressList(to)
	if err != nil || len(list) == 0 { // unparsable, or an empty group
		return to // decoded already; show it as sent
	}
	names := make([]string, 0, len(list))
	for _, a := range list {
		switch {
		case me.address(a.Address):
			names = append(names, "me")
		case a.Name != "":
			names = append(names, a.Name)
		default:
			names = append(names, a.Address)
		}
	}
	return strings.Join(names, ", ")
}

func shortDate(t, now time.Time) string {
	now = now.Local()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return t.Format("3:04 PM")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("1/2/06")
	}
}

func longDate(t time.Time) string { return t.Format("Mon, Jan 2, 2006 at 3:04 PM") }

// ---- thread ----------------------------------------------------------------

var threadIDRE = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

type messageView struct {
	ID       string // raw Message-ID
	MsgID    string // url.PathEscape(ID), for data-msgid and URLs
	Depth    int
	Pos      int // 1-based position among the shown messages
	Class    string
	FromName string
	FromAddr string
	To       string
	Cc       string
	Date     string
	ISO      string
	Gmail    string
	Kind     string // "html" or "text"; "" when the message has no body
	BodyURL  string
	// HoldImages keeps remote images behind a click: spam and trash, where
	// an open confirms the address. Everywhere else they load (app.js).
	HoldImages bool
	// Unsub: the message has a List-Unsubscribe header, so the key bar
	// shows X for it.
	Unsub  bool
	Text   template.HTML
	Attach []attachView
	Drive  []driveView // the thread's Drive files, on its newest message (thread sets it)
}

type attachView struct {
	Href   string
	Name   string
	Inline bool   // the part endpoint serves it inline: open it in a tab, don't force a download
	View   string // the in-app viewer's kind (attach.go viewKind); "" downloads only
}

type threadPage struct {
	Page
	Account  string
	Thread   string
	Subject  string
	Messages []messageView
	Total    int // len(Messages), for the "n of N" header
	// UnreadIDs are the shown unread messages, url.QueryEscape'd and
	// space-separated (the data-msgids form). The page does not tag on GET —
	// that would wait on the write lock during a pull; the browser POSTs
	// action=read with these after render.
	UnreadIDs string
	MsgIDs    string // every shown message, same form: what archive/trash/star send
	// In is "spam" or "trash" when the whole thread is excluded (opened from
	// those views), "" otherwise; app.js keys trash's no-op on it as the list
	// does on data-view. Any spam wins: trash must not add +trash to spam.
	In string
}

func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.account(r)
	id := r.PathValue("thread")
	if !ok || !threadIDRE.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	at := s.viewLabel()
	msgs, err := acct.Show(r.Context(), "thread:"+id)
	if err != nil {
		log.Printf("thread %s/%s: %v", acct.Name, id, err)
		http.Error(w, "notmuch failed", http.StatusInternalServerError)
		return
	}
	data := threadPage{Account: acct.Name, Thread: id}
	// Trash/spam inside a live thread stays hidden, as in Gmail. A thread
	// that is all trash or spam (opened from those views) matches nothing
	// under exclude_tags; naming the tags lifts the exclusion and shows it.
	if !slices.ContainsFunc(msgs, func(m notmuch.Message) bool { return !m.Excluded }) {
		msgs, err = acct.Show(r.Context(), "thread:"+id+" and (tag:spam or tag:trash)")
		if err != nil {
			log.Printf("thread %s/%s: %v", acct.Name, id, err)
			http.Error(w, "notmuch failed", http.StatusInternalServerError)
			return
		}
		if len(msgs) > 0 {
			data.In = "trash"
		}
		if slices.ContainsFunc(msgs, func(m notmuch.Message) bool { return slices.Contains(m.Tags, "spam") }) {
			data.In = "spam"
		}
	}
	var shown, unread []string
	var drive []driveView
	for i := range msgs {
		m := &msgs[i]
		if m.Excluded {
			continue
		}
		shown = append(shown, url.QueryEscape(m.ID))
		if slices.Contains(m.Tags, "unread") {
			unread = append(unread, url.QueryEscape(m.ID))
		}
		if data.Subject == "" {
			data.Subject = m.Headers["Subject"]
		}
		v := s.messageView(acct, m)
		v.Pos = len(data.Messages) + 1
		drive = mergeDrive(drive, driveViews(driveRefs(m), acct.Email))
		data.Messages = append(data.Messages, v)
	}
	if len(data.Messages) == 0 {
		http.NotFound(w, r)
		return
	}
	// On the newest message, which is always expanded: the one that first
	// linked a file is often an older, folded one.
	data.Messages[len(data.Messages)-1].Drive = drive
	if strings.TrimSpace(data.Subject) == "" {
		data.Subject = "(no subject)"
	}
	data.Total = len(data.Messages)
	if !s.readOnly(acct.Name) { // no mark-read while the first pull runs
		data.UnreadIDs = strings.Join(unread, " ")
	}
	data.MsgIDs = strings.Join(shown, " ")
	data.Page = s.page(r, data.Subject, "", at)
	s.render(w, http.StatusOK, "thread", data)
}

func (s *Server) messageView(acct notmuch.Account, m *notmuch.Message) messageView {
	esc := url.PathEscape(m.ID)
	prefix := "/part/" + url.PathEscape(acct.Name) + "/" + esc + "/"
	class := "message"
	unread := slices.Contains(m.Tags, "unread")
	if unread {
		class += " unread"
	}
	if slices.Contains(m.Tags, "flagged") {
		class += " flagged"
	}
	if !unread {
		class += " collapsed"
	}
	name, addr := splitAddress(m.Headers["From"])
	when := time.Unix(m.Timestamp, 0).Local()
	an := analyze(m)
	v := messageView{
		ID:       m.ID,
		MsgID:    esc,
		Depth:    m.Depth,
		Class:    class,
		FromName: name,
		FromAddr: addr,
		To:       m.Headers["To"],
		Cc:       m.Headers["Cc"],
		Date:     longDate(when),
		ISO:      when.Format(time.RFC3339),
		Gmail:    gmailURL(acct.Email, m.Filename),
		Kind:     an.Kind,
		Text:     an.Text,
		Unsub:    unsub.HasList(acct.Maildir, m.Filename),
	}
	if an.Kind == "html" {
		v.BodyURL = "/body/" + url.PathEscape(acct.Name) + "/" + esc
		v.HoldImages = holdImages(m.Tags)
	}
	for _, a := range an.Attachments {
		mt := effectiveType(a.Type, a.Name)
		v.Attach = append(v.Attach, attachView{Href: prefix + strconv.Itoa(a.Part), Name: a.Name, Inline: inlineTypes[mt], View: viewKind(mt)})
	}
	return v
}

// ---- gmail -----------------------------------------------------------------

// gmail handles GET /gmail/{account}/{thread}: a redirect to the thread's
// newest shown message in Gmail (the `v` key on a list row). Resolved on
// demand so a list render doesn't pay a file lookup per row.
func (s *Server) gmail(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.account(r)
	id := r.PathValue("thread")
	if !ok || !threadIDRE.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	files, err := acct.Files(r.Context(), "thread:"+id)
	if err != nil {
		log.Printf("gmail %s/%s: %v", acct.Name, id, err)
		http.Error(w, "notmuch failed", http.StatusInternalServerError)
		return
	}
	// Newest message first; a message with several files names one Gmail id.
	u := gmailURL(acct.Email, files)
	if u == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// ---- body ------------------------------------------------------------------

// holdImages keeps remote images behind a click where an open confirms the
// address: spam and trash.
func holdImages(tags []string) bool {
	return slices.Contains(tags, "spam") || slices.Contains(tags, "trash")
}

// body returns the raw HTML body as JSON, never as a document: the browser
// sanitizes it and renders it only inside the sandboxed frame.
func (s *Server) body(w http.ResponseWriter, r *http.Request) {
	acct, m, ok := s.message(w, r)
	if !ok {
		return
	}
	an := analyze(&m)
	if an.Kind != "html" {
		http.NotFound(w, r)
		return
	}
	prefix := "/part/" + url.PathEscape(acct.Name) + "/" + url.PathEscape(m.ID) + "/"
	cids := map[string]string{}
	for cid, n := range m.CIDs() {
		cids[cid] = prefix + strconv.Itoa(n)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		HTML string            `json:"html"`
		CIDs map[string]string `json:"cids"`
		Hold bool              `json:"hold"`
	}{an.Body.Content, cids, holdImages(m.Tags)}); err != nil {
		log.Printf("body %s: %v", m.ID, err)
	}
}

// message loads {account}/{msgid}, answering 404/500 itself on failure.
func (s *Server) message(w http.ResponseWriter, r *http.Request) (notmuch.Account, notmuch.Message, bool) {
	acct, ok := s.account(r)
	id := r.PathValue("msgid")
	if !ok || id == "" || strings.ContainsAny(id, "\r\n\x00") {
		http.NotFound(w, r)
		return acct, notmuch.Message{}, false
	}
	m, err := acct.Message(r.Context(), id)
	if errors.Is(err, notmuch.ErrNotFound) {
		http.NotFound(w, r)
		return acct, m, false
	}
	if err != nil {
		log.Printf("message %s/%s: %v", acct.Name, id, err)
		http.Error(w, "notmuch failed", http.StatusInternalServerError)
		return acct, m, false
	}
	return acct, m, true
}

// ---- part ------------------------------------------------------------------

// inlineTypes may render in the browser when navigated to. Everything else
// downloads. SVG is deliberately absent: it is a scriptable document. Audio
// and video are inline so <video>/<audio> can play them; navigated to, they
// are the browser's own player, which runs nothing from the file.
var inlineTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"image/avif": true, "image/bmp": true, "application/pdf": true,
	"video/mp4": true, "video/webm": true, "video/quicktime": true, "video/ogg": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/wav": true, "audio/mp4": true, "audio/flac": true,
	"audio/opus": true, "audio/aac": true, "audio/webm": true,
}

// neuter reports media types that are active documents in a browser; they
// are served as text/plain whatever the message claims.
func neuter(mt string) bool {
	return strings.HasPrefix(mt, "text/html") || strings.Contains(mt, "xml") ||
		strings.Contains(mt, "script") || mt == "multipart/x-mixed-replace"
}

func (s *Server) part(w http.ResponseWriter, r *http.Request) {
	_, _, p, body, ct, ok := s.loadPart(w, r)
	if !ok {
		return
	}
	name := partName(p)
	if lowerType(p) == "message/rfc822" {
		name = emlName(p)
	}
	ctype, inline := partHeaders(ct, name)
	if strings.HasPrefix(ctype, "text/plain") && r.Header.Get("Sec-Fetch-Dest") == "image" && svgPart(ct, name) {
		// An <img> (the viewer's) renders SVG as an image: no scripts, no
		// subresources. Only the browser sets Sec-Fetch-Dest; a navigation
		// or fetch() still gets text/plain.
		ctype = "image/svg+xml"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	h.Set("Content-Disposition", disp+"; filename*=UTF-8''"+rfc5987(name))
	// If anything here is ever navigated to and rendered, it runs nothing;
	// a PDF, which the viewer frames, is the exception (PolicyPDF).
	if strings.HasPrefix(ctype, "application/pdf") {
		usePolicy(w, PolicyPDF)
	} else {
		usePolicy(w, PolicyPart)
	}
	// Ranges for <video>/<audio> seeking; HEAD is handled here too. No
	// modtime or ETag, so If-Modified-Since is ignored and an If-Range
	// request gets the whole body, but ServeContent still answers
	// If-None-Match: * with 304 and an If-Match naming a tag with 412.
	singleRange(r)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// svgPart: the part is SVG, by declared type or by name.
func svgPart(declared, name string) bool {
	mt, _, _ := mime.ParseMediaType(declared)
	return effectiveType(strings.ToLower(mt), name) == "image/svg+xml"
}

// partHeaders turns a message-declared content type into the one served.
func partHeaders(declared, name string) (ctype string, inline bool) {
	mt, params, err := mime.ParseMediaType(declared)
	if err != nil {
		mt = ""
	}
	mt = effectiveType(strings.ToLower(mt), name)
	if !strings.Contains(mt, "/") {
		return "application/octet-stream", false
	}
	out := map[string]string{}
	if cs := params["charset"]; cs != "" {
		out["charset"] = cs
	}
	if neuter(mt) {
		mt = "text/plain"
	}
	ctype = mime.FormatMediaType(mt, out)
	if ctype == "" {
		ctype = mime.FormatMediaType(mt, nil)
	}
	return ctype, inlineTypes[mt]
}

// rfc5987 percent-encodes everything outside attr-char.
func rfc5987(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
