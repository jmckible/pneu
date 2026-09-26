package main

// The local repository: lieer's config, state and resume files, its lock,
// its maildir cache and its notmuch database, after lieer/local.py.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	configFile      = ".gmailieer.json"
	stateFile       = ".state.gmailieer.json"
	credentialsFile = ".credentials.gmailieer.json"
	resumeFile      = ".resume-pull.gmailieer.json"
	// stubFile is the simulator's own state (which one-shot failures have
	// fired). Its name matches INSTALL.md's new.ignore regex.
	stubFile = ".stubgmi.json"
)

// defaultIgnoreRemote is remote.py:Remote.DEFAULT_IGNORE_LABELS.
var defaultIgnoreRemote = []string{"CATEGORY_PERSONAL", "CATEGORY_SOCIAL", "CATEGORY_PROMOTIONS", "CATEGORY_UPDATES", "CATEGORY_FORUMS"}

// localIgnore is local.py:Local.ignore_labels: local tags lieer never
// pushes and never clears.
var localIgnore = []string{"archive", "arxiv", "attachment", "encrypted", "signed", "passed", "replied", "muted", "mute", "todo", "Trash", "voicemail"}

// config is local.py:Local.Config.
type config struct {
	ReplaceSlashWithDot   bool
	Account               string
	Timeout               numLit
	DropNonExistingLabel  bool
	IgnoreEmptyHistory    bool
	IgnoreTags            []string
	IgnoreRemoteLabels    []string
	RemoveLocalMessages   bool
	FileExtension         string
	LocalTrashTag         string
	TranslationListOverly []string
}

// loadConfig reads .gmailieer.json with Local.Config.__init__'s defaults.
func loadConfig() (*config, error) {
	c := &config{Account: "me", Timeout: "600", IgnoreRemoteLabels: defaultIgnoreRemote,
		RemoveLocalMessages: true, LocalTrashTag: "trash", IgnoreTags: []string{}, TranslationListOverly: []string{}}
	b, err := os.ReadFile(configFile)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var j map[string]json.RawMessage
	if err := json.Unmarshal(b, &j); err != nil {
		fmt.Printf("Failed to decode config file `%s`.\n", configFile)
		return nil, raise("json.decoder.JSONDecodeError: "+err.Error(),
			lieerFrame("local.py", 127, "__init__", "self.json = json.load(fd)"))
	}
	get := func(k string, dst any) {
		if v, ok := j[k]; ok {
			json.Unmarshal(v, dst)
		}
	}
	get("replace_slash_with_dot", &c.ReplaceSlashWithDot)
	get("account", &c.Account)
	if v, ok := j["timeout"]; ok {
		c.Timeout = numLit(v)
	}
	get("drop_non_existing_label", &c.DropNonExistingLabel)
	get("ignore_empty_history", &c.IgnoreEmptyHistory)
	get("ignore_tags", &c.IgnoreTags)
	get("ignore_remote_labels", &c.IgnoreRemoteLabels)
	get("remove_local_messages", &c.RemoveLocalMessages)
	get("file_extension", &c.FileExtension)
	get("local_trash_tag", &c.LocalTrashTag)
	get("translation_list_overlay", &c.TranslationListOverly)
	return c, nil
}

// write is Local.Config.write: key order and formatting are json.dump's.
func (c *config) write() error {
	return writePy(configFile, []kv{
		{"replace_slash_with_dot", c.ReplaceSlashWithDot},
		{"account", c.Account},
		{"timeout", c.Timeout},
		{"drop_non_existing_label", c.DropNonExistingLabel},
		{"ignore_empty_history", c.IgnoreEmptyHistory},
		{"ignore_tags", orEmpty(c.IgnoreTags)},
		{"ignore_remote_labels", orEmpty(c.IgnoreRemoteLabels)},
		{"remove_local_messages", c.RemoveLocalMessages},
		{"file_extension", c.FileExtension},
		{"local_trash_tag", c.LocalTrashTag},
		{"translation_list_overlay", orEmpty(c.TranslationListOverly)},
	}, true)
}

