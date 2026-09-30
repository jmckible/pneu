package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
)

// Route is one entry of the route table: a method and ServeMux pattern and
// the response contract every answer on it must meet. The server registers
// its handlers from the table and nowhere else, and the client proxy
// (docs/client.md, "Routing") matches requests and checks responses against
// the same entries, so matching and policy are one piece of code in both.
type Route struct {
	Method, Pattern string
	// Handler is the server's handler for the route.
	Handler func(*Server) http.Handler
	// Local routes are answered by the client daemon itself, never
	// forwarded: its own launch, theme and event stream. The peer listener
	// doesn't serve them, unless Upstream.
	Local bool
	// Upstream: a Local route the client daemon itself reads from the
	// server over the link (/events, its one upstream stream), so the peer
	// listener serves it; the client still never forwards a browser's.
	Upstream bool
	// PeerOnly routes exist only on the peer listener (/peer/hello).
	PeerOnly bool
	// Types are the policy classes the route may answer with, each with
	// the parsed media types allowed under it. anyInert is any type /part
	// may serve (never an active one; SVG only per Dest).
	Types       map[Policy][]string
	Disposition DispRule
	Dest        DestRule
	Location    LocRule
	Cache       CacheRule
}

// DispRule says whether a response may carry Content-Disposition.
type DispRule int

const (
	DispNone DispRule = iota // never
	// DispPart: a part class always carries it, inline only for a type
	// the browser may show (inlineTypes), with a filename; nothing else does.
	DispPart
)

// DestRule constrains responses by the request's Sec-Fetch-Dest.
type DestRule int

const (
	DestAny DestRule = iota
	// DestSVGImage: image/svg+xml only to an image destination, where it
	// runs no script (read.go part).
	DestSVGImage
)

// LocRule says where a redirect may point.
type LocRule int

const (
	LocNone  LocRule = iota // no redirects
	LocLocal                // on the app's own origin (localLocation)
	LocGmail                // the account's message in Gmail (validGmailURL)
)

// CacheRule is the route's Cache-Control, set on every response.
type CacheRule string

const (
	// CacheNoStore: pages, mail, parts, JSON. The --app window shares the
	// daily browser's profile and its disk cache.
	CacheNoStore CacheRule = "no-store"
	// CacheRevalidate: /static/. Stored, but checked on every use against
	// its ETag, so a repaired file replaces a bad one at the next load.
	CacheRevalidate CacheRule = "no-cache"
)

// anyInert in Types allows any media type /part may serve.
const anyInert = "*"

var (
	pageTypes    = map[Policy][]string{PolicyApp: {"text/html"}, PolicyData: {"text/plain"}}
	listTypes    = map[Policy][]string{PolicyApp: {"text/html"}, PolicyData: {"text/plain", "application/json"}} // ?total=1
	composeTypes = map[Policy][]string{PolicyCompose: {"text/html"}, PolicyData: {"text/plain"}}
	jsonTypes    = map[Policy][]string{PolicyData: {"application/json", "text/plain"}}
	partTypes    = map[Policy][]string{PolicyPart: {anyInert}, PolicyPDF: {"application/pdf"}, PolicyData: {"text/plain"}}
	// An HTML redirect body: http.Redirect writes one for GET.
	redirectTypes = map[Policy][]string{PolicyApp: {"text/html"}, PolicyData: {"text/plain"}}
)

func listRoute(pattern, view, title, fixed string) Route {
	return Route{Method: "GET", Pattern: pattern, Types: listTypes, Cache: CacheNoStore,
		Handler: func(s *Server) http.Handler { return s.list(view, title, fixed) }}
}

func handler(f func(*Server, http.ResponseWriter, *http.Request)) func(*Server) http.Handler {
	return func(s *Server) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f(s, w, r) })
	}
}

