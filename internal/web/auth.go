package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// CookieName carries the install token.
const CookieName = "pneu"

// AppCSP is the app page's policy: a second script wall behind the frame's
// sandbox. A srcdoc frame inherits it and both policies must allow a load,
// so it names nothing the frame needs (inline style, data:/https: images,
// data: fonts) — no default-src, no img-src, no style-src. worker-src
// 'none': a service worker registered by script in this origin would
// outlive the page and the fix, answering future navigations itself
// (docs/client.md, R4). The frame runs no script, so it loses nothing.
const AppCSP = "script-src 'self'; object-src 'none'; base-uri 'none'; worker-src 'none'"

// Auth guards every request. localhost is not a boundary: the browser that
// hosts the --app window is also the user's daily browser, so any open tab
// can reach the port, and DNS rebinding can make a hostile page same-origin
// with 127.0.0.1. Hence:
//
//   - exact Host (defeats rebinding: the rebound page sends its own host),
//   - exact Origin on anything but GET/HEAD (defeats cross-site POST),
//   - the install token in a SameSite=Strict cookie on everything else
//     (defeats cross-site GETs that would leak mail into timing or opener tricks).
//
// The cookie is set only by GET /open?nonce=N. The nonce is single-use: the
// server keeps the current one in the launch file, `pneu open` reads it,
// and a successful open replaces it, so the URL left in history, logs, or
// argv is dead. The token itself never leaves the
// token file and the cookie.
type Auth struct {
	Host   string // e.g. "pneu.localhost:7317"
	Origin string // e.g. "http://pneu.localhost:7317"
	token  string

	mu         sync.Mutex
	nonce      string // "" until StartLaunch: /open refuses everything
	launchPath string

	// check sees every response's final headers (policyWriter); tests only.
	check func(rt *Route, status int, h http.Header, r *http.Request)
}

func NewAuth(host, token string) *Auth {
	return &Auth{Host: host, Origin: "http://" + host, token: token}
}

func (a *Auth) tokenOK(s string) bool {
	return subtle.ConstantTimeCompare([]byte(s), []byte(a.token)) == 1
}

// sessionOK accepts the request if any pneu cookie carries the token.
// Cookies aren't port-isolated: a page on another port of pneu.localhost
// can plant its own "pneu" cookie (with a longer Path, so it is sent
// first), and r.Cookie would read only that one.
func (a *Auth) sessionOK(r *http.Request) bool {
	ok := false
	for _, c := range r.Cookies() {
		if c.Name == CookieName && a.tokenOK(c.Value) {
			ok = true
		}
	}
	return ok
}

// Middleware enforces Host, Origin, and the session cookie. Every response
// it passes on, refusals included, carries its policy class's security
// headers and its route's cache rule (policyWriter).
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &policyWriter{ResponseWriter: rw, r: r, check: a.check}

		if r.Host != a.Host {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != a.Origin {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/open" {
			next.ServeHTTP(w, r)
			w.finish()
			return
		}
		if !a.sessionOK(r) {
			http.Error(w, "pneu: no session. Launch pneu with `pneu open`, which loads its single-use /open URL.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
		w.finish()
	})
}

func isHTML(ctype string) bool {
	mt, _, err := mime.ParseMediaType(ctype)
	return err == nil && (mt == "text/html" || mt == "application/xhtml+xml")
}

// StartLaunch writes a fresh launch nonce to path (0600). The server calls it
// once it is listening; until then /open refuses everything.
func (a *Auth) StartLaunch(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.launchPath = path
	return a.rotateLocked()
}

// rotateLocked retires the current nonce. The new one is live in memory even
// if the file write fails, so a used nonce is dead either way.
func (a *Auth) rotateLocked() error {
	a.nonce = randomHex()
	if a.launchPath == "" {
		return nil
	}
	return writePrivate(a.launchPath, a.nonce+"\n")
}

// Open handles GET /open?nonce=N: check and retire the nonce, plant the
// cookie, go home. It starts no sync: `pneu open` queues the launch sync
// over the control socket first, however the window then opens (a nonce,
// a cookie already there, a focus), and no page can reach that socket.
func (a *Auth) Open(w http.ResponseWriter, r *http.Request) {
	// Chromium restores --app windows after a crash by reloading their URL,
	// which is a spent nonce. A request that already carries the session is
	// simply sent home; the nonce is neither checked nor rotated.
	if a.sessionOK(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	a.mu.Lock()
	// The nonce rides in the query, not the path: Chromium derives a --app
	// window's class from host+path and drops the query, so this keeps the
	// class stable (chrome-pneu.localhost__open-Default under helium) for
	// `pneu open`'s focus match and window rules, while the nonce still
	// changes on every launch.
	ok := a.nonce != "" && subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("nonce")), []byte(a.nonce)) == 1
	if ok {
		if err := a.rotateLocked(); err != nil {
			log.Printf("rotate launch nonce: %v", err)
		}
	}
	a.mu.Unlock()
	if !ok {
		http.Error(w, "bad or used launch nonce", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    a.token,
		Path:     "/",
		MaxAge:   400 * 24 * 60 * 60, // Chromium's cap; the launcher re-plants it anyway
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// stateDir is $XDG_STATE_HOME/pneu, default ~/.local/state/pneu.
func stateDir() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "pneu"), nil
}

// TokenPath is $XDG_STATE_HOME/pneu/token, default ~/.local/state/pneu/token.
func TokenPath() (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "token"), err
}

// LaunchPath is $XDG_STATE_HOME/pneu/launch, default ~/.local/state/pneu/launch.
func LaunchPath() (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "launch"), err
}

// LaunchURL reads the launch file and returns the /open URL for origin. The
// error wraps fs.ErrNotExist when the server has never started.
func LaunchURL(path, origin string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	nonce := strings.TrimSpace(string(b))
	if len(nonce) != 64 || strings.Trim(nonce, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%s: malformed launch nonce", path)
	}
	return origin + "/open?nonce=" + nonce, nil
}

// LoadOrCreateToken reads the install token, generating it (0600) on first
// run. A token file readable by group or others is refused.
func LoadOrCreateToken(path string) (string, error) {
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return "", err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s: mode %04o lets other users read the install token; chmod 600 it (or delete it to mint a new one)", path, fi.Mode().Perm())
		}
		b, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		tok := strings.TrimSpace(string(b))
		if len(tok) < 32 {
			return "", fmt.Errorf("%s: token too short", path)
		}
		return tok, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tok := randomHex()
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return LoadOrCreateToken(path) // lost a race with another first run
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return tok, f.Close()
}

// randomHex is 32 bytes from crypto/rand, hex-encoded.
func randomHex() string {
	raw := make([]byte, 32)
	rand.Read(raw) // never fails (crypto/rand panics on failure)
	return hex.EncodeToString(raw)
}

// writePrivate replaces path with content, 0600, via a rename so a reader
// never sees a partial file.
func writePrivate(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op once renamed
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
