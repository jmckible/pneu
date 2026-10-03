package google

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jmckible/pneu/internal/strictjson"
)

// Credentials are the push project's own OAuth client (D1), from the
// client JSON `pneu push init` copied in. Never lieer's.
type Credentials struct {
	ID     string
	Secret string
}

func (c Credentials) String() string   { return "google.Credentials{" + c.ID + "}" }
func (c Credentials) GoString() string { return c.String() }

// Kind is which of push's two tokens a consent or refresh is for (D2).
type Kind int

const (
	// Owner: the push project's owner. Pub/Sub, and its identity from the
	// ID token's sub.
	Owner Kind = iota + 1
	// Mailbox: one pushed account's mailbox. Gmail metadata only.
	Mailbox
)

func (k Kind) String() string {
	switch k {
	case Owner:
		return "owner"
	case Mailbox:
		return "mailbox"
	}
	return "unknown"
}

// The scopes each kind asks for.
const (
	ScopeOpenID   = "openid"
	ScopeEmail    = "email"
	ScopePubSub   = "https://www.googleapis.com/auth/pubsub"
	ScopeMetadata = "https://www.googleapis.com/auth/gmail.metadata"
	// scopeUserinfoEmail is how a token answer names the email scope.
	scopeUserinfoEmail = "https://www.googleapis.com/auth/userinfo.email"
)

// Scopes is what a consent for kind asks for.
func (k Kind) Scopes() []string {
	switch k {
	case Owner:
		return []string{ScopeOpenID, ScopeEmail, ScopePubSub}
	case Mailbox:
		return []string{ScopeMetadata}
	}
	return nil
}

// granted is the set a token answer for kind must carry, exactly. Google
// answers the "email" scope by its long name; both spellings are the same
// scope, so they're folded together before comparing.
func (k Kind) granted() []string {
	s := slices.Clone(k.Scopes())
	for i, v := range s {
		if v == ScopeEmail {
			s[i] = scopeUserinfoEmail
		}
	}
	slices.Sort(s)
	return s
}

// Redirect is where Google sends the consent's answer: lieer's fixed
// port, which pneu binds the same way (D3).
const Redirect = "http://localhost:8080/"

// Consent is one consent session: the state, PKCE verifier and nonce it
// was opened with, its client and redirect. It's what the callback is
// checked against and what the code is exchanged with; nothing else
// holds them.
type Consent struct {
	creds    Credentials
	kind     Kind
	hint     string
	state    string
	verifier string
	nonce    string // owner only
}

func (c Consent) String() string   { return "google.Consent{" + c.kind.String() + "}" }
func (c Consent) GoString() string { return c.String() }

// NewConsent opens a consent for kind with creds. loginHint is the address
// expected ("" for none); it preselects the account on Google's screen.
func NewConsent(creds Credentials, kind Kind, loginHint string) (*Consent, error) {
	switch {
	case !ValidClientID(creds.ID) || !ValidSecret(creds.Secret):
		return nil, errors.New("google: bad client credentials")
	case kind != Owner && kind != Mailbox:
		return nil, errors.New("google: bad consent kind")
	case loginHint != "" && !ValidAddress(loginHint):
		return nil, errors.New("google: bad login hint")
	}
	c := &Consent{creds: creds, kind: kind, hint: loginHint, state: random(16), verifier: random(32)}
	if kind == Owner {
		c.nonce = random(16)
	}
	return c, nil
}

