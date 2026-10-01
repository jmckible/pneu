package link_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/peer"
	"github.com/jmckible/pneu/internal/tailscale"
)

// fast is a link clock for tests.
func fast(l *link.Link) {
	l.Timing.BackoffMin = 20 * time.Millisecond
	l.Timing.BackoffMax = 100 * time.Millisecond
	l.Timing.HelloWait = 2 * time.Second
	l.Timing.ProbeWait = time.Second
	l.Timing.Dial = time.Second
	l.Timing.Handshake = 2 * time.Second
}

func start(t *testing.T, api link.API, keys linktest.Keys, port int, tune func(*link.Link)) *link.Link {
	t.Helper()
	l := link.New(api, keys.Creds, port)
	fast(l)
	if tune != nil {
		tune(l)
	}
	l.Start()
	t.Cleanup(l.Close)
	return l
}

func waitReason(t *testing.T, l *link.Link, want link.Reason) link.State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := l.State()
		if st.Reason == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("link %s (%s), want %s", st.Reason, st.Detail, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func get(t *testing.T, l *link.Link, path string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", "https://server"+path, nil)
	return l.RoundTrip(req)
}

func TestUp(t *testing.T) {
	keys := linktest.NewKeys(t)
	var host string
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		w.Write([]byte("hi"))
	}))
	l := start(t, linktest.NewAPI(), keys, u.Port, nil)
	waitReason(t, l, link.Up)
	h := l.Hello()
	if h == nil || h.Name != "dell" || h.Epoch != "abcd" || h.Gen != 3 || len(h.Revision) != 40 {
		t.Fatalf("hello %+v", h)
	}
	if e, ok := l.Email("personal"); !ok || e != "me@example.com" {
		t.Fatalf("email %q %v", e, ok)
	}
	if _, ok := l.Email("work"); ok {
		t.Fatal("an account hello didn't name")
	}
	resp, err := get(t, l, "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// HTTP/2, the checked address as scheme, authority and Host.
	if resp.ProtoMajor != 2 || string(b) != "hi" || host != u.Host() {
		t.Fatalf("%s %q host %q", resp.Proto, b, host)
	}
	// One connection carries hello and requests.
	get(t, l, "/y")
	if n := u.Conns.Load(); n != 1 {
		t.Fatalf("%d connections", n)
	}
}

// A server holding another key fails the pin in the handshake: no request
// is ever sent on it, and it's never retried in the background, only when
// asked.
func TestPinMismatch(t *testing.T) {
	keys := linktest.NewKeys(t)
	other, err := peer.LoadOrCreateServer(filepath.Join(t.TempDir(), "peer"), "impostor")
	if err != nil {
		t.Fatal(err)
	}
	u := linktest.StartUpstream(t, other, "", nil)
	l := start(t, linktest.NewAPI(), keys, u.Port, nil)
	waitReason(t, l, link.PinMismatch)
	time.Sleep(300 * time.Millisecond) // many backoffs' worth
	if u.Requests.Load() != 0 || u.Conns.Load() != 1 {
		t.Fatalf("%d requests, %d connections", u.Requests.Load(), u.Conns.Load())
	}
	if _, err := get(t, l, "/x"); !errors.As(err, new(*link.DownError)) {
		t.Fatalf("request while pin-mismatched: %v", err)
	}
	l.Retry()
	waitFor(t, "an asked-for retry", func() bool { return u.Conns.Load() == 2 })
	waitReason(t, l, link.PinMismatch)
	if u.Requests.Load() != 0 {
		t.Fatal("a request reached the impostor")
	}
}

// PinnedTLS is the only verification: a server with a different key is
// refused, and InsecureSkipVerify never comes without the pin.
func TestPinnedTLS(t *testing.T) {
	keys := linktest.NewKeys(t)
	cfg := link.PinnedTLS(keys.Creds.Identity.Cert, keys.Server.SPKI)
	if !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil || cfg.ClientSessionCache != nil || cfg.MinVersion != 0x0304 {
		t.Fatalf("config %+v", cfg)
	}
	other, _ := peer.LoadOrCreateServer(filepath.Join(t.TempDir(), "peer"), "x")
	for _, c := range []struct {
		pin  string
		cert *peer.Identity
		ok   bool
	}{{keys.Server.SPKI, &keys.Server, true}, {keys.Server.SPKI, &other, false}, {"", &keys.Server, false}} {
		cfg := link.PinnedTLS(keys.Creds.Identity.Cert, c.pin)
		cs := tlsState(c.cert)
		if err := cfg.VerifyConnection(cs); (err == nil) != c.ok {
			t.Errorf("pin %.8s cert %.8s: %v", c.pin, c.cert.SPKI, err)
		}
	}
}

