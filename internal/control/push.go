package control

// Push sync's two commands (docs/push.md D4). The CLI writes state.json
// and hands each generation over with push-reload; it watches a pushed
// account come up with push-state. Both have fixed shapes, their
// arguments validated before any handler runs; the handlers are the
// daemon's push manager's.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmckible/pneu/internal/config"
	"github.com/jmckible/pneu/internal/strictjson"
)

const (
	// PushReload, as "push-reload <generation> <hash>", asks the daemon to
	// apply that generation of push's state.json (ReloadPush).
	PushReload Command = "push-reload"
	// PushStateCmd, as "push-state <account>", asks for the push health of
	// one account (AskPushState).
	PushStateCmd Command = "push-state"
)

// Reload is how the daemon answered a push-reload.
type Reload int

const (
	// ReloadApplied: the daemon read state.json, found that generation
	// with that hash, and applied it: workers that had to go are
	// cancelled and joined, new ones started. Reply "ok <gen>".
	ReloadApplied Reload = iota + 1
	// ReloadStale: the daemon has applied a newer generation. Reply
	// "stale".
	ReloadStale
	// ReloadMismatch: state.json isn't that generation with that hash (a
	// later commit, or damage); the CLI re-reads and retries. Reply
	// "mismatch".
	ReloadMismatch
)

func (r Reload) String() string {
	switch r {
	case ReloadApplied:
		return "applied"
	case ReloadStale:
		return "stale"
	case ReloadMismatch:
		return "mismatch"
	}
	return "unknown"
}

// The push health states (D5), and "off" for an account the daemon runs
// no worker for.
const (
	PushOff        = "off"
	PushStarting   = "starting"
	PushDelivering = "delivering"
	PushQuiet      = "quiet"
	PushFailing    = "failing"
	PushReauth     = "reauth"
)

// PushStates are every state, the only values PushState.State may take.
var PushStates = []string{PushOff, PushStarting, PushDelivering, PushQuiet, PushFailing, PushReauth}

// The closed reasons (D5): the reauth ones only with PushReauth, the
// rest only with PushFailing.
const (
	ReasonOwnerReauth   = "owner-reauth"
	ReasonMailboxReauth = "mailbox-reauth"
	ReasonAPIDisabled   = "api-disabled"
	ReasonPermission    = "permission"
	ReasonOrgPolicy     = "org-policy"
	ReasonNetwork       = "network"
	ReasonWatchExpired  = "watch-expired"
	ReasonUnknown       = "unknown"
)

// ReauthReasons and FailingReasons are the reasons each state takes.
var (
	ReauthReasons  = []string{ReasonOwnerReauth, ReasonMailboxReauth}
	FailingReasons = []string{ReasonAPIDisabled, ReasonPermission, ReasonOrgPolicy, ReasonNetwork, ReasonWatchExpired, ReasonUnknown}
)

// PushState is the push-state reply: which daemon process answered, the
// state.json generation it has applied (0: none yet), and the account's
// health.
type PushState struct {
	Instance     string // the daemon's Instance(): a restart changes it
	Generation   uint64
	State        string    // one of PushStates
	Reason       string    // with PushReauth or PushFailing only
	LastDelivery time.Time // the last message on the account's subscription; zero: none
}

// Validate checks every field against its shape.
func (p PushState) Validate() error {
	switch {
	case !instanceHex.MatchString(p.Instance):
		return errors.New("instance isn't 16 hex digits")
	case p.Generation > maxPushGen:
		return errors.New("generation out of bounds")
	case !slices.Contains(PushStates, p.State):
		return fmt.Errorf("unknown push state %q", p.State)
	case p.State == PushReauth && !slices.Contains(ReauthReasons, p.Reason),
		p.State == PushFailing && !slices.Contains(FailingReasons, p.Reason):
		return fmt.Errorf("state %s with reason %q", p.State, p.Reason)
	case p.State != PushReauth && p.State != PushFailing && p.Reason != "":
		return fmt.Errorf("state %s takes no reason", p.State)
	case !p.LastDelivery.IsZero() && !validDelivery(p.LastDelivery):
		return errors.New("lastDelivery out of bounds")
	}
	return nil
}

// maxPushGen bounds a generation on this wire: state.json's own bound.
const maxPushGen = 1 << 53

const deliveryLayout = time.RFC3339

func validDelivery(t time.Time) bool {
	t = t.UTC()
	return t.Year() >= 2020 && t.Year() <= 9999
}

