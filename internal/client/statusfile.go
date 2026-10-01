package client

// status.json on a client (docs/client.md, "Status file and the bar"; R12,
// R14, N14): version 2, owned by this daemon. updated and running are this
// daemon's; unread, senders and accounts the last valid status from the
// server, held to cleanStatus's limits; server says what the link is, in
// local codes only.

import (
	"context"
	"log"
	"time"

	"github.com/jmckible/pneu/internal/link"
	"github.com/jmckible/pneu/internal/web"
)

// StatusGap is the least time between two status.json writes: a server
// flooding valid status events costs a parse each, not a disk write each.
const StatusGap = time.Second

// StatusV2 is a client's status.json.
type StatusV2 struct {
	Version  int                 `json:"version"` // 2
	Updated  string              `json:"updated"` // this daemon's clock
	Running  bool                `json:"running"` // this daemon
	Unread   int                 `json:"unread"`
	Senders  []string            `json:"senders"`
	Accounts []web.StatusAccount `json:"accounts"`
	Server   StatusServer        `json:"server"`
}

// StatusServer is the link as the bar sees it. Name is the SSH target the
// user paired with; Link and Reason are local codes; StatusAt is when the
// last valid status arrived here (null before one did), so the bar can
// call the counts stale while Updated keeps ticking. Update is the
// version nudge, null until step 7 builds it.
type StatusServer struct {
	Name      string  `json:"name"`
	Link      string  `json:"link"` // starting | up | down
	LinkSince string  `json:"linkSince"`
	StatusAt  *string `json:"statusAt"`
	Reason    *string `json:"reason"`
	Update    *string `json:"update"`
}

// wakeStatus asks for a rewrite; requests while one waits collapse.
func (d *Daemon) wakeStatus() {
	select {
	case d.statusWake <- struct{}{}:
	default:
	}
}

// runStatus writes status.json now, again on every wake but at most once
// per statusGap (the latest state wins: it's read at write time), on its
// own heartbeat, and once more with running false when ctx ends. Every
// write happens here, so they never interleave.
func (d *Daemon) runStatus(ctx context.Context) {
	d.writeStatus(true)
	last := time.Now()
	heartbeat := time.NewTicker(web.StatusHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			d.writeStatus(false)
			return
		case <-d.statusWake:
		case <-heartbeat.C:
		}
		if wait := time.Until(last.Add(d.statusGap)); wait > 0 {
			select {
			case <-ctx.Done():
				d.writeStatus(false)
				return
			case <-time.After(wait):
			}
		}
		d.writeStatus(true)
		last = time.Now()
	}
}

func (d *Daemon) writeStatus(running bool) {
	if err := web.WriteStatusFile(d.statusPath, d.statusDoc(running)); err != nil {
		log.Printf("client: status file: %v", err)
	}
	if d.onStatusWrite != nil {
		d.onStatusWrite()
	}
}

// statusDoc is status.json as of now.
func (d *Daemon) statusDoc(running bool) StatusV2 {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.live.link
	doc := StatusV2{
		Version: 2, Updated: time.Now().UTC().Format(time.RFC3339), Running: running,
		Senders: []string{}, Accounts: []web.StatusAccount{},
		Server: StatusServer{Name: d.server, Link: linkState(st.Reason), LinkSince: st.Since.UTC().Format(time.RFC3339)},
	}
	if st.Reason != link.Up {
		r := string(st.Reason)
		doc.Server.Reason = &r
	}
	if s := d.live.status; s != nil {
		doc.Unread, doc.Senders, doc.Accounts = s.Unread, s.Senders, s.Accounts
		at := d.live.statusAt.UTC().Format(time.RFC3339)
		doc.Server.StatusAt = &at
	}
	return doc
}
