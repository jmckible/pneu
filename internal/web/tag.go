package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
)

const (
	maxTagIDs  = 500
	maxIDBytes = 998 // RFC 5322 line limit; nothing longer is a real Message-ID
	maxTagBody = 2 << 20
	undoDepth  = 20
	// maxQuery keeps each id query far below Linux's 128KiB per-argv-string
	// cap (MAX_ARG_STRLEN); longer id lists are looked up in chunks.
	maxQuery = 64 << 10
)

// tagOp is one notmuch tag write: changes applied to exactly ids.
type tagOp struct {
	Account string
	Changes []string
	IDs     []string
}

// tagActions maps an action to its tag changes and their exact inverse.
// star and unstar pick their ids server-side (see tag).
var tagActions = map[string]struct{ do, undo []string }{
	"archive": {[]string{"-inbox"}, []string{"+inbox"}},
	"trash":   {[]string{"+trash", "-inbox"}, []string{"-trash", "+inbox"}}, // lieer: one of inbox/spam/trash
	"read":    {[]string{"-unread"}, []string{"+unread"}},
	"unread":  {[]string{"+unread"}, []string{"-unread"}},
	"star":    {[]string{"+flagged"}, []string{"-flagged"}},
	"unstar":  {[]string{"-flagged"}, []string{"+flagged"}},
}

type tagResponse struct {
	OK      bool     `json:"ok"`
	Action  string   `json:"action"`
	ID      string   `json:"id,omitempty"` // action id, for undo; absent when nothing was written, and for read
	Account string   `json:"account"`
	IDs     []string `json:"ids"` // url.QueryEscape'd, as data-msgids carries them
	Changes []string `json:"changes"`
	Undid   string   `json:"undid,omitempty"` // undo only: the action reverted
}

