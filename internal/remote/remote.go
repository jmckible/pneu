// Package remote is the account protocol a client speaks to its server
// over SSH (docs/client.md, "Server work from a client"; as built: step 8).
// The client runs one fixed remote command per verb, `pneu account <verb>
// --stdin`, sends one JSON object of parameters on stdin, and reads
// line-delimited JSON events back from stdout. Both directions are parsed
// by token, with exact keys, bounds and shapes; text meant for a person
// travels only in an event's display field and is made plain on both ends.
package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/gmi"
)

// Verb is one of the account commands a client forwards.
type Verb string

const (
	Add    Verb = "add"
	Auth   Verb = "auth"
	Status Verb = "status"
)

// commands are the remote commands, one fixed string per verb, never built
// from anything a user typed: SSH joins its arguments into a line the
// remote shell parses. The PATH prefix is pairing's (a non-interactive
// session reads no login profile).
var commands = map[Verb]string{
	Add:    `PATH="$HOME/.local/bin:$PATH" exec pneu account add --stdin`,
	Auth:   `PATH="$HOME/.local/bin:$PATH" exec pneu account auth --stdin`,
	Status: `PATH="$HOME/.local/bin:$PATH" exec pneu account status --stdin`,
}

// Command is v's remote command; ok is false for anything but a Verb above.
func Command(v Verb) (string, bool) {
	c, ok := commands[v]
	return c, ok
}

// Consent reports whether v may run Google's consent, and so needs the
// forward of lieer's callback port: auth, whose credential check can turn
// into a consent only the server knows it needs.
func (v Verb) Consent() bool { return v == Auth }

// MaxRequest bounds the parameters on stdin.
const MaxRequest = 16 << 10

// AddRequest is `pneu account add`'s parameters. ClientSecret is the OAuth
// client JSON's validated fields (gmi.CleanClientSecret), read on the
// client, or "" for none.
type AddRequest struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	FullName     string `json:"fullName"`
	ClientSecret string `json:"clientSecret"`
}

// AuthRequest is `pneu account auth`'s. ConsentOpen is always "print": the
// server sends the consent URL as an event instead of opening a browser on
// its own screen.
type AuthRequest struct {
	Name        string `json:"name"`
	Force       bool   `json:"force"`
	ConsentOpen string `json:"consentOpen"`
}

// ConsentPrint is the one ConsentOpen a request may carry.
const ConsentPrint = "print"

// StatusRequest is `pneu account status`'s; Name "" is every account.
type StatusRequest struct {
	Name string `json:"name"`
}

// MaxFullName bounds a From name; it goes into notmuch's config as a line.
const MaxFullName = 128

// ValidAddress is an address `pneu account add` takes: one '@', none of
// " <>", no control or format characters, at most 254 bytes.
func ValidAddress(a string) bool {
	return len(a) <= 254 && strings.Count(a, "@") == 1 && !strings.ContainsAny(a, " <>") && printable(a)
}

// ValidFullName is a From name: at most MaxFullName bytes of printable
// text, so it can't add a line to the notmuch config it's written into.
func ValidFullName(n string) bool { return len(n) <= MaxFullName && printable(n) }

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// check bounds one request line (without its newline).
func check(b []byte) error {
	if len(b) > MaxRequest {
		return fmt.Errorf("request over %d bytes", MaxRequest)
	}
	return nil
}

// The request is the first line on stdin (ReadLine, at most MaxRequest
// bytes). add and status then need the end of input; auth's stdin stays
// open for at most one more line, the consent callback (ParseCallbackLine).

// ParseAdd reads add's request: exactly its four keys, each a string.
func ParseAdd(b []byte) (AddRequest, error) {
	var q AddRequest
	err := check(b)
	if err == nil {
		err = decodeObject(b, map[string]any{"name": &q.Name, "address": &q.Address, "fullName": &q.FullName, "clientSecret": &q.ClientSecret})
	}
	switch {
	case err != nil:
		return q, fmt.Errorf("request: %w", err)
	case !config.ValidName(q.Name):
		return q, fmt.Errorf("request: bad account name: %s", config.NameRule)
	case !ValidAddress(q.Address):
		return q, errors.New("request: bad address")
	case !ValidFullName(q.FullName):
		return q, errors.New("request: bad full name")
	}
	if q.ClientSecret != "" {
		if _, err := gmi.CleanClientSecret([]byte(q.ClientSecret)); err != nil {
			return q, fmt.Errorf("request: OAuth client: %w", err)
		}
	}
	return q, nil
}

