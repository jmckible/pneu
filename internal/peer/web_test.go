package peer

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmckible/pneu/internal/notmuch"
	"github.com/jmckible/pneu/internal/testmail"
	"github.com/jmckible/pneu/internal/web"
)

// The real web handler behind the real listener: the page's origin is the
// peer record's, every answer names the protocol, the client's local
// routes don't exist, Host is checked, and no cookie or Origin is asked.
func TestWebOverLink(t *testing.T) {
	env := testmail.Setup(t)
	var accounts []notmuch.Account
	for _, a := range env.Accounts {
		accounts = append(accounts, notmuch.Account{Name: a.Name, Email: a.Email, ConfigPath: a.NotmuchConfig, Maildir: filepath.Join(a.Root, "gmail", "mail")})
	}
	srv, err := web.New(accounts, "pneu.localhost:7317", strings.Repeat("0123456789abcdef", 2))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(s *Server) http.Handler { return srv.PeerHandler(s) }, nil)
	mac := newIdentity(t)
	h.pairOrigin("macbook", clientNode, mac, "http://pneu.localhost:7400")
	c := h.client(&mac, nil)

	resp, body, err := get(t, c, h.url("/"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.ProtoMajor != 2 || !strings.Contains(body, `data-origin="http://pneu.localhost:7400"`) {
		t.Fatalf("GET /: %d %s\n%.300s", resp.StatusCode, resp.Proto, body)
	}
	if resp.Header.Get(web.ProtocolHeader) != "1" || resp.Header.Get(web.PolicyHeader) != "app" {
		t.Fatalf("headers %v", resp.Header)
	}

	for _, p := range []string{"/open?nonce=x", "/theme.css"} {
		if resp, _, err := get(t, c, h.url(p)); err != nil || resp.StatusCode != 404 || resp.Header.Get(web.ProtocolHeader) != "1" {
			t.Errorf("%s: %v %v", p, resp, err)
		}
	}

	resp, body, err = get(t, c, h.url("/peer/hello"))
	var hello struct {
		Protocol int    `json:"protocol"`
		Name     string `json:"name"`
		Epoch    string `json:"epoch"`
	}
	if err != nil || resp.StatusCode != 200 || json.Unmarshal([]byte(body), &hello) != nil || hello.Protocol != web.Protocol || hello.Epoch == "" {
		t.Fatalf("hello: %v %q", err, body)
	}

	// Host must be the listener's own address.
	req, _ := http.NewRequest("GET", h.url("/"), nil)
	req.Host = "pneu.localhost:7317"
	if resp, err := c.Do(req); err != nil || resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("wrong Host: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// A mutation needs no cookie and no Origin; a hostile Origin changes
	// nothing (the empty form is the handler's 400, past the guard).
	req, _ = http.NewRequest("POST", h.url("/tag"), strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	if resp, err := c.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /tag: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// Removed: the connection is gone and a new one fails the handshake.
	if err := h.unpair("macbook"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := get(t, h.client(&mac, nil), h.url("/")); err == nil {
		t.Fatal("removed peer served")
	}
}
