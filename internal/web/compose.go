package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
)

const (
	maxSendBody  = 4 << 20
	addressTTL   = 10 * time.Minute
	maxAddresses = 20
	sentMemory   = 64 // Message-IDs remembered for double-submit protection
	maxRefs      = 20 // References kept on a reply, newest last
	// SendWait bounds a send's wait for the account lock (a sync may hold
	// it for minutes); past it POST /send answers 503 and the draft stays.
	SendWait = 20 * time.Second
	// sentNoCopy is the outbox's redirect for a message Gmail accepted but
	// lieer failed to store locally: never a URL, never resent.
	sentNoCopy = "\x00sent-no-copy"
)

const (
	msgBusy       = "Sync in progress, try again in a moment."
	msgSentNoCopy = "Sent, but the local copy failed; check Sent in Gmail before retrying."
)

// ---- state -----------------------------------------------------------------

// composeState is the compose side's process memory: the address cache and
// the Message-IDs already sent or in flight. The zero value is ready.
type composeState struct {
	mu    sync.Mutex
	addrs map[string]addrCache // account -> recipients of sent mail
	sends map[string]string    // Message-ID -> "" while in flight, redirect once sent
	order []string             // sent (not in-flight) ids, oldest first
}

type addrCache struct {
	at   time.Time
	list []notmuch.AddressEntry
}

// claim marks msgID in flight. If it is already in flight or sent, ok is
// false and done is the sent message's redirect ("" while in flight).
func (c *composeState) claim(msgID string) (done string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, seen := c.sends[msgID]; seen {
		return d, false
	}
	if c.sends == nil {
		c.sends = map[string]string{}
	}
	c.sends[msgID] = ""
	return "", true
}

// finish records msgID as sent (redirect != "") or releases it after a
// failure so the user can retry.
func (c *composeState) finish(msgID, redirect string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if redirect == "" {
		delete(c.sends, msgID)
		return
	}
	c.sends[msgID] = redirect
	c.order = append(c.order, msgID)
	if len(c.order) > sentMemory {
		delete(c.sends, c.order[0])
		c.order = c.order[1:]
	}
}

// ---- pages -----------------------------------------------------------------

type composeAccount struct {
	Name     string
	Label    string // "Name <email>"
	Selected bool
	ReadOnly bool // first pull still running: can't send yet
}

type composePage struct {
	Page
	Accounts   []composeAccount
	Account    string // selected account
	Locked     bool   // replying: the account is the one the original came from
	To         string
	Cc         string
	Bcc        string
	Subject    string
	Body       string
	InReplyTo  string // "<id>", as notmuch reply gives it
	References string
	MessageID  string // generated at render; POSTed back so a resubmit can't send twice
	QuoteFrom  string // HTML-only original: /body URL the browser quotes from
	DraftKey   string // localStorage key: reply:<account>:<msgid> or compose:<account>
	Err        string
}

// identity is the account's From: user.name from its notmuch config and
// its address. Never notmuch reply's From, which picks by the original's
// headers and so answers a cross-account message as the other account.
func (s *Server) identity(ctx context.Context, acct notmuch.Account) mail.Address {
	name, err := acct.UserName(ctx)
	if err != nil {
		log.Printf("compose %s: user.name: %v", acct.Name, err)
	}
	return mail.Address{Name: name, Address: acct.Email}
}

// displayAddr is "Name <addr>" as a person types it (no RFC 2047).
func displayAddr(name, addr string) string {
	if name == "" {
		return addr
	}
	if strings.ContainsAny(name, `()<>[]:;@\,."`) {
		name = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
	}
	return name + " <" + addr + ">"
}

func (s *Server) composeAccounts(ctx context.Context, selected string) []composeAccount {
	out := make([]composeAccount, len(s.Accounts))
	for i, a := range s.Accounts {
		id := s.identity(ctx, a)
		out[i] = composeAccount{Name: a.Name, Label: displayAddr(id.Name, id.Address), Selected: a.Name == selected, ReadOnly: s.readOnly(a.Name)}
	}
	return out
}