// Routes is the route table. A new route goes here with its classes, or
// it doesn't exist: New registers nothing else.
var Routes = []Route{
	{Method: "GET", Pattern: "/open", Local: true, Types: redirectTypes, Location: LocLocal, Cache: CacheNoStore,
		Handler: func(s *Server) http.Handler { return http.HandlerFunc(s.Auth.Open) }},
	{Method: "GET", Pattern: "/static/", Cache: CacheRevalidate,
		Types:   map[Policy][]string{PolicyData: {"text/css", "text/javascript", "application/javascript", "text/plain"}, PolicyStaticSVG: {"image/svg+xml"}},
		Handler: func(s *Server) http.Handler { return s.static }},
	{Method: "GET", Pattern: "/events", Local: true, Upstream: true, Cache: CacheNoStore,
		Types: map[Policy][]string{PolicyData: {"text/event-stream", "text/plain"}}, Handler: handler((*Server).events)},
	{Method: "GET", Pattern: "/theme.css", Local: true, Cache: CacheNoStore,
		Types: map[Policy][]string{PolicyData: {"text/css", "text/plain"}}, Handler: handler((*Server).theme)},
	listRoute("/{$}", "inbox", "Inbox", "tag:inbox"),
	listRoute("/starred", "starred", "Starred", "tag:flagged"),
	listRoute("/sent", "sent", "Sent", "tag:sent"),
	listRoute("/spam", "spam", "Spam", "tag:spam"),
	listRoute("/trash", "trash", "Trash", "tag:trash"),
	// Gmail's All Mail: everything but spam and trash. Fixed queries run
	// verbatim (list never combines them with ?q=), so no outer parens.
	listRoute("/all", "all", "All Mail", "not tag:spam and not tag:trash"),
	listRoute("/search", "search", "Search", ""),
	{Method: "GET", Pattern: "/t/{account}/{thread}", Types: pageTypes, Cache: CacheNoStore, Handler: handler((*Server).thread)},
	{Method: "GET", Pattern: "/gmail/{account}/{thread}", Types: redirectTypes, Location: LocGmail, Cache: CacheNoStore, Handler: handler((*Server).gmail)},
	// {msgid} is one path segment: callers url.PathEscape it ('/' is legal in a Message-ID).
	{Method: "GET", Pattern: "/body/{account}/{msgid}", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).body)},
	{Method: "GET", Pattern: "/part/{account}/{msgid}/{n}", Types: partTypes, Disposition: DispPart, Dest: DestSVGImage, Cache: CacheNoStore, Handler: handler((*Server).part)},
	{Method: "GET", Pattern: "/part/{account}/{msgid}/{n}/zip", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).partZip)},
	{Method: "POST", Pattern: "/tag", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).tag)},
	{Method: "POST", Pattern: "/sync", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).syncNow)},
	{Method: "GET", Pattern: "/status", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).status)},
	{Method: "GET", Pattern: "/reply/{account}/{msgid}", Types: composeTypes, Cache: CacheNoStore, Handler: handler((*Server).reply)},
	{Method: "GET", Pattern: "/compose", Types: composeTypes, Cache: CacheNoStore, Handler: handler((*Server).compose)},
	// Answers with the re-rendered form, or a bodiless redirect to the sent message.
	{Method: "POST", Pattern: "/send", Types: composeTypes, Location: LocLocal, Cache: CacheNoStore, Handler: handler((*Server).send)},
	{Method: "GET", Pattern: "/addresses", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).addresses)},
	// {msgid} as for /body. The literal "result" segment wins over {account}.
	{Method: "GET", Pattern: "/unsubscribe/{account}/{msgid}", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).unsubPreview)},
	{Method: "GET", Pattern: "/unsubscribe-result/{token}", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).unsubResultGet)},
	{Method: "POST", Pattern: "/unsubscribe", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).unsubExecute)},
	{Method: "POST", Pattern: "/accounts/{account}/pull", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).retryPull)},
	{Method: "POST", Pattern: "/accounts/{account}/reauth", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).reauth)},
	{Method: "POST", Pattern: "/accounts/{account}/reauth/cancel", Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).reauthCancel)},
	{Method: "GET", Pattern: "/peer/hello", PeerOnly: true, Types: jsonTypes, Cache: CacheNoStore, Handler: handler((*Server).peerHello)},
}

// onLoopback and onPeer say which listener serves a route.
func onLoopback(rt *Route) bool { return !rt.PeerOnly }
func onPeer(rt *Route) bool     { return rt.PeerOnly || !rt.Local || rt.Upstream }

// routeMux builds a ServeMux from the table, each route served by h(route).
// The client proxy builds its matcher the same way, so matching (escaped
// segments, {$}, GET-implies-HEAD) is the same code on both sides.
func routeMux(table []Route, keep func(rt *Route) bool, h func(rt *Route) http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	for i := range table {
		rt := &table[i]
		if keep(rt) {
			mux.Handle(rt.Method+" "+rt.Pattern, h(rt))
		}
	}
	return mux
}

// serveRoutes is the server's loopback mux, and peerRoutes the peer
// listener's: each table handler, told its route so the response gets the
// route's cache rule and is checked against it.
func (s *Server) serveRoutes() *http.ServeMux { return s.routesFor(onLoopback) }
func (s *Server) peerRoutes() *http.ServeMux  { return s.routesFor(onPeer) }

