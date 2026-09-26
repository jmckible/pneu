package main

// pull, push and send, after gmailieer.py.

import (
	"bytes"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	pageSize  = 100 // messages.list's default page; Remote.all_messages passes maxResults=None
	batchSize = 50  // Remote.BATCH_REQUEST_SIZE
)

// pull is Gmailieer.pull.
func (g *gmi) pull() error {
	switch {
	case g.args["force"] != "":
		g.vprint("pull: full synchronization (forced)")
		return g.fullPull()
	case g.local.st.LastHistoryID == 0:
		g.vprint("pull: full synchronization (no previous synchronization state)")
		return g.fullPull()
	default:
		g.vprint("pull: partial synchronization.. (hid: %d)", g.local.st.LastHistoryID)
		return g.partialPull()
	}
}

// partialPull is Gmailieer.partial_pull against a mailbox that never
// changes: no history, so no bars, just the summary.
func (g *gmi) partialPull() error {
	lastID := g.box.historyID()
	g.vprint("pull: everything is up-to-date.")
	g.local.st.LastHistoryID = lastID
	if err := g.local.st.write(); err != nil {
		return err
	}
	g.vprint("current historyId: %d", lastID)
	return nil
}

// pacing splits STUBGMI_DURATION: a tenth listing, the rest downloading.
func (g *gmi) pacing(pages, batches int) (perPage, perBatch time.Duration) {
	d := envDur("STUBGMI_DURATION")
	if d <= 0 {
		return 0, 0
	}
	return d / 10 / time.Duration(max(pages, 1)), d * 9 / 10 / time.Duration(max(batches, 1))
}

// fullPull is Gmailieer.full_pull.
func (g *gmi) fullPull() error {
	resume := g.args["resume"] != ""
	listing := g.barCreate(1, "fetching messages")
	lastID := g.box.historyID()
	_, statErr := os.Stat(resumeFile)
	exists := statErr == nil
	var prev *resumePull
	var err error
	switch {
	case !resume:
		if exists {
			g.vprint("pull: previous pull can be resumed using --resume")
		}
		prev, err = g.loadResume(lastID)
	case !exists:
		g.vprint("pull: no previous resume file exists, continuing with full pull")
		prev, err = g.loadResume(lastID)
	default:
		g.vprint("pull: attempting to resume previous pull..")
		prev, err = g.loadResume(lastID) // the stub's history ids never expire
	}
	if err != nil {
		return err
	}

	limit := -1
	if v := g.args["limit"]; v != "" {
		limit, _ = strconv.Atoi(v)
	}
	msgs := g.box.msgs
	pages := (len(msgs) + pageSize - 1) / pageSize
	need := 0
	for _, m := range msgs {
		if !g.local.has(m.gid) {
			need++
		}
	}
	perPage, perBatch := g.pacing(pages, (need+batchSize-1)/batchSize)
	var listed []*rmsg
	for i := 0; i < len(msgs); i += pageSize {
		if err := sleep(perPage); err != nil {
			return err
		}
		page := msgs[i:min(i+pageSize, len(msgs))]
		listing.update(len(page))
		listed = append(listed, page...)
		if limit >= 0 && len(listed) >= limit {
			break
		}
	}
	listing.close()

	if g.local.cfg.RemoveLocalMessages {
		if limit >= 0 {
			return raise(`ValueError: --limit with "remove_local_messages" will cause lots of messages to be deleted`,
				lieerFrame("gmailieer.py", 627, "pull", "self.full_pull()"),
				lieerFrame("gmailieer.py", 874, "full_pull", "raise ValueError("))
		}
		var remove []string
		for gid := range g.local.gids {
			if _, ok := g.box.byGID[gid]; !ok {
				remove = append(remove, gid)
			}
		}
		slices.Sort(remove)
		b := g.barCreate(len(remove), "removing deleted")
		for _, gid := range remove {
			os.Remove(filepath.Join(g.local.md, g.local.gids[gid]))
			delete(g.local.gids, gid)
			b.update(1)
		}
		if len(remove) > 0 {
			if _, err := notmuchOut("new", "--quiet"); err != nil {
				return err
			}
		}
		b.close()
	}

	if len(listed) == 0 {
		g.vprint("pull: no messages.")
	} else {
		updated, err := g.getContent(listed, perBatch)
		if err != nil {
			return err
		}
		var needsUpdate []*rmsg
		for _, m := range listed {
			if !updated[m.gid] {
				needsUpdate = append(needsUpdate, m)
			}
		}
		if resume {
			g.vprint("pull: resume: skipping metadata for %d messages", len(prev.MetaFetched))
			needsUpdate = slices.DeleteFunc(needsUpdate, func(m *rmsg) bool { return slices.Contains(prev.MetaFetched, m.gid) })
		}
		if err := g.getMeta(needsUpdate, prev, resume); err != nil {
			return err
		}
	}

	rev, err := notmuchRevision()
	if err != nil {
		return err
	}
	g.local.st.Lastmod = rev
	if err := g.local.st.write(); err != nil {
		return err
	}
	if resume {
		g.local.st.LastHistoryID = prev.LastID
	} else {
		g.local.st.LastHistoryID = lastID
	}
	if err := g.local.st.write(); err != nil {
		return err
	}
	g.vprint("pull: complete, removing resume file")
	os.Remove(resumeFile)
	g.vprint("current historyId: %d, current revision: %d", lastID, rev)
	if resume {
		g.vprint("pull: resume: performing partial pull to complete")
		if err := g.partialPull(); err != nil {
			return err
		}
		g.vprint("pull: note that local changes made in the interim might be ignored in the next push")
	}
	return nil
}