func (s *Server) renderCompose(w http.ResponseWriter, r *http.Request, status int, data composePage) {
	title := "Compose"
	if data.InReplyTo != "" {
		title = data.Subject
	}
	data.Page = s.page(title, "", s.viewLabel()) // no list or thread to reconcile; labeled all the same
	data.Accounts = s.composeAccounts(r.Context(), data.Account)
	// The middleware's no-referrer policy makes Chromium send "Origin: null" on
	// a form-navigation POST, which Auth rightly refuses (sandboxed frames send
	// null too). same-origin still sends nothing cross-site and restores the
	// real Origin on POST /send. Only this page is a form.
	w.Header().Set("Referrer-Policy", "same-origin")
	data.Locked = data.InReplyTo != ""
	if data.MessageID == "" {
		data.MessageID = newMessageID()
	}
	if data.DraftKey == "" {
		data.DraftKey = draftKey(data.Account, data.InReplyTo)
	}
	s.render(w, status, "compose", data)
}

func draftKey(account, inReplyTo string) string {
	if inReplyTo != "" {
		return "reply:" + account + ":" + strings.TrimSuffix(strings.TrimPrefix(inReplyTo, "<"), ">")
	}
	return "compose:" + account
}

// reply handles GET /reply/{account}/{msgid}[?all=1].
func (s *Server) reply(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.account(r)
	id := r.PathValue("msgid")
	if !ok || !validID(id) {
		http.NotFound(w, r)
		return
	}
	rep, err := acct.Reply(r.Context(), id, r.URL.Query().Get("all") == "1")
	if errors.Is(err, notmuch.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("reply %s/%s: %v", acct.Name, id, err)
		http.Error(w, "notmuch failed", http.StatusInternalServerError)
		return
	}
	if rep.Original.ID != id {
		http.NotFound(w, r)
		return
	}
	h := rep.Headers
	data := composePage{
		Account: acct.Name,
		To:      h.To,
		Cc:      h.Cc,
		Subject: h.Subject,
	}
	data.InReplyTo, data.References = replyRefs(h, id)
	quote, html := quoteOf(&rep.Original)
	data.Body = "\n\n" + attribution(&rep.Original) + "\n" + quote
	if html {
		data.QuoteFrom = "/body/" + url.PathEscape(acct.Name) + "/" + url.PathEscape(id)
	}
	s.renderCompose(w, r, http.StatusOK, data)
}

// replyRefs takes In-Reply-To and References from notmuch reply, keeping
// only well-formed <msg-id> tokens (the original's headers are the sender's
// to mangle; one stray word must not make the reply unsendable), the last
// maxRefs of References. In-Reply-To falls back to the original's id; it is
// never empty, so the page stays locked to the account.
func replyRefs(h notmuch.ReplyHeaders, id string) (inReplyTo, refs string) {
	toks := msgIDTokenRE.FindAllString(h.References, -1)
	if len(toks) > maxRefs {
		toks = toks[len(toks)-maxRefs:]
	}
	inReplyTo = "<" + id + ">"
	if t := msgIDTokenRE.FindString(h.InReplyTo); t != "" {
		inReplyTo = t
	}
	return inReplyTo, strings.Join(toks, " ")
}

// compose handles GET /compose[?account=name].
func (s *Server) compose(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("account")
	if _, ok := s.byName[name]; !ok && len(s.Accounts) > 0 {
		name = s.Accounts[0].Name
	}
	s.renderCompose(w, r, http.StatusOK, composePage{Account: name})
}

// attribution is the line above the quote, in Gmail's form
// "On <date> Name <addr> wrote:", the one clients look for.
func attribution(m *notmuch.Message) string {
	name, addr := splitAddress(m.Headers["From"])
	who := "someone"
	switch {
	case name != "" && addr != "":
		who = name + " <" + addr + ">"
	case addr != "":
		who = "<" + addr + ">"
	case name != "":
		who = name
	}
	return "On " + longDate(time.Unix(m.Timestamp, 0).Local()) + " " + who + " wrote:"
}

