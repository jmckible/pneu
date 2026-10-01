// Package wake notices that the machine slept. Go's timers and the
// monotonic reading in time.Time run on CLOCK_MONOTONIC, which stops in
// suspend: a 60s lease armed before five hours asleep still has its 60s
// after them, a 5-minute heartbeat is still minutes away, and a
// connection the other end dropped meanwhile looks alive. The wall clock
// keeps going, so how much further it moved than the monotonic clock
// over an interval is the time spent asleep.
package wake

import (
	"sync"
	"time"
)

const (
	// Gap is how much further the wall clock must move than the
	// monotonic one for a check to call it sleep: far above scheduling
	// jitter, far below any lease. A wall clock stepped forward (NTP)
	// reads as sleep too, which costs a reconnect and a status write.
	Gap = 5 * time.Second
	// Every is how often Watch looks.
	Every = 2 * time.Second
)

// Clock remembers the last check's two readings.
type Clock struct {
	mu sync.Mutex
	// wall and mono are the seams for tests: the wall clock without its
	// monotonic reading, and the monotonic clock.
	wall     func() time.Time
	mono     func() time.Duration
	lastWall time.Time
	lastMono time.Duration
}

// New is a clock whose first check counts from now.
func New() *Clock {
	start := time.Now()
	return NewWith(func() time.Time { return time.Now().Round(0) }, func() time.Duration { return time.Since(start) })
}

// NewWith is a clock on the given readings (tests).
func NewWith(wall func() time.Time, mono func() time.Duration) *Clock {
	return &Clock{wall: wall, mono: mono, lastWall: wall(), lastMono: mono()}
}

// Slept reports the time asleep since the last check, and whether it's
// over Gap. Each gap is reported once, to whichever caller sees it first.
func (c *Clock) Slept() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, m := c.wall(), c.mono()
	asleep := w.Sub(c.lastWall) - (m - c.lastMono)
	c.lastWall, c.lastMono = w, m
	return asleep, asleep > Gap
}

// Watch checks every interval until stop closes, and calls fn with the
// time asleep whenever a check says the machine slept.
func (c *Clock) Watch(stop <-chan struct{}, every time.Duration, fn func(asleep time.Duration)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if asleep, ok := c.Slept(); ok {
			fn(asleep)
		}
	}
}