func TestTailscaleDown(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	api := linktest.NewAPI()
	api.Set(func(a *linktest.API) { a.State = "Stopped" })
	l := start(t, api, keys, u.Port, nil)
	waitReason(t, l, link.TailscaleDown)
	api.Set(func(a *linktest.API) { a.State = tailscale.Running; a.StatusErr = errors.New("no socket") })
	time.Sleep(100 * time.Millisecond)
	waitReason(t, l, link.TailscaleDown)
	if u.Conns.Load() != 0 {
		t.Fatal("dialed with tailscale down")
	}
	api.Set(func(a *linktest.API) { a.StatusErr = nil })
	waitReason(t, l, link.Up)
}

func TestNodeOffline(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	api := linktest.NewAPI()
	api.Set(func(a *linktest.API) { a.Online = false })
	l := start(t, api, keys, u.Port, nil)
	if st := waitReason(t, l, link.NodeOffline); !strings.Contains(st.Detail, "last seen") {
		t.Fatalf("detail %q", st.Detail)
	}
	api.Set(func(a *linktest.API) { a.Online = true; a.Missing = true })
	time.Sleep(100 * time.Millisecond)
	waitReason(t, l, link.NodeOffline)
	if u.Conns.Load() != 0 {
		t.Fatal("dialed an offline node")
	}
}

// whois about the address about to be dialed must be the recorded server
// node, untagged, not shared in, our user: else no dial.
func TestWhoIsMismatch(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	lo := netip.MustParseAddr("127.0.0.1")
	for name, mod := range map[string]func(*tailscale.WhoIs){
		"other node": func(w *tailscale.WhoIs) { w.Node.StableID = "nOTHER1CNTRL" },
		"tagged":     func(w *tailscale.WhoIs) { w.Node.Tags = []string{"tag:server"} },
		"shared in":  func(w *tailscale.WhoIs) { w.Node.Sharer = 9 },
		"other user": func(w *tailscale.WhoIs) { w.UserProfile.ID = 9 },
		"not its address": func(w *tailscale.WhoIs) {
			w.Node.Addresses = []netip.Prefix{netip.MustParsePrefix("100.64.0.1/32")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := linktest.NewAPI()
			api.WhoIsFn = func(addr netip.AddrPort) (tailscale.WhoIs, error) {
				w := linktest.ServerWhoIs(lo)
				mod(&w)
				return w, nil
			}
			l := start(t, api, keys, u.Port, nil)
			waitReason(t, l, link.NodeMismatch)
		})
	}
	if u.Conns.Load() != 0 {
		t.Fatal("dialed after a whois refusal")
	}
}

func TestProtocol(t *testing.T) {
	keys := linktest.NewKeys(t)
	for name, hello := range map[string]string{
		"other protocol": `{"protocol":99,"epoch":"ab"}`,
		"bad account":    `{"protocol":1,"epoch":"ab","accounts":[{"name":"-x","email":"a@b"}]}`,
		"bad email":      `{"protocol":1,"epoch":"ab","accounts":[{"name":"x","email":"a b@c"}]}`,
		// Names are shown as text on this machine: anything not plain is
		// refused, never cleaned.
		"bidi name":    `{"protocol":1,"epoch":"ab","accounts":[{"name":"work\u202egro","email":"a@b.c"}]}`,
		"newline name": `{"protocol":1,"epoch":"ab","accounts":[{"name":"a\nb","email":"a@b.c"}]}`,
		"control name": `{"protocol":1,"epoch":"ab","accounts":[{"name":"a\u0007","email":"a@b.c"}]}`,
		"line sep":     `{"protocol":1,"epoch":"ab","accounts":[{"name":"a\u2028b","email":"a@b.c"}]}`,
		"no epoch":     `{"protocol":1}`,
		"not json":     `{"protocol":1,`,
	} {
		t.Run(name, func(t *testing.T) {
			u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
			u.SetHello(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Pneu-Protocol", "1")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(hello))
			})
			l := start(t, linktest.NewAPI(), keys, u.Port, nil)
			waitReason(t, l, link.Protocol)
		})
	}
	t.Run("header", func(t *testing.T) {
		u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
		u.SetHello(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Pneu-Protocol", "2")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"protocol":1,"epoch":"ab"}`))
		})
		l := start(t, linktest.NewAPI(), keys, u.Port, nil)
		waitReason(t, l, link.Protocol)
	})
}

