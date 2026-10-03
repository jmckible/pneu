package client

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/link/linktest"
	"github.com/jmckible/pneu/internal/web"
)

// D7: push health, field by field. Off is absent; state from the closed
// list but never "off"; a reason exactly when reauth or failing, from
// that state's list; lastDelivery RFC 3339 UTC to the second, within
// bounds. Anything else drops push and nothing more.
func TestCleanPush(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ raw, want string }{
		{``, `null`},
		{`null`, `null`},
		{`{"state":"delivering","lastDelivery":"2026-10-03T11:58:00Z"}`, `{"state":"delivering","lastDelivery":"2026-10-03T11:58:00Z"}`},
		{`{"state":"quiet"}`, `{"state":"quiet"}`},
		{`{"state":"reauth","reason":"owner-reauth"}`, `{"state":"reauth","reason":"owner-reauth"}`},
		{`{"state":"reauth","reason":"mailbox-reauth"}`, `{"state":"reauth","reason":"mailbox-reauth"}`},
		{`{"state":"failing","reason":"api-disabled","x":"<b>"}`, `{"state":"failing","reason":"api-disabled"}`},
		{`{"state":"failing","reason":"watch-expired","lastDelivery":"2026-10-02T00:00:00Z"}`, `{"state":"failing","reason":"watch-expired","lastDelivery":"2026-10-02T00:00:00Z"}`},
		{`{"state":"delivering","lastDelivery":"2026-10-04T11:00:00Z"}`, `{"state":"delivering","lastDelivery":"2026-10-04T11:00:00Z"}`}, // a server clock ahead

		// Dropped.
		{`{"state":"starting","reason":null}`, `null`},
		{`{"state":"quiet","reason":""}`, `null`},
		{`{"state":"delivering","lastDelivery":null}`, `null`},
		{`{"state":"failing","reason":null}`, `null`},
		{`{"state":null}`, `null`},
		{`{"state":"off"}`, `null`},
		{`{"state":""}`, `null`},
		{`{}`, `null`},
		{`{"state":"Delivering"}`, `null`},
		{`{"state":"<img src=x onerror=alert(1)>"}`, `null`},
		{`{"state":"delivering","reason":"network"}`, `null`},
		{`{"state":"quiet","reason":"unknown"}`, `null`},
		{`{"state":"reauth"}`, `null`},
		{`{"state":"reauth","reason":"network"}`, `null`},
		{`{"state":"failing"}`, `null`},
		{`{"state":"failing","reason":""}`, `null`},
		{`{"state":"failing","reason":"owner-reauth"}`, `null`},
		{`{"state":"failing","reason":"Google says: <b>no</b>"}`, `null`},
		{`{"state":"delivering","lastDelivery":"2026-10-03T11:58:00+02:00"}`, `null`},
		{`{"state":"delivering","lastDelivery":"2026-10-03T11:58:00.5Z"}`, `null`},
		{`{"state":"delivering","lastDelivery":"2026-10-03t11:58:00z"}`, `null`},
		{`{"state":"delivering","lastDelivery":"2026-13-03T11:58:00Z"}`, `null`},
		{`{"state":"delivering","lastDelivery":"yesterday"}`, `null`},
		{`{"state":"delivering","lastDelivery":""}`, `null`},
		{`{"state":"delivering","lastDelivery":"0000-01-01T00:00:00Z"}`, `null`},
		{`{"state":"delivering","lastDelivery":"1999-12-31T23:59:59Z"}`, `null`},
		{`{"state":"delivering","lastDelivery":"2026-10-04T12:00:01Z"}`, `null`}, // over a day ahead
		{`{"state":"delivering","lastDelivery":"9999-12-31T23:59:59Z"}`, `null`},
		{`{"state":"delivering","lastDelivery":1759492680}`, `null`},
		{`{"state":7}`, `null`},
		{`{"state":["delivering"]}`, `null`},
		{`"delivering"`, `null`},
		{`true`, `null`},
		{`[]`, `null`},
		{`{"state":"delivering"`, `null`},
	} {
		got, _ := json.Marshal(cleanPush(json.RawMessage(c.raw), now))
		if string(got) != c.want {
			t.Errorf("%s: got %s, want %s", c.raw, got, c.want)
		}
	}
}

