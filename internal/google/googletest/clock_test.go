package googletest

import (
	"testing"
	"time"
)

func TestClock(t *testing.T) {
	c := NewClock(Epoch)
	a := c.After(time.Minute)
	b := c.After(time.Hour)
	if c.Waiters() != 2 {
		t.Fatal(c.Waiters())
	}
	c.Advance(59 * time.Second)
	select {
	case <-a:
		t.Fatal("fired early")
	default:
	}
	c.Advance(time.Second)
	if got := <-a; !got.Equal(Epoch.Add(time.Minute)) {
		t.Fatal(got)
	}
	if c.Waiters() != 1 {
		t.Fatal(c.Waiters())
	}
	c.Set(Epoch) // backwards: nothing fires
	select {
	case <-b:
		t.Fatal("fired going backwards")
	default:
	}
	c.Set(Epoch.Add(2 * time.Hour))
	<-b
	select {
	case <-c.After(0):
	default:
		t.Fatal("After(0) didn't fire at once")
	}
}
