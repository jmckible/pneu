package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// Code is the closed local vocabulary every Google failure is classified
// into (docs/push.md D6). Scopes don't grant IAM, so each is told apart:
// a token can carry the right scope and still lack the permission.
type Code string

const (
	CodeScope        Code = "scope"         // the token lacks a scope the call needs, or a token answer's scopes aren't exactly ours
	CodePermission   Code = "permission"    // IAM (or the OAuth client) refused
	CodeAPIDisabled  Code = "api-disabled"  // the API isn't enabled in the push project
	CodeOrgPolicy    Code = "org-policy"    // an organization policy or a Workspace admin refused
	CodeNotFound     Code = "not-found"     // no such resource
	CodeConflict     Code = "conflict"      // it already exists, or a concurrent change (an IAM etag) won
	CodeInvalidGrant Code = "invalid-grant" // the refresh token or code is dead: a consent is needed
	CodeQuota        Code = "quota"         // rate or quota limit
	CodeUnavailable  Code = "unavailable"   // Google's side failed (5xx)
	CodeNetwork      Code = "network"       // no answer: dial, TLS, timeout, a cut connection
	CodeUnknown      Code = "unknown"       // anything else, including an answer pneu couldn't read or wouldn't accept
)

// Codes are every Code, for tests and checks on the other side of a wire.
var Codes = []Code{CodeScope, CodePermission, CodeAPIDisabled, CodeOrgPolicy, CodeNotFound,
	CodeConflict, CodeInvalidGrant, CodeQuota, CodeUnavailable, CodeNetwork, CodeUnknown}

// Op names the call that failed, a constant per Google method.
type Op string

const (
	OpExchange  Op = "oauth.exchange"
	OpRefresh   Op = "oauth.refresh"
	OpProfile   Op = "gmail.getProfile"
	OpWatch     Op = "gmail.watch"
	OpStop      Op = "gmail.stop"
	OpTopicGet  Op = "pubsub.topics.get"
	OpTopicMake Op = "pubsub.topics.create"
	OpTopicList Op = "pubsub.topics.list"
	OpGetPolicy Op = "pubsub.topics.getIamPolicy"
	OpSetPolicy Op = "pubsub.topics.setIamPolicy"
	OpSubGet    Op = "pubsub.subscriptions.get"
	OpSubMake   Op = "pubsub.subscriptions.create"
	OpPull      Op = "pubsub.subscriptions.pull"
	OpAck       Op = "pubsub.subscriptions.acknowledge"
)

// Ops are every Op.
var Ops = []Op{OpExchange, OpRefresh, OpProfile, OpWatch, OpStop, OpTopicGet, OpTopicMake,
	OpTopicList, OpGetPolicy, OpSetPolicy, OpSubGet, OpSubMake, OpPull, OpAck}

// Error is a failed Google call. It carries only the operation and the
// local code: nothing Google wrote (a message, a status line, a header)
// survives into it, so it can be logged, printed or sent in an event.
type Error struct {
	Op   Op
	Code Code
}

func (e *Error) Error() string { return "google: " + string(e.Op) + ": " + string(e.Code) }

// CodeOf is err's Code: an *Error's, CodeNetwork for a context's end,
// CodeUnknown for anything else (nil: "").
func CodeOf(err error) Code {
	var ge *Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &ge):
		return ge.Code
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return CodeNetwork
	}
	return CodeUnknown
}

func fail(op Op, code Code) error { return &Error{Op: op, Code: code} }

// The fixed strings a failure is matched against. Google's text is read
// only to compare it with these; it never leaves this file.
var (
	oauthCodes = map[string]Code{
		"invalid_grant":         CodeInvalidGrant,
		"invalid_scope":         CodeScope,
		"admin_policy_enforced": CodeOrgPolicy,
		"org_internal":          CodeOrgPolicy,
		"invalid_client":        CodePermission,
		"unauthorized_client":   CodePermission,
		"access_denied":         CodePermission,
	}
	apiDisabledReasons = []string{"SERVICE_DISABLED", "accessNotConfigured", "API_DISABLED"}
	orgPolicyReasons   = []string{"domainPolicy", "ORG_POLICY_CONSTRAINT_FAILED", "ORG_RESTRICTION_VIOLATION", "ORG_RESTRICTION_HEADER_INVALID"}
	scopeReasons       = []string{"ACCESS_TOKEN_SCOPE_INSUFFICIENT", "insufficientPermissions"}
	quotaReasons       = []string{"RATE_LIMIT_EXCEEDED", "RESOURCE_EXHAUSTED", "rateLimitExceeded",
		"userRateLimitExceeded", "dailyLimitExceeded", "quotaExceeded"}
)

// orgConstraint is the violation type domain-restricted sharing reports
// (constraints/iam.allowedPolicyMemberDomains, and its managed successor).
const orgConstraint = "constraints/"

// classify maps a non-2xx answer to a Code: the OAuth endpoint's `error`
// codes, then the API's error.details[].reason and error.errors[].reason
// (Gmail's older shape), then error.status, then the HTTP status.
func classify(status int, body []byte, oauth bool) Code {
	if oauth {
		var e struct {
			Error any `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil {
			if s, ok := e.Error.(string); ok {
				if c, ok := oauthCodes[s]; ok {
					return c
				}
			}
		}
	}
	var e struct {
		Error struct {
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason     string `json:"reason"`
				Violations []struct {
					Type string `json:"type"`
				} `json:"violations"`
			} `json:"details"`
		} `json:"error"`
	}
	var reasons []string
	org := false
	if json.Unmarshal(body, &e) != nil {
		e.Error.Status = "" // a partial decode says nothing: the HTTP status decides
	} else {
		for _, r := range e.Error.Errors {
			reasons = append(reasons, r.Reason)
		}
		for _, d := range e.Error.Details {
			reasons = append(reasons, d.Reason)
			for _, v := range d.Violations {
				if strings.HasPrefix(v.Type, orgConstraint) {
					org = true
				}
			}
		}
	}
	has := func(set []string) bool {
		return slices.ContainsFunc(reasons, func(r string) bool { return slices.Contains(set, r) })
	}
	switch {
	case has(apiDisabledReasons):
		return CodeAPIDisabled
	case org || has(orgPolicyReasons):
		return CodeOrgPolicy
	case has(scopeReasons):
		return CodeScope
	case status == http.StatusTooManyRequests || has(quotaReasons) || e.Error.Status == "RESOURCE_EXHAUSTED":
		return CodeQuota
	case status == http.StatusForbidden:
		return CodePermission
	case status == http.StatusNotFound:
		return CodeNotFound
	case status == http.StatusConflict || status == http.StatusPreconditionFailed ||
		e.Error.Status == "ALREADY_EXISTS" || e.Error.Status == "ABORTED":
		return CodeConflict
	case status >= 500:
		return CodeUnavailable
	}
	return CodeUnknown
}