type tagError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// tag handles POST /tag. Every write names explicit message ids; the only
// queries are the lookups star (newest of the given ids) and unstar (every
// flagged message in the thread) need, and archive/trash of a thread too
// long to send as ids (thread= with no ids).
func (s *Server) tag(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTagBody)
	if err := r.ParseForm(); err != nil {
		tagFail(w, http.StatusBadRequest, "bad form")
		return
	}
	f := r.PostForm
	action := f.Get("action")
	if action == "undo" {
		s.undoTag(w, r, f.Get("id"))
		return
	}
	act, ok := tagActions[action]
	if !ok {
		tagFail(w, http.StatusBadRequest, "unknown action")
		return
	}
	acct, ok := s.byName[f.Get("account")]
	if !ok {
		tagFail(w, http.StatusBadRequest, "unknown account")
		return
	}
	if s.readOnly(acct.Name) {
		tagFail(w, http.StatusConflict, readOnlyMsg(acct.Name))
		return
	}
	thread := f.Get("thread")
	if thread != "" && !threadIDRE.MatchString(thread) {
		tagFail(w, http.StatusBadRequest, "bad thread id")
		return
	}
	var ids []string
	if strings.TrimSpace(f.Get("ids")) == "" && thread != "" && (action == "archive" || action == "trash") {
		// A thread past maxTagIDs can't be sent as ids; the page sends
		// thread= alone and this is the one write that resolves thread:X.
		// Archive and trash act on the whole thread anyway; the cost is that
		// a reply landing after render is swept up too.
		var err error
		ids, err = acct.MessageIDs(r.Context(), "thread:"+thread)
		if err != nil {
			s.tagLookupFail(w, acct, action, err)
			return
		}
		if len(ids) == 0 {
			tagFail(w, http.StatusBadRequest, "no such thread")
			return
		}
	} else {
		var err error
		if ids, err = parseIDs(f.Get("ids")); err != nil {
			tagFail(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	switch action {
	case "star": // Gmail stars one message: the newest the view showed
		newest, err := s.newest(r.Context(), acct, ids)
		if err != nil {
			s.tagLookupFail(w, acct, action, err)
			return
		}
		if newest == "" {
			tagFail(w, http.StatusBadRequest, "no such message")
			return
		}
		ids = []string{newest}
	case "unstar": // every flagged message, including ones the view didn't render
		var err error
		if thread != "" {
			ids, err = acct.MessageIDs(r.Context(), "thread:"+thread+" and tag:flagged")
		} else {
			ids, err = s.lookup(r.Context(), acct, ids, "tag:flagged")
		}
		if err != nil {
			s.tagLookupFail(w, acct, action, err)
			return
		}
		if len(ids) == 0 {
			tagJSON(w, http.StatusOK, tagResponse{OK: true, Action: action, Account: acct.Name, IDs: []string{}, Changes: act.do})
			return
		}
	}

	if !s.applyTag(w, tagOp{acct.Name, act.do, ids}) {
		return
	}
	var id string
	if action != "read" { // read-on-open is what looking did, not an action: z never undoes it
		id = s.undo.push(action, tagOp{acct.Name, act.undo, ids})
	}
	tagJSON(w, http.StatusOK, tagResponse{OK: true, Action: action, ID: id, Account: acct.Name, IDs: escapeIDs(ids), Changes: act.do})
}

// undoTag reverts the action named by id, or the most recent one.
func (s *Server) undoTag(w http.ResponseWriter, r *http.Request, id string) {
	e, ok := s.undo.find(id)
	if !ok {
		tagFail(w, http.StatusConflict, "nothing to undo")
		return
	}
	if s.readOnly(e.inverse.Account) {
		tagFail(w, http.StatusConflict, readOnlyMsg(e.inverse.Account))
		return
	}
	if !s.applyTag(w, e.inverse) {
		return // entry stays: a locked undo can be retried
	}
	s.undo.remove(e.id)
	tagJSON(w, http.StatusOK, tagResponse{OK: true, Action: "undo", ID: e.id, Account: e.inverse.Account,
		IDs: escapeIDs(e.inverse.IDs), Changes: e.inverse.Changes, Undid: e.action})
}

// applyTag writes op in batches of at most maxTagIDs ids and requests a
// push, or answers the failure itself. A failure part-way leaves the earlier
// batches written (and pushed); the action isn't recorded for undo.
func (s *Server) applyTag(w http.ResponseWriter, op tagOp) bool {
	acct := s.byName[op.Account]
	for start := 0; start < len(op.IDs); start += maxTagIDs {
		batch := op.IDs[start:min(start+maxTagIDs, len(op.IDs))]
		if err := s.tagBatch(acct, op.Changes, batch); err != nil {
			s.written(op.Account, op.IDs[:start])
			log.Printf("tag %s %v: %v", op.Account, op.Changes, err)
			if errors.Is(err, notmuch.ErrLocked) {
				w.Header().Set("Retry-After", "2")
				tagFail(w, http.StatusServiceUnavailable, "locked")
				return false
			}
			tagFail(w, http.StatusInternalServerError, "notmuch failed")
			return false
		}
	}
	s.written(op.Account, op.IDs)
	return true
}

func (s *Server) tagBatch(acct notmuch.Account, changes, ids []string) error {
	// Not the request context: a write that has started should finish even if
	// the tab navigates away; the lock wait is bounded either way.
	ctx := context.Background()
	if s.TagTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.TagTimeout)
		defer cancel()
	}
	return acct.Tag(ctx, changes, ids)
}

// written tells the sync engine about a completed write: NoteWrite (so a
// write that raced a sync still gets pushed) and a debounced push. The
// status file's unread count follows, debounced too.
func (s *Server) written(account string, ids []string) {
	if len(ids) == 0 {
		return
	}
	s.statusSoon()
	if s.Syncer == nil {
		return
	}
	s.Syncer.NoteWrite(account, ids)
	if err := s.Syncer.RequestPush(account); err != nil {
		log.Printf("push %s: %v", account, err)
	}
}

func (s *Server) tagLookupFail(w http.ResponseWriter, acct notmuch.Account, action string, err error) {
	log.Printf("tag %s %s lookup: %v", acct.Name, action, err)
	tagFail(w, http.StatusInternalServerError, "notmuch failed")
}

// newest returns the newest of ids that exists in acct ("" if none does).
func (s *Server) newest(ctx context.Context, acct notmuch.Account, ids []string) (string, error) {
	chunks := chunkIDs(ids)
	if len(chunks) > 1 {
		var winners []string
		for _, c := range chunks {
			n, err := s.newest(ctx, acct, c)
			if err != nil {
				return "", err
			}
			if n != "" {
				winners = append(winners, n)
			}
		}
		if len(winners) == 0 {
			return "", nil
		}
		// One winner per chunk; with ids capped at maxIDBytes a chunk holds
		// dozens of ids, so this recursion is a single extra query.
		return s.newest(ctx, acct, winners)
	}
	got, err := acct.MessageIDs(ctx, notmuch.IDsQuery(ids))
	if err != nil || len(got) == 0 {
		return "", err
	}
	return got[0], nil
}

