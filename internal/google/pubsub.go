package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/jmckible/pneu/internal/strictjson"
)

func topicPath(project, topic string) string { return "/v1/projects/" + project + "/topics/" + topic }
func subPath(project, sub string) string     { return "/v1/projects/" + project + "/subscriptions/" + sub }

// checkNames guards every Pub/Sub call: the paths are built from these
// alone.
func checkNames(access, project, resource string) bool {
	return printable(access, MaxAccess) && ValidProject(project) && (resource == "" || resourceRE.MatchString(resource))
}

// ProbeTopics lists one topic in project (topics.list): a permission
// probe that the owner's token reaches the project's Pub/Sub, not an
// identity check.
func (a *API) ProbeTopics(ctx context.Context, access, project string) error {
	if !checkNames(access, project, "") {
		return fail(OpTopicList, CodeUnknown)
	}
	_, err := a.call(ctx, request{op: OpTopicList, host: PubSubHost, method: http.MethodGet,
		path: "/v1/projects/" + project + "/topics", query: url.Values{"pageSize": {"1"}}, bearer: access})
	return err
}

// GetTopic checks that topic exists in project.
func (a *API) GetTopic(ctx context.Context, access, project, topic string) error {
	if !checkNames(access, project, topic) {
		return fail(OpTopicGet, CodeUnknown)
	}
	b, err := a.call(ctx, request{op: OpTopicGet, host: PubSubHost, method: http.MethodGet,
		path: topicPath(project, topic), bearer: access})
	if err != nil {
		return err
	}
	if !namesItself(b, "projects/"+project+"/topics/"+topic) {
		return fail(OpTopicGet, CodeUnknown)
	}
	return nil
}

// EnsureTopic creates topic in project, or finds it there already;
// created says which. Idempotent (C5).
func (a *API) EnsureTopic(ctx context.Context, access, project, topic string) (created bool, err error) {
	if !checkNames(access, project, topic) {
		return false, fail(OpTopicMake, CodeUnknown)
	}
	b, err := a.call(ctx, request{op: OpTopicMake, host: PubSubHost, method: http.MethodPut,
		path: topicPath(project, topic), bearer: access, json: []byte("{}")})
	if CodeOf(err) == CodeConflict {
		return false, a.GetTopic(ctx, access, project, topic)
	}
	if err != nil {
		return false, err
	}
	if !namesItself(b, "projects/"+project+"/topics/"+topic) {
		return false, fail(OpTopicMake, CodeUnknown)
	}
	return true, nil
}

// namesItself reports whether a resource's answer has name want.
func namesItself(b []byte, want string) bool {
	var name string
	err := strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		if key != "name" {
			return strictjson.Skip(dec)
		}
		var err error
		name, err = strictjson.String(dec, 512)
		return err
	})
	return err == nil && name == want
}

// The subscription D4 provisions.
const (
	AckDeadlineSeconds = 30
	MessageRetention   = "3600s"
)

// MismatchError: an existing subscription differs from D4's in Field, one
// of the Field* constants (never Google's words). OtherInstall: it's
// labelled as another server's (K9).
type MismatchError struct {
	Field        string
	OtherInstall bool
}

func (e *MismatchError) Error() string {
	if e.OtherInstall {
		return "google: the subscription belongs to another pneu server (" + InstallLabel + " label)"
	}
	return "google: the subscription differs from pneu's in " + e.Field
}

// The fields a subscription is compared on.
const (
	FieldName       = "name"
	FieldTopic      = "topic"
	FieldPush       = "pushConfig"
	FieldBigQuery   = "bigqueryConfig"
	FieldStorage    = "cloudStorageConfig"
	FieldBigtable   = "bigtableConfig"
	FieldAck        = "ackDeadlineSeconds"
	FieldRetention  = "messageRetentionDuration"
	FieldLabel      = "labels." + InstallLabel
	FieldExpiration = "expirationPolicy"
	FieldFilter     = "filter"
	FieldDeadLetter = "deadLetterPolicy"
	FieldTransforms = "messageTransforms"
	FieldDetached   = "detached"
)