// noise mimics Remote.get_messages adapting its batch size to Gmail's
// concurrency errors, which real pulls print constantly (a 7,498-message
// pull printed about 180 of these lines).
func noise(batch int) {
	if os.Getenv("STUBGMI_NOISE") == "0" {
		return
	}
	switch batch % 13 {
	case 5:
		fmt.Println("remote: reducing batch request size to: 25")
	case 6:
		fmt.Println("remote: increasing batch request size to: 50")
	}
}

// getContent is Gmailieer.get_content: download what isn't local, newest
// first, a batch at a time, each batch stored and indexed before the next.
func (g *gmi) getContent(listed []*rmsg, perBatch time.Duration) (map[string]bool, error) {
	var need []*rmsg
	for _, m := range listed {
		if !g.local.has(m.gid) {
			need = append(need, m)
		}
	}
	updated := map[string]bool{}
	if len(need) == 0 {
		g.vprint("receiving content: everything up-to-date.")
		return updated, nil
	}
	b := g.barCreate(len(need), "receiving content")
	failAt := int(float64(len(need)) * envFloat("STUBGMI_FAIL_AT", 0.5))
	stall, kill := false, false
	stall, kill = failMode("stall"), failMode("kill")
	for i, n := 0, 0; i < len(need); i, n = i+batchSize, n+1 {
		batch := need[i:min(i+batchSize, len(need))]
		if err := sleep(perBatch); err != nil {
			return nil, err
		}
		noise(n)
		if n == 0 {
			if err := slowBatch(envDur("STUBGMI_SLOW_BATCH")); err != nil {
				return nil, err
			}
		}
		var stored []*rmsg
		for j, m := range batch {
			if (stall || kill) && i+j >= failAt {
				if err := g.local.index(stored); err != nil {
					return nil, err
				}
				if kill {
					g.dieMidStore(m)
				}
				for !interrupted.Load() {
					time.Sleep(50 * time.Millisecond) // stalled: silent, never exits
				}
				return nil, errInterrupt
			}
			if err := g.local.store(m, m.raw()); err != nil {
				g.local.index(stored)
				return nil, err
			}
			stored = append(stored, m)
			updated[m.gid] = true
			b.update(1)
		}
		if err := g.local.index(stored); err != nil {
			return nil, err
		}
		if interrupted.Load() {
			return nil, errInterrupt
		}
	}
	b.close()
	return updated, nil
}

