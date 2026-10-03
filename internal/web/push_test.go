package web

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/control"
)

// D7: push health rides the account view (data-accounts, SSE `account`,
// hello) and status.json; absent when off, reason only with reauth or
// failing. A push change alone rewrites the status file and sends
// `account`; it never makes an account sick.
func TestPushSurfaces(t *testing.T) {
	f := newTagFixture(t)
	var mu sync.Mutex
	push := map[string]control.PushState{
		"personal": {State: control.PushDelivering, LastDelivery: time.Date(2026, 10, 3, 12, 0, 0, 5e8, time.UTC)},
	}
	f.s.Push = func(a string) control.PushState {
		mu.Lock()
		defer mu.Unlock()
		if p, ok := push[a]; ok {
			return p
		}
		return control.PushState{State: control.PushOff}
	}
	setPush := func(a string, p control.PushState) {
		mu.Lock()
		push[a] = p
		mu.Unlock()
	}

	_, body := get(t, f.s, "/")
	m := regexp.MustCompile(`<div id="accounts" data-accounts="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no data-accounts")
	}
	raw := html.UnescapeString(m[1])
	var views []AccountView
	if err := json.Unmarshal([]byte(raw), &views); err != nil {
		t.Fatal(err)
	}
	if p := views[0].Push; p == nil || p.State != control.PushDelivering || p.Reason != "" || p.LastDelivery == nil || *p.LastDelivery != "2026-10-03T12:00:00Z" {
		t.Fatalf("personal's push: %s", raw)
	}
	if views[1].Push != nil || strings.Count(raw, `"push"`) != 1 || strings.Contains(raw, `"reason"`) {
		t.Fatalf("an off account carries push, or a reason without failing: %s", raw)
	}

	doc := f.s.statusSnapshot(true)
	if p := doc.Accounts[0].Push; p == nil || p.State != control.PushDelivering || *p.LastDelivery != "2026-10-03T12:00:00Z" {
		t.Fatalf("status file, personal: %+v", doc.Accounts[0])
	}
	if doc.Accounts[1].Push != nil {
		t.Fatalf("status file, work: %+v", doc.Accounts[1].Push)
	}

	drain := func() bool {
		select {
		case <-f.s.statusNow:
			return true
		default:
			return false
		}
	}
	c := f.s.Hub.Subscribe()
	defer f.s.Hub.unsubscribe(c)
	f.s.AccountChanged("personal")
	drain()
	nextEvent(t, c, "account")
	f.s.AccountChanged("personal")
	if drain() {
		t.Fatal("rewrote the status file with nothing changed")
	}
	setPush("personal", control.PushState{State: control.PushDelivering, LastDelivery: time.Date(2026, 10, 3, 12, 5, 0, 0, time.UTC)})
	f.s.AccountChanged("personal")
	if drain() {
		t.Fatal("rewrote the status file for a delivery alone (its sync does)")
	}
	setPush("personal", control.PushState{State: control.PushFailing, Reason: control.ReasonNetwork})
	f.s.AccountChanged("personal")
	if !drain() {
		t.Fatal("a push state change didn't rewrite the status file")
	}
	var ev []byte
	for range 3 { // nothing changed, a delivery, failing: each an `account`
		ev = nextEvent(t, c, "account")
	}
	if !strings.Contains(string(ev), `"push":{"state":"failing","reason":"network"}`) {
		t.Fatalf("account event: %s", ev)
	}
	setPush("personal", control.PushState{State: control.PushFailing, Reason: control.ReasonPermission})
	f.s.AccountChanged("personal")
	if !drain() {
		t.Fatal("a reason change didn't rewrite the status file")
	}

	doc = f.s.statusSnapshot(true)
	if Sick(doc.Accounts[0]) || len(FailingAccounts(doc.Accounts)) != 1 {
		t.Fatalf("push failing made an account sick: %+v", doc.Accounts)
	}
	_, h := f.s.subscribe()
	if p := h.Accounts[0].Push; p == nil || p.State != control.PushFailing || p.Reason != control.ReasonPermission {
		t.Fatalf("hello: %+v", h.Accounts[0])
	}

	setPush("personal", control.PushState{State: control.PushOff})
	f.s.AccountChanged("personal")
	if !drain() {
		t.Fatal("push going off didn't rewrite the status file")
	}
	f.s.Push = nil
	if v := f.s.accountViews(); v[0].Push != nil {
		t.Fatal("push without a manager")
	}
}

// The account view carries the engine's poll delay (PollEvery, seconds),
// so the sync details' polling note reads the real one: the period,
// longer while an account fails. status.json doesn't carry it.
func TestPollEvery(t *testing.T) {
	f := newTagFixture(t)
	_, body := get(t, f.s, "/")
	m := regexp.MustCompile(`<div id="accounts" data-accounts="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no data-accounts")
	}
	var views []AccountView
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &views); err != nil {
		t.Fatal(err)
	}
	if views[0].PollEvery != 30 || views[1].PollEvery != 120 {
		t.Fatalf("poll delays %d, %d; want 30 and 120 (two failures)", views[0].PollEvery, views[1].PollEvery)
	}
	c := f.s.Hub.Subscribe()
	defer f.s.Hub.unsubscribe(c)
	f.s.AccountChanged("work")
	if ev := nextEvent(t, c, "account"); !strings.Contains(string(ev), `"pollEvery":120`) {
		t.Fatalf("account event: %s", ev)
	}
	if _, h := f.s.subscribe(); h.Accounts[0].PollEvery != 30 {
		t.Fatalf("hello: %+v", h.Accounts[0])
	}
	b, _ := json.Marshal(f.s.statusSnapshot(true))
	if strings.Contains(string(b), "pollEvery") {
		t.Fatalf("status.json carries the poll delay: %s", b)
	}
}
