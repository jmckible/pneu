package web

import (
	"log"
	"net/http"
)

// syncNow handles POST /sync (R in the app): queue an immediate sync on every
// account. The engine coalesces a request made while one is pending or
// running, so repeated presses cost one extra run at most. Host, Origin and
// the session are the middleware's, as for every POST. The answer only says
// the request was queued; the SSE `sync` event reports a pull that changed
// something.
func (s *Server) syncNow(w http.ResponseWriter, r *http.Request) {
	if s.Syncer == nil {
		tagFail(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	queued := s.syncAll()
	if queued == 0 && len(s.Accounts) > 0 {
		tagFail(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	tagJSON(w, http.StatusOK, struct {
		OK       bool `json:"ok"`
		Accounts int  `json:"accounts"`
	}{true, queued})
}

// syncAll queues an immediate sync on every account and says how many took
// the request.
func (s *Server) syncAll() int {
	queued := 0
	for _, a := range s.Accounts {
		if err := s.Syncer.SyncNow(a.Name); err != nil {
			log.Printf("sync now %s: %v", a.Name, err)
			continue
		}
		queued++
	}
	return queued
}
