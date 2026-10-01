package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// strictObject reads b as exactly one JSON object into fields, by token:
// every key in fields present exactly once, spelled exactly, no other key,
// no null, nothing after the object. encoding/json would keep the last of
// a duplicate key, match keys case-insensitively, take null as "leave it",
// and stop after the first value.
func strictObject(b []byte, fields map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		dst, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		if seen[key] {
			return fmt.Errorf("field %q twice", key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("%s: null", key)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errors.New("unterminated object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the object")
	}
	for k := range fields {
		if !seen[k] {
			return fmt.Errorf("field %q missing", k)
		}
	}
	return nil
}
