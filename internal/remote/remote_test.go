package remote

import (
	"bufio"
	"strconv"
	"strings"
	"testing"

	"github.com/jmckible/pneu/internal/gmi"
)

const clientJSON = `{"installed":{"client_id":"1-x.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`

// googleURL is the shape lieer prints (run_local_server's auth URL).
const googleURL = "https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=1-x.apps.googleusercontent.com&code_challenge=abc&code_challenge_method=S256&redirect_uri=http%3A%2F%2Flocalhost%3A8080%2F&response_type=code&scope=https%3A%2F%2Fmail.google.com%2F&state=0123abcd"

// The remote commands are fixed text, one per verb; nothing else has one.
func TestCommands(t *testing.T) {
	for v, want := range map[Verb]string{
		Add:    `PATH="$HOME/.local/bin:$PATH" exec pneu account add --stdin`,
		Auth:   `PATH="$HOME/.local/bin:$PATH" exec pneu account auth --stdin`,
		Status: `PATH="$HOME/.local/bin:$PATH" exec pneu account status --stdin`,
	} {
		if got, ok := Command(v); !ok || got != want {
			t.Errorf("%s: %q %v", v, got, ok)
		}
	}
	for _, v := range []Verb{"", "gmi", "add; id", "status --config /tmp/x", "Add", "peer"} {
		if _, ok := Command(v); ok {
			t.Errorf("%q has a remote command", v)
		}
	}
	if !Auth.Consent() || Add.Consent() || Status.Consent() {
		t.Error("only auth runs consent")
	}
}

