// Package testmail materializes the embedded mail fixture as two lieer-shaped
// maildirs, each with its own notmuch database and config, tagged from the
// fixture manifest the way lieer would have tagged them.
package testmail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jmckible/pneu/testdata"
)

// Account is one fixture account: its own maildir root and notmuch database.
type Account struct {
	Name          string // "personal" or "work"
	Email         string // user.primary_email
	NotmuchConfig string // path for NOTMUCH_CONFIG
	Root          string // database.path; lieer's dir is Root/gmail, messages in Root/gmail/mail/cur
}

// Environ returns the process environment with NOTMUCH_CONFIG pointing at
// this account's database.
func (a Account) Environ() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NOTMUCH_") {
			env = append(env, kv)
		}
	}
	return append(env, "NOTMUCH_CONFIG="+a.NotmuchConfig)
}

// Env is a materialized fixture.
type Env struct {
	Accounts []Account
}

// Account returns the named account or fails the test.
func (e Env) Account(t testing.TB, name string) Account {
	t.Helper()
	for _, a := range e.Accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("testmail: no account %q", name)
	return Account{}
}

var fixtureAccounts = []struct{ name, email string }{
	{"personal", "robin@hale.example"},
	{"work", "robin@northwind.example"},
}

// Manifest is tags.json: account -> message-id -> tags lieer applied.
type Manifest map[string]map[string][]string

// LoadManifest reads the embedded tag manifest.
func LoadManifest() (Manifest, error) {
	b, err := testdata.FS.ReadFile("mail/tags.json")
	if err != nil {
		return nil, err
	}
	var m Manifest
	return m, json.Unmarshal(b, &m)
}

// Setup copies the fixture into t.TempDir(), writes one notmuch config per
// account, indexes each with `notmuch new`, and applies the tag manifest.
// It skips the test when notmuch is not installed.
func Setup(t testing.TB) Env {
	t.Helper()
	if _, err := exec.LookPath("notmuch"); err != nil {
		t.Skip("testmail: notmuch not on PATH")
	}
	manifest, err := LoadManifest()
	if err != nil {
		t.Fatalf("testmail: manifest: %v", err)
	}
	base := t.TempDir()
	var env Env
	for _, fa := range fixtureAccounts {
		a := Account{
			Name:          fa.name,
			Email:         fa.email,
			Root:          filepath.Join(base, "mail", fa.name),
			NotmuchConfig: filepath.Join(base, "config", fa.name, "notmuch-config"),
		}
		if err := materialize(a); err != nil {
			t.Fatalf("testmail: %s: %v", a.Name, err)
		}
		if err := writeConfig(a, otherEmail(fa.email)); err != nil {
			t.Fatalf("testmail: %s: config: %v", a.Name, err)
		}
		if out, err := run(a, nil, "new", "--quiet"); err != nil {
			t.Fatalf("testmail: %s: notmuch new: %v\n%s", a.Name, err, out)
		}
		if batch := tagBatch(manifest[a.Name]); len(batch) > 0 {
			if out, err := run(a, batch, "tag", "--batch"); err != nil {
				t.Fatalf("testmail: %s: notmuch tag --batch: %v\n%s", a.Name, err, out)
			}
		}
		env.Accounts = append(env.Accounts, a)
	}
	return env
}

func otherEmail(email string) string {
	for _, fa := range fixtureAccounts {
		if fa.email != email {
			return fa.email
		}
	}
	return ""
}

// materialize copies mail/<account> into a.Root, restoring lieer's ':2,'
// suffix, and creates the maildir's new/ and tmp/ (git can't hold empty dirs).
func materialize(a Account) error {
	src := path.Join("mail", a.Name)
	err := fs.WalkDir(testdata.FS, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		dst := filepath.Join(a.Root, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := testdata.FS.ReadFile(p)
		if err != nil {
			return err
		}
		dst = filepath.Join(filepath.Dir(dst), strings.Replace(d.Name(), "!2,", ":2,", 1))
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return err
	}
	for _, sub := range []string{"new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(a.Root, "gmail", "mail", sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// writeConfig mirrors the production config described in INSTALL.md.
func writeConfig(a Account, other string) error {
	cfg := fmt.Sprintf(`[database]
path=%s

[user]
name=Robin Hale
primary_email=%s
other_email=%s

[new]
tags=
ignore=.gmailieer.json;.state.gmailieer.json;.credentials.gmailieer.json;.resume-pull.gmailieer.json;.lock;/.*[.](json|lock|bak)$/

[search]
exclude_tags=spam;trash

[maildir]
synchronize_flags=false
`, a.Root, a.Email, other)
	if err := os.MkdirAll(filepath.Dir(a.NotmuchConfig), 0o755); err != nil {
		return err
	}
	return os.WriteFile(a.NotmuchConfig, []byte(cfg), 0o644)
}

// tagBatch renders the manifest in `notmuch tag --batch` format. Tags only
// add: notmuch's own index-time tags (attachment) survive, as they do under lieer.
func tagBatch(tags map[string][]string) []byte {
	ids := make([]string, 0, len(tags))
	for id := range tags {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b bytes.Buffer
	for _, id := range ids {
		if len(tags[id]) == 0 {
			continue
		}
		for _, tag := range tags[id] {
			b.WriteString("+" + encodeTag(tag) + " ")
		}
		b.WriteString("-- " + QuoteID(id) + "\n")
	}
	return b.Bytes()
}

// QuoteID returns an id: query term quoted per Xapian boolean-term rules.
func QuoteID(id string) string {
	return `id:"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

func encodeTag(tag string) string {
	var b strings.Builder
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.:/@", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02x", c)
		}
	}
	return b.String()
}

func run(a Account, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("notmuch", args...)
	cmd.Env = a.Environ()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

// Notmuch runs notmuch against the account's database and returns stdout,
// failing the test on a non-zero exit.
func (a Account) Notmuch(t testing.TB, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("notmuch", args...)
	cmd.Env = a.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("notmuch %s (%s): %v\n%s", strings.Join(args, " "), a.Name, err, stderr.String())
	}
	return out
}
