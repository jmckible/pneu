package web

import (
	"cmp"
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
)

// The status file is the bar widget's whole view of pneu (shell/BarWidget.qml).
// It carries counts, first names, and sync health, never the token, the
// nonce, or anything from a message body.
const (
	// StatusDebounce collapses a burst of tag writes (a run of `e` presses)
	// into one rewrite.
	StatusDebounce = 500 * time.Millisecond
	// StatusHeartbeat rewrites the file with nothing else happening, so
	// `updated` going stale means the server is gone or wedged, whatever
	// sync is doing (an unpulled account never ends a sync). The widget
	// calls it stale at 20 minutes.
	StatusHeartbeat = 5 * time.Minute
	// statusReadTimeout bounds each notmuch read behind one rewrite.
	statusReadTimeout = 10 * time.Second
)

// unreadQuery is what the bar counts: unread threads still in the inbox.
const unreadQuery = "tag:unread and tag:inbox"

// statusSenders is how many sender names the tooltip gets.
const statusSenders = 5

// StatusPath is $XDG_STATE_HOME/pneu/status.json, default
// ~/.local/state/pneu/status.json.
func StatusPath() (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "status.json"), err
}

type statusDoc struct {
	Version  int             `json:"version"`
	Updated  string          `json:"updated"`
	Running  bool            `json:"running"`
	Unread   int             `json:"unread"`
	Senders  []string        `json:"senders"`
	Accounts []statusAccount `json:"accounts"`
}

type statusAccount struct {
	Name     string  `json:"name"`
	Unread   int     `json:"unread"`
	Pulled   bool    `json:"pulled"`
	LastSync *string `json:"lastSync"` // null until the first successful sync
	Failures int     `json:"failures"` // consecutive
	Error    *string `json:"error"`    // null after any success
	// State is the account's onboarding state (gmi.State); Progress its
	// first pull's, while State is "pulling".
	State    gmi.State     `json:"state"`
	Progress *progressView `json:"progress"`
}

// StatusChanged asks for a rewrite now: a sync or push ended.
func (s *Server) StatusChanged() { signal(s.statusNow) }

// statusSoon asks for a debounced rewrite: tags changed.
func (s *Server) statusSoon() { signal(s.statusTags) }

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// RunStatus writes the status file at path now, again on every
// StatusChanged, StatusDebounce after the last tag write, and every
// StatusHeartbeat; when ctx ends it writes it once more with running false
// and returns. Every write happens here, so they never interleave.
func (s *Server) RunStatus(ctx context.Context, path string) {
	s.writeStatus(path, true)
	debounce := time.NewTimer(StatusDebounce)
	debounce.Stop()
	heartbeat := time.NewTicker(StatusHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			debounce.Stop()
			s.writeStatus(path, false)
			return
		case <-s.statusTags:
			debounce.Reset(StatusDebounce)
			continue
		case <-s.statusNow:
		case <-debounce.C:
		case <-heartbeat.C:
		}
		debounce.Stop() // this write covers a pending tag rewrite too
		s.writeStatus(path, true)
	}
}

func (s *Server) writeStatus(path string, running bool) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // read by QML, never by a browser
	err := enc.Encode(s.statusSnapshot(running))
	if err == nil {
		err = writePrivate(path, b.String())
	}
	if err != nil {
		log.Printf("status file: %v", err)
	}
}

// statusSnapshot reads every account's database (readers never wait on
// lieer's writer) and the engine's sync health. An account whose database
// can't be read counts nothing rather than failing the whole file.
func (s *Server) statusSnapshot(running bool) statusDoc {
	doc := statusDoc{
		Version:  1,
		Updated:  s.now().Format(time.RFC3339),
		Running:  running,
		Senders:  []string{},
		Accounts: []statusAccount{},
	}
	var threads []notmuch.ThreadSummary
	for _, a := range s.Accounts {
		sa := statusAccount{Name: a.Name}
		ctx, cancel := context.WithTimeout(context.Background(), statusReadTimeout)
		if n, err := a.Count(ctx, unreadQuery, true); err != nil {
			log.Printf("status file: %v", err)
		} else {
			sa.Unread = n
		}
		if ts, err := a.Search(ctx, unreadQuery, notmuch.SearchOpts{Limit: statusSenders}); err != nil {
			log.Printf("status file: %v", err)
		} else {
			threads = append(threads, ts...)
		}
		cancel()
		if s.Syncer != nil {
			if st, err := s.Syncer.Status(a.Name); err == nil {
				v := viewOf(a.Name, st)
				sa.Pulled, sa.State, sa.Progress = st.Pulled, st.State, v.Progress
				sa.Failures = st.Failures
				if !st.LastSync.IsZero() {
					at := st.LastSync.Format(time.RFC3339)
					sa.LastSync = &at
				}
				if st.LastErr != nil {
					msg := st.LastErr.Error()
					sa.Error = &msg
				}
			}
		}
		doc.Unread += sa.Unread
		doc.Accounts = append(doc.Accounts, sa)
	}
	slices.SortStableFunc(threads, func(a, b notmuch.ThreadSummary) int { return cmp.Compare(b.Timestamp, a.Timestamp) })
	for _, t := range threads {
		name := firstName(t.Authors)
		if name != "" && !slices.Contains(doc.Senders, name) {
			doc.Senders = append(doc.Senders, name)
		}
		if len(doc.Senders) == statusSenders {
			break
		}
	}
	return doc
}

// firstName is the bar's name for a thread: the first matched author's
// first word, or an address's local part. notmuch lists authors
// "Matched One, Matched Two| Unmatched".
func firstName(authors string) string {
	name, _, _ := strings.Cut(authors, "|")
	name, _, _ = strings.Cut(name, ",")
	name = strings.Trim(strings.TrimSpace(name), `"`)
	name, _, _ = strings.Cut(name, "@")
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return ""
}
