package client

import (
	"bytes"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/web"
)

var navigate = map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}

// Every reason has its page: local words, the SSH target, the bar-menu
// action and the command where one fixes it. pin-mismatch never offers to
// trust the new key, and doesn't claim to be retrying.
func TestExplain(t *testing.T) {
	want := map[link.Reason][]string{
		link.Starting:      {"Connecting to dell"},
		link.TailscaleDown: {"Tailscale is off", "sudo tailscale up", "Fix with agent"},
		link.NodeOffline:   {"dell is offline", "Fix with agent"},
		link.NodeMismatch:  {"another machine", "Fix with agent"},
		link.Refused:       {"isn't answering", "ssh -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlPath=none -o PermitLocalCommand=no dell systemctl --user status pneu", "Fix with agent"},
		link.PinMismatch:   {"identity changed", "pneu client unpair", "pneu client pair dell", "Fix with agent", "Check again", "Not retrying"},
		link.NotPaired:     {"doesn't know this machine", "pneu client unpair", "pneu client pair dell"},
		link.Protocol:      {"different versions", "Update pneu", "Update dell"},
	}
	for reason, words := range want {
		var b bytes.Buffer
		if err := errorTmpl.Execute(&b, explain(link.State{Reason: reason, Since: time.Now()}, "dell")); err != nil {
			t.Fatal(err)
		}
		page := b.String()
		for _, w := range append(words, `data-reason="`+string(reason)+`"`, "/client/static/error.js", "/theme.css") {
			if !strings.Contains(html.UnescapeString(page), w) {
				t.Errorf("%s: no %q in\n%s", reason, w, page)
			}
		}
		if strings.Contains(strings.ToLower(page), "trust") || strings.Contains(strings.ToLower(page), "anyway") {
			t.Errorf("%s offers to trust:\n%s", reason, page)
		}
		if retrying := strings.Contains(page, "Retrying on its own"); retrying == (reason == link.PinMismatch) {
			t.Errorf("%s: retrying %v", reason, retrying)
		}
	}
	// The target is the user's own text, and still escaped.
	var b bytes.Buffer
	errorTmpl.Execute(&b, explain(link.State{Reason: link.Refused}, `x"<b>`))
	if strings.Contains(b.String(), `x"<b>`) || !strings.Contains(b.String(), "x&#34;&lt;b&gt;") {
		t.Fatalf("unescaped:\n%s", b.String())
	}
}

// While the link isn't up a page navigation gets the error page, under
// the route's HTML class and saying not-sent; a script's fetch of the same
// route gets the plain not-sent answer it always did.
func TestErrorPage(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	api := linktest.NewAPI()
	api.Set(func(a *linktest.API) { a.State = "Stopped" })
	r := newRig(t, keys, u.Port, api)
	waitState(t, r.link, link.TailscaleDown)

	resp, body := r.get("/", navigate)
	if resp.StatusCode != 503 || resp.Header.Get(LinkHeader) != NotSent || resp.Header.Get(web.PolicyHeader) != "app" ||
		resp.Header.Get("Content-Security-Policy") != web.AppCSP || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(body, "Tailscale is off") || !strings.Contains(body, "sudo tailscale up") {
		t.Fatalf("GET / navigation: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}
	if resp, body := r.get("/t/personal/abc", navigate); resp.StatusCode != 503 || !strings.Contains(body, "Tailscale is off") {
		t.Fatalf("thread navigation: %d", resp.StatusCode)
	}
	if resp, _ := r.get("/compose", navigate); resp.StatusCode != 503 || resp.Header.Get(web.PolicyHeader) != "compose" {
		t.Fatalf("compose navigation: %d %v", resp.StatusCode, resp.Header)
	}
	for _, c := range []struct {
		path string
		hdr  map[string]string
	}{
		{"/", nil},
		{"/", map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}},
		{"/t/personal/abc", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}},
		{"/static/app.css", navigate},
		{"/status", navigate},
	} {
		resp, body := r.get(c.path, c.hdr)
		if resp.StatusCode != 502 || resp.Header.Get(LinkHeader) != NotSent || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || strings.Contains(body, "<html") {
			t.Errorf("%s %v: %d %v %q", c.path, c.hdr, resp.StatusCode, resp.Header, body)
		}
	}
	if u.Requests.Load() != 0 {
		t.Fatal("something went upstream")
	}

	// Back up: the same navigation is the server's page.
	api.Set(func(a *linktest.API) { a.State = "Running" })
	r.link.Retry()
	r.waitUp()
	u.SetHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer(w, "app", "text/html; charset=utf-8", 200, "<!doctype html><p>inbox")
	}))
	if resp, body := r.get("/", navigate); resp.StatusCode != 200 || body != "<!doctype html><p>inbox" {
		t.Fatalf("up: %d %q", resp.StatusCode, body)
	}
}

