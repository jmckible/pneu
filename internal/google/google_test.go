package google_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jmckible/pneu/internal/gmi"
	"github.com/jmckible/pneu/internal/google"
	"github.com/jmckible/pneu/internal/google/googletest"
	"github.com/jmckible/pneu/internal/remote"
)

const install = "0123456789abcdef"

func code(t *testing.T, err error, want google.Code) {
	t.Helper()
	if got := google.CodeOf(err); got != want {
		t.Fatalf("got %v (code %q), want code %q", err, got, want)
	}
}

// consent runs a consent for kind as email through the fake and returns
// the exchanged token.
func consent(t *testing.T, f *googletest.Fake, api *google.API, kind google.Kind, email string) (*google.Consent, google.Token) {
	t.Helper()
	c, err := google.NewConsent(f.Credentials(), kind, email)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.Approve(c.URL(), email)
	if err != nil {
		t.Fatal(err)
	}
	q, err := remote.ParseCallback(raw)
	if err != nil {
		t.Fatal(err)
	}
	cd, err := c.Callback(q)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := api.Exchange(context.Background(), c, cd)
	if err != nil {
		t.Fatal(err)
	}
	return c, tok
}

func TestConsentURL(t *testing.T) {
	f := googletest.New(t)
	for _, kind := range []google.Kind{google.Owner, google.Mailbox} {
		c, err := google.NewConsent(f.Credentials(), kind, "j@example.com")
		if err != nil {
			t.Fatal(err)
		}
		u := c.URL()
		if !gmi.ValidConsentURL(u) {
			t.Fatalf("%v: %s fails gmi.ValidConsentURL", kind, u)
		}
		if st, ok := remote.ConsentState(u); !ok || st != c.State() || len(st) < 22 {
			t.Fatalf("%v: state %q", kind, st)
		}
		q, _ := url.Parse(u)
		if got := q.Query().Get("login_hint"); got != "j@example.com" {
			t.Fatalf("login_hint %q", got)
		}
		if (q.Query().Get("nonce") != "") != (kind == google.Owner) {
			t.Fatalf("%v: nonce %q", kind, q.Query().Get("nonce"))
		}
		if strings.Contains(u, f.Secret) {
			t.Fatal("client secret in the consent URL")
		}
		// The fake checks everything else D3 asks of it.
		if _, err := f.Approve(u, "nobody@example.com"); err == nil || !strings.Contains(err.Error(), "no user") {
			t.Fatalf("%v: %v", kind, err)
		}
	}
	c1, _ := google.NewConsent(f.Credentials(), google.Owner, "")
	c2, _ := google.NewConsent(f.Credentials(), google.Owner, "")
	if c1.State() == c2.State() || strings.Contains(c1.URL(), "login_hint") {
		t.Fatal("states repeat, or an empty hint was sent")
	}
	for _, bad := range []struct {
		creds google.Credentials
		kind  google.Kind
		hint  string
	}{
		{google.Credentials{ID: "nope", Secret: "s"}, google.Owner, ""},
		{google.Credentials{ID: googletest.ClientID, Secret: "has space"}, google.Owner, ""},
		{f.Credentials(), 0, ""},
		{f.Credentials(), google.Mailbox, "not an address"},
	} {
		if _, err := google.NewConsent(bad.creds, bad.kind, bad.hint); err == nil {
			t.Errorf("NewConsent(%v, %v, %q) accepted", bad.creds, bad.kind, bad.hint)
		}
	}
	if s := fmt.Sprintf("%v %+v %#v", c1, *c1, c1); strings.Contains(s, c1.State()) {
		t.Fatal("a Consent prints its state")
	}
}

func TestCallback(t *testing.T) {
	f := googletest.New(t)
	f.AddUser("o@example.com")
	c, _ := google.NewConsent(f.Credentials(), google.Owner, "o@example.com")
	raw, _ := f.Approve(c.URL(), "o@example.com")
	q, _ := remote.ParseCallback(raw)

	other, _ := google.NewConsent(f.Credentials(), google.Owner, "o@example.com")
	if _, err := other.Callback(q); !errors.Is(err, google.ErrState) {
		t.Fatalf("another consent's callback: %v", err)
	}
	denied, _ := f.Deny(c.URL())
	dq, _ := remote.ParseCallback(denied)
	if _, err := c.Callback(dq); !errors.Is(err, google.ErrDenied) {
		t.Fatalf("denial: %v", err)
	}
	if _, err := c.Callback(url.Values{"state": {c.State()}}); !errors.Is(err, google.ErrCallback) {
		t.Fatalf("no code: %v", err)
	}
	if _, err := c.Callback(url.Values{"state": {c.State(), c.State()}, "code": {"x"}}); !errors.Is(err, google.ErrState) {
		t.Fatalf("two states: %v", err)
	}
	if _, err := c.Callback(url.Values{"state": {c.State()}, "code": {strings.Repeat("a", google.MaxCode+1)}}); !errors.Is(err, google.ErrCallback) {
		t.Fatalf("long code: %v", err)
	}
	if cd, err := c.Callback(q); err != nil || cd == "" {
		t.Fatal(cd, err)
	}
}

