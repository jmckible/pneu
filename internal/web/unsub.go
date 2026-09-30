package web

// Unsubscribe (docs/actions.md): GET previews the action a message's
// List-Unsubscribe headers lead to and binds it to a token; POST executes
// exactly that stored action, once. Logs carry the account, the method and
// a result category — never the URL, host, address or a raw error.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/unsub"
)

const (
	unsubPreviewWait = 20 * time.Second
	unsubPreviews    = 2 // previews at once, server-wide
)

// unsubAction is what a token binds: the message, the chosen action, and
// for mailto the sender and the outgoing Message-ID fixed at preview.
type unsubAction struct {
	account   string
	msgid     string
	offer     unsub.Offer
	from      mail.Address
	messageID string
}

type unsubResult struct {
	State    string `json:"state"` // ok | failed | refused
	Category string `json:"category"`
	Fallback bool   `json:"fallback"`
}

type unsubPreviewJSON struct {
	Method   unsub.Method `json:"method"`
	Reason   string       `json:"reason,omitempty"`
	Index    *int         `json:"index,omitempty"`
	Token    string       `json:"token,omitempty"`
	URL      string       `json:"url,omitempty"`
	Origin   string       `json:"origin,omitempty"`
	SignedBy string       `json:"signedBy,omitempty"`
	Account  string       `json:"account,omitempty"`
	From     string       `json:"from,omitempty"`
	To       string       `json:"to,omitempty"`
	Subject  string       `json:"subject,omitempty"`
	Body     *string      `json:"body,omitempty"`
	Context  string       `json:"context,omitempty"`
}

func unsubNone(w http.ResponseWriter, status int, reason string) {
	tagJSON(w, status, unsubPreviewJSON{Method: unsub.MethodNone, Reason: reason})
}

// unsubPreview handles GET /unsubscribe/{account}/{msgid}[?after=N]. The
// msgid is one path segment, url.PathEscape'd as for /body.
func (s *Server) unsubPreview(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.account(r)
	id := r.PathValue("msgid")
	if !ok || id == "" || strings.ContainsAny(id, "\r\n\x00") {
		unsubNone(w, http.StatusNotFound, "No such message.")
		return
	}
	after := -1
	if q := r.URL.Query().Get("after"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			unsubNone(w, http.StatusBadRequest, "Bad after.")
			return
		}
		after = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), unsubPreviewWait)
	defer cancel()
	msgs, err := acct.Headers(ctx, notmuch.IDsQuery([]string{id}))
	if err != nil {
		log.Printf("unsubscribe %s preview: notmuch failed", acct.Name)
		unsubNone(w, http.StatusInternalServerError, "notmuch failed.")
		return
	}
	var files []string
	found := false
	for _, m := range msgs {
		if m.ID == id {
			files, found = m.Filename, true
			break
		}
	}
	if !found {
		unsubNone(w, http.StatusNotFound, "No such message.")
		return
	}
	// Reading and verifying is CPU and I/O a page can ask for in a loop:
	// at most unsubPreviews run at once; the rest wait within the deadline.
	select {
	case s.unsubSlots <- struct{}{}:
	case <-ctx.Done():
		unsubNone(w, http.StatusServiceUnavailable, "busy")
		return
	}
	offer := func() unsub.Offer {
		defer func() { <-s.unsubSlots }()
		return unsub.Inspect(ctx, s.unsubDKIM, acct.Maildir, files, after)
	}()
	if offer.Method == unsub.MethodNone {
		unsubNone(w, http.StatusOK, offer.Reason)
		return
	}
	out := unsubPreviewJSON{
		Method:   offer.Method,
		Index:    &offer.Index,
		URL:      offer.URL,
		Origin:   offer.Origin,
		SignedBy: offer.SignedBy,
		Account:  acct.Name,
		Context:  offer.Context,
	}
	act := unsubAction{account: acct.Name, msgid: id, offer: offer}
	switch offer.Method {
	case unsub.MethodOpen:
		// The browser opens it; nothing for the server to execute.
		tagJSON(w, http.StatusOK, out)
		return
	case unsub.MethodMailto:
		if s.readOnly(acct.Name) {
			unsubNone(w, http.StatusOK, readOnlyMsg(acct.Name)+".")
			return
		}
		if s.Syncer == nil {
			unsubNone(w, http.StatusOK, "Sending is unavailable: no sync engine.")
			return
		}
		if s.Sends == nil {
			unsubNone(w, http.StatusOK, "Sending is unavailable: no send log.")
			return
		}
		act.from = s.identity(ctx, acct)
		act.messageID = newMessageID()
		out.Origin = ""
		out.From = displayAddr(act.from.Name, act.from.Address)
		out.To = offer.Mailto.To
		out.Subject = offer.Mailto.Subject
		body := offer.Mailto.Body
		out.Body = &body
	}
	tok, err := s.unsubTokens.Issue(act)
	if err != nil {
		unsubNone(w, http.StatusServiceUnavailable, "Too many unsubscribes pending; try again in a minute.")
		return
	}
	out.Token = tok
	tagJSON(w, http.StatusOK, out)
}

