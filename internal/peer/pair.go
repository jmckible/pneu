package peer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxAddRequest bounds `pneu peer add --stdin`'s input.
const MaxAddRequest = 16 << 10

// AddRequest is what a client sends at pairing, one JSON object on stdin
// (docs/client.md, "Pairing"): never arguments, which SSH would join into
// a shell line.
type AddRequest struct {
	Name   string `json:"name"`
	Node   string `json:"node"`
	Origin string `json:"origin"`
	Cert   string `json:"cert"` // PEM
}

// AddResult is what `pneu peer add` prints on stdout for the client to pin.
type AddResult struct {
	Cert     string `json:"cert"`     // the server's certificate, PEM
	Node     string `json:"node"`     // the server's Tailscale StableID
	Port     int    `json:"port"`     // the peer port
	Protocol int    `json:"protocol"` // web.Protocol
	Name     string `json:"name"`     // the name the client was paired under
	// Applied says whether the running server took it: "live", "next-start"
	// (no server running) or "pending" (asked, no acknowledgment).
	Applied string `json:"applied"`
}

// ParseAddRequest reads exactly one JSON object of the four known fields,
// each once, each a string, spelled exactly (encoding/json would match
// "Name" to name), at most MaxAddRequest bytes, and returns it with the
// record it pairs (Added unset). Nothing here is lenient.
func ParseAddRequest(r io.Reader) (AddRequest, Record, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxAddRequest+1))
	if err != nil {
		return AddRequest{}, Record{}, err
	}
	if len(b) > MaxAddRequest {
		return AddRequest{}, Record{}, fmt.Errorf("request over %d bytes", MaxAddRequest)
	}
	req, err := decodeAddRequest(b)
	if err != nil {
		return AddRequest{}, Record{}, fmt.Errorf("request: %w", err)
	}
	if !ValidName(req.Name) {
		return AddRequest{}, Record{}, errors.New("request: name must be a lowercase DNS label of at most 32 characters")
	}
	if !ValidNode(req.Node) {
		return AddRequest{}, Record{}, errors.New("request: node isn't a Tailscale stable ID")
	}
	if !ValidOrigin(req.Origin) {
		return AddRequest{}, Record{}, errors.New("request: origin must be exactly http://pneu.localhost:<port>")
	}
	c, err := ParseCertPEM(req.Cert)
	if err != nil {
		return AddRequest{}, Record{}, fmt.Errorf("request: %w", err)
	}
	return req, Record{Name: req.Name, Node: req.Node, SPKI: SPKI(c), Origin: req.Origin}, nil
}

// decodeAddRequest walks the tokens itself: '{', then exactly the keys
// name, node, origin and cert, once each, each with a string value, then
// '}' and the end of input.
func decodeAddRequest(b []byte) (AddRequest, error) {
	var req AddRequest
	fields := map[string]*string{"name": &req.Name, "node": &req.Node, "origin": &req.Origin, "cert": &req.Cert}
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return req, errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return req, err
		}
		k, _ := t.(string)
		dst, ok := fields[k]
		switch {
		case !ok:
			return req, fmt.Errorf("unknown field %q", k)
		case seen[k]:
			return req, fmt.Errorf("%q twice", k)
		}
		seen[k] = true
		v, err := dec.Token()
		if err != nil {
			return req, err
		}
		str, ok := v.(string)
		if !ok {
			return req, fmt.Errorf("%q isn't a string", k)
		}
		*dst = str
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return req, errors.New("unterminated object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return req, errors.New("trailing data after the object")
	}
	if len(seen) != len(fields) {
		return req, errors.New("name, node, origin and cert are all required")
	}
	return req, nil
}