// slowBatch spends d silently reading, as lieer does while one batch
// request's response streams in (remote.py:Remote.get_messages sends 50
// messages.get in one HTTP batch and stores them only when it's complete).
// The reads show in /proc/<pid>/io, not on stdout.
func slowBatch(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	f, err := os.Open("/dev/zero")
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	for end := time.Now().Add(d); time.Now().Before(end); {
		if _, err := f.Read(buf); err != nil {
			return err
		}
		if err := sleep(50 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// dieMidStore leaves m in tmp/ the way a kill between Local.store's write
// and its rename does, then dies as SIGKILL would have it.
func (g *gmi) dieMidStore(m *rmsg) {
	name := m.gid + ":" + maildirFlags(m.tags)
	p := filepath.Join(g.local.md, "tmp", name)
	os.WriteFile(p, bytes.ReplaceAll(m.raw(), []byte("\r\n"), []byte("\n")), 0o666)
	os.Chtimes(p, m.date, m.date) // killed after os.utime, before os.rename
	os.Stdout.Sync()
	logInvocation("killed")
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

// getMeta is Gmailieer.get_meta: fetch labels for messages already local
// and overwrite their tags (Local.update_tags), recording progress in the
// resume file per batch.
func (g *gmi) getMeta(ms []*rmsg, prev *resumePull, resume bool) error {
	if len(ms) == 0 {
		g.vprint("receiving metadata: everything up-to-date.")
		return nil
	}
	total := len(ms)
	if resume {
		total += len(prev.MetaFetched)
	}
	b := g.barCreate(total, "receiving metadata")
	if resume {
		b.update(len(prev.MetaFetched))
	}
	for i := 0; i < len(ms); i += batchSize {
		batch := ms[i:min(i+batchSize, len(ms))]
		if err := g.local.updateTags(batch); err != nil {
			return err
		}
		gids := make([]string, len(batch))
		for j, m := range batch {
			b.update(1)
			gids[j] = m.gid
		}
		if prev != nil {
			if err := prev.update(gids); err != nil {
				return err
			}
		}
		if interrupted.Load() {
			return errInterrupt
		}
	}
	b.close()
	return nil
}

// push is Gmailieer.push: the messages changed since lastmod are compared
// with the remote; the simulated remote takes the changes without keeping
// them (a later full pull would put the fixture's tags back).
func (g *gmi) push() error {
	rev, err := notmuchRevision()
	if err != nil {
		return err
	}
	st := g.local.st
	if rev == st.Lastmod {
		g.vprint("push: everything is up-to-date.")
		return nil
	}
	q := fmt.Sprintf("path:%s/** and lastmod:%d..%d", g.local.nmRelative, st.Lastmod, rev)
	out, err := notmuchOut("dump", "--format=batch-tag", "--", q)
	if err != nil {
		return err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "-- id:") {
			lines = append(lines, l)
		}
	}
	if limit, err := strconv.Atoi(g.args["limit"]); err == nil && len(lines) > limit {
		lines = lines[:limit]
	}
	b := g.barCreate(len(lines), "receiving metadata")
	for range lines {
		b.update(1)
	}
	b.close()
	b = g.barCreate(len(lines), "resolving changes")
	actions := 0
	for _, l := range lines {
		b.update(1)
		if g.differsFromRemote(l) {
			actions++
		}
	}
	b.close()
	if actions > 0 {
		b = g.barCreate(actions, "pushing, 0 changed")
		for range actions {
			b.update(1)
		}
		b.close()
	} else {
		g.vprint("push: nothing to push")
	}
	st.Lastmod = rev
	if err := st.write(); err != nil {
		return err
	}
	g.vprint("remote historyId: %d", g.box.historyID())
	return nil
}

// differsFromRemote reports whether a dump line's tags (less ignored ones)
// differ from the mailbox's labels for that message (Remote.update).
func (g *gmi) differsFromRemote(line string) bool {
	tagPart, idPart, _ := strings.Cut(line, "-- id:")
	id := decodeTerm(idPart)
	var remote []string
	found := false
	for _, m := range g.box.msgs {
		if m.messageID() == id {
			remote, found = m.tags, true
			break
		}
	}
	if !found {
		return false
	}
	var local []string
	for _, t := range strings.Fields(tagPart) {
		if t = decodeTerm(strings.TrimPrefix(t, "+")); !g.local.ignored(t) {
			local = append(local, t)
		}
	}
	remote = slices.DeleteFunc(slices.Clone(remote), g.local.ignored)
	slices.Sort(local)
	slices.Sort(remote)
	return !slices.Equal(local, remote)
}

// send is Gmailieer.send: the message goes to "Gmail", which assigns it an
// id, and lieer stores the sent copy locally with the SENT label.
func (g *gmi) send() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return raise("ValueError: "+err.Error(), lieerFrame("gmailieer.py", 1028, "send", "eml = email.message_from_bytes(msg)"))
	}
	header := map[string]bool{}
	for _, f := range []string{"To", "Cc", "Bcc"} {
		for _, v := range msg.Header[f] {
			if list, err := mail.ParseAddressList(v); err == nil {
				for _, a := range list {
					header[a.Address] = true
				}
			}
		}
	}
	cli := g.pos
	if g.args["read_recipients"] != "" {
		var missing []string
		for _, r := range cli {
			if !header[r] {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			return raise("ValueError: Recipients passed via sendmail(1) arguments, but not part of message headers: "+strings.Join(missing, ", "),
				lieerFrame("gmailieer.py", 1048, "send", "raise ValueError("))
		}
	} else if len(cli) != len(header) || slices.ContainsFunc(cli, func(r string) bool { return !header[r] }) {
		hs := make([]string, 0, len(header))
		for h := range header {
			hs = append(hs, h)
		}
		return raise(fmt.Sprintf("ValueError: Recipients passed via sendmail(1) arguments (%s) differ from those in message headers (%s), perhaps you are missing the '-t' option?",
			strings.Join(cli, ", "), strings.Join(hs, ", ")), lieerFrame("gmailieer.py", 1054, "send", "raise ValueError("))
	}
	g.vprint("sending message, from: %s..", msg.Header.Get("From"))
	threadNote := ""
	if irt := strings.Trim(strings.TrimSpace(msg.Header.Get("In-Reply-To")), "<>"); irt != "" {
		g.vprint("looking for original message: %s", irt)
		files, _ := notmuchOut("search", "--output=files", "id:"+quoteID(irt))
		if files = strings.TrimSpace(files); files == "" {
			threadNote = "warning: could not find parent message, sent message will not be associated in the same thread"
		} else if gid := g.local.filenameToGID(filepath.Base(strings.SplitN(files, "\n", 2)[0])); gid != "" {
			threadNote = "found existing thread for new message: " + gid
		} else {
			threadNote = "warning: could not find gid of parent message, sent message will not be associated in the same thread"
		}
		g.vprint("%s", threadNote)
	}
	// Gmail assigns the id and, when missing, a Message-ID.
	now := time.Now()
	gid := fmt.Sprintf("%x", now.UnixMilli()<<20|int64(now.Nanosecond()&0xfffff))
	id := strings.Trim(strings.TrimSpace(msg.Header.Get("Message-Id")), "<>")
	if id == "" {
		id = "stub-" + gid + "@mail.gmail.com"
		raw = append([]byte("Message-ID: <"+id+">\r\n"), raw...)
	}
	sent := &rmsg{gid: gid, src: &source{gid: gid, raw: raw, id: id, date: now, tags: []string{"sent"}}, date: now, tags: []string{"sent"}}
	b := g.barCreate(1, "receiving content")
	if err := g.local.store(sent, raw); err != nil {
		return err
	}
	if err := g.local.index([]*rmsg{sent}); err != nil {
		return err
	}
	b.update(1)
	b.close()
	b = g.barCreate(1, "receiving metadata")
	b.update(1)
	b.close()
	g.vprint("message sent successfully: %s", gid)
	return nil
}
