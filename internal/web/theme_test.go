package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestThemeCSS(t *testing.T) {
	s := newServer(t)
	s.ThemePath = filepath.Join(t.TempDir(), "absent.css") // never the real ~/.config file
	w := do(s, "GET", "/theme.css", withCookie)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/css; charset=utf-8" || w.Body.Len() != 0 {
		t.Fatalf("missing theme: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	p := filepath.Join(t.TempDir(), "theme.css")
	os.WriteFile(p, []byte(":root{--bg:#000}"), 0o600)
	s.ThemePath = p
	w = do(s, "GET", "/theme.css", withCookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "--bg:#000") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("theme: %d %q", w.Code, w.Body.String())
	}
	if !strings.Contains(getOK(t, s, "/"), `href="/theme.css"`) {
		t.Fatal("base.html does not link /theme.css")
	}
}

// themeEvent waits for the next message on c; "" on timeout.
func themeEvent(c chan []byte, wait time.Duration) string {
	select {
	case m := <-c:
		return string(m)
	case <-time.After(wait):
		return ""
	}
}

func TestWatchThemeBroadcasts(t *testing.T) {
	s := newServer(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "theme.css")
	if err := os.WriteFile(p, []byte(":root{--bg:#000}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.ThemePath = p
	c := s.Hub.Subscribe()
	defer s.Hub.unsubscribe(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.WatchTheme(ctx, 10*time.Millisecond)

	if m := themeEvent(c, 100*time.Millisecond); m != "" {
		t.Fatalf("event with no change: %q", m)
	}
	// The hook's atomic replace: write aside, rename over. Same size, and
	// possibly the same mtime second, so the new inode is what shows it.
	tmp := filepath.Join(dir, "theme.css.tmp")
	if err := os.WriteFile(tmp, []byte(":root{--bg:#111}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if m := themeEvent(c, time.Second); !strings.HasPrefix(m, "event: theme\ndata: {\"at\":") {
		t.Fatalf("after rename: %q, want a theme event", m)
	}
	if m := themeEvent(c, 100*time.Millisecond); m != "" {
		t.Fatalf("second event for one change: %q", m)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if m := themeEvent(c, time.Second); !strings.HasPrefix(m, "event: theme") {
		t.Fatalf("after remove: %q, want a theme event", m)
	}
}
