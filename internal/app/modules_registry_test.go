//go:build !core_only

package app

import (
	"context"
	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"strings"
	"testing"
)

// TestDefaultBuildEnablesModules: the default build compiles in every
// module, and a module's nav entry, page and sitemap line come from its
// registration.
func TestDefaultBuildEnablesModules(t *testing.T) {
	for _, id := range []string{"piplanner", "intel"} {
		if !moduleCompiledIn(id) {
			t.Fatalf("the default build does not register %s", id)
		}
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

// TestModuleDeclarations: every registered module declares all five
// things, requests only scopes the developer-portal application is
// enabled for (a scope outside that list kills the login for everyone),
// and the core scope surfaces read its declaration from the registry.
func TestModuleDeclarations(t *testing.T) {
	portal := map[string]bool{}
	for _, s := range eveScopes {
		portal[s] = true
	}
	known := map[string]bool{}
	for _, m := range registeredModules {
		known[m.ID] = true
	}
	for _, m := range moduleManifest {
		known[m.ID] = true
	}
	for _, m := range registeredModules {
		if m.Title == "" || m.Requires == nil || m.Scopes == nil || m.EnhanceScopes == nil {
			t.Errorf("module %q leaves a declaration out: %+v", m.ID, m)
		}
		for _, id := range m.Requires {
			if !known[id] {
				t.Errorf("module %q requires %q, which is not a module", m.ID, id)
			}
		}
		for _, s := range append(append([]module.Scope{}, m.Scopes...), m.EnhanceScopes...) {
			if !portal[s.Scope] {
				t.Errorf("module %q asks for %q, which the portal application is not enabled for", m.ID, s.Scope)
			}
			if s.Unlocks == "" {
				t.Errorf("module %q scope %q says nothing about what it unlocks", m.ID, s.Scope)
			}
		}
	}

	// The PI planner needs no scope; its colonies prefill is the
	// enhancement, read back through the manifest surfaces.
	def, ok := moduleByID("piplanner")
	if !ok || def.Title != "PI Planner" {
		t.Fatalf("moduleByID(piplanner) = %+v, %v", def, ok)
	}
	if got := def.requiredScopes(); len(got) != 0 {
		t.Errorf("piplanner requires scopes %v, want none", got)
	}
	st := def.statusFor(map[string]bool{})
	if st.State != moduleLimited || len(st.MissingOptional) != 1 || st.MissingOptional[0].Scope != planetScope {
		t.Errorf("piplanner without the planets scope: %+v, want limited by %s", st, planetScope)
	}
	if st := def.statusFor(map[string]bool{planetScope: true}); st.State != moduleEnabled {
		t.Errorf("piplanner with the planets scope: %+v, want enabled", st)
	}
	if _, ok := characterModuleStates(db.Character{})["piplanner"]; !ok {
		t.Error("characterModuleStates omits the registered module")
	}
}

// TestRegisterRejectsAnUndeclaredModule: a module that omits a
// declaration cannot register.
func TestRegisterRejectsAnUndeclaredModule(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a module with no Title and nil lists registered")
		}
	}()
	registerModules([]module.Module{{ID: "half-declared"}})
}
