package client

// The error page (docs/client.md, "Unreachable, mismatch, unknown
// outcomes"): what a page navigation gets while the link isn't up. It's
// this binary's own template and /client/static/ assets, embedded, so it
// renders with the server unreachable, and it says only what the daemon
// knows from local codes: the reason, the SSH target the user paired
// with, the bar-menu action that fixes it and the command where one does.
// Its script reloads it when the local /events says the link is up.

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"path"
	"strings"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/web"
)

//go:embed page
var pageFS embed.FS

var (
	errorTmpl = template.Must(template.ParseFS(pageFS, "page/error.html"))
	sendTmpl  = template.Must(template.ParseFS(pageFS, "page/send.html"))
)

// clientStatic is /client/static/: the error pages' two files, nothing
// else (no listing, no ranges, no validators).
var clientStatic = map[string]string{
	"error.css": "text/css; charset=utf-8",
	"error.js":  "text/javascript; charset=utf-8",
}

func (d *Daemon) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/client/static/")
	ctype, ok := clientStatic[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := pageFS.ReadFile(path.Join("page", name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Write(b)
}

// step is one thing to do: a bar-menu item (step 6 builds the menu) or a
// command for a terminal.
type step struct {
	Menu, Command, Note string
}

type errorPage struct {
	Reason, Title, Says, Server, Since string
	Steps                              []step
	Retrying                           bool
	Mark                               template.HTML // pneu's drawn mark (web.Mark)
}

// explain is the page for a reason, from local codes and the SSH target
// alone: no server text.
func explain(st link.State, server string) errorPage {
	p := errorPage{Reason: string(st.Reason), Server: server, Since: st.Since.Local().Format("15:04 Jan 2"),
		Retrying: st.Reason != link.PinMismatch, Mark: web.Mark()}
	fix := step{Menu: "Fix with agent"}
	repair := []step{{Command: "pneu client unpair"}, {Command: "pneu client pair " + config.ShellWord(server), Note: "then add this machine again on " + server}}
	switch st.Reason {
	case link.Starting:
		// Just started, just woken, or reconnecting after a dropped
		// connection: nothing to fix, only to wait for.
		p.Title, p.Says = "Connecting to "+server+"…", "pneu is opening its link to "+server+"."
	case link.TailscaleDown:
		p.Title = "Tailscale is off on this machine"
		p.Says = "pneu reaches " + server + " over the tailnet, and Tailscale isn't running here."
		p.Steps = []step{{Command: "sudo tailscale up"}, fix}
	case link.NodeOffline:
		p.Title = server + " is offline"
		p.Says = "The tailnet doesn't see " + server + " online. Wake it or start it, and this page comes back on its own."
		p.Steps = []step{fix}
	case link.NodeMismatch:
		p.Title = server + "'s address belongs to another machine"
		p.Says = "The tailnet says the address it gives for " + server + " is another node's, so pneu isn't connecting."
		p.Steps = []step{fix}
	case link.PinMismatch:
		p.Title = server + "'s identity changed; not connecting"
		p.Says = server + " answered with a key that isn't the one paired. That's a reinstalled " + server +
			", or something else answering in its place. pneu won't connect until it's paired again."
		p.Steps = append([]step{{Menu: "Fix with agent", Note: "walks through pairing again"}}, repair...)
	case link.NotPaired:
		p.Title = server + " doesn't know this machine"
		p.Says = server + " refused this machine's key: it isn't paired there (any more)."
		p.Steps = append(repair, fix)
	case link.Protocol:
		p.Title = "This machine and " + server + " run different versions of pneu"
		p.Says = "Their link protocols differ, so pneu won't use the link until they match."
		p.Steps = []step{{Menu: "Update pneu"}, {Menu: "Update " + server}}
	default: // refused, and anything new
		p.Title = server + " is up, but pneu isn't answering"
		p.Says = "pneu on " + server + " isn't running, or isn't letting other machines in."
		p.Steps = []step{{Command: config.SSHHint(server, false) + " systemctl --user status pneu"}, fix}
	}
	return p
}

// errorPage answers a page navigation with the error page, under the
// HTML class rt allows (app, or compose on the form's routes), saying in
// LinkHeader whether the request was sent (a page load's failure on a
// connection it had is unknown, though a GET changes nothing).
func (d *Daemon) errorPage(w http.ResponseWriter, rt *web.Route, st link.State, outcome string) {
	var b bytes.Buffer
	if err := errorTmpl.Execute(&b, explain(st, d.server)); err != nil {
		http.Error(w, "the error page failed", http.StatusInternalServerError)
		return
	}
	p, _ := web.HTMLPolicy(rt)
	a := web.Admitted{Policy: p, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}}
	if err := a.Install(w, rt); err != nil {
		http.Error(w, "the error page failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set(LinkHeader, outcome)
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write(b.Bytes())
}

// pageNavigation: r loads a page on a route that answers HTML, which the
// error page can stand in for.
func pageNavigation(rt *web.Route, r *http.Request) bool {
	_, html := web.HTMLPolicy(rt)
	return html && r.Method == http.MethodGet && web.Navigation(r)
}

// sendPage is what a compose form's POST /send gets when the link failed
// it: the draft is in this browser's storage (compose.js keeps it, id and
// all, until the send's own redirect), so the page says what happened and
// takes the user back to it. It never reloads itself: that would post the
// form again.
type sendPage struct {
	Outcome, Title string
	Says           []string
}

func explainSend(outcome, server string) sendPage {
	if outcome == NotSent {
		return sendPage{Outcome: outcome, Title: "Not sent: can't reach " + server, Says: []string{
			"Nothing left this machine.",
			"Your draft is kept in this browser. Go back to it and send again once " + server + " is back; the status line says when.",
		}}
	}
	return sendPage{Outcome: outcome, Title: server + " didn't answer", Says: []string{
		"The message may have been sent: check Sent before writing it again.",
		"Your draft is kept in this browser. Sending it again from the draft won't send it twice: " + server + " answers a repeat of the same draft from its record.",
	}}
}

// sendNavigation: r is the compose form's own POST /send, a top-level
// navigation, which a local page can answer under the compose class.
func sendNavigation(rt *web.Route, r *http.Request) bool {
	return rt.Method == http.MethodPost && rt.Pattern == "/send" && r.Method == http.MethodPost && web.Navigation(r)
}

// sendFailed answers a form's POST /send the link failed: 503 not-sent or
// 504 unknown, under the route's HTML class (compose).
func (d *Daemon) sendFailed(w http.ResponseWriter, rt *web.Route, outcome string) {
	var b bytes.Buffer
	if err := sendTmpl.Execute(&b, explainSend(outcome, d.server)); err != nil {
		http.Error(w, "the error page failed", http.StatusInternalServerError)
		return
	}
	p, _ := web.HTMLPolicy(rt)
	a := web.Admitted{Policy: p, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}}
	if err := a.Install(w, rt); err != nil {
		http.Error(w, "the error page failed", http.StatusInternalServerError)
		return
	}
	status := http.StatusGatewayTimeout
	if outcome == NotSent {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set(LinkHeader, outcome)
	w.WriteHeader(status)
	w.Write(b.Bytes())
}
