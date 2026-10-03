// Package googletest is an in-process Google for push's tests: the token
// endpoint, Gmail's getProfile, watch and stop, and Pub/Sub's topics, IAM
// policies, subscriptions, pull and acknowledge, all on one
// httptest.Server, with a fake clock, failures injectable per call, and a
// scripted message feed. It behaves as the real services do where pneu
// depends on it (PKCE, one-shot codes, scopes per token, etags, a watch
// publishing at once, unacked messages outstanding until redelivered),
// and no further.
package googletest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/google/internal/testhook"
)

// The Fake's defaults.
const (
	Project  = "pneu-push-test"
	ClientID = "123456789-fake.apps.googleusercontent.com"
	Secret   = "GOCSPX-fake-secret"
)

// Epoch is where a Fake's clock starts.
var Epoch = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// WatchLife is how long a watch lasts (Gmail: 7 days).
const WatchLife = 7 * 24 * time.Hour

// TokenLife is the expires_in the fake gives (Google: 3599s).
const TokenLife = 3599 * time.Second

// User is a Google account the fake knows.
type User struct {
	Email string
	Sub   string
}

// Request is one request the fake answered, as it arrived.
type Request struct {
	Op     google.Op
	Method string
	Host   string
	Path   string // without the host prefix
	Query  string
	Header http.Header
	Body   []byte
}

// Failure is an answer injected in place of the fake's own, for the next
// Times requests of an op (0 means 1; negative means until cleared).
type Failure struct {
	Status int    // HTTP status of the answer
	Body   string // its body
	Drop   bool   // close the connection with no answer instead
	Stall  bool   // hold the request until the client gives up instead
	Times  int
}

// Fake is the fake Google. Its zero value isn't usable; call New.
type Fake struct {
	Clock *Clock
	// Project, ClientID and Secret are what it accepts; set before use.
	Project, ClientID, Secret string
	// PullHold is how long (real time) a pull with nothing to deliver is
	// held before an empty answer; default 50ms.
	PullHold time.Duration

	srv    *httptest.Server
	closed chan struct{}

	mu         sync.Mutex
	users      map[string]*User      // by email
	codes      map[string]*grant     // authorization code -> grant
	refresh    map[string]*grant     // refresh token -> grant
	access     map[string]*accessTok // access token -> its grant
	watches    map[string]watch      // email -> its watch
	stops      map[string]int        // email -> users.stop calls
	topics     map[string]*topic     // full name
	subs       map[string]*sub       // full name
	failures   map[google.Op][]Failure
	tokenMut   []func(map[string]any)
	claimMut   []func(map[string]any)
	policyRace map[string]int // topic full name -> concurrent changes to make
	requests   []Request
	nextID     int
}

type grant struct {
	user      *User
	kind      google.Kind
	challenge string
	nonce     string
	used      bool
	revoked   bool
}

type accessTok struct {
	g      *grant
	expiry time.Time
}

type watch struct {
	topic  string
	expiry time.Time
}

type topic struct {
	policy map[string]json.RawMessage
	etag   int
	writes int
}

type sub struct {
	resource    []byte // the resource as GET answers it
	topic       string
	queue       []message
	outstanding map[string]message // ackId -> message
	acked       int
	wake        chan struct{} // closed and replaced when the queue grows
}

type message struct {
	id      string
	data    string
	attempt int
}