// random is n bytes from crypto/rand, base64url without padding.
func random(n int) string {
	b := make([]byte, n)
	rand.Read(b) // never fails (crypto/rand)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Kind is the consent's kind.
func (c *Consent) Kind() Kind { return c.kind }

// State is the value the callback must echo (for the client relay's own
// check; the server's is Callback).
func (c *Consent) State() string { return c.state }

// URL is the consent screen to open (D3). It always passes
// gmi.ValidConsentURL.
func (c *Consent) URL() string {
	sum := sha256.Sum256([]byte(c.verifier))
	q := url.Values{
		"client_id":              {c.creds.ID},
		"redirect_uri":           {Redirect},
		"response_type":          {"code"},
		"scope":                  {strings.Join(c.kind.Scopes(), " ")},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"false"},
		"state":                  {c.state},
		"code_challenge":         {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method":  {"S256"},
	}
	if c.nonce != "" {
		q.Set("nonce", c.nonce)
	}
	if c.hint != "" {
		q.Set("login_hint", c.hint)
	}
	return AuthURL + "?" + q.Encode()
}

// Callback errors.
var (
	// ErrState: the callback isn't this consent's (its state differs).
	ErrState = errors.New("google: the callback isn't this consent's")
	// ErrDenied: Google answered with an error (the user declined, or the
	// client or an admin refused); its words are never read.
	ErrDenied = errors.New("google: consent was not granted")
	// ErrCallback: the callback carries no usable code.
	ErrCallback = errors.New("google: the callback carries no usable code")
)

// MaxCode bounds an authorization code.
const MaxCode = 512

// Callback checks a callback query (remote.ParseCallback's output, or the
// listener's own parse) against this consent: its state in constant time,
// then error (ErrDenied) or code. The scope it carries is never read.
func (c *Consent) Callback(q url.Values) (code string, err error) {
	st := q["state"]
	if len(st) != 1 || subtle.ConstantTimeCompare([]byte(st[0]), []byte(c.state)) != 1 {
		return "", ErrState
	}
	if _, ok := q["error"]; ok {
		return "", ErrDenied
	}
	codes := q["code"]
	if len(codes) != 1 || !printable(codes[0], MaxCode) {
		return "", ErrCallback
	}
	return codes[0], nil
}

// Identity is the owner as the ID token names them: sub is the identity
// pinned at init (K6); email is for showing.
type Identity struct {
	Sub   string
	Email string
}

// Token is one token answer, held to D2's rules.
type Token struct {
	Access  string
	Expiry  time.Time // by the API's clock: now + expires_in
	Refresh string    // an exchange's; "" from a refresh
	// Identity is the owner's, from an exchange's ID token (or a refresh's,
	// when Google sends one); nil for a mailbox.
	Identity *Identity
}

func (t Token) String() string   { return "google.Token{…}" }
func (t Token) GoString() string { return t.String() }

// MaxExpiresIn bounds a token's expires_in (D2: in (0, 24h]).
const MaxExpiresIn = 24 * 60 * 60

// Exchange trades a callback's code for tokens, with this consent's
// verifier and redirect. The answer must carry exactly the consent's
// scopes, a Bearer token_type, expires_in in (0, 24h], a refresh token,
// and for the owner an ID token whose iss, aud, exp and nonce check out.
func (a *API) Exchange(ctx context.Context, c *Consent, code string) (Token, error) {
	if !printable(code, MaxCode) {
		return Token{}, fail(OpExchange, CodeUnknown)
	}
	b, err := a.call(ctx, request{op: OpExchange, host: TokenHost, method: http.MethodPost, path: "/token",
		form: url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {c.verifier},
			"redirect_uri":  {Redirect},
			"client_id":     {c.creds.ID},
			"client_secret": {c.creds.Secret},
		}})
	if err != nil {
		return Token{}, err
	}
	return a.token(OpExchange, b, c.creds, c.kind, c.nonce, "")
}

// Refresh trades a stored refresh token for a fresh access token. The
// answer is held to the same rules as an exchange's (K7), except that it
// carries no refresh token to keep: Google doesn't rotate them, and one it
// sends anyway is dropped. For the owner, an ID token Google includes must
// name sub, the identity pinned at init.
func (a *API) Refresh(ctx context.Context, creds Credentials, kind Kind, refresh, sub string) (Token, error) {
	if !ValidRefresh(refresh) || (kind != Owner && kind != Mailbox) || (kind == Owner && !ValidSub(sub)) {
		return Token{}, fail(OpRefresh, CodeUnknown)
	}
	b, err := a.call(ctx, request{op: OpRefresh, host: TokenHost, method: http.MethodPost, path: "/token",
		form: url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refresh},
			"client_id":     {creds.ID},
			"client_secret": {creds.Secret},
		}})
	if err != nil {
		return Token{}, err
	}
	t, err := a.token(OpRefresh, b, creds, kind, "", sub)
	t.Refresh = ""
	return t, err
}

// tokenAnswer is the token endpoint's answer, as read.
type tokenAnswer struct {
	access, refresh, scope, tokenType, idToken string
	expiresIn                                  uint64
	hasExpires                                 bool
}

func parseTokenAnswer(b []byte) (tokenAnswer, error) {
	var t tokenAnswer
	err := strictjson.Whole(b, func(dec *json.Decoder, key string) error {
		var err error
		switch key {
		case "access_token":
			t.access, err = strictjson.String(dec, MaxAccess)
		case "refresh_token":
			t.refresh, err = strictjson.String(dec, MaxRefresh)
		case "scope":
			t.scope, err = strictjson.String(dec, 2048)
		case "token_type":
			t.tokenType, err = strictjson.String(dec, 32)
		case "id_token":
			t.idToken, err = strictjson.String(dec, 16<<10)
		case "expires_in":
			t.expiresIn, err = strictjson.Uint(dec, 1<<32)
			t.hasExpires = true
		default:
			return strictjson.Skip(dec) // refresh_token_expires_in and whatever Google adds
		}
		return err
	})
	return t, err
}

