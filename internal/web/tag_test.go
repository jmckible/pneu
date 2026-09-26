package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/testmail"
)

const (
	kitchen1 = "0100019a7c3e-kitchen-1@delgadobuild.example"
	kitchen2 = "0100019a9d11-kitchen-2@delgadobuild.example"
	ssoPR    = "northwind/app/pull/4821@codehost.example"
)

type fakeSyncer struct {
	mu      sync.Mutex
	pushes  map[string]int
	notes   map[string][]string   // account -> every id passed to NoteWrite
	syncs   int                   // SyncNow calls
	sent    []sentMsg             // every Send, failed ones included
	sendErr error                 // Send's answer
	sendOut string                // Send's Result.Output
	sendCtx context.Context       // the last Send's context
	states  map[string]gmi.Status // Status overrides per account
	reauths []string              // accounts Reauth was called for
	reURL   string                // Reauth's answer
	reErr   error
	cancels []string // CancelReauth calls
}

func (f *fakeSyncer) setStatus(account string, st gmi.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[string]gmi.Status{}
	}
	f.states[account] = st
}

func (f *fakeSyncer) Reauth(ctx context.Context, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reauths = append(f.reauths, account)
	return f.reURL, f.reErr
}

func (f *fakeSyncer) CancelReauth(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, account)
	return nil
}

type sentMsg struct {
	account string
	raw     []byte
}

func (f *fakeSyncer) Send(ctx context.Context, account string, msg io.Reader) (gmi.Result, error) {
	raw, err := io.ReadAll(msg)
	if err != nil {
		return gmi.Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCtx = ctx
	f.sent = append(f.sent, sentMsg{account, raw})
	return gmi.Result{Account: account, Op: gmi.OpSend, Output: f.sendOut, Err: f.sendErr}, f.sendErr
}

func (f *fakeSyncer) NoteWrite(account string, ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notes == nil {
		f.notes = map[string][]string{}
	}
	f.notes[account] = append(f.notes[account], ids...)
}

func (f *fakeSyncer) noted(account string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.notes[account])
}

func (f *fakeSyncer) RequestPush(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes[account]++
	return nil
}

func (f *fakeSyncer) SyncNow(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
	return nil
}

func (f *fakeSyncer) Status(account string) (gmi.Status, error) {
	f.mu.Lock()
	st, ok := f.states[account]
	f.mu.Unlock()
	if ok {
		return st, nil
	}
	if account == "work" {
		return gmi.Status{LastErr: errors.New("gmi exited 1"), Failures: 2, Running: true, Pulled: true, State: gmi.StateReady}, nil
	}
	at := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	return gmi.Status{LastSync: at, LastPush: at.Add(time.Minute), Pulled: true, State: gmi.StateReady}, nil
}

func (f *fakeSyncer) count(account string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushes[account]
}

type tagFixture struct {
	env  testmail.Env
	s    *Server
	sync *fakeSyncer
}

func newTagFixture(t *testing.T) tagFixture {
	t.Helper()
	env := testmail.Setup(t)
	s := serverFor(t, env.Accounts)
	f := &fakeSyncer{pushes: map[string]int{}}
	s.Syncer = f
	return tagFixture{env, s, f}
}