// EnsureSubscription creates the pull subscription sub on topic in
// project, labelled with install (D4): ack deadline 30s, retention 1h,
// never expiring, no filter, dead-letter, transform or detachment. One
// that exists already must match all of that, label included, or it's a
// *MismatchError naming the field. created says which.
func (a *API) EnsureSubscription(ctx context.Context, access, project, sub, topic, install string) (created bool, err error) {
	if !checkNames(access, project, sub) || !resourceRE.MatchString(topic) || !ValidInstall(install) {
		return false, fail(OpSubMake, CodeUnknown)
	}
	body, _ := json.Marshal(struct {
		Topic              string            `json:"topic"`
		Labels             map[string]string `json:"labels"`
		AckDeadlineSeconds int               `json:"ackDeadlineSeconds"`
		Retention          string            `json:"messageRetentionDuration"`
		Expiration         struct{}          `json:"expirationPolicy"` // {}: never (omitted means 31 days)
	}{
		Topic:              "projects/" + project + "/topics/" + topic,
		Labels:             map[string]string{InstallLabel: install},
		AckDeadlineSeconds: AckDeadlineSeconds,
		Retention:          MessageRetention,
	})
	b, err := a.call(ctx, request{op: OpSubMake, host: PubSubHost, method: http.MethodPut,
		path: subPath(project, sub), bearer: access, json: body})
	if CodeOf(err) == CodeConflict {
		return false, a.CheckSubscription(ctx, access, project, sub, topic, install)
	}
	if err != nil {
		return false, err
	}
	// What Google made, held to what was asked: a field it dropped shows.
	if err := compareSubscription(b, project, sub, topic, install); err != nil {
		return true, wrapShape(OpSubMake, err)
	}
	return true, nil
}

// CheckSubscription reads sub and compares it with D4's (see
// EnsureSubscription).
func (a *API) CheckSubscription(ctx context.Context, access, project, sub, topic, install string) error {
	if !checkNames(access, project, sub) || !resourceRE.MatchString(topic) || !ValidInstall(install) {
		return fail(OpSubGet, CodeUnknown)
	}
	b, err := a.call(ctx, request{op: OpSubGet, host: PubSubHost, method: http.MethodGet,
		path: subPath(project, sub), bearer: access})
	if err != nil {
		return err
	}
	return wrapShape(OpSubGet, compareSubscription(b, project, sub, topic, install))
}

// wrapShape passes a *MismatchError through and turns any other error
// (an unreadable answer) into fail(op, CodeUnknown).
func wrapShape(op Op, err error) error {
	var me *MismatchError
	if err == nil || errors.As(err, &me) {
		return err
	}
	return fail(op, CodeUnknown)
}

