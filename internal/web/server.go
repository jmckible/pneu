// Package web is pneu's HTTP front: auth, routes, templates, SSE.
package web

import (
	"context"
	"embed"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/unsub"
	assets "github.com/jmckible/pneu/web"
)

//go:embed templates
var templateFS embed.FS

// Syncer is what the tag handler pokes after a change, /status reads and
// /send sends through; *gmi.Engine satisfies it.
type Syncer interface {
	RequestPush(account string) error
	NoteWrite(account string, changes, ids []string)
	SyncNow(account string) error
	Status(account string) (gmi.Status, error)
	Send(ctx context.Context, account string, rfc822 io.Reader) (gmi.Result, error)
}

type Server struct {
	Accounts  []notmuch.Account // display/merge order
	Auth      *Auth
	Hub       *Hub
	Syncer    Syncer // nil in tests that don't care
	PerPage   int    // threads per list page; each account is asked for enough to fill it
	ThemePath string // override for tests; default ThemePath()
	// TagTimeout bounds a tag write's wait on the Xapian lock; 0 means
	// notmuch.TagTimeout. Past it POST /tag answers 503.
	TagTimeout time.Duration
	// SendWait bounds a send's wait for the account lock; 0 means SendWait.
	SendWait time.Duration
	// SyncInterval is the engine's sync period, for the status line's
	// staleness threshold; 0 leaves the page its default.
	SyncInterval time.Duration

	byName map[string]notmuch.Account
	order  map[string]int
	pages  map[string]*template.Template // "base", "list", "thread"
	now    func() time.Time
	undo   undoRing
	// userNames caches each account's user.name for the list's "me".
	userNames sync.Map
	// totals caches each account's thread count for a list query at the
	// database revision it was counted at (totalKey -> totalVal).
	totals  sync.Map
	outbox  composeState
	handler http.Handler
	// RunStatus's wakeups, cap 1 so pending requests collapse.
	statusNow, statusTags chan struct{}
	marks                 accountMarks
	// Unsubscribe: preview tokens and the network side. Tests replace
	// unsubDKIM's resolver and unsubHTTP's resolver, dialer and policy.
	unsubTokens unsub.Store[unsubAction, unsubResult]
	unsubDKIM   *unsub.Verifier
	unsubHTTP   *unsub.Client
	// unsubSlots bounds concurrent previews (DKIM is CPU work) server-wide.
	unsubSlots chan struct{}
	// view is the view generation (view.go).
	view viewGen
	// onLabel runs right after a page reads its label; tests only.
	onLabel func()
}

