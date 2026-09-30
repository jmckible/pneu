package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Hub fans server events out to every open /events stream.
type Hub struct {
	Heartbeat time.Duration

	mu      sync.Mutex
	clients map[chan []byte]struct{}
	closed  chan struct{}
}

func NewHub() *Hub {
	return &Hub{Heartbeat: 25 * time.Second, clients: map[chan []byte]struct{}{}, closed: make(chan struct{})}
}

// Broadcast sends `event: name` with payload as JSON data. A client whose
// buffer is full is dropped rather than stalling everyone; its EventSource
// reconnects and the page re-renders from notmuch anyway.
func (h *Hub) Broadcast(name string, payload any) {
	msg := encodeEvent(name, payload)
	if msg == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- msg:
		default:
			delete(h.clients, c)
			close(c)
		}
	}
}

// encodeEvent is one SSE event, nil if payload doesn't marshal.
func encodeEvent(name string, payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("sse: marshal %s: %v", name, err)
		return nil
	}
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", name, data)
}

// Close ends every stream; register it with http.Server.RegisterOnShutdown
// since SSE connections never go idle on their own.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.closed:
	default:
		close(h.closed)
	}
}

func (h *Hub) subscribe() chan []byte {
	c := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) unsubscribe(c chan []byte) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c)
	}
	h.mu.Unlock()
}

// serve streams c, already subscribed (Server.subscribe): subscribed
// before the preamble, so once the client sees ": open" no broadcast can be
// missed. first, the stream's hello, goes out ahead of anything queued.
func (h *Hub) serve(w http.ResponseWriter, r *http.Request, c chan []byte, first []byte) {
	defer h.unsubscribe(c)
	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": open\n\n"); err != nil {
		return
	}
	if _, err := w.Write(first); err != nil || rc.Flush() != nil {
		return
	}
	tick := time.NewTicker(h.Heartbeat)
	defer tick.Stop()

	for {
		var msg []byte
		select {
		case <-r.Context().Done():
			return
		case <-h.closed:
			return
		case <-tick.C:
			msg = []byte(": ping\n\n")
		case m, ok := <-c:
			if !ok {
				return // dropped as a slow client
			}
			msg = m
		}
		if _, err := w.Write(msg); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
