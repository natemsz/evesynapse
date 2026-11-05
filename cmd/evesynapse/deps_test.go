package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestReleaseBinaryLeavesOutTestCode: the release server links no test
// support and no dev-only routes. Each of these got in once through a
// helper file in internal/app that was not a _test.go file; go list
// sees what the compiler will link, so this catches the next one.
func TestReleaseBinaryLeavesOutTestCode(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := map[string]bool{}
	for p := range strings.FieldsSeq(string(out)) {
		deps[p] = true
	}
	for _, banned := range []string{
		"testing",
		"evesynapse/internal/pgtest",
		"github.com/fergusstrange/embedded-postgres",
		"evesynapse/internal/devtools",
	} {
		if deps[banned] {
			t.Errorf("the release binary links %s", banned)
		}
	}
}
