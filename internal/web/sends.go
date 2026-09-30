package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SendKeep is how long a send's record is kept: the stated limit of the
// guarantee that a draft is never sent twice. compose.js gives a draft
// whose id is older than this a new one.
const SendKeep = 30 * 24 * time.Hour

// sendPruneEvery bounds how often reserve prunes on its way past.
const sendPruneEvery = 24 * time.Hour

// Send outcomes as recorded. A reservation with none is outcome unknown:
// the server stopped between reserving and recording, and Gmail may or
// may not have the message.
const (
	sendAccepted = "accepted" // Gmail took it and the local copy was stored
	sendNoCopy   = "accepted-no-local-copy"
	sendRejected = "rejected" // provably never reached Gmail: the id is free again
	sendUnknown  = "unknown"  // gmi ran and failed without proving either way
)

// sendRecord is one draft id's file in the send log.
type sendRecord struct {
	ID       string    `json:"id"` // the Message-ID, which is the draft id
	Account  string    `json:"account"`
	Hash     string    `json:"hash"` // outgoing.hash
	Reserved time.Time `json:"reserved"`
	// Submitted is the last submit of this id, first or replayed: the one
	// clock retention runs on, as compose.js's idAt does.
	Submitted time.Time `json:"submitted"`
	Result    string    `json:"result,omitempty"`
	Dest      string    `json:"dest,omitempty"` // sendAccepted: where the send redirected
	Done      time.Time `json:"done,omitzero"`
}

// SendLog makes sends idempotent across restarts (docs/client.md N7): a
// reservation is on disk before gmi is handed the message, and the result
// replaces it after. Records live one per draft id in a 0700 directory,
// each written whole (temp, fsync, rename, fsync the directory).
type SendLog struct {
	dir string
	now func() time.Time

	mu        sync.Mutex
	inFlight  map[string]bool // draft ids between reserve and record: the per-draft lock
	lastPrune time.Time
	// files serializes every read-decide-write on the log's files, so
	// Prune can't remove a record reserve has just replaced.
	files sync.Mutex
	// pinned are replays whose refresh write failed, by id, with the
	// submit time to write: Prune keeps them and retries the write.
	pinned map[string]time.Time
	// beforeRemove runs between Prune's check and its removal; writeErr
	// fails write before it touches the disk, syncErr after the rename.
	// Tests only.
	beforeRemove func(path string)
	writeErr     func(rec sendRecord) error
	syncErr      func() error
}

// SendsPath is $XDG_STATE_HOME/pneu/sends, default ~/.local/state/pneu/sends.
func SendsPath() (string, error) {
	dir, err := stateDir()
	return filepath.Join(dir, "sends"), err
}

// OpenSendLog opens (creating) the log at dir and prunes it.
func OpenSendLog(dir string) (*SendLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	l := &SendLog{dir: dir, now: time.Now, inFlight: map[string]bool{}, pinned: map[string]time.Time{}}
	l.Prune()
	return l, nil
}

var errInFlight = errors.New("send in flight")

// reserve takes the draft id's lock and returns its record, if any. With
// no record, or a rejected one, it writes a reservation for (account, hash)
// first and fresh is true: the caller may send, and must end with record.
// Otherwise it refreshes the record's Submitted, and the caller answers
// from the record and must release. The error is errInFlight when another
// request holds the id.
func (l *SendLog) reserve(id, account, hash string) (rec sendRecord, fresh bool, err error) {
	l.mu.Lock()
	if l.inFlight[id] {
		l.mu.Unlock()
		return rec, false, errInFlight
	}
	l.inFlight[id] = true
	prune := l.now().Sub(l.lastPrune) > sendPruneEvery
	l.mu.Unlock()
	if prune {
		l.Prune()
	}

	l.files.Lock()
	defer l.files.Unlock()
	now := l.now().UTC()
	old, found, err := l.read(id)
	if err != nil {
		l.release(id)
		return rec, false, err
	}
	if found && old.Result != sendRejected {
		old.Submitted = now
		err := l.write(old)
		l.mu.Lock()
		_, was := l.pinned[id]
		if err != nil {
			// Still answered from the record; pinned so Prune keeps it
			// and retries the refresh (a crash first leaves the old date).
			l.pinned[id] = now
		} else {
			delete(l.pinned, id)
		}
		l.mu.Unlock()
		if err != nil && !was {
			log.Printf("send log %s: refresh: %v", id, err)
		}
		return old, false, nil
	}
	rec = sendRecord{ID: id, Account: account, Hash: hash, Reserved: now, Submitted: now}
	if err := l.write(rec); err != nil {
		l.release(id)
		return rec, false, err
	}
	return rec, true, nil
}

