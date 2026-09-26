// Package hostile_test runs the browser harness over the hostile corpus.
// `go test ./...` skips testdata/, so run it by path: go test ./testdata/hostile
package hostile_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestHostileCorpusInChromium(t *testing.T) {
	if testing.Short() {
		t.Skip("launches headless chromium")
	}
	if os.Getenv("CHROMIUM") == "" {
		if _, err := exec.LookPath("chromium"); err != nil {
			t.Skip("chromium not installed")
		}
	}
	for _, bin := range []string{"python3", "openssl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	out, err := exec.Command("./run.sh").CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("run.sh: %v", err)
	}
}
