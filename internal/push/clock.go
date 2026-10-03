package push

import "time"

// Clock is the manager's time. Every fact and deadline is a wall-clock
// reading: Go's timers and monotonic readings stop in suspend, and a
// token's expiry, a watch's expiration and "no pull for 3 minutes" are
// about the world's time, not the process's. Waits are capped wherever a
// wall-clock jump could stretch them, and a wake re-reads them all.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// wallClock is the real clock, without the monotonic reading.
type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now().Round(0) }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// WallNow is the wall clock without its monotonic reading: what main
// gives google.Options.Now, so token expiry is by the same clock.
func WallNow() time.Time { return wallClock{}.Now() }

// backoff doubles from lo to hi on each failure. With clean set, it starts
// over at lo once that long has passed without a failure (since the first
// success after one); without, only reset starts it over.
type backoff struct {
	lo, hi, clean time.Duration
	cur           time.Duration
	okSince       time.Time
}

// fail records a failure at now and returns how long to wait.
func (b *backoff) fail(now time.Time) time.Duration {
	switch {
	case b.cur == 0:
		b.cur = b.lo
	case b.clean > 0 && !b.okSince.IsZero() && now.Sub(b.okSince) >= b.clean:
		b.cur = b.lo
	default:
		b.cur = min(2*b.cur, b.hi)
	}
	b.okSince = time.Time{}
	return b.cur
}

// ok records a success at now.
func (b *backoff) ok(now time.Time) {
	if b.okSince.IsZero() {
		b.okSince = now
	}
}

func (b *backoff) reset() { b.cur, b.okSince = 0, time.Time{} }
