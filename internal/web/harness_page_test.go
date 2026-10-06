package web

import (
	"bytes"
	"encoding/json"
	"flag"
	"html/template"
	"os"
	"testing"

	"github.com/jmckible/pneu/internal/gmi"
)

var updateHarness = flag.Bool("update-harness", false, "rewrite testdata/hostile/app-thread.html")

// harnessPage is the thread page the hostile harness runs app.js on
// (testdata/hostile/app-thread.html): base.html and thread.html as the
// server renders them, for a two-message thread whose first message has a
// text body with a sign-in link and whose last has an HTML body. It is checked in so the harness needs no server; this test
// fails when the templates change until it is regenerated with
//
//	go test ./internal/web -run TestHarnessThreadPage -update-harness
const harnessPage = "../../testdata/hostile/app-thread.html"

func TestHarnessThreadPage(t *testing.T) {
	pages, err := parsePages()
	if err != nil {
		t.Fatal(err)
	}
	// The #accounts strip as accountsJSON writes it, one ready account.
	acct, err := json.Marshal([]AccountView{{Name: "personal", State: gmi.StateReady, Pulled: true}})
	if err != nil {
		t.Fatal(err)
	}
	data := threadPage{
		Page: Page{
			Origin:   "http://pneu.localhost:7317",
			Title:    "Your pull request was reviewed",
			Accounts: template.HTMLAttr(`data-accounts="` + template.HTMLEscapeString(string(acct)) + `"`),
			Label:    viewLabel{Epoch: "00000000000000e1", Gen: 1},
		},
		Account: "personal",
		Thread:  "00000000000000a1",
		Subject: "Your pull request was reviewed",
		Messages: []messageView{
			{
				ID: "h1@hostile.test", MsgID: "h1@hostile.test", Pos: 1, Class: "message collapsed",
				FromName: "Robin Hale", FromAddr: "robin@example.com", To: "you@example.com",
				Date: "Mon, Sep 28, 9:12 AM", ISO: "2026-09-28T09:12:00Z", Kind: "text",
				Text: RenderText("An earlier message. Sign in with this link:\n\nhttps://login.example/m/1\n\n-- \nRobin https://robin.example", false, false),
			},
			{
				ID: "h2@hostile.test", MsgID: "h2@hostile.test", Pos: 2, Class: "message collapsed",
				FromName: "GitHub", FromAddr: "notifications@github.com", To: "you@example.com",
				Date: "Mon, Sep 28, 10:40 AM", ISO: "2026-09-28T10:40:00Z", Kind: "html",
				BodyURL: "/body/personal/h2@hostile.test",
			},
		},
		Total:  2,
		MsgIDs: "h1%40hostile.test h2%40hostile.test",
	}
	var b bytes.Buffer
	if err := pages["thread"].ExecuteTemplate(&b, "base", data); err != nil {
		t.Fatal(err)
	}
	if *updateHarness {
		if err := os.WriteFile(harnessPage, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(harnessPage)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, b.Bytes()) {
		t.Fatalf("%s is stale: the templates changed; rerun with -update-harness", harnessPage)
	}
}
