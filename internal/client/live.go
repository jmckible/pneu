package client

// The daemon's live side (docs/client.md, "Events"): what it knows of the
// server from its one upstream stream, its link's state, and the local Hub
// that fans both out to this desk's browsers. Every change to that state
// and its broadcast happen together under live.mu, and each browser's
// /events subscribes and takes its hello under the same lock: as on the
// server, nothing older than a stream's hello is ever queued behind it.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/web"
)

// LinkView is the link as pages see it (hello's `link`, SSE `link`, GET
// /client/link): local enums and local text only. Server is the SSH
// target the user paired with, never the name the server gives itself.
type LinkView struct {
	State    string    `json:"state"`  // starting | up | down
	Since    string    `json:"since"`  // RFC 3339
	Reason   *string   `json:"reason"` // the link.Reason while not up, else null
	Server   string    `json:"server"`
	Revision Revisions `json:"revision"`
}

// Revisions are both builds' vcs.revision, "" when unknown; the server's
// only as 40 hex digits (link.Hello).
type Revisions struct {
	Client string `json:"client"`
	Server string `json:"server"`
}

// Link states in LinkView and status.json's server.link.
const (
	linkStarting = "starting"
	linkUp       = "up"
	linkDown     = "down"
)

// localHello is the first event on a browser's stream: the server's hello
// shape (as last known here) plus the link.
type localHello struct {
	web.HelloEvent
	Link LinkView `json:"link"`
}

// live is the daemon's state. epoch and gen are the last relayed (gen
// moves with every `view` passed on), accounts the last views in hello's
// order with `syncing`/`sync` applied, status the last valid doc and
// statusAt when it arrived here. All of it survives the link going down:
// an open page keeps its content, and a new one renders what was known.
type live struct {
	link     link.State // as last published
	epoch    string
	gen      uint64
	accounts []web.AccountView
	status   *web.StatusDoc
	statusAt time.Time
}

// subscribe registers a browser stream and encodes its hello, under the
// lock every publisher holds across its change and broadcast.
func (d *Daemon) subscribe() (chan []byte, []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.hub.Subscribe()
	return c, web.EncodeEvent("hello", d.helloLocked())
}

func (d *Daemon) helloLocked() localHello {
	accts := d.live.accounts
	if accts == nil {
		accts = []web.AccountView{}
	}
	return localHello{
		HelloEvent: web.HelloEvent{Epoch: d.live.epoch, Gen: d.live.gen, Accounts: accts, Status: d.live.status},
		Link:       d.linkViewLocked(),
	}
}

// linkViewLocked is the published link state as pages see it.
func (d *Daemon) linkViewLocked() LinkView {
	st := d.live.link
	v := LinkView{State: linkState(st.Reason), Since: st.Since.UTC().Format(time.RFC3339), Server: d.server}
	if st.Reason != link.Up {
		r := string(st.Reason)
		v.Reason = &r
	}
	v.Revision.Client = control.Self().Revision
	if h := d.up.Hello(); h != nil {
		v.Revision.Server = h.Revision
	}
	return v
}

func linkState(r link.Reason) string {
	switch r {
	case link.Up:
		return linkUp
	case link.Starting:
		return linkStarting
	}
	return linkDown
}

// LinkChanged is the link's OnChange: SSE `link` to every page, and a
// status.json rewrite. The state is read here, under the lock, rather
// than taken from the call: OnChange runs outside the link's lock, so two
// changes' calls can arrive in either order, and the later one must win.
func (d *Daemon) LinkChanged() {
	d.mu.Lock()
	st := d.up.State()
	if st.Reason == d.live.link.Reason && st.Since.Equal(d.live.link.Since) {
		d.mu.Unlock()
		return
	}
	d.live.link = st
	d.hub.Broadcast("link", d.linkViewLocked())
	d.mu.Unlock()
	d.wakeStatus()
}

// themeChanged is this desk's theme file changing: SSE `theme`. The
// server's never passes (it's its desk's theme, not this one's).
func (d *Daemon) themeChanged() {
	d.mu.Lock()
	d.hub.Broadcast("theme", web.ThemeEvent{At: time.Now().Unix()})
	d.mu.Unlock()
}

// events is the browser's /events: hello from this daemon's state, then
// whatever it publishes.
func (d *Daemon) events(w http.ResponseWriter, r *http.Request) {
	c, hello := d.subscribe()
	d.hub.Serve(w, r, c, hello)
}

// linkJSON is GET /client/link: the link's state, for the error page and
// `pneu update`'s readiness check. Read-only.
func (d *Daemon) linkJSON(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	v := d.linkViewLocked()
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// retry is POST /client/retry: try the link now (a probe when up). It
// takes nothing, neither a query nor a body: the only page-reachable
// action on /client/, and it can't be steered.
func (d *Daemon) retry(w http.ResponseWriter, r *http.Request) {
	// net/http says -1 for a chunked body, so any body is non-zero.
	if r.URL.RawQuery != "" || r.ContentLength != 0 {
		writeJSON(w, http.StatusBadRequest, struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}{false, "retry takes nothing"})
		return
	}
	d.up.Retry()
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
