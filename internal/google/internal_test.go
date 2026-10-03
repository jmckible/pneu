package google

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		oauth  bool
		want   Code
	}{
		{400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, true, CodeInvalidGrant},
		{400, `{"error":"invalid_scope"}`, true, CodeScope},
		{400, `{"error":"admin_policy_enforced"}`, true, CodeOrgPolicy},
		{401, `{"error":"invalid_client"}`, true, CodePermission},
		{400, `{"error":"something_new"}`, true, CodeUnknown},
		{503, `{"error":"temporarily_unavailable"}`, true, CodeUnavailable},
		{403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":"SERVICE_DISABLED"}]}}`, false, CodeAPIDisabled},
		{403, `{"error":{"code":403,"errors":[{"reason":"accessNotConfigured"}]}}`, false, CodeAPIDisabled},
		{403, `{"error":{"code":403,"details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`, false, CodeScope},
		{403, `{"error":{"code":403,"errors":[{"reason":"insufficientPermissions"}]}}`, false, CodeScope},
		{403, `{"error":{"code":403,"errors":[{"reason":"forbidden"}]}}`, false, CodePermission},
		{403, `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`, false, CodeQuota},
		{400, `{"error":{"status":"FAILED_PRECONDITION","details":[{"violations":[{"type":"constraints/iam.allowedPolicyMemberDomains"}]}]}}`, false, CodeOrgPolicy},
		{400, `{"error":{"details":[{"reason":"ORG_POLICY_CONSTRAINT_FAILED"}]}}`, false, CodeOrgPolicy},
		{404, `{"error":{"status":"NOT_FOUND"}}`, false, CodeNotFound},
		{409, `{"error":{"status":"ALREADY_EXISTS"}}`, false, CodeConflict},
		{409, `{"error":{"status":"ABORTED"}}`, false, CodeConflict},
		{412, ``, false, CodeConflict},
		{429, ``, false, CodeQuota},
		{500, `not json`, false, CodeUnavailable},
		{502, ``, false, CodeUnavailable},
		{401, `{"error":{"status":"UNAUTHENTICATED"}}`, false, CodeUnknown},
		{400, `{"error":{"status":"INVALID_ARGUMENT"}}`, false, CodeUnknown},
		{302, ``, false, CodeUnknown},
	} {
		if got := classify(c.status, []byte(c.body), c.oauth); got != c.want {
			t.Errorf("classify(%d, %s) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

func TestErrorCarriesNothingElse(t *testing.T) {
	e := &Error{Op: OpWatch, Code: CodePermission}
	if e.Error() != "google: gmail.watch: permission" {
		t.Fatal(e.Error())
	}
	if n := reflect.TypeOf(*e).NumField(); n != 2 {
		t.Fatalf("Error has %d fields; it may carry only Op and Code", n)
	}
	if CodeOf(e) != CodePermission || CodeOf(nil) != "" || CodeOf(errors.New("x")) != CodeUnknown {
		t.Fatal("CodeOf")
	}
}

func TestParseMillis(t *testing.T) {
	for s, ok := range map[string]bool{
		"1431990098200": true, "1": true, "": false, "0": false, "01": false, "-1": false,
		"14319900982000": false, "1e12": false, "１２": false, " 1": false,
	} {
		if _, got := parseMillis(s); got != ok {
			t.Errorf("parseMillis(%q) = %v", s, got)
		}
	}
	if ms, _ := parseMillis("1431990098200"); !time.UnixMilli(ms).Equal(time.Date(2015, 5, 18, 23, 1, 38, 200e6, time.UTC)) {
		t.Fatal(time.UnixMilli(ms))
	}
}

func TestSameScopes(t *testing.T) {
	for _, c := range []struct {
		scope string
		kind  Kind
		ok    bool
	}{
		{"openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/pubsub", Owner, true},
		{"https://www.googleapis.com/auth/pubsub openid email", Owner, true},
		{"openid https://www.googleapis.com/auth/pubsub", Owner, false},
		{"openid email https://www.googleapis.com/auth/pubsub https://www.googleapis.com/auth/gmail.metadata", Owner, false},
		{"openid email email https://www.googleapis.com/auth/pubsub", Owner, false},
		{"openid email https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/pubsub", Owner, false},
		{"https://www.googleapis.com/auth/gmail.metadata", Mailbox, true},
		{"https://www.googleapis.com/auth/gmail.readonly", Mailbox, false},
		{"https://www.googleapis.com/auth/gmail.metadata https://www.googleapis.com/auth/gmail.modify", Mailbox, false},
		{"", Mailbox, false},
	} {
		if got := sameScopes(c.scope, c.kind); got != c.ok {
			t.Errorf("sameScopes(%q, %v) = %v", c.scope, c.kind, got)
		}
	}
}

// policy reads a policy back as plain JSON values, for comparison.
func policy(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

func TestAddPublisher(t *testing.T) {
	// Empty policy: a binding appears, version 3, etag kept.
	next, add, err := addPublisher([]byte(`{"etag":"ACAB"}`))
	if err != nil || !add {
		t.Fatal(add, err)
	}
	if got, want := policy(t, next), policy(t, []byte(`{"etag":"ACAB","version":3,"bindings":[{"role":"roles/pubsub.publisher","members":["serviceAccount:gmail-api-push@system.gserviceaccount.com"]}]}`)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}

	// Everything else round-trips untouched: other bindings, a conditional
	// publisher binding (kept, and not counted), unknown fields at both
	// levels, auditConfigs, key order.
	in := `{"version":3,"etag":"BwXyz=","futureField":{"a":[1,2,{"b":null}]},` +
		`"bindings":[` +
		`{"role":"roles/pubsub.viewer","members":["user:a@example.com","group:g@example.com"],"x-extra":true},` +
		`{"role":"roles/pubsub.publisher","members":["serviceAccount:gmail-api-push@system.gserviceaccount.com"],` +
		`"condition":{"title":"t","expression":"request.time < timestamp(\"2030-01-01T00:00:00Z\")"}},` +
		`{"role":"roles/pubsub.publisher","members":["user:b@example.com"],"note":"keep"}],` +
		`"auditConfigs":[{"service":"allServices"}]}`
	next, add, err = addPublisher([]byte(in))
	if err != nil || !add {
		t.Fatal(add, err)
	}
	want := strings.Replace(in, `"members":["user:b@example.com"]`,
		`"members":["user:b@example.com","serviceAccount:gmail-api-push@system.gserviceaccount.com"]`, 1)
	if string(next) != want {
		t.Fatalf("round trip changed more than the member:\n got %s\nwant %s", next, want)
	}

	// Version 1 becomes 3; whitespace is compacted, nothing else changes.
	next, _, err = addPublisher([]byte("{ \"etag\" : \"e\", \"version\": 1, \"bindings\": [ ] }"))
	if err != nil || string(next) != `{"etag":"e","version":3,"bindings":[{"role":"roles/pubsub.publisher","members":["serviceAccount:gmail-api-push@system.gserviceaccount.com"]}]}` {
		t.Fatalf("%s %v", next, err)
	}

	// Already there unconditionally: nothing to write.
	_, add, err = addPublisher([]byte(`{"etag":"e","version":1,"bindings":[{"role":"roles/pubsub.publisher","members":["user:x@example.com","serviceAccount:gmail-api-push@system.gserviceaccount.com"]}]}`))
	if err != nil || add {
		t.Fatal("present binding rewritten", add, err)
	}

	// What can't be preserved is refused.
	for _, bad := range []string{
		`{"etag":"e","etag":"f"}`,
		`{"bindings":[]}`,
		`{"etag":5}`,
		`{"etag":"e","version":2}`,
		`{"etag":"e","version":4}`,
		`{"etag":"e","version":"3"}`,
		`{"etag":"e","bindings":{}}`,
		`{"etag":"e","bindings":[{"members":["user:a@example.com"]}]}`,
		`{"etag":"e","bindings":[{"role":"r","role":"s","members":[]}]}`,
		`{"etag":"e","bindings":[{"role":"r","members":"user:a"}]}`,
		`{"etag":"e","bindings":[{"role":"r","members":[1]}]}`,
		`{"etag":"e","version":1,"bindings":[{"role":"r","members":[],"condition":{}}]}`,
		`{"etag":"e"} {}`,
		`{"etag":"e"} x`,
		`[]`,
		`{"etag":"e",}`,
	} {
		if _, _, err := addPublisher([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestCompareSubscription(t *testing.T) {
	const (
		project = "pneu-push-123"
		install = "0123456789abcdef"
	)
	good := map[string]any{
		"name":                     "projects/pneu-push-123/subscriptions/pneu-personal",
		"topic":                    "projects/pneu-push-123/topics/pneu-personal",
		"pushConfig":               map[string]any{},
		"ackDeadlineSeconds":       30,
		"messageRetentionDuration": "3600s",
		"labels":                   map[string]any{"pneu-install": install, "other": "x"},
		"expirationPolicy":         map[string]any{},
		"state":                    "ACTIVE",
		"retryPolicy":              map[string]any{"minimumBackoff": "10s"},
	}
	enc := func(m map[string]any) []byte {
		b, _ := json.Marshal(m)
		return b
	}
	cmp := func(m map[string]any) error {
		return compareSubscription(enc(m), project, "pneu-personal", "pneu-personal", install)
	}
	if err := cmp(good); err != nil {
		t.Fatal(err)
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range good {
			m[a] = b
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct {
		m     map[string]any
		field string
		other bool
	}{
		{with("name", "projects/pneu-push-123/subscriptions/other"), FieldName, false},
		{with("topic", "projects/pneu-push-123/topics/pneu-vocal"), FieldTopic, false},
		{with("topic", "_deleted-topic_"), FieldTopic, false},
		{with("pushConfig", map[string]any{"pushEndpoint": "https://example.com"}), FieldPush, false},
		{with("bigqueryConfig", map[string]any{"table": "t"}), FieldBigQuery, false},
		{with("cloudStorageConfig", map[string]any{"bucket": "b"}), FieldStorage, false},
		{with("ackDeadlineSeconds", 10), FieldAck, false},
		{with("ackDeadlineSeconds", nil), FieldAck, false},
		{with("messageRetentionDuration", "604800s"), FieldRetention, false},
		{with("labels", map[string]any{"pneu-install": "fedcba9876543210"}), FieldLabel, true},
		{with("labels", map[string]any{}), FieldLabel, false},
		{with("labels", nil), FieldLabel, false},
		{with("expirationPolicy", nil), FieldExpiration, false},
		{with("expirationPolicy", map[string]any{"ttl": "2678400s"}), FieldExpiration, false},
		{with("filter", "attributes.x = \"y\""), FieldFilter, false},
		{with("deadLetterPolicy", map[string]any{"deadLetterTopic": "t"}), FieldDeadLetter, false},
		{with("messageTransforms", []any{map[string]any{"javascriptUdf": map[string]any{}}}), FieldTransforms, false},
		{with("detached", true), FieldDetached, false},
	} {
		err := cmp(c.m)
		var me *MismatchError
		if !errors.As(err, &me) || me.Field != c.field || me.OtherInstall != c.other {
			t.Errorf("%v: got %v, want field %s other %v", c.m, err, c.field, c.other)
		}
	}
	// Empty forms of the optional ones match.
	for _, m := range []map[string]any{
		with("filter", ""), with("detached", false), with("messageTransforms", []any{}),
		with("deadLetterPolicy", map[string]any{}), with("expirationPolicy", map[string]any{"ttl": ""}),
	} {
		if err := cmp(m); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
	for _, bad := range []string{`{"name":"a","name":"b"}`, `[]`, `{"labels":{"pneu-install":"a","pneu-install":"b"}}`, `{"ackDeadlineSeconds":"30"}`} {
		var me *MismatchError
		if err := compareSubscription([]byte(bad), project, "pneu-personal", "pneu-personal", install); err == nil || errors.As(err, &me) {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestNames(t *testing.T) {
	for p, ok := range map[string]bool{"pneu-push-123": true, "abcdef": true, "abcde": false, "Pneu-push": false,
		"pneu-push-": false, "1pneu-push": false, strings.Repeat("a", 30): true, strings.Repeat("a", 31): false} {
		if ValidProject(p) != ok {
			t.Errorf("ValidProject(%q)", p)
		}
	}
	for a, ok := range map[string]bool{"personal": true, "a.b_c-d": true, "": false, ".x": false, "a b": false,
		strings.Repeat("a", 32): true, strings.Repeat("a", 33): false, "x/y": false} {
		if _, got := Resource(a); got != ok {
			t.Errorf("Resource(%q)", a)
		}
	}
	for a, ok := range map[string]bool{"j@example.com": true, "j.m+x@mail.example.co.uk": true, "j@localhost": false,
		"j@@example.com": false, "j @example.com": false, "j@exa_mple.com": false, "\"j\"@example.com": false} {
		if ValidAddress(a) != ok {
			t.Errorf("ValidAddress(%q)", a)
		}
	}
	if !ValidClientID("123456789-abc123.apps.googleusercontent.com") || ValidClientID("x.apps.googleusercontent.com") {
		t.Error("ValidClientID")
	}
}

func TestCheckIDToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	base := map[string]any{"iss": "https://accounts.google.com", "aud": "cid", "azp": "cid", "sub": "123",
		"email": "o@example.com", "email_verified": true, "exp": now.Unix() + 60, "nonce": "n0"}
	tok := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return "e30." + strings.TrimRight(b64(b), "=") + ".sig"
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	if id, err := checkIDToken(tok(base), "cid", "n0", "", now); err != nil || id.Sub != "123" || id.Email != "o@example.com" {
		t.Fatal(id, err)
	}
	if _, err := checkIDToken(tok(with("nonce", nil)), "cid", "", "123", now); err != nil {
		t.Fatal("refresh-style token with the pinned sub:", err)
	}
	for name, c := range map[string]struct {
		m          map[string]any
		nonce, sub string
	}{
		"iss":           {with("iss", "https://evil.example.com"), "n0", ""},
		"aud":           {with("aud", "other"), "n0", ""},
		"aud list":      {with("aud", []any{"cid", "other"}), "n0", ""},
		"azp":           {with("azp", "other"), "n0", ""},
		"exp past":      {with("exp", now.Unix()), "n0", ""},
		"exp missing":   {with("exp", nil), "n0", ""},
		"exp string":    {with("exp", "1900000000"), "n0", ""},
		"nonce":         {with("nonce", "n1"), "n0", ""},
		"nonce missing": {with("nonce", nil), "n0", ""},
		"sub pinned":    {base, "", "456"},
		"unbound":       {base, "", ""},
		"sub bad":       {with("sub", "12 3"), "n0", ""},
		"unverified":    {with("email_verified", false), "n0", ""},
		"email bad":     {with("email", "nope"), "n0", ""},
	} {
		if _, err := checkIDToken(tok(c.m), "cid", c.nonce, c.sub, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := checkIDToken(`e30.`+b64([]byte(`{"sub":"1","sub":"2"}`))+`.x`, "cid", "n0", "", now); err == nil {
		t.Error("duplicate claim accepted")
	}
	if _, err := checkIDToken("a.b", "cid", "n0", "", now); err == nil {
		t.Error("two segments accepted")
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// The transport takes no proxy from the environment and follows no
// redirect (C10).
func TestTransportFixed(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	a := New(Options{})
	if a.tr.Proxy != nil {
		t.Fatal("proxy set")
	}
	if a.hc.CheckRedirect == nil || a.hc.Jar != nil {
		t.Fatal("redirects or cookies")
	}
	for h, b := range a.bases {
		if b != "https://"+h {
			t.Fatalf("%s -> %s", h, b)
		}
	}
	if len(a.bases) != 3 {
		t.Fatal(a.bases)
	}
}