func TestOwnerExchangeAndRefresh(t *testing.T) {
	f := googletest.New(t)
	u := f.AddUser("owner@example.com")
	api := f.API()
	_, tok := consent(t, f, api, google.Owner, u.Email)
	if tok.Identity == nil || tok.Identity.Sub != u.Sub || tok.Identity.Email != u.Email {
		t.Fatalf("identity %+v", tok.Identity)
	}
	if tok.Refresh == "" || tok.Access == "" || !tok.Expiry.Equal(f.Clock.Now().Add(googletest.TokenLife)) {
		t.Fatalf("token %+v", tok)
	}
	if s := fmt.Sprintf("%v %+v %#v", tok, tok, tok); strings.Contains(s, tok.Access) || strings.Contains(s, tok.Refresh) {
		t.Fatal("a Token prints its secrets")
	}

	f.Clock.Advance(2 * time.Hour)
	r, err := api.Refresh(context.Background(), f.Credentials(), google.Owner, tok.Refresh, u.Sub)
	if err != nil {
		t.Fatal(err)
	}
	if r.Refresh != "" || r.Access == tok.Access || r.Identity == nil || r.Identity.Sub != u.Sub {
		t.Fatalf("refresh %+v", r)
	}
	// A refresh whose ID token names someone else isn't the pinned owner.
	f.MutateNextClaims(func(c map[string]any) { c["sub"] = "999" })
	_, err = api.Refresh(context.Background(), f.Credentials(), google.Owner, tok.Refresh, u.Sub)
	code(t, err, google.CodeUnknown)
	// Google sending a new refresh token anyway: dropped, never stored.
	f.MutateNextToken(func(a map[string]any) { a["refresh_token"] = "1//rotated" })
	r, err = api.Refresh(context.Background(), f.Credentials(), google.Owner, tok.Refresh, u.Sub)
	if err != nil || r.Refresh != "" {
		t.Fatal(r, err)
	}
	if _, err := api.Refresh(context.Background(), f.Credentials(), google.Owner, tok.Refresh, ""); err == nil {
		t.Fatal("owner refresh without a pinned sub")
	}

	f.Revoke(u.Email)
	_, err = api.Refresh(context.Background(), f.Credentials(), google.Owner, tok.Refresh, u.Sub)
	code(t, err, google.CodeInvalidGrant)
}

func TestMailboxExchange(t *testing.T) {
	f := googletest.New(t)
	f.AddUser("j@example.com")
	api := f.API()
	_, tok := consent(t, f, api, google.Mailbox, "j@example.com")
	if tok.Identity != nil || tok.Refresh == "" {
		t.Fatalf("%+v", tok)
	}
	addr, err := api.Profile(context.Background(), tok.Access)
	if err != nil || addr != "j@example.com" {
		t.Fatal(addr, err)
	}
	r, err := api.Refresh(context.Background(), f.Credentials(), google.Mailbox, tok.Refresh, "")
	if err != nil || r.Identity != nil {
		t.Fatal(r, err)
	}
}

