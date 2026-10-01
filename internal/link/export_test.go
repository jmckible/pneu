package link

import "net"

// Seams for link_test: set before Start.
func SetBeforePublish(l *Link, f func())     { l.beforePublish = f }
func SetBeforeClose(l *Link, f func())       { l.beforeClose = f }
func SetAfterDial(l *Link, f func(net.Conn)) { l.afterDial = f }

// Owned is how many sessions the link owns and how many connections they
// hold open.
func Owned(l *Link) (sessions, conns int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for s := range l.owned {
		s.mu.Lock()
		conns += len(s.conns)
		s.mu.Unlock()
	}
	return len(l.owned), conns
}

// GoDown takes the live session down, as a failed renewal or probe does.
func GoDown(l *Link) {
	l.mu.Lock()
	s := l.sess
	l.mu.Unlock()
	l.setDown(s, Refused, "test")
}

// Current is the live session's id, 0 while down.
func Current(l *Link) SessionID {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess == nil {
		return 0
	}
	return l.sess.id
}