// unsubExecute handles POST /unsubscribe: token only. It runs the stored
// action and never re-reads or re-selects. The work isn't tied to the
// request: a dropped connection still ends in a stored result.
func (s *Server) unsubExecute(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		tagJSON(w, http.StatusBadRequest, map[string]string{"state": "failed", "category": "bad-request"})
		return
	}
	tok := r.PostForm.Get("token")
	act, ok := s.unsubTokens.Take(tok)
	if !ok {
		tagJSON(w, http.StatusConflict, map[string]string{"state": "expired"})
		return
	}
	res := s.runUnsub(context.WithoutCancel(r.Context()), act)
	if act.offer.Method == unsub.MethodMailto && res.State == "ok" && res.Category == unsub.CatOK {
		s.viewChanged(windowFrom(r), nil) // its sent copy is in Sent, in a thread of its own
	}
	s.unsubTokens.Finish(tok, res)
	log.Printf("unsubscribe %s %s %s", act.account, act.offer.Method, res.Category)
	tagJSON(w, http.StatusOK, res)
}

// unsubResultGet handles GET /unsubscribe-result/{token}: a lost POST
// response is asked about here, never retried.
func (s *Server) unsubResultGet(w http.ResponseWriter, r *http.Request) {
	res, done, found := s.unsubTokens.Result(r.PathValue("token"))
	switch {
	case !found:
		tagJSON(w, http.StatusNotFound, map[string]string{"state": "unknown"})
	case !done:
		tagJSON(w, http.StatusOK, map[string]string{"state": "pending"})
	default:
		tagJSON(w, http.StatusOK, res)
	}
}

func (s *Server) runUnsub(ctx context.Context, a unsubAction) unsubResult {
	switch a.offer.Method {
	case unsub.MethodOneClick:
		cat := s.unsubHTTP.OneClick(ctx, a.offer.URL)
		switch {
		case cat == unsub.CatOK:
			return unsubResult{State: "ok", Category: cat}
		case unsub.Refused(cat):
			return unsubResult{State: "refused", Category: cat}
		}
		return unsubResult{State: "failed", Category: cat, Fallback: unsub.Transport(cat) && a.offer.Fallback}
	case unsub.MethodMailto:
		return s.sendUnsub(ctx, a)
	}
	return unsubResult{State: "failed", Category: "unsupported"}
}

