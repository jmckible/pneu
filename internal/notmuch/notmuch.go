// Package notmuch drives the notmuch CLI for one account's database.
//
// Every call shells out with NOTMUCH_CONFIG pointing at the account's config;
// the database that answered is the account. Output is always JSON where the
// subcommand supports it (count and raw parts are the exceptions).
package notmuch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Binary is the notmuch executable; tests may override it.
var Binary = "notmuch"

// Account is one notmuch database, selected by its config file.
type Account struct {
	Name       string
	Email      string
	ConfigPath string
}

// Error is a failed notmuch invocation. Stderr is the tool's own diagnosis.
type Error struct {
	Account string
	Args    []string
	Stderr  string
	Err     error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("notmuch %s [%s]: %s", strings.Join(e.Args, " "), e.Account, msg)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrLocked means the Xapian write lock stayed held past the retry budget.
var ErrLocked = errors.New("notmuch: database write lock held")

func (a Account) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, Binary, args...)
	cmd.Env = append(os.Environ(), "NOTMUCH_CONFIG="+a.ConfigPath)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%w)", ctxErr, err)
		}
		return nil, &Error{Account: a.Name, Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

func (a Account) runJSON(ctx context.Context, v any, args ...string) error {
	out, err := a.run(ctx, nil, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("notmuch %s [%s]: decode: %w", args[0], a.Name, err)
	}
	return nil
}

// idQuery is the only way a Message-ID becomes a query term. notmuch quoted
// phrases escape '"' by doubling it; notmuch emits the same form itself.
func idQuery(msgid string) string {
	return `id:"` + strings.ReplaceAll(msgid, `"`, `""`) + `"`
}

// ---- search ----------------------------------------------------------------

type SearchOpts struct {
	Limit  int // 0 = no limit
	Offset int
}

// ThreadSummary is one row of `notmuch search --output=summary`.
type ThreadSummary struct {
	Account      string     `json:"-"`
	Thread       string     `json:"thread"`
	Timestamp    int64      `json:"timestamp"`
	DateRelative string     `json:"date_relative"`
	Matched      int        `json:"matched"`
	Total        int        `json:"total"`
	Authors      string     `json:"authors"` // matched and unmatched separated by "|"
	Subject      string     `json:"subject"`
	Query        [2]*string `json:"query"` // [matched, unmatched]; nil when empty
	Tags         []string   `json:"tags"`
}

// Search runs the user's query verbatim, newest first. Excluded messages
// (search.exclude_tags, unless the query names the tag) are left out of each
// thread entirely (--exclude=all): the unmatched half of Query then lists
// only messages the thread page shows, so a whole-thread trash never names a
// spam message, and Total counts what the thread page shows.
func (a Account) Search(ctx context.Context, query string, opts SearchOpts) ([]ThreadSummary, error) {
	args := []string{"search", "--format=json", "--output=summary", "--sort=newest-first", "--exclude=all"}
	if opts.Limit > 0 {
		args = append(args, "--limit="+strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		args = append(args, "--offset="+strconv.Itoa(opts.Offset))
	}
	args = append(args, "--", query)
	var out []ThreadSummary
	if err := a.runJSON(ctx, &out, args...); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Account = a.Name
	}
	return out, nil
}

// Oldest is the date of the oldest message in the database, spam and trash
// included, or the zero time when it is empty. With --sort=oldest-first a
// summary's timestamp is its thread's oldest date. Messages without a
// parseable Date header are indexed at timestamp 0 and skipped: they'd put
// the oldest date at 1970.
func (a Account) Oldest(ctx context.Context) (time.Time, error) {
	var out []ThreadSummary
	if err := a.runJSON(ctx, &out, "search", "--format=json", "--output=summary", "--sort=oldest-first", "--exclude=false", "--limit=1", "--", "date:@1.."); err != nil {
		return time.Time{}, err
	}
	if len(out) == 0 {
		return time.Time{}, nil
	}
	return time.Unix(out[0].Timestamp, 0), nil
}

// Count counts messages, or threads when threads is true.
func (a Account) Count(ctx context.Context, query string, threads bool) (int, error) {
	output := "--output=messages"
	if threads {
		output = "--output=threads"
	}
	// count has no --format=json; its output is a bare integer.
	out, err := a.run(ctx, nil, "count", output, "--", query)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// Revision is the database's identity and revision ("uuid lastmod"): equal
// revisions mean nothing in the database changed between them. It costs a
// few milliseconds where a thread count of a large query costs a second.
func (a Account) Revision(ctx context.Context) (string, error) {
	// count --lastmod prints "count<TAB>uuid<TAB>lastmod"; a query that
	// matches nothing keeps the count free.
	out, err := a.run(ctx, nil, "count", "--lastmod", "--", "id:pneu-revision@invalid")
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) != 3 {
		return "", fmt.Errorf("notmuch count --lastmod: unexpected output %q", out)
	}
	return f[1] + " " + f[2], nil
}

// MessageIDs lists the Message-IDs matching query, newest first.
// search.exclude_tags applies unless the query names an excluded tag.
func (a Account) MessageIDs(ctx context.Context, query string) ([]string, error) {
	var out []string
	if err := a.runJSON(ctx, &out, "search", "--format=json", "--output=messages", "--sort=newest-first", "--", query); err != nil {
		return nil, err
	}
	for i, id := range out {
		out[i] = strings.TrimPrefix(id, "id:") // 0.40 prints bare ids; older text output carried the prefix
	}
	return out, nil
}

// IDsQuery is a query matching exactly the given Message-IDs.
func IDsQuery(ids []string) string {
	terms := make([]string, len(ids))
	for i, id := range ids {
		terms[i] = idQuery(id)
	}
	return "(" + strings.Join(terms, " or ") + ")"
}

// Address lists distinct addresses from messages matching query, as
// "Name <addr>" when a name is known (the JSON output carries it for free).
func (a Account) Address(ctx context.Context, query string) ([]string, error) {
	rows, err := a.address(ctx, "--output=address", query)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.NameAddr != "" {
			out = append(out, r.NameAddr)
		} else {
			out = append(out, r.Address)
		}
	}
	return out, nil
}