// record writes rec's result over its reservation and releases the id.
func (l *SendLog) record(rec sendRecord, result, dest string) {
	defer l.release(rec.ID)
	l.files.Lock()
	defer l.files.Unlock()
	rec.Result, rec.Dest, rec.Done = result, dest, l.now().UTC()
	if err := l.write(rec); err != nil {
		// The reservation stands: the id now reads as unknown, which errs
		// toward never sending twice.
		log.Printf("send log %s: %v", rec.ID, err)
	}
}

func (l *SendLog) release(id string) {
	l.mu.Lock()
	delete(l.inFlight, id)
	l.mu.Unlock()
}

func (l *SendLog) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(l.dir, hex.EncodeToString(sum[:])+".json")
}

// read loads id's record. A file that doesn't parse, or names another id,
// is an error: the draft isn't sent on a record nobody can read.
func (l *SendLog) read(id string) (sendRecord, bool, error) {
	var rec sendRecord
	b, err := os.ReadFile(l.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil || rec.ID != id {
		return rec, false, fmt.Errorf("send log %s: unreadable record", l.path(id))
	}
	return rec, true, nil
}

// write replaces rec's file durably: once it returns, a crash leaves the
// new record or (had it failed) the old one, never a torn one.
func (l *SendLog) write(rec sendRecord) error {
	if l.writeErr != nil {
		if err := l.writeErr(rec); err != nil {
			return err
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	final := l.path(rec.ID)
	f, err := os.CreateTemp(l.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op once renamed
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), final); err != nil {
		return err
	}
	if l.syncErr != nil {
		if err := l.syncErr(); err != nil {
			return err
		}
	}
	d, err := os.Open(l.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Prune drops records last submitted more than SendKeep ago (one that
// doesn't parse, by its file's time), never one in flight, and temp files
// an interrupted write left an hour or more ago. Each check and removal
// holds the files lock, so a record replaced meanwhile is never removed.
func (l *SendLog) Prune() {
	now := l.now()
	l.mu.Lock()
	l.lastPrune = now
	l.mu.Unlock()
	l.retryPinned()
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		log.Printf("send log prune: %v", err)
		return
	}
	for _, e := range entries {
		l.pruneOne(e.Name(), now)
	}
}

// retryPinned writes the refreshes that failed, unpinning each that lands.
func (l *SendLog) retryPinned() {
	l.mu.Lock()
	ids := maps.Clone(l.pinned)
	l.mu.Unlock()
	for id, at := range ids {
		l.files.Lock()
		// Always rewrite: a failure after the rename (the directory sync)
		// leaves the new time visible but not durable, so reading it back
		// proves nothing.
		rec, found, err := l.read(id)
		if err == nil && found {
			if rec.Submitted.Before(at) {
				rec.Submitted = at.UTC()
			}
			err = l.write(rec)
		}
		l.files.Unlock()
		if err == nil {
			l.mu.Lock()
			if l.pinned[id].Equal(at) {
				delete(l.pinned, id)
			}
			l.mu.Unlock()
		}
	}
}

func (l *SendLog) pruneOne(name string, now time.Time) {
	l.files.Lock()
	defer l.files.Unlock()
	p := filepath.Join(l.dir, name)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	switch {
	case strings.HasPrefix(name, ".tmp-"):
		if now.Sub(fi.ModTime()) < time.Hour {
			return
		}
	case strings.HasSuffix(name, ".json"):
		at := fi.ModTime()
		var rec sendRecord
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &rec) == nil {
			l.mu.Lock()
			_, pinned := l.pinned[rec.ID]
			busy := l.inFlight[rec.ID] || pinned
			l.mu.Unlock()
			if busy {
				return
			}
			switch {
			case !rec.Submitted.IsZero():
				at = rec.Submitted
			case !rec.Reserved.IsZero():
				at = rec.Reserved
			}
		}
		if now.Sub(at) <= SendKeep {
			return
		}
	default:
		return
	}
	if l.beforeRemove != nil {
		l.beforeRemove(p)
	}
	os.Remove(p)
}
