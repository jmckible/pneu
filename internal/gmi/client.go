package gmi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CleanClientSecret validates the OAuth client JSON Google Cloud gives for
// a Desktop-app client and returns it rebuilt from the validated fields
// alone, so nothing unchecked reaches lieer. It never includes the secret
// in an error.
//
// The whole file matters, not just "installed": google_auth_oauthlib's
// Flow.from_client_config uses "web" when both are present, so a clean
// "installed" beside a "web" whose token_uri is someone else's would send
// the authorization code and PKCE verifier there. Only a lone "installed"
// is accepted, and its auth_uri and token_uri must be Google's.
func CleanClientSecret(b []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, errors.New("not JSON: download the client's JSON from Google Cloud → Clients")
	}
	raw, ok := top["installed"]
	if !ok {
		return nil, errors.New(`not a Desktop-app client (no "installed" key): create the client as type Desktop app`)
	}
	for k := range top {
		if k != "installed" {
			return nil, fmt.Errorf("unexpected top-level key %q: a Desktop-app client's JSON has only \"installed\"", k)
		}
	}
	var c struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		AuthURI      string `json:"auth_uri"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errors.New(`"installed" isn't an object`)
	}
	if !strings.HasSuffix(c.ClientID, ".apps.googleusercontent.com") || c.ClientSecret == "" {
		return nil, errors.New("missing client_id or client_secret")
	}
	// lieer sends the consent to auth_uri and the code to token_uri, as
	// written in this file: they must be Google's.
	for _, u := range []struct{ name, uri, host string }{
		{"auth_uri", c.AuthURI, "accounts.google.com"},
		{"token_uri", c.TokenURI, "oauth2.googleapis.com"},
	} {
		p, err := url.Parse(u.uri)
		if err != nil || p.Scheme != "https" || p.Host != u.host || p.User != nil {
			return nil, fmt.Errorf("%s %q isn't Google's (want https://%s/...)", u.name, u.uri, u.host)
		}
	}
	return json.Marshal(map[string]map[string]string{"installed": {
		"client_id": c.ClientID, "client_secret": c.ClientSecret, "auth_uri": c.AuthURI, "token_uri": c.TokenURI,
	}})
}
