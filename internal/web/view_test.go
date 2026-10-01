package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func genOf(s *Server) uint64 {
	s.view.mu.Lock()
	defer s.view.mu.Unlock()
	return s.view.gen
}

// viewEvents decodes the `view` events queued on c, in order.
func viewEvents(t *testing.T, c chan []byte) []ViewEvent {
	t.Helper()
	var out []ViewEvent
	for {
		select {
		case msg := <-c:
			name, data, ok := strings.Cut(strings.TrimPrefix(string(msg), "event: "), "\ndata: ")
			if !ok || name != "view" {
				continue
			}
			var ev ViewEvent
			if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
				t.Fatal(err)
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

const window = "0123456789abcdef0123456789abcdef"

func fromWindow(id string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-Pneu-Window", id) }
}

// Every successful write bumps the generation exactly once and says so to
// every stream and to the writer; a write that wrote nothing doesn't.
func TestViewGenTagWrites(t *testing.T) {
	f := newTagFixture(t)
	c := f.s.Hub.Subscribe()
	defer f.s.Hub.unsubscribe(c)
	thread := strings.TrimPrefix(strings.TrimSpace(string(f.env.Account(t, "personal").Notmuch(t, "search", "--output=threads", "--", "id:"+kitchen1))), "thread:")
	want := []ThreadRef{{"personal", thread}}

	w := post(f.s, form("account", "personal", "ids", esc(kitchen1, kitchen2), "action", "archive"), fromWindow(window))
	var resp tagResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK {
		t.Fatalf("archive %d %s", w.Code, w.Body)
	}
	if resp.Gen != 1 || resp.Epoch != f.s.view.epoch {
		t.Fatalf("response label %q/%d", resp.Epoch, resp.Gen)
	}
	evs := viewEvents(t, c)
	if len(evs) != 1 || evs[0].Gen != 1 || evs[0].Epoch != f.s.view.epoch || evs[0].From != window || !slices.Equal(evs[0].Threads, want) {
		t.Fatalf("events %+v, want one naming %v from %s", evs, want, window)
	}

	// Refused before writing: no bump.
	for _, v := range []map[string]string{
		{"action": "nope", "account": "personal", "ids": esc(kitchen1)},
		{"action": "archive", "account": "nobody", "ids": esc(kitchen1)},
		{"action": "archive", "account": "personal"},
	} {
		fv := form()
		for k, x := range v {
			fv.Set(k, x)
		}
		if w := post(f.s, fv); w.Code == http.StatusOK {
			t.Fatalf("%v accepted", v)
		}
	}
	// Unstar with nothing flagged writes nothing.
	tagOK(t, f.s, form("account", "work", "ids", esc(ssoPR), "action", "unstar"))
	if g := genOf(f.s); g != 1 {
		t.Fatalf("gen %d after writes that wrote nothing", g)
	}

	// Undo and mark-read are writes; neither page's header is required.
	if u := tagOK(t, f.s, form("action", "undo")); u.Gen != 2 {
		t.Fatalf("undo gen %d", u.Gen)
	}
	if r := tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen2), "action", "read")); r.Gen != 3 {
		t.Fatalf("read gen %d", r.Gen)
	}
	evs = viewEvents(t, c)
	if len(evs) != 2 || evs[0].Gen != 2 || evs[0].From != "" || !slices.Equal(evs[0].Threads, want) || evs[1].Gen != 3 {
		t.Fatalf("events %+v", evs)
	}

	// A locked write fails whole: no bump.
	f.s.TagTimeout = 300 * time.Millisecond
	release := holdLock(t, f.env.Account(t, "personal"))
	w = post(f.s, form("account", "personal", "ids", esc(kitchen1), "action", "archive"))
	release()
	if w.Code != http.StatusServiceUnavailable || genOf(f.s) != 3 || len(viewEvents(t, c)) != 0 {
		t.Fatalf("locked write: %d, gen %d", w.Code, genOf(f.s))
	}
}