func orEmpty(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// writePy writes like Config.write and State.write: shutil.copyfile the old
// file to .bak (mode from the umask), json.dump into a NamedTemporaryFile in
// the same directory (mode 0600), rename over. ResumePull.save renames the
// old file to .bak instead of copying it (bak=false).
func writePy(name string, v any, copyBak bool) error {
	if old, err := os.ReadFile(name); err == nil {
		if copyBak {
			if err := os.WriteFile(name+".bak", old, 0o666); err != nil {
				return err
			}
		}
	}
	f, err := os.CreateTemp(".", "tmp")
	if err != nil {
		return err
	}
	if _, err := f.WriteString(pyJSON(v)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !copyBak {
		if _, err := os.Stat(name); err == nil {
			if err := os.Rename(name, name+".bak"); err != nil {
				return err
			}
		}
	}
	return os.Rename(f.Name(), name)
}

// state is local.py:Local.State.
type state struct {
	LastHistoryID int64 `json:"last_historyId"`
	Lastmod       int64 `json:"lastmod"`
}

func loadState() (*state, error) {
	s := &state{}
	b, err := os.ReadFile(stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		fmt.Printf("Failed to decode state file `%s`.\n", stateFile)
		return nil, raise("json.decoder.JSONDecodeError: "+err.Error(),
			lieerFrame("local.py", 271, "__init__", "self.json = json.load(fd)"))
	}
	return s, nil
}

func (s *state) write() error {
	return writePy(stateFile, []kv{{"last_historyId", s.LastHistoryID}, {"lastmod", s.Lastmod}}, true)
}

// resumePull is lieer/resume.py:ResumePull.
type resumePull struct {
	LastID      int64    `json:"lastId"`
	MetaFetched []string `json:"meta_fetched"`
}

func (r *resumePull) save() error {
	return writePy(resumeFile, []kv{{"version", 1}, {"lastId", r.LastID}, {"meta_fetched", orEmpty(r.MetaFetched)}}, false)
}

// loadResume is gmailieer.py:Gmailieer.load_resume: the existing file, or a
// new one saved at once.
func (g *gmi) loadResume(lastID int64) (*resumePull, error) {
	if b, err := os.ReadFile(resumeFile); err == nil {
		var r struct {
			Version int `json:"version"`
			resumePull
		}
		if err := json.Unmarshal(b, &r); err == nil && r.Version == 1 {
			return &r.resumePull, nil
		} else if err == nil {
			fmt.Printf("error: mismatching version in resume file: %d != 1\n", r.Version)
		}
		g.vprint("failed to load resume file, creating new: ")
	}
	r := &resumePull{LastID: lastID}
	return r, r.save()
}

func (r *resumePull) update(gids []string) error {
	r.MetaFetched = append(r.MetaFetched, gids...)
	slices.Sort(r.MetaFetched)
	r.MetaFetched = slices.Compact(r.MetaFetched)
	return r.save()
}

// local is the loaded repository (Local.load_repository).
type local struct {
	cfg        *config
	st         *state
	md         string // <wd>/mail
	nmRelative string // md relative to the database path
	newTags    []string
	gids       map[string]string // gid -> "cur/<name>" (Local.__load_cache__)
	lock       *os.File
}

// loadRepository is Local.load_repository.
func (g *gmi) loadRepository(block bool) error {
	fr := lieerFrame("gmailieer.py", 488, "setup", "self.local.load_repository(block)")
	if _, err := os.Stat(configFile); err != nil {
		return raise("lieer.local.Local.RepositoryException: local repository not initialized: could not find config file", fr,
			lieerFrame("local.py", 345, "load_repository", "raise Local.RepositoryException("))
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	l := &local{md: filepath.Join(wd, "mail")}
	for _, d := range []string{"cur", "new", "tmp"} {
		if _, err := os.Stat(filepath.Join(l.md, d)); err != nil {
			return raise("lieer.local.Local.RepositoryException: local repository not initialized: could not find mail dir structure", fr,
				lieerFrame("local.py", 355, "load_repository", "raise Local.RepositoryException("))
		}
	}
	// notmuch2.Database(): the database must exist.
	if _, err := notmuchRevision(); err != nil {
		return raise("notmuch2.FileError: "+err.Error(), fr,
			lieerFrame("local.py", 360, "load_repository", "with notmuch2.Database() as db:"))
	}
	dbPath, err := notmuchOut("config", "get", "database.path")
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(strings.TrimSpace(dbPath), l.md)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return raise("lieer.local.Local.RepositoryException: local mail repository not in notmuch db", fr,
			lieerFrame("local.py", 364, "load_repository", "raise Local.RepositoryException("))
	}
	l.nmRelative = rel
	// fcntl.lockf on .lock, LOCK_NB unless block.
	f, err := os.OpenFile(".lock", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	cmd := syscall.F_SETLK
	if block {
		cmd = syscall.F_SETLKW
	}
	if err := syscall.FcntlFlock(f.Fd(), cmd, &syscall.Flock_t{Type: syscall.F_WRLCK}); err != nil {
		f.Close()
		return raise("lieer.local.Local.RepositoryException: failed to lock repository (probably in use by another gmi instance)", fr,
			lieerFrame("local.py", 377, "load_repository", "raise Local.RepositoryException("))
	}
	l.lock = f
	if l.cfg, err = loadConfig(); err != nil {
		return err
	}
	if l.st, err = loadState(); err != nil {
		return err
	}
	nt, _ := notmuchOut("config", "get", "new.tags")
	for _, t := range strings.FieldsFunc(nt, func(r rune) bool { return r == '\n' || r == ';' }) {
		if t = strings.TrimSpace(t); t != "" {
			l.newTags = append(l.newTags, t)
		}
	}
	if err := l.loadCache(); err != nil {
		return err
	}
	g.local = l
	return nil
}

// loadCache is Local.__load_cache__: the gids lieer holds a file for,
// from cur/ and new/ (not tmp/).
func (l *local) loadCache() error {
	l.gids = map[string]string{}
	for _, d := range []string{"cur", "new"} {
		ents, err := os.ReadDir(filepath.Join(l.md, d))
		if err != nil {
			return err
		}
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if gid := l.filenameToGID(e.Name()); gid != "" {
				l.gids[gid] = d + "/" + e.Name()
			}
		}
	}
	return nil
}

// filenameToGID is Local.__filename_to_gid__.
func (l *local) filenameToGID(name string) string {
	ext := ""
	if l.cfg.FileExtension != "" {
		ext = "." + l.cfg.FileExtension
	}
	ext += ":2,"
	if i := strings.LastIndex(name, ext); i > 5 {
		return name[:i]
	}
	fmt.Printf("'%s' does not contain valid maildir delimiter, correct file name extension, or does not seem to have a valid GID, ignoring.\n", name)
	return ""
}

func (l *local) has(gid string) bool { _, ok := l.gids[gid]; return ok }

// ignored is Local.ignore_labels plus the configured ignore_tags.
func (l *local) ignored(t string) bool {
	return slices.Contains(localIgnore, t) || slices.Contains(l.cfg.IgnoreTags, t)
}

// store is Local.store for one message: tmp/ then rename into cur/, the
// file's mtime set to Gmail's internalDate. The caller indexes the batch.
func (l *local) store(m *rmsg, raw []byte) error {
	ext := ""
	if l.cfg.FileExtension != "" {
		ext = "." + l.cfg.FileExtension
	}
	bname := m.gid + ext + ":" + maildirFlags(m.tags)
	p := filepath.Join(l.md, "cur", bname)
	tmp := filepath.Join(l.md, "tmp", bname)
	fr := lieerFrame("local.py", 591, "store", "")
	if _, err := os.Stat(p); err == nil {
		fr.line, fr.src = 601, "raise Local.RepositoryException(\"local file already exists: %s\" % p)"
		return raise("lieer.local.Local.RepositoryException: local file already exists: "+p, contentFrames(fr)...)
	}
	if _, err := os.Stat(tmp); err == nil {
		fr.line, fr.src = 604, "raise Local.RepositoryException("
		return raise("lieer.local.Local.RepositoryException: local temporary file already exists: "+tmp, contentFrames(fr)...)
	}
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if err := os.WriteFile(tmp, raw, 0o666); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, m.date, m.date); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	l.gids[m.gid] = "cur/" + bname
	return nil
}

func contentFrames(last frame) []frame {
	return []frame{
		lieerFrame("gmailieer.py", 892, "full_pull", "updated = self.get_content(message_gids)"),
		lieerFrame("gmailieer.py", 993, "get_content", "self.remote.get_messages(need_content, _got_msgs, \"raw\")"),
		lieerFrame("remote.py", 460, "get_messages", "cb(msg_batch)"),
		lieerFrame("gmailieer.py", 991, "_got_msgs", "self.local.store(m, db)"),
		last,
	}
}

// index adds the batch's new files to the database and gives each its
// labels plus new.tags, as Local.update_tags does for a message not yet in
// the database. lieer calls db.add per file; `notmuch new` is the CLI's way
// to add, and ignores tmp/ as lieer's cache does.
func (l *local) index(ms []*rmsg) error {
	if _, err := notmuchOut("new", "--quiet"); err != nil {
		return err
	}
	var batch bytes.Buffer
	for _, m := range ms {
		tags := append(slices.Clone(m.tags), l.newTags...)
		if len(tags) == 0 {
			continue
		}
		for _, t := range tags {
			if l.cfg.ReplaceSlashWithDot {
				t = strings.ReplaceAll(t, "/", ".")
			}
			batch.WriteString("+" + encodeTerm(t) + " ")
		}
		batch.WriteString("-- " + idTerm(m.messageID()) + "\n")
	}
	if batch.Len() == 0 {
		return nil
	}
	_, err := notmuchIn(batch.Bytes(), "tag", "--batch")
	return err
}

// updateTags is Local.update_tags for messages already in the database: set
// the local tags to the remote labels, keeping ignored tags, and nothing
// else. This is the step that reverts an unpushed local change.
func (l *local) updateTags(ms []*rmsg) error {
	if len(ms) == 0 {
		return nil
	}
	q := make([]string, 0, len(ms)*2)
	want := map[string][]string{}
	for i, m := range ms {
		if i > 0 {
			q = append(q, "or")
		}
		q = append(q, "id:"+quoteID(m.messageID()))
		want[m.messageID()] = m.tags
	}
	out, err := notmuchOut(append([]string{"dump", "--format=batch-tag", "--"}, q...)...)
	if err != nil {
		return err
	}
	var batch bytes.Buffer
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		tagPart, idPart, ok := strings.Cut(line, " -- id:")
		if !ok {
			if strings.HasPrefix(line, "-- id:") {
				tagPart, idPart, ok = "", strings.TrimPrefix(line, "-- id:"), true
			} else {
				continue
			}
		}
		id := decodeTerm(idPart)
		labels, found := want[id]
		if !found {
			continue
		}
		seen[id] = true
		have := map[string]bool{}
		for _, t := range strings.Fields(tagPart) {
			have[decodeTerm(strings.TrimPrefix(t, "+"))] = true
		}
		wantSet := map[string]bool{}
		for _, t := range labels {
			wantSet[t] = true
		}
		var ops []string
		for t := range have {
			if !wantSet[t] && !l.ignored(t) {
				ops = append(ops, "-"+encodeTerm(t))
			}
		}
		for t := range wantSet {
			if !have[t] {
				ops = append(ops, "+"+encodeTerm(t))
			}
		}
		if len(ops) == 0 {
			continue
		}
		sort.Strings(ops)
		batch.WriteString(strings.Join(ops, " ") + " -- " + idTerm(id) + "\n")
	}
	for id, labels := range want {
		if !seen[id] && len(labels) > 0 { // untagged messages may be missing from the dump
			ops := make([]string, len(labels))
			for i, t := range labels {
				ops[i] = "+" + encodeTerm(t)
			}
			batch.WriteString(strings.Join(ops, " ") + " -- " + idTerm(id) + "\n")
		}
	}
	if batch.Len() == 0 {
		return nil
	}
	_, err = notmuchIn(batch.Bytes(), "tag", "--batch")
	return err
}

