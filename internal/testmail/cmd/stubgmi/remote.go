package main

// The simulated Gmail side: a static mailbox built from testdata/mail,
// multiplied to STUBGMI_COUNT messages, plus its labels and the OAuth
// credentials lieer would hold. The mailbox never changes, so every partial
// pull is "everything is up-to-date".

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/mail"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jmckible/pneu/internal/testmail"
	"github.com/jmckible/pneu/testdata"
)

// source is one fixture message.
type source struct {
	gid  string
	raw  []byte
	id   string // Message-ID without brackets
	date time.Time
	tags []string
}

// rmsg is one message in the simulated mailbox. Clones (gen > 0) render
// their raw bytes on demand, so a large STUBGMI_COUNT costs no memory.
type rmsg struct {
	gid  string
	src  *source
	gen  int // 0: the fixture itself; n: the nth copy, n spans older
	date time.Time
	tags []string // notmuch tags lieer would give it (labels translated)
}

func (m *rmsg) messageID() string {
	if m.gen == 0 {
		return m.src.id
	}
	return clonePrefix(m.gen) + m.src.id
}

func clonePrefix(gen int) string { return fmt.Sprintf("c%d.", gen) }

// raw is what messages.get(format=raw) would return, before lieer's CRLF
// conversion (local.py:Local.store). A clone gets its own Message-ID, its
// In-Reply-To and References rewritten the same way (so copies thread among
// themselves), and its Date moved back by gen spans.
func (m *rmsg) raw() []byte {
	if m.gen == 0 {
		return m.src.raw
	}
	return rewriteHeaders(m.src.raw, clonePrefix(m.gen), m.date)
}

func rewriteHeaders(raw []byte, prefix string, date time.Time) []byte {
	sep := []byte("\n\n")
	end := bytes.Index(raw, sep)
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 && (end < 0 || i < end) {
		end = i
	}
	if end < 0 {
		end = len(raw)
	}
	head, body := raw[:end], raw[end:]
	var out bytes.Buffer
	cur := ""
	for _, line := range bytes.SplitAfter(head, []byte("\n")) {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			if i := bytes.IndexByte(line, ':'); i > 0 {
				cur = strings.ToLower(string(line[:i]))
			}
		}
		switch cur {
		case "message-id", "in-reply-to", "references":
			line = bytes.ReplaceAll(line, []byte("<"), []byte("<"+prefix))
		case "date":
			if line[0] == ' ' || line[0] == '\t' {
				continue // folded Date: dropped, the new one is one line
			}
			eol := "\n"
			if bytes.HasSuffix(line, []byte("\r\n")) {
				eol = "\r\n"
			}
			line = []byte("Date: " + date.Format(time.RFC1123Z) + eol)
		}
		out.Write(line)
	}
	out.Write(body)
	return out.Bytes()
}

// mailbox is the remote as a full pull lists it: newest first, the order
// Gmail's messages.list returns (remote.py:Remote.all_messages; lieer keeps
// that order through gmailieer.py:Gmailieer.get_content).
type mailbox struct {
	msgs  []*rmsg
	byGID map[string]*rmsg
}

// historyID is the mailbox's current historyId; constant, since it never changes.
func (b *mailbox) historyID() int64 { return 7_000_000 + int64(len(b.msgs)) }