// The server refusing this client's certificate is a TLS alert on the
// first read.
func TestNotPaired(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, strings.Repeat("0", 64), nil)
	l := start(t, linktest.NewAPI(), keys, u.Port, nil)
	waitReason(t, l, link.NotPaired)
	if u.Requests.Load() != 0 {
		t.Fatal("a request got past the server's refusal")
	}
}

func TestRefused(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	u.Close()
	l := start(t, linktest.NewAPI(), keys, u.Port, nil)
	waitReason(t, l, link.Refused)
}

// Backoff doubles to its cap; Retry cuts it short.
func TestBackoffAndRetry(t *testing.T) {
	keys := linktest.NewKeys(t)
	api := linktest.NewAPI()
	api.Set(func(a *linktest.API) { a.Online = false })
	calls := &attempts{}
	l := link.New(countingAPI{api, calls}, keys.Creds, 1)
	fast(l)
	l.Timing.BackoffMin, l.Timing.BackoffMax = 40*time.Millisecond, 160*time.Millisecond
	l.Start()
	defer l.Close()
	time.Sleep(700 * time.Millisecond)
	l.Close()
	at := calls.times()
	// 0, 40, 80, 160, 160, ...: gaps grow, then hold at the cap.
	if len(at) < 4 || len(at) > 8 {
		t.Fatalf("%d attempts in 700ms", len(at))
	}
	if g1, g3 := at[2].Sub(at[1]), at[len(at)-1].Sub(at[len(at)-2]); g3 < g1 || g3 > 250*time.Millisecond {
		t.Fatalf("gaps %v then %v", g1, g3)
	}

	calls2 := &attempts{}
	l2 := link.New(countingAPI{api, calls2}, keys.Creds, 1)
	fast(l2)
	l2.Timing.BackoffMin, l2.Timing.BackoffMax = 10*time.Second, 10*time.Second
	l2.Start()
	defer l2.Close()
	waitFor(t, "the first attempt", func() bool { return l2.State().Reason == link.NodeOffline })
	l2.Retry()
	waitFor(t, "a retry on demand", func() bool { return len(calls2.times()) >= 2 })
}

// The client's lease (N9): a renewal whois that refuses closes the
// connection at once, with the stream it carries.
func TestLeaseRefusalClosesStreams(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(stream))
	api := linktest.NewAPI()
	l := start(t, api, keys, u.Port, func(l *link.Link) {
		l.Timing.Lease, l.Timing.RenewBelow, l.Timing.Sweep = 2*time.Second, 1900*time.Millisecond, 50*time.Millisecond
	})
	waitReason(t, l, link.Up)
	body := openStream(t, l)
	api.Set(func(a *linktest.API) {
		a.WhoIsFn = func(netip.AddrPort) (tailscale.WhoIs, error) {
			w := linktest.ServerWhoIs(netip.MustParseAddr("127.0.0.1"))
			w.Node.Tags = []string{"tag:x"}
			return w, nil
		}
	})
	set := time.Now()
	// At the next sweep, not at the lease's end (nor by a reconnect's
	// pre-dial check after it).
	if st := waitReason(t, l, link.NodeMismatch); time.Since(set) > 500*time.Millisecond || !strings.Contains(st.Detail, "tagged") {
		t.Fatalf("%+v after %v", st, time.Since(set))
	}
	assertStreamDies(t, body)
}

// A lease that can't be renewed (whois doesn't answer) runs out, and the
// connection goes with it.
func TestLeaseExpiry(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(stream))
	api := linktest.NewAPI()
	changes := make(chan link.State, 16)
	l := start(t, api, keys, u.Port, func(l *link.Link) {
		l.Timing.Lease, l.Timing.RenewBelow, l.Timing.Sweep = 400*time.Millisecond, 300*time.Millisecond, 50*time.Millisecond
		l.OnChange = func(st link.State) {
			select {
			case changes <- st:
			default:
			}
		}
	})
	waitReason(t, l, link.Up)
	body := openStream(t, l)
	// Renewals keep it up while whois answers.
	time.Sleep(600 * time.Millisecond)
	if l.State().Reason != link.Up {
		t.Fatalf("renewed lease lapsed: %+v", l.State())
	}
	api.Set(func(a *linktest.API) {
		a.WhoIsFn = func(netip.AddrPort) (tailscale.WhoIs, error) { return tailscale.WhoIs{}, errors.New("timeout") }
	})
	set := time.Now()
	for st := range changes {
		if st.Reason == link.Up {
			continue
		}
		if st.Reason != link.TailscaleDown || !strings.Contains(st.Detail, "lease") || time.Since(set) > time.Second {
			t.Fatalf("%+v after %v", st, time.Since(set))
		}
		break
	}
	assertStreamDies(t, body)
}

