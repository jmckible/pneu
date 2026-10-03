package main

// The consent relay (docs/client.md, "As built: step 8", Y1). lieer on the
// server waits for Google's redirect on its own localhost:8080 (and push's
// consent on the server waits on stdin for it), but the browser is here.
// No SSH forward carries it: a consent command on a client listens on
// this machine's 127.0.0.1:8080 and [::1]:8080 itself (callback.go), takes
// one well-formed callback for this consent, and hands its query to the
// session, which sends it to the server as one line on stdin.

import (
	"fmt"
	"net"
)

// listenRelay binds port on both loopbacks with Go's SO_REUSEADDR, so a
// TIME_WAIT from a recent consent doesn't refuse.
func listenRelay(port int) ([]net.Listener, error) {
	lns, err := listenLoopback(port)
	if err != nil {
		return nil, fmt.Errorf("%w here (or can't be bound): Google's consent comes back to it, so this command needs it free. `ss -ltnp 'sport = :%d'` shows what holds it", err, port)
	}
	return lns, nil
}

// relayDone is the relay's answer to the callback it takes.
const relayDone = "pneu: Google's answer is on its way to the server. You can close this tab; the terminal shows the result.\n"

// startRelay serves the relay on lns.
func startRelay(lns []net.Listener) *callbackServer { return startCallback(lns, relayDone) }
