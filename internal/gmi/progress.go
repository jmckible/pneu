package gmi

import (
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Progress is a first pull's progress, read off lieer 1.6's non-TTY output
// (lieer/nobar.py): each phase prints a header "desc (total) ...", then one
// '.' whenever its running count reaches a multiple of 10, then
// "done: N its in D". While listing, the count grows by a page of 100 ids at
// a time, so a dot is a page. Other lines ("remote: reducing batch request
// size to: 25") land mid-line and end the run of dots, and some carry dots
// of their own, so only a line's leading dots, or those right after a
// header, count. It is for display: when the output doesn't parse, Phase
// stays empty and the meter is indeterminate, never wrong.
type Progress struct {
	Phase    Phase     // "" until the first header
	Done     int       // items so far in Phase; exact once its "done:" line arrives
	Total    int       // Phase's total; 0 while listing, whose total isn't known yet
	Listed   int       // messages the listing found, once it finished
	Frontier time.Time // the oldest message stored so far (newest-first, so "complete back to")
	Started  time.Time
	Updated  time.Time // last output of any kind
}

// Phase is one of lieer's full-pull phases.
type Phase string

const (
	PhaseListing  Phase = "listing"  // fetching messages: listing every id
	PhaseRemoving Phase = "removing" // removing deleted: local files gone from Gmail
	PhaseContent  Phase = "content"  // receiving content: downloading raw messages
	PhaseMetadata Phase = "metadata" // receiving metadata: labels for mail already local
)

var phaseHeaders = map[string]Phase{
	"fetching messages":  PhaseListing,
	"removing deleted":   PhaseRemoving,
	"receiving content":  PhaseContent,
	"receiving metadata": PhaseMetadata,
}

var (
	headerRE = regexp.MustCompile(`^(fetching messages|removing deleted|receiving content|receiving metadata) \((\d+)\) \.\.\.$`)
	doneRE   = regexp.MustCompile(`done: (\d+) its in`)
)

// progressWriter parses gmi's output as it streams. Safe for one writer and
// concurrent snapshot readers.
type progressWriter struct {
	mu      sync.Mutex
	p       Progress
	line    []byte
	dotting bool // counting dots: at a line's start, or right after a header
	dots    int  // in the current phase
	now     func() time.Time
}

func newProgressWriter(now func() time.Time) *progressWriter {
	t := now()
	return &progressWriter{p: Progress{Started: t, Updated: t}, dotting: true, now: now}
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.p.Updated = w.now()
	for _, c := range b {
		switch {
		case c == '\n':
			if m := doneRE.FindSubmatch(w.line); m != nil && w.p.Phase != "" {
				n, _ := strconv.Atoi(string(m[1]))
				w.p.Done = n
				if w.p.Phase == PhaseListing {
					w.p.Listed = n
				}
			}
			w.line = w.line[:0]
			w.dotting = true
			continue
		case c == '.' && w.dotting: // dotting means the line holds only dots so far
			w.dots++
			w.line = append(w.line, c)
			w.count()
			continue
		}
		w.line = append(w.line, c)
		if c != '.' {
			w.dotting = false
		}
		if c == '.' {
			if m := headerRE.FindSubmatch(w.line); m != nil {
				w.p.Phase = phaseHeaders[string(m[1])]
				w.p.Total, _ = strconv.Atoi(string(m[2]))
				if w.p.Phase == PhaseListing {
					w.p.Total = 0 // lieer's placeholder total of 1
				}
				w.p.Done, w.dots = 0, 0
				w.dotting = true
				w.line = w.line[:0] // dots after the header count from here
			}
		}
	}
	return len(b), nil
}

func (w *progressWriter) count() {
	per := 10
	if w.p.Phase == PhaseListing {
		per = 100
	}
	w.p.Done = w.dots * per
	if w.p.Total > 0 && w.p.Done > w.p.Total {
		w.p.Done = w.p.Total
	}
}

// restart resets the clocks: the run starts now, whatever waited before it.
func (w *progressWriter) restart(t time.Time) {
	w.mu.Lock()
	w.p.Started, w.p.Updated = t, t
	w.mu.Unlock()
}

func (w *progressWriter) snapshot() Progress {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.p
}

func (w *progressWriter) setFrontier(t time.Time) {
	w.mu.Lock()
	w.p.Frontier = t
	w.mu.Unlock()
}