func (s *Server) routesFor(keep func(*Route) bool) *http.ServeMux {
	return routeMux(Routes, keep, func(rt *Route) http.Handler {
		next := rt.Handler(s)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if pw := findPolicyWriter(w); pw != nil {
				pw.route = rt
			}
			next.ServeHTTP(w, r)
		})
	})
}

// staticHandler serves the embedded assets with a strong ETag each (their
// content hash): embedded files have no modtime, so FileServerFS alone
// sends no validator and a revalidation would refetch every file. No
// directory listings.
func staticHandler(static fs.FS) (http.Handler, error) {
	tags := map[string]string{}
	err := fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(static, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		tags[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	if err != nil {
		return nil, err
	}
	files := http.FileServerFS(static)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag, ok := tags[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if path.Ext(r.URL.Path) == ".svg" {
			usePolicy(w, PolicyStaticSVG)
		}
		w.Header().Set("ETag", tag)
		singleRange(r)
		files.ServeHTTP(w, r)
	})), nil
}

// singleRange drops a multi-range request's Range, so it gets the whole
// file as a 200: a multipart/byteranges answer is one more type every
// route would have to allow, and nothing of ours asks for one.
func singleRange(r *http.Request) {
	if strings.Contains(r.Header.Get("Range"), ",") {
		r.Header.Del("Range")
	}
}

// ---- checking a response against its route ---------------------------------

// Checker validates responses against the route table: what the client
// proxy runs on every upstream answer before re-applying its class, and
// what the server's tests run on every handler's.
type Checker struct {
	Origin string // "http://pneu.localhost:7317"
	// Email is an account's address, for LocGmail.
	Email func(account string) (string, bool)
}

// unrouted is the contract for a response no route answered: the
// middleware's refusals and the mux's 404 and 405; unroutedRedirect, the
// mux's trailing-slash redirect ("/static" to "/static/").
var (
	unrouted         = Route{Types: map[Policy][]string{PolicyData: {"text/plain"}}, Cache: CacheNoStore}
	unroutedRedirect = Route{Types: redirectTypes, Location: LocLocal, Cache: CacheNoStore}
)

// Check reports why a response with status and headers h, answering r on
// route rt (nil: unrouted), breaks the route's contract.
func (c Checker) Check(rt *Route, status int, h http.Header, r *http.Request) error {
	if rt == nil {
		rt = &unrouted
		if status >= 300 && status < 400 {
			rt = &unroutedRedirect
		}
	}
	pv := h.Values(PolicyHeader)
	if len(pv) != 1 {
		return fmt.Errorf("%d %s values", len(pv), PolicyHeader)
	}
	p := Policy(pv[0])
	allowed, ok := rt.Types[p]
	if !ok {
		return fmt.Errorf("class %q not allowed on %s", p, rt.Pattern)
	}
	cts := h.Values("Content-Type")
	mt := ""
	switch len(cts) {
	case 0:
		// A redirect, a 304, or a 204 may have no body to type.
		if !(status >= 300 && status < 400 || status == http.StatusNoContent) {
			return fmt.Errorf("no Content-Type on a %d", status)
		}
	case 1:
		var err error
		if mt, _, err = mime.ParseMediaType(cts[0]); err != nil {
			return fmt.Errorf("Content-Type %q: %v", cts[0], err)
		}
		mt = strings.ToLower(mt)
		if !typeAllowed(allowed, mt) {
			return fmt.Errorf("%s not allowed as %q on %s", mt, p, rt.Pattern)
		}
	default:
		return fmt.Errorf("%d Content-Type values", len(cts))
	}
	if mt == "image/svg+xml" && p == PolicyData {
		return errors.New("SVG without a sandbox")
	}
	if mt == "image/svg+xml" && rt.Dest == DestSVGImage && r.Header.Get("Sec-Fetch-Dest") != "image" {
		return errors.New("SVG outside an image destination")
	}
	if p == PolicyData && rt.Disposition == DispPart && status < 400 {
		return errors.New("a part as data: only its errors are")
	}
	if err := checkDisposition(rt, p, status, mt, h); err != nil {
		return err
	}
	return c.checkLocation(rt, status, h, r)
}

func typeAllowed(allowed []string, mt string) bool {
	if slices.Contains(allowed, mt) {
		return true
	}
	// SVG is XML, which neuter catches: DestSVGImage is its rule.
	return slices.Contains(allowed, anyInert) && strings.Contains(mt, "/") && (!neuter(mt) || mt == "image/svg+xml")
}