// A probe (launch, focus, R) finds a dead connection at once.
func TestProbe(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	l := start(t, linktest.NewAPI(), keys, u.Port, func(l *link.Link) { l.Timing.BackoffMin = time.Minute })
	waitReason(t, l, link.Up)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !l.WaitUp(ctx) {
		t.Fatal("WaitUp while up")
	}
	u.Close()
	l.Retry()
	waitReason(t, l, link.Refused)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if l.WaitUp(ctx2) {
		t.Fatal("WaitUp while down")
	}
}

// stream answers with a byte every 20ms until the client goes.
func stream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	for {
		if _, err := w.Write([]byte("x")); err != nil {
			return
		}
		rc.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func openStream(t *testing.T, l *link.Link) io.ReadCloser {
	t.Helper()
	resp, err := get(t, l, "/stream")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp.Body
}

func assertStreamDies(t *testing.T, body io.Reader) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stream ended cleanly; want it cut")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream still running after the link went down")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// attempts records when StatusPeers was asked: one per attempt.
type attempts struct {
	mu sync.Mutex
	at []time.Time
}

func (a *attempts) times() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.at...)
}

type countingAPI struct {
	*linktest.API
	a *attempts
}

func (c countingAPI) StatusPeers(ctx context.Context) (tailscale.Status, error) {
	c.a.mu.Lock()
	c.a.at = append(c.a.at, time.Now())
	c.a.mu.Unlock()
	return c.API.StatusPeers(ctx)
}

func tlsState(id *peer.Identity) tls.ConnectionState {
	return tls.ConnectionState{PeerCertificates: []*x509.Certificate{id.Cert.Leaf}}
}

// Credentials are checked on every load: the directory 0700, both files
// 0600 and ours, the pin's SPKI its certificate's, no unknown fields.
func TestCreds(t *testing.T) {
	keys := linktest.NewKeys(t)
	pin := filepath.Join(keys.ClientDir, link.PinFile)
	good, _ := os.ReadFile(pin)
	for name, mod := range map[string]func(){
		"pin 0644":      func() { os.Chmod(pin, 0o644) },
		"dir 0755":      func() { os.Chmod(keys.ClientDir, 0o755) },
		"key 0640":      func() { os.Chmod(filepath.Join(keys.ClientDir, peer.ClientKeyFile), 0o640) },
		"unknown field": func() { os.WriteFile(pin, []byte(strings.Replace(string(good), "{", `{"extra":1,`, 1)), 0o600) },
		"spki": func() {
			os.WriteFile(pin, []byte(strings.Replace(string(good), keys.Server.SPKI, strings.Repeat("0", 64), 1)), 0o600)
		},
		"own key": func() {
			os.WriteFile(pin, []byte(strings.Replace(string(good), keys.Server.SPKI, keys.Creds.Identity.SPKI, 1)), 0o600)
		},
		"symlink": func() {
			other := filepath.Join(t.TempDir(), "pin.json")
			os.WriteFile(other, good, 0o600)
			os.Remove(pin)
			os.Symlink(other, pin)
		},
	} {
		t.Run(name, func(t *testing.T) {
			mod()
			if _, err := link.LoadCreds(keys.ClientDir); err == nil {
				t.Fatal("loaded")
			}
			os.Chmod(keys.ClientDir, 0o700)
			os.Chmod(filepath.Join(keys.ClientDir, peer.ClientKeyFile), 0o600)
			os.Remove(pin)
			os.WriteFile(pin, good, 0o600)
			if _, err := link.LoadCreds(keys.ClientDir); err != nil {
				t.Fatalf("restored: %v", err)
			}
		})
	}
	if err := link.RemoveCreds(keys.ClientDir); err != nil || link.Paired(keys.ClientDir) {
		t.Fatalf("remove: %v", err)
	}
}

