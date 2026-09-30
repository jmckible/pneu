package unsub

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	TokenTTL  = 2 * time.Minute  // a preview's token
	ResultTTL = 10 * time.Minute // an executed token's result
	maxTokens = 1024
)

// ErrFull means too many live tokens; the preview is refused.
var ErrFull = errors.New("unsub: too many pending unsubscribes")

// Store binds previews to executions: a token names one stored action,
// is consumed at most once (atomically), and then names its result.
type Store[A, R any] struct {
	Now func() time.Time // time.Now unless a test replaces it

	mu      sync.Mutex
	pending map[string]pendingEntry[A]
	results map[string]resultEntry[R]
}

type pendingEntry[A any] struct {
	action  A
	expires time.Time
}

type resultEntry[R any] struct {
	result  R
	done    bool // false while the action runs
	expires time.Time
}

func (s *Store[A, R]) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// prune drops expired entries; s.mu is held.
func (s *Store[A, R]) prune(now time.Time) {
	for k, e := range s.pending {
		if !now.Before(e.expires) {
			delete(s.pending, k)
		}
	}
	for k, e := range s.results {
		if e.done && !now.Before(e.expires) {
			delete(s.results, k)
		}
	}
}

// Issue stores action under a fresh 128-bit token valid for TokenTTL.
func (s *Store[A, R]) Issue(action A) (string, error) {
	raw := make([]byte, 16)
	rand.Read(raw) // never fails
	tok := hex.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	if len(s.pending)+len(s.results) >= maxTokens {
		return "", ErrFull
	}
	if s.pending == nil {
		s.pending = map[string]pendingEntry[A]{}
		s.results = map[string]resultEntry[R]{}
	}
	s.pending[tok] = pendingEntry[A]{action, now.Add(TokenTTL)}
	return tok, nil
}

// Take consumes the token: the first caller within TokenTTL gets its action
// and must call Finish; every other call gets ok=false.
func (s *Store[A, R]) Take(tok string) (action A, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, found := s.pending[tok]
	if !found {
		return action, false
	}
	delete(s.pending, tok)
	now := s.now()
	if !now.Before(e.expires) {
		return action, false
	}
	s.results[tok] = resultEntry[R]{}
	return e.action, true
}

// Finish records the taken token's result for ResultTTL.
func (s *Store[A, R]) Finish(tok string, result R) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[tok] = resultEntry[R]{result: result, done: true, expires: s.now().Add(ResultTTL)}
}

// Result is the token's outcome: found=false for a token never taken (or
// long gone); done=false while its action still runs.
func (s *Store[A, R]) Result(tok string) (result R, done, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.now())
	e, ok := s.results[tok]
	if !ok {
		return result, false, false
	}
	return e.result, e.done, true
}
