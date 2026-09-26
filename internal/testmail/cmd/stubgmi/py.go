package main

// Python-shaped output: lieer's non-TTY progress bar, json.dump's formatting,
// and tracebacks, so pneu sees the bytes real lieer 1.6 would print.

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// lieerDir is where the Arch package installs lieer; tracebacks cite it.
const lieerDir = "/usr/lib/python3.14/site-packages/lieer"

// vprint is Gmailieer.vprint: print unless --quiet.
func (g *gmi) vprint(format string, a ...any) {
	if !g.quiet {
		fmt.Fprintf(os.Stdout, format+"\n", a...)
	}
}

// bar is lieer/nobar.py:tqdm, the drop-in lieer uses whenever stdout or
// stderr isn't a TTY (gmailieer.py:Gmailieer.setup), which is always the case
// under pneu. Gmailieer.bar_create/bar_update/bar_close skip it under --quiet.
type bar struct {
	quiet bool
	it    int
	start time.Time
}

// barCreate prints `desc (total) ...` without a newline (nobar.tqdm.__init__).
// total < 0 stands for Python's None: `desc ...`.
func (g *gmi) barCreate(total int, desc string) *bar {
	b := &bar{quiet: g.quiet, start: time.Now()}
	if !b.quiet {
		if total >= 0 {
			fmt.Printf("%s (%d) ...", desc, total)
		} else {
			fmt.Printf("%s ...", desc)
		}
	}
	return b
}

// update prints one '.' whenever the running count is a multiple of 10
// (nobar.tqdm.update), so an update of 100 prints one dot, not ten.
func (b *bar) update(n int) {
	if b.quiet {
		return
	}
	b.it += n
	if b.it%10 == 0 {
		fmt.Print(".")
	}
}

// close prints `done: N its in <duration>` (nobar.tqdm.close).
func (b *bar) close() {
	if b.quiet {
		return
	}
	fmt.Println("done:", b.it, "its in", ppDuration(time.Since(b.start).Seconds()))
}

// ppDuration is nobar.tqdm.pp_duration.
func ppDuration(d float64) string {
	dys := math.Floor(d / 86400)
	d -= dys * 86400
	h := math.Floor(d / 3600)
	d -= h * 3600
	m := math.Floor(d / 60)
	d -= m * 60
	o, above := "", false
	if dys > 0 {
		o, above = fmt.Sprintf("%dd-", int(dys)), true
	}
	if above || h > 0 {
		o, above = o+fmt.Sprintf("%02dh:", int(h)), true
	}
	if above || m > 0 {
		o = o + fmt.Sprintf("%02dm:", int(m))
	}
	return o + fmt.Sprintf("%06.3fs", d)
}

// kv is one member of a JSON object in insertion order, as Python dicts keep it.
type kv struct {
	k string
	v any
}

// pyJSON renders v as Python's json.dump does by default: ", " and ": "
// separators, no indent, ensure_ascii escaping. v is a []kv, string, bool,
// int, int64, float64, json-number string (numLit), []string or []any.
func pyJSON(v any) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

// numLit is a number kept verbatim from a parsed file (600 vs 600.0).
type numLit string

func writeJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case []kv:
		b.WriteByte('{')
		for i, m := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSON(b, m.k)
			b.WriteString(": ")
			writeJSON(b, m.v)
		}
		b.WriteByte('}')
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSON(b, s)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSON(b, s)
		}
		b.WriteByte(']')
	case string:
		b.WriteString(pyQuoteJSON(x))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		b.WriteString(s)
	case numLit:
		b.WriteString(string(x))
	case nil:
		b.WriteString("null")
	default:
		panic(fmt.Sprintf("pyJSON: %T", v))
	}
}

func pyQuoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || r >= 0x7f && r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			case r > 0xffff:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyRepr is repr() of a str.
func pyRepr(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	var b strings.Builder
	b.WriteString(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == q:
			b.WriteString(`\` + q)
		case r == '\n':
			b.WriteString(`\n`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r == utf8.RuneError:
			b.WriteString(`�`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(q)
	return b.String()
}

// pySet is repr() of a set of str: {'a', 'b'}, or set() when empty.
func pySet(xs []string) string {
	if len(xs) == 0 {
		return "set()"
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = pyRepr(x)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// pyList is repr() of a list of str.
func pyList(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = pyRepr(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// frame is one traceback entry.
type frame struct {
	file string
	line int
	fn   string
	src  string
}

// Frames every lieer traceback starts with: /usr/bin/gmi, then
// gmailieer.py:Gmailieer.main dispatching to the subcommand.
var (
	frameScript = frame{"/usr/bin/gmi", 23, "<module>", "g.main ()"}
	frameMain   = frame{lieerDir + "/gmailieer.py", 418, "main", "args.func(args)"}
)

func lieerFrame(file string, line int, fn, src string) frame {
	return frame{lieerDir + "/" + file, line, fn, src}
}

// pyError is an uncaught Python exception: main prints it as a traceback on
// stderr and exits 1, as the interpreter does.
type pyError struct {
	frames []frame
	exc    string // "module.Class: message"
}

func (e *pyError) Error() string { return e.exc }

func raise(exc string, frames ...frame) error {
	return &pyError{frames: append([]frame{frameScript, frameMain}, frames...), exc: exc}
}

func (e *pyError) print() {
	var b strings.Builder
	b.WriteString("Traceback (most recent call last):\n")
	for _, f := range e.frames {
		fmt.Fprintf(&b, "  File %q, line %d, in %s\n    %s\n", f.file, f.line, f.fn, f.src)
	}
	b.WriteString(e.exc + "\n")
	os.Stdout.Sync()
	os.Stderr.WriteString(b.String())
}