// Every token answer, exchange or refresh, is held to D2's rules.
func TestTokenAnswerRules(t *testing.T) {
	f := googletest.New(t)
	f.AddUser("o@example.com")
	f.AddUser("j@example.com")
	api := f.API()
	_, owner := consent(t, f, api, google.Owner, "o@example.com")
	_, mbox := consent(t, f, api, google.Mailbox, "j@example.com")

	exchange := func(kind google.Kind, email string) error {
		c, _ := google.NewConsent(f.Credentials(), kind, email)
		raw, _ := f.Approve(c.URL(), email)
		q, _ := url.ParseQuery(raw)
		_, err := api.Exchange(context.Background(), c, q.Get("code"))
		return err
	}
	refresh := func(kind google.Kind) error {
		var err error
		if kind == google.Owner {
			_, err = api.Refresh(context.Background(), f.Credentials(), kind, owner.Refresh, owner.Identity.Sub)
		} else {
			_, err = api.Refresh(context.Background(), f.Credentials(), kind, mbox.Refresh, "")
		}
		return err
	}
	set := func(k string, v any) func(map[string]any) {
		return func(a map[string]any) {
			if v == nil {
				delete(a, k)
			} else {
				a[k] = v
			}
		}
	}
	const pubsub = "https://www.googleapis.com/auth/pubsub"
	for _, c := range []struct {
		name string
		kind google.Kind
		mut  func(map[string]any)
		want google.Code
		exch bool // exchange only
	}{
		{"extra scope", google.Mailbox, set("scope", google.ScopeMetadata+" https://www.googleapis.com/auth/gmail.modify"), google.CodeScope, false},
		{"other scope", google.Mailbox, set("scope", "https://www.googleapis.com/auth/gmail.readonly"), google.CodeScope, false},
		{"no scope", google.Mailbox, set("scope", nil), google.CodeScope, false},
		{"owner missing pubsub", google.Owner, set("scope", "openid email"), google.CodeScope, false},
		{"owner plus gmail", google.Owner, set("scope", "openid email "+pubsub+" "+google.ScopeMetadata), google.CodeScope, false},
		{"token_type", google.Mailbox, set("token_type", "mac"), google.CodeUnknown, false},
		{"no token_type", google.Mailbox, set("token_type", nil), google.CodeUnknown, false},
		{"expires_in 0", google.Mailbox, set("expires_in", 0), google.CodeUnknown, false},
		{"expires_in over 24h", google.Mailbox, set("expires_in", 86401), google.CodeUnknown, false},
		{"expires_in string", google.Mailbox, set("expires_in", "3599"), google.CodeUnknown, false},
		{"expires_in fraction", google.Mailbox, set("expires_in", 3599.5), google.CodeUnknown, false},
		{"no expires_in", google.Mailbox, set("expires_in", nil), google.CodeUnknown, false},
		{"no access_token", google.Mailbox, set("access_token", nil), google.CodeUnknown, false},
		{"access_token with space", google.Mailbox, set("access_token", "ya29 x"), google.CodeUnknown, false},
		{"no refresh_token", google.Mailbox, set("refresh_token", nil), google.CodeUnknown, true},
		{"no id_token", google.Owner, set("id_token", nil), google.CodeUnknown, true},
		{"garbage id_token", google.Owner, set("id_token", "a.b.c"), google.CodeUnknown, false},
	} {
		f.MutateNextToken(c.mut)
		email := "j@example.com"
		if c.kind == google.Owner {
			email = "o@example.com"
		}
		code(t, exchange(c.kind, email), c.want)
		if !c.exch {
			f.MutateNextToken(c.mut)
			if err := refresh(c.kind); google.CodeOf(err) != c.want {
				t.Errorf("%s (refresh): %v", c.name, err)
			}
		}
	}
	// An exchange's ID token: wrong audience, nonce, issuer, expiry.
	for name, mut := range map[string]func(map[string]any){
		"aud":      func(c map[string]any) { c["aud"] = "other.apps.googleusercontent.com" },
		"azp":      func(c map[string]any) { c["azp"] = "other.apps.googleusercontent.com" },
		"nonce":    func(c map[string]any) { c["nonce"] = "replayed" },
		"no nonce": func(c map[string]any) { delete(c, "nonce") },
		"iss":      func(c map[string]any) { c["iss"] = "https://evil.example.com" },
		"exp":      func(c map[string]any) { c["exp"] = f.Clock.Now().Unix() - 1 },
		"no sub":   func(c map[string]any) { delete(c, "sub") },
	} {
		f.MutateNextClaims(mut)
		if err := exchange(google.Owner, "o@example.com"); google.CodeOf(err) != google.CodeUnknown {
			t.Errorf("claims %s: %v", name, err)
		}
	}
	// Duplicate keys, trailing data, an oversized answer: not read.
	for _, body := range []string{
		`{"access_token":"a","access_token":"b","expires_in":3599,"token_type":"Bearer","scope":"` + google.ScopeMetadata + `"}`,
		`{"access_token":"a","expires_in":3599,"token_type":"Bearer","scope":"` + google.ScopeMetadata + `"}{}`,
		`{"access_token":"a","expires_in":3599,"token_type":"Bearer","scope":"` + google.ScopeMetadata + `","pad":"` + strings.Repeat("x", google.MaxBody) + `"}`,
		`[]`,
	} {
		f.Fail(google.OpRefresh, googletest.Failure{Status: 200, Body: body})
		code(t, refresh(google.Mailbox), google.CodeUnknown)
	}
	// Nothing above was a degraded success: the clean path still works.
	if err := refresh(google.Mailbox); err != nil {
		t.Fatal(err)
	}
}

