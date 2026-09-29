package web

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestViewKind(t *testing.T) {
	cases := []struct{ declared, name, kind string }{
		{"image/png", "a.png", "image"},
		{"image/jpg", "a.jpg", "image"},
		{"image/svg+xml", "logo.svg", "image"},
		{"application/pdf", "a.pdf", "pdf"},
		{"video/quicktime", "a.mov", "video"},
		{"audio/x-wav", "a.wav", "audio"},
		{"audio/mp3", "a.mp3", "audio"},
		{"text/plain", "notes.txt", "text"},
		{"text/x-diff", "fix.patch", "text"},
		{"application/json", "a.json", "text"},
		{"text/csv", "a.csv", "csv"},
		{"text/markdown", "README", "markdown"},
		{"text/x-markdown", "a.md", "markdown"},
		{"text/html", "a.html", "html"},
		{"text/calendar", "invite.ics", "ics"},
		{"application/ics", "invite.ics", "ics"},
		{"application/zip", "a.zip", "zip"},
		{"application/x-zip-compressed", "a.zip", "zip"},
		// Generic types fall back to the extension.
		{"application/octet-stream", "photo.JPEG", "image"},
		{"application/octet-stream", "scan.pdf", "pdf"},
		{"application/octet-stream", "clip.mp4", "video"},
		{"application/octet-stream", "data.json", "text"},
		{"application/octet-stream", "main.go", "text"},
		{"application/octet-stream", "table.tsv", "csv"},
		{"application/octet-stream", "notes.markdown", "markdown"},
		{"application/octet-stream", "event.ics", "ics"},
		{"application/x-download", "a.zip", "zip"},
		{"", "a.png", "image"},
		{"garbage", "a.pdf", "pdf"},
		{"text/plain", "table.csv", "csv"},
		{"text/plain", "notes.md", "markdown"},
		{"application/vnd.ms-excel", "export.csv", "csv"},
		// Nothing to show.
		{"application/octet-stream", "blob.bin", ""},
		{"application/octet-stream", "noext", ""},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "a.docx", ""},
		{"application/vnd.ms-excel", "a.xls", ""},
		{"image/heic", "a.heic", ""},
		{"message/rfc822", "fwd.eml", ""},
		// A declared type wins over the name.
		{"image/png", "trick.html", "image"},
		{"application/pdf", "trick.zip", "pdf"},
		{"text/plain", "x.png", "text"},
	}
	for _, c := range cases {
		if got := viewKind(effectiveType(c.declared, c.name)); got != c.kind {
			t.Errorf("%q %q: %q, want %q", c.declared, c.name, got, c.kind)
		}
	}
}

const (
	survey1 = "/part/personal/lot14-survey-1@larkspur-survey.example/"
	survey2 = "/part/personal/lot14-survey-2@larkspur-survey.example/"
)

