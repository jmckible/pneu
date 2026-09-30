package peer

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func rec(name, node string) Record {
	id := strings.Repeat(fmt.Sprintf("%02x", len(name)+len(node)*7), 32)
	return Record{Name: name, Node: node, SPKI: id, Origin: "http://pneu.localhost:7317", Added: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func TestStoreAddRemove(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	f, err := s.Load()
	if err != nil || f.Generation != 0 || len(f.Peers) != 0 {
		t.Fatalf("empty: %+v %v", f, err)
	}
	l, err := s.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a, b := rec("macbook", "nA"), rec("air", "nB")
	b.SPKI = strings.Repeat("b", 64)
	f1, err := l.Add(a)
	if err != nil || f1.Generation != 1 {
		t.Fatalf("add: %+v %v", f1, err)
	}
	f2, err := l.Add(b)
	if err != nil || f2.Generation != 2 || len(f2.Peers) != 2 {
		t.Fatalf("add 2: %+v %v", f2, err)
	}
	// Name, node and key are each unique.
	for name, r := range map[string]Record{
		"name": {Name: "macbook", Node: "nC", SPKI: strings.Repeat("c", 64), Origin: a.Origin},
		"node": {Name: "c", Node: "nA", SPKI: strings.Repeat("c", 64), Origin: a.Origin},
		"key":  {Name: "c", Node: "nC", SPKI: a.SPKI, Origin: a.Origin},
	} {
		if _, err := l.Add(r); err == nil {
			t.Errorf("duplicate %s accepted", name)
		}
	}
	f3, removed, err := l.Remove("macbook")
	if err != nil || f3.Generation != 3 || removed.Node != "nA" || len(f3.Peers) != 1 || f3.Peers[0].Name != "air" {
		t.Fatalf("remove: %+v %+v %v", f3, removed, err)
	}
	if _, _, err := l.Remove("macbook"); err == nil {
		t.Fatal("removed twice")
	}
	l.Unlock()

	got, err := s.Load()
	if err != nil || got.Generation != 3 || got.Hash != f3.Hash || got.Hash != hashOf(3, got.Peers) {
		t.Fatalf("reload: %+v %v", got, err)
	}
	fi, _ := os.Stat(s.path())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("peers.json mode %v", fi.Mode())
	}
	// No temp files left beside it.
	ents, _ := os.ReadDir(s.Dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("left %s", e.Name())
		}
	}
}