func TestCodeIsOneShotAndBoundToVerifier(t *testing.T) {
	f := googletest.New(t)
	f.AddUser("j@example.com")
	api := f.API()
	c, _ := google.NewConsent(f.Credentials(), google.Mailbox, "j@example.com")
	raw, _ := f.Approve(c.URL(), "j@example.com")
	q, _ := url.ParseQuery(raw)
	// Another session's verifier: refused, and the code is spent.
	other, _ := google.NewConsent(f.Credentials(), google.Mailbox, "j@example.com")
	_, err := api.Exchange(context.Background(), other, q.Get("code"))
	code(t, err, google.CodeInvalidGrant)
	_, err = api.Exchange(context.Background(), c, q.Get("code"))
	code(t, err, google.CodeInvalidGrant)
	// A fresh code works once.
	raw, _ = f.Approve(c.URL(), "j@example.com")
	q, _ = url.ParseQuery(raw)
	if _, err := api.Exchange(context.Background(), c, q.Get("code")); err != nil {
		t.Fatal(err)
	}
	_, err = api.Exchange(context.Background(), c, q.Get("code"))
	code(t, err, google.CodeInvalidGrant)
}

// Code, verifier, refresh token and client secret go in POST bodies only;
// access tokens in the Authorization header; every request to its host.
func TestWhereSecretsGo(t *testing.T) {
	f := googletest.New(t)
	f.AddUser("o@example.com")
	f.AddUser("j@example.com")
	api := f.API()
	_, owner := consent(t, f, api, google.Owner, "o@example.com")
	_, mbox := consent(t, f, api, google.Mailbox, "j@example.com")
	api.Refresh(context.Background(), f.Credentials(), google.Mailbox, mbox.Refresh, "")
	api.Profile(context.Background(), mbox.Access)
	api.ProbeTopics(context.Background(), owner.Access, f.Project)
	hosts := map[google.Op]string{google.OpExchange: google.TokenHost, google.OpRefresh: google.TokenHost,
		google.OpProfile: google.GmailHost, google.OpTopicList: google.PubSubHost}
	for _, r := range f.Requests() {
		if hosts[r.Op] != r.Host {
			t.Errorf("%s went to %s", r.Op, r.Host)
		}
		for _, secret := range []string{f.Secret, owner.Refresh, mbox.Refresh, owner.Access, mbox.Access} {
			if strings.Contains(r.Path, secret) || strings.Contains(r.Query, secret) {
				t.Errorf("%s: a secret in the URL", r.Op)
			}
		}
		switch r.Op {
		case google.OpExchange, google.OpRefresh:
			if !strings.Contains(string(r.Body), "client_secret=") || r.Header.Get("Authorization") != "" {
				t.Errorf("%s: secret not in the body, or a bearer sent", r.Op)
			}
		default:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ya29.") {
				t.Errorf("%s: no bearer", r.Op)
			}
		}
	}
}

// setup is an owner and a mailbox token on the fake.
func setup(t *testing.T) (*googletest.Fake, *google.API, google.Token, google.Token) {
	t.Helper()
	f := googletest.New(t)
	f.AddUser("o@example.com")
	f.AddUser("j@example.com")
	api := f.API()
	_, owner := consent(t, f, api, google.Owner, "o@example.com")
	_, mbox := consent(t, f, api, google.Mailbox, "j@example.com")
	return f, api, owner, mbox
}