func TestAttachmentViews(t *testing.T) {
	s := newServer(t)
	body := getOK(t, s, threadURL(t, s, "/all", "Survey files for lot 14"))
	for _, want := range []string{
		`<a href="` + survey1 + `3" target="_blank" rel="noopener" data-view="image">site-plan.png</a>`,
		`<a href="` + survey1 + `4" download="boundary.svg" data-view="image">boundary.svg</a>`,
		`<a href="` + survey1 + `5" target="_blank" rel="noopener" data-view="pdf">survey.pdf</a>`,
		`<a href="` + survey1 + `6" target="_blank" rel="noopener" data-view="video">walkthrough.mp4</a>`,
		`<a href="` + survey1 + `7" download="measurements.csv" data-view="csv">measurements.csv</a>`,
		`<a href="` + survey1 + `8" download="notes.md" data-view="markdown">notes.md</a>`,
		`<a href="` + survey1 + `9" download="readings.json" data-view="text">readings.json</a>`,
		`<a href="` + survey2 + `3" download="permit.html" data-view="html">permit.html</a>`,
		`<a href="` + survey2 + `4" download="site-visit.ics" data-view="ics">site-visit.ics</a>`,
		`<a href="` + survey2 + `5" download="photos.zip" data-view="zip">photos.zip</a>`,
		`<a href="` + survey2 + `6" download="contract.docx">contract.docx</a>`,
		`<a href="` + survey2 + `7" target="_blank" rel="noopener" data-view="audio">memo.wav</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("thread missing %s", want)
		}
	}
}

func TestPartViewer(t *testing.T) {
	s := newServer(t)
	get := func(target string, hdr map[string]string) (int, http.Header, string) {
		t.Helper()
		w := do(s, "GET", target, func(r *http.Request) {
			withCookie(r)
			for k, v := range hdr {
				r.Header.Set(k, v)
			}
		})
		return w.Code, w.Header(), w.Body.String()
	}
	sandboxed := func(h http.Header) bool {
		return strings.HasPrefix(h.Get("Content-Security-Policy"), "sandbox;") && h.Get("X-Content-Type-Options") == "nosniff"
	}

	// SVG is an image only to an image fetch; anything else gets text.
	code, h, b := get(survey1+"4", map[string]string{"Sec-Fetch-Dest": "image"})
	if code != 200 || h.Get("Content-Type") != "image/svg+xml" || !sandboxed(h) ||
		h.Get("Content-Disposition") != "attachment; filename*=UTF-8''boundary.svg" || !strings.HasPrefix(b, "<svg") {
		t.Errorf("svg as image: %d %v", code, h)
	}
	for _, dest := range []string{"", "document", "iframe", "empty", "script"} {
		code, h, _ := get(survey1+"4", map[string]string{"Sec-Fetch-Dest": dest})
		if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || !sandboxed(h) {
			t.Errorf("svg, dest %q: %d %v", dest, code, h)
		}
	}
	// Other parts don't care.
	if _, h, _ := get(survey2+"3", map[string]string{"Sec-Fetch-Dest": "image"}); !strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Errorf("html as image: %v", h)
	}

	// Media: inline under its own type, ranges answered.
	code, h, b = get(survey1+"6", map[string]string{"Range": "bytes=0-99"})
	if code != http.StatusPartialContent || h.Get("Content-Type") != "video/mp4" || len(b) != 100 ||
		!strings.HasPrefix(h.Get("Content-Range"), "bytes 0-99/") || h.Get("Content-Length") != "100" ||
		h.Get("Content-Disposition") != "inline; filename*=UTF-8''walkthrough.mp4" || !sandboxed(h) ||
		h.Get("Cache-Control") != "private, no-store" {
		t.Errorf("range: %d %v", code, h)
	}
	if code, h, _ := get(survey2+"7", nil); code != 200 || h.Get("Content-Type") != "audio/wav" || h.Get("Accept-Ranges") != "bytes" {
		t.Errorf("wav: %d %v", code, h)
	}
	if code, _, _ := get(survey1+"6", map[string]string{"Range": "bytes=999999-"}); code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("unsatisfiable range: %d", code)
	}
	w := do(s, "HEAD", survey1+"6", withCookie)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" || w.Header().Get("Content-Type") != "video/mp4" {
		t.Errorf("head: %d %v %d", w.Code, w.Header(), w.Body.Len())
	}

	// PDF: frameable by the app alone, never sandboxed.
	code, h, _ = get(survey1+"5", nil)
	if code != 200 || h.Get("Content-Type") != "application/pdf" || h.Get("X-Frame-Options") != "SAMEORIGIN" ||
		h.Get("Content-Security-Policy") != "frame-ancestors 'self'" {
		t.Errorf("pdf: %d %v", code, h)
	}
	// Everything else stays unframeable.
	if _, h, _ := get(survey1+"3", nil); h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("png frame options: %v", h)
	}

	// An octet-stream JSON is served as what its name says.
	if code, h, b := get(survey1+"9", nil); code != 200 || h.Get("Content-Type") != "application/json" || !strings.Contains(b, `"station"`) {
		t.Errorf("json: %d %v", code, h)
	}
}

func TestPartZip(t *testing.T) {
	s := newServer(t)
	w := do(s, "GET", survey2+"5/zip", withCookie)
	var got struct {
		Entries   []zipEntry `json:"entries"`
		Total     int        `json:"total"`
		Truncated bool       `json:"truncated"`
	}
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []zipEntry{
		{Name: "photos/", Size: 0, Modified: "2026-09-08T10:00:00Z", Dir: true},
		{Name: "photos/north-line.jpg", Size: 2048, Modified: "2026-09-08T10:12:30Z"},
		{Name: "photos/p3-post.jpg", Size: 1024, Modified: "2026-09-08T10:20:02Z"},
		{Name: "README.txt", Size: 24, Modified: "2026-09-08T11:00:00Z"},
	}
	if got.Total != 4 || got.Truncated || len(got.Entries) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		// zip stores local times; archive/zip reads them as UTC when the
		// archive carries no timezone.
		if got.Entries[i] != want[i] {
			t.Errorf("entry %d: %+v, want %+v", i, got.Entries[i], want[i])
		}
	}

	// Not a zip (the docx fixture is a stub; the PDF is a PDF): a JSON error.
	for _, n := range []string{"6", "3"} {
		w := do(s, "GET", survey2+n+"/zip", withCookie)
		if w.Code != http.StatusUnsupportedMediaType || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("part %s: %d %s", n, w.Code, w.Body)
		}
	}
	for _, target := range []string{survey2 + "99/zip", survey2 + "1/zip", "/part/work/lot14-survey-2@larkspur-survey.example/5/zip"} {
		if w := do(s, "GET", target, withCookie); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
	// The same gate as every GET: session and Host.
	if w := do(s, "GET", survey2+"5/zip", nil); w.Code != http.StatusForbidden {
		t.Errorf("no session: %d", w.Code)
	}
	if w := do(s, "GET", survey2+"5/zip", func(r *http.Request) { withCookie(r); r.Host = "127.0.0.1:7317" }); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("wrong host: %d", w.Code)
	}
}

// zipOf is an archive of n empty files named by name(i).
func zipOf(t *testing.T, n int, name func(int) string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < n; i++ {
		if _, err := zw.CreateHeader(&zip.FileHeader{Name: name(i), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func short(i int) string { return fmt.Sprintf("f%d", i) }

func TestZipDirectory(t *testing.T) {
	check := func(label string, b []byte, entries uint64, tooMany bool) {
		t.Helper()
		d, err := zipDirectory(b)
		if err != nil || d.entries != entries || d.tooMany != tooMany {
			t.Errorf("%s: %+v %v, want %d %v", label, d, err, entries, tooMany)
		}
	}
	small := zipOf(t, 3, short)
	check("small", small, 3, false)
	check("empty archive", zipOf(t, 0, short), 0, false)
	check("at the limit", zipOf(t, maxZipDirEntries, short), maxZipDirEntries, false)
	// Over it, by the end record's count: refused before archive/zip parses.
	check("over the limit", zipOf(t, maxZipDirEntries+1, short), maxZipDirEntries+1, true)
	// zip.Writer writes zip64 records from 65535 entries on.
	check("zip64", zipOf(t, 70000, short), 70000, true)

	// A count that understates (the 16-bit count wraps at 65536): the
	// headers are counted too, and the total isn't known.
	lie := zipOf(t, maxZipDirEntries+5, short)
	binary.LittleEndian.PutUint16(lie[len(lie)-22+8:], 5)
	binary.LittleEndian.PutUint16(lie[len(lie)-22+10:], 5)
	check("lying count", lie, 0, true)

	// A central directory claimed larger than any listing needs.
	big := bytes.Clone(small)
	binary.LittleEndian.PutUint32(big[len(big)-22+12:], maxZipDirSize+1)
	check("huge directory", big, 3, true)

	// A small archive dressed as zip64 (locator and record before the end
	// record, which then holds only 0xffff…): read through, and archive/zip
	// agrees.
	eocd := len(small) - 22
	var z64 bytes.Buffer
	z64.Write(small[:eocd])
	le := binary.LittleEndian
	rec := make([]byte, 56)
	le.PutUint32(rec, 0x06064b50)
	le.PutUint64(rec[4:], 44)
	le.PutUint16(rec[12:], 45)
	le.PutUint16(rec[14:], 45)
	le.PutUint64(rec[24:], 3)
	le.PutUint64(rec[32:], 3)
	le.PutUint64(rec[40:], uint64(le.Uint32(small[eocd+12:])))
	le.PutUint64(rec[48:], uint64(le.Uint32(small[eocd+16:])))
	z64.Write(rec)
	loc := make([]byte, 20)
	le.PutUint32(loc, 0x07064b50)
	le.PutUint64(loc[8:], uint64(eocd))
	le.PutUint32(loc[16:], 1)
	z64.Write(loc)
	end := bytes.Clone(small[eocd:])
	le.PutUint16(end[8:], 0xffff)
	le.PutUint16(end[10:], 0xffff)
	le.PutUint32(end[12:], 0xffffffff)
	le.PutUint32(end[16:], 0xffffffff)
	z64.Write(end)
	check("small zip64", z64.Bytes(), 3, false)
	if zr, err := zip.NewReader(bytes.NewReader(z64.Bytes()), int64(z64.Len())); err != nil || len(zr.File) != 3 {
		t.Errorf("small zip64: archive/zip says %v", err)
	}

	// Malformed: all errors, none a panic.
	badLoc := bytes.Clone(z64.Bytes())
	le.PutUint64(badLoc[len(badLoc)-22-20+8:], 1<<62)
	badComment := bytes.Clone(small)
	le.PutUint16(badComment[len(badComment)-2:], 100)
	badOffset := bytes.Clone(small)
	le.PutUint32(badOffset[len(badOffset)-22+12:], uint32(len(small)))
	for label, b := range map[string][]byte{
		"nothing": nil, "text": []byte("not a zip at all"), "truncated": small[:len(small)-5],
		"end record alone": small[eocd:], "zip64 locator past the end": badLoc,
		"comment past the end": badComment, "directory before the file": badOffset,
		"zip64 locator alone": z64.Bytes()[eocd+56:],
	} {
		if d, err := zipDirectory(b); err == nil {
			t.Errorf("%s: %+v, want an error", label, d)
		}
	}
}

func TestZipListing(t *testing.T) {
	read := func(b []byte) []*zip.File {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		return zr.File
	}
	if e, tr := zipListing(read(zipOf(t, maxZipEntries+1, short))); len(e) != maxZipEntries || !tr {
		t.Errorf("entries: %d %v", len(e), tr)
	}
	// Long names stop the listing at maxZipNameBytes.
	long := func(i int) string { return fmt.Sprintf("%04d", i) + strings.Repeat("x", 996) }
	if e, tr := zipListing(read(zipOf(t, 300, long))); len(e) != maxZipNameBytes/1000 || !tr {
		t.Errorf("names: %d %v", len(e), tr)
	}
	if e, tr := zipListing(read(zipOf(t, 3, short))); len(e) != 3 || tr {
		t.Errorf("small: %d %v", len(e), tr)
	}
}