// C2: the lease runs from the pre-dial whois, not from the end of hello: a
// slow hello doesn't stretch it, and one slower than the lease never
// publishes a session.
func TestLeaseFromWhoisNotHello(t *testing.T) {
	for _, c := range []struct {
		name  string
		hello time.Duration
	}{{"slow hello", 400 * time.Millisecond}, {"hello past the lease", 700 * time.Millisecond}} {
		t.Run(c.name, func(t *testing.T) {
			keys := linktest.NewKeys(t)
			u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
			u.SetHello(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(c.hello)
				linktest.DefaultHello(w, r)
			})
			api := linktest.NewAPI()
			var n atomic.Int32
			lo := netip.MustParseAddr("127.0.0.1")
			api.WhoIsFn = func(netip.AddrPort) (tailscale.WhoIs, error) {
				if n.Add(1) == 1 {
					return linktest.ServerWhoIs(lo), nil
				}
				return tailscale.WhoIs{}, errors.New("timeout") // no renewal ever
			}
			changes := make(chan link.State, 16)
			start(t, api, keys, u.Port, func(l *link.Link) {
				l.Timing.Lease, l.Timing.RenewBelow, l.Timing.Sweep = 600*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
				l.OnChange = func(st link.State) {
					select {
					case changes <- st:
					default:
					}
				}
			})
			first := <-changes
			if c.hello > 600*time.Millisecond {
				if first.Reason != link.TailscaleDown || !strings.Contains(first.Detail, "before hello") {
					t.Fatalf("published a lapsed session: %+v", first)
				}
				return
			}
			if first.Reason != link.Up {
				t.Fatalf("%+v", first)
			}
			down := <-changes
			// Up at ~400ms, down at the lease from the whois (~600ms), not
			// a full lease after hello (~1s).
			if down.Reason != link.TailscaleDown || down.Since.Sub(first.Since) > 400*time.Millisecond {
				t.Fatalf("down %+v, %v after up", down, down.Since.Sub(first.Since))
			}
		})
	}
}

// C3: unlinking forgets the pairing at once: a busy stream is cut, the
// link reports not-paired, requests fail before dialing, and nothing
// reconnects, even on demand.
func TestUnpair(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(stream))
	l := start(t, linktest.NewAPI(), keys, u.Port, nil)
	waitReason(t, l, link.Up)
	body := openStream(t, l)
	l.Unpair()
	if st := l.State(); st.Reason != link.NotPaired {
		t.Fatalf("%+v", st)
	}
	assertStreamDies(t, body)
	var down *link.DownError
	if _, err := get(t, l, "/x"); !errors.As(err, &down) || down.Reason != link.NotPaired {
		t.Fatalf("request after unpair: %v", err)
	}
	conns := u.Conns.Load()
	l.Retry()
	time.Sleep(300 * time.Millisecond)
	if u.Conns.Load() != conns || l.State().Reason != link.NotPaired {
		t.Fatalf("reconnected: %d -> %d, %+v", conns, u.Conns.Load(), l.State())
	}
}

// gate blocks a seam once armed, until released.
type gate struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan struct{}, 4), release: make(chan struct{})} }

func (g *gate) hook() {
	if g.armed.Load() {
		g.entered <- struct{}{}
		<-g.release
	}
}

func unpairAsync(l *link.Link) chan struct{} {
	done := make(chan struct{})
	go func() { l.Unpair(); close(done) }()
	return done
}

func returned(c chan struct{}, within time.Duration) bool {
	select {
	case <-c:
		return true
	case <-time.After(within):
		return false
	}
}

// D2: Unpair between an attempt's hello and its publish closes that
// attempt's connection before returning, and the attempt never publishes.
func TestUnpairRacesPublish(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	l := link.New(linktest.NewAPI(), keys.Creds, u.Port)
	fast(l)
	checked := make(chan string, 1)
	link.SetBeforePublish(l, func() {
		done := unpairAsync(l)
		if !returned(done, 3*time.Second) {
			checked <- "Unpair didn't return"
			return
		}
		if s, c := link.Owned(l); s != 0 || c != 0 {
			checked <- "Unpair returned owning sessions/conns " + strconv.Itoa(s) + "/" + strconv.Itoa(c)
			return
		}
		checked <- ""
	})
	l.Start()
	defer l.Close()
	if msg := <-checked; msg != "" {
		t.Fatal(msg)
	}
	time.Sleep(200 * time.Millisecond)
	if st := l.State(); st.Reason != link.NotPaired {
		t.Fatalf("the attempt published after Unpair: %+v", st)
	}
}