// quoteOf quotes the original's text body. With no text/plain body but an
// HTML one it returns html=true and no text: the server has no HTML parser,
// so the browser quotes from the sanitized DOM.
func quoteOf(m *notmuch.Message) (quote string, html bool) {
	var plain *notmuch.Part
	var walk func(parts []notmuch.Part)
	walk = func(parts []notmuch.Part) { // multiparts only: never into an embedded message
		for i := range parts {
			p := &parts[i]
			if plain != nil {
				return
			}
			switch ct := lowerType(p); {
			case strings.HasPrefix(ct, "multipart/"):
				walk(p.Children)
			case isAttachment(p):
			case ct == "text/plain" && p.HasContent:
				plain = p
			case ct == "text/html":
				html = true
			}
		}
	}
	walk(m.Body)
	if plain == nil {
		return "", html
	}
	return quoteText(plain.Content), false
}

func quoteText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	if i := strings.Index(s, "\n-- \n"); i >= 0 {
		s = s[:i]
	} else if strings.HasPrefix(s, "-- \n") {
		s = ""
	}
	s = strings.TrimRight(s, "\n \t")
	if s == "" {
		return ""
	}
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, " \t") // format=flowed soft breaks become hard
		if line == "" {
			b.WriteString(">\n")
		} else {
			b.WriteString("> " + line + "\n")
		}
	}
	return b.String()
}

// ---- send ------------------------------------------------------------------

// send handles POST /send.
func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSendBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	data := composePage{
		Account:    f.Get("account"),
		To:         f.Get("to"),
		Cc:         f.Get("cc"),
		Bcc:        f.Get("bcc"),
		Subject:    f.Get("subject"),
		Body:       f.Get("body"),
		InReplyTo:  strings.TrimSpace(f.Get("in_reply_to")),
		References: strings.TrimSpace(f.Get("references")),
		MessageID:  f.Get("message_id"),
	}
	if !generatedIDRE.MatchString(data.MessageID) {
		data.MessageID = ""
	}
	fail := func(status int, msg string) {
		data.Err = msg
		s.renderCompose(w, r, status, data)
	}
	acct, ok := s.byName[data.Account]
	if !ok {
		data.Account = ""
		if len(s.Accounts) > 0 {
			data.Account = s.Accounts[0].Name
		}
		fail(http.StatusBadRequest, "Unknown account.")
		return
	}
	if s.readOnly(acct.Name) {
		// gmi send needs the account's lock, which its first pull holds.
		fail(http.StatusConflict, readOnlyMsg(acct.Name)+".")
		return
	}
	if data.MessageID == "" {
		data.MessageID = newMessageID()
	}
	msg, err := buildMessage(s.identity(r.Context(), acct), data, s.now())
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if s.Syncer == nil {
		fail(http.StatusServiceUnavailable, "Sending is unavailable: no sync engine.")
		return
	}
	if done, ok := s.outbox.claim(data.MessageID); !ok {
		if done == sentNoCopy {
			fail(http.StatusConflict, msgSentNoCopy)
			return
		}
		if done != "" {
			http.Redirect(w, r, done+sentFragment, http.StatusSeeOther) // resubmit of a sent message
			return
		}
		fail(http.StatusConflict, "This message is already being sent.")
		return
	}
	// ctx bounds only the wait for the account lock (a sync may hold it);
	// once gmi starts, the send runs to completion.
	wait := s.SendWait
	if wait <= 0 {
		wait = SendWait
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	if res, err := s.Syncer.Send(ctx, acct.Name, bytes.NewReader(msg)); err != nil {
		log.Printf("send %s %s: %v", acct.Name, data.MessageID, err)
		switch {
		case errors.Is(err, gmi.ErrBusy):
			s.outbox.finish(data.MessageID, "")
			w.Header().Set("Retry-After", "10")
			fail(http.StatusServiceUnavailable, msgBusy)
		case res.Accepted():
			// Gmail has it: keep the id claimed so a resubmit can't send it
			// twice, and pull so the sent copy shows up.
			s.outbox.finish(data.MessageID, sentNoCopy)
			s.Syncer.SyncNow(acct.Name)
			fail(http.StatusBadGateway, msgSentNoCopy)
		default:
			s.outbox.finish(data.MessageID, "")
			fail(http.StatusBadGateway, "Not sent: "+err.Error())
		}
		return
	}
	dest := "/"
	var threads []threadRef // a new message's thread is new: unknown, so every page refreshes
	if data.InReplyTo != "" {
		// lieer stores the sent copy itself, so it is usually in the thread
		// already; the original's thread is the one to show either way.
		orig := strings.TrimSuffix(strings.TrimPrefix(data.InReplyTo, "<"), ">")
		if t, err := acct.ThreadOf(context.WithoutCancel(r.Context()), orig); err != nil {
			log.Printf("send %s: thread of %s: %v", acct.Name, orig, err)
		} else if t != "" {
			dest = "/t/" + url.PathEscape(acct.Name) + "/" + url.PathEscape(t)
			threads = []threadRef{{acct.Name, t}}
		}
	}
	// The sent copy is in the database now: Sent and the thread show it.
	s.viewChanged(windowFrom(r), threads)
	s.outbox.finish(data.MessageID, dest)
	http.Redirect(w, r, dest+sentFragment, http.StatusSeeOther)
}