// ParseAuth reads auth's request: name, force (a boolean) and consentOpen,
// which must be "print".
func ParseAuth(b []byte) (AuthRequest, error) {
	var q AuthRequest
	err := check(b)
	if err == nil {
		err = decodeObject(b, map[string]any{"name": &q.Name, "force": &q.Force, "consentOpen": &q.ConsentOpen})
	}
	switch {
	case err != nil:
		return q, fmt.Errorf("request: %w", err)
	case !config.ValidName(q.Name):
		return q, fmt.Errorf("request: bad account name: %s", config.NameRule)
	case q.ConsentOpen != ConsentPrint:
		return q, errors.New(`request: consentOpen must be "print"`)
	}
	return q, nil
}

// ParseStatus reads status's request: name, "" or an account name.
func ParseStatus(b []byte) (StatusRequest, error) {
	var q StatusRequest
	err := check(b)
	if err == nil {
		err = decodeObject(b, map[string]any{"name": &q.Name})
	}
	switch {
	case err != nil:
		return q, fmt.Errorf("request: %w", err)
	case q.Name != "" && !config.ValidName(q.Name):
		return q, fmt.Errorf("request: bad account name: %s", config.NameRule)
	}
	return q, nil
}

// decodeObject walks the tokens itself: '{', exactly the keys of fields,
// once each, spelled exactly, each a string (*string) or a boolean
// (*bool), then '}' and the end of input. encoding/json would keep the
// last of a duplicate and match "Name" to name.
func decodeObject(b []byte, fields map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		k, _ := t.(string)
		dst, ok := fields[k]
		switch {
		case !ok:
			return fmt.Errorf("unknown field %q", k)
		case seen[k]:
			return fmt.Errorf("%q twice", k)
		}
		seen[k] = true
		v, err := dec.Token()
		if err != nil {
			return err
		}
		switch d := dst.(type) {
		case *string:
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%q isn't a string", k)
			}
			*d = s
		case *bool:
			x, ok := v.(bool)
			if !ok {
				return fmt.Errorf("%q isn't a boolean", k)
			}
			*d = x
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errors.New("unterminated object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the object")
	}
	if len(seen) != len(fields) {
		return fmt.Errorf("every field is required (%s)", strings.Join(keys(fields), ", "))
	}
	return nil
}

func keys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

// Event kinds, the server's whole vocabulary.
const (
	Progress = "progress"    // {text}: a line for the person at the terminal
	Waiting  = "waiting"     // {}: still waiting on consent (a heartbeat)
	Consent  = "consent-url" // {url}: Google's consent screen, to open on the client
	Result   = "result"      // {text}: done; text may be empty
	Error    = "error"       // {text}: failed
)

// Event is one line of the stream. Text is display text; URL only a
// consent URL.
type Event struct {
	Kind string
	Text string
	URL  string
}

// Bounds on the stream. A line holds one event; text is display text at
// most MaxText runes once plain (MaxTextBytes raw); a session is at most
// MaxEvents events and MaxStream bytes. A heartbeat every 15s for the
// server's 10-minute consent wait is 40 events.
const (
	MaxLine      = 8 << 10
	MaxText      = 500
	MaxTextBytes = 2048
	MaxEvents    = 4096
	MaxStream    = 2 << 20
)

// Line is e encoded for the wire, newline-terminated. Text is made plain
// here too, so the server never sends what the client would refuse.
func (e Event) Line() []byte {
	var v any
	switch e.Kind {
	case Waiting:
		v = struct {
			Event string `json:"event"`
		}{e.Kind}
	case Consent:
		v = struct {
			Event string `json:"event"`
			URL   string `json:"url"`
		}{e.Kind, e.URL}
	default:
		v = struct {
			Event string `json:"event"`
			Text  string `json:"text"`
		}{e.Kind, config.Plain(e.Text, MaxText)}
	}
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

// shapes are each kind's keys besides "event".
var shapes = map[string][]string{
	Progress: {"text"},
	Waiting:  {},
	Consent:  {"url"},
	Result:   {"text"},
	Error:    {"text"},
}

// ParseEvent is the client's strict reading of one line: at most MaxLine
// bytes, one JSON object of string values, "event" a known kind and
// exactly that kind's other keys, each once. Text over MaxTextBytes is
// refused, and what's kept is made plain; a consent URL must pass
// gmi.ValidConsentURL. Anything else is an error: the caller ends the
// session.
func ParseEvent(line []byte) (Event, error) {
	if len(line) > MaxLine {
		return Event{}, fmt.Errorf("event over %d bytes", MaxLine)
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return Event{}, errors.New("event isn't a JSON object")
	}
	got := map[string]string{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return Event{}, errors.New("event isn't valid JSON")
		}
		k, _ := t.(string)
		if _, dup := got[k]; dup {
			return Event{}, fmt.Errorf("event key %q twice", plainKey(k))
		}
		if len(got) == 2 {
			return Event{}, errors.New("event has extra fields")
		}
		v, err := dec.Token()
		if err != nil {
			return Event{}, errors.New("event isn't valid JSON")
		}
		s, ok := v.(string)
		if !ok {
			return Event{}, fmt.Errorf("event field %q isn't a string", plainKey(k))
		}
		got[k] = s
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return Event{}, errors.New("event isn't one JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Event{}, errors.New("data after the event")
	}
	kind, ok := got["event"]
	want, known := shapes[kind]
	if !ok || !known {
		return Event{}, fmt.Errorf("unknown event %q", plainKey(kind))
	}
	if len(got) != len(want)+1 {
		return Event{}, fmt.Errorf("%s event has the wrong fields", kind)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			return Event{}, fmt.Errorf("%s event has the wrong fields", kind)
		}
	}
	e := Event{Kind: kind}
	if kind == Consent {
		if !gmi.ValidConsentURL(got["url"]) {
			return Event{}, errors.New("the consent URL isn't Google's (or isn't plain https)")
		}
		e.URL = got["url"]
		return e, nil
	}
	if t := got["text"]; len(t) > MaxTextBytes {
		return Event{}, fmt.Errorf("%s text over %d bytes", kind, MaxTextBytes)
	}
	e.Text = config.Plain(got["text"], MaxText)
	return e, nil
}

