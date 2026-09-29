package web

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jmckible/pneu/internal/notmuch"
)

// ---- attachment viewer -----------------------------------------------------
// Each attachment gets a viewer kind (data-view on its link); app.js's viewer
// (viewer.js) shows it in a modal. "" is download-only. The kind and the
// type /part serves come from the same effective type, so a viewer never
// asks the part endpoint for something it won't serve in that form.

// typeAliases folds the nonstandard spellings mailers send into the type
// browsers know.
var typeAliases = map[string]string{
	"image/jpg": "image/jpeg", "image/pjpeg": "image/jpeg", "image/x-png": "image/png",
	"audio/mp3": "audio/mpeg", "audio/x-mp3": "audio/mpeg", "audio/mpeg3": "audio/mpeg", "audio/x-mpeg": "audio/mpeg",
	"audio/x-wav": "audio/wav", "audio/wave": "audio/wav", "audio/vnd.wave": "audio/wav",
	"audio/x-flac": "audio/flac", "audio/x-m4a": "audio/mp4", "audio/m4a": "audio/mp4",
	"application/x-zip-compressed": "application/zip", "application/x-zip": "application/zip",
	"text/x-markdown": "text/markdown", "application/ics": "text/calendar",
}

// genericTypes say nothing about the bytes; the filename decides instead.
var genericTypes = map[string]bool{
	"": true, "application/octet-stream": true, "application/x-download": true, "application/force-download": true,
	"application/download": true, "application/binary": true, "application/unknown": true, "application/x-unknown": true,
}

// typeByExt is what a generic part's extension makes it. Source files are
// text/plain: the viewer shows them as text, and nothing here serves them as
// anything a browser would run (.svg and .html still pass through neuter).
var typeByExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".avif": "image/avif", ".bmp": "image/bmp", ".svg": "image/svg+xml",
	".pdf": "application/pdf",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".ogv": "video/ogg",
	".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".wav": "audio/wav", ".m4a": "audio/mp4",
	".flac": "audio/flac", ".opus": "audio/opus", ".aac": "audio/aac",
	".json": "application/json", ".patch": "text/x-diff", ".diff": "text/x-diff",
	".csv": "text/csv", ".tsv": "text/tab-separated-values",
	".md": "text/markdown", ".markdown": "text/markdown",
	".html": "text/html", ".htm": "text/html",
	".ics": "text/calendar",
	".zip": "application/zip",
}

// textExts are plain text by any other name.
var textExts = []string{
	".txt", ".text", ".log", ".rb", ".go", ".py", ".js", ".mjs", ".ts", ".tsx", ".jsx", ".sh", ".bash", ".zsh",
	".fish", ".yml", ".yaml", ".toml", ".ini", ".cfg", ".conf", ".env", ".sql", ".c", ".h", ".cc", ".cpp", ".hpp",
	".rs", ".java", ".kt", ".swift", ".cs", ".php", ".pl", ".lua", ".ex", ".exs", ".erl", ".hs", ".clj", ".scala",
	".r", ".css", ".scss", ".xml", ".tex", ".srt", ".vtt", ".nfo", ".rst", ".adoc", ".org", ".gradle", ".lock",
}

func init() {
	for _, e := range textExts {
		typeByExt[e] = "text/plain"
	}
}

// effectiveType is the part's media type as pneu treats it: the declared
// type (lower-cased, no parameters), aliases folded, or the filename's when
// the declared one is generic. A text/plain part can become a more specific
// text format (.csv, .md, .ics…), a CSV sent as Excel's type becomes text/csv;
// nothing else is overridden by a name.
func effectiveType(mt, name string) string {
	if a, ok := typeAliases[mt]; ok {
		mt = a
	}
	byExt := typeByExt[strings.ToLower(path.Ext(name))]
	switch {
	case byExt == "":
	case genericTypes[mt] || !strings.Contains(mt, "/"):
		return byExt
	case mt == "text/plain" && strings.HasPrefix(byExt, "text/"):
		return byExt
	case mt == "application/vnd.ms-excel" && byExt == "text/csv":
		return byExt
	}
	return mt
}