// post sends form exactly as the browser will: urlencoded, same-origin, cookie.
func post(s http.Handler, form url.Values, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/tag", strings.NewReader(form.Encode()))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+host)
	withCookie(r)
	for _, m := range mods {
		m(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// esc renders ids the way data-msgids carries them.
func esc(ids ...string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = url.QueryEscape(id)
	}
	return strings.Join(out, " ")
}

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func tagOK(t *testing.T, s http.Handler, f url.Values) tagResponse {
	t.Helper()
	w := post(s, f)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /tag %v: %d %s", f, w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	var resp tagResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("not ok: %s", w.Body)
	}
	return resp
}

func tagsOf(t *testing.T, a testmail.Account, id string) []string {
	t.Helper()
	var tags []string
	out := a.Notmuch(t, "search", "--format=json", "--output=tags", "--exclude=false", "--", testmail.QuoteID(id))
	if err := json.Unmarshal(out, &tags); err != nil {
		t.Fatal(err)
	}
	return tags
}

func has(t *testing.T, a testmail.Account, id, tag string) bool {
	t.Helper()
	return slices.Contains(tagsOf(t, a, id), tag)
}

func inInbox(t *testing.T, s http.Handler, subject string) bool {
	t.Helper()
	for _, r := range rows(t, s, "/") {
		if strings.HasPrefix(r.Subject, subject) {
			return true
		}
	}
	return false
}

func TestTagArchiveUndo(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	k := find(t, rows(t, f.s, "/"), "Kitchen quote")

	resp := tagOK(t, f.s, form("account", "personal", "ids", esc(k.MsgIDs...), "action", "archive"))
	if resp.Action != "archive" || resp.Account != "personal" || resp.ID == "" ||
		!slices.Equal(resp.IDs, strings.Fields(esc(kitchen1, kitchen2))) || !slices.Equal(resp.Changes, []string{"-inbox"}) {
		t.Fatalf("response %+v", resp)
	}
	if has(t, p, kitchen1, "inbox") || has(t, p, kitchen2, "inbox") {
		t.Fatal("archive left inbox")
	}
	if !has(t, p, kitchen2, "unread") || !has(t, p, kitchen2, "flagged") {
		t.Fatal("archive touched other tags")
	}
	if inInbox(t, f.s, "Kitchen quote") {
		t.Fatal("archived thread still listed")
	}
	if n := f.sync.count("personal"); n != 1 {
		t.Fatalf("pushes %d", n)
	}
	if got := f.sync.noted("personal"); !slices.Equal(got, []string{kitchen1, kitchen2}) {
		t.Fatalf("NoteWrite got %q", got)
	}

	undo := tagOK(t, f.s, form("action", "undo"))
	if undo.Action != "undo" || undo.Undid != "archive" || undo.ID != resp.ID || !slices.Equal(undo.Changes, []string{"+inbox"}) ||
		!slices.Equal(undo.IDs, resp.IDs) || undo.Account != "personal" {
		t.Fatalf("undo %+v", undo)
	}
	if !inInbox(t, f.s, "Kitchen quote") {
		t.Fatal("undo did not restore the thread")
	}
	if n := f.sync.count("personal"); n != 2 {
		t.Fatalf("pushes %d", n)
	}
	// Undo pops: nothing left.
	if w := post(f.s, form("action", "undo")); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("second undo: %d %s", w.Code, w.Body)
	}
	if f.sync.count("personal") != 2 || f.sync.count("work") != 0 {
		t.Fatalf("pushes %v", f.sync.pushes)
	}
}

func TestTagTrashUndo(t *testing.T) {
	f := newTagFixture(t)
	v := f.env.Account(t, "work")
	sso := find(t, rows(t, f.s, "/"), "[northwind/app] SSO")
	if !slices.Contains(sso.MsgIDs, ssoPR) {
		t.Fatalf("sso ids %q", sso.MsgIDs)
	}
	resp := tagOK(t, f.s, form("account", "work", "ids", esc(sso.MsgIDs...), "action", "trash"))
	if !slices.Equal(resp.Changes, []string{"+trash", "-inbox"}) {
		t.Fatalf("changes %v", resp.Changes)
	}
	for _, id := range sso.MsgIDs {
		if tags := tagsOf(t, v, id); !slices.Contains(tags, "trash") || slices.Contains(tags, "inbox") {
			t.Fatalf("%s tags %v", id, tags)
		}
	}
	if inInbox(t, f.s, "[northwind/app] SSO") {
		t.Fatal("trashed thread still listed")
	}
	// Another action in between: undo by id still reaches the trash.
	tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen1), "action", "archive"))
	tagOK(t, f.s, form("action", "undo", "id", resp.ID))
	for _, id := range sso.MsgIDs {
		if tags := tagsOf(t, v, id); slices.Contains(tags, "trash") || !slices.Contains(tags, "inbox") {
			t.Fatalf("%s after undo: %v", id, tags)
		}
	}
	if !inInbox(t, f.s, "[northwind/app] SSO") {
		t.Fatal("undo did not restore the thread")
	}
	if f.sync.count("work") != 2 || f.sync.count("personal") != 1 {
		t.Fatalf("pushes %v", f.sync.pushes)
	}
	if w := post(f.s, form("action", "undo", "id", resp.ID)); w.Code != http.StatusConflict {
		t.Fatalf("undo of popped id: %d", w.Code)
	}
}

