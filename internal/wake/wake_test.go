package wake

import (
	"testing"
	"time"
)

type fake struct {
	wall time.Time
	mono time.Duration
}

func (f *fake) clock() *Clock {
	return NewWith(func() time.Time { return f.wall }, func() time.Duration { return f.mono })
}

// awake moves both clocks; asleep only the wall clock.
func (f *fake) awake(d time.Duration)  { f.wall = f.wall.Add(d); f.mono += d }
func (f *fake) asleep(d time.Duration) { f.wall = f.wall.Add(d) }

func TestSlept(t *testing.T) {
	f := &fake{wall: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	c := f.clock()
	f.awake(2 * time.Second)
	if _, ok := c.Slept(); ok {
		t.Fatal("awake read as sleep")
	}
	f.awake(time.Second)
	f.asleep(5 * time.Hour)
	f.awake(time.Second)
	if d, ok := c.Slept(); !ok || d != 5*time.Hour {
		t.Fatalf("five hours asleep: %v %v", d, ok)
	}
	// Reported once.
	f.awake(2 * time.Second)
	if _, ok := c.Slept(); ok {
		t.Fatal("the same sleep reported twice")
	}
	// Under the gap, and a wall clock stepped back: not sleep.
	f.asleep(Gap)
	if _, ok := c.Slept(); ok {
		t.Fatal("a gap of exactly Gap read as sleep")
	}
	f.wall = f.wall.Add(-time.Hour)
	if _, ok := c.Slept(); ok {
		t.Fatal("a wall clock stepped back read as sleep")
	}
}

func TestWatch(t *testing.T) {
	var mono time.Duration
	wall := time.Now()
	calls := 0
	c := NewWith(func() time.Time {
		calls++
		if calls == 3 {
			wall = wall.Add(time.Hour)
		}
		return wall
	}, func() time.Duration { return mono })
	stop := make(chan struct{})
	got := make(chan time.Duration, 1)
	go c.Watch(stop, time.Millisecond, func(d time.Duration) { got <- d })
	defer close(stop)
	select {
	case d := <-got:
		if d != time.Hour {
			t.Fatalf("asleep %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch never reported the sleep")
	}
}
