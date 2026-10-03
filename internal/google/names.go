package google

import (
	"regexp"
	"strings"
)

var (
	projectRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	clientIDRE = regexp.MustCompile(`^[0-9]{1,32}-[a-z0-9]{1,64}\.apps\.googleusercontent\.com$`)
	// resourceRE is "pneu-" and an account name (config.ValidName's rule),
	// which is also a valid Pub/Sub topic and subscription ID.
	resourceRE = regexp.MustCompile(`^pneu-[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	addressRE  = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]{1,63}(\.[A-Za-z0-9-]{1,63})+$`)
	subRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	installRE  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ValidProject reports whether p is a GCP project ID (D1).
func ValidProject(p string) bool { return projectRE.MatchString(p) }

// ValidClientID reports whether id is an OAuth client ID's shape.
func ValidClientID(id string) bool { return clientIDRE.MatchString(id) }

// ValidSecret reports whether s can be a client secret: printable ASCII
// with no space, at most 256 bytes.
func ValidSecret(s string) bool { return printable(s, 256) }

// ValidAddress reports whether a is a plain email address pneu will hold
// and show: ASCII letters, digits and ._%+- before one @, a dotted domain,
// at most 254 bytes. Compare addresses with SameAddress.
func ValidAddress(a string) bool { return len(a) <= 254 && addressRE.MatchString(a) }

// SameAddress compares two addresses as Gmail does, ignoring case.
func SameAddress(a, b string) bool { return strings.EqualFold(a, b) }

// ValidSub reports whether s can be an ID token's subject: Google's are
// decimal; anything outside a short token alphabet is refused.
func ValidSub(s string) bool { return subRE.MatchString(s) }

// ValidInstall reports whether s is an install id: 16 lowercase hex,
// usable as a Pub/Sub label value.
func ValidInstall(s string) bool { return installRE.MatchString(s) }

// Token bounds: Google documents refresh tokens up to 512 bytes and
// access tokens up to 2048; pneu allows twice that.
const (
	MaxRefresh = 1024
	MaxAccess  = 4096
)

// ValidRefresh reports whether r can be a refresh token pneu stores.
func ValidRefresh(r string) bool { return printable(r, MaxRefresh) }

// printable: 1..max bytes of printable ASCII, no space.
func printable(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

// Resource is the topic and subscription ID for an account: "pneu-" and
// its name. ok is false for a name that doesn't make a valid ID (anything
// config.ValidName refuses).
func Resource(account string) (string, bool) {
	r := "pneu-" + account
	return r, resourceRE.MatchString(r)
}

// PublisherMember is the identity Gmail publishes watch notifications as.
const PublisherMember = "serviceAccount:gmail-api-push@system.gserviceaccount.com"

// PublisherRole is the role it needs on each topic.
const PublisherRole = "roles/pubsub.publisher"

// InstallLabel is the subscription label naming the server that owns it
// (K9).
const InstallLabel = "pneu-install"