func TestTagStar(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	cabin := find(t, rows(t, f.s, "/"), "Cabin weekend")
	var all []string
	if err := json.Unmarshal(p.Notmuch(t, "search", "--format=json", "--output=messages", "--sort=newest-first", "--", "thread:"+cabin.Thread), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("cabin thread %q", all)
	}
	newest := all[0]
	// Oldest first in the request: the server picks by date, not position.
	req := slices.Clone(all)
	slices.Reverse(req)
	resp := tagOK(t, f.s, form("account", "personal", "ids", esc(req...), "action", "star", "thread", cabin.Thread))
	if !slices.Equal(resp.IDs, []string{url.QueryEscape(newest)}) {
		t.Fatalf("starred %v want %s", resp.IDs, newest)
	}
	for _, id := range all {
		if has(t, p, id, "flagged") != (id == newest) {
			t.Fatalf("%s flagged=%v", id, !(id == newest))
		}
	}
	tagOK(t, f.s, form("action", "undo"))
	for _, id := range all {
		if has(t, p, id, "flagged") {
			t.Fatalf("%s still flagged after undo", id)
		}
	}
	if f.sync.count("personal") != 2 {
		t.Fatalf("pushes %v", f.sync.pushes)
	}
}

func TestTagUnstar(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	k := find(t, rows(t, f.s, "/"), "Kitchen quote")
	p.Notmuch(t, "tag", "+flagged", "--", testmail.QuoteID(kitchen1))

	// The view sent only the newest id; unstar still clears the whole thread.
	resp := tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen2), "action", "unstar", "thread", k.Thread))
	if got := slices.Sorted(slices.Values(resp.IDs)); !slices.Equal(got, strings.Fields(esc(kitchen1, kitchen2))) {
		t.Fatalf("unstarred %v", resp.IDs)
	}
	if has(t, p, kitchen1, "flagged") || has(t, p, kitchen2, "flagged") {
		t.Fatal("flag left behind")
	}
	if rs := rows(t, f.s, "/starred"); slices.ContainsFunc(rs, func(r listRow) bool { return strings.HasPrefix(r.Subject, "Kitchen") }) {
		t.Fatal("kitchen still starred")
	}
	// Nothing flagged: ok, no write, no push, nothing to undo.
	none := tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen2), "action", "unstar", "thread", k.Thread))
	if none.ID != "" || len(none.IDs) != 0 {
		t.Fatalf("no-op unstar %+v", none)
	}
	if f.sync.count("personal") != 1 {
		t.Fatalf("pushes %v", f.sync.pushes)
	}
	undo := tagOK(t, f.s, form("action", "undo"))
	if undo.Undid != "unstar" || !has(t, p, kitchen1, "flagged") || !has(t, p, kitchen2, "flagged") {
		t.Fatalf("undo %+v", undo)
	}
	// Without thread=, unstar clears the flagged among the given ids only.
	resp = tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen2), "action", "unstar"))
	if !slices.Equal(resp.IDs, []string{url.QueryEscape(kitchen2)}) || !has(t, p, kitchen1, "flagged") || has(t, p, kitchen2, "flagged") {
		t.Fatalf("id-scoped unstar %+v", resp)
	}
}