var viewByType = map[string]string{
	"image/png": "image", "image/jpeg": "image", "image/gif": "image", "image/webp": "image",
	"image/avif": "image", "image/bmp": "image",
	// Only ever as <img>: /part serves SVG as image/svg+xml to an image
	// fetch alone (Sec-Fetch-Dest), where scripts don't run.
	"image/svg+xml":   "image",
	"application/pdf": "pdf",
	"video/mp4":       "video", "video/webm": "video", "video/quicktime": "video", "video/ogg": "video",
	"audio/mpeg": "audio", "audio/ogg": "audio", "audio/wav": "audio", "audio/mp4": "audio",
	"audio/flac": "audio", "audio/opus": "audio", "audio/aac": "audio", "audio/webm": "audio",
	"application/json": "text", "application/x-yaml": "text", "application/yaml": "text", "application/toml": "text",
	"application/sql": "text", "application/x-sh": "text", "application/javascript": "text", "application/xml": "text",
	"text/csv": "csv", "text/tab-separated-values": "csv",
	"text/markdown":   "markdown",
	"text/html":       "html",
	"text/calendar":   "ics",
	"application/zip": "zip",
}

// viewKind is the viewer that shows a part of effective type mt, or "".
func viewKind(mt string) string {
	if k, ok := viewByType[mt]; ok {
		return k
	}
	if strings.HasPrefix(mt, "text/") {
		return "text" // shown as source, via textContent
	}
	return ""
}

// ---- zip listing -----------------------------------------------------------

const (
	maxZipEntries   = 1000 // listed
	maxZipNameBytes = 256 << 10
	// Past these the archive isn't parsed at all: zip.NewReader builds a
	// File for every central-directory entry before any cap applies.
	maxZipDirEntries = 20000
	maxZipDirSize    = 8 << 20
)

type zipEntry struct {
	Name     string `json:"name"`
	Size     uint64 `json:"size"`
	Modified string `json:"modified,omitempty"` // RFC 3339; absent when the archive has none
	Dir      bool   `json:"dir,omitempty"`
}

