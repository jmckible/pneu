package peer

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
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

// decodeAddRequest reads exactly the keys name, node, origin and cert.
func decodeAddRequest(b []byte) (AddRequest, error) {
	var req AddRequest
	err := decodeStrict(b, map[string]any{"name": &req.Name, "node": &req.Node, "origin": &req.Origin, "cert": &req.Cert})
	if err != nil && errors.Is(err, errMissing) {
		return req, errors.New("name, node, origin and cert are all required")
	}
	return req, err
}

var errMissing = errors.New("a field is missing")

// decodeStrict walks the tokens itself: '{', then exactly the keys of
// fields, once each, spelled exactly, each a string (*string) or an
// integer (*int), then '}' and the end of input. encoding/json would keep
// the last of a duplicate and match "Name" to name.
func decodeStrict(b []byte, fields map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		k, _ := t.(string)
		dst, ok := fields[k]
		switch {
		case !ok:
			return fmt.Errorf("unknown field %q", k)
		case seen[k]:
			return fmt.Errorf("%q twice", k)
		}
		seen[k] = true
		v, err := dec.Token()
		if err != nil {
			return err
		}
		switch d := dst.(type) {
		case *string:
			str, ok := v.(string)
			if !ok {
				return fmt.Errorf("%q isn't a string", k)
			}
			*d = str
		case *int:
			n, ok := v.(json.Number)
			if !ok {
				return fmt.Errorf("%q isn't a number", k)
			}
			i, err := strconv.Atoi(n.String())
			if err != nil {
				return fmt.Errorf("%q isn't an integer", k)
			}
			*d = i
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return errors.New("unterminated object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the object")
	}
	if len(seen) != len(fields) {
		return errMissing
	}
	return nil
}

// MaxAddResult bounds what the client reads of `pneu peer add`'s stdout.
const MaxAddResult = 16 << 10

// ParseAddResult is the client's strict reading of `pneu peer add`'s
// stdout (docs/client.md, "Pairing", step 4): at most MaxAddResult bytes,
// one JSON object of exactly AddResult's fields, a certificate that is one
// ECDSA P-256 certificate (ParseCertPEM), a StableID, a port, and an
// applied state it knows. The protocol is returned for the caller to
// compare: a mismatch has its own message.
func ParseAddResult(b []byte) (AddResult, *x509.Certificate, error) {
	var res AddResult
	if len(b) > MaxAddResult {
		return res, nil, fmt.Errorf("answer over %d bytes", MaxAddResult)
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	err := decodeStrict(b, map[string]any{
		"cert": &res.Cert, "node": &res.Node, "port": &res.Port, "protocol": &res.Protocol, "name": &res.Name, "applied": &res.Applied,
	})
	if errors.Is(err, errMissing) {
		return res, nil, errors.New("answer: cert, node, port, protocol, name and applied are all required")
	}
	if err != nil {
		return res, nil, fmt.Errorf("answer: %w", err)
	}
	c, err := ParseCertPEM(res.Cert)
	switch {
	case err != nil:
		return res, nil, fmt.Errorf("answer: %w", err)
	case !ValidNode(res.Node):
		return res, nil, errors.New("answer: node isn't a Tailscale stable ID")
	case res.Port < 1 || res.Port > 65535:
		return res, nil, fmt.Errorf("answer: bad port %d", res.Port)
	case !ValidName(res.Name):
		return res, nil, errors.New("answer: bad name")
	case res.Applied != "live" && res.Applied != "next-start" && res.Applied != "pending":
		return res, nil, errors.New("answer: unknown applied state")
	}
	return res, c, nil
}