func TestProvisionAndWatch(t *testing.T) {
	f, api, owner, mbox := setup(t)
	ctx := context.Background()
	res, ok := google.Resource("personal")
	if !ok {
		t.Fatal("resource")
	}
	if err := api.ProbeTopics(ctx, owner.Access, f.Project); err != nil {
		t.Fatal(err)
	}
	code(t, api.ProbeTopics(ctx, owner.Access, "pneu-other-1"), google.CodePermission)
	// A mailbox token can't reach Pub/Sub, nor the owner's Gmail.
	code(t, api.ProbeTopics(ctx, mbox.Access, f.Project), google.CodeScope)
	_, err := api.Profile(ctx, owner.Access)
	code(t, err, google.CodeScope)

	// Watching before the topic exists, or before Gmail may publish to it.
	_, err = api.Watch(ctx, mbox.Access, f.Project, res)
	code(t, err, google.CodeNotFound)
	if created, err := api.EnsureTopic(ctx, owner.Access, f.Project, res); err != nil || !created {
		t.Fatal(created, err)
	}
	if created, err := api.EnsureTopic(ctx, owner.Access, f.Project, res); err != nil || created {
		t.Fatal("second EnsureTopic:", created, err)
	}
	_, err = api.Watch(ctx, mbox.Access, f.Project, res)
	code(t, err, google.CodePermission)

	if changed, err := api.GrantPublisher(ctx, owner.Access, f.Project, res); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, err := api.GrantPublisher(ctx, owner.Access, f.Project, res); err != nil || changed {
		t.Fatal("second grant:", changed, err)
	}
	if n := f.PolicyWrites(res); n != 1 {
		t.Fatalf("%d policy writes", n)
	}
	if created, err := api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install); err != nil || !created {
		t.Fatal(created, err)
	}
	if created, err := api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install); err != nil || created {
		t.Fatal("second EnsureSubscription:", created, err)
	}

	exp, err := api.Watch(ctx, mbox.Access, f.Project, res)
	if err != nil {
		t.Fatal(err)
	}
	if want := f.Clock.Now().Add(googletest.WatchLife); !exp.Equal(want.Truncate(time.Millisecond)) {
		t.Fatalf("expiration %v, want %v", exp, want)
	}
	if topic, _, ok := f.Watch("j@example.com"); !ok || topic != "projects/"+f.Project+"/topics/"+res {
		t.Fatal("watch", topic, ok)
	}
	// The watch published once at once (C4); new mail publishes again.
	f.Notify("j@example.com")
	ids, err := api.Pull(ctx, owner.Access, f.Project, res)
	if err != nil || len(ids) != 2 {
		t.Fatal(ids, err)
	}
	if err := api.Ack(ctx, owner.Access, f.Project, res, ids); err != nil {
		t.Fatal(err)
	}
	if f.Acked(res) != 2 || f.Outstanding(res) != 0 {
		t.Fatal("acks", f.Acked(res), f.Outstanding(res))
	}
	if err := api.Stop(ctx, mbox.Access); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.Watch("j@example.com"); ok || f.Stops("j@example.com") != 1 {
		t.Fatal("stop")
	}
	if f.Notify("j@example.com") {
		t.Fatal("a stopped watch published")
	}
}

func TestWatchExpirationBounded(t *testing.T) {
	f, api, owner, mbox := setup(t)
	ctx := context.Background()
	res, _ := google.Resource("personal")
	api.EnsureTopic(ctx, owner.Access, f.Project, res)
	api.GrantPublisher(ctx, owner.Access, f.Project, res)
	for _, body := range []string{
		`{"historyId":"1","expiration":"14319900982000"}`,
		`{"historyId":"1","expiration":1431990098200}`,
		`{"historyId":"1","expiration":"-1"}`,
		`{"historyId":"1","expiration":"0"}`,
		`{"historyId":"1"}`,
		`{"historyId":"1","expiration":"1","expiration":"2"}`,
	} {
		f.Fail(google.OpWatch, googletest.Failure{Status: 200, Body: body})
		_, err := api.Watch(ctx, mbox.Access, f.Project, res)
		code(t, err, google.CodeUnknown)
	}
}

