package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var granted = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func store(t *testing.T) Store {
	t.Helper()
	return Store{Dir: Dir(filepath.Join(t.TempDir(), "pneu"))}
}

// initFile is what push init commits.
func initFile(f *File) error {
	f.Project = "pneu-push-123"
	f.Install = "0123456789abcdef"
	f.Client = "123456789-abc.apps.googleusercontent.com"
	f.Owner = Owner{Sub: "1234567890", Email: "o@example.com", Refresh: "1//owner-refresh", Granted: granted}
	return nil
}

func commit(t *testing.T, s Store, modify func(*File) error) Snapshot {
	t.Helper()
	l, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	snap, err := l.Update(modify)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestRoundTrip(t *testing.T) {
	s := store(t)
	if _, err := s.Load(); !errors.Is(err, ErrNoState) {
		t.Fatalf("empty: %v", err)
	}
	first := commit(t, s, initFile)
	if first.Generation != 1 || len(first.Accounts) != 0 {
		t.Fatalf("%+v", first)
	}
	second := commit(t, s, func(f *File) error {
		f.Accounts["personal"] = Account{State: On, Address: "j@example.com", Refresh: "1//p", Granted: granted}
		f.Accounts["vocal"] = Account{State: OffPending, Address: "j@vocal.example", Refresh: "1//v", Granted: granted}
		return nil
	})
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 2 || got.Hash != second.Hash || got.Hash == first.Hash {
		t.Fatalf("gen %d hash %s / %s", got.Generation, got.Hash, second.Hash)
	}
	if got.Accounts["vocal"].State != OffPending || got.Owner.Sub != "1234567890" || !got.Owner.Granted.Equal(granted) {
		t.Fatalf("%+v", got)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, StateFile))
	if Hash(b) != got.Hash {
		t.Fatal("hash isn't over the file's bytes")
	}
	fi, _ := os.Stat(filepath.Join(s.Dir, StateFile))
	di, _ := os.Stat(s.Dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatal("modes", fi.Mode(), di.Mode())
	}
	// A failed modify writes nothing.
	l, _ := s.Lock(context.Background())
	if _, err := l.Update(func(f *File) error { f.Project = "x"; return errors.New("no") }); err == nil {
		t.Fatal("modify error ignored")
	}
	// An invalid result is refused.
	if _, err := l.Update(func(f *File) error { f.Accounts["x y"] = Account{}; return nil }); err == nil {
		t.Fatal("invalid result written")
	}
	l.Unlock()
	if again, _ := s.Load(); again.Hash != second.Hash {
		t.Fatal("a refused update changed the file")
	}
	// Secrets don't print.
	if s := fmt.Sprintf("%v %+v %#v", got, got.File, got.Accounts); strings.Contains(s, "1//") {
		t.Fatalf("refresh token printed: %s", s)
	}
}