func TestParseRequests(t *testing.T) {
	secret, _ := gmi.CleanClientSecret([]byte(clientJSON))
	sj := strconv.Quote(string(secret))
	okAdd := []string{
		`{"name":"work","address":"me@work.example","fullName":"","clientSecret":""}`,
		`{"clientSecret":` + sj + `,"fullName":"Pat Doe","address":"me@work.example","name":"work"}` + "\n",
	}
	for _, s := range okAdd {
		if _, err := ParseAdd([]byte(s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	badAdd := []string{
		``, `[]`, `{}`, `null`,
		`{"name":"work","address":"me@work.example","fullName":""}`, // missing
		`{"name":"work","address":"me@work.example","fullName":"","clientSecret":"","x":""}`,
		`{"name":"work","name":"home","address":"me@work.example","fullName":"","clientSecret":""}`,
		`{"Name":"work","address":"me@work.example","fullName":"","clientSecret":""}`,
		`{"name":"work","address":"me@work.example","fullName":"","clientSecret":""} ` + "{}",
		`{"name":"work","address":"me@work.example","fullName":"","clientSecret":""}x`,
		`{"name":"work","address":"me@work.example","fullName":null,"clientSecret":""}`,
		`{"name":"work","address":"me@work.example","fullName":1,"clientSecret":""}`,
		`{"name":"$(id)","address":"me@work.example","fullName":"","clientSecret":""}`,
		`{"name":"work","address":"me@work.example\nx","fullName":"","clientSecret":""}`,
		`{"name":"work","address":"me<me@work.example>","fullName":"","clientSecret":""}`,
		`{"name":"work","address":"me@work.example","fullName":"Pat\n[database]","clientSecret":""}`,
		`{"name":"work","address":"me@work.example","fullName":"Pat‮","clientSecret":""}`,
		`{"name":"work","address":"me@work.example","fullName":"` + strings.Repeat("a", 129) + `","clientSecret":""}`,
		`{"name":"work","address":"me@work.example","fullName":"","clientSecret":"{\"web\":{}}"}`,
		`{"name":"work","address":"me@work.example","fullName":"","clientSecret":"` + strings.Repeat("a", MaxRequest) + `"}`,
	}
	for _, s := range badAdd {
		if _, err := ParseAdd([]byte(s)); err == nil {
			t.Errorf("add accepted %q", s)
		}
	}
	if q, err := ParseAuth([]byte(`{"name":"work","force":true,"consentOpen":"print"}`)); err != nil || !q.Force || q.Name != "work" {
		t.Errorf("auth: %+v %v", q, err)
	}
	for _, s := range []string{
		`{"name":"work","force":"true","consentOpen":"print"}`,
		`{"name":"work","force":false,"consentOpen":"xdg-open"}`,
		`{"name":"work","force":false,"consentOpen":""}`,
		`{"name":"work","force":false}`,
		`{"name":"work","force":false,"consentOpen":"print","consentopen":"print"}`,
		`{"name":"work","force":false,"consentOpen":"print","force":true}`,
		`{"name":"work","FORCE":false,"consentOpen":"print"}`,
		`{"name":"../x","force":false,"consentOpen":"print"}`,
	} {
		if _, err := ParseAuth([]byte(s)); err == nil {
			t.Errorf("auth accepted %s", s)
		}
	}
	for _, s := range []string{`{"name":""}`, `{"name":"work"}`} {
		if _, err := ParseStatus([]byte(s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{`{}`, `{"name":"a b"}`, `{"name":"work","all":true}`, `{"name":"work"}{"name":"x"}`, `{"name":["work"]}`} {
		if _, err := ParseStatus([]byte(s)); err == nil {
			t.Errorf("status accepted %s", s)
		}
	}
	// Oversize is refused before it's parsed.
	big := `{"name":"` + strings.Repeat(" ", MaxRequest) + `"}`
	if _, err := ParseStatus([]byte(big)); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("oversize: %v", err)
	}
}

// The one consent-URL rule, shared by a local consent (lieer's line), the
// server's Reauth, and a client reading its server's event.
func TestValidConsentURL(t *testing.T) {
	if !gmi.ValidConsentURL(googleURL) {
		t.Fatal("Google's own URL refused")
	}
	for _, bad := range []string{
		"", "http://accounts.google.com/o/oauth2/auth",
		"https://accounts.google.com.evil.example/o/oauth2/auth",
		"https://accounts.google.com@evil.example/",
		"https://user:pw@accounts.google.com/o/oauth2/auth",
		"https://accounts.google.com:8443/o/oauth2/auth",
		"javascript:alert(1)//https://accounts.google.com/",
		"file:///etc/passwd", "data:text/html,https://accounts.google.com/",
		"https://evil.example/https://accounts.google.com/",
		"https://accounts.google.com/\\evil.example",
		"https://accounts.google.com/ o",
		"https://accounts.google.com/\no",
		"https://accounts.google.com/\x00",
		"https://accounts.google.com/é",
		"https://accounts.google.com/" + strings.Repeat("a", gmi.MaxConsentURL),
		"HTTPS://ACCOUNTS.GOOGLE.COM/",
	} {
		if gmi.ValidConsentURL(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if u, ok := gmi.ConsentURL("Please visit this URL to authorize this application: " + googleURL); !ok || u != googleURL {
		t.Errorf("lieer's line: %q %v", u, ok)
	}
	if _, ok := gmi.ConsentURL("Please visit this URL to authorize this application: https://user:pw@accounts.google.com/"); ok {
		t.Error("lieer's line with credentials passed")
	}
}

func TestEvents(t *testing.T) {
	for _, e := range []Event{
		{Kind: Progress, Text: "  wrote the notmuch config"},
		{Kind: Waiting},
		{Kind: Consent, URL: googleURL},
		{Kind: Result},
		{Kind: Result, Text: "done"},
		{Kind: Error, Text: "no account \"x\""},
	} {
		line := e.Line()
		if line[len(line)-1] != '\n' {
			t.Fatalf("%+v: no newline", e)
		}
		got, err := ParseEvent(line[:len(line)-1])
		if err != nil || got != e {
			t.Errorf("%+v: %+v %v", e, got, err)
		}
	}
	// The server makes text plain before it's sent.
	if got := string((Event{Kind: Progress, Text: "a\x1b[31mb‮c\nd"}).Line()); got != `{"event":"progress","text":"a[31mbcd"}`+"\n" {
		t.Errorf("server line: %q", got)
	}
}

// What a hostile server can send: refused, or made plain, never raw.
func TestParseEventHostile(t *testing.T) {
	refused := map[string]string{
		"oversize":         `{"event":"progress","text":"` + strings.Repeat("a", MaxLine) + `"}`,
		"long text":        `{"event":"progress","text":"` + strings.Repeat("a", MaxTextBytes+1) + `"}`,
		"unknown event":    `{"event":"prompt","text":"password?"}`,
		"no event":         `{"text":"x"}`,
		"extra field":      `{"event":"progress","text":"x","url":"` + googleURL + `"}`,
		"wrong field":      `{"event":"progress","url":"x"}`,
		"waiting fields":   `{"event":"waiting","text":"x"}`,
		"duplicate":        `{"event":"progress","event":"result","text":"x"}`,
		"dup text":         `{"event":"progress","text":"x","text":"y"}`,
		"case variant":     `{"Event":"progress","text":"x"}`,
		"number":           `{"event":"progress","text":1}`,
		"null":             `{"event":"progress","text":null}`,
		"nested":           `{"event":"progress","text":{"a":"b"}}`,
		"array":            `["progress","x"]`,
		"two objects":      `{"event":"result","text":""}{"event":"result","text":""}`,
		"trailing":         `{"event":"result","text":""} x`,
		"not json":         `progress x`,
		"empty":            ``,
		"proto":            `{"event":"__proto__","text":"x"}`,
		"not google":       `{"event":"consent-url","url":"https://evil.example/o/oauth2/auth"}`,
		"javascript":       `{"event":"consent-url","url":"javascript:alert(1)"}`,
		"file":             `{"event":"consent-url","url":"file:///etc/passwd"}`,
		"credentials":      `{"event":"consent-url","url":"https://me:pw@accounts.google.com/o/oauth2/auth"}`,
		"userinfo trick":   `{"event":"consent-url","url":"https://accounts.google.com@evil.example/"}`,
		"long url":         `{"event":"consent-url","url":"https://accounts.google.com/` + strings.Repeat("a", gmi.MaxConsentURL) + `"}`,
		"newline in url":   `{"event":"consent-url","url":"https://accounts.google.com/\nx"}`,
		"consent no url":   `{"event":"consent-url","text":"https://accounts.google.com/"}`,
		"http":             `{"event":"consent-url","url":"http://accounts.google.com/o/oauth2/auth"}`,
		"bidi in url":      `{"event":"consent-url","url":"https://accounts.google.com/‮"}`,
		"invalid escape":   `{"event":"progress","text":"\x"}`,
		"control raw":      "{\"event\":\"progress\",\"text\":\"a\x1bb\"}",
		"three keys":       `{"event":"error","text":"x","x":"y"}`,
		"event non-string": `{"event":1}`,
	}
	for name, line := range refused {
		if e, err := ParseEvent([]byte(line)); err == nil {
			t.Errorf("%s: accepted as %+v", name, e)
		}
	}
	cleaned := map[string]string{
		`{"event":"progress","text":"a\u001b[2Jb"}`:                      "a[2Jb",
		`{"event":"progress","text":"x‮evil⁦y"}`:                         "xevily",
		`{"event":"error","text":"line1\nline2\r z"}`:                    "line1line2z",
		`{"event":"result","text":"\u0000\u0007ok\u0085"}`:               "ok",
		`{"event":"progress","text":"` + strings.Repeat("é", 600) + `"}`: strings.Repeat("é", MaxText),
	}
	for line, want := range cleaned {
		e, err := ParseEvent([]byte(line))
		if err != nil || e.Text != want {
			t.Errorf("%s: %q %v", line, e.Text, err)
		}
	}
}

const callbackQuery = "state=0123abcd&code=4%2F0Adeu5B&scope=https%3A%2F%2Fmail.google.com%2F+openid&authuser=0&prompt=consent&iss=https%3A%2F%2Faccounts.google.com"

// What may come back from Google's redirect: its keys only, each once,
// printable, a state and one of code or error, bounded.
func TestParseCallback(t *testing.T) {
	for _, ok := range []string{
		callbackQuery,
		"state=s&error=access_denied&error_description=The+user+said+no",
		"state=s&code=c&hd=example.com",
	} {
		if _, err := ParseCallback(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "code=c", "state=s", "state=s&code=c&error=x", "state=&code=c",
		"state=s&code=c&redirect=https://evil.example", "state=s&code=c&State=x",
		"state=s&state=t&code=c", "state=s&code=c&code=d",
		"state=s&code=c%0Ax", "state=s&code=c%00", "state=s&code=%E2%80%AE",
		"state=s&code=c;x=y", "state=s&code=%zz",
		"state=s&code=" + strings.Repeat("a", MaxCallback),
	} {
		if _, err := ParseCallback(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	q, err := ParseCallbackLine(CallbackLine(callbackQuery)[:len(CallbackLine(callbackQuery))-1])
	if err != nil || q.Get("code") != "4/0Adeu5B" || q.Get("scope") != "https://mail.google.com/ openid" {
		t.Errorf("round trip: %v %v", q, err)
	}
	for _, bad := range []string{
		`{"callback":"state=s&code=c","x":""}`, `{"Callback":"state=s&code=c"}`,
		`{"callback":"state=s&code=c"}{}`, `{"callback":1}`, `{}`, `state=s&code=c`,
		`{"callback":"state=s&code=c&evil=1"}`,
		`{"callback":"` + strings.Repeat("a", MaxCallback+64) + `"}`,
	} {
		if _, err := ParseCallbackLine([]byte(bad)); err == nil {
			t.Errorf("line accepted %s", bad)
		}
	}
}

func TestConsentState(t *testing.T) {
	if st, ok := ConsentState(googleURL); !ok || st != "0123abcd" {
		t.Errorf("%q %v", st, ok)
	}
	for _, bad := range []string{
		"https://accounts.google.com/o/oauth2/auth?code=x",
		"https://accounts.google.com/o/oauth2/auth?state=a&state=b",
		"https://evil.example/?state=a",
	} {
		if _, ok := ConsentState(bad); ok {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestReadLine(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("one\n"+strings.Repeat("x", 100)+"\nlast"), 16)
	if l, err := ReadLine(br, 10); string(l) != "one" || err != nil {
		t.Errorf("%q %v", l, err)
	}
	if _, err := ReadLine(br, 10); err != ErrLongLine {
		t.Errorf("long: %v", err)
	}
}
