package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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

const maxZipEntries = 1000

type zipEntry struct {
	Name     string `json:"name"`
	Size     uint64 `json:"size"`
	Modified string `json:"modified,omitempty"` // RFC 3339; absent when the archive has none
	Dir      bool   `json:"dir,omitempty"`
}

// partZip handles GET /part/{account}/{msgid}/{n}/zip: the archive's central
// directory as JSON, {entries, total, truncated}. Nothing is decompressed.
func (s *Server) partZip(w http.ResponseWriter, r *http.Request) {
	acct, m, p, body, _, ok := s.loadPart(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		log.Printf("zip %s/%s/%d: %v", acct.Name, m.ID, p.ID, err)
		w.WriteHeader(http.StatusUnsupportedMediaType)
		json.NewEncoder(w).Encode(map[string]string{"error": "not a zip archive"})
		return
	}
	out := struct {
		Entries   []zipEntry `json:"entries"`
		Total     int        `json:"total"`
		Truncated bool       `json:"truncated"`
	}{Entries: []zipEntry{}, Total: len(zr.File)}
	for _, f := range zr.File {
		if len(out.Entries) == maxZipEntries {
			out.Truncated = true
			break
		}
		e := zipEntry{Name: f.Name, Size: f.UncompressedSize64, Dir: f.FileInfo().IsDir()}
		if !f.Modified.IsZero() {
			e.Modified = f.Modified.Format(time.RFC3339)
		}
		out.Entries = append(out.Entries, e)
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("zip %s/%s/%d: %v", acct.Name, m.ID, p.ID, err)
	}
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
