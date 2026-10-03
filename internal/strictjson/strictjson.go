// Package strictjson reads JSON by token, for files and replies whose
// every field pneu bounds: encoding/json would keep the last of a
// duplicate key, match keys case-insensitively, take null as "leave it",
// and stop after the first value. Nested objects go through the same
// reader, so a duplicate key is refused at any depth the caller walks.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrUnknown is what a field func returns for a key it doesn't take.
var ErrUnknown = errors.New("unknown field")

// Object reads one JSON object from dec, calling field once per key with
// the decoder positioned at its value; field must consume the value (Value,
// String, Skip, or a nested Object). A key seen twice is refused before
// field sees it again. Keys are compared exactly.
func Object(dec *json.Decoder, field func(key string) error) error {
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := t.(string)
		if !ok {
			return errors.New("object key isn't a string")
		}
		if seen[key] {
			return fmt.Errorf("field %q twice", key)
		}
		seen[key] = true
		if err := field(key); err != nil {
			if errors.Is(err, ErrUnknown) {
				return fmt.Errorf("unknown field %q", key)
			}
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errors.New("unterminated object")
	}
	return nil
}

// Array reads one JSON array from dec, calling elem once per element with
// the decoder positioned at it; at most max elements (max < 0: no bound).
func Array(dec *json.Decoder, max int, elem func(i int) error) error {
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return errors.New("not a JSON array")
	}
	for i := 0; dec.More(); i++ {
		if max >= 0 && i >= max {
			return fmt.Errorf("more than %d elements", max)
		}
		if err := elem(i); err != nil {
			return fmt.Errorf("[%d]: %w", i, err)
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim(']') {
		return errors.New("unterminated array")
	}
	return nil
}

// Whole reads b as exactly one object through Object, and nothing after
// it but whitespace.
func Whole(b []byte, field func(dec *json.Decoder, key string) error) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := Object(dec, func(key string) error { return field(dec, key) }); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the object")
	}
	return nil
}

// Value decodes the next value into dst, refusing null. dst should be a
// scalar or a type with no nested objects whose duplicate keys matter.
func Value(dec *json.Decoder, dst any) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if bytes.Equal(raw, []byte("null")) {
		return errors.New("null")
	}
	return json.Unmarshal(raw, dst)
}

// String decodes the next value as a string of at most max bytes.
func String(dec *json.Decoder, max int) (string, error) {
	t, err := dec.Token()
	if err != nil {
		return "", err
	}
	s, ok := t.(string)
	if !ok {
		return "", errors.New("not a string")
	}
	if len(s) > max {
		return "", fmt.Errorf("over %d bytes", max)
	}
	return s, nil
}

// Bool decodes the next value as a boolean.
func Bool(dec *json.Decoder) (bool, error) {
	t, err := dec.Token()
	if err != nil {
		return false, err
	}
	b, ok := t.(bool)
	if !ok {
		return false, errors.New("not a boolean")
	}
	return b, nil
}

// Uint decodes the next value as a non-negative integer of at most max:
// a JSON number with no sign, fraction or exponent.
func Uint(dec *json.Decoder, max uint64) (uint64, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return 0, err
	}
	s := string(raw)
	if s == "" || len(s) > 20 || (len(s) > 1 && s[0] == '0') {
		return 0, errors.New("not a plain non-negative integer")
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("not a plain non-negative integer")
		}
		d := uint64(c - '0')
		if d > max || v > (max-d)/10 {
			return 0, fmt.Errorf("over %d", max)
		}
		v = v*10 + d
	}
	return v, nil
}

// Skip consumes the next value, whatever it is.
func Skip(dec *json.Decoder) error {
	var raw json.RawMessage
	return dec.Decode(&raw)
}
