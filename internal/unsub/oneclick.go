package unsub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Result categories: the only thing about a one-click POST that is logged
// or returned. Never the URL, the host, or a Go error (a *url.Error
// carries the full URL).
const (
	CatOK             = "ok"
	CatRefusedAddress = "refused-address"
	CatRefusedPort    = "refused-port" // https on a port other than 443
	CatTimeout        = "timeout"
	CatTLS            = "tls"
	CatDNS            = "dns"
	CatConnect        = "connect"  // refused, reset, unreachable
	CatProtocol       = "protocol" // oversized or malformed response
	CatBusy           = "busy"     // two already in flight
)

// Refused reports whether a category is a security refusal: the dialog
// says so and offers nothing else.
func Refused(cat string) bool { return cat == CatRefusedAddress || cat == CatRefusedPort }

// Transport reports whether a failure category may offer the next item:
// the destination was fine to try, it just didn't work.
func Transport(cat string) bool {
	switch cat {
	case CatTimeout, CatTLS, CatDNS, CatConnect, CatProtocol:
		return true
	}
	return strings.HasPrefix(cat, "http-")
}

const (
	UserAgent     = "pneu (RFC 8058 one-click unsubscribe)"
	maxRespHeader = 16 << 10
	maxRespBody   = 64 << 10
	maxOneClicks  = 2
)

// oneClickWait is the one deadline over a POST; a var only so a test can
// wait out a shorter one.
var oneClickWait = 10 * time.Second

// special is IANA's IPv4 special-purpose registry, plus multicast and
// reserved: nothing here is a mail sender's web server.
var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

// PublicIPv4 is the production address policy: a global unicast IPv4
// address outside every special-purpose range. IPv6 is never used: a
// network's NAT64 prefix can make a public-looking IPv6 address reach a
// private IPv4 one.
func PublicIPv4(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range special {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Client makes one-click POSTs. The zero value is not ready; use
// NewClient. Tests replace Resolve, Dial, Allowed and TLS.
type Client struct {
	// Resolve returns the host's A records.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial connects to the pinned address ("ip:443"), network "tcp4".
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Allowed vets every resolved address; PublicIPv4 in production.
	Allowed func(netip.Addr) bool
	// TLS is cloned per request; its ServerName is always the URL's host.
	TLS *tls.Config

	slots chan struct{}
}

func NewClient() *Client {
	d := &net.Dialer{}
	return &Client{
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		},
		Dial:    d.DialContext,
		Allowed: PublicIPv4,
		slots:   make(chan struct{}, maxOneClicks),
	}
}

// OneClick POSTs List-Unsubscribe=One-Click to rawURL and returns the
// result category. One deadline covers the wait for a slot (at most
// maxOneClicks run at once), DNS, connect, TLS, headers and body.
func (c *Client) OneClick(ctx context.Context, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || strings.ToLower(u.Scheme) != "https" || !webURLOK(u) {
		return CatRefusedAddress
	}
	if p := u.Port(); p != "" && p != "443" {
		return CatRefusedPort
	}
	host := u.Hostname()

	ctx, cancel := context.WithTimeout(ctx, oneClickWait)
	defer cancel()
	if c.slots == nil {
		c.slots = make(chan struct{}, maxOneClicks)
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return CatBusy
	}
	defer func() { <-c.slots }()

	addrs, err := c.Resolve(ctx, host)
	if err != nil {
		if ctx.Err() != nil {
			return CatTimeout
		}
		return CatDNS
	}
	if len(addrs) == 0 {
		return CatDNS
	}
	allowed := c.Allowed
	if allowed == nil {
		allowed = PublicIPv4
	}
	for _, a := range addrs {
		if a.Unmap().Is6() || !allowed(a) {
			return CatRefusedAddress
		}
	}
	pinned := netip.AddrPortFrom(addrs[0].Unmap(), 443).String()

	var tc *tls.Config
	if c.TLS != nil {
		tc = c.TLS.Clone()
	} else {
		tc = &tls.Config{}
	}
	tc.ServerName = host
	tc.MinVersion = tls.VersionTLS12
	tr := &http.Transport{
		Proxy: nil,
		// The name is ignored: DNS was answered once, above, and vetted.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return c.Dial(ctx, "tcp4", pinned)
		},
		TLSClientConfig:        tc,
		MaxResponseHeaderBytes: maxRespHeader,
		DisableKeepAlives:      true,
		DisableCompression:     true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(OneClickArg))
	if err != nil {
		return CatRefusedAddress
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return errCategory(ctx, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody))
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return CatOK
	}
	return "http-" + strconv.Itoa(resp.StatusCode/100) + "xx"
}

// errCategory names a transport error's category without keeping the error.
func errCategory(ctx context.Context, err error) string {
	var ne net.Error
	switch {
	case ctx.Err() != nil, errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return CatTimeout
	}
	var (
		cv  *tls.CertificateVerificationError
		ua  x509.UnknownAuthorityError
		he  x509.HostnameError
		ci  x509.CertificateInvalidError
		rh  tls.RecordHeaderError
		al  tls.AlertError
		ech *tls.ECHRejectionError
	)
	if errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci) ||
		errors.As(err, &rh) || errors.As(err, &al) || errors.As(err, &ech) || strings.Contains(err.Error(), "tls: ") {
		return CatTLS
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return CatConnect
	}
	return CatProtocol
}
