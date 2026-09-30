// Package tailscale is the little of tailscaled's LocalAPI pneu uses: who
// this node is, and who is behind a tailnet address (docs/client.md,
// "Tailscale as the second check"). Stdlib HTTP over tailscaled's unix
// socket, at its fixed path only.
package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"syscall"
	"time"
)

// SocketPath is tailscaled's LocalAPI socket. Fixed, never from the
// environment: whatever answers here vouches for peers, so it must be
// root's (checked before each call).
const SocketPath = "/var/run/tailscale/tailscaled.sock"

// CallTimeout bounds each LocalAPI call.
const CallTimeout = 2 * time.Second

// maxBody bounds a LocalAPI answer. status?peers=false is a few KiB.
const maxBody = 1 << 20

// Running is the BackendState of a connected node.
const Running = "Running"

// Status is what pneu reads of /localapi/v0/status.
type Status struct {
	BackendState string
	Self         Self
}

// Self is this node.
type Self struct {
	StableID     string       `json:"ID"` // status names the stable ID "ID"; the numeric one is NodeID
	UserID       int64        // the node's owner
	TailscaleIPs []netip.Addr // this node's own addresses, v4 and v6
}

// WhoIs is /localapi/v0/whois: the node behind an address and its owner.
type WhoIs struct {
	Node        Node
	UserProfile UserProfile
}

// Node is the part of tailcfg.Node the whois predicate reads. Tags and
// Sharer are omitempty on the wire: absent is empty.
type Node struct {
	StableID  string
	Addresses []netip.Prefix // the node's own addresses, as /32 and /128
	Tags      []string
	Sharer    int64 // non-zero: shared in from another tailnet
	User      int64
}

// UserProfile is the node's user; a tagged node's is "tagged-devices".
type UserProfile struct {
	ID        int64
	LoginName string
}

// API is the LocalAPI as pneu uses it; tests use a fake.
type API interface {
	Status(ctx context.Context) (Status, error)
	WhoIs(ctx context.Context, addr netip.AddrPort) (WhoIs, error)
}

// ErrNoSocket: tailscaled isn't there (or isn't root's).
var ErrNoSocket = errors.New("tailscale: no tailscaled socket")

// ErrNoMatch: whois knows no node at the address.
var ErrNoMatch = errors.New("tailscale: no node at that address")

// Local talks to tailscaled on SocketPath.
type Local struct {
	path  string // SocketPath outside tests
	owner int    // uid the socket must belong to: root outside tests
	hc    *http.Client
}

// NewLocal is the LocalAPI client on the fixed socket.
func NewLocal() *Local { return newLocal(SocketPath, 0) }

func newLocal(path string, owner int) *Local {
	l := &Local{path: path, owner: owner}
	l.hc = &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if err := l.checkSocket(); err != nil {
					return nil, err
				}
				var d net.Dialer
				return d.DialContext(ctx, "unix", l.path)
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return l
}

// checkSocket insists the path is a socket owned by the expected uid, via
// Lstat so a symlink is seen as one.
func (l *Local) checkSocket() error {
	fi, err := os.Lstat(l.path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoSocket, err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%w: %s is %v, not a socket", ErrNoSocket, l.path, fi.Mode().Type())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != l.owner {
		return fmt.Errorf("%w: %s isn't owned by uid %d", ErrNoSocket, l.path, l.owner)
	}
	return nil
}

// Status is this node's backend state and identity (no peers).
func (l *Local) Status(ctx context.Context) (Status, error) {
	var st Status
	err := l.get(ctx, "/localapi/v0/status?peers=false", &st)
	return st, err
}

// WhoIs asks who is behind addr.
func (l *Local) WhoIs(ctx context.Context, addr netip.AddrPort) (WhoIs, error) {
	var w WhoIs
	err := l.get(ctx, "/localapi/v0/whois?addr="+url.QueryEscape(addr.String()), &w)
	return w, err
}

func (l *Local) get(ctx context.Context, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	// tailscaled refuses any other Host (DNS rebinding of its HTTP side).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Sec-Tailscale", "localapi")
	resp, err := l.hc.Do(req)
	if err != nil {
		return fmt.Errorf("tailscale: %w", err)
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxBody+1)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoMatch
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("tailscale: %s: HTTP %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("tailscale: %w", err)
	}
	if len(b) > maxBody {
		return fmt.Errorf("tailscale: %s: answer over %d bytes", path, maxBody)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("tailscale: %s: %w", path, err)
	}
	return nil
}