func TestCleanPoll(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int
	}{
		{``, 0}, {`null`, 0}, {`30`, 30}, {`1`, 1}, {`900`, 900}, {`86400`, 86400},
		{`0`, 0}, {`-30`, 0}, {`86401`, 0}, {`30.5`, 0}, {`1e2`, 0}, {`"30"`, 0},
		{`99999999999999999999`, 0}, {`{}`, 0}, {`true`, 0},
	} {
		if got := cleanPoll(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("%s: got %d, want %d", c.raw, got, c.want)
		}
	}
}

// Over the wire: hello's accounts, `account` and `status` each carry a
// valid push and pollEvery on; a bad one of either, of any type, drops
// that field for that account only, and the event, the other accounts'
// push and status.json all still go through.
func TestPushOverTheWire(t *testing.T) {
	good := `"push":{"state":"failing","reason":"network","lastDelivery":"2026-10-03T11:58:00Z"}`
	acct := func(push, poll string) string {
		return `{"name":"personal","state":"ready","pulled":true,"failures":0,"error":null,"authing":false,"progress":null,` +
			`"lastSync":"2026-09-30T10:00:00Z","queued":false,"running":false,` + push + `,"pollEvery":` + poll + `}`
	}
	hello := block("hello", `{"epoch":"abcd","gen":3,"accounts":[`+acct(good, "30")+`],"status":`+
		`{"version":1,"updated":"x","running":true,"unread":1,"senders":[],"accounts":[{"name":"personal","unread":1,"state":"ready",`+good+`}]}}`)
	release := make(chan struct{})
	r, _, _ := scriptRig(t, newScript(release,
		hello,
		sleep(1500*time.Millisecond), // hello's status written before the next
		block("account", acct(`"push":"<script>alert(1)</script>"`, `"<b>"`)),
		block("account", acct(`"push":{"state":"failing","reason":"<b>Google's words</b>"}`, `1e9`)),
		block("account", acct(`"push":{"state":"reauth","reason":"mailbox-reauth"}`, `120`)),
		statusBlock(2, `[]`, `[{"name":"personal","unread":2,"state":"ready","push":{"state":"off"}}]`),
		statusBlock(3, `[]`, `[{"name":"personal","unread":3,"state":"ready","push":{"state":"reauth","reason":"owner-reauth"}}]`),
	))
	p := r.page()
	close(release)
	h := p.handshaken()
	if a := h.Accounts[0]; a.Push == nil || a.Push.State != "failing" || a.Push.Reason != "network" || *a.Push.LastDelivery != "2026-10-03T11:58:00Z" || a.PollEvery != 30 {
		t.Fatalf("hello's account: %+v", a)
	}
	doc := waitV2(t, r.status, "hello's status", func(d StatusV2) bool { return d.Unread == 1 })
	if p := doc.Accounts[0].Push; p == nil || p.State != "failing" || p.Reason != "network" || *p.LastDelivery != "2026-10-03T11:58:00Z" {
		t.Fatalf("status.json from hello: %+v", doc.Accounts[0])
	}

	for i, want := range []string{"", "", `"push":{"state":"reauth","reason":"mailbox-reauth"}`} {
		ev := p.until("account", ownLinkUp)
		var av web.AccountView
		if err := json.Unmarshal(ev.data, &av); err != nil || av.Name != "personal" || av.LastSync == nil {
			t.Fatalf("account %d dropped or mangled: %s", i, ev.data)
		}
		s := string(ev.data)
		if strings.Contains(s, "<") || strings.Contains(s, `<`) || strings.Contains(s, "Google") {
			t.Errorf("account %d passed upstream text: %s", i, s)
		}
		if want == "" && (strings.Contains(s, `"push"`) || strings.Contains(s, "pollEvery")) {
			t.Errorf("account %d kept a bad push or pollEvery: %s", i, s)
		}
		if want != "" && (!strings.Contains(s, want) || av.PollEvery != 120) {
			t.Errorf("account %d: %s", i, s)
		}
	}
	doc = waitV2(t, r.status, "the last status", func(d StatusV2) bool { return d.Unread == 3 })
	if p := doc.Accounts[0].Push; p == nil || p.State != "reauth" || p.Reason != "owner-reauth" || p.LastDelivery != nil {
		t.Fatalf("status.json: %+v", doc.Accounts[0])
	}
	// The daemon's own state took the last view: a later page's hello.
	if a := r.page().handshaken().Accounts[0]; a.Push == nil || a.Push.Reason != "mailbox-reauth" || a.PollEvery != 120 {
		t.Fatalf("later hello: %+v", a)
	}
}