// New starts a fake, closed when the test ends.
func New(t testing.TB) *Fake {
	f := &Fake{
		Clock: NewClock(Epoch), Project: Project, ClientID: ClientID, Secret: Secret,
		PullHold: 50 * time.Millisecond,
		closed:   make(chan struct{}),
		users:    map[string]*User{}, codes: map[string]*grant{}, refresh: map[string]*grant{},
		access: map[string]*accessTok{}, watches: map[string]watch{},
		stops: map[string]int{}, topics: map[string]*topic{}, subs: map[string]*sub{},
		failures: map[google.Op][]Failure{}, policyRace: map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Close stops the server, releasing held pulls and stalls.
func (f *Fake) Close() {
	select {
	case <-f.closed:
		return
	default:
	}
	close(f.closed)
	f.srv.CloseClientConnections()
	f.srv.Close()
}

// API is a google.API pointed at the fake, on the fake's clock.
func (f *Fake) API() *google.API { return f.NewAPI(google.Options{}) }

// NewAPI is API with options; Now is the fake's clock unless set.
func (f *Fake) NewAPI(opts google.Options) *google.API {
	if opts.Now == nil {
		opts.Now = f.Clock.Now
	}
	a := google.New(opts)
	testhook.WithBase(a, f.srv.URL)
	return a
}

// Credentials are the client the fake accepts.
func (f *Fake) Credentials() google.Credentials {
	return google.Credentials{ID: f.ClientID, Secret: f.Secret}
}

// AddUser makes a Google account with email and a fresh sub.
func (f *Fake) AddUser(email string) User {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	u := &User{Email: email, Sub: fmt.Sprintf("1000000000000000%05d", f.nextID)}
	f.users[strings.ToLower(email)] = u
	return *u
}

// --- consent ---------------------------------------------------------

// Approve plays email approving the consent at consentURL and returns
// the callback query Google would redirect with. It fails if the URL
// isn't one pneu should send (wrong client, redirect, PKCE method,
// scopes, offline access or prompt).
func (f *Fake) Approve(consentURL, email string) (string, error) {
	q, kind, err := f.checkConsent(consentURL)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[strings.ToLower(email)]
	if u == nil {
		return "", fmt.Errorf("googletest: no user %s", email)
	}
	code := "4/" + randomID()
	f.codes[code] = &grant{user: u, kind: kind, challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	scope := strings.Join(grantedScopes(kind), " ")
	return url.Values{"state": {q.Get("state")}, "code": {code}, "scope": {scope}}.Encode(), nil
}

// Deny plays the user declining: the callback query carries an error.
func (f *Fake) Deny(consentURL string) (string, error) {
	q, _, err := f.checkConsent(consentURL)
	if err != nil {
		return "", err
	}
	return url.Values{"state": {q.Get("state")}, "error": {"access_denied"}}.Encode(), nil
}

func (f *Fake) checkConsent(consentURL string) (url.Values, google.Kind, error) {
	u, err := url.Parse(consentURL)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != google.AuthURL {
		return nil, 0, fmt.Errorf("googletest: not Google's consent URL")
	}
	q := u.Query()
	for k, vs := range q {
		if len(vs) != 1 {
			return nil, 0, fmt.Errorf("googletest: %s twice", k)
		}
	}
	want := map[string]string{
		"client_id": f.ClientID, "redirect_uri": google.Redirect, "response_type": "code",
		"access_type": "offline", "prompt": "consent", "include_granted_scopes": "false",
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			return nil, 0, fmt.Errorf("googletest: consent %s is %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q.Get("state")) < 22 || len(q.Get("code_challenge")) != 43 {
		return nil, 0, fmt.Errorf("googletest: weak state or bad challenge")
	}
	var kind google.Kind
	switch q.Get("scope") {
	case strings.Join(google.Owner.Scopes(), " "):
		kind = google.Owner
		if len(q.Get("nonce")) < 22 {
			return nil, 0, fmt.Errorf("googletest: owner consent without a nonce")
		}
	case strings.Join(google.Mailbox.Scopes(), " "):
		kind = google.Mailbox
	default:
		return nil, 0, fmt.Errorf("googletest: unexpected scope %q", q.Get("scope"))
	}
	return q, kind, nil
}

// grantedScopes is how the token endpoint names kind's scopes.
func grantedScopes(kind google.Kind) []string {
	if kind == google.Owner {
		return []string{google.ScopeOpenID, "https://www.googleapis.com/auth/userinfo.email", google.ScopePubSub}
	}
	return []string{google.ScopeMetadata}
}

// Revoke revokes every grant email has made so far: their refreshes
// answer invalid_grant and their access tokens 401. A later Approve is a
// new grant.
func (f *Fake) Revoke(email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range []map[string]*grant{f.codes, f.refresh} {
		for _, g := range m {
			if strings.EqualFold(g.user.Email, email) {
				g.revoked = true
			}
		}
	}
	for _, at := range f.access {
		if strings.EqualFold(at.g.user.Email, email) {
			at.g.revoked = true
		}
	}
}

// MutateNextToken changes the next token answer (an exchange's or a
// refresh's) before it's sent: a test's way to send wrong scopes, no
// refresh token, a bad token_type or expires_in.
func (f *Fake) MutateNextToken(fn func(answer map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenMut = append(f.tokenMut, fn)
}

// MutateNextClaims changes the next ID token's claims (iss, aud, exp,
// nonce, sub, email, email_verified) before it's encoded.
func (f *Fake) MutateNextClaims(fn func(claims map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimMut = append(f.claimMut, fn)
}

// --- failures and the request log ------------------------------------

// Fail queues fl for op's next requests.
func (f *Fake) Fail(op google.Op, fl Failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[op] = append(f.failures[op], fl)
}

// FailCode queues, for op's next times requests (0: 1; negative: until
// cleared), an answer google classifies as code.
func (f *Fake) FailCode(op google.Op, code google.Code, times int) {
	fl := Answer(op, code)
	fl.Times = times
	f.Fail(op, fl)
}

// ClearFailures drops every queued failure.
func (f *Fake) ClearFailures() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = map[google.Op][]Failure{}
}

// Answer is a canned Google answer for op that classifies as code.
func Answer(op google.Op, code google.Code) Failure {
	if op == google.OpExchange || op == google.OpRefresh {
		oauth := map[google.Code]string{
			google.CodeInvalidGrant: "invalid_grant", google.CodeScope: "invalid_scope",
			google.CodeOrgPolicy: "admin_policy_enforced", google.CodePermission: "invalid_client",
		}
		if e, ok := oauth[code]; ok {
			st := http.StatusBadRequest
			if code == google.CodePermission {
				st = http.StatusUnauthorized
			}
			return Failure{Status: st, Body: `{"error":"` + e + `","error_description":"Google's words"}`}
		}
	}
	apiErr := func(st int, status, reason string) Failure {
		return Failure{Status: st, Body: fmt.Sprintf(`{"error":{"code":%d,"message":"Google's words, never shown","status":%q,`+
			`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":%q,"domain":"googleapis.com"}]}}`, st, status, reason)}
	}
	switch code {
	case google.CodeScope:
		return apiErr(403, "PERMISSION_DENIED", "ACCESS_TOKEN_SCOPE_INSUFFICIENT")
	case google.CodePermission:
		return apiErr(403, "PERMISSION_DENIED", "IAM_PERMISSION_DENIED")
	case google.CodeAPIDisabled:
		return apiErr(403, "PERMISSION_DENIED", "SERVICE_DISABLED")
	case google.CodeOrgPolicy:
		return Failure{Status: 400, Body: `{"error":{"code":400,"message":"One or more users named in the policy do not belong to a permitted customer.","status":"FAILED_PRECONDITION",` +
			`"details":[{"@type":"type.googleapis.com/google.rpc.PreconditionFailure","violations":[{"type":"constraints/iam.allowedPolicyMemberDomains","subject":"orgpolicy:projects/x"}]}]}}`}
	case google.CodeNotFound:
		return apiErr(404, "NOT_FOUND", "")
	case google.CodeConflict:
		return apiErr(409, "ALREADY_EXISTS", "")
	case google.CodeInvalidGrant:
		return Failure{Status: 400, Body: `{"error":"invalid_grant"}`}
	case google.CodeUnauthenticated:
		return apiErr(401, "UNAUTHENTICATED", "")
	case google.CodeQuota:
		return apiErr(429, "RESOURCE_EXHAUSTED", "RATE_LIMIT_EXCEEDED")
	case google.CodeUnavailable:
		return apiErr(503, "UNAVAILABLE", "")
	case google.CodeNetwork:
		return Failure{Drop: true}
	}
	return Failure{Status: 400, Body: `{"error":{"code":400,"message":"?","status":"INVALID_ARGUMENT"}}`}
}

// Calls counts op's requests so far, failed ones included.
func (f *Fake) Calls(op google.Op) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Op == op {
			n++
		}
	}
	return n
}

// Requests is every request so far, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// --- Gmail -----------------------------------------------------------

// Watch is email's watch: its topic (full name) and expiry; ok false when
// it has none (never watched, or stopped).
func (f *Fake) Watch(email string) (topic string, expiry time.Time, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.watches[strings.ToLower(email)]
	return w.topic, w.expiry, ok
}

// Stops counts users.stop calls for email.
func (f *Fake) Stops(email string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops[strings.ToLower(email)]
}

// Notify plays new mail in email's inbox: Gmail publishes to its watch's
// topic if the watch is live. It reports whether anything was published.
func (f *Fake) Notify(email string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.watches[strings.ToLower(email)]
	if !ok || !f.Clock.Now().Before(w.expiry) {
		return false
	}
	f.publishLocked(w.topic, email)
	return true
}

// --- Pub/Sub ---------------------------------------------------------

func (f *Fake) topicName(id string) string { return "projects/" + f.Project + "/topics/" + id }
func (f *Fake) subName(id string) string   { return "projects/" + f.Project + "/subscriptions/" + id }

// AddTopic makes topic id in the fake's project, with an empty policy.
func (f *Fake) AddTopic(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topics[f.topicName(id)] = &topic{policy: map[string]json.RawMessage{}, etag: 1}
}

// HasTopic reports whether topic id exists.
func (f *Fake) HasTopic(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.topics[f.topicName(id)] != nil
}

// SetPolicy replaces topic id's IAM policy with raw (its etag is the
// fake's own), making the topic if needed.
func (f *Fake) SetPolicy(id, raw string) {
	var p map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		panic(err)
	}
	delete(p, "etag")
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.topics[f.topicName(id)]
	if t == nil {
		t = &topic{etag: 1}
		f.topics[f.topicName(id)] = t
	}
	t.policy = p
	t.etag++
}

// Policy is topic id's IAM policy as getIamPolicy answers it.
func (f *Fake) Policy(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.topics[f.topicName(id)]
	if t == nil {
		return ""
	}
	return string(t.policyJSON())
}

// PolicyWrites counts accepted setIamPolicy calls on topic id.
func (f *Fake) PolicyWrites(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.topics[f.topicName(id)]; t != nil {
		return t.writes
	}
	return 0
}

// RacePolicy makes the next n getIamPolicy answers on topic id followed at
// once by someone else's change, so a setIamPolicy with that etag
// conflicts.
func (f *Fake) RacePolicy(id string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policyRace[f.topicName(id)] = n
}

func (t *topic) policyJSON() []byte {
	p := map[string]json.RawMessage{}
	for k, v := range t.policy {
		p[k] = v
	}
	p["etag"], _ = json.Marshal(base64.StdEncoding.EncodeToString([]byte("etag" + strconv.Itoa(t.etag))))
	b, _ := json.Marshal(p)
	return b
}

// SetSubscription plants subscription id with raw as its resource (what
// GET answers), on topic id topicID: a test's way to make one that exists
// already, or differs.
func (f *Fake) SetSubscription(id, topicID, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[f.subName(id)] = &sub{resource: []byte(raw), topic: f.topicName(topicID),
		outstanding: map[string]message{}, wake: make(chan struct{})}
}

// Subscription is subscription id's resource; "" when there's none.
func (f *Fake) Subscription(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.subs[f.subName(id)]; s != nil {
		return string(s.resource)
	}
	return ""
}

// Publish queues n messages on subscription id directly.
func (f *Fake) Publish(id string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[f.subName(id)]
	if s == nil {
		panic("googletest: no subscription " + id)
	}
	for range n {
		f.enqueueLocked(s, "")
	}
}

// Redeliver puts subscription id's unacked messages back in its queue, as
// Pub/Sub does once their ack deadline passes.
func (f *Fake) Redeliver(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[f.subName(id)]
	if s == nil {
		return
	}
	for ackID, m := range s.outstanding {
		delete(s.outstanding, ackID)
		s.queue = append(s.queue, m)
	}
	s.signal()
}

// Queued, Outstanding and Acked count subscription id's messages waiting,
// delivered but unacked, and acked.
func (f *Fake) Queued(id string) int { return f.subCount(id, func(s *sub) int { return len(s.queue) }) }
func (f *Fake) Outstanding(id string) int {
	return f.subCount(id, func(s *sub) int { return len(s.outstanding) })
}
func (f *Fake) Acked(id string) int { return f.subCount(id, func(s *sub) int { return s.acked }) }

func (f *Fake) subCount(id string, n func(*sub) int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.subs[f.subName(id)]; s != nil {
		return n(s)
	}
	return 0
}

func (f *Fake) publishLocked(topic, email string) {
	for _, s := range f.subs {
		if s.topic == topic {
			f.enqueueLocked(s, email)
		}
	}
}

func (f *Fake) enqueueLocked(s *sub, email string) {
	f.nextID++
	data, _ := json.Marshal(map[string]any{"emailAddress": email, "historyId": 1000 + f.nextID})
	s.queue = append(s.queue, message{id: strconv.Itoa(f.nextID), data: base64.StdEncoding.EncodeToString(data)})
	s.signal()
}

func (s *sub) signal() {
	close(s.wake)
	s.wake = make(chan struct{})
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- serving ---------------------------------------------------------

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	host, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	path = "/" + path
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	op := route(host, r.Method, path)
	if op == google.OpExchange {
		if form, err := url.ParseQuery(string(body)); err == nil && form.Get("grant_type") == "refresh_token" {
			op = google.OpRefresh
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, Request{Op: op, Method: r.Method, Host: host, Path: path,
		Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
	fl, failed := f.takeFailure(op)
	f.mu.Unlock()
	if failed {
		switch {
		case fl.Drop:
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		case fl.Stall:
			select {
			case <-r.Context().Done():
			case <-f.closed:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fl.Status)
		io.WriteString(w, fl.Body)
		return
	}
	if op == "" {
		jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
		return
	}
	switch op {
	case google.OpExchange, google.OpRefresh:
		f.serveToken(w, r, body)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
		jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", ""))
		return
	}
	at, st := f.bearer(r, op)
	if st != 0 {
		reason := map[int]string{401: "", 403: "ACCESS_TOKEN_SCOPE_INSUFFICIENT"}[st]
		jsonAnswer(w, st, apiError(st, map[int]string{401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED"}[st], reason))
		return
	}
	switch op {
	case google.OpProfile, google.OpWatch, google.OpStop:
		f.serveGmail(w, op, at, body)
	case google.OpPull:
		f.servePull(w, r, path, body)
	default:
		f.servePubSub(w, op, path, r.URL.RawQuery, body)
	}
}

// takeFailure pops op's next queued failure.
func (f *Fake) takeFailure(op google.Op) (Failure, bool) {
	q := f.failures[op]
	if len(q) == 0 {
		return Failure{}, false
	}
	fl := q[0]
	switch {
	case fl.Times < 0:
	case fl.Times > 1:
		q[0].Times--
	default:
		f.failures[op] = q[1:]
	}
	return fl, true
}

// route names the Google method a request is, from its host and path.
func route(host, method, path string) google.Op {
	switch host {
	case google.TokenHost:
		if method == http.MethodPost && path == "/token" {
			return google.OpExchange // or a refresh: serveToken tells them apart
		}
	case google.GmailHost:
		switch {
		case method == http.MethodGet && path == "/gmail/v1/users/me/profile":
			return google.OpProfile
		case method == http.MethodPost && path == "/gmail/v1/users/me/watch":
			return google.OpWatch
		case method == http.MethodPost && path == "/gmail/v1/users/me/stop":
			return google.OpStop
		}
	case google.PubSubHost:
		parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
		if len(parts) == 3 && parts[0] == "projects" && parts[2] == "topics" && method == http.MethodGet {
			return google.OpTopicList
		}
		if len(parts) != 4 || parts[0] != "projects" {
			return ""
		}
		_, verb, _ := strings.Cut(parts[3], ":")
		switch {
		case parts[2] == "topics" && verb == "" && method == http.MethodPut:
			return google.OpTopicMake
		case parts[2] == "topics" && verb == "" && method == http.MethodGet:
			return google.OpTopicGet
		case parts[2] == "topics" && verb == "getIamPolicy" && method == http.MethodGet:
			return google.OpGetPolicy
		case parts[2] == "topics" && verb == "setIamPolicy" && method == http.MethodPost:
			return google.OpSetPolicy
		case parts[2] == "subscriptions" && verb == "" && method == http.MethodPut:
			return google.OpSubMake
		case parts[2] == "subscriptions" && verb == "" && method == http.MethodGet:
			return google.OpSubGet
		case parts[2] == "subscriptions" && verb == "pull" && method == http.MethodPost:
			return google.OpPull
		case parts[2] == "subscriptions" && verb == "acknowledge" && method == http.MethodPost:
			return google.OpAck
		}
	}
	return ""
}

func jsonAnswer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(code int, status, reason string) map[string]any {
	e := map[string]any{"code": code, "message": "Google's words, never shown", "status": status}
	if reason != "" {
		e["details"] = []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason}}
	}
	return map[string]any{"error": e}
}

// bearer finds the request's access token and checks it fits op: alive,
// not revoked, and of the kind whose scope op needs.
func (f *Fake) bearer(r *http.Request, op google.Op) (*accessTok, int) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	at := f.access[tok]
	if !ok || at == nil || !f.Clock.Now().Before(at.expiry) || at.g.revoked {
		return nil, 401
	}
	gmail := op == google.OpProfile || op == google.OpWatch || op == google.OpStop
	if gmail != (at.g.kind == google.Mailbox) {
		return nil, 403
	}
	return at, 0
}

func (f *Fake) serveToken(w http.ResponseWriter, r *http.Request, body []byte) {
	oauthErr := func(status int, e string) {
		jsonAnswer(w, status, map[string]any{"error": e, "error_description": "Google's words"})
	}
	form, err := url.ParseQuery(string(body))
	if err != nil || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.URL.RawQuery != "" {
		oauthErr(400, "invalid_request")
		return
	}
	if form.Get("client_id") != f.ClientID || form.Get("client_secret") != f.Secret {
		oauthErr(401, "invalid_client")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var g *grant
	issueRefresh := false
	switch form.Get("grant_type") {
	case "authorization_code":
		g = f.codes[form.Get("code")]
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if g == nil || g.used || g.revoked || form.Get("redirect_uri") != google.Redirect ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			if g != nil {
				g.used = true // a code is good for one try
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
			return
		}
		g.used = true
		issueRefresh = true
	case "refresh_token":
		g = f.refresh[form.Get("refresh_token")]
		if g == nil || g.revoked {
			oauthErr(400, "invalid_grant")
			return
		}
	default:
		oauthErr(400, "unsupported_grant_type")
		return
	}
	now := f.Clock.Now()
	access := "ya29." + randomID()
	f.access[access] = &accessTok{g: g, expiry: now.Add(TokenLife)}
	answer := map[string]any{
		"access_token": access, "expires_in": int(TokenLife / time.Second), "token_type": "Bearer",
		"scope": strings.Join(grantedScopes(g.kind), " "),
	}
	if issueRefresh {
		rt := "1//" + randomID()
		f.refresh[rt] = g
		answer["refresh_token"] = rt
	}
	if g.kind == google.Owner {
		claims := map[string]any{
			"iss": "https://accounts.google.com", "azp": f.ClientID, "aud": f.ClientID,
			"sub": g.user.Sub, "email": g.user.Email, "email_verified": true,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}
		if issueRefresh && g.nonce != "" {
			claims["nonce"] = g.nonce
		}
		if len(f.claimMut) > 0 {
			f.claimMut[0](claims)
			f.claimMut = f.claimMut[1:]
		}
		answer["id_token"] = idToken(claims)
	}
	if len(f.tokenMut) > 0 {
		f.tokenMut[0](answer)
		f.tokenMut = f.tokenMut[1:]
	}
	jsonAnswer(w, 200, answer)
}

// idToken encodes claims as an unsigned-looking JWT: pneu reads the
// claims of a token it got from the endpoint over TLS and checks no
// signature (OIDC Core 3.1.3.7).
func idToken(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "RS256", "kid": "fake", "typ": "JWT"}) + "." + enc(claims) + ".c2lnbmF0dXJl"
}

func (f *Fake) serveGmail(w http.ResponseWriter, op google.Op, at *accessTok, body []byte) {
	email := at.g.user.Email
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op {
	case google.OpProfile:
		jsonAnswer(w, 200, map[string]any{"emailAddress": email, "messagesTotal": 12, "threadsTotal": 10, "historyId": "4242"})
	case google.OpStop:
		f.stops[strings.ToLower(email)]++
		delete(f.watches, strings.ToLower(email))
		w.WriteHeader(204)
	case google.OpWatch:
		var req struct {
			TopicName           string   `json:"topicName"`
			LabelIDs            []string `json:"labelIds"`
			LabelFilterBehavior string   `json:"labelFilterBehavior"`
		}
		if json.Unmarshal(body, &req) != nil || !slices.Equal(req.LabelIDs, []string{"INBOX"}) || req.LabelFilterBehavior != "include" {
			jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", ""))
			return
		}
		if !strings.HasPrefix(req.TopicName, "projects/"+f.Project+"/topics/") {
			jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", "")) // not the client's project
			return
		}
		t := f.topics[req.TopicName]
		if t == nil {
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		if !t.grantsPublisher() {
			jsonAnswer(w, 403, map[string]any{"error": map[string]any{"code": 403, "status": "PERMISSION_DENIED",
				"message": "Error sending test message to Cloud PubSub: User not authorized to perform this action.",
				"errors":  []any{map[string]any{"reason": "forbidden", "domain": "global"}}}})
			return
		}
		exp := f.Clock.Now().Add(WatchLife)
		f.watches[strings.ToLower(email)] = watch{topic: req.TopicName, expiry: exp}
		f.publishLocked(req.TopicName, email) // a watch publishes once at once (C4)
		jsonAnswer(w, 200, map[string]any{"historyId": "4243", "expiration": strconv.FormatInt(exp.UnixMilli(), 10)})
	}
}

// hasCondition reports whether any binding in p carries a condition.
func hasCondition(p map[string]json.RawMessage) bool {
	var bindings []struct {
		Condition json.RawMessage `json:"condition"`
	}
	json.Unmarshal(p["bindings"], &bindings)
	for _, b := range bindings {
		if b.Condition != nil {
			return true
		}
	}
	return false
}

// grantsPublisher reports whether the policy has PublisherMember in an
// unconditional PublisherRole binding.
func (t *topic) grantsPublisher() bool {
	var bindings []struct {
		Role      string          `json:"role"`
		Members   []string        `json:"members"`
		Condition json.RawMessage `json:"condition"`
	}
	json.Unmarshal(t.policy["bindings"], &bindings)
	for _, b := range bindings {
		if b.Role == google.PublisherRole && b.Condition == nil && slices.Contains(b.Members, google.PublisherMember) {
			return true
		}
	}
	return false
}

func (f *Fake) servePubSub(w http.ResponseWriter, op google.Op, path, query string, body []byte) {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	if parts[1] != f.Project {
		jsonAnswer(w, 403, apiError(403, "PERMISSION_DENIED", "IAM_PERMISSION_DENIED"))
		return
	}
	if op == google.OpTopicList {
		jsonAnswer(w, 200, map[string]any{})
		return
	}
	id, _, _ := strings.Cut(parts[3], ":")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op {
	case google.OpTopicMake:
		name := f.topicName(id)
		if f.topics[name] != nil {
			jsonAnswer(w, 409, apiError(409, "ALREADY_EXISTS", ""))
			return
		}
		f.topics[name] = &topic{policy: map[string]json.RawMessage{}, etag: 1}
		jsonAnswer(w, 200, map[string]any{"name": name})
	case google.OpTopicGet:
		name := f.topicName(id)
		if f.topics[name] == nil {
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		jsonAnswer(w, 200, map[string]any{"name": name})
	case google.OpGetPolicy:
		t := f.topics[f.topicName(id)]
		if t == nil {
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		// A policy with conditions is only read at version 3; anything
		// asking for less would get it stripped, so the fake refuses.
		if q, _ := url.ParseQuery(query); q.Get("options.requestedPolicyVersion") != "3" {
			jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", ""))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(t.policyJSON())
		if n := f.policyRace[f.topicName(id)]; n > 0 {
			f.policyRace[f.topicName(id)] = n - 1
			t.etag++
		}
	case google.OpSetPolicy:
		t := f.topics[f.topicName(id)]
		if t == nil {
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		var req struct {
			Policy map[string]json.RawMessage `json:"policy"`
		}
		if json.Unmarshal(body, &req) != nil || req.Policy == nil {
			jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", ""))
			return
		}
		var etag string
		json.Unmarshal(req.Policy["etag"], &etag)
		if etag != base64.StdEncoding.EncodeToString([]byte("etag"+strconv.Itoa(t.etag))) {
			jsonAnswer(w, 409, apiError(409, "ABORTED", ""))
			return
		}
		if string(req.Policy["version"]) != "3" && hasCondition(req.Policy) {
			jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", "")) // conditions need version 3
			return
		}
		delete(req.Policy, "etag")
		t.policy = req.Policy
		t.etag++
		t.writes++
		w.Header().Set("Content-Type", "application/json")
		w.Write(t.policyJSON())
	case google.OpSubMake:
		name := f.subName(id)
		if f.subs[name] != nil {
			jsonAnswer(w, 409, apiError(409, "ALREADY_EXISTS", ""))
			return
		}
		var req map[string]json.RawMessage
		var topicName string
		if json.Unmarshal(body, &req) != nil || json.Unmarshal(req["topic"], &topicName) != nil {
			jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", ""))
			return
		}
		if f.topics[topicName] == nil {
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		res := map[string]json.RawMessage{}
		for k, v := range req {
			res[k] = v
		}
		res["name"], _ = json.Marshal(name)
		res["pushConfig"] = json.RawMessage(`{}`)
		res["state"] = json.RawMessage(`"ACTIVE"`)
		if _, ok := res["expirationPolicy"]; !ok {
			res["expirationPolicy"] = json.RawMessage(`{"ttl":"2678400s"}`)
		}
		b, _ := json.Marshal(res)
		f.subs[name] = &sub{resource: b, topic: topicName, outstanding: map[string]message{}, wake: make(chan struct{})}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	case google.OpSubGet:
		s := f.subs[f.subName(id)]
		if s == nil {
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.resource)
	case google.OpAck:
		s := f.subs[f.subName(id)]
		if s == nil {
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		var req struct {
			AckIDs []string `json:"ackIds"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.AckIDs) == 0 {
			jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", ""))
			return
		}
		for _, a := range req.AckIDs {
			if _, ok := s.outstanding[a]; ok {
				delete(s.outstanding, a)
				s.acked++
			}
		}
		jsonAnswer(w, 200, map[string]any{})
	}
}

func (f *Fake) servePull(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	id, _, _ := strings.Cut(parts[3], ":")
	var req struct {
		MaxMessages int `json:"maxMessages"`
	}
	if json.Unmarshal(body, &req) != nil || req.MaxMessages < 1 {
		jsonAnswer(w, 400, apiError(400, "INVALID_ARGUMENT", ""))
		return
	}
	hold := time.NewTimer(f.PullHold)
	defer hold.Stop()
	for {
		f.mu.Lock()
		s := f.subs[f.subName(id)]
		if parts[1] != f.Project || s == nil {
			f.mu.Unlock()
			jsonAnswer(w, 404, apiError(404, "NOT_FOUND", ""))
			return
		}
		if len(s.queue) > 0 {
			n := min(req.MaxMessages, len(s.queue))
			var out []any
			for _, m := range s.queue[:n] {
				m.attempt++
				ackID := "ack-" + randomID() + strings.Repeat("x", 60)
				s.outstanding[ackID] = m
				out = append(out, map[string]any{"ackId": ackID, "deliveryAttempt": m.attempt,
					"message": map[string]any{"data": m.data, "messageId": m.id, "publishTime": f.Clock.Now().Format(time.RFC3339Nano)}})
			}
			s.queue = s.queue[n:]
			f.mu.Unlock()
			jsonAnswer(w, 200, map[string]any{"receivedMessages": out})
			return
		}
		wake := s.wake
		f.mu.Unlock()
		select {
		case <-wake:
		case <-hold.C:
			jsonAnswer(w, 200, map[string]any{})
			return
		case <-r.Context().Done():
			return
		case <-f.closed:
			return
		}
	}
}