func checkDisposition(rt *Route, p Policy, status int, mt string, h http.Header) error {
	ds := h.Values("Content-Disposition")
	bodiless := status == http.StatusNotModified || status == http.StatusNoContent
	part := rt.Disposition == DispPart && (p == PolicyPart || p == PolicyPDF) && !bodiless
	if !part {
		if len(ds) > 0 {
			return errors.New("unexpected Content-Disposition")
		}
		return nil
	}
	if len(ds) != 1 {
		return fmt.Errorf("%d Content-Disposition values on a part", len(ds))
	}
	disp, params, err := mime.ParseMediaType(ds[0])
	if err != nil {
		return fmt.Errorf("Content-Disposition %q: %v", ds[0], err)
	}
	switch disp {
	case "attachment":
	case "inline":
		if !inlineTypes[mt] {
			return fmt.Errorf("%s inline", mt)
		}
	default:
		return fmt.Errorf("Content-Disposition %q", disp)
	}
	if params["filename"] == "" {
		return errors.New("Content-Disposition without a filename")
	}
	return nil
}

func (c Checker) checkLocation(rt *Route, status int, h http.Header, r *http.Request) error {
	ls := h.Values("Location")
	redirect := status >= 300 && status < 400 && status != http.StatusNotModified
	if !redirect || rt.Location == LocNone {
		if len(ls) > 0 {
			return errors.New("unexpected Location")
		}
		if redirect {
			return fmt.Errorf("%d on a route that doesn't redirect", status)
		}
		return nil
	}
	if len(ls) != 1 {
		return fmt.Errorf("%d Location values", len(ls))
	}
	switch rt.Location {
	case LocGmail:
		email, ok := "", false
		if c.Email != nil {
			email, ok = c.Email(r.PathValue("account"))
		}
		if !ok || !validGmailURL(ls[0], email) {
			return fmt.Errorf("Location %q isn't the account's Gmail", ls[0])
		}
	default:
		base, err := url.Parse(c.Origin)
		if err != nil {
			return err
		}
		base = base.ResolveReference(&url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath})
		if _, err := localLocation(ls[0], base); err != nil {
			return err
		}
	}
	return nil
}

// gmailPrefix is gmailURL's fixed part for an account.
func gmailPrefix(email string) string {
	return "https://mail.google.com/mail/?authuser=" + url.QueryEscape(email) + "#all/"
}

// validGmailURL reports whether u is exactly what gmailURL produces for
// email: the fixed prefix and a hex message id, nothing else. A string
// comparison, so case, userinfo, ports, extra parameters or an encoded
// variant all fail.
func validGmailURL(u, email string) bool {
	if email == "" {
		return false
	}
	id, ok := strings.CutPrefix(u, gmailPrefix(email))
	return ok && gmailIDRE.MatchString(id)
}

// localLocation resolves a redirect's Location against base (the local
// origin and the request's path) and returns it if it stays on that
// origin. The raw value is held to more than the resolution: no scheme, no
// authority, no backslash, no control, space or non-ASCII byte, and no
// encoded slash or backslash, since browsers and URL parsers disagree on
// all of these at the edges.
func localLocation(loc string, base *url.URL) (string, error) {
	bad := func(why string) (string, error) { return "", fmt.Errorf("Location %q: %s", loc, why) }
	if loc == "" {
		return bad("empty")
	}
	for i := 0; i < len(loc); i++ {
		if c := loc[i]; c <= ' ' || c >= 0x7f || c == '\\' {
			return bad("backslash, space, control or non-ASCII byte")
		}
	}
	if low := strings.ToLower(loc); strings.Contains(low, "%2f") || strings.Contains(low, "%5c") {
		return bad("encoded slash")
	}
	if strings.HasPrefix(loc, "//") {
		return bad("authority")
	}
	u, err := url.Parse(loc)
	if err != nil {
		return bad(err.Error())
	}
	if u.Scheme != "" || u.Opaque != "" || u.User != nil || u.Host != "" {
		return bad("scheme or authority")
	}
	// A colon before the first slash would read as a scheme to a browser
	// even where url.Parse saw a path.
	if i := strings.IndexAny(loc, ":/?#"); i >= 0 && loc[i] == ':' {
		return bad("scheme")
	}
	out := base.ResolveReference(u)
	if out.Scheme != base.Scheme || out.Host != base.Host || out.User != nil {
		return bad("leaves the origin")
	}
	return out.String(), nil
}