// pushStateWire is the reply's encoding.
type pushStateWire struct {
	Instance     string `json:"instance"`
	Generation   uint64 `json:"generation"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	LastDelivery string `json:"lastDelivery,omitempty"`
}

func pushStateReply(p PushState) string {
	if p.Instance == "" {
		p.Instance = instance
	}
	if err := p.Validate(); err != nil {
		return "error push-state: " + oneLine(err.Error())
	}
	w := pushStateWire{Instance: p.Instance, Generation: p.Generation, State: p.State, Reason: p.Reason}
	if !p.LastDelivery.IsZero() {
		w.LastDelivery = p.LastDelivery.UTC().Truncate(time.Second).Format(deliveryLayout)
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "error " + oneLine(err.Error())
	}
	return string(b)
}

// ParsePushState reads a push-state reply by token: the known keys once
// each, instance, generation and state required, nothing after, every
// field within its shape.
func ParsePushState(b []byte) (PushState, error) {
	var p PushState
	seen := map[string]bool{}
	err := strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		seen[key] = true
		var err error
		switch key {
		case "instance":
			p.Instance, err = strictjson.String(dec, 16)
		case "generation":
			p.Generation, err = strictjson.Uint(dec, maxPushGen)
		case "state":
			p.State, err = strictjson.String(dec, 16)
		case "reason":
			p.Reason, err = strictjson.String(dec, 32)
			if err == nil && p.Reason == "" {
				err = errors.New("empty")
			}
		case "lastDelivery":
			var s string
			if s, err = strictjson.String(dec, 32); err != nil {
				return err
			}
			t, perr := time.Parse(deliveryLayout, s)
			if perr != nil || t.UTC().Format(deliveryLayout) != s || !validDelivery(t) {
				return errors.New("not a UTC RFC 3339 time in bounds")
			}
			p.LastDelivery = t
		default:
			return strictjson.ErrUnknown
		}
		return err
	})
	if err != nil {
		return PushState{}, err
	}
	if !seen["instance"] || !seen["generation"] || !seen["state"] {
		return PushState{}, errors.New("missing fields")
	}
	if err := p.Validate(); err != nil {
		return PushState{}, err
	}
	return p, nil
}

// The replies' fixed text.
const (
	pushOff   = "push off"
	stale     = "stale"
	mismatch  = "mismatch"
	badPushRL = "error bad push-reload"
	badPushSt = "error bad push-state"
)

// ErrPushOff: the daemon runs no push manager (a client's daemon, or one
// started without push state).
var ErrPushOff = errors.New("control: the running pneu runs no push sync")

// answerPush answers push-reload and push-state; handled false for any
// other command.
func (s *Server) answerPush(cmd Command) (reply string, handled bool) {
	if args, ok := strings.CutPrefix(string(cmd), string(PushReload)+" "); ok {
		gen, hash, ok := parseReload(args)
		switch {
		case !ok || gen > maxPushGen:
			return badPushRL, true
		case s.h.Client || s.h.PushReload == nil:
			return "error " + pushOff, true
		}
		r, err := s.h.PushReload(gen, hash)
		if err != nil {
			return "error " + oneLine(err.Error()), true
		}
		switch r {
		case ReloadApplied:
			return "ok " + strconv.FormatUint(gen, 10), true
		case ReloadStale:
			return stale, true
		case ReloadMismatch:
			return mismatch, true
		}
		return "error push-reload: no outcome", true
	}
	if account, ok := strings.CutPrefix(string(cmd), string(PushStateCmd)+" "); ok {
		switch {
		case !config.ValidName(account):
			return badPushSt, true
		case s.h.Client || s.h.PushState == nil:
			return "error " + pushOff, true
		}
		return pushStateReply(s.h.PushState(account)), true
	}
	return "", false
}

// ReloadPush asks the daemon at path to apply generation gen (hash hash)
// of push's state.json and waits up to ReloadTimeout for its answer:
// ReloadApplied only on "ok <gen>" naming this generation. ErrNotRunning
// when nothing answers on the socket, which is not proof there's no
// daemon: the CLI decides that only by taking daemon.lock. ErrPushOff when
// the daemon runs no push. Any other error leaves the outcome unknown:
// pending, never done.
func ReloadPush(path string, gen uint64, hash string) (Reload, error) {
	return reloadPush(path, gen, hash, uidSelf())
}

func reloadPush(path string, gen uint64, hash string, uid int) (Reload, error) {
	if gen == 0 || gen > maxPushGen || !ValidHash(hash) {
		return 0, errors.New("control: bad generation or hash")
	}
	reply, err := exchange(path, fmt.Sprintf("%s %d %s", PushReload, gen, hash), ReloadTimeout, uid)
	if err != nil {
		return 0, err
	}
	switch reply {
	case "ok " + strconv.FormatUint(gen, 10):
		return ReloadApplied, nil
	case stale:
		return ReloadStale, nil
	case mismatch:
		return ReloadMismatch, nil
	case "error " + pushOff:
		return 0, ErrPushOff
	}
	if msg, ok := strings.CutPrefix(reply, "error "); ok {
		return 0, fmt.Errorf("control: push-reload: %s", msg)
	}
	return 0, fmt.Errorf("control: push-reload: unrecognized acknowledgment %q", oneLine(reply))
}

// AskPushState asks the daemon at path for account's push health, parsed
// strictly (ParsePushState). ErrNotRunning when nothing answers;
// ErrPushOff when the daemon runs no push.
func AskPushState(path, account string) (PushState, error) {
	return askPushState(path, account, uidSelf())
}

func askPushState(path, account string, uid int) (PushState, error) {
	if !config.ValidName(account) {
		return PushState{}, errors.New("control: bad account name")
	}
	reply, err := exchange(path, string(PushStateCmd)+" "+account, Timeout, uid)
	if err != nil {
		return PushState{}, err
	}
	if reply == "error "+pushOff {
		return PushState{}, ErrPushOff
	}
	if msg, ok := strings.CutPrefix(reply, "error "); ok {
		return PushState{}, fmt.Errorf("control: push-state: %s", msg)
	}
	p, err := ParsePushState([]byte(reply))
	if err != nil {
		return PushState{}, fmt.Errorf("control: push-state: %w", err)
	}
	return p, nil
}

// exchange sends one line and returns the reply line as sent (an "error
// …" reply included), within timeout, refusing a socket another uid
// answers. A missing or refusing socket is ErrNotRunning.
func exchange(path, line string, timeout time.Duration, uid int) (string, error) {
	reply, err := sendLine(path, line, timeout, uid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return "", fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	return reply, err
}
