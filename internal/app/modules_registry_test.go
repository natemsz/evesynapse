//go:build !core_only

package app

import (
	"context"
	"evesynapse/internal/app/module"
	"strings"
	"testing"
)

// TestDefaultBuildEnablesModules: the default build compiles in every
// module, and a module's nav entry, page and sitemap line come from its
// registration.
func TestDefaultBuildEnablesModules(t *testing.T) {
	if !moduleCompiledIn("piplanner") {
		t.Fatal("the default build does not register the PI planner")
	}
	found := false
	for _, e := range sectionNav["industry"] {
		if e.Href == "/industry/pi/" {
			found = true
		}
	}
	if !found {
		t.Error("the industry nav has no /industry/pi/ entry in the default build")
	}
	if _, ok := registeredPages["piplanner.html"]; !ok {
		t.Error("piplanner.html is not a registered page")
	}

	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	sitemap, err := app.buildSitemap(context.Background(), "https://node.example.org")
	if err != nil {
		t.Fatalf("sitemap: %v", err)
	}
	if !strings.Contains(string(sitemap), "https://node.example.org/industry/pi/") {
		t.Error("sitemap omits /industry/pi/ in the default build")
	}
}

// TestRegisterModulesIsIdempotent: registering a module twice adds its
// nav entry once.
func TestRegisterModulesIsIdempotent(t *testing.T) {
	before := len(sectionNav["industry"])
	for _, m := range registeredModules {
		registerModules(registeredModules[:0:0])
		registerModules([]module.Module{m})
	}
	if got := len(sectionNav["industry"]); got != before {
		t.Fatalf("industry nav grew from %d to %d on re-registering", before, got)
	}
}