var unreadAttrRE = regexp.MustCompile(`<main class="thread" data-account="([^"]*)" data-thread="([^"]*)" data-msgids="([^"]*)" data-unread-ids="([^"]*)"[^>]*>`)

func TestTagReadOnOpen(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	k := find(t, rows(t, f.s, "/"), "Kitchen quote")

	page := getOK(t, f.s, k.URL)
	m := unreadAttrRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no thread main in page")
	}
	if got := html.UnescapeString(m[3]); got != esc(kitchen1, kitchen2) {
		t.Fatalf("data-msgids %q", got)
	}
	unread := html.UnescapeString(m[4])
	if unread != esc(kitchen2) {
		t.Fatalf("data-unread-ids %q", unread)
	}
	// GET did not tag.
	if !has(t, p, kitchen2, "unread") || f.sync.count("personal") != 0 {
		t.Fatal("GET changed tags")
	}

	read := tagOK(t, f.s, form("account", "personal", "ids", unread, "action", "read"))
	if has(t, p, kitchen2, "unread") {
		t.Fatal("read left unread")
	}
	if read.ID != "" {
		t.Fatalf("read-on-open got an undo id: %+v", read)
	}
	if m := unreadAttrRE.FindStringSubmatch(getOK(t, f.s, k.URL)); m == nil || m[4] != "" {
		t.Fatalf("after read: %q", m)
	}
	if r := find(t, rows(t, f.s, "/"), "Kitchen quote"); strings.Contains(r.Class, "unread") {
		t.Fatalf("row class %q", r.Class)
	}

	tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen1, kitchen2), "action", "unread"))
	if !has(t, p, kitchen1, "unread") || !has(t, p, kitchen2, "unread") {
		t.Fatal("unread not applied")
	}
	u := tagOK(t, f.s, form("action", "undo"))
	if u.Undid != "unread" || has(t, p, kitchen1, "unread") || has(t, p, kitchen2, "unread") {
		t.Fatalf("undo unread %+v", u)
	}
	// read-on-open never went on the ring: nothing further to undo, and it
	// didn't push an older action out of the ring either.
	if w := post(f.s, form("action", "undo")); w.Code != http.StatusConflict {
		t.Fatalf("undo past read: %d %s", w.Code, w.Body)
	}
	if has(t, p, kitchen2, "unread") {
		t.Fatal("undo reverted read-on-open")
	}
	if f.sync.count("personal") != 3 {
		t.Fatalf("pushes %v", f.sync.pushes)
	}
	for range undoDepth + 5 {
		tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen2), "action", "read"))
	}
	if len(f.s.undo.entries) != 0 {
		t.Fatalf("reads filled the ring: %d", len(f.s.undo.entries))
	}
}

// holdLock keeps a `notmuch tag --batch` open (it holds the Xapian write lock
// while its stdin is open), as notmuch's TestTagLocked does.
func holdLock(t *testing.T, a testmail.Account) func() {
	t.Helper()
	cmd := exec.Command("notmuch", "tag", "--batch")
	cmd.Env = a.Environ()
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(w, "+holder -- "+testmail.QuoteID(kitchen1))
	probe := notmuch.Account{Name: a.Name, ConfigPath: a.NotmuchConfig}
	for i := 0; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		err := probe.Tag(ctx, []string{"+probe"}, []string{"none@example.com"})
		cancel()
		if errors.Is(err, notmuch.ErrLocked) {
			break
		}
		if i > 50 {
			w.Close()
			cmd.Wait()
			t.Fatal("holder never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {
		w.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("holder: %v", err)
		}
	}
}