// lookup returns those of ids that also match and (a query), in chunks.
func (s *Server) lookup(ctx context.Context, acct notmuch.Account, ids []string, and string) ([]string, error) {
	var out []string
	for _, c := range chunkIDs(ids) {
		got, err := acct.MessageIDs(ctx, notmuch.IDsQuery(c)+" and "+and)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// chunkIDs splits ids so each chunk's query stays under maxQuery, counting
// the worst case where every byte is a doubled quote.
func chunkIDs(ids []string) [][]string {
	var out [][]string
	start, n := 0, 0
	for i, id := range ids {
		size := 2*len(id) + len(` or id:""`)
		if i > start && n+size > maxQuery {
			out = append(out, ids[start:i])
			start, n = i, 0
		}
		n += size
	}
	if start < len(ids) {
		out = append(out, ids[start:])
	}
	return out
}

// parseIDs decodes the space-separated ids field. Each token is
// percent-decoded with PathUnescape, which inverts url.QueryEscape (ids never
// contain spaces, so '+' never stands for one), url.PathEscape and
// encodeURIComponent alike.
func parseIDs(v string) ([]string, error) {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return nil, errors.New("no ids")
	}
	if len(fields) > maxTagIDs {
		return nil, fmt.Errorf("more than %d ids", maxTagIDs)
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		id, err := url.PathUnescape(f)
		if err != nil || !validID(id) {
			return nil, fmt.Errorf("bad id %q", f)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

func validID(id string) bool {
	if id == "" || len(id) > maxIDBytes {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func escapeIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = url.QueryEscape(id)
	}
	return out
}

func tagJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("tag response: %v", err)
	}
}

func tagFail(w http.ResponseWriter, status int, msg string) {
	tagJSON(w, status, tagError{Error: msg})
}

// ---- undo ------------------------------------------------------------------

type undoEntry struct {
	id      string
	action  string
	inverse tagOp
}

// undoRing holds the last undoDepth actions of this server process. One
// user, one window: the process is the session.
type undoRing struct {
	mu      sync.Mutex
	entries []undoEntry // oldest first
}

func (u *undoRing) push(action string, inverse tagOp) string {
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.entries = append(u.entries, undoEntry{id, action, inverse})
	if len(u.entries) > undoDepth {
		u.entries = slices.Delete(u.entries, 0, len(u.entries)-undoDepth)
	}
	return id
}

// find returns the entry with id, or the newest when id is "".
func (u *undoRing) find(id string) (undoEntry, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if id == "" {
		if len(u.entries) == 0 {
			return undoEntry{}, false
		}
		return u.entries[len(u.entries)-1], true
	}
	for _, e := range u.entries {
		if e.id == id {
			return e, true
		}
	}
	return undoEntry{}, false
}

func (u *undoRing) remove(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.entries = slices.DeleteFunc(u.entries, func(e undoEntry) bool { return e.id == id })
}

// ---- status ----------------------------------------------------------------

type syncStatus struct {
	LastSync *time.Time    `json:"lastSync"` // null until the first successful sync
	LastPush *time.Time    `json:"lastPush"`
	Failures int           `json:"failures"` // consecutive
	Running  bool          `json:"running"`
	LastErr  *string       `json:"lastErr"` // null after any success
	State    gmi.State     `json:"state"`
	Progress *progressView `json:"progress"` // the first pull's, while pulling
}

// status handles GET /status: per-account sync health, {} without a Syncer.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out := map[string]syncStatus{}
	if s.Syncer != nil {
		for _, a := range s.Accounts {
			st, err := s.Syncer.Status(a.Name)
			if err != nil {
				log.Printf("status %s: %v", a.Name, err)
				continue
			}
			v := syncStatus{Failures: st.Failures, Running: st.Running, State: st.State, Progress: viewOf(a.Name, st).Progress}
			if !st.LastSync.IsZero() {
				v.LastSync = &st.LastSync
			}
			if !st.LastPush.IsZero() {
				v.LastPush = &st.LastPush
			}
			if st.LastErr != nil {
				msg := st.LastErr.Error()
				v.LastErr = &msg
			}
			out[a.Name] = v
		}
	}
	tagJSON(w, http.StatusOK, out)
}
