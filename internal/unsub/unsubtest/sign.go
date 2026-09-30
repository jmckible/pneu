// Package unsubtest signs test messages with DKIM (ed25519-sha256,
// relaxed/relaxed), written apart from unsub's verifier so the two check
// each other. Keys are generated in tests; nothing touches DNS.
package unsubtest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
)

// KeyRecord is the TXT record publishing pub.
func KeyRecord(pub ed25519.PublicKey) string {
	return "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)
}

var wsp = regexp.MustCompile(`[ \t]+`)

// Sign returns msg (LF line ends) with a DKIM-Signature by key for d= and
// s= over the colon-separated headers.
func Sign(msg string, key ed25519.PrivateKey, domain, selector, headers string) string {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	head, body, _ := strings.Cut(msg, "\n\n")

	// Relaxed body: per line, WSP runs to one SP, trailing WSP gone; then
	// trailing empty lines gone; CRLF line ends.
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(wsp.ReplaceAllString(l, " "), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	canonBody := ""
	if len(lines) > 0 {
		canonBody = strings.Join(lines, "\r\n") + "\r\n"
	}
	bh := sha256.Sum256([]byte(canonBody))

	// Header fields, unfolded, in order.
	var fields []string
	for _, l := range strings.Split(head, "\n") {
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(fields) > 0 {
			fields[len(fields)-1] += l
		} else {
			fields = append(fields, l)
		}
	}
	relaxed := func(f string) string {
		name, value, _ := strings.Cut(f, ":")
		value = strings.TrimSpace(wsp.ReplaceAllString(value, " "))
		return strings.ToLower(strings.TrimSpace(name)) + ":" + value
	}
	used := map[int]bool{}
	var data strings.Builder
	for _, h := range strings.Split(headers, ":") {
		for i := len(fields) - 1; i >= 0; i-- {
			name, _, _ := strings.Cut(fields[i], ":")
			if !used[i] && strings.EqualFold(strings.TrimSpace(name), h) {
				used[i] = true
				data.WriteString(relaxed(fields[i]) + "\r\n")
				break
			}
		}
	}
	sigField := "DKIM-Signature: v=1; a=ed25519-sha256; c=relaxed/relaxed; d=" + domain + "; s=" + selector +
		"; h=" + headers + "; bh=" + base64.StdEncoding.EncodeToString(bh[:]) + "; b="
	data.WriteString(relaxed(sigField))
	sum := sha256.Sum256([]byte(data.String()))
	sig := ed25519.Sign(key, sum[:])
	return sigField + base64.StdEncoding.EncodeToString(sig) + "\n" + msg
}