func TestTwoAccountsOneAddress(t *testing.T) {
	s := store(t)
	commit(t, s, initFile)
	l, _ := s.Lock(context.Background())
	defer l.Unlock()
	_, err := l.Update(func(f *File) error {
		f.Accounts["a"] = Account{State: On, Address: "j@example.com", Refresh: "1//a", Granted: granted}
		f.Accounts["b"] = Account{State: OffPending, Address: "J@Example.com", Refresh: "1//b", Granted: granted}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "same address") {
		t.Fatal(err)
	}
}

const good = `{"version":1,"generation":7,"project":"pneu-push-123","install":"0123456789abcdef",
 "client":"123456789-abc.apps.googleusercontent.com",
 "owner":{"sub":"1","email":"o@example.com","refresh":"1//o","granted":"2026-10-03T12:00:00Z"},
 "accounts":{"personal":{"state":"on","address":"j@example.com","refresh":"1//p","granted":"2026-10-03T12:00:00Z"}}}`

func TestParse(t *testing.T) {
	f, err := Parse([]byte(good))
	if err != nil || f.Generation != 7 || f.Accounts["personal"].State != On {
		t.Fatal(f, err)
	}
	replace := func(old, new string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("fixture lacks %s", old)
		}
		return strings.Replace(good, old, new, 1)
	}
	for name, bad := range map[string]string{
		"duplicate top key":      replace(`"version":1,`, `"version":1,"version":1,`),
		"duplicate owner key":    replace(`"sub":"1",`, `"sub":"1","sub":"2",`),
		"duplicate account":      replace(`"accounts":{`, `"accounts":{"personal":{"state":"on","address":"x@example.com","refresh":"1//x","granted":"2026-10-03T12:00:00Z"},`),
		"duplicate account key":  replace(`"state":"on",`, `"state":"on","state":"on",`),
		"unknown top key":        replace(`"version":1,`, `"version":1,"extra":1,`),
		"unknown owner key":      replace(`"sub":"1",`, `"sub":"1","name":"x",`),
		"case-folded key":        replace(`"project"`, `"Project"`),
		"missing key":            replace(`"install":"0123456789abcdef",`, ``),
		"missing owner field":    replace(`"refresh":"1//o",`, ``),
		"null":                   replace(`"client":"123456789-abc.apps.googleusercontent.com"`, `"client":null`),
		"trailing data":          good + `{}`,
		"trailing garbage":       good + ` x`,
		"version 2":              replace(`"version":1`, `"version":2`),
		"generation 0":           replace(`"generation":7`, `"generation":0`),
		"generation negative":    replace(`"generation":7`, `"generation":-7`),
		"generation float":       replace(`"generation":7`, `"generation":7.0`),
		"generation string":      replace(`"generation":7`, `"generation":"7"`),
		"generation huge":        replace(`"generation":7`, `"generation":18446744073709551615`),
		"bad project":            replace(`"pneu-push-123"`, `"Pneu"`),
		"bad install":            replace(`"0123456789abcdef"`, `"0123"`),
		"bad client":             replace(`123456789-abc.apps`, `evil.example/abc.apps`),
		"bad sub":                replace(`"sub":"1"`, `"sub":"1 2"`),
		"bad email":              replace(`"o@example.com"`, `"o"`),
		"refresh with newline":   replace(`"1//o"`, `"1//o\n"`),
		"refresh too long":       replace(`"1//o"`, `"`+strings.Repeat("a", 1025)+`"`),
		"bad time":               replace(`"granted":"2026-10-03T12:00:00Z"}`, `"granted":"2026-10-03 12:00:00"}`),
		"non-UTC time":           replace(`"granted":"2026-10-03T12:00:00Z"}`, `"granted":"2026-10-03T14:00:00+02:00"}`),
		"fractional time":        replace(`"granted":"2026-10-03T12:00:00Z"}`, `"granted":"2026-10-03T12:00:00.5Z"}`),
		"bad account name":       replace(`"personal":`, `"../x":`),
		"long account name":      replace(`"personal":`, `"`+strings.Repeat("a", 33)+`":`),
		"bad state":              replace(`"state":"on"`, `"state":"off"`),
		"account not an object":  replace(`"accounts":{"personal":{`, `"accounts":{"personal":[{`) + `]`,
		"accounts not an object": replace(`"accounts":{`, `"accounts":[{`),
		"not an object":          `[]`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// More than MaxAccounts.
	var b strings.Builder
	b.WriteString(strings.TrimSuffix(strings.Split(good, `"accounts":`)[0], " ") + `"accounts":{`)
	for i := range MaxAccounts + 1 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"a%d":{"state":"on","address":"j%d@example.com","refresh":"1//x","granted":"2026-10-03T12:00:00Z"}`, i, i)
	}
	b.WriteString("}}")
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Error("too many accounts accepted")
	}
}

func TestFileChecks(t *testing.T) {
	s := store(t)
	commit(t, s, initFile)
	path := filepath.Join(s.Dir, StateFile)
	orig, _ := os.ReadFile(path)
	restore := func() {
		os.Remove(path)
		os.WriteFile(path, orig, 0o600)
		os.Chmod(path, 0o600)
		os.Chmod(s.Dir, 0o700)
	}
	for name, mess := range map[string]func(){
		"file mode 0644": func() { os.Chmod(path, 0o644) },
		"file mode 0400": func() { os.Chmod(path, 0o400) },
		"dir mode 0755":  func() { os.Chmod(s.Dir, 0o755) },
		"symlinked file": func() {
			other := filepath.Join(t.TempDir(), "elsewhere.json")
			os.WriteFile(other, orig, 0o600)
			os.Remove(path)
			os.Symlink(other, path)
		},
		"directory at the file": func() { os.Remove(path); os.Mkdir(path, 0o700) },
		"fifo at the file":      func() { os.Remove(path); syscall.Mkfifo(path, 0o600) },
		"oversized": func() {
			os.WriteFile(path, append(orig[:len(orig)-2], []byte(strings.Repeat(" ", MaxState)+"}\n")...), 0o600)
		},
	} {
		mess()
		done := make(chan error, 1)
		go func() { _, err := s.Load(); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: loaded", name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Load hung", name)
		}
		restore()
	}
	if _, err := s.Load(); err != nil {
		t.Fatal("restore:", err)
	}

	// A symlinked push directory.
	real := s.Dir
	link := filepath.Join(t.TempDir(), "push")
	os.Symlink(real, link)
	if _, err := (Store{Dir: link}).Load(); err == nil || errors.Is(err, ErrNoState) {
		t.Fatalf("symlinked dir: %v", err)
	}
	if _, err := (Store{Dir: link}).Lock(context.Background()); err == nil {
		t.Fatal("symlinked dir locked")
	}
	// A symlinked or loose lock file.
	lock := filepath.Join(s.Dir, LockFile)
	os.Remove(lock)
	os.Symlink(filepath.Join(t.TempDir(), "x"), lock)
	if _, err := s.Lock(context.Background()); err == nil {
		t.Fatal("symlinked lock taken")
	}
	os.Remove(lock)
	os.WriteFile(lock, nil, 0o644)
	os.Chmod(lock, 0o644)
	if _, err := s.Lock(context.Background()); err == nil {
		t.Fatal("0644 lock taken")
	}
}

func TestLockSerializesWriters(t *testing.T) {
	s := store(t)
	commit(t, s, initFile)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			commit(t, s, func(f *File) error { return nil })
		})
	}
	wg.Wait()
	if snap, _ := s.Load(); snap.Generation != 9 {
		t.Fatalf("generation %d after 8 writers on 1", snap.Generation)
	}

	l, _ := s.Lock(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.Lock(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock: %v", err)
	}
	l.Unlock()
	// The lock file is never replaced by a write.
	before, _ := os.Stat(filepath.Join(s.Dir, LockFile))
	commit(t, s, func(*File) error { return nil })
	after, _ := os.Stat(filepath.Join(s.Dir, LockFile))
	if !os.SameFile(before, after) {
		t.Fatal("push.lock replaced")
	}
}

func TestDaemonLock(t *testing.T) {
	s := store(t)
	if running, err := s.DaemonRunning(); err != nil || running {
		t.Fatal(running, err)
	}
	d, err := s.TryDaemonLock()
	if err != nil {
		t.Fatal(err)
	}
	if running, err := s.DaemonRunning(); err != nil || !running {
		t.Fatal("held lock not seen", running, err)
	}
	if _, err := s.TryDaemonLock(); !errors.Is(err, ErrDaemonRunning) {
		t.Fatal(err)
	}
	d.Unlock()
	if running, _ := s.DaemonRunning(); running {
		t.Fatal("released lock still seen")
	}
	// The daemon reads without push.lock: a reader never sees a torn file
	// while a writer holds the lock and replaces it.
	commit(t, s, initFile)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.Load(); err != nil {
				t.Error("reader:", err)
				return
			}
		}
	})
	for range 50 {
		commit(t, s, func(f *File) error {
			f.Accounts["personal"] = Account{State: On, Address: "j@example.com", Refresh: "1//p", Granted: granted}
			return nil
		})
	}
	close(stop)
	wg.Wait()
}

const clientJSON = `{"installed":{"client_id":"123456789-abc.apps.googleusercontent.com","project_id":"pneu-push-123",` +
	`"auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://evil.example/token",` +
	`"auth_provider_x509_cert_url":"https://www.googleapis.com/oauth2/v1/certs","client_secret":"GOCSPX-abc",` +
	`"redirect_uris":["http://localhost"]}}`

func TestClient(t *testing.T) {
	s := store(t)
	l, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.WriteClient([]byte(clientJSON))
	l.Unlock()
	if err != nil || c.ID != "123456789-abc.apps.googleusercontent.com" || c.Project != "pneu-push-123" || c.Secret != "GOCSPX-abc" {
		t.Fatal(c, err)
	}
	got, err := s.LoadClient()
	if err != nil || got != c {
		t.Fatal(got, err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", c, c, c), "GOCSPX") {
		t.Fatal("secret printed")
	}
	snap := commit(t, s, initFile)
	if err := CheckClient(snap.File, c); err != nil {
		t.Fatal(err)
	}
	if err := CheckClient(snap.File, Client{ID: "9-x.apps.googleusercontent.com", Secret: "s", Project: c.Project}); err == nil {
		t.Fatal("another client accepted")
	}
	if err := CheckClient(snap.File, Client{ID: c.ID, Secret: "s", Project: "pneu-other-1"}); err == nil {
		t.Fatal("another project accepted")
	}

	for name, bad := range map[string]string{
		"web client":       strings.Replace(clientJSON, `"installed"`, `"web"`, 1),
		"two tops":         strings.TrimSuffix(clientJSON, "}") + `,"web":{}}`,
		"duplicate key":    strings.Replace(clientJSON, `"client_id"`, `"client_id":"x","client_id"`, 1),
		"no secret":        strings.Replace(clientJSON, `"client_secret":"GOCSPX-abc",`, ``, 1),
		"bad project":      strings.Replace(clientJSON, `"pneu-push-123"`, `"PNEU"`, 1),
		"bad id":           strings.Replace(clientJSON, `123456789-abc.apps`, `abc.apps`, 1),
		"trailing":         clientJSON + "x",
		"empty":            `{}`,
		"oversized":        strings.Replace(clientJSON, `"redirect_uris"`, `"pad":"`+strings.Repeat("x", MaxClient)+`","redirect_uris"`, 1),
		"secret with tab":  strings.Replace(clientJSON, `GOCSPX-abc`, `GOCSPX\tabc`, 1),
		"installed scalar": `{"installed":"x"}`,
	} {
		if _, err := ParseClient([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	os.Chmod(filepath.Join(s.Dir, ClientFile), 0o640)
	if _, err := s.LoadClient(); err == nil {
		t.Fatal("0640 client.json loaded")
	}
}
