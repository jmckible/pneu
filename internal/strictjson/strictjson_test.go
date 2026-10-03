package strictjson

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWhole(t *testing.T) {
	type got struct {
		a string
		n uint64
		b bool
	}
	parse := func(s string) (got, error) {
		var g got
		err := Whole([]byte(s), func(dec *json.Decoder, key string) error {
			var err error
			switch key {
			case "a":
				g.a, err = String(dec, 8)
			case "n":
				g.n, err = Uint(dec, 1000)
			case "b":
				g.b, err = Bool(dec)
			case "o":
				return Object(dec, func(k string) error {
					if k != "x" {
						return ErrUnknown
					}
					return Skip(dec)
				})
			default:
				return ErrUnknown
			}
			return err
		})
		return g, err
	}
	if g, err := parse(` {"a":"hi","n":1000,"b":true,"o":{"x":[1,{"y":2}]}} `); err != nil || g != (got{"hi", 1000, true}) {
		t.Fatalf("good: %+v %v", g, err)
	}
	for _, bad := range []string{
		``, `[]`, `{"a":"hi"`, `{"a":"hi"}{}`, `{"a":"hi"} x`,
		`{"a":"hi","a":"ho"}`, `{"A":"hi"}`, `{"z":1}`,
		`{"a":"123456789"}`, `{"a":1}`, `{"a":null}`,
		`{"n":1001}`, `{"n":-1}`, `{"n":1.5}`, `{"n":1e3}`, `{"n":"5"}`, `{"n":01}`, `{"n":null}`,
		`{"n":99999999999999999999999}`,
		`{"b":"true"}`, `{"b":null}`,
		`{"o":{"x":1,"x":2}}`, `{"o":{"q":1}}`, `{"o":[]}`,
	} {
		if _, err := parse(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestUintEdges(t *testing.T) {
	for _, c := range []struct {
		in  string
		max uint64
		ok  bool
	}{
		{"0", 0, true}, {"1", 0, false}, {"9", 5, false}, {"5", 5, true},
		{"18446744073709551615", ^uint64(0), true}, {"18446744073709551616", ^uint64(0), false},
	} {
		dec := json.NewDecoder(strings.NewReader(c.in))
		_, err := Uint(dec, c.max)
		if (err == nil) != c.ok {
			t.Errorf("Uint(%s, %d): %v", c.in, c.max, err)
		}
	}
}

func TestArrayBound(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`[1,2,3]`))
	if err := Array(dec, 2, func(int) error { return Skip(dec) }); err == nil {
		t.Fatal("over the bound accepted")
	}
	dec = json.NewDecoder(strings.NewReader(`[1,2]`))
	n := 0
	if err := Array(dec, 2, func(int) error { n++; return Skip(dec) }); err != nil || n != 2 {
		t.Fatal(err, n)
	}
}