// sendUnsub sends the mailto from the account that received the original,
// through the send log like a draft (SendLog), keyed by unsubSendID: a
// second preview of the same message's same mailto is the same send, across
// restarts. Gmail having it (or maybe having it) means it isn't sent again.
func (s *Server) sendUnsub(ctx context.Context, a unsubAction) unsubResult {
	fail := func(cat string) unsubResult { return unsubResult{State: "failed", Category: cat} }
	maybe := unsubResult{State: "maybe-sent", Category: "maybe-sent"}
	acct, ok := s.byName[a.account]
	if !ok {
		return fail("unavailable")
	}
	if s.readOnly(acct.Name) {
		return fail("read-only")
	}
	if s.Syncer == nil || s.Sends == nil {
		return fail("unavailable")
	}
	msg, err := buildUnsubMessage(a.from, a.offer.Mailto, a.messageID, s.now())
	if err != nil {
		return fail("bad-message")
	}
	id := unsubSendID(a)
	rec, fresh, err := s.Sends.reserve(id, acct.Name, id)
	switch {
	case errors.Is(err, errInFlight):
		return fail("duplicate")
	case err != nil:
		log.Printf("unsubscribe %s: send log: %v", acct.Name, err)
		return fail("unavailable")
	case !fresh:
		defer s.Sends.release(id)
		switch rec.Result {
		case sendAccepted:
			return unsubResult{State: "ok", Category: "already-sent"}
		case sendNoCopy:
			return unsubResult{State: "ok", Category: "sent-no-copy"}
		}
		return maybe
	}
	wait := s.SendWait
	if wait <= 0 {
		wait = SendWait
	}
	sctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	res, err := s.Syncer.Send(sctx, acct.Name, bytes.NewReader(msg))
	switch {
	case err == nil:
		s.Sends.record(rec, sendAccepted, "/")
		return unsubResult{State: "ok", Category: unsub.CatOK}
	case errors.Is(err, gmi.ErrBusy):
		s.Sends.record(rec, sendRejected, "")
		return fail(unsub.CatBusy)
	case res.Accepted():
		// Gmail has it; never resent.
		s.Sends.record(rec, sendNoCopy, "")
		s.Syncer.SyncNow(acct.Name)
		return unsubResult{State: "ok", Category: "sent-no-copy"}
	case res.NotSent():
		s.Sends.record(rec, sendRejected, "")
		return fail("send-failed")
	default:
		s.Sends.record(rec, sendUnknown, "")
		s.Syncer.SyncNow(acct.Name)
		return maybe
	}
}

// unsubSendID is a mailto unsubscribe's send log key: the account, the
// message it unsubscribes from, and exactly what the mailto sends.
func unsubSendID(a unsubAction) string {
	h := sha256.New()
	m := a.offer.Mailto
	for _, f := range []string{a.account, a.msgid, m.To, m.Subject, m.Body} {
		fmt.Fprintf(h, "%d:%s\n", len(f), f)
	}
	return "unsub:" + hex.EncodeToString(h.Sum(nil))
}

// unsubSubject is the Subject header for exactly s, the text the
// confirmation showed: printable ASCII words with single spaces go as they
// are; anything else (an "=?" a decoder would take for an encoded-word,
// non-ASCII, runs or edges of whitespace, a word too long to fold) goes
// entirely as B encoded-words, which decode, adjacent whitespace ignored,
// to s. mime.BEncoding.Encode leaves printable ASCII unencoded, so the
// words are built here.
func unsubSubject(s string) string {
	plain := !strings.Contains(s, "=?") && s == strings.Join(strings.Fields(s), " ")
	for i := 0; plain && i < len(s); i++ {
		plain = s[i] >= 0x20 && s[i] < 0x7f
	}
	for w := range strings.SplitSeq(s, " ") {
		plain = plain && len(w) <= 900
	}
	if plain {
		return s
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	// 45 bytes of text is 60 of base64: 72 octets with "=?utf-8?b?" and
	// "?=", within RFC 2047's 75. Chunks end on rune boundaries.
	const chunk = 45
	var words []string
	for len(s) > 0 {
		n := min(chunk, len(s))
		for n < len(s) && n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		words = append(words, "=?utf-8?b?"+base64.StdEncoding.EncodeToString([]byte(s[:n]))+"?=")
		s = s[n:]
	}
	return strings.Join(words, " ")
}

// buildUnsubMessage is the mailto's message: one recipient, taken as an
// address, never through compose's recipient-list parser.
func buildUnsubMessage(from mail.Address, m unsub.Mailto, messageID string, now time.Time) ([]byte, error) {
	if m.To == "" || strings.ContainsAny(m.To, "\r\n\x00,;") {
		return nil, errors.New("bad recipient")
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil || to.Address != m.To {
		return nil, errors.New("bad recipient")
	}
	if !generatedIDRE.MatchString(messageID) {
		return nil, errors.New("bad Message-ID")
	}
	var b bytes.Buffer
	header := func(name, value string) {
		if value != "" {
			b.WriteString(foldHeader(name, value))
		}
	}
	header("From", headerAddr(&from))
	header("To", headerAddr(&mail.Address{Address: to.Address}))
	header("Subject", unsubSubject(m.Subject))
	header("Date", now.Format(time.RFC1123Z))
	header("Message-ID", messageID)
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	body, cte := encodeBody(m.Body)
	header("Content-Transfer-Encoding", cte)
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes(), nil
}