// D2: a session going down (setDown mid-close) is still the link's: Unpair
// waits for that close to finish.
func TestUnpairRacesSetDown(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	l := link.New(linktest.NewAPI(), keys.Creds, u.Port)
	fast(l)
	l.Timing.BackoffMin = time.Hour // no reconnect muddying the count
	g := newGate()
	link.SetBeforeClose(l, g.hook)
	l.Start()
	defer l.Close()
	waitReason(t, l, link.Up)
	g.armed.Store(true)
	go link.GoDown(l)
	<-g.entered
	g.armed.Store(false)
	done := unpairAsync(l)
	if returned(done, 200*time.Millisecond) {
		t.Fatal("Unpair returned while a session was still closing")
	}
	close(g.release)
	if !returned(done, 3*time.Second) {
		t.Fatal("Unpair never returned")
	}
	if s, c := link.Owned(l); s != 0 || c != 0 {
		t.Fatalf("owned %d sessions, %d conns", s, c)
	}
}

// D2: concurrent Unpairs all wait for the one teardown.
func TestConcurrentUnpairs(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	l := link.New(linktest.NewAPI(), keys.Creds, u.Port)
	fast(l)
	g := newGate()
	link.SetBeforeClose(l, g.hook)
	l.Start()
	defer l.Close()
	waitReason(t, l, link.Up)
	g.armed.Store(true)
	a := unpairAsync(l)
	<-g.entered
	g.armed.Store(false)
	b := unpairAsync(l)
	if returned(a, 200*time.Millisecond) || returned(b, 10*time.Millisecond) {
		t.Fatal("an Unpair returned while the session was still closing")
	}
	close(g.release)
	if !returned(a, 3*time.Second) || !returned(b, 3*time.Second) {
		t.Fatal("Unpair never returned")
	}
	if s, c := link.Owned(l); s != 0 || c != 0 {
		t.Fatalf("owned %d sessions, %d conns", s, c)
	}
}

// E2: a socket established while the session closes is closed before
// Unpair returns, even when it hasn't been registered yet.
func TestUnpairWaitsForDial(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	l := link.New(linktest.NewAPI(), keys.Creds, u.Port)
	fast(l)
	dialed := make(chan net.Conn, 1)
	release := make(chan struct{})
	var once sync.Once
	link.SetAfterDial(l, func(nc net.Conn) {
		first := false
		once.Do(func() { first = true })
		if first {
			dialed <- nc
			<-release
		}
	})
	l.Start()
	defer l.Close()
	nc := <-dialed // the attempt's dial: established, not registered
	done := unpairAsync(l)
	if returned(done, 200*time.Millisecond) {
		t.Fatal("Unpair returned with a dial's socket still open")
	}
	close(release)
	if !returned(done, 3*time.Second) {
		t.Fatal("Unpair never returned")
	}
	if _, err := nc.Write([]byte{0}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("the dial's socket is still open: %v", err)
	}
	if st := l.State(); st.Reason != link.NotPaired {
		t.Fatalf("%+v", st)
	}
}

// Stalled blames only the session that carried the stream: one already
// replaced is left alone, the live one goes down.
func TestStalledNamesItsSession(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, http.HandlerFunc(stream))
	l := start(t, linktest.NewAPI(), keys, u.Port, nil)
	waitReason(t, l, link.Up)
	req, _ := http.NewRequest("GET", "https://server/stream", nil)
	resp, a, err := l.RoundTripOn(req)
	if err != nil || a == 0 || a != link.Current(l) {
		t.Fatalf("A: %v %d %d", err, a, link.Current(l))
	}
	resp.Body.Close()
	// A goes down and B replaces it before A's stall is reported.
	link.GoDown(l)
	l.Retry()
	waitReason(t, l, link.Up)
	b := link.Current(l)
	if b == 0 || b == a {
		t.Fatalf("B %d, A %d", b, a)
	}
	l.Stalled(a)
	time.Sleep(100 * time.Millisecond)
	if st := l.State(); st.Reason != link.Up || link.Current(l) != b {
		t.Fatalf("A's stall took B down: %+v, session %d", st, link.Current(l))
	}
	l.Stalled(b)
	if st := l.State(); st.Reason != link.Refused {
		t.Fatalf("B's own stall: %+v", st)
	}
}