func TestPolicyKeepsWhatItDoesntModel(t *testing.T) {
	f, api, owner, _ := setup(t)
	ctx := context.Background()
	res, _ := google.Resource("personal")
	api.EnsureTopic(ctx, owner.Access, f.Project, res)
	f.SetPolicy(res, `{"version":3,"bindings":[`+
		`{"role":"roles/pubsub.publisher","members":["serviceAccount:gmail-api-push@system.gserviceaccount.com"],"condition":{"title":"until 2030","expression":"request.time < timestamp('2030-01-01T00:00:00Z')"}},`+
		`{"role":"roles/pubsub.subscriber","members":["user:o@example.com"]}],"futureField":{"x":1}}`)
	if changed, err := api.GrantPublisher(ctx, owner.Access, f.Project, res); err != nil || !changed {
		t.Fatal("a conditional binding doesn't count:", changed, err)
	}
	got := f.Policy(res)
	for _, want := range []string{`"condition":{"title":"until 2030"`, `"roles/pubsub.subscriber"`, `"futureField":{"x":1}`, `"version":3`} {
		if !strings.Contains(got, want) {
			t.Errorf("policy lost %s: %s", want, got)
		}
	}
	if strings.Count(got, google.PublisherMember) != 2 {
		t.Fatalf("want the conditional and an unconditional binding: %s", got)
	}
	// On the wire: the policy is read at version 3 and written at 3.
	for _, r := range f.Requests() {
		switch r.Op {
		case google.OpGetPolicy:
			if q, _ := url.ParseQuery(r.Query); q.Get("options.requestedPolicyVersion") != "3" {
				t.Errorf("getIamPolicy query %q", r.Query)
			}
		case google.OpSetPolicy:
			if !strings.Contains(string(r.Body), `"version":3`) || !strings.Contains(string(r.Body), `"etag":`) {
				t.Errorf("setIamPolicy body %s", r.Body)
			}
		}
	}

	// A policy pneu can't round-trip is refused, never written.
	other, _ := google.Resource("vocal")
	api.EnsureTopic(ctx, owner.Access, f.Project, other)
	f.Fail(google.OpGetPolicy, googletest.Failure{Status: 200, Body: `{"etag":"e","bindings":[{"role":"r","role":"s","members":[]}]}`})
	_, err := api.GrantPublisher(ctx, owner.Access, f.Project, other)
	code(t, err, google.CodeUnknown)
	if f.Calls(google.OpSetPolicy) != 1 {
		t.Fatal("an unreadable policy was written")
	}
}

func TestPolicyEtagConflict(t *testing.T) {
	f, api, owner, _ := setup(t)
	ctx := context.Background()
	res, _ := google.Resource("personal")
	api.EnsureTopic(ctx, owner.Access, f.Project, res)

	f.RacePolicy(res, google.PolicyAttempts-1)
	if changed, err := api.GrantPublisher(ctx, owner.Access, f.Project, res); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if gets := f.Calls(google.OpGetPolicy); gets != google.PolicyAttempts {
		t.Fatalf("%d reads", gets)
	}

	other, _ := google.Resource("vocal")
	api.EnsureTopic(ctx, owner.Access, f.Project, other)
	before := f.Calls(google.OpGetPolicy)
	f.RacePolicy(other, 100)
	_, err := api.GrantPublisher(ctx, owner.Access, f.Project, other)
	code(t, err, google.CodeConflict)
	if gets := f.Calls(google.OpGetPolicy) - before; gets != google.PolicyAttempts {
		t.Fatalf("%d reads, want the bound %d", gets, google.PolicyAttempts)
	}
	if f.PolicyWrites(other) != 0 {
		t.Fatal("a conflicted write landed")
	}
}

func TestOrgPolicyRefusesBinding(t *testing.T) {
	f, api, owner, _ := setup(t)
	ctx := context.Background()
	res, _ := google.Resource("personal")
	api.EnsureTopic(ctx, owner.Access, f.Project, res)
	f.FailCode(google.OpSetPolicy, google.CodeOrgPolicy, 1)
	_, err := api.GrantPublisher(ctx, owner.Access, f.Project, res)
	code(t, err, google.CodeOrgPolicy)
}

func TestSubscriptionMismatch(t *testing.T) {
	f, api, owner, _ := setup(t)
	ctx := context.Background()
	res, _ := google.Resource("personal")
	api.EnsureTopic(ctx, owner.Access, f.Project, res)
	name := "projects/" + f.Project + "/subscriptions/" + res
	topic := "projects/" + f.Project + "/topics/" + res
	plant := func(extra string) {
		f.SetSubscription(res, res, `{"name":"`+name+`","topic":"`+topic+`","pushConfig":{},"ackDeadlineSeconds":30,`+
			`"messageRetentionDuration":"3600s","expirationPolicy":{},"state":"ACTIVE"`+extra+`}`)
	}
	var me *google.MismatchError
	plant(`,"labels":{"pneu-install":"fedcba9876543210"}`)
	_, err := api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install)
	if !errors.As(err, &me) || !me.OtherInstall {
		t.Fatalf("another server's: %v", err)
	}
	plant(`,"labels":{"pneu-install":"` + install + `"},"filter":"attributes.a=\"b\""`)
	_, err = api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install)
	if !errors.As(err, &me) || me.Field != google.FieldFilter {
		t.Fatalf("filter: %v", err)
	}
	plant(`,"labels":{"pneu-install":"` + install + `"},"bigtableConfig":{"table":"projects/p/instances/i/tables/t"}`)
	_, err = api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install)
	if !errors.As(err, &me) || me.Field != google.FieldBigtable {
		t.Fatalf("bigtable export: %v", err)
	}
	plant(`,"labels":{"pneu-install":"` + install + `"}`)
	if created, err := api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install); err != nil || created {
		t.Fatal(created, err)
	}

	// A create whose answer dropped a field: caught.
	other, _ := google.Resource("vocal")
	api.EnsureTopic(ctx, owner.Access, f.Project, other)
	f.Fail(google.OpSubMake, googletest.Failure{Status: 200, Body: `{"name":"projects/` + f.Project + `/subscriptions/` + other +
		`","topic":"projects/` + f.Project + `/topics/` + other + `","pushConfig":{},"ackDeadlineSeconds":30,` +
		`"messageRetentionDuration":"3600s","labels":{"pneu-install":"` + install + `"},"expirationPolicy":{"ttl":"2678400s"}}`})
	_, err = api.EnsureSubscription(ctx, owner.Access, f.Project, other, other, install)
	if !errors.As(err, &me) || me.Field != google.FieldExpiration {
		t.Fatalf("dropped expirationPolicy: %v", err)
	}
	if strings.Contains(err.Error(), "2678400") {
		t.Fatal("Google's value in the error")
	}
}

