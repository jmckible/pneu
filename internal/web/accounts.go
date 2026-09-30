package web

// Account onboarding as the app and the bar see it: each account's state
// (docs/onboarding.md), its first pull's progress, the read-only rule while
// that pull runs, and re-auth once a token has died.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
)

// accountView is one account's state, as the SSE `account` event and the
// page's data-accounts carry it: the #accounts strip and the header's
// status line both render from it.
type accountView struct {
	Name     string        `json:"name"`
	State    gmi.State     `json:"state"`
	Pulled   bool          `json:"pulled"`
	Failures int           `json:"failures"` // consecutive
	Error    *string       `json:"error"`    // null after any success
	Authing  bool          `json:"authing"`  // a re-auth waits on the consent screen
	Progress *progressView `json:"progress"` // the first pull's, while pulling
	LastSync *string       `json:"lastSync"` // RFC 3339; null until the first successful sync
	Queued   bool          `json:"queued"`   // a sync was asked for and hasn't started
	Running  bool          `json:"running"`  // a sync or first pull runs (never a push)
}

type progressView struct {
	Phase    gmi.Phase `json:"phase"`
	Done     int       `json:"done"`
	Total    int       `json:"total"`  // 0 while listing
	Listed   int       `json:"listed"` // messages found, once listing ended
	Percent  *int      `json:"percent"`
	Frontier *string   `json:"frontier"` // RFC 3339: mail is complete back to here
}

func viewOf(name string, st gmi.Status) accountView {
	v := accountView{Name: name, State: st.State, Pulled: st.Pulled, Failures: st.Failures, Authing: st.Authing,
		Queued: st.Queued, Running: st.Syncing}
	if !st.LastSync.IsZero() {
		at := st.LastSync.Format(time.RFC3339)
		v.LastSync = &at
	}
	if st.LastErr != nil {
		msg := st.LastErr.Error()
		v.Error = &msg
	}
	if p := st.Progress; p != nil {
		pv := &progressView{Phase: p.Phase, Done: p.Done, Total: p.Total, Listed: p.Listed}
		if p.Total > 0 && (p.Phase == gmi.PhaseContent || p.Phase == gmi.PhaseMetadata) {
			pct := min(100, p.Done*100/p.Total)
			pv.Percent = &pct
		}
		if !p.Frontier.IsZero() {
			f := p.Frontier.Format(time.RFC3339)
			pv.Frontier = &f
		}
		v.Progress = pv
	}
	return v
}

// accountViews is every account's view; nil without a sync engine.
func (s *Server) accountViews() []accountView {
	if s.Syncer == nil {
		return nil
	}
	out := make([]accountView, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		st, err := s.Syncer.Status(a.Name)
		if err != nil {
			log.Printf("status %s: %v", a.Name, err)
			continue
		}
		out = append(out, viewOf(a.Name, st))
	}
	return out
}

// AccountsJSON is the page's initial copy of accountViews, for app.js to
// render the onboarding strip from (then kept current by SSE `account`).
func (s *Server) accountsJSON() template.HTMLAttr {
	b, err := json.Marshal(s.accountViews())
	if err != nil {
		return ""
	}
	return template.HTMLAttr(`data-accounts="` + template.HTMLEscapeString(string(b)) + `"`)
}

// readOnly reports whether account's first pull hasn't finished: lieer has
// never recorded one, or one is running. The second matters because a
// resumed pull records its history id before its closing partial pull
// (gmailieer.py:full_pull). Nothing is written to it meanwhile (no tags, no
// mark-read, no send), so the pull's closing lastmod can't swallow a
// change (pneu-touch has nothing to re-mark) and a resumed pull's label
// refresh can't revert one (docs/onboarding.md).
func (s *Server) readOnly(account string) bool {
	if s.Syncer == nil {
		return false
	}
	st, err := s.Syncer.Status(account)
	return err == nil && st.State != "" && (!st.Pulled || st.State == gmi.StatePulling)
}

func readOnlyMsg(account string) string {
	return fmt.Sprintf("%s is still downloading its mail; changes unlock when that finishes", account)
}

// progressMark is what the status file last showed of an account's pull.
type progressMark struct {
	state   gmi.State
	phase   gmi.Phase
	percent int
}

// accountMarks remembers progressMark per account for AccountChanged. mu
// also spans each read-and-broadcast, so `account` events go out in the
// order their views were read: the engine's and the handlers' calls race,
// and a queued view overtaking its sync's end would leave the page
// checking.
type accountMarks struct {
	mu sync.Mutex
	m  map[string]progressMark
}