func TestTagLocked(t *testing.T) {
	f := newTagFixture(t)
	f.s.TagTimeout = 300 * time.Millisecond
	p := f.env.Account(t, "personal")
	archived := tagOK(t, f.s, form("account", "work", "ids", esc(ssoPR), "action", "archive"))

	release := holdLock(t, p)
	w := post(f.s, form("account", "personal", "ids", esc(kitchen1, kitchen2), "action", "archive"))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "2" ||
		strings.TrimSpace(w.Body.String()) != `{"ok":false,"error":"locked"}` {
		release()
		t.Fatalf("locked: %d %v %s", w.Code, w.Header(), w.Body)
	}
	release()
	if !has(t, p, kitchen1, "inbox") {
		t.Fatal("locked write applied")
	}
	if f.sync.count("personal") != 0 {
		t.Fatalf("push after failed write: %v", f.sync.pushes)
	}
	// The failed action left no undo entry: undo still reaches the work archive.
	if u := tagOK(t, f.s, form("action", "undo")); u.ID != archived.ID {
		t.Fatalf("undo hit %+v", u)
	}

	// A locked undo keeps its entry for a retry.
	v := f.env.Account(t, "work")
	archived = tagOK(t, f.s, form("account", "work", "ids", esc(ssoPR), "action", "archive"))
	release = holdLock(t, v)
	w = post(f.s, form("action", "undo"))
	release()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("locked undo: %d %s", w.Code, w.Body)
	}
	if u := tagOK(t, f.s, form("action", "undo")); u.ID != archived.ID || !has(t, v, ssoPR, "inbox") {
		t.Fatalf("retried undo %+v", u)
	}
}

