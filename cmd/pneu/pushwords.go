package main

// What a failed Google call means, in pneu's own words (docs/push.md D6).
// A *google.Error carries only the operation and a closed code; this turns
// the pair into a sentence and its fix. Nothing here reads anything Google
// wrote, so every word can go to a terminal or a client's event stream.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jmckible/pneu/internal/control"
	"github.com/jmckible/pneu/internal/google"
)

// pushCtx is what the words may name: all of it pneu's own (the project
// and account as configured, the owner's address as pinned at init).
type pushCtx struct {
	project string
	name    string // the account; "" for push init
	address string // the account's configured address
	owner   string // the owner's address; "" before init
}

// What each operation was doing, for the start of a sentence.
var opWords = map[google.Op]string{
	google.OpExchange:  "trading Google's consent for a token",
	google.OpRefresh:   "refreshing the stored grant",
	google.OpProfile:   "asking Gmail whose mailbox the grant is",
	google.OpWatch:     "starting Gmail's watch",
	google.OpStop:      "stopping Gmail's watch",
	google.OpTopicList: "checking the owner reaches the project's Pub/Sub",
	google.OpTopicGet:  "creating the topic",
	google.OpTopicMake: "creating the topic",
	google.OpGetPolicy: "letting Gmail publish to the topic",
	google.OpSetPolicy: "letting Gmail publish to the topic",
	google.OpSubGet:    "creating the subscription",
	google.OpSubMake:   "creating the subscription",
	google.OpPull:      "pulling from the subscription",
	google.OpAck:       "acknowledging messages",
}

func gmailOp(op google.Op) bool {
	return op == google.OpProfile || op == google.OpWatch || op == google.OpStop
}

// explain is err, a failed call made with kind's token, as pneu's words.
func (p pushCtx) explain(err error, kind google.Kind) error {
	var me *google.MismatchError
	var ge *google.Error
	switch {
	case errors.As(err, &me):
		res, _ := google.Resource(p.name)
		if me.OtherInstall {
			return fmt.Errorf("subscription %s in project %s belongs to another pneu server (its %s label): one server per push project", res, p.project, google.InstallLabel)
		}
		return fmt.Errorf("subscription %s in project %s exists, but its %s isn't what pneu makes: delete it (gcloud pubsub subscriptions delete %s --project=%s) and run this again", res, p.project, me.Field, res, p.project)
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return errors.New("stopped before Google answered; nothing past the last step shown was done")
	case !errors.As(err, &ge):
		return errors.New("an unexpected failure talking to Google")
	}
	what, ok := opWords[ge.Op]
	if !ok {
		what = "talking to Google"
	}
	return fmt.Errorf("%s failed: %s", what, p.why(ge.Op, ge.Code, kind))
}

// why is the code's meaning for op, with the fix.
func (p pushCtx) why(op google.Op, code google.Code, kind google.Kind) string {
	switch code {
	case google.CodeAPIDisabled:
		api, host := "Cloud Pub/Sub API", "pubsub.googleapis.com"
		if gmailOp(op) {
			api, host = "Gmail API", "gmail.googleapis.com"
		}
		return fmt.Sprintf("the %s isn't enabled in project %s. Enable it at https://console.cloud.google.com/apis/library/%s?project=%s, then run this again", api, p.project, host, p.project)
	case google.CodeOrgPolicy:
		switch {
		case op == google.OpSetPolicy || op == google.OpGetPolicy:
			return fmt.Sprintf("an organization policy (domain-restricted sharing) refused the grant to %s. Override iam.allowedPolicyMemberDomains to Allow All on project %s only (https://console.cloud.google.com/iam-admin/orgpolicies/iam-allowedPolicyMemberDomains?project=%s), then run this again", google.PublisherMember[len("serviceAccount:"):], p.project, p.project)
		case kind == google.Mailbox:
			return fmt.Sprintf("%s's Workspace admin doesn't allow this app: in the Admin console, Security → Access and data control → API controls, trust the push project's OAuth client for Gmail metadata, then run this again", p.address)
		}
		return "an organization policy or a Workspace admin refused it"
	case google.CodePermission:
		switch {
		case op == google.OpExchange || op == google.OpRefresh:
			return "Google refused the push OAuth client (deleted, or its secret changed): download its client JSON again and run pneu push init --client-secret <file>"
		case gmailOp(op):
			return "Gmail refused the mailbox's grant"
		}
		return fmt.Sprintf("the push owner%s lacks permission in project %s: the owner must own the project (or hold Pub/Sub Admin there)", p.ownerWords(), p.project)
	case google.CodeScope:
		if kind == google.Owner {
			return "the owner's grant lacks a scope it needs: run pneu push init --reconsent and allow everything asked"
		}
		return fmt.Sprintf("the mailbox's grant lacks a scope it needs: run pneu account push %s --reconsent and allow everything asked", p.name)
	case google.CodeNotFound:
		if op == google.OpTopicList {
			return fmt.Sprintf("project %s doesn't exist, or the push owner%s can't see it", p.project, p.ownerWords())
		}
		return "Google found no such resource (deleted meanwhile?): run this again"
	case google.CodeConflict:
		return "a concurrent change kept winning: run this again"
	case google.CodeInvalidGrant:
		switch {
		case op == google.OpExchange:
			return "Google wouldn't trade the consent's code (expired, or used already): run this again"
		case kind == google.Owner:
			return fmt.Sprintf("the push owner's grant%s no longer works (revoked, or expired): run pneu push init --reconsent", p.ownerWords())
		}
		return fmt.Sprintf("%s's mailbox grant no longer works (revoked, or expired): run pneu account push %s --reconsent", p.address, p.name)
	case google.CodeUnauthenticated:
		return "Google refused the access token it had just issued: run this again"
	case google.CodeQuota:
		return "Google's rate limit: try again in a minute"
	case google.CodeUnavailable:
		return "Google's side failed: try again shortly"
	case google.CodeNetwork:
		return "couldn't reach Google (no network, or a timeout)"
	}
	return "Google answered in a way pneu doesn't accept"
}

func (p pushCtx) ownerWords() string {
	if p.owner == "" {
		return ""
	}
	return " (" + p.owner + ")"
}

// reasonWords is a push-state reason (D5's closed set) in words, with its
// fix where there's one to run.
func reasonWords(reason, name string) string {
	switch reason {
	case control.ReasonOwnerReauth:
		return "the push owner's grant no longer works: run pneu push init --reconsent"
	case control.ReasonMailboxReauth:
		return fmt.Sprintf("the mailbox's grant no longer works: run pneu account push %s --reconsent", name)
	case control.ReasonAPIDisabled:
		return "an API isn't enabled in the push project"
	case control.ReasonPermission:
		return "Google refused permission"
	case control.ReasonOrgPolicy:
		return "an organization policy refused it"
	case control.ReasonNetwork:
		return "Google can't be reached"
	case control.ReasonWatchExpired:
		return "Gmail's watch lapsed"
	}
	return "an unknown failure"
}
