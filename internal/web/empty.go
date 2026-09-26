package web

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxEmptyArt caps the empty-list art file; a 54x26 block logo is ~4KB.
const maxEmptyArt = 16 << 10

// EmptyArtPath is the optional text art an empty list shows as a watermark:
// $XDG_CONFIG_HOME/pneu/empty.txt. The install links it to the desktop's
// branding (Omarchy's about.txt); the server only knows it is plain text.
func EmptyArtPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pneu", "empty.txt"), nil
}

// emptyArt reads the art on every empty render, so a relinked or edited file
// shows on the next load. Missing, oversized, or non-UTF-8 means no art.
func (s *Server) emptyArt() string {
	p := s.EmptyArtPath
	if p == "" {
		var err error
		if p, err = EmptyArtPath(); err != nil {
			return ""
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxEmptyArt+1))
	if err != nil || len(b) > maxEmptyArt || !utf8.Valid(b) {
		return ""
	}
	return strings.Trim(string(b), "\n")
}
