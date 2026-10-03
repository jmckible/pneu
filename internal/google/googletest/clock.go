package googletest

import (
	"sync"
	"time"
)

// Clock is a fake clock: time moves only when a test says. The Fake's
// token expiry, ID tokens and watch expirations read it, and so does any
// google.API the Fake makes (Options.Now). After lets code under test wait
// on it.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	c  chan time.Time
}

// NewClock starts a clock at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now is the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d and fires every After due by then.
func (c *Clock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// Set moves the clock to t (backwards too, as a wall clock may) and fires
// every After due by then.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	var keep []waiter
	var due []waiter
	for _, w := range c.waiters {
		if !w.at.After(t) {
			due = append(due, w)
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
	for _, w := range due {
		w.c <- t
	}
}

// After is time.After on this clock: the channel gets the time once the
// clock reaches now+d (at once for d <= 0).
func (c *Clock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), c: ch})
	return ch
}

// Waiters counts Afters not yet fired: a test advances once code under
// test is waiting.
func (c *Clock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}