// AddressEntry is one row of `notmuch address --format=json`.
type AddressEntry struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	NameAddr string `json:"name-addr"`
}

// Recipients lists the distinct To/Cc/Bcc addresses of messages matching
// query, deduplicated by address.
func (a Account) Recipients(ctx context.Context, query string) ([]AddressEntry, error) {
	return a.address(ctx, "--output=recipients", query)
}

func (a Account) address(ctx context.Context, output, query string) ([]AddressEntry, error) {
	var rows []AddressEntry
	err := a.runJSON(ctx, &rows, "address", "--format=json", output, "--deduplicate=address", "--", query)
	return rows, err
}

// Files lists the files of messages matching query, newest message first.
// search.exclude_tags applies unless the query names an excluded tag.
func (a Account) Files(ctx context.Context, query string) ([]string, error) {
	var out []string
	if err := a.runJSON(ctx, &out, "search", "--format=json", "--output=files", "--sort=newest-first", "--", query); err != nil {
		return nil, err
	}
	return out, nil
}

// ThreadOf returns the thread id holding messageID, or "" if no message has
// it. The id is explicit, so search.exclude_tags does not hide it.
func (a Account) ThreadOf(ctx context.Context, messageID string) (string, error) {
	var out []string
	if err := a.runJSON(ctx, &out, "search", "--format=json", "--output=threads", "--exclude=false", "--", idQuery(messageID)); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", nil
	}
	return strings.TrimPrefix(out[0], "thread:"), nil
}

