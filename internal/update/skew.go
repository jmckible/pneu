// Package update is version skew and `pneu update` (docs/client.md,
// "Versions and updates"; R15, R16, N13).
//
// Skew: two builds are the same only when their vcs.revision is equal and
// neither was built from a modified tree. Which side is older is never
// guessed from a timestamp (vcs.time is never read, R16): only `git
// merge-base --is-ancestor` in this machine's recorded checkout, after a
// fetch, says it, and only `pneu update` runs git. The daemon reads the
// answer from a small cache file (Cache) that `pneu update --check` and
// `pneu update` write, keyed by the exact pair of revisions, so a stale
// answer can never apply to another pair. Anything it can't answer is
// "different": the nudge then says the builds differ and claims nothing
// about which is older.
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Skew states: status.json's server.update.state, the page's link.update,
// and the control socket's situation `update`. Local enums only.
const (
	ClientOlder = "client-older" // this machine is behind the server
	ServerOlder = "server-older" // the server is behind this machine
	Different   = "different"    // not the same build; which is older unknown
)

// Relations a check can find between this machine's build (a) and the
// server's (b), as the cache records them.
const (
	RelClientOlder = "client-older" // a is an ancestor of b
	RelServerOlder = "server-older" // b is an ancestor of a
	RelDiverged    = "diverged"     // neither
	RelUnknown     = "unknown"      // a commit isn't in the checkout, or git failed
)

var revisionRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidRevision: 40 lowercase hex digits, the only revision shape any of
// this trusts.
func ValidRevision(r string) bool { return revisionRE.MatchString(r) }

// Shown is r if it's a valid revision, else "unknown".
func Shown(r string) string {
	if ValidRevision(r) {
		return r
	}
	return "unknown"
}

// Build is one side's identity: vcs.revision and vcs.modified.
type Build struct {
	Revision string
	Modified bool
}

// View is the nudge as status.json and pages carry it: a state and both
// revisions, each 40 hex or "unknown".
type View struct {
	State  string `json:"state"`
	Client string `json:"client"`
	Server string `json:"server"`
}

// Skew is the nudge for this machine's build against the server's: nil
// when they're the same build. Ancestry comes only from c, and only for
// exactly this pair of clean builds.
func Skew(client, server Build, c *Cache) *View {
	cr, sr := client.Revision, server.Revision
	if ValidRevision(cr) && cr == sr && !client.Modified && !server.Modified {
		return nil
	}
	v := &View{State: Different, Client: Shown(cr), Server: Shown(sr)}
	if client.Modified || server.Modified || !ValidRevision(cr) || !ValidRevision(sr) || cr == sr {
		return v
	}
	switch c.Lookup(cr, sr) {
	case RelClientOlder:
		v.State = ClientOlder
	case RelServerOlder:
		v.State = ServerOlder
	}
	return v
}

// ValidState reports whether s is one of the three skew states.
func ValidState(s string) bool { return s == ClientOlder || s == ServerOlder || s == Different }

// Cache is what checks found, newest first, at most MaxPairs.
type Cache struct {
	Pairs []Pair `json:"pairs"`
}

// Pair is one check: the relation of client to server, by revision.
type Pair struct {
	Client   string `json:"client"`
	Server   string `json:"server"`
	Relation string `json:"relation"`
	Checked  string `json:"checked"` // when, RFC 3339; for people, never compared
}

// MaxPairs bounds the cache: a check adds one pair, and an update the
// pair it's about to make.
const MaxPairs = 8

// maxCache bounds the file read.
const maxCache = 8 << 10

// CacheFile is the cache's name in the state dir.
const CacheFile = "skew.json"

// Lookup is the recorded relation of client to server, or "".
func (c *Cache) Lookup(client, server string) string {
	if c == nil {
		return ""
	}
	for _, p := range c.Pairs {
		if p.Client == client && p.Server == server {
			return p.Relation
		}
	}
	return ""
}

func validRelation(r string) bool {
	return r == RelClientOlder || r == RelServerOlder || r == RelDiverged || r == RelUnknown
}

// Add records a check, replacing an older one for the same pair.
func (c *Cache) Add(p Pair) {
	out := []Pair{p}
	for _, q := range c.Pairs {
		if (q.Client != p.Client || q.Server != p.Server) && len(out) < MaxPairs {
			out = append(out, q)
		}
	}
	c.Pairs = out
}

// LoadCache reads the cache at path: nil, nil when there is none. It must
// be a regular 0600 file of ours (O_NOFOLLOW), within maxCache, every pair
// two valid revisions and a known relation, parsed by token (strictObject);
// anything else is an error and the caller uses no cache.
func LoadCache(path string) (*Cache, error) {
	b, err := readPrivate(path, maxCache)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	if err := strictObject(b, map[string]any{"pairs": &raws}); err != nil {
		return nil, fmt.Errorf("update: %s: %w", path, err)
	}
	if len(raws) > MaxPairs {
		return nil, fmt.Errorf("update: %s: more than %d pairs", path, MaxPairs)
	}
	var c Cache
	for _, raw := range raws {
		var p Pair
		if err := strictObject(raw, map[string]any{"client": &p.Client, "server": &p.Server, "relation": &p.Relation, "checked": &p.Checked}); err != nil {
			return nil, fmt.Errorf("update: %s: a pair: %w", path, err)
		}
		if !ValidRevision(p.Client) || !ValidRevision(p.Server) || !validRelation(p.Relation) {
			return nil, fmt.Errorf("update: %s: a pair out of shape", path)
		}
		c.Pairs = append(c.Pairs, p)
	}
	return &c, nil
}

// SaveCache adds p to the cache at path (a missing or unreadable one
// starts afresh) and writes it back.
func SaveCache(path string, p Pair, now time.Time) error {
	if !ValidRevision(p.Client) || !ValidRevision(p.Server) || !validRelation(p.Relation) {
		return errors.New("update: not a pair to record")
	}
	c, err := LoadCache(path)
	if err != nil || c == nil {
		c = &Cache{}
	}
	p.Checked = now.UTC().Format(time.RFC3339)
	c.Add(p)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(b, '\n'))
}

// readPrivate reads a regular file of ours, mode exactly 0600, without
// following a symlink, up to max bytes.
func readPrivate(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("update: %s isn't a regular file", path)
	case !ok || int(st.Uid) != os.Getuid():
		return nil, fmt.Errorf("update: %s isn't ours", path)
	case fi.Mode().Perm() != 0o600:
		return nil, fmt.Errorf("update: %s has mode %v, want 0600", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("update: %s is too large", path)
	}
	return b, nil
}

// writePrivate replaces path with b: temp file, fsync, rename, fsync dir;
// 0600.
func writePrivate(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
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
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
