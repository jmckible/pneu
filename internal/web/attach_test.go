package web

import (
	"encoding/json"
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