func loadMailbox(which string, count int) (*mailbox, error) {
	manifest, err := testmail.LoadManifest()
	if err != nil {
		return nil, err
	}
	accounts := []string{"personal", "work"}
	if which != "all" {
		if _, ok := manifest[which]; !ok {
			return nil, fmt.Errorf("STUBGMI_FIXTURE=%q: want personal, work or all", which)
		}
		accounts = []string{which}
	}
	var srcs []*source
	seen := map[string]bool{}
	for _, acct := range accounts {
		dir := path.Join("mail", acct, "gmail", "mail", "cur")
		ents, err := fs.ReadDir(testdata.FS, dir)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			raw, err := testdata.FS.ReadFile(path.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			id := strings.Trim(strings.TrimSpace(msg.Header.Get("Message-Id")), "<>")
			if id == "" || seen[id] {
				continue // mail between the two accounts is in both
			}
			seen[id] = true
			date, err := msg.Header.Date()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			gid, _, _ := strings.Cut(e.Name(), "!")
			srcs = append(srcs, &source{gid: gid, raw: raw, id: id, date: date, tags: manifest[acct][id]})
		}
	}
	if len(srcs) == 0 {
		return nil, errors.New("no fixture messages")
	}
	if count <= 0 {
		count = len(srcs)
	}
	oldest, newest := srcs[0].date, srcs[0].date
	for _, s := range srcs {
		oldest = minTime(oldest, s.date)
		newest = maxTime(newest, s.date)
	}
	span := newest.Sub(oldest) + 24*time.Hour
	var msgs []*rmsg
	for gen := 0; len(msgs) < count; gen++ {
		for i, s := range srcs {
			m := &rmsg{gid: s.gid, src: s, gen: gen, date: s.date, tags: s.tags}
			if gen > 0 {
				m.date = s.date.Add(-time.Duration(gen) * span)
				// Gmail ids are hex of the millisecond timestamp shifted left 20 bits.
				m.gid = fmt.Sprintf("%x", m.date.UnixMilli()<<20|int64(gen*len(srcs)+i)&0xfffff)
				// Older copies have been read and archived long ago.
				m.tags = slices.DeleteFunc(slices.Clone(s.tags), func(t string) bool { return t == "inbox" || t == "unread" })
			}
			msgs = append(msgs, m)
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].date.After(msgs[j].date) })
	msgs = msgs[:count]
	b := &mailbox{msgs: msgs, byGID: make(map[string]*rmsg, len(msgs))}
	for _, m := range msgs {
		b.byGID[m.gid] = m
	}
	return b, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// translateLabels is local.py:Local.translate_labels_default: Gmail's system
// labels and the notmuch tags lieer gives them.
var translateLabels = [][2]string{
	{"INBOX", "inbox"}, {"SPAM", "spam"}, {"TRASH", "trash"}, {"UNREAD", "unread"},
	{"STARRED", "flagged"}, {"IMPORTANT", "important"}, {"SENT", "sent"}, {"DRAFT", "draft"},
	{"CHAT", "chat"}, {"CATEGORY_PERSONAL", "personal"}, {"CATEGORY_SOCIAL", "social"},
	{"CATEGORY_PROMOTIONS", "promotions"}, {"CATEGORY_UPDATES", "updates"}, {"CATEGORY_FORUMS", "forums"},
}

// labels is what users.labels.list returns, as `gmi pull -t` prints it
// (gmailieer.py:Gmailieer.pull): system labels keep their names as ids,
// user labels get Label_N ids.
func (b *mailbox) labels(slashToDot bool) [][2]string {
	var out [][2]string // name, id
	system := map[string]bool{}
	for _, t := range translateLabels {
		out = append(out, [2]string{t[0], t[0]})
		system[t[1]] = true
	}
	user := map[string]bool{}
	for _, m := range b.msgs {
		for _, t := range m.tags {
			if !system[t] {
				user[t] = true
			}
		}
	}
	names := make([]string, 0, len(user))
	for t := range user {
		names = append(names, t)
	}
	sort.Strings(names)
	for i, t := range names {
		if slashToDot {
			t = strings.ReplaceAll(t, ".", "/")
		}
		out = append(out, [2]string{t, fmt.Sprintf("Label_%d", i+1)})
	}
	return out
}

// maildirFlags is local.py:Local.__make_maildir_name__'s info part.
func maildirFlags(tags []string) string {
	info := "2,"
	if slices.Contains(tags, "draft") {
		info += "D"
	}
	if slices.Contains(tags, "flagged") {
		info += "F"
	}
	if !slices.Contains(tags, "unread") {
		info += "S"
	}
	return info
}