// partZip handles GET /part/{account}/{msgid}/{n}/zip: the archive's central
// directory as JSON, {entries, total, truncated}, or {tooMany, total} (total
// 0 when unknown) for one too large to parse. Nothing is decompressed.
func (s *Server) partZip(w http.ResponseWriter, r *http.Request) {
	acct, m, p, body, _, ok := s.loadPart(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	out := struct {
		Entries   []zipEntry `json:"entries"`
		Total     uint64     `json:"total"`
		Truncated bool       `json:"truncated"`
		TooMany   bool       `json:"tooMany,omitempty"`
	}{Entries: []zipEntry{}}
	var zr *zip.Reader
	d, err := zipDirectory(body)
	if err == nil {
		if d.tooMany {
			out.Total, out.Truncated, out.TooMany = d.entries, true, true
		} else {
			zr, err = zip.NewReader(bytes.NewReader(body), int64(len(body)))
		}
	}
	if err != nil {
		log.Printf("zip %s/%s/%d: %v", acct.Name, m.ID, p.ID, err)
		w.WriteHeader(http.StatusUnsupportedMediaType)
		json.NewEncoder(w).Encode(map[string]string{"error": "not a zip archive"})
		return
	}
	if zr != nil {
		out.Total = uint64(len(zr.File))
		out.Entries, out.Truncated = zipListing(zr.File)
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("zip %s/%s/%d: %v", acct.Name, m.ID, p.ID, err)
	}
}

// zipListing is the first maxZipEntries entries, fewer if their names pass
// maxZipNameBytes; truncated says whether any were left out.
func zipListing(files []*zip.File) (entries []zipEntry, truncated bool) {
	entries = []zipEntry{}
	names := 0
	for _, f := range files {
		names += len(f.Name)
		if len(entries) == maxZipEntries || names > maxZipNameBytes {
			return entries, true
		}
		e := zipEntry{Name: f.Name, Size: f.UncompressedSize64, Dir: f.FileInfo().IsDir()}
		if !f.Modified.IsZero() {
			e.Modified = f.Modified.Format(time.RFC3339)
		}
		entries = append(entries, e)
	}
	return entries, false
}

type zipDir struct {
	entries uint64 // as the end record says; 0 when it can't be believed
	tooMany bool
}

var errNotZip = errors.New("no end of central directory")

// zipDirectory reads the End Of Central Directory record (and its zip64
// successor) the way archive/zip does, and says whether the directory is
// too large to hand to it. The record's 16-bit count is only the true count
// mod 65536, and archive/zip reads headers until one fails to parse, so the
// headers are also counted here, without allocating, up to the limit. An
// error means archive/zip would refuse it too (or near enough): not a zip.
func zipDirectory(b []byte) (zipDir, error) {
	const eocdLen, loc64Len, eocd64Len, hdrLen = 22, 20, 56, 46
	size := int64(len(b))
	end := int64(-1)
	for i := size - eocdLen; i >= 0 && i >= size-(eocdLen+0xffff); i-- {
		if binary.LittleEndian.Uint32(b[i:]) == 0x06054b50 {
			// A comment running past the end: archive/zip gives up here.
			if int64(binary.LittleEndian.Uint16(b[i+20:]))+eocdLen+i > size {
				return zipDir{}, errNotZip
			}
			end = i
			break
		}
	}
	if end < 0 {
		return zipDir{}, errNotZip
	}
	rec := b[end:]
	count := uint64(binary.LittleEndian.Uint16(rec[10:]))
	dirSize := uint64(binary.LittleEndian.Uint32(rec[12:]))
	dirOff := uint64(binary.LittleEndian.Uint32(rec[16:]))
	if count == 0xffff || dirSize == 0xffffffff || dirOff == 0xffffffff {
		// zip64: a locator just before the record points at the real one.
		if l := end - loc64Len; l >= 0 && binary.LittleEndian.Uint32(b[l:]) == 0x07064b50 &&
			binary.LittleEndian.Uint32(b[l+4:]) == 0 && binary.LittleEndian.Uint32(b[l+16:]) == 1 {
			p := binary.LittleEndian.Uint64(b[l+8:])
			if size < eocd64Len || p > uint64(size-eocd64Len) || binary.LittleEndian.Uint32(b[p:]) != 0x06064b50 {
				return zipDir{}, errNotZip
			}
			end = int64(p)
			count = binary.LittleEndian.Uint64(b[p+32:])
			dirSize = binary.LittleEndian.Uint64(b[p+40:])
			dirOff = binary.LittleEndian.Uint64(b[p+48:])
		}
	}
	if count > maxZipDirEntries || dirSize > maxZipDirSize {
		return zipDir{entries: count, tooMany: true}, nil
	}
	// Where archive/zip starts reading: just before the end record, the
	// directory's size back. Data prepended to the archive (a
	// self-extractor) shows as a base offset, unless a header is already at
	// the stated offset.
	start := end - int64(dirSize)
	if dirOff > 1<<63-1 || start < 0 || start >= size {
		return zipDir{}, errNotZip
	}
	base := start - int64(dirOff)
	hdrAt := func(o int64) bool { return o+hdrLen <= size && binary.LittleEndian.Uint32(b[o:]) == 0x02014b50 }
	if base > 0 && hdrAt(int64(dirOff)) {
		start = int64(dirOff)
	}
	var n uint64
	for o := start; hdrAt(o); n++ {
		if n == maxZipDirEntries {
			if count < n {
				count = 0 // the record lied (or wrapped at 65536)
			}
			return zipDir{entries: count, tooMany: true}, nil
		}
		o += hdrLen + int64(binary.LittleEndian.Uint16(b[o+28:])) +
			int64(binary.LittleEndian.Uint16(b[o+30:])) + int64(binary.LittleEndian.Uint16(b[o+32:]))
	}
	return zipDir{entries: count}, nil
}

// loadPart resolves {account}/{msgid}/{n} to a leaf part and its decoded
// bytes, answering 404/500 itself on failure. ct is the type notmuch
// reports, with any charset.
func (s *Server) loadPart(w http.ResponseWriter, r *http.Request) (acct notmuch.Account, m notmuch.Message, p *notmuch.Part, body []byte, ct string, ok bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	acct, m, ok = s.message(w, r)
	if !ok {
		return
	}
	ok = false
	p = findPart(&m, n)
	if p == nil || strings.HasPrefix(lowerType(p), "multipart/") {
		http.NotFound(w, r)
		return
	}
	body, ct, err = acct.Part(r.Context(), m.ID, n)
	if err != nil {
		log.Printf("part %s/%s/%d: %v", acct.Name, m.ID, n, err)
		http.Error(w, "notmuch failed", http.StatusInternalServerError)
		return
	}
	return acct, m, p, body, ct, true
}
