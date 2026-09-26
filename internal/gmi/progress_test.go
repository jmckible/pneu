package gmi

import (
	"strings"
	"testing"
	"time"
)

// Shapes from a real first pull's journal (the work account, 2026-09-23),
// fed a byte at a time as a pipe may deliver them.
func TestProgressWriter(t *testing.T) {
	w := newProgressWriter(time.Now)
	feed := func(s string) {
		for i := range len(s) {
			w.Write([]byte{s[i]})
		}
	}
	feed("pull: full synchronization (no previous synchronization state)\n")
	if p := w.snapshot(); p.Phase != "" {
		t.Fatalf("phase before any header: %q", p.Phase)
	}
	feed("fetching messages (1) ...pull: previous pull can be resumed using --resume\n")
	feed(strings.Repeat(".", 12))
	if p := w.snapshot(); p.Phase != PhaseListing || p.Done != 1200 || p.Total != 0 {
		t.Fatalf("listing: %+v", p)
	}
	feed(strings.Repeat(".", 63) + "done: 7498 its in 19.866s\n")
	if p := w.snapshot(); p.Done != 7498 || p.Listed != 7498 {
		t.Fatalf("listing done: %+v", p)
	}
	feed("removing deleted (0) ...done: 0 its in 00.000s\n")
	feed("receiving content (7498) ...remote: reducing batch request size to: 25\n")
	feed("..remote: increasing batch request size to: 50\n")
	feed("...remote: user rate error, increasing delay to 1\nremote: waiting 1.0 seconds..\n")
	feed(".......")
	if p := w.snapshot(); p.Phase != PhaseContent || p.Total != 7498 || p.Done != 120 || p.Listed != 7498 {
		t.Fatalf("content: %+v", p)
	}
	feed(strings.Repeat(".", 800) + "done: 7498 its in 08m:37.126s\n")
	if p := w.snapshot(); p.Done != 7498 {
		t.Fatalf("content done: %+v", p)
	}
	feed("receiving metadata (40) ....done: 40 its in 00.100s\n")
	if p := w.snapshot(); p.Phase != PhaseMetadata || p.Done != 40 || p.Total != 40 {
		t.Fatalf("metadata: %+v", p)
	}
}

// Dots never overshoot the total (a resumed metadata phase starts with one
// big update).
func TestProgressWriterClamps(t *testing.T) {
	w := newProgressWriter(time.Now)
	w.Write([]byte("receiving content (25) ......"))
	if p := w.snapshot(); p.Done != 25 {
		t.Fatalf("%+v", p)
	}
}
