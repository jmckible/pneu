package google

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jmckible/pneu/internal/strictjson"
)

// Profile is the mailbox's address (users.getProfile), which must be the
// configured one before anything is provisioned for it (C8).
func (a *API) Profile(ctx context.Context, access string) (string, error) {
	if !printable(access, MaxAccess) {
		return "", fail(OpProfile, CodeUnknown)
	}
	b, err := a.call(ctx, request{op: OpProfile, host: GmailHost, method: http.MethodGet,
		path: "/gmail/v1/users/me/profile", bearer: access})
	if err != nil {
		return "", err
	}
	var addr string
	err = strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		if key != "emailAddress" {
			return strictjson.Skip(dec)
		}
		var err error
		addr, err = strictjson.String(dec, 254)
		return err
	})
	if err != nil || !ValidAddress(addr) {
		return "", fail(OpProfile, CodeUnknown)
	}
	return addr, nil
}

// maxExpiryDigits bounds a watch's expiration: epoch milliseconds before
// the year 2286.
const maxExpiryDigits = 13

// Watch (users.watch) asks Gmail to publish the mailbox's INBOX changes to
// topic in project, and returns when the watch lapses. The topic must be
// in the calling client's project (the push project) and grant
// PublisherMember the publisher role.
func (a *API) Watch(ctx context.Context, access, project, topic string) (time.Time, error) {
	if !printable(access, MaxAccess) || !ValidProject(project) || !resourceRE.MatchString(topic) {
		return time.Time{}, fail(OpWatch, CodeUnknown)
	}
	body, _ := json.Marshal(struct {
		TopicName           string   `json:"topicName"`
		LabelIDs            []string `json:"labelIds"`
		LabelFilterBehavior string   `json:"labelFilterBehavior"`
	}{"projects/" + project + "/topics/" + topic, []string{"INBOX"}, "include"})
	b, err := a.call(ctx, request{op: OpWatch, host: GmailHost, method: http.MethodPost,
		path: "/gmail/v1/users/me/watch", bearer: access, json: body})
	if err != nil {
		return time.Time{}, err
	}
	var exp string
	err = strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		if key != "expiration" {
			return strictjson.Skip(dec) // historyId: never decoded
		}
		var err error
		exp, err = strictjson.String(dec, 32)
		return err
	})
	if err != nil {
		return time.Time{}, fail(OpWatch, CodeUnknown)
	}
	ms, ok := parseMillis(exp)
	if !ok {
		return time.Time{}, fail(OpWatch, CodeUnknown)
	}
	return time.UnixMilli(ms), nil
}

// parseMillis reads a positive decimal of at most maxExpiryDigits digits,
// no sign or leading zero.
func parseMillis(s string) (int64, bool) {
	if s == "" || len(s) > maxExpiryDigits || s[0] == '0' {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// Stop (users.stop) ends every watch on the mailbox: Gmail's watch has no
// identity of its own, so this is per mailbox, not per installation (K9).
func (a *API) Stop(ctx context.Context, access string) error {
	if !printable(access, MaxAccess) {
		return fail(OpStop, CodeUnknown)
	}
	_, err := a.call(ctx, request{op: OpStop, host: GmailHost, method: http.MethodPost,
		path: "/gmail/v1/users/me/stop", bearer: access, json: []byte("{}")})
	return err
}