// New wires routes. host is the exact Host header to accept
// ("pneu.localhost:7317"); token is the install token.
func New(accounts []notmuch.Account, host, token string) (*Server, error) {
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets.Static, "static")
	if err != nil {
		return nil, err
	}
	s := &Server{
		Accounts: accounts,
		Auth:     NewAuth(host, token),
		Hub:      NewHub(),
		PerPage:  PerPage,
		byName:   map[string]notmuch.Account{},
		order:    map[string]int{},
		pages:    pages,
		now:      time.Now,

		statusNow:  make(chan struct{}, 1),
		statusTags: make(chan struct{}, 1),
		unsubDKIM:  unsub.NewVerifier(),
		unsubHTTP:  unsub.NewClient(),
		unsubSlots: make(chan struct{}, unsubPreviews),
		view:       viewGen{epoch: newEpoch()},
	}
	for i, a := range accounts {
		s.byName[a.Name] = a
		s.order[a.Name] = i
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /open", s.Auth.Open)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /theme.css", s.theme)
	mux.HandleFunc("GET /{$}", s.list("inbox", "Inbox", "tag:inbox"))
	mux.HandleFunc("GET /starred", s.list("starred", "Starred", "tag:flagged"))
	mux.HandleFunc("GET /sent", s.list("sent", "Sent", "tag:sent"))
	mux.HandleFunc("GET /spam", s.list("spam", "Spam", "tag:spam"))
	mux.HandleFunc("GET /trash", s.list("trash", "Trash", "tag:trash"))
	// Gmail's All Mail: everything but spam and trash. Fixed queries run
	// verbatim (list never combines them with ?q=), so no outer parens.
	mux.HandleFunc("GET /all", s.list("all", "All Mail", "not tag:spam and not tag:trash"))
	mux.HandleFunc("GET /search", s.list("search", "Search", ""))
	mux.HandleFunc("GET /t/{account}/{thread}", s.thread)
	mux.HandleFunc("GET /gmail/{account}/{thread}", s.gmail)
	// {msgid} is one path segment: callers url.PathEscape it ('/' is legal in a Message-ID).
	mux.HandleFunc("GET /body/{account}/{msgid}", s.body)
	mux.HandleFunc("GET /part/{account}/{msgid}/{n}", s.part)
	mux.HandleFunc("GET /part/{account}/{msgid}/{n}/zip", s.partZip)
	mux.HandleFunc("POST /tag", s.tag)
	mux.HandleFunc("POST /sync", s.syncNow)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /reply/{account}/{msgid}", s.reply)
	mux.HandleFunc("GET /compose", s.compose)
	mux.HandleFunc("POST /send", s.send)
	mux.HandleFunc("GET /addresses", s.addresses)
	// {msgid} as for /body. The literal "result" segment wins over {account}.
	mux.HandleFunc("GET /unsubscribe/{account}/{msgid}", s.unsubPreview)
	mux.HandleFunc("GET /unsubscribe-result/{token}", s.unsubResultGet)
	mux.HandleFunc("POST /unsubscribe", s.unsubExecute)
	mux.HandleFunc("POST /accounts/{account}/pull", s.retryPull)
	mux.HandleFunc("POST /accounts/{account}/reauth", s.reauth)
	mux.HandleFunc("POST /accounts/{account}/reauth/cancel", s.reauthCancel)

	s.handler = s.Auth.Middleware(mux)
	return s, nil
}

// parsePages gives each page its own set: every page redefines "content".
func parsePages() (map[string]*template.Template, error) {
	base, err := template.ParseFS(templateFS, "templates/base.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{"base": base}
	for _, name := range []string{"list", "thread", "compose"} {
		t, err := template.Must(base.Clone()).ParseFS(templateFS, "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		pages[name] = t
	}
	return pages, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// account resolves the {account} path value; the database is the identity.
func (s *Server) account(r *http.Request) (notmuch.Account, bool) {
	a, ok := s.byName[r.PathValue("account")]
	return a, ok
}

// Page is what base.html needs; every page's data embeds it.
type Page struct {
	Origin string
	Title  string
	Query  string // echoed into the search box
	View   string // the index view, for the header nav; "" on thread and compose pages
	// Accounts is the #accounts strip's data-accounts attribute: every
	// account's onboarding and sync state, which app.js renders (the strip
	// and the status line) and SSE keeps current.
	Accounts template.HTMLAttr
	// SyncEvery is the sync period in seconds (0: unknown), the status
	// line's stale threshold.
	SyncEvery int
	// Label is the view generation the page was rendered at, read before
	// its query (viewLabel); app.js compares it with hello and `view`.
	Label viewLabel
}

// NavLink is one header nav entry.
type NavLink struct {
	Key, View, Name, Href string
	Active                bool
}

// navViews is the header nav in key order; app.js VIEWS binds 1..6 to the
// same hrefs.
var navViews = []NavLink{
	{Key: "1", View: "inbox", Name: "Inbox", Href: "/"},
	{Key: "2", View: "starred", Name: "Starred", Href: "/starred"},
	{Key: "3", View: "sent", Name: "Sent", Href: "/sent"},
	{Key: "4", View: "spam", Name: "Spam", Href: "/spam"},
	{Key: "5", View: "trash", Name: "Trash", Href: "/trash"},
	{Key: "6", View: "all", Name: "All", Href: "/all"},
}

// Nav is the header nav with the current view marked.
func (p Page) Nav() []NavLink {
	out := make([]NavLink, len(navViews))
	for i, l := range navViews {
		l.Active = l.View == p.View
		out[i] = l
	}
	return out
}

// page is base.html's data; at is the label read before the page's query.
func (s *Server) page(title, query string, at viewLabel) Page {
	return Page{Origin: s.Auth.Origin, Title: title, Query: query, Accounts: s.accountsJSON(), SyncEvery: int(s.SyncInterval / time.Second), Label: at}
}

func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.pages[page].ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}