// N8: the lock is on peers.lock, which no write replaces; peers.json's
// inode changes on every write. A second writer waits on the same inode,
// so concurrent adds and removes never lose one another's generation.
func TestStoreLock(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	l, err := s.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	lockIno := inode(t, s.lockPath())
	if _, err := l.Add(rec("seed", "nSeed")); err != nil {
		t.Fatal(err)
	}
	jsonIno := inode(t, s.path())
	// Held: another Lock (its own open file description) times out.
	start := time.Now()
	if _, err := s.Lock(100 * time.Millisecond); err == nil || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("second lock while held: %v", err)
	}
	f, _, err := l.Remove("seed")
	if err != nil {
		t.Fatal(err)
	}
	if inode(t, s.lockPath()) != lockIno {
		t.Fatal("peers.lock was replaced")
	}
	if inode(t, s.path()) == jsonIno || inode(t, s.path()) == lockIno {
		t.Fatal("peers.json not replaced by rename, or is the lock")
	}
	l.Unlock()

	// Concurrent writers: every one lands, in consecutive generations.
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			l, err := s.Lock(10 * time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer l.Unlock()
			r := rec(fmt.Sprintf("p%d", i), fmt.Sprintf("n%d", i))
			r.SPKI = fmt.Sprintf("%064x", i+1)
			if _, err := l.Add(r); err != nil {
				errs <- err
				return
			}
			if i%2 == 1 { // odd ones leave again
				if _, _, err := l.Remove(r.Name); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := f.Generation + n + n/2; got.Generation != want || len(got.Peers) != n/2 {
		t.Fatalf("generation %d with %d peers, want %d with %d", got.Generation, len(got.Peers), want, n/2)
	}
	if inode(t, s.lockPath()) != lockIno {
		t.Fatal("peers.lock was replaced")
	}
}

func TestStoreLoadRefuses(t *testing.T) {
	write := func(t *testing.T, s Store, f File, mode fs.FileMode) {
		t.Helper()
		b, _ := json.Marshal(f)
		if err := os.WriteFile(s.path(), b, mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(s.path(), mode)
	}
	good := File{Generation: 1, Peers: []Record{rec("macbook", "nA")}}
	good.Hash = hashOf(1, good.Peers)

	cases := map[string]func(t *testing.T, s Store){
		"mode 0644": func(t *testing.T, s Store) { write(t, s, good, 0o644) },
		"mode 0400": func(t *testing.T, s Store) { write(t, s, good, 0o400) },
		"hash": func(t *testing.T, s Store) {
			f := good
			f.Peers = []Record{rec("macbook", "nB")}
			write(t, s, f, 0o600)
		},
		"generation": func(t *testing.T, s Store) {
			f := good
			f.Generation = 2
			write(t, s, f, 0o600)
		},
		"unknown field": func(t *testing.T, s Store) {
			os.WriteFile(s.path(), []byte(`{"generation":1,"hash":"`+good.Hash+`","peers":[],"extra":1}`), 0o600)
		},
		"trailing": func(t *testing.T, s Store) {
			b, _ := json.Marshal(good)
			os.WriteFile(s.path(), append(b, []byte(" {}")...), 0o600)
		},
		"bad record": func(t *testing.T, s Store) {
			f := File{Generation: 1, Peers: []Record{rec("Mac Book", "nA")}}
			f.Hash = hashOf(1, f.Peers)
			write(t, s, f, 0o600)
		},
		"bad origin": func(t *testing.T, s Store) {
			r := rec("macbook", "nA")
			r.Origin = "http://evil.example:7317"
			f := File{Generation: 1, Peers: []Record{r}}
			f.Hash = hashOf(1, f.Peers)
			write(t, s, f, 0o600)
		},
		"duplicate": func(t *testing.T, s Store) {
			r := rec("macbook", "nA")
			f := File{Generation: 1, Peers: []Record{r, r}}
			f.Hash = hashOf(1, f.Peers)
			write(t, s, f, 0o600)
		},
		"symlink": func(t *testing.T, s Store) {
			other := filepath.Join(s.Dir, "other.json")
			b, _ := json.Marshal(good)
			os.WriteFile(other, b, 0o600)
			os.Symlink(other, s.path())
		},
		"directory": func(t *testing.T, s Store) { os.Mkdir(s.path(), 0o700) },
		"oversize": func(t *testing.T, s Store) {
			os.WriteFile(s.path(), []byte(`{"generation":1,"hash":"`+strings.Repeat(" ", maxFile)+`"}`), 0o600)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s := Store{Dir: t.TempDir()}
			setup(t, s)
			if _, err := s.Load(); err == nil {
				t.Fatal("loaded")
			}
		})
	}
	s := Store{Dir: t.TempDir()}
	write(t, s, good, 0o600)
	if f, err := s.Load(); err != nil || f.Generation != 1 {
		t.Fatalf("good file: %v", err)
	}
	// A lock file someone else can read is refused too, as is a symlink.
	os.WriteFile(s.lockPath(), nil, 0o644)
	os.Chmod(s.lockPath(), 0o644)
	if _, err := s.Lock(time.Second); err == nil {
		t.Fatal("0644 lock accepted")
	}
	os.Remove(s.lockPath())
	os.Symlink(filepath.Join(s.Dir, "elsewhere"), s.lockPath())
	if _, err := s.Lock(time.Second); err == nil {
		t.Fatal("symlinked lock accepted")
	}
}

func TestValidOrigin(t *testing.T) {
	for o, want := range map[string]bool{
		"http://pneu.localhost:7317":   true,
		"http://pneu.localhost:1":      true,
		"http://pneu.localhost:65535":  true,
		"http://pneu.localhost:65536":  false,
		"http://pneu.localhost:0":      false,
		"http://pneu.localhost:07317":  false,
		"http://pneu.localhost":        false,
		"http://pneu.localhost:":       false,
		"https://pneu.localhost:7317":  false,
		"http://PNEU.localhost:7317":   false,
		"http://pneu.localhost:7317/":  false,
		"http://pneu.localhost:+7317":  false,
		"http://pneu.localhost:7317 ":  false,
		"http://x.pneu.localhost:7317": false,
		"http://127.0.0.1:7317":        false,
	} {
		if ValidOrigin(o) != want {
			t.Errorf("%q: %v", o, !want)
		}
	}
	for n, want := range map[string]bool{
		"macbook": true, "macbook-1": true, "a": true, strings.Repeat("a", 32): true,
		strings.Repeat("a", 33): false, "": false, "-mac": false, "mac-": false, "Mac": false,
		"mac book": false, "mac.book": false, "mac_book": false, "mac\n": false, "mäc": false,
	} {
		if ValidName(n) != want {
			t.Errorf("name %q: %v", n, !want)
		}
	}
}
