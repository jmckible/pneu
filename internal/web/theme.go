package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ThemePath is where the desktop's theme hook writes CSS variable overrides:
// $XDG_CONFIG_HOME/pneu/theme.css, default ~/.config/pneu/theme.css.
// The hook is install/pneu-theme; the server only serves the file.
func ThemePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pneu", "theme.css"), nil
}

// configDir is $XDG_CONFIG_HOME, default ~/.config.
func configDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config"), nil
}

// theme serves the override sheet, or an empty sheet when there is none, so
// base.html can always link it. Never cached: theme-set rewrites it in place.
func (s *Server) theme(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	p := s.ThemePath
	if p == "" {
		var err error
		if p, err = ThemePath(); err != nil {
			return
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "theme unreadable", http.StatusInternalServerError)
		}
		return
	}
	w.Write(b)
}

// ThemePoll is how often WatchTheme looks at the theme file.
const ThemePoll = 2 * time.Second

// WatchTheme broadcasts SSE `theme` whenever the theme file changes, so open
// pages restyle without a reload. The hook replaces the file by rename, so a
// new inode counts as a change, as do mtime, size, and appearing or going.
// Polling os.Stat keeps it stdlib; it runs until ctx ends.
func (s *Server) WatchTheme(ctx context.Context, every time.Duration) {
	p := s.ThemePath
	if p == "" {
		var err error
		if p, err = ThemePath(); err != nil {
			return
		}
	}
	prev, _ := os.Stat(p)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur, _ := os.Stat(p)
		if themeChanged(prev, cur) {
			s.Hub.Broadcast("theme", map[string]any{"at": time.Now().Unix()})
		}
		prev = cur
	}
}

func themeChanged(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return (a == nil) != (b == nil)
	}
	return !os.SameFile(a, b) || !a.ModTime().Equal(b.ModTime()) || a.Size() != b.Size()
}
