package link

import (
	"slices"
	"testing"

	"github.com/jmckible/pneu/internal/control"
)

// control.LinkCodes is what a Situation reply may say; it must be exactly
// the link's reasons.
func TestLinkCodes(t *testing.T) {
	all := []Reason{Starting, Up, TailscaleDown, NodeOffline, NodeMismatch, Refused, PinMismatch, NotPaired, Protocol}
	if len(all) != len(control.LinkCodes) {
		t.Fatalf("%d reasons, %d codes", len(all), len(control.LinkCodes))
	}
	for _, r := range all {
		if !slices.Contains(control.LinkCodes, string(r)) {
			t.Errorf("%s isn't in control.LinkCodes", r)
		}
	}
}