func TestWindowFrom(t *testing.T) {
	for v, want := range map[string]string{
		window:                         window,
		"":                             "",
		strings.ToUpper(window):        "",
		window[:31]:                    "",
		window + "0":                   "",
		strings.Repeat("g", 32):        "",
		window[:30] + "\r\n":           "",
		"0123456789abcdef-123456789ab": "",
	} {
		r, _ := http.NewRequest("POST", "/tag", nil)
		if v != "" {
			r.Header["X-Pneu-Window"] = []string{v}
		}
		if got := windowFrom(r); got != want {
			t.Errorf("windowFrom(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestViewGenSend(t *testing.T) {
	fx := newTagFixture(t)
	v := replyForm(t, fx.s, "personal", cabin3, false).values
	fx.sync.sendErr = errors.New("gmi send [personal]: exit 1: HttpError 400")
	if w := postSend(fx.s, v); w.Code != http.StatusBadGateway || genOf(fx.s) != 0 {
		t.Fatalf("failed send: %d, gen %d", w.Code, genOf(fx.s))
	}
	fx.sync.mu.Lock()
	fx.sync.sendErr = nil
	fx.sync.mu.Unlock()
	c := fx.s.Hub.Subscribe()
	defer fx.s.Hub.unsubscribe(c)
	if w := postSend(fx.s, v); w.Code != http.StatusSeeOther {
		t.Fatalf("send: %d", w.Code)
	}
	thread := strings.TrimPrefix(strings.TrimSpace(string(fx.env.Account(t, "personal").Notmuch(t, "search", "--output=threads", "--", "id:"+cabin3))), "thread:")
	evs := viewEvents(t, c)
	if len(evs) != 1 || evs[0].Gen != 1 || !slices.Equal(evs[0].Threads, []ThreadRef{{"personal", thread}}) {
		t.Fatalf("events %+v", evs)
	}
}

// hello is the first event on a stream and carries the generation and the
// account views as of subscribing.
func TestHelloSnapshot(t *testing.T) {
	f := newTagFixture(t)
	tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen1), "action", "read"))
	f.s.Launch() // every account queued
	c, h := f.s.subscribe()
	defer f.s.Hub.unsubscribe(c)
	if h.Epoch != f.s.view.epoch || h.Gen != 1 || len(h.Accounts) != len(f.env.Accounts) {
		t.Fatalf("hello %+v", h)
	}
	for _, a := range h.Accounts {
		if !a.Queued {
			t.Errorf("hello says %s isn't queued", a.Name)
		}
	}
	if len(c) != 0 {
		t.Fatalf("%d events queued ahead of hello", len(c))
	}
}

// No stream ever carries a `view` its hello already counted, or skips one:
// each subscription races a writer bumping the generation.
func TestHelloOrderedAgainstBumps(t *testing.T) {
	s := newServer(t)
	const bumps, subs = 2000, 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range bumps {
			s.viewChanged("", nil)
		}
	}()
	genRE := regexp.MustCompile(`"gen":(\d+)`)
	errs := make(chan string, subs)
	for range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, h := s.subscribe()
			defer s.Hub.unsubscribe(c)
			next := h.Gen + 1
			for {
				select {
				case msg, ok := <-c:
					if !ok {
						return // dropped as slow: its page would reconnect and get a new hello
					}
					m := genRE.FindSubmatch(msg)
					g, _ := strconv.ParseUint(string(m[1]), 10, 64)
					if g != next {
						errs <- "hello at " + strconv.FormatUint(h.Gen, 10) + " then gen " + string(m[1])
						return
					}
					next++
				case <-time.After(50 * time.Millisecond):
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// A page's label is read before its query: a write landing between the two
// shows in the page, which says it is older than that write.
func TestPageLabelBeforeQuery(t *testing.T) {
	f := newTagFixture(t)
	p := f.env.Account(t, "personal")
	thread := strings.TrimPrefix(strings.TrimSpace(string(p.Notmuch(t, "search", "--output=threads", "--", "id:"+kitchen1))), "thread:")
	for _, target := range []string{"/", "/t/personal/" + thread} {
		before := genOf(f.s)
		f.s.onLabel = func() {
			f.s.onLabel = nil
			tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen2), "action", "unread"))
		}
		body := getOK(t, f.s, target)
		if !strings.Contains(body, `data-gen="`+strconv.FormatUint(before, 10)+`"`) || genOf(f.s) != before+1 {
			t.Fatalf("%s: labeled after the write (gen now %d)", target, genOf(f.s))
		}
		tagOK(t, f.s, form("account", "personal", "ids", esc(kitchen2), "action", "read"))
	}
}
