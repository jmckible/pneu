package unsub

import (
	"slices"
	"strings"
	"testing"
)

func raws(items []Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Raw)
	}
	return out
}

func TestParseList(t *testing.T) {
	long := "https://a.example/" + strings.Repeat("x", maxItemLen)
	cases := []struct {
		name, in string
		want     []string
	}{
		{"one", " <https://a.example/u>", []string{"https://a.example/u"}},
		{"two", "<mailto:u@a.example>, <https://a.example/u>", []string{"mailto:u@a.example", "https://a.example/u"}},
		{"comment", "<mailto:u@a.example> (by mail), <https://a.example/u> (web)", []string{"mailto:u@a.example", "https://a.example/u"}},
		{"nested comment", "<https://a.example/u> (a (b) \\) c)", []string{"https://a.example/u"}},
		{"whitespace in brackets", "< https://a.example/ u\t>", []string{"https://a.example/u"}},
		{"no space after comma", "<https://a.example/1>,<https://a.example/2>", []string{"https://a.example/1", "https://a.example/2"}},
		{"trailing comma", "<https://a.example/1>, ", []string{"https://a.example/1"}},
		{"malformed stops list", "<https://a.example/1>, https://a.example/2, <https://a.example/3>", []string{"https://a.example/1"}},
		{"garbage after item", "<https://a.example/1> x, <https://a.example/2>", []string{}},
		{"leading comment", "(hi) <https://a.example/1>", []string{}},
		{"unclosed bracket", "<https://a.example/1", []string{}},
		{"unclosed comment", "<https://a.example/1> (oops, <https://a.example/2>", []string{}},
		{"nested bracket", "<https://a<.example/1>", []string{}},
		{"empty item", "<>, <https://a.example/1>", []string{}},
		{"too long", "<" + long + ">, <https://a.example/1>", []string{}},
		{"bare", "https://a.example/1", []string{}},
		{"empty", "", []string{}},
	}
	for _, c := range cases {
		if got := raws(ParseList(c.in)); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	var many []string
	for i := range 12 {
		many = append(many, "<https://a.example/"+string(rune('a'+i))+">")
	}
	if n := len(ParseList(strings.Join(many, ", "))); n != maxItems {
		t.Errorf("%d items kept, want %d", n, maxItems)
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"https://a.example/u?x=1":        KindHTTPS,
		"HTTPS://A.example/u":            KindHTTPS,
		"http://a.example/u":             KindHTTP,
		"https://a.example:8443/u":       KindHTTPS, // valid; one-click refuses the port
		"mailto:u@a.example":             KindMailto,
		"https://user@a.example/u":       KindNone,
		"https://user:pw@a.example/u":    KindNone,
		"https://127.0.0.1/u":            KindNone,
		"https://[::1]/u":                KindNone,
		"https://2130706433/u":           KindNone,
		"https://0x7f.1/u":               KindNone,
		"https://127.1/u":                KindNone,
		"https://10.0.0.1./u":            KindNone,
		"https:///u":                     KindNone,
		"https://a%2eexample/u":          KindNone,
		"https://exämple.com/u":          KindNone,
		"https://a.example/\x01":         KindNone,
		"https://a.example/\x7f":         KindNone,
		"ftp://a.example/u":              KindNone,
		"javascript:alert(1)":            KindNone,
		"https:opaque":                   KindNone,
		"mailto:u@a.example?cc=x@b.test": KindNone,
	}
	for raw, want := range cases {
		if got := classify(0, raw).Kind; got != want {
			t.Errorf("%q: %q want %q", raw, got, want)
		}
	}
	if !classify(0, "https://user@a.example/").Web || classify(0, "mailto:x@y.example").Web {
		t.Error("Web flag")
	}
}

func TestParseMailto(t *testing.T) {
	type want struct {
		ok                bool
		to, subject, body string
	}
	cases := map[string]want{
		"mailto:u@a.example":                                             {true, "u@a.example", "unsubscribe", ""},
		"MAILTO:u@a.example?Subject=stop":                                {true, "u@a.example", "stop", ""},
		"mailto:u+tag@a.example?subject=a+b":                             {true, "u+tag@a.example", "a+b", ""},
		"mailto:u@a.example?subject=a%20b":                               {true, "u@a.example", "a b", ""},
		"mailto:u@a.example?subject=%2525":                               {true, "u@a.example", "%25", ""},
		"mailto:u%40a.example":                                           {true, "u@a.example", "unsubscribe", ""},
		"mailto:u@a.example?body=line1%0D%0Aline2%0D":                    {true, "u@a.example", "unsubscribe", "line1\nline2\n"},
		"mailto:u@a.example?subject=x%0D%0ABcc:%20evil@b.test":           {true, "u@a.example", "xBcc: evil@b.test", ""},
		"mailto:u@a.example?subject=a%00b":                               {true, "u@a.example", "ab", ""},
		"mailto:u@a.example?subject=":                                    {true, "u@a.example", "", ""},
		"mailto:u@a.example?subject=%20%20stop%20":                       {true, "u@a.example", "  stop ", ""},
		"mailto:u@a.example?body=x":                                      {true, "u@a.example", "unsubscribe", "x"},
		"mailto:" + strings.Repeat("l", 64) + "@a.example":               {true, strings.Repeat("l", 64) + "@a.example", "unsubscribe", ""},
		"mailto:" + strings.Repeat("l", 65) + "@a.example":               {},
		"mailto:u@" + strings.Repeat("d", 240) + ".example":              {true, "u@" + strings.Repeat("d", 240) + ".example", "unsubscribe", ""},
		"mailto:u@" + strings.Repeat("d", 245) + ".example":              {},
		"mailto:u@a.example?subject=%E2%9C%93":                           {true, "u@a.example", "✓", ""},
		"mailto:u@a.example?subject=%FF":                                 {false, "", "", ""},
		"mailto:u@a.example?cc=v@a.example":                              {},
		"mailto:u@a.example?bcc=v@a.example":                             {},
		"mailto:u@a.example?to=v@a.example":                              {},
		"mailto:?to=v@a.example":                                         {},
		"mailto:u@a.example?in-reply-to=%3Cx@y%3E":                       {},
		"mailto:u@a.example?x-anything=1":                                {},
		"mailto:u@a.example?subject=a&subject=b":                         {},
		"mailto:u@a.example?subject=a&SUBJECT=b":                         {},
		"mailto:u@a.example?s%75bject=a&subject=b":                       {},
		"mailto:u@a.example,v@a.example":                                 {},
		"mailto:u@a.example%2Cv@a.example":                               {},
		"mailto:":                                                        {},
		"mailto:u@a.example?subject":                                     {},
		"mailto:u@a.example?subject=%zz":                                 {},
		"mailto:u@a.example?subject=%2":                                  {},
		"mailto:u@a.example#frag":                                        {},
		"mailto:u%0D%0A@a.example":                                       {},
		"mailto:%22a%20b%22@a.example":                                   {},
		"mailto:list:;":                                                  {},
		"mailto:u@a.example?subject=" + strings.Repeat("x", maxHfield+1): {},
		"mailto:u@a.example?body=" + strings.Repeat("%41", maxHfield+1):  {},
	}
	for raw, w := range cases {
		m, err := ParseMailto(raw)
		if (err == nil) != w.ok {
			t.Errorf("%q: err %v, want ok=%v", raw, err, w.ok)
			continue
		}
		if w.ok && (m.To != w.to || m.Subject != w.subject || m.Body != w.body) {
			t.Errorf("%q: got %+v want %+v", raw, m, w)
		}
	}
}