func TestPull(t *testing.T) {
	f, api, owner, _ := setup(t)
	ctx := context.Background()
	res, _ := google.Resource("personal")
	api.EnsureTopic(ctx, owner.Access, f.Project, res)
	api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install)

	ids, err := api.Pull(ctx, owner.Access, f.Project, res)
	if err != nil || len(ids) != 0 {
		t.Fatal("empty pull:", ids, err)
	}
	f.Publish(res, 15)
	ids, err = api.Pull(ctx, owner.Access, f.Project, res)
	if err != nil || len(ids) != google.MaxMessages {
		t.Fatal(len(ids), err)
	}
	// A failed ack leaves them outstanding; redelivery brings them back.
	f.FailCode(google.OpAck, google.CodeUnavailable, 1)
	code(t, api.Ack(ctx, owner.Access, f.Project, res, ids), google.CodeUnavailable)
	f.Redeliver(res)
	if f.Queued(res) != 15 {
		t.Fatal("redelivery", f.Queued(res))
	}
	// A pull held open is answered when a message arrives.
	for f.Queued(res) > 0 {
		ids, _ := api.Pull(ctx, owner.Access, f.Project, res)
		api.Ack(ctx, owner.Access, f.Project, res, ids)
	}
	f.PullHold = 5 * time.Second
	got := make(chan []string, 1)
	go func() {
		ids, _ := api.Pull(ctx, owner.Access, f.Project, res)
		got <- ids
	}()
	time.Sleep(50 * time.Millisecond)
	f.Publish(res, 1)
	select {
	case ids := <-got:
		if len(ids) != 1 {
			t.Fatal(ids)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("held pull not answered")
	}

	// Out of shape: too many, an oversized or missing ack ID, duplicate keys.
	many := `{"receivedMessages":[` + strings.TrimSuffix(strings.Repeat(`{"ackId":"a"},`, 11), ",") + `]}`
	for _, body := range []string{
		many,
		`{"receivedMessages":[{"ackId":"` + strings.Repeat("a", google.MaxAckID+1) + `"}]}`,
		`{"receivedMessages":[{"message":{}}]}`,
		`{"receivedMessages":[{"ackId":"a b"}]}`,
		`{"receivedMessages":[{"ackId":"a","ackId":"b"}]}`,
		`{"receivedMessages":[],"receivedMessages":[]}`,
		`{"receivedMessages":{}}`,
	} {
		f.Fail(google.OpPull, googletest.Failure{Status: 200, Body: body})
		_, err := api.Pull(ctx, owner.Access, f.Project, res)
		code(t, err, google.CodeUnknown)
	}
	// Ack bounds are checked before anything is sent.
	n := f.Calls(google.OpAck)
	for _, ids := range [][]string{nil, make([]string, 11), {"a b"}, {strings.Repeat("a", 513)}} {
		for i := range ids {
			if ids[i] == "" {
				ids[i] = "a"
			}
		}
		code(t, api.Ack(ctx, owner.Access, f.Project, res, ids), google.CodeUnknown)
	}
	if f.Calls(google.OpAck) != n {
		t.Fatal("an out-of-bounds ack was sent")
	}
}

