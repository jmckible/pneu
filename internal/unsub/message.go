// Package unsub is the pure side of unsubscribe (docs/actions.md): reading
// a message file inside its account's maildir, the RFC 2369 List-Unsubscribe
// grammar, RFC 6068 mailto, DKIM verification (RFC 6376, RFC 8463), the
// choice of method, the one-click POST (RFC 8058) and the bound tokens.
package unsub

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// MaxHeader caps a message's header block.
	MaxHeader = 256 << 10
	// MaxBody caps the body DKIM hashes; past it, no one-click.
	MaxBody = 50 << 20
)

var errOutside = errors.New("unsub: file outside the account's maildir")

// Open opens name, a path notmuch reported, through os.OpenRoot(maildir):
// nothing outside the maildir is reachable, symlinks included. The file
// must be regular, checked on the opened descriptor.
func Open(maildir, name string) (*os.File, error) {
	if maildir == "" {
		return nil, errOutside
	}
	rel, ok := relTo(maildir, name)
	if !ok {
		// The configured maildir may be spelled through a symlink notmuch's
		// database.path resolves (or the reverse). Resolving the root is
		// safe: it is ours; the file's own path still goes through the Root.
		if real, err := filepath.EvalSymlinks(maildir); err == nil {
			rel, ok = relTo(real, name)
		}
	}
	if !ok {
		return nil, errOutside
	}
	root, err := os.OpenRoot(maildir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// O_NONBLOCK: opening a FIFO for reading must not wait for a writer.
	// It is harmless on the regular file this must be.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("unsub: not a regular file")
	}
	return f, nil
}

func relTo(base, name string) (string, bool) {
	if !filepath.IsAbs(base) || !filepath.IsAbs(name) {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(name))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// Field is one header field as stored: Raw is the whole field, name through
// its last continuation line, with every line ending CRLF (a file's bare LF
// endings are taken as the CRLF they stood for on the wire).
type Field struct {
	Name string // as written
	Raw  []byte
}

// Value is the field body after the colon, unfolded (CRLFs removed).
func (f Field) Value() string {
	_, v, _ := bytes.Cut(f.Raw, []byte(":"))
	return strings.ReplaceAll(string(v), "\r\n", "")
}

// Header is a message's header block in order.
type Header struct {
	Fields []Field
}

// Values returns the unfolded values of every field named name (ASCII
// case-insensitive), in order.
func (h *Header) Values(name string) []string {
	var out []string
	for _, f := range h.Fields {
		if strings.EqualFold(f.Name, name) {
			out = append(out, f.Value())
		}
	}
	return out
}

// Count is how many fields are named name.
func (h *Header) Count(name string) int {
	n := 0
	for _, f := range h.Fields {
		if strings.EqualFold(f.Name, name) {
			n++
		}
	}
	return n
}

var errHeader = errors.New("unsub: malformed header")

// ReadHeader reads the header block from br, up to MaxHeader bytes, leaving
// br at the first body byte. A line that is neither a field nor its
// continuation fails the whole message. The block must also parse with
// net/mail.ReadMessage and agree with it on the fields that matter here, so
// no two parsers see different List-Unsubscribe headers.
func ReadHeader(br *bufio.Reader) (*Header, error) {
	var h Header
	var block bytes.Buffer
	total := 0
	for {
		line, err := readLine(br, MaxHeader-total)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		total += len(line)
		content := trimEOL(line)
		if len(content) == 0 {
			break // the blank line, or EOF: no body
		}
		if content[0] == ' ' || content[0] == '\t' {
			if len(h.Fields) == 0 {
				return nil, errHeader
			}
			f := &h.Fields[len(h.Fields)-1]
			f.Raw = append(f.Raw, content...)
			f.Raw = append(f.Raw, '\r', '\n')
		} else {
			name, _, ok := bytes.Cut(content, []byte(":"))
			if !ok || !validFieldName(name) {
				return nil, errHeader
			}
			raw := append(append([]byte{}, content...), '\r', '\n')
			h.Fields = append(h.Fields, Field{Name: string(name), Raw: raw})
		}
		block.Write(content)
		block.WriteString("\r\n")
		if err != nil {
			break // last line, unterminated
		}
	}
	block.WriteString("\r\n")
	m, err := mail.ReadMessage(&block)
	if err != nil {
		return nil, errHeader
	}
	for _, name := range []string{"List-Unsubscribe", "List-Unsubscribe-Post", "DKIM-Signature"} {
		if len(m.Header[textproto.CanonicalMIMEHeaderKey(name)]) != h.Count(name) {
			return nil, errHeader
		}
	}
	return &h, nil
}

// readLine reads through the next '\n' (or EOF), failing past limit bytes.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(out)+len(chunk) > limit {
			return nil, errors.New("unsub: header too large")
		}
		out = append(out, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}

func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// validFieldName is RFC 5322 ftext: printable ASCII but ':'.
func validFieldName(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 33 || c > 126 || c == ':' {
			return false
		}
	}
	return true
}
