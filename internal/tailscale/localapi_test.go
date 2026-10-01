package tailscale

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Shapes as tailscale 1.102.4 answers them (trimmed; real field names and
// types, including the ones pneu ignores).
const statusJSON = `{
	"Version": "1.102.4", "TUN": true, "BackendState": "Running", "HaveNodeKey": true,
	"TailscaleIPs": ["100.121.74.67", "fd7a:115c:a1e0::2a01:4a5c"],
	"Self": {"ID": "nxzXSZ2TfK11CNTRL", "NodeID": 2390029678764129, "HostName": "server",
		"UserID": 131792011117475, "TailscaleIPs": ["100.121.74.67", "fd7a:115c:a1e0::2a01:4a5c"],
		"Online": true, "CapMap": {"funnel": []}},
	"User": {"131792011117475": {"ID": 131792011117475, "LoginName": "a@b.example"}}
}`

const whoisJSON = `{
	"Node": {"ID": 1, "StableID": "nV6M3oJKo721CNTRL", "Name": "macbook.x.ts.net.", "User": 131792011117475,
		"Addresses": ["100.91.195.0/32", "fd7a:115c:a1e0::2836:c301/128"],
		"AllowedIPs": ["100.91.195.0/32", "10.0.0.0/24"], "Hostinfo": {"Hostname": "macbook"}},
	"UserProfile": {"ID": 131792011117475, "LoginName": "a@b.example", "DisplayName": "A"},
	"CapMap": null
}`

const taggedJSON = `{
	"Node": {"StableID": "nTHnBkfDVn11CNTRL", "User": 7909936612895534, "Tags": ["tag:ingress"], "Sharer": 55,
		"Addresses": ["fd7a:115c:a1e0::3901:8ebc/128"]},
	"UserProfile": {"ID": 7909936612895534, "LoginName": "tagged-devices"}
}`

// fakeTailscaled serves h on a unix socket in a short temp dir.
func fakeTailscaled(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	d, err := os.MkdirTemp("", "pneuts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	path := filepath.Join(d, "tailscaled.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func TestLocal(t *testing.T) {
	var gotHost, gotSec string
	path := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotSec = r.Host, r.Header.Get("Sec-Tailscale")
		if r.Host != "local-tailscaled.sock" {
			http.Error(w, "invalid localapi request", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/localapi/v0/status":
			if r.URL.Query().Get("peers") != "false" {
				t.Errorf("status query %q", r.URL.RawQuery)
			}
			w.Write([]byte(statusJSON))
		case "/localapi/v0/whois":
			switch r.URL.Query().Get("addr") {
			case "100.91.195.0:51234":
				w.Write([]byte(whoisJSON))
			case "[fd7a:115c:a1e0::3901:8ebc]:1":
				w.Write([]byte(taggedJSON))
			case "100.64.0.9:1":
				http.Error(w, "no match for IP:port", http.StatusNotFound)
			case "100.64.0.10:1":
				time.Sleep(3 * time.Second)
			default:
				t.Errorf("whois addr %q", r.URL.Query().Get("addr"))
			}
		}
	})
	l := newLocal(path, os.Getuid())
	ctx := context.Background()

	st, err := l.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.BackendState != Running || st.Self.StableID != "nxzXSZ2TfK11CNTRL" || st.Self.UserID != 131792011117475 ||
		len(st.Self.TailscaleIPs) != 2 || st.Self.TailscaleIPs[1] != netip.MustParseAddr("fd7a:115c:a1e0::2a01:4a5c") {
		t.Fatalf("status %+v", st)
	}
	if gotHost != "local-tailscaled.sock" || gotSec != "localapi" {
		t.Errorf("host %q, Sec-Tailscale %q", gotHost, gotSec)
	}

	w, err := l.WhoIs(ctx, netip.MustParseAddrPort("100.91.195.0:51234"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Node.StableID != "nV6M3oJKo721CNTRL" || w.UserProfile.ID != 131792011117475 || len(w.Node.Tags) != 0 || w.Node.Sharer != 0 ||
		len(w.Node.Addresses) != 2 || w.Node.Addresses[0] != netip.MustParsePrefix("100.91.195.0/32") {
		t.Fatalf("whois %+v", w)
	}
	w, err = l.WhoIs(ctx, netip.MustParseAddrPort("[fd7a:115c:a1e0::3901:8ebc]:1"))
	if err != nil || len(w.Node.Tags) != 1 || w.Node.Sharer != 55 {
		t.Fatalf("tagged whois %+v %v", w, err)
	}
	if _, err := l.WhoIs(ctx, netip.MustParseAddrPort("100.64.0.9:1")); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("unknown address: %v", err)
	}
	start := time.Now()
	if _, err := l.WhoIs(ctx, netip.MustParseAddrPort("100.64.0.10:1")); err == nil || time.Since(start) > CallTimeout+time.Second {
		t.Fatalf("stalled whois: %v after %v", err, time.Since(start))
	}
}

func TestLocalSocketChecks(t *testing.T) {
	path := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(statusJSON)) })
	// Another owner: refused before any request.
	if _, err := newLocal(path, os.Getuid()+1).Status(context.Background()); !errors.Is(err, ErrNoSocket) {
		t.Fatalf("wrong owner: %v", err)
	}
	// A symlink to the socket is not the socket.
	link := filepath.Join(filepath.Dir(path), "link.sock")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newLocal(link, os.Getuid()).Status(context.Background()); !errors.Is(err, ErrNoSocket) {
		t.Fatalf("symlink: %v", err)
	}
	// A regular file isn't one either; nor is nothing.
	file := filepath.Join(filepath.Dir(path), "file")
	os.WriteFile(file, nil, 0o600)
	for _, p := range []string{file, filepath.Join(filepath.Dir(path), "missing")} {
		if _, err := newLocal(p, os.Getuid()).Status(context.Background()); !errors.Is(err, ErrNoSocket) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if NewLocal().path != "/var/run/tailscale/tailscaled.sock" || NewLocal().owner != 0 {
		t.Fatal("NewLocal must use the fixed root-owned socket")
	}
}

func TestLocalOversize(t *testing.T) {
	path := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"BackendState":"`))
		w.Write(make([]byte, maxBody))
	})
	if _, err := newLocal(path, os.Getuid()).Status(context.Background()); err == nil {
		t.Fatal("oversize answer accepted")
	}
}