// Names that would build another path are refused before any request.
func TestNamesGuardPaths(t *testing.T) {
	f, api, owner, mbox := setup(t)
	ctx := context.Background()
	n := len(f.Requests())
	for _, bad := range []string{"pneu-../x", "pneu-a/b", "other", "pneu-a?b", ""} {
		if bad == "" {
			continue
		}
		code(t, api.GetTopic(ctx, owner.Access, f.Project, bad), google.CodeUnknown)
		_, err := api.Pull(ctx, owner.Access, f.Project, bad)
		code(t, err, google.CodeUnknown)
		_, err = api.Watch(ctx, mbox.Access, f.Project, bad)
		code(t, err, google.CodeUnknown)
	}
	code(t, api.ProbeTopics(ctx, owner.Access, "x/../y"), google.CodeUnknown)
	code(t, api.Stop(ctx, "tok en"), google.CodeUnknown)
	if len(f.Requests()) != n {
		t.Fatal("a bad name reached Google")
	}
}

// Every failure is only an op and a code: none of Google's words.
func TestFailuresCarryNoGoogleText(t *testing.T) {
	f, api, owner, mbox := setup(t)
	ctx := context.Background()
	for _, c := range google.Codes {
		if c == google.CodeNetwork {
			continue
		}
		f.FailCode(google.OpProfile, c, 1)
		_, err := api.Profile(ctx, mbox.Access)
		if err == nil || strings.Contains(err.Error(), "Google") || strings.Contains(err.Error(), "words") {
			t.Fatalf("%s: %v", c, err)
		}
	}
	f.FailCode(google.OpRefresh, google.CodeOrgPolicy, 1)
	_, err := api.Refresh(ctx, f.Credentials(), google.Owner, owner.Refresh, owner.Identity.Sub)
	code(t, err, google.CodeOrgPolicy)
}

func TestTransportFailures(t *testing.T) {
	f, _, owner, mbox := setup(t)
	api := f.NewAPI(google.Options{Timeout: 200 * time.Millisecond})
	ctx := context.Background()

	f.Fail(google.OpProfile, googletest.Failure{Drop: true})
	_, err := api.Profile(ctx, mbox.Access)
	code(t, err, google.CodeNetwork)

	f.Fail(google.OpProfile, googletest.Failure{Stall: true})
	start := time.Now()
	_, err = api.Profile(ctx, mbox.Access)
	code(t, err, google.CodeNetwork)
	if time.Since(start) > 3*time.Second {
		t.Fatal("stall outlived the timeout")
	}

	// The caller's own cancellation comes back as the context's error.
	f.Fail(google.OpProfile, googletest.Failure{Stall: true})
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err = api.Profile(cctx, mbox.Access)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}

	// A redirect is an answer, never followed.
	n := len(f.Requests())
	f.Fail(google.OpTopicList, googletest.Failure{Status: http.StatusFound, Body: ""})
	code(t, api.ProbeTopics(ctx, owner.Access, f.Project), google.CodeUnknown)
	if len(f.Requests()) != n+1 {
		t.Fatal("redirect followed")
	}
	// An oversized answer, success or failure, isn't read.
	f.Fail(google.OpProfile, googletest.Failure{Status: 200, Body: `{"emailAddress":"j@example.com","pad":"` + strings.Repeat("x", google.MaxBody) + `"}`})
	_, err = api.Profile(ctx, mbox.Access)
	code(t, err, google.CodeUnknown)
	f.Fail(google.OpProfile, googletest.Failure{Status: 403, Body: strings.Repeat("x", google.MaxBody+1)})
	_, err = api.Profile(ctx, mbox.Access)
	code(t, err, google.CodeUnknown)
	// A profile that isn't an address.
	f.Fail(google.OpProfile, googletest.Failure{Status: 200, Body: `{"emailAddress":"j@example.com\n"}`})
	_, err = api.Profile(ctx, mbox.Access)
	code(t, err, google.CodeUnknown)
}

// A pull is bounded by PullTimeout, not the shorter per-call Timeout.
func TestPullTimeout(t *testing.T) {
	f, _, owner, _ := setup(t)
	ctx := context.Background()
	api := f.NewAPI(google.Options{Timeout: 50 * time.Millisecond, PullTimeout: 400 * time.Millisecond})
	res, _ := google.Resource("personal")
	api.EnsureTopic(ctx, owner.Access, f.Project, res)
	api.EnsureSubscription(ctx, owner.Access, f.Project, res, res, install)
	f.Fail(google.OpPull, googletest.Failure{Stall: true})
	start := time.Now()
	_, err := api.Pull(ctx, owner.Access, f.Project, res)
	code(t, err, google.CodeNetwork)
	if d := time.Since(start); d < 300*time.Millisecond || d > 3*time.Second {
		t.Fatalf("pull gave up after %v", d)
	}
}