// AccountChanged broadcasts the account's view (SSE `account`) and rewrites
// the status file when what the bar shows moved: the state, the phase, or
// a whole percent. Called on every progress report, so the file isn't
// rewritten once a second for an hour.
func (s *Server) AccountChanged(account string) {
	if s.Syncer == nil {
		return
	}
	s.marks.mu.Lock()
	st, err := s.Syncer.Status(account)
	if err != nil {
		s.marks.mu.Unlock()
		return
	}
	v := viewOf(account, st)
	s.Hub.Broadcast("account", v)
	mark := progressMark{state: v.State}
	if v.Progress != nil {
		mark.phase = v.Progress.Phase
		if v.Progress.Percent != nil {
			mark.percent = *v.Progress.Percent
		}
	}
	if s.marks.m == nil {
		s.marks.m = map[string]progressMark{}
	}
	moved := s.marks.m[account] != mark
	s.marks.m[account] = mark
	s.marks.mu.Unlock()
	if moved {
		s.StatusChanged()
	}
}

// Reauther is the part of the engine behind the re-auth endpoints;
// *gmi.Engine satisfies it.
type Reauther interface {
	Reauth(ctx context.Context, account string) (string, error)
	CancelReauth(account string) error
}

// reauth handles POST /accounts/{account}/reauth: start the consent flow
// and answer {url} for the page to open. Refused unless the account's token
// has failed (gmi.ErrNoReauth).
func (s *Server) reauth(w http.ResponseWriter, r *http.Request) {
	// Consent waits on this machine's localhost:8080, which a peer's
	// browser can't reach: `pneu account auth` on the server does it.
	if _, ok := peerOf(r); ok {
		tagFail(w, http.StatusConflict, "reauth-on-server")
		return
	}
	acct, ok := s.account(r)
	if !ok {
		tagFail(w, http.StatusNotFound, "unknown account")
		return
	}
	ra, ok := s.Syncer.(Reauther)
	if !ok {
		tagFail(w, http.StatusServiceUnavailable, "re-auth unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	u, err := ra.Reauth(ctx, acct.Name)
	if errors.Is(err, gmi.ErrAlreadyConnected) {
		// Fixed meanwhile (a terminal `pneu account auth`): nothing to do.
		s.AccountChanged(acct.Name)
		tagJSON(w, http.StatusOK, struct {
			OK        bool `json:"ok"`
			Connected bool `json:"connected"`
		}{true, true})
		return
	}
	if err != nil {
		log.Printf("reauth %s: %v", acct.Name, err)
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, gmi.ErrNoReauth), errors.Is(err, gmi.ErrAuthing):
			status = http.StatusConflict
		case errors.Is(err, gmi.ErrAuthPort), errors.Is(err, gmi.ErrBusy):
			status = http.StatusServiceUnavailable
		}
		tagFail(w, status, err.Error())
		return
	}
	s.AccountChanged(acct.Name)
	tagJSON(w, http.StatusOK, struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
	}{true, u})
}

// reauthCancel handles POST /accounts/{account}/reauth/cancel.
func (s *Server) reauthCancel(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.account(r)
	if !ok {
		tagFail(w, http.StatusNotFound, "unknown account")
		return
	}
	if ra, ok := s.Syncer.(Reauther); ok {
		ra.CancelReauth(acct.Name)
	}
	tagJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

// retryPull handles POST /accounts/{account}/pull: start a first pull now
// rather than after the failure backoff.
func (s *Server) retryPull(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.account(r)
	if !ok {
		tagFail(w, http.StatusNotFound, "unknown account")
		return
	}
	if s.Syncer == nil {
		tagFail(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	st, err := s.Syncer.Status(acct.Name)
	if err != nil || st.State != gmi.StateNeedsPull {
		tagFail(w, http.StatusConflict, "no first pull to start")
		return
	}
	s.Syncer.SyncNow(acct.Name)
	tagJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

// StatusDoc, StatusAccount and ProgressView are the status file's shapes,
// for `pneu account status`.
type (
	StatusDoc     = statusDoc
	StatusAccount = statusAccount
	ProgressView  = progressView
)

// ReadStatus reads a status file written by RunStatus.
func ReadStatus(path string) (StatusDoc, error) {
	var d StatusDoc
	b, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(b, &d)
}

// DescribeProgress is a first pull's progress in words, as the terminal
// shows it; app.js and the bar widget word it the same way.
// commas writes n with thousands separators, as app.js's num does.
func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func DescribeProgress(p *ProgressView) string {
	n := commas
	var out string
	switch p.Phase {
	case gmi.PhaseListing:
		out = "listing messages: " + n(p.Done) + " found"
	case gmi.PhaseRemoving:
		out = "removing deleted messages"
	case gmi.PhaseContent:
		out = "downloading " + n(p.Done) + " of " + n(p.Total)
	case gmi.PhaseMetadata:
		out = "checking labels " + n(p.Done) + " of " + n(p.Total)
	default:
		return "starting"
	}
	if p.Percent != nil {
		out += fmt.Sprintf(" (%d%%)", *p.Percent)
	}
	if p.Frontier != nil {
		if t, err := time.Parse(time.RFC3339, *p.Frontier); err == nil {
			out += ", complete back to " + t.Format("2 Jan 2006")
		}
	}
	return out
}