// plainKey keeps a key from the server short and plain in an error.
func plainKey(k string) string { return config.Plain(k, 32) }

// ErrLongLine is a line past its bound.
var ErrLongLine = errors.New("a line over its size cap")

// ReadLine is one '\n'-terminated line without it, at most max bytes; a
// final line without '\n' counts. io.EOF only at a clean end.
func ReadLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > max+1 {
			return nil, ErrLongLine
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}

// The consent callback. lieer's redirect_uri is http://localhost:8080/ on
// the server, but Google sends the browser there on the client. No SSH
// forward carries it (a user's ssh config can add forwards that a
// preflight can't see, and ClearAllForwardings clears ours too): the
// client answers the redirect itself and sends its query to the server as
// one line on stdin, and the server replays it to lieer on its own
// loopback.

// MaxCallback bounds a callback's raw query.
const MaxCallback = 4096

// callbackKeys are what Google's redirect carries (RFC 6749 4.1.2 and
// 4.1.2.1, and Google's own additions); oauthlib's
// parse_authorization_code_response reads state, code and error from it.
var callbackKeys = map[string]bool{
	"state": true, "code": true, "scope": true, "authuser": true, "prompt": true,
	"hd": true, "iss": true,
	"error": true, "error_description": true, "error_uri": true,
}

// ParseCallback is a callback query held to shape: at most MaxCallback
// bytes, well-formed, only callbackKeys, each once, values printable ASCII
// (a scope's spaces included), a state and exactly one of code or error.
func ParseCallback(raw string) (url.Values, error) {
	if len(raw) > MaxCallback {
		return nil, fmt.Errorf("callback over %d bytes", MaxCallback)
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, errors.New("callback isn't a well-formed query")
	}
	for k, vs := range q {
		if !callbackKeys[k] {
			return nil, fmt.Errorf("callback key %q isn't one Google sends", plainKey(k))
		}
		if len(vs) != 1 {
			return nil, fmt.Errorf("callback key %q twice", k)
		}
		for i := 0; i < len(vs[0]); i++ {
			if c := vs[0][i]; c < ' ' || c >= 0x7f {
				return nil, fmt.Errorf("callback %s isn't printable ASCII", k)
			}
		}
	}
	if q.Get("state") == "" || (q.Get("code") == "") == (q.Get("error") == "") {
		return nil, errors.New("callback needs a state and one of code or error")
	}
	return q, nil
}

// CallbackLine is the client's line carrying a callback query.
func CallbackLine(raw string) []byte {
	b, _ := json.Marshal(struct {
		Callback string `json:"callback"`
	}{raw})
	return append(b, '\n')
}

// ParseCallbackLine reads that line by token, exactly the key
// "callback", and the query in it with ParseCallback.
func ParseCallbackLine(line []byte) (url.Values, error) {
	if len(line) > MaxCallback+64 {
		return nil, errors.New("callback line over its size cap")
	}
	var raw string
	if err := decodeObject(line, map[string]any{"callback": &raw}); err != nil {
		return nil, fmt.Errorf("callback line: %w", err)
	}
	return ParseCallback(raw)
}

// ConsentState is the state parameter of a valid consent URL, which the
// callback must echo: a request without it isn't Google's redirect for
// this consent, and doesn't use up the one-shot.
func ConsentState(u string) (string, bool) {
	if !gmi.ValidConsentURL(u) {
		return "", false
	}
	p, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	vs := p.Query()["state"]
	if len(vs) != 1 || vs[0] == "" {
		return "", false
	}
	return vs[0], true
}
