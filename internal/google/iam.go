package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"

	"github.com/jmckible/pneu/internal/strictjson"
)

// PolicyAttempts bounds GrantPublisher's read-modify-write: the first try
// and up to three more after an etag conflict.
const PolicyAttempts = 4

// GrantPublisher gives PublisherMember PublisherRole on topic,
// unconditionally, keeping every other binding, condition and field of the
// topic's policy exactly as read (D4): getIamPolicy at version 3, add the
// member, setIamPolicy with the read etag at version 3. On an etag
// conflict it reads again and recomputes, up to PolicyAttempts in all.
// changed is false when the binding was already there (nothing is
// written). A policy it can't round-trip intact is refused, never
// rewritten.
func (a *API) GrantPublisher(ctx context.Context, access, project, topic string) (changed bool, err error) {
	if !checkNames(access, project, topic) {
		return false, fail(OpGetPolicy, CodeUnknown)
	}
	for range PolicyAttempts {
		b, err := a.call(ctx, request{op: OpGetPolicy, host: PubSubHost, method: http.MethodGet,
			path: topicPath(project, topic) + ":getIamPolicy", bearer: access,
			query: url.Values{"options.requestedPolicyVersion": {"3"}}})
		if err != nil {
			return false, err
		}
		next, add, err := addPublisher(b)
		if err != nil {
			return false, fail(OpGetPolicy, CodeUnknown)
		}
		if !add {
			return false, nil
		}
		var body bytes.Buffer
		body.WriteString(`{"policy":`)
		body.Write(next)
		body.WriteString(`}`)
		_, err = a.call(ctx, request{op: OpSetPolicy, host: PubSubHost, method: http.MethodPost,
			path: topicPath(project, topic) + ":setIamPolicy", bearer: access, json: body.Bytes()})
		if CodeOf(err) == CodeConflict {
			continue
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fail(OpSetPolicy, CodeConflict)
}

// member is one key and its value exactly as read.
type member struct {
	key string
	raw json.RawMessage
}

// readMembers reads one object as its keys and raw values in order,
// refusing a duplicate key (it couldn't be round-tripped faithfully).
func readMembers(dec *json.Decoder) ([]member, error) {
	var ms []member
	err := strictjson.Object(dec, func(key string) error {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		ms = append(ms, member{key, raw})
		return nil
	})
	return ms, err
}

func writeMembers(buf *bytes.Buffer, ms []member) error {
	buf.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		buf.Write(k)
		buf.WriteByte(':')
		if err := json.Compact(buf, m.raw); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func find(ms []member, key string) (int, bool) {
	i := slices.IndexFunc(ms, func(m member) bool { return m.key == key })
	return i, i >= 0
}

// errPolicy: a policy this code can't round-trip intact.
var errPolicy = errors.New("google: IAM policy can't be preserved")

// addPublisher is the modify step: it returns the policy with
// PublisherMember in an unconditional PublisherRole binding, at version
// 3, and add false when it's there already. Every key and value it doesn't
// change goes back as read. It refuses (errPolicy) a policy with a
// duplicate key at any level it walks, a version other than 1 or 3 (or a
// condition below version 3), no etag, or a binding whose role or members
// it can't read.
func addPublisher(policy []byte) (next []byte, add bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(policy))
	top, err := readMembers(dec)
	if err != nil {
		return nil, false, errPolicy
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false, errPolicy // something after the object
	}
	version := uint64(1)
	if i, ok := find(top, "version"); ok {
		d := json.NewDecoder(bytes.NewReader(top[i].raw))
		v, err := strictjson.Uint(d, 3)
		if err != nil || (v != 0 && v != 1 && v != 3) {
			return nil, false, errPolicy
		}
		version = max(v, 1)
	}
	if i, ok := find(top, "etag"); !ok {
		return nil, false, errPolicy
	} else {
		var etag string
		if json.Unmarshal(top[i].raw, &etag) != nil || !printable(etag, 256) {
			return nil, false, errPolicy
		}
	}
	type binding struct {
		ms          []member
		role        string
		members     []string
		conditional bool
	}
	var bindings []binding
	bi, hasBindings := find(top, "bindings")
	if hasBindings {
		d := json.NewDecoder(bytes.NewReader(top[bi].raw))
		err := strictjson.Array(d, 1500, func(int) error {
			ms, err := readMembers(d)
			if err != nil {
				return err
			}
			b := binding{ms: ms}
			ri, ok := find(ms, "role")
			if !ok || json.Unmarshal(ms[ri].raw, &b.role) != nil || b.role == "" {
				return errPolicy
			}
			if mi, ok := find(ms, "members"); ok {
				if err := json.Unmarshal(ms[mi].raw, &b.members); err != nil {
					return errPolicy
				}
			}
			_, b.conditional = find(ms, "condition")
			bindings = append(bindings, b)
			return nil
		})
		if err != nil {
			return nil, false, errPolicy
		}
	}
	target := -1
	for i, b := range bindings {
		if b.conditional && version != 3 {
			return nil, false, errPolicy
		}
		if b.role != PublisherRole || b.conditional {
			continue
		}
		if slices.Contains(b.members, PublisherMember) {
			return nil, false, nil
		}
		if target < 0 {
			target = i
		}
	}
	// The bindings array, rebuilt: every binding as read but the one that
	// gains the member.
	var arr bytes.Buffer
	arr.WriteByte('[')
	for i, b := range bindings {
		if i > 0 {
			arr.WriteByte(',')
		}
		ms := b.ms
		if i == target {
			members, _ := json.Marshal(append(slices.Clone(b.members), PublisherMember))
			ms = slices.Clone(ms)
			if mi, ok := find(ms, "members"); ok {
				ms[mi].raw = members
			} else {
				ms = append(ms, member{"members", members})
			}
		}
		if err := writeMembers(&arr, ms); err != nil {
			return nil, false, errPolicy
		}
	}
	if target < 0 {
		if len(bindings) > 0 {
			arr.WriteByte(',')
		}
		nb, _ := json.Marshal(struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		}{PublisherRole, []string{PublisherMember}})
		arr.Write(nb)
	}
	arr.WriteByte(']')

	top = slices.Clone(top)
	if hasBindings {
		top[bi].raw = arr.Bytes()
	} else {
		top = append(top, member{"bindings", arr.Bytes()})
	}
	if i, ok := find(top, "version"); ok {
		top[i].raw = json.RawMessage("3")
	} else {
		top = append(top, member{"version", json.RawMessage("3")})
	}
	var out bytes.Buffer
	if err := writeMembers(&out, top); err != nil {
		return nil, false, errPolicy
	}
	return out.Bytes(), true, nil
}