// sentFragment marks the send's own redirect: compose.js deletes the draft
// only on a page reached this way (and strips it from the URL).
const sentFragment = "#sent"

var (
	// msgIDRE is one <msg-id> with nothing that could break a header.
	msgIDRE       = regexp.MustCompile(`^<[^<>\s]{1,995}>$`)
	msgIDTokenRE  = regexp.MustCompile(`<[^<>\s]{1,995}>`)
	generatedIDRE = regexp.MustCompile(`^<[0-9a-f]{32}@pneu\.[A-Za-z0-9.-]{1,253}>$`)
	hostRE        = regexp.MustCompile(`[^A-Za-z0-9.-]`)
)

func newMessageID() string {
	b := make([]byte, 16)
	rand.Read(b)
	host, err := os.Hostname()
	host = strings.Trim(hostRE.ReplaceAllString(host, ""), ".-")
	if err != nil || host == "" {
		host = "localhost"
	}
	return "<" + hex.EncodeToString(b) + "@pneu." + host + ">"
}

// buildMessage renders the RFC 5322 message gmi sends. Errors are the
// user's to fix (shown on the re-rendered form).
func buildMessage(from mail.Address, d composePage, now time.Time) ([]byte, error) {
	var rcpt int
	lists := map[string]string{}
	for _, f := range []struct{ name, value string }{{"To", d.To}, {"Cc", d.Cc}, {"Bcc", d.Bcc}} {
		addrs, err := parseList(f.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", f.name, err)
		}
		rcpt += len(addrs)
		strs := make([]string, len(addrs))
		for i, a := range addrs {
			strs[i] = headerAddr(a)
		}
		lists[f.name] = strings.Join(strs, ", ")
	}
	if rcpt == 0 {
		return nil, errors.New("No recipients.")
	}
	if d.InReplyTo != "" && !msgIDRE.MatchString(d.InReplyTo) {
		return nil, errors.New("Bad In-Reply-To.")
	}
	refs := strings.Fields(d.References)
	for _, ref := range refs {
		if !msgIDRE.MatchString(ref) {
			return nil, errors.New("Bad References.")
		}
	}
	if !generatedIDRE.MatchString(d.MessageID) {
		return nil, errors.New("Bad Message-ID.")
	}

	var b bytes.Buffer
	header := func(name, value string) {
		if value != "" {
			b.WriteString(foldHeader(name, value))
		}
	}
	header("From", headerAddr(&from))
	header("To", lists["To"])
	header("Cc", lists["Cc"])
	// Gmail delivers to Bcc and strips the header from what others receive;
	// lieer checks recipients against To/Cc/Bcc.
	header("Bcc", lists["Bcc"])
	header("Subject", encodeSubject(d.Subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", d.MessageID)
	header("In-Reply-To", d.InReplyTo)
	header("References", strings.Join(refs, " "))
	header("MIME-Version", "1.0")
	if !hasQuote(d.Body) {
		header("Content-Type", "text/plain; charset=utf-8")
		body, cte := encodeBody(d.Body)
		header("Content-Transfer-Encoding", cte)
		b.WriteString("\r\n")
		b.WriteString(body)
		return b.Bytes(), nil
	}
	// A quote goes out with an HTML twin so clients can fold it (see
	// quoteHTML); text/plain stays first and is the message of record.
	boundary := newBoundary()
	header("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, p := range []struct{ ct, s string }{
		{"text/plain; charset=utf-8", d.Body},
		{"text/html; charset=utf-8", quoteHTML(d.Body)},
	} {
		body, cte := encodeBody(p.s)
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + p.ct + "\r\n")
		b.WriteString("Content-Transfer-Encoding: " + cte + "\r\n\r\n")
		b.WriteString(body + "\r\n") // the CRLF before a delimiter belongs to it
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

func newBoundary() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "pneu-" + hex.EncodeToString(b)
}

func hasQuote(body string) bool {
	for _, l := range plainLines(strings.ReplaceAll(body, "\r", "")) {
		if l.depth > 0 {
			return true
		}
	}
	return false
}

// quoteStyle is Gmail's own blockquote rule, inline because clients drop <style>.
const quoteStyle = `margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex`

// quoteHTML renders the typed body as HTML in the markup Gmail's web UI
// folds behind "...": each top-level quoted run, with the "... wrote:" line
// right above it, becomes div.gmail_quote > div.gmail_attr +
// blockquote.gmail_quote, one nested blockquote per '>' level. Gmail only
// guesses at plain-text quotes, and misses when the original was HTML.
func quoteHTML(body string) string {
	lines := plainLines(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"))
	isAttr := func(i int) bool {
		return i+1 < len(lines) && lines[i].depth == 0 && lines[i+1].depth > 0 &&
			strings.HasSuffix(strings.TrimSpace(lines[i].text), "wrote:")
	}
	var b strings.Builder
	depth, open := 0, false
	for i, l := range lines {
		for ; depth > l.depth; depth-- {
			b.WriteString("</blockquote>")
		}
		if l.depth == 0 && open {
			b.WriteString("</div>\n")
			open = false
		}
		if isAttr(i) {
			b.WriteString(`<div class="gmail_quote"><div dir="ltr" class="gmail_attr">`)
			b.WriteString(htmlLine(l.text))
			b.WriteString("<br></div>")
			open = true
			continue
		}
		if l.depth > 0 && !open {
			b.WriteString(`<div class="gmail_quote">`)
			open = true
		}
		for ; depth < l.depth; depth++ {
			b.WriteString(`<blockquote class="gmail_quote" style="` + quoteStyle + `">`)
		}
		b.WriteString(htmlLine(l.text) + "<br>\n")
	}
	for ; depth > 0; depth-- {
		b.WriteString("</blockquote>")
	}
	if open {
		b.WriteString("</div>\n")
	}
	return b.String()
}

// htmlLine escapes and links one line, keeping runs of spaces.
func htmlLine(s string) string {
	var b strings.Builder
	linkify(&b, s)
	out := strings.ReplaceAll(b.String(), "  ", " &nbsp;")
	if strings.HasPrefix(out, " ") {
		out = "&nbsp;" + out[1:]
	}
	return out
}

// headerAddr formats an address for a header: a bare address without a
// name, an RFC 2047 Q-encoded name when it isn't ASCII, a quoted string when
// it holds specials. (mail.Address.String quotes any name with a space.) The
// addr-spec is always re-serialized: a parsed quoted local part comes back
// unquoted, and written as-is `"x>, evil@a.example, <y"@b.example` would be
// three recipients.
func headerAddr(a *mail.Address) string {
	spec := (&mail.Address{Address: a.Address}).String() // "<spec>", local part quoted when needed
	spec = strings.TrimSuffix(strings.TrimPrefix(spec, "<"), ">")
	if a.Name == "" {
		return spec
	}
	for _, r := range a.Name {
		if r < 0x20 || r >= 0x7f {
			return mime.QEncoding.Encode("utf-8", a.Name) + " <" + spec + ">"
		}
	}
	return displayAddr(a.Name, spec)
}

// parseList parses a typed recipient field; a trailing separator (left by
// autocomplete) is fine, blank is no addresses.
func parseList(s string) ([]*mail.Address, error) {
	s = strings.TrimRight(strings.TrimSpace(s), ",; \t")
	if s == "" {
		return nil, nil
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return nil, errors.New("line break in address")
	}
	return mail.ParseAddressList(s)
}

// encodeSubject strips line breaks and Q-encodes non-ASCII.
func encodeSubject(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\r' || r == '\n' }), " ")
	s = strings.ToValidUTF8(strings.TrimSpace(s), "�")
	return mime.QEncoding.Encode("utf-8", s) // unchanged when printable ASCII
}

// foldHeader writes "Name: value\r\n", folding at spaces to keep lines near
// 78 octets. Every space is a legal fold point in the values built here:
// between address-list items, inside display names and quoted strings,
// between encoded-words and between References ids.
func foldHeader(name, value string) string {
	var b strings.Builder
	b.WriteString(name + ":")
	n := len(name) + 1
	for i, word := range strings.Split(value, " ") {
		if i > 0 && n+1+len(word) > 78 {
			b.WriteString("\r\n")
			n = 0
		}
		b.WriteString(" " + word)
		n += 1 + len(word)
	}
	b.WriteString("\r\n")
	return b.String()
}

// encodeBody normalizes the textarea to CRLF lines. Gmail's API takes 8bit
// (lieer base64url-wraps the whole message for the API's raw field), so text
// goes 7bit or 8bit as typed; only a line past RFC 5322's 998-octet limit
// forces quoted-printable.
func encodeBody(s string) (body, cte string) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	long := false
	for line := range strings.SplitSeq(s, "\n") {
		if len(line) > 998 {
			long = true
			break
		}
	}
	if long {
		var b bytes.Buffer
		qp := quotedprintable.NewWriter(&b) // emits CRLF line breaks
		qp.Write([]byte(s))
		qp.Close()
		return b.String(), "quoted-printable"
	}
	cte = "7bit"
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			cte = "8bit"
			break
		}
	}
	return strings.ReplaceAll(s, "\n", "\r\n"), cte
}

