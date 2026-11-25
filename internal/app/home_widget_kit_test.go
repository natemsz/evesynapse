package app

import (
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestHomeWidgetDeclarationsHoldTheirContract: every registered home
// widget declares what the home needs to list, size, load and draw it
// without naming it anywhere else: a title, a description, a known
// category and manifest feature, a page to link to (the Briefing has
// none), a module that exists when it needs one, a default among its
// sizes, no size in two forms, a template for every form, and option
// defaults among their choices. The catalog lists them by category,
// then title.
func TestHomeWidgetDeclarationsHoldTheirContract(t *testing.T) {
	ts, err := homeTemplateSet()
	if err != nil {
		t.Fatalf("parse the home templates: %v", err)
	}
	category := make(map[string]int, len(widgetCategoryOrder))
	for i, c := range widgetCategoryOrder {
		category[c] = i
	}
	noPage := map[string]bool{widgetBriefing: true}
	if len(homeWidgetRegistry) == 0 {
		t.Fatal("no home widgets are registered")
	}
	for id, def := range homeWidgetRegistry {
		if def.ID != id {
			t.Errorf("widget %q is registered under %q", def.ID, id)
		}
		if def.Title == "" || def.Description == "" {
			t.Errorf("widget %q needs a title and a description", id)
		}
		if strings.Contains(def.Title+def.Description, "\u2014") {
			t.Errorf("widget %q has an em dash in its copy", id)
		}
		if _, ok := category[def.Category]; !ok {
			t.Errorf("widget %q has unknown category %q", id, def.Category)
		}
		if _, ok := moduleByID(def.Feature); !ok && def.builtIn() {
			t.Errorf("widget %q names unknown manifest feature %q", id, def.Feature)
		}
		if def.Href == "" && !noPage[id] {
			t.Errorf("widget %q has no page to link to", id)
		}
		if def.Href != "" && (!strings.HasPrefix(def.Href, "/") || !strings.HasSuffix(def.Href, "/")) {
			t.Errorf("widget %q links to %q, want a site path ending in /", id, def.Href)
		}
		if def.Requires != "" && len(registeredModules) > 0 && !moduleCompiledIn(def.Requires) {
			t.Errorf("widget %q requires module %q, which no module declares", id, def.Requires)
		}
		if !def.Allows(def.Default) {
			t.Errorf("widget %q default %s is not one of its sizes", id, def.Default)
		}
		seen := map[wSize]string{}
		for _, f := range def.Forms {
			if f.Says == "" {
				t.Errorf("widget %q form %q says nothing for the size picker", id, f.Name)
			}
			for _, raw := range strings.Fields(f.Sizes) {
				s, _ := parseWSize(raw)
				if other, dup := seen[s]; dup {
					t.Errorf("widget %q draws %s in both %q and %q", id, s, other, f.Name)
				}
				seen[s] = f.Name
			}
			if ts.Lookup("w-"+id+"-"+f.Name) == nil {
				t.Errorf("widget %q form %q has no template w-%s-%s", id, f.Name, id, f.Name)
			}
		}
		for _, o := range def.Options {
			if o.Label == "" || !o.allows(o.Default) {
				t.Errorf("widget %q option %q needs a label and a default among its choices", id, o.Key)
			}
		}
	}
	catalog := homeWidgetCatalog()
	for i := 1; i < len(catalog); i++ {
		a, b := catalog[i-1], catalog[i]
		if category[a.Category] > category[b.Category] ||
			(a.Category == b.Category && a.Title > b.Title) {
			t.Errorf("catalog lists %q before %q, want category then title order", a.ID, b.ID)
		}
	}
}

// TestWidgetSettingsFormsReadAsSent: the settings sheet's form reads
// back as the account set it. A toggle posts "no" ahead of its
// checkbox, so the last value wins; a multi posts an empty marker
// ahead of its picks; a field the form leaves out keeps its stored
// value. A form with a value that is not valid for the account (picks
// past the limit, no picks, another account's character) is refused,
// and the stored settings stand.
func TestWidgetSettingsFormsReadAsSent(t *testing.T) {
	def := &homeWidgetDef{Options: []widgetOption{
		optScope(),
		optToggle("due", "Only planets that need a visit", false),
		optMulti("topics", "Topics", "a,b", 2, "a:A", "b:B", "c:C"),
		optChoice("range", "History", "30", "30:30 days", "90:90 days"),
	}}
	chars := []db.Character{{CharacterID: 7, Name: "Juno Park", Tags: "industry"}}
	stored := def.optionsFrom(`{"scope":"tag:industry","due":"yes","topics":"c","range":"90"}`, chars)
	if stored.Get("scope") != "tag:industry" || stored.Get("due") != "yes" || stored.Get("topics") != "c" || stored.Get("range") != "90" {
		t.Fatalf("stored settings read as %v", stored)
	}
	got, ok := def.formOptions(map[string][]string{
		"scope":  {"char:7"},
		"due":    {"no"},
		"topics": {"", "a", "b"},
	}, stored, chars)
	if !ok || got.Get("scope") != "char:7" || got.Get("due") != "no" || got.Get("topics") != "a,b" || got.Get("range") != "90" {
		t.Errorf("unticked toggle, two picks, range left out: %v (ok %v)", got, ok)
	}
	if got, ok := def.formOptions(map[string][]string{"due": {"no", "yes"}}, stored, chars); !ok || got.Get("due") != "yes" {
		t.Errorf("ticked toggle: due = %q (ok %v), want yes", got.Get("due"), ok)
	}
	for name, form := range map[string]map[string][]string{
		"another account's character": {"scope": {"char:8"}},
		"three picks of two":          {"topics": {"", "a", "b", "c"}},
		"no picks":                    {"topics": {""}},
	} {
		if got, ok := def.formOptions(form, stored, chars); ok || got.Get("scope") != "tag:industry" || got.Get("topics") != "c" {
			t.Errorf("%s: saved %v (ok %v), want refused with the stored settings", name, got, ok)
		}
	}
}
