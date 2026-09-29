package web

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jmckible/pneu/internal/notmuch"
)

const (
	docID    = "1sSoRoLL0utPlaN4kQ7vXw2Zt9Yb3Cd5Ef6Gh"
	sheetID  = "1TeNaNtLiSt9pQ2rS4tU6vW8xY0zA2bC4d"
	fileID   = "0B7x_Qm-2kLpRsTuVwXyZ01234"
	folderID = "1FoLdEr_aBcDeFgHiJkLmNoPq"
)

func textMsg(body string) *notmuch.Message {
	return &notmuch.Message{Body: []notmuch.Part{{ID: 1, ContentType: "text/plain", Content: body, HasContent: true}}}
}

func htmlMsg(plain, body string) *notmuch.Message {
	return &notmuch.Message{Body: []notmuch.Part{{ID: 1, ContentType: "multipart/alternative", Children: []notmuch.Part{
		{ID: 2, ContentType: "text/plain", Content: plain, HasContent: true},
		{ID: 3, ContentType: "text/html", Content: body, HasContent: true},
	}}}}
}

// refKeys is each ref as "kind/id title".
func refKeys(refs []driveRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.key()+" "+r.Title)
	}
	return out
}

func TestDriveRefs(t *testing.T) {
	cases := []struct {
		name string
		m    *notmuch.Message
		want []string
	}{
		{"doc", textMsg("see https://docs.google.com/document/d/" + docID + "/edit?usp=sharing"), []string{"doc/" + docID + " "}},
		{"sheet", textMsg("https://docs.google.com/spreadsheets/d/" + sheetID + "/edit#gid=0"), []string{"sheet/" + sheetID + " "}},
		{"slides", textMsg("https://docs.google.com/presentation/d/" + docID + "/"), []string{"slides/" + docID + " "}},
		{"form", textMsg("https://docs.google.com/forms/d/" + docID + "/edit"), []string{"form/" + docID + " "}},
		{"form responder", textMsg("https://docs.google.com/forms/d/e/" + docID + "/viewform"), []string{"form/" + docID + " "}},
		{"drawing", textMsg("https://docs.google.com/drawings/d/" + docID + "/edit"), []string{"drawing/" + docID + " "}},
		{"file", textMsg("https://drive.google.com/file/d/" + fileID + "/view?usp=drive_link"), []string{"file/" + fileID + " "}},
		{"folder", textMsg("https://drive.google.com/drive/folders/" + folderID + "?usp=sharing"), []string{"folder/" + folderID + " "}},
		{"folder u/1", textMsg("https://drive.google.com/drive/u/1/folders/" + folderID), []string{"folder/" + folderID + " "}},
		{"u/0", textMsg("https://docs.google.com/document/u/0/d/" + docID + "/edit"), []string{"doc/" + docID + " "}},
		{"open?id", textMsg("https://drive.google.com/open?id=" + fileID), []string{"file/" + fileID + " "}},
		{"docs open?id", textMsg("https://docs.google.com/open?id=" + fileID), []string{"file/" + fileID + " "}},
		{"trailing punctuation", textMsg("(https://drive.google.com/open?id=" + fileID + ")."), []string{"file/" + fileID + " "}},
		{"angle-bracketed", textMsg("Plan <https://docs.google.com/document/d/" + docID + "/edit>"), []string{"doc/" + docID + " "}},

		{"entity-escaped href", htmlMsg("", `<a href="https://drive.google.com/open?usp=x&amp;id=`+fileID+`&amp;authuser=0">Budget.xlsx</a>`),
			[]string{"file/" + fileID + " Budget.xlsx"}},
		{"anchor title", htmlMsg("", `<a class="x" href='https://docs.google.com/document/d/`+docID+`/edit'><img src="i.png"> <span>Q3 &amp; Q4
			plan</span></a>`), []string{"doc/" + docID + " Q3 & Q4 plan"}},
		{"url as anchor text", htmlMsg("", `<a href="https://docs.google.com/document/d/`+docID+`/edit">https://docs.google.com/document/d/`+docID+`/edit</a>`),
			[]string{"doc/" + docID + " "}},
		{"button text skipped", htmlMsg("", `<a href="https://docs.google.com/document/d/`+docID+`/edit?a=1">Open</a> <a href="https://docs.google.com/document/d/`+docID+`/edit?a=2">Launch notes</a>`),
			[]string{"doc/" + docID + " Launch notes"}},
		{"long title capped", htmlMsg("", `<a href="https://docs.google.com/document/d/`+docID+`/edit">`+strings.Repeat("é", 100)+`</a>`),
			[]string{"doc/" + docID + " " + strings.Repeat("é", 79) + "…"}},
		{"data-href is not href", htmlMsg("", `<a data-href="https://docs.google.com/document/d/`+docID+`/edit" href="#">Decoy</a> https://docs.google.com/document/d/`+docID),
			[]string{"doc/" + docID + " "}},

		{"short id", textMsg("https://docs.google.com/document/d/abc123/edit"), nil},
		{"bad id chars", textMsg("https://docs.google.com/document/d/abc.def.ghi.jkl.mno/edit"), nil},
		{"no id", textMsg("https://docs.google.com/document/d/"), nil},
		{"open without id", textMsg("https://drive.google.com/open?usp=sharing"), nil},
		{"drive home", textMsg("https://drive.google.com/drive/my-drive"), nil},
		{"lookalike host", textMsg("https://docs.google.com.evil.com/document/d/" + docID + "/edit"), nil},
		{"host in path", textMsg("https://evil.com/docs.google.com/document/d/" + docID + "/edit"), nil},
		{"subdomain", textMsg("https://xdocs.google.com/document/d/" + docID + "/edit"), nil},
		{"userinfo", textMsg("https://docs.google.com@evil.com/document/d/" + docID), nil},
		{"lookalike href", htmlMsg("", `<a href="https://drive.google.com.evil.example/file/d/`+fileID+`/view">Invoice</a>`), nil},
		{"attachment skipped", &notmuch.Message{Body: []notmuch.Part{{ID: 1, ContentType: "text/plain", Filename: "links.txt", Content: "https://docs.google.com/document/d/" + docID, HasContent: true}}}, nil},

		{"deduped in order", htmlMsg(
			"https://docs.google.com/spreadsheets/d/"+sheetID+"/edit and https://docs.google.com/document/d/"+docID+"/edit and again https://docs.google.com/spreadsheets/u/0/d/"+sheetID,
			`<a href="https://docs.google.com/document/d/`+docID+`/edit?usp=sharing">Plan</a>`),
			[]string{"sheet/" + sheetID + " ", "doc/" + docID + " Plan"}},
		{"same id, two kinds", textMsg("https://docs.google.com/document/d/" + docID + " https://drive.google.com/file/d/" + docID),
			[]string{"doc/" + docID + " ", "file/" + docID + " "}},
	}
	for _, c := range cases {
		if got := refKeys(driveRefs(c.m)); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// The link is rebuilt from kind and id; nothing of the sender's URL survives.
func TestDriveViews(t *testing.T) {
	m := textMsg("https://docs.google.com/document/u/0/d/" + docID + "/edit?usp=sharing&evil=1#x " +
		"https://docs.google.com/spreadsheets/d/" + sheetID + " https://docs.google.com/presentation/d/" + docID +
		" https://docs.google.com/forms/d/" + docID + " https://docs.google.com/forms/d/e/" + sheetID + "/viewform?x=1" +
		" https://docs.google.com/drawings/d/" + docID + " https://drive.google.com/open?id=" + fileID +
		" https://drive.google.com/drive/folders/" + folderID)
	var got []string
	for _, v := range driveViews(driveRefs(m), "robin+work@northwind.example") {
		got = append(got, v.Href)
		if v.Named || v.Title != v.Label {
			t.Errorf("%s: untitled chip %+v", v.Key, v)
		}
	}
	q := "?authuser=robin%2Bwork%40northwind.example"
	want := []string{
		"https://docs.google.com/document/d/" + docID + "/edit" + q,
		"https://docs.google.com/spreadsheets/d/" + sheetID + "/edit" + q,
		"https://docs.google.com/presentation/d/" + docID + "/edit" + q,
		"https://docs.google.com/forms/d/" + docID + "/edit" + q,
		"https://docs.google.com/forms/d/e/" + sheetID + "/viewform" + q,
		"https://docs.google.com/drawings/d/" + docID + "/edit" + q,
		"https://drive.google.com/file/d/" + fileID + "/view" + q,
		"https://drive.google.com/drive/folders/" + folderID + q,
	}
	if !slices.Equal(got, want) {
		t.Errorf("hrefs:\n got %q\nwant %q", got, want)
	}
	if v := driveViews([]driveRef{{Kind: kindDoc, ID: docID}}, ""); v[0].Href != "https://docs.google.com/document/d/"+docID+"/edit" {
		t.Errorf("no email: %s", v[0].Href)
	}
}

func TestFirstMentions(t *testing.T) {
	seen := map[string]bool{}
	a := driveViews([]driveRef{{Kind: kindDoc, ID: docID}, {Kind: kindSheet, ID: sheetID}}, "")
	b := driveViews([]driveRef{{Kind: kindSheet, ID: sheetID}, {Kind: kindFile, ID: fileID}}, "")
	if got := firstMentions(a, seen); len(got) != 2 {
		t.Errorf("first: %v", got)
	}
	if got := firstMentions(b, seen); len(got) != 1 || got[0].Kind != "file" {
		t.Errorf("second: %v", got)
	}
}

var driveChipRE = regexp.MustCompile(`<a href="([^"]*)" target="_blank" rel="noopener noreferrer" class="drive" data-kind="([^"]*)">(.*?)</a>`)

// A Drive share gets its chip; the reply quoting it gets only the file it
// adds, not the quoted one again.
func TestThreadDriveChips(t *testing.T) {
	s := newServer(t)
	body := getOK(t, s, threadURL(t, s, "/all", `Document shared with you: "SSO rollout plan"`))
	articles := strings.Split(body, "<article ")[1:]
	if len(articles) != 2 {
		t.Fatalf("%d articles", len(articles))
	}
	var got [][]string
	for _, a := range articles {
		var chips []string
		for _, m := range driveChipRE.FindAllStringSubmatch(a, -1) {
			chips = append(chips, m[2]+" "+m[1]+" "+m[3])
		}
		got = append(got, chips)
	}
	q := "?authuser=robin%40northwind.example"
	want := [][]string{
		{"doc https://docs.google.com/document/d/" + docID + "/edit" + q + ` SSO rollout plan <span class="kind">Google Doc</span>`},
		{"sheet https://docs.google.com/spreadsheets/d/" + sheetID + "/edit" + q + " Google Sheet"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("chips:\n got %q\nwant %q", got, want)
	}
	if strings.Count(body, `<ul class="attachments drive-files">`) != 2 || strings.Contains(body, `<ul class="attachments">`) {
		t.Errorf("chip lists: %s", body)
	}
}