// A status whose push is off-shape still writes, without push.
func TestStatusPushDropped(t *testing.T) {
	r, _, _ := scriptRig(t, newScript(nil, helloBlock(""),
		statusBlock(5, `[]`, `[{"name":"personal","unread":5,"state":"ready","push":{"state":"failing","reason":"rm -rf /"}}]`)))
	waitV2(t, r.status, "the status", func(d StatusV2) bool { return d.Unread == 5 })
	doc, raw := readV2(t, r.status)
	if doc.Accounts[0].Push != nil {
		t.Fatalf("push kept: %+v", doc.Accounts[0])
	}
	if a := raw["accounts"].([]any)[0].(map[string]any); a["push"] != nil {
		t.Fatalf("status.json has push: %v", a)
	}
}

// Two accounts, one push off-shape in each of hello's accounts, `status`
// and `account` (hello's own status goes through the same cleanStatus): the bad one loses push alone, its
// sibling's reaches the page and status.json, and nothing is dropped.
func TestPushPerAccount(t *testing.T) {
	good := `{"state":"delivering","lastDelivery":"2026-10-03T11:58:00Z"}`
	bad := `{"state":"quiet","reason":"<b>x</b>"}`
	view := func(name, push string) string {
		return `{"name":"` + name + `","state":"ready","pulled":true,"failures":0,"error":null,"authing":false,"progress":null,` +
			`"lastSync":"2026-09-30T10:00:00Z","queued":false,"running":false,"push":` + push + `,"pollEvery":30}`
	}
	status := func(unread int, pPush, wPush string) string {
		return statusBlock(unread, `[]`, `[{"name":"personal","unread":1,"state":"ready","push":`+pPush+`},{"name":"work","unread":0,"state":"ready","push":`+wPush+`}]`)
	}
	keys := linktest.NewKeys(t)
	u := linktest.StartUpstream(t, keys.Server, keys.Creds.Identity.SPKI, newScript(nil,
		block("hello", `{"epoch":"abcd","gen":3,"accounts":[`+view("personal", bad)+`,`+view("work", good)+`],"status":null}`),
		status(1, bad, good),
		sleep(1500*time.Millisecond),
		block("account", view("work", bad)),
		block("account", view("personal", good)),
		status(2, good, `"delivering"`),
	))
	u.SetHello(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(web.ProtocolHeader, strconv.Itoa(web.Protocol))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"protocol":` + strconv.Itoa(web.Protocol) + `,"name":"server","revision":"0123456789abcdef0123456789abcdef01234567","modified":false,"epoch":"abcd","gen":3,` +
			`"accounts":[{"name":"personal","email":"me@example.com"},{"name":"work","email":"me@work.example"}]}`))
	})
	r := newRig(t, keys, u.Port, nil)
	r.waitUp()
	p := r.page()
	h := p.handshaken()
	if len(h.Accounts) != 2 || h.Accounts[0].Push != nil || h.Accounts[0].PollEvery != 30 || h.Accounts[1].Push == nil || h.Accounts[1].Push.State != "delivering" {
		t.Fatalf("hello: %+v", h.Accounts)
	}
	doc := waitV2(t, r.status, "the first status", func(d StatusV2) bool { return d.Unread == 1 })
	if len(doc.Accounts) != 2 || doc.Accounts[0].Push != nil || doc.Accounts[1].Push == nil || *doc.Accounts[1].Push.LastDelivery != "2026-10-03T11:58:00Z" {
		t.Fatalf("status.json: %+v", doc.Accounts)
	}
	for _, want := range []struct {
		name string
		push bool
	}{{"work", false}, {"personal", true}} {
		var av web.AccountView
		ev := p.until("account", func(ev sseEvent) bool { return ownLinkUp(ev) || ev.name == "status" })
		if json.Unmarshal(ev.data, &av) != nil || av.Name != want.name || (av.Push != nil) != want.push || av.PollEvery != 30 {
			t.Fatalf("account %s: %s", want.name, ev.data)
		}
	}
	doc = waitV2(t, r.status, "the second status", func(d StatusV2) bool { return d.Unread == 2 })
	if doc.Accounts[0].Push == nil || doc.Accounts[1].Push != nil {
		t.Fatalf("status.json: %+v", doc.Accounts)
	}
	if a := r.page().handshaken().Accounts; a[0].Push == nil || a[1].Push != nil {
		t.Fatalf("later hello: %+v", a)
	}
}