// compareSubscription holds a subscription resource to D4's shape.
func compareSubscription(b []byte, project, sub, topic, install string) error {
	var (
		name, gotTopic, retention, filter string
		ack                               uint64
		label                             string
		hasLabel, hasExpiration, detached bool
		mismatch                          string
	)
	miss := func(f string) {
		if mismatch == "" {
			mismatch = f
		}
	}
	// emptyObject: absent, or an object with nothing set in it.
	emptyObject := func(dec *json.Decoder, field string) error {
		return strictjson.Object(dec, func(string) error {
			miss(field)
			return strictjson.Skip(dec)
		})
	}
	err := strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		var err error
		switch key {
		case "name":
			name, err = strictjson.String(dec, 512)
		case "topic":
			gotTopic, err = strictjson.String(dec, 512)
		case "pushConfig":
			err = emptyObject(dec, FieldPush)
		case "bigqueryConfig":
			err = emptyObject(dec, FieldBigQuery)
		case "cloudStorageConfig":
			err = emptyObject(dec, FieldStorage)
		case "bigtableConfig":
			err = emptyObject(dec, FieldBigtable)
		case "deadLetterPolicy":
			err = emptyObject(dec, FieldDeadLetter)
		case "ackDeadlineSeconds":
			ack, err = strictjson.Uint(dec, 1<<20)
		case "messageRetentionDuration":
			retention, err = strictjson.String(dec, 64)
		case "filter":
			filter, err = strictjson.String(dec, 1024)
		case "detached":
			detached, err = strictjson.Bool(dec)
		case "messageTransforms":
			err = strictjson.Array(dec, -1, func(int) error {
				miss(FieldTransforms)
				return strictjson.Skip(dec)
			})
		case "labels":
			err = strictjson.Object(dec, func(k string) error {
				if k != InstallLabel {
					return strictjson.Skip(dec)
				}
				hasLabel = true
				var err error
				label, err = strictjson.String(dec, 63)
				return err
			})
		case "expirationPolicy":
			hasExpiration = true
			err = strictjson.Object(dec, func(k string) error {
				if k != "ttl" {
					miss(FieldExpiration)
					return strictjson.Skip(dec)
				}
				ttl, err := strictjson.String(dec, 64)
				if ttl != "" {
					miss(FieldExpiration)
				}
				return err
			})
		default:
			return strictjson.Skip(dec)
		}
		return err
	})
	if err != nil {
		return errShape
	}
	switch {
	case hasLabel && label != install:
		return &MismatchError{Field: FieldLabel, OtherInstall: true}
	case name != "projects/"+project+"/subscriptions/"+sub:
		return &MismatchError{Field: FieldName}
	case gotTopic != "projects/"+project+"/topics/"+topic:
		return &MismatchError{Field: FieldTopic}
	case !hasLabel:
		return &MismatchError{Field: FieldLabel}
	case mismatch != "":
		return &MismatchError{Field: mismatch}
	case ack != AckDeadlineSeconds:
		return &MismatchError{Field: FieldAck}
	case retention != MessageRetention:
		return &MismatchError{Field: FieldRetention}
	case !hasExpiration:
		return &MismatchError{Field: FieldExpiration}
	case filter != "":
		return &MismatchError{Field: FieldFilter}
	case detached:
		return &MismatchError{Field: FieldDetached}
	}
	return nil
}

// Pull bounds (D5).
const (
	MaxMessages = 10
	MaxAckID    = 512
)

// Pull asks sub for up to MaxMessages messages (subscriptions.pull) and
// returns their ack IDs only: a message is a nudge, and its body is never
// decoded. Empty means Google answered with none. Bounded by the API's
// PullTimeout as well as ctx.
func (a *API) Pull(ctx context.Context, access, project, sub string) ([]string, error) {
	if !checkNames(access, project, sub) {
		return nil, fail(OpPull, CodeUnknown)
	}
	b, err := a.call(ctx, request{op: OpPull, host: PubSubHost, method: http.MethodPost,
		path: subPath(project, sub) + ":pull", bearer: access,
		json: []byte(`{"maxMessages":10}`), timeout: a.opts.PullTimeout})
	if err != nil {
		return nil, err
	}
	var ids []string
	err = strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		if key != "receivedMessages" {
			return strictjson.Skip(dec)
		}
		return strictjson.Array(dec, MaxMessages, func(int) error {
			id := ""
			err := strictjson.Object(dec, func(k string) error {
				if k != "ackId" {
					return strictjson.Skip(dec) // message, deliveryAttempt: never decoded
				}
				var err error
				id, err = strictjson.String(dec, MaxAckID)
				return err
			})
			if err != nil {
				return err
			}
			if !printable(id, MaxAckID) {
				return errShape
			}
			ids = append(ids, id)
			return nil
		})
	})
	if err != nil {
		return nil, fail(OpPull, CodeUnknown)
	}
	return ids, nil
}

// Ack acknowledges ackIDs on sub (subscriptions.acknowledge): 1 to
// MaxMessages IDs, each one Pull returned.
func (a *API) Ack(ctx context.Context, access, project, sub string, ackIDs []string) error {
	if !checkNames(access, project, sub) || len(ackIDs) == 0 || len(ackIDs) > MaxMessages {
		return fail(OpAck, CodeUnknown)
	}
	for _, id := range ackIDs {
		if !printable(id, MaxAckID) {
			return fail(OpAck, CodeUnknown)
		}
	}
	body, _ := json.Marshal(struct {
		AckIDs []string `json:"ackIds"`
	}{ackIDs})
	_, err := a.call(ctx, request{op: OpAck, host: PubSubHost, method: http.MethodPost,
		path: subPath(project, sub) + ":acknowledge", bearer: access, json: body})
	return err
}