// status?peers=true as 1.102.4 answers it (trimmed): the peer map is keyed
// by node key, "ID" is the StableID, Tags is absent when empty.
const peersJSON = `{
	"BackendState": "Running",
	"Self": {"ID": "nMAC1CNTRL", "NodeID": 7, "UserID": 131792011117475, "TailscaleIPs": ["100.91.195.0"]},
	"Peer": {
		"nodekey:0378": {"ID": "nxzXSZ2TfK11CNTRL", "NodeID": 2390029678764129, "HostName": "server", "DNSName": "server.x.ts.net.",
			"UserID": 131792011117475, "TailscaleIPs": ["100.121.74.67", "fd7a:115c:a1e0::2a01:4a5c"],
			"Online": true, "LastSeen": "0001-01-01T00:00:00Z", "Active": true},
		"nodekey:a1b2": {"ID": "nTHnBkfDVn11CNTRL", "UserID": 7909936612895534, "Tags": ["tag:ingress"], "ShareeNode": true,
			"TailscaleIPs": ["fd7a:115c:a1e0::3901:8ebc"], "Online": false, "LastSeen": "2026-09-29T19:26:43.1Z"}
	}
}`

func TestLocalStatusPeers(t *testing.T) {
	path := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/status" || r.URL.Query().Get("peers") != "true" {
			t.Errorf("request %s", r.URL)
		}
		w.Write([]byte(peersJSON))
	})
	st, err := newLocal(path, os.Getuid()).StatusPeers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server, off := st.Peer["nodekey:0378"], st.Peer["nodekey:a1b2"]
	if st.Self.StableID != "nMAC1CNTRL" || len(st.Peer) != 2 || server.StableID != "nxzXSZ2TfK11CNTRL" || !server.Online ||
		len(server.TailscaleIPs) != 2 || server.TailscaleIPs[0] != netip.MustParseAddr("100.121.74.67") ||
		off.Online || off.LastSeen.IsZero() {
		t.Fatalf("status %+v", st)
	}
}