func TestTagAuth(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	ok := form("account", "personal", "ids", esc(kitchen1), "action", "archive")
	for name, mod := range map[string]func(*http.Request){
		"no origin":    func(r *http.Request) { r.Header.Del("Origin") },
		"cross origin": func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		"no cookie":    func(r *http.Request) { r.Header.Del("Cookie") },
	} {
		if w := post(f.s, ok, mod); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if !has(t, p, kitchen1, "inbox") || f.sync.count("personal") != 0 {
		t.Fatal("refused request wrote")
	}
}

func TestTagBadRequests(t *testing.T) {
	f := newTagFixture(t)
	many := make([]string, maxTagIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("m%d@example.com", i)
	}
	cases := map[string]url.Values{
		"unknown account": form("account", "nobody", "ids", esc(kitchen1), "action", "archive"),
		"no account":      form("ids", esc(kitchen1), "action", "archive"),
		"unknown action":  form("account", "personal", "ids", esc(kitchen1), "action", "delete"),
		"no action":       form("account", "personal", "ids", esc(kitchen1)),
		"no ids":          form("account", "personal", "ids", "  ", "action", "archive"),
		"newline id":      form("account", "personal", "ids", esc("a\nb@x"), "action", "archive"),
		"nul id":          form("account", "personal", "ids", "a%00b@x", "action", "archive"),
		"space id":        form("account", "personal", "ids", "a%20b@x", "action", "archive"),
		"bad escape":      form("account", "personal", "ids", "a%zzb@x", "action", "archive"),
		"long id":         form("account", "personal", "ids", esc(strings.Repeat("x", maxIDBytes-1)+"@y"), "action", "archive"),
		"too many ids":    form("account", "personal", "ids", esc(many...), "action", "archive"),
		"bad thread":      form("account", "personal", "ids", esc(kitchen1), "action", "unstar", "thread", "x or tag:inbox"),
		"star missing":    form("account", "personal", "ids", esc("nope@example.com"), "action", "star"),
		"star elsewhere":  form("account", "work", "ids", esc(kitchen1), "action", "star"),
	}
	for name, v := range cases {
		w := post(f.s, v)
		if w.Code != http.StatusBadRequest || !strings.HasPrefix(w.Body.String(), `{"ok":false,"error":`) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if len(f.sync.pushes) != 0 {
		t.Fatalf("pushes %v", f.sync.pushes)
	}
	// A bare '+' is a literal plus (PathUnescape), never a space.
	if r := tagOK(t, f.s, form("account", "personal", "ids", "a+b@x", "action", "archive")); !slices.Equal(r.IDs, []string{"a%2Bb%40x"}) {
		t.Fatalf("plus id %v", r.IDs)
	}
	// The limits themselves are allowed.
	tagOK(t, f.s, form("account", "personal", "ids", esc(many[:maxTagIDs]...), "action", "archive"))
	tagOK(t, f.s, form("account", "personal", "ids", esc(strings.Repeat("x", maxIDBytes-2)+"@y"), "action", "archive"))
}

func TestChunkIDs(t *testing.T) {
	ids := make([]string, 300)
	for i := range ids {
		ids[i] = strings.Repeat("x", 990) + fmt.Sprint(i)
	}
	chunks := chunkIDs(ids)
	if len(chunks) < 2 || !slices.Equal(slices.Concat(chunks...), ids) {
		t.Fatalf("%d chunks", len(chunks))
	}
	for _, c := range chunks {
		if q := notmuch.IDsQuery(c); len(q) > maxQuery {
			t.Fatalf("chunk query %d bytes", len(q))
		}
	}
}

func TestStatus(t *testing.T) {
	f := newTagFixture(t)
	w := do(f.s, "GET", "/status", withCookie)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	want := `{"personal":{"lastSync":"2026-09-23T10:00:00Z","lastPush":"2026-09-23T10:01:00Z","failures":0,"running":false,"lastErr":null,"state":"ready","progress":null},` +
		`"work":{"lastSync":null,"lastPush":null,"failures":2,"running":true,"lastErr":"gmi exited 1","state":"ready","progress":null}}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("status\n got %s\nwant %s", got, want)
	}
	if w := do(f.s, "GET", "/status", nil); w.Code != http.StatusForbidden {
		t.Fatalf("no cookie: %d", w.Code)
	}
	f.s.Syncer = nil
	if w := do(f.s, "GET", "/status", withCookie); strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("nil syncer: %s", w.Body)
	}
}

// bigThread adds a synthetic n-message thread to a's maildir, all in the
// inbox, and returns its thread id.
func bigThread(t *testing.T, a testmail.Account, n int) (string, []string) {
	t.Helper()
	dir := filepath.Join(a.Root, "gmail", "mail", "cur")
	var ids []string
	for i := range n {
		id := fmt.Sprintf("big-%03d@thread.example", i)
		ids = append(ids, id)
		hdr := ""
		if i > 0 {
			hdr = "In-Reply-To: <big-000@thread.example>\nReferences: <big-000@thread.example>\n"
		}
		msg := fmt.Sprintf("From: a@thread.example\nTo: robin@hale.example\nSubject: big\nDate: Mon, 1 Jun 2026 10:%02d:%02d +0000\nMessage-ID: <%s>\n%s\nbody %d\n", i/60, i%60, id, hdr, i)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("big%04d:2,S", i)), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a.Notmuch(t, "new", "--quiet")
	a.Notmuch(t, "tag", "+inbox", "--", "subject:big")
	var threads []string
	if err := json.Unmarshal(a.Notmuch(t, "search", "--format=json", "--output=threads", "--", "subject:big"), &threads); err != nil || len(threads) != 1 {
		t.Fatalf("threads %q %v", threads, err)
	}
	return strings.TrimPrefix(threads[0], "thread:"), ids
}

// countingNotmuch wraps the notmuch binary and counts `tag` invocations.
func countingNotmuch(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath(notmuch.Binary)
	if err != nil {
		t.Skip("no notmuch")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := filepath.Join(dir, "notmuch")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >>"+log+"\nexec "+real+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	saved := notmuch.Binary
	notmuch.Binary = script
	t.Cleanup(func() { notmuch.Binary = saved })
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "tag\n")
	}
}

func TestTagThreadTooLongForIDs(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	thread, ids := bigThread(t, p, 600)
	count := countingNotmuch(t)

	// Explicit ids stay capped.
	if w := post(f.s, form("account", "personal", "ids", esc(ids...), "action", "archive")); w.Code != http.StatusBadRequest {
		t.Fatalf("600 explicit ids: %d", w.Code)
	}
	// thread= alone is only for archive and trash.
	for _, action := range []string{"star", "unstar", "read", "unread"} {
		if w := post(f.s, form("account", "personal", "thread", thread, "action", action)); w.Code != http.StatusBadRequest {
			t.Fatalf("%s by thread: %d %s", action, w.Code, w.Body)
		}
	}
	if w := post(f.s, form("account", "personal", "thread", "00000000deadbeef", "action", "archive")); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown thread: %d", w.Code)
	}

	before := count()
	resp := tagOK(t, f.s, form("account", "personal", "thread", thread, "action", "archive"))
	if len(resp.IDs) != 600 || resp.ID == "" {
		t.Fatalf("archived %d ids, undo id %q", len(resp.IDs), resp.ID)
	}
	if n := count() - before; n != 2 {
		t.Errorf("%d tag invocations for 600 ids, want 2 batches", n)
	}
	if n := p.Notmuch(t, "count", "--", "subject:big and tag:inbox"); strings.TrimSpace(string(n)) != "0" {
		t.Fatalf("%s still in inbox", n)
	}
	if got := f.sync.noted("personal"); len(got) != 600 {
		t.Fatalf("NoteWrite saw %d ids", len(got))
	}
	tagOK(t, f.s, form("action", "undo"))
	if n := p.Notmuch(t, "count", "--", "subject:big and tag:inbox"); strings.TrimSpace(string(n)) != "600" {
		t.Fatalf("undo restored %s", n)
	}
	tagOK(t, f.s, form("account", "personal", "thread", thread, "action", "trash"))
	if n := p.Notmuch(t, "count", "--exclude=false", "--", "subject:big and tag:trash and not tag:inbox"); strings.TrimSpace(string(n)) != "600" {
		t.Fatalf("trash reached %s", n)
	}
}

// A Starred row's matched ids are only the flagged messages; archive and
// trash must still act on the whole thread (data-thread-ids).
func TestListArchiveActsOnWholeThread(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	cabin2 := "CAJm4kR9-cabin-2@mail.gmail.com"
	all := []string{cabin1Tag, cabin2, cabin3Tag}
	for _, id := range all {
		p.Notmuch(t, "tag", "+inbox", "--", testmail.QuoteID(id))
	}
	p.Notmuch(t, "tag", "+flagged", "--", testmail.QuoteID(cabin1Tag))

	r := find(t, rows(t, f.s, "/starred"), "Cabin weekend")
	if !slices.Equal(r.MsgIDs, []string{cabin1Tag}) {
		t.Fatalf("matched ids %q", r.MsgIDs)
	}
	if got := slices.Sorted(slices.Values(r.ThreadIDs)); !slices.Equal(got, slices.Sorted(slices.Values(all))) {
		t.Fatalf("thread ids %q", r.ThreadIDs)
	}
	tagOK(t, f.s, form("account", "personal", "ids", esc(r.ThreadIDs...), "action", "archive", "thread", r.Thread))
	for _, id := range all {
		if has(t, p, id, "inbox") {
			t.Errorf("%s still in inbox", id)
		}
	}
	if !has(t, p, cabin1Tag, "flagged") {
		t.Error("archive unstarred")
	}

	// A trashed message in the thread is left out of the row's ids, so a
	// whole-thread trash can't name it (or a spam one) again.
	p.Notmuch(t, "tag", "+trash", "--", testmail.QuoteID(cabin2))
	r = find(t, rows(t, f.s, "/starred"), "Cabin weekend")
	if slices.Contains(r.ThreadIDs, cabin2) || len(r.ThreadIDs) != 2 {
		t.Fatalf("thread ids with a trashed message: %q", r.ThreadIDs)
	}
}

const (
	cabin1Tag = "CAH7x2Lq-cabin-1@mail.ortega.example"
	cabin3Tag = "b7e1c0d2-44aa-4c1e-9d3e-cabin3@fastmail.example"
)