// A server answering with another key: the page says so and offers no way
// past it.
func TestErrorPagePinMismatch(t *testing.T) {
	keys := linktest.NewKeys(t)
	other := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, other.Server, keys.Creds.Identity.SPKI, nil)
	r := newRig(t, keys, u.Port, nil)
	waitState(t, r.link, link.PinMismatch)
	resp, body := r.get("/", navigate)
	if resp.StatusCode != 503 || !strings.Contains(body, "identity changed") || strings.Contains(strings.ToLower(body), "trust") ||
		!strings.Contains(body, "Check again") || !strings.Contains(body, `data-reason="pin-mismatch"`) {
		t.Fatalf("%d\n%s", resp.StatusCode, body)
	}
}

// The compose form's POST /send that the link fails gets a page, under the
// compose class: not-sent says nothing left and the draft is kept;
// unknown says check Sent and that a resend won't send twice. A script's
// POST /send (no navigation) still gets the plain answer.
func TestSendPage(t *testing.T) {
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, nil)
	api := linktest.NewAPI()
	api.Set(func(a *linktest.API) { a.State = "Stopped" })
	r := newRig(t, keys, u.Port, api)
	waitState(t, r.link, link.TailscaleDown)

	form := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	resp, body := r.do(r.req("POST", "/send", "message_id=x", form))
	if resp.StatusCode != 503 || resp.Header.Get(LinkHeader) != NotSent || resp.Header.Get(web.PolicyHeader) != "compose" ||
		!strings.Contains(body, "Not sent: can&#39;t reach dell") || !strings.Contains(body, "draft is kept") ||
		!strings.Contains(body, `data-kind="send"`) || !strings.Contains(body, "Back to the draft") {
		t.Fatalf("not-sent: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}
	if resp, body := r.do(r.req("POST", "/send", "message_id=x", nil)); resp.StatusCode != 502 || strings.Contains(body, "<html") {
		t.Errorf("a fetch: %d %q", resp.StatusCode, body)
	}
	if u.Requests.Load() != 0 {
		t.Fatal("something went upstream")
	}

	// Up, and the server never answers the send: unknown.
	api.Set(func(a *linktest.API) { a.State = "Running" })
	r.link.Retry()
	r.waitUp()
	u.SetHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler) // the stream dies after the request went up
	}))
	resp, body = r.do(r.req("POST", "/send", "message_id=x", form))
	if resp.Header.Get(LinkHeader) != Unknown || resp.Header.Get(web.PolicyHeader) != "compose" ||
		!strings.Contains(body, "may have been sent") || !strings.Contains(body, "won&#39;t send it twice") {
		t.Fatalf("unknown: %d %v\n%s", resp.StatusCode, resp.Header, body)
	}
}

func TestExplainSend(t *testing.T) {
	for _, o := range []string{NotSent, Unknown} {
		var b bytes.Buffer
		if err := sendTmpl.Execute(&b, explainSend(o, `x"<b>`)); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), `x"<b>`) || strings.Contains(b.String(), "retry") {
			t.Errorf("%s:\n%s", o, b.String())
		}
	}
}