// encodeTerm is notmuch's batch-tag hex encoding: every byte outside
// [A-Za-z0-9@=.,_+-] becomes %nn (notmuch-dump(1)).
func encodeTerm(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("@=.,_+-", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02x", c)
		}
	}
	return b.String()
}

func decodeTerm(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func quoteID(id string) string { return `"` + strings.ReplaceAll(id, `"`, `""`) + `"` }

// idTerm is an id: query term for a batch line: quoted, then hex-encoded
// inside the quotes (the batch decodes before the query parser runs).
func idTerm(id string) string {
	return `id:"` + encodeTerm(strings.ReplaceAll(id, `"`, `""`)) + `"`
}

// notmuchRevision is db.revision().rev, from `notmuch count --lastmod`.
func notmuchRevision() (int64, error) {
	out, err := notmuchOut("count", "--lastmod", "*")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) != 3 {
		return 0, fmt.Errorf("notmuch count --lastmod: %q", out)
	}
	return strconv.ParseInt(f[2], 10, 64)
}

func notmuchOut(args ...string) (string, error) { return notmuchIn(nil, args...) }

// notmuchIn runs notmuch against $NOTMUCH_CONFIG. Errors carry notmuch's
// own first stderr line, which is what notmuch2 would raise with.
func notmuchIn(stdin []byte, args ...string) (string, error) {
	for attempt := 0; ; attempt++ {
		cmd := exec.Command("notmuch", args...)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil {
			return string(out), nil
		}
		msg := strings.TrimSpace(stderr.String())
		// Another writer (pneu tagging): lieer's notmuch2 open blocks under
		// retry_lock; a CLI build without it fails fast, so wait here.
		if strings.Contains(msg, "already locked") && attempt < 300 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
}