// token holds a token answer to D2's rules. nonce is the consent's (an
// exchange's ID token must carry it); sub is the pinned identity (a
// refresh's ID token must name it).
func (a *API) token(op Op, b []byte, creds Credentials, kind Kind, nonce, sub string) (Token, error) {
	t, err := parseTokenAnswer(b)
	if err != nil {
		return Token{}, fail(op, CodeUnknown)
	}
	if !sameScopes(t.scope, kind) {
		return Token{}, fail(op, CodeScope)
	}
	switch {
	case !strings.EqualFold(t.tokenType, "Bearer"),
		!t.hasExpires || t.expiresIn == 0 || t.expiresIn > MaxExpiresIn,
		!printable(t.access, MaxAccess),
		op == OpExchange && !ValidRefresh(t.refresh):
		return Token{}, fail(op, CodeUnknown)
	}
	now := a.now()
	tok := Token{Access: t.access, Refresh: t.refresh, Expiry: now.Add(time.Duration(t.expiresIn) * time.Second)}
	if kind == Owner {
		switch {
		case op == OpExchange && t.idToken == "":
			return Token{}, fail(op, CodeUnknown)
		case t.idToken != "":
			id, err := checkIDToken(t.idToken, creds.ID, nonce, sub, now)
			if err != nil {
				return Token{}, fail(op, CodeUnknown)
			}
			tok.Identity = &id
		}
	}
	return tok, nil
}

// sameScopes reports whether a token answer's space-separated scope is
// exactly kind's set: every one there, nothing else, none twice.
func sameScopes(scope string, kind Kind) bool {
	got := strings.Fields(scope)
	for i, s := range got {
		if s == ScopeEmail {
			got[i] = scopeUserinfoEmail
		}
	}
	slices.Sort(got)
	return slices.Equal(got, kind.granted())
}

// checkIDToken reads the owner's identity from an ID token received
// directly from the token endpoint over TLS. Its signature isn't checked:
// OpenID Connect Core 3.1.3.7 lets a client skip it for a token it got
// straight from the token endpoint over TLS, as here, since TLS already
// authenticates the issuer. The claims are: iss Google's, aud exactly our
// client (azp too, if present), exp in the future, nonce the consent's
// (an exchange) or sub the pinned one (a refresh), a valid sub, and a
// verified email.
func checkIDToken(raw, clientID, nonce, sub string, now time.Time) (Identity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Identity{}, errShape
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Identity{}, errShape
	}
	var (
		iss, azp, gotNonce string
		id                 Identity
		aud                []string
		exp                uint64
		verified, hasExp   bool
	)
	err = strictjson.Whole(payload, func(dec *json.Decoder, key string) error {
		var err error
		switch key {
		case "iss":
			iss, err = strictjson.String(dec, 64)
		case "azp":
			azp, err = strictjson.String(dec, 256)
		case "aud":
			aud, err = readAud(dec)
		case "sub":
			id.Sub, err = strictjson.String(dec, 255)
		case "email":
			id.Email, err = strictjson.String(dec, 254)
		case "email_verified":
			verified, err = readVerified(dec)
		case "nonce":
			gotNonce, err = strictjson.String(dec, 256)
		case "exp":
			exp, err = strictjson.Uint(dec, 1<<40)
			hasExp = true
		default:
			return strictjson.Skip(dec)
		}
		return err
	})
	if err != nil {
		return Identity{}, errShape
	}
	switch {
	case iss != "https://accounts.google.com" && iss != "accounts.google.com":
		return Identity{}, errors.New("iss")
	case len(aud) != 1 || aud[0] != clientID:
		return Identity{}, errors.New("aud")
	case azp != "" && azp != clientID:
		return Identity{}, errors.New("azp")
	case !hasExp || int64(exp) <= now.Unix():
		return Identity{}, errors.New("exp")
	case nonce != "" && subtle.ConstantTimeCompare([]byte(gotNonce), []byte(nonce)) != 1:
		return Identity{}, errors.New("nonce")
	case nonce == "" && sub == "":
		return Identity{}, errors.New("nothing to bind the token to")
	case !ValidSub(id.Sub):
		return Identity{}, errors.New("sub")
	case sub != "" && id.Sub != sub:
		return Identity{}, errors.New("sub isn't the pinned owner")
	case !ValidAddress(id.Email) || !verified:
		return Identity{}, errors.New("email")
	}
	return id, nil
}

// readAud reads aud: a string, or an array of strings (OIDC allows both).
func readAud(dec *json.Decoder) ([]string, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || len(many) > 8 {
		return nil, errShape
	}
	return many, nil
}

// readVerified reads email_verified: a boolean, or Google's older "true".
func readVerified(dec *json.Decoder) (bool, error) {
	t, err := dec.Token()
	if err != nil {
		return false, err
	}
	switch v := t.(type) {
	case bool:
		return v, nil
	case string:
		return v == "true", nil
	}
	return false, errShape
}
