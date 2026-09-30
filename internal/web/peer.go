package web

import (
	"context"
	"net/http"
	"os"
	"strconv"

	"github.com/jmckible/pneu/internal/control"
)

// Protocol is the link's contract version (docs/client.md, "What skews"):
// the route table, event shapes, policy classes and the status doc. A
// change to any of them bumps it.
const Protocol = 1

// ProtocolHeader carries Protocol on every peer response.
const ProtocolHeader = "Pneu-Protocol"

// PeerGuard is the peer listener's view of a request (peer.Server): who
// sent it, judged afresh on every request, and which Host names it.
type PeerGuard interface {
	// Identify returns the peer's name and recorded origin, or false when
	// the request's connection isn't a live, leased, paired peer's.
	Identify(r *http.Request) (name, origin string, ok bool)
	// HostOK reports whether host is one of the listener's own addresses.
	HostOK(host string) bool
}

// peerInfo is an admitted peer request's identity, in its context.
type peerInfo struct{ name, origin string }

type peerKey struct{}

// peerOf is the peer a request came from; false on loopback.
func peerOf(r *http.Request) (peerInfo, bool) {
	p, ok := r.Context().Value(peerKey{}).(peerInfo)
	return p, ok
}

// PeerHandler is the route table behind the peer guard, for the peer
// listener: the same handlers as loopback minus the client's local routes
// (/open, /theme.css) plus /peer/hello. The only credential is the TLS peer,
// re-checked on every request (R2); there's no cookie, Origin or nonce:
// the client daemon applied its own Auth to the browser's request, and no
// browser can complete the handshake. Host must still be one of the
// listener's addresses.
func (s *Server) PeerHandler(g PeerGuard) http.Handler {
	mux := s.peerRoutes()
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set(ProtocolHeader, strconv.Itoa(Protocol))
		w := &policyWriter{ResponseWriter: rw, r: r, check: s.Auth.check}
		if !g.HostOK(r.Host) {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		name, origin, ok := g.Identify(r)
		if !ok {
			http.Error(w, "not a paired peer", http.StatusForbidden)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), peerKey{}, peerInfo{name, origin}))
		mux.ServeHTTP(w, r)
		w.finish()
	})
}

// origin is the origin a request's pages run in: the loopback's own, or
// for a peer the origin recorded at pairing, never a header (T8).
func (s *Server) origin(r *http.Request) string {
	if p, ok := peerOf(r); ok {
		return p.origin
	}
	return s.Auth.Origin
}

// helloDoc is GET /peer/hello: what the client checks before trusting the
// link's contract, and the server's display name.
type helloDoc struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Modified bool   `json:"modified"`
	Epoch    string `json:"epoch"`
	Gen      uint64 `json:"gen"`
	// Accounts are the archive's accounts: the client checks a /gmail
	// redirect's authuser against these addresses (validGmailURL).
	Accounts []HelloAccount `json:"accounts"`
}

// HelloAccount is one account in hello.
type HelloAccount struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (s *Server) peerHello(w http.ResponseWriter, r *http.Request) {
	name, err := os.Hostname()
	if err != nil {
		name = ""
	}
	in := control.Self()
	l := s.viewLabel()
	accts := make([]HelloAccount, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		accts = append(accts, HelloAccount{Name: a.Name, Email: a.Email})
	}
	tagJSON(w, http.StatusOK, helloDoc{Protocol: Protocol, Name: name, Revision: in.Revision, Modified: in.Modified, Epoch: l.Epoch, Gen: l.Gen, Accounts: accts})
}
