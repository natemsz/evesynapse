package app

import "testing"

func TestModuleManifestScopesAreRequested(t *testing.T) {
	requested := map[string]bool{}
	for _, s := range eveScopes {
		requested[s] = true
	}
	for _, m := range moduleManifest {
		for _, s := range m.Scopes {
			if !requested[s.Scope] {
				t.Errorf("module %q names scope %q that eveScopes does not request", m.ID, s.Scope)
			}
			if s.Unlocks == "" {
				t.Errorf("module %q scope %q has no Unlocks text", m.ID, s.Scope)
			}
		}
	}
}

func TestEveScopesBelongToAModule(t *testing.T) {
	owner := map[string]string{}
	for _, m := range moduleManifest {
		for _, s := range m.Scopes {
			if prev, dup := owner[s.Scope]; dup {
				t.Errorf("scope %q is in both %q and %q", s.Scope, prev, m.ID)
			}
			owner[s.Scope] = m.ID
		}
	}
	for _, s := range eveScopes {
		if owner[s] == "" {
			t.Errorf("eveScopes requests %q but no module owns it", s)
		}
	}
}

func TestModuleManifestShape(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range moduleManifest {
		if seen[m.ID] {
			t.Errorf("duplicate module id %q", m.ID)
		}
		seen[m.ID] = true
	}
	for _, m := range moduleManifest {
		switch m.Layer {
		case layerPublic, layerDerived:
			if len(m.Scopes) != 0 {
				t.Errorf("%s module %q must not list scopes", m.Layer, m.ID)
			}
		case layerCharacter, layerCorp:
			if len(m.requiredScopes()) == 0 {
				t.Errorf("%s module %q needs at least one required scope", m.Layer, m.ID)
			}
		default:
			t.Errorf("module %q has unknown layer %q", m.ID, m.Layer)
		}
		if (m.Layer == layerDerived) != (len(m.Uses) > 0) {
			t.Errorf("module %q: Uses must be set exactly on derived modules", m.ID)
		}
		for _, u := range m.Uses {
			if d, ok := moduleByID(u); !ok || d.Layer == layerDerived {
				t.Errorf("module %q uses %q, which is missing or itself derived", m.ID, u)
			}
		}
	}
}

func TestWidgetCatalogPointsAtManifest(t *testing.T) {
	for _, w := range homeWidgetCatalog {
		if _, ok := moduleByID(w.Module); !ok {
			t.Errorf("widget %q names unknown module %q", w.ID, w.Module)
		}
	}
}