// UserName is the database config's user.name ("" when unset).
func (a Account) UserName(ctx context.Context) (string, error) {
	out, err := a.run(ctx, nil, "config", "get", "user.name")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ---- show ------------------------------------------------------------------

// Message is one message from `notmuch show --format=json`.
type Message struct {
	Account      string            `json:"-"`
	Thread       int               `json:"-"` // index of the thread within one Show result
	Depth        int               `json:"-"` // reply depth in notmuch's thread tree
	ID           string            `json:"id"`
	Match        bool              `json:"match"`
	Excluded     bool              `json:"excluded"`
	Filename     []string          `json:"filename"`
	Timestamp    int64             `json:"timestamp"`
	DateRelative string            `json:"date_relative"`
	Tags         []string          `json:"tags"`
	Headers      map[string]string `json:"headers"`
	Duplicate    int               `json:"duplicate"`
	Body         []Part            `json:"body"`
	Crypto       json.RawMessage   `json:"crypto,omitempty"`
}

// Embedded is the payload of a message/rfc822 part.
type Embedded struct {
	Headers map[string]string `json:"headers"`
	Body    []Part            `json:"body"`
}

// Part is a MIME part. notmuch overloads "content" by content-type: a string
// for leaf text, []Part for multipart/*, []Embedded for message/rfc822, and
// absent for binary leaves (content-length is then the *encoded* size).
type Part struct {
	ID                      int    `json:"id"` // depth-first part number, for --part=N
	ContentType             string `json:"content-type"`
	ContentDisposition      string `json:"content-disposition,omitempty"`
	ContentID               string `json:"content-id,omitempty"` // without angle brackets
	Filename                string `json:"filename,omitempty"`
	ContentCharset          string `json:"content-charset,omitempty"`
	ContentLength           int64  `json:"content-length,omitempty"`
	ContentTransferEncoding string `json:"content-transfer-encoding,omitempty"`

	Content    string     `json:"-"` // leaf text, already charset-decoded to UTF-8
	HasContent bool       `json:"-"`
	Children   []Part     `json:"-"` // multipart/*
	Messages   []Embedded `json:"-"` // message/rfc822

	EncStatus json.RawMessage `json:"encstatus,omitempty"`
	SigStatus json.RawMessage `json:"sigstatus,omitempty"`
}

func (p *Part) UnmarshalJSON(b []byte) error {
	type plain Part
	var raw struct {
		plain
		ID      json.RawMessage `json:"id"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = Part(raw.plain)
	// The schema allows int|string ids; every id notmuch prints today is an int.
	if err := json.Unmarshal(raw.ID, &p.ID); err != nil {
		var s string
		if json.Unmarshal(raw.ID, &s) != nil {
			return fmt.Errorf("part id %s", raw.ID)
		}
		if p.ID, err = strconv.Atoi(s); err != nil {
			p.ID = -1
		}
	}
	c := bytes.TrimSpace(raw.Content)
	switch {
	case len(c) == 0 || string(c) == "null":
	case c[0] == '"':
		p.HasContent = true
		return json.Unmarshal(c, &p.Content)
	case strings.EqualFold(p.ContentType, "message/rfc822"):
		return json.Unmarshal(c, &p.Messages)
	default:
		return json.Unmarshal(c, &p.Children)
	}
	return nil
}

// Walk visits every part depth-first, descending into multiparts and
// embedded messages. Returning false stops the walk.
func Walk(parts []Part, fn func(*Part) bool) bool {
	for i := range parts {
		p := &parts[i]
		if !fn(p) || !Walk(p.Children, fn) {
			return false
		}
		for j := range p.Messages {
			if !Walk(p.Messages[j].Body, fn) {
				return false
			}
		}
	}
	return true
}

// CIDs maps Content-ID (no angle brackets) to part number for cid: URLs.
func (m *Message) CIDs() map[string]int {
	out := map[string]int{}
	Walk(m.Body, func(p *Part) bool {
		if p.ContentID != "" {
			if _, dup := out[p.ContentID]; !dup {
				out[p.ContentID] = p.ID
			}
		}
		return true
	})
	return out
}

// threadNode is [message|null, [threadNode...]].
type threadNode struct {
	Message  *Message
	Children []threadNode
}

func (n *threadNode) UnmarshalJSON(b []byte) error {
	var pair [2]json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil {
		return err
	}
	if err := json.Unmarshal(pair[0], &n.Message); err != nil {
		return err
	}
	return json.Unmarshal(pair[1], &n.Children)
}

// Show returns every message in every thread matching query (whole threads),
// flattened. Messages are grouped by Thread in notmuch's order and sorted by
// date within a thread; Depth keeps the reply-tree position.
func (a Account) Show(ctx context.Context, query string) ([]Message, error) {
	return a.show(ctx, query, "--entire-thread=true")
}

// Headers returns the messages matching query (not their threads) with
// headers only, no bodies. search.exclude_tags does not apply: callers name
// messages by id.
func (a Account) Headers(ctx context.Context, query string) ([]Message, error) {
	return a.show(ctx, query, "--entire-thread=false", "--exclude=false", "--body=false")
}

// ErrNotFound means a query that names one message matched none.
var ErrNotFound = errors.New("notmuch: no such message")

// Message returns one message by Message-ID with its body tree (HTML parts
// included). The id is explicit, so search.exclude_tags does not hide it.
func (a Account) Message(ctx context.Context, messageID string) (Message, error) {
	msgs, err := a.show(ctx, idQuery(messageID), "--entire-thread=false", "--exclude=false")
	if err != nil {
		return Message{}, err
	}
	for _, m := range msgs {
		if m.ID == messageID {
			return m, nil
		}
	}
	return Message{}, fmt.Errorf("%w: %s [%s]", ErrNotFound, messageID, a.Name)
}

func (a Account) show(ctx context.Context, query string, flags ...string) ([]Message, error) {
	args := append([]string{"show", "--format=json", "--include-html"}, flags...)
	var threads [][]threadNode
	if err := a.runJSON(ctx, &threads, append(args, "--", query)...); err != nil {
		return nil, err
	}
	var out []Message
	for ti, roots := range threads {
		start := len(out)
		var walk func([]threadNode, int)
		walk = func(nodes []threadNode, depth int) {
			for _, n := range nodes {
				if n.Message != nil {
					m := *n.Message
					m.Account, m.Thread, m.Depth = a.Name, ti, depth
					out = append(out, m)
				}
				walk(n.Children, depth+1)
			}
		}
		walk(roots, 0)
		sort.SliceStable(out[start:], func(i, j int) bool {
			return out[start+i].Timestamp < out[start+j].Timestamp
		})
	}
	return out, nil
}

// Part returns part n of a message and its content type. Text parts that
// notmuch inlines in JSON come back as that UTF-8 string with
// "charset=utf-8" (raw output keeps the original charset, and notmuch omits
// content-charset whenever it inlines content). Everything else is
// `--format=raw --part=N`.
//
// VERIFY: raw --part=N returned base64/QP-decoded bytes on notmuch 0.40
// (TestReadPath/Part checks a base64 PNG byte-for-byte); recheck on upgrades.
func (a Account) Part(ctx context.Context, messageID string, n int) ([]byte, string, error) {
	q := idQuery(messageID)
	var meta Part
	if err := a.runJSON(ctx, &meta, "show", "--format=json", "--part="+strconv.Itoa(n), "--", q); err != nil {
		return nil, "", err
	}
	if meta.ID != n {
		return nil, "", fmt.Errorf("notmuch [%s]: no part %d in %s", a.Name, n, q)
	}
	if meta.HasContent {
		return []byte(meta.Content), meta.ContentType + "; charset=utf-8", nil
	}
	body, err := a.run(ctx, nil, "show", "--format=raw", "--part="+strconv.Itoa(n), "--", q)
	if err != nil {
		return nil, "", err
	}
	ct := meta.ContentType
	if meta.ContentCharset != "" {
		ct += "; charset=" + meta.ContentCharset
	}
	return body, ct, nil
}

// ---- reply -----------------------------------------------------------------

type ReplyHeaders struct {
	Subject    string `json:"Subject"`
	From       string `json:"From"`
	To         string `json:"To,omitempty"`
	Cc         string `json:"Cc,omitempty"`
	Bcc        string `json:"Bcc,omitempty"`
	InReplyTo  string `json:"In-reply-to"` // notmuch's spelling
	References string `json:"References"`
}

type Reply struct {
	Headers  ReplyHeaders `json:"reply-headers"`
	Original Message      `json:"original"`
}

// Reply builds reply headers for one message; all selects --reply-to=all.
func (a Account) Reply(ctx context.Context, messageID string, all bool) (Reply, error) {
	to := "--reply-to=sender"
	if all {
		to = "--reply-to=all"
	}
	var r Reply
	if err := a.runJSON(ctx, &r, "reply", "--format=json", to, "--", idQuery(messageID)); err != nil {
		var ne *Error
		if errors.As(err, &ne) && strings.Contains(ne.Stderr, "matched 0 messages") {
			return Reply{}, fmt.Errorf("%w: %s [%s]", ErrNotFound, messageID, a.Name)
		}
		return Reply{}, err
	}
	r.Original.Account = a.Name
	return r, nil
}

// ---- tag -------------------------------------------------------------------

// TagTimeout bounds a Tag call whose context has no deadline.
var TagTimeout = 10 * time.Second

// isLockErr matches Xapian's lock failure on notmuch builds without
// retry_lock ("Unable to get write lock on ...: already locked").
func isLockErr(stderr string) bool {
	return strings.Contains(stderr, "already locked") ||
		strings.Contains(stderr, "Unable to acquire") ||
		strings.Contains(stderr, "Unable to get write lock")
}

// Tag applies changes ("+inbox", "-unread") to exactly the given messages.
//
// Xapian has one writer. notmuch built with retry_lock (the Arch package is)
// blocks until the lock frees; other builds fail with "already locked", which
// is retried with backoff. Either way the wait is bounded by ctx or TagTimeout,
// and running out returns ErrLocked. Killing notmuch while it waits for the
// lock is safe: nothing has been written.
func (a Account) Tag(ctx context.Context, changes []string, messageIDs []string) error {
	if len(changes) == 0 || len(messageIDs) == 0 {
		return nil
	}
	var ops strings.Builder
	for _, c := range changes {
		if len(c) < 2 || (c[0] != '+' && c[0] != '-') {
			return fmt.Errorf("notmuch: bad tag change %q", c)
		}
		ops.WriteByte(c[0])
		ops.WriteString(encodeTag(c[1:]))
		ops.WriteByte(' ')
	}
	var batch bytes.Buffer
	for _, id := range messageIDs {
		if id == "" || strings.ContainsAny(id, "\r\n\x00") {
			return fmt.Errorf("notmuch: bad message id %q", id)
		}
		fmt.Fprintf(&batch, "%s-- %s\n", ops.String(), idQuery(id))
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, TagTimeout)
		defer cancel()
	}
	delay := 50 * time.Millisecond
	for {
		_, err := a.run(ctx, bytes.NewReader(batch.Bytes()), "tag", "--batch")
		if err == nil {
			return nil
		}
		var ne *Error
		locked := errors.As(err, &ne) && isLockErr(ne.Stderr)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Killed while blocked on the lock (retry_lock builds) or out of retries.
			return fmt.Errorf("%w: %w", ErrLocked, err)
		}
		if ctx.Err() != nil {
			return err
		}
		if !locked {
			return err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%w: %w", ErrLocked, err)
			}
			return err
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Second)
	}
}

// encodeTag hex-encodes everything but a conservative set, per the batch
// format: spaces must be %20, and any tag byte may be %NN.
func encodeTag(tag string) string {
	var b strings.Builder
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.@:/=", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02x", c)
		}
	}
	return b.String()
}
