package app

import (
	"strings"
	"testing"
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
