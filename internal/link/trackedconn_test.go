package link

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// blockingConn's first Close blocks until release, the descriptor still
// being torn down; later Closes return at once, as Go's own do.
type blockingConn struct {
	net.Conn
	release chan struct{}
	calls   atomic.Int32
}

func (c *blockingConn) Close() error {
	if c.calls.Add(1) == 1 {
		<-c.release
	}
	return nil
}

// A second Close waits for the first to finish (F1): Unpair racing a failed
// handshake's close must not see the connection gone while its socket is
// still open.
func TestTrackedConnCloseWaits(t *testing.T) {
	s := &session{conns: map[net.Conn]struct{}{}}
	bc := &blockingConn{release: make(chan struct{})}
	tc := &trackedConn{Conn: bc, s: s}
	s.conns[tc] = struct{}{}

	go tc.Close() // the handshake failure's close, stuck mid-teardown
	time.Sleep(50 * time.Millisecond)
	second := make(chan struct{})
	go func() { tc.Close(); close(second) }()
	select {
	case <-second:
		t.Fatal("the second Close returned while the first was still closing")
	case <-time.After(200 * time.Millisecond):
	}
	close(bc.release)
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the second Close never returned")
	}
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d conns still tracked", n)
	}
}