// ---- addresses -------------------------------------------------------------

// addresses handles GET /addresses?q=prefix: up to maxAddresses name-addr
// strings, from the recipients of every account's sent mail.
func (s *Server) addresses(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := []string{}
	seen := map[string]bool{}
	for _, a := range s.Accounts {
		list, err := s.recipients(r.Context(), a)
		if err != nil {
			log.Printf("addresses %s: %v", a.Name, err)
			continue
		}
		for _, e := range list {
			key := strings.ToLower(e.Address)
			if e.Address == "" || seen[key] || !addrMatch(e, q) {
				continue
			}
			seen[key] = true
			out = append(out, displayAddr(e.Name, e.Address))
			if len(out) == maxAddresses {
				break
			}
		}
		if len(out) == maxAddresses {
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("addresses: %v", err)
	}
}

// addrMatch: q prefixes the address, the name, or any word of the name.
func addrMatch(e notmuch.AddressEntry, q string) bool {
	if q == "" || strings.HasPrefix(strings.ToLower(e.Address), q) {
		return true
	}
	name := strings.ToLower(e.Name)
	if strings.HasPrefix(name, q) {
		return true
	}
	for _, w := range strings.Fields(name) {
		if strings.HasPrefix(strings.Trim(w, `"'(`), q) {
			return true
		}
	}
	return false
}

// recipients is the account's sent-mail recipients, cached for addressTTL.
func (s *Server) recipients(ctx context.Context, a notmuch.Account) ([]notmuch.AddressEntry, error) {
	c := &s.outbox
	c.mu.Lock()
	e, ok := c.addrs[a.Name]
	c.mu.Unlock()
	if ok && s.now().Sub(e.at) < addressTTL {
		return e.list, nil
	}
	list, err := a.Recipients(ctx, "tag:sent")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.addrs == nil {
		c.addrs = map[string]addrCache{}
	}
	c.addrs[a.Name] = addrCache{at: s.now(), list: list}
	c.mu.Unlock()
	return list, nil
}
