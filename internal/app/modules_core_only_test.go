//go:build core_only

package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestCoreOnlyOmitsModules: in the core-only build no optional module
// is compiled in -- the PI planner's nav entry, route and sitemap line
// are all absent.
func TestCoreOnlyOmitsModules(t *testing.T) {
	if len(registeredModules) != 0 {
		t.Fatalf("core-only build registered %d modules, want none", len(registeredModules))
	}
	for section, entries := range sectionNav {
		for _, e := range entries {
			if e.Href == "/industry/pi/" || e.Href == "/planner/" || e.Href == "/fittings/public/" || e.Href == "/fittings/" || strings.HasPrefix(e.Href, "/doctrines/") || e.Href == "/srp/" || e.Href == "/wallet/" || e.Href == "/mail/" || e.Href == "/calendar/" || e.Href == "/contacts/" || e.Href == "/orders/" || e.Href == "/contracts/" || e.Href == "/groups/" || (strings.HasPrefix(e.Href, "/corporations/") && e.Href != "/corporations/" && e.Href != "/corporations/killmails/" && e.Href != "/corporations/settings/") || e.Href == "/fittings/saved/" || e.Href == "/skills/plans" || strings.HasPrefix(e.Href, "/market/") && e.Href != "/market/" || strings.HasPrefix(e.Href, "/intel/") || e.Href == "/market/ideas/" || e.Href == "/discord/servers" {
				t.Errorf("nav section %q lists %s in the core-only build", section, e.Href)
			}
		}
	}

	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	if code, _ := getPage(t, app, cookie, "/industry/pi/"); code != http.StatusNotFound {
		t.Errorf("GET /industry/pi/ in the core-only build: status %d, want 404", code)
	}

	for _, path := range []string{"/wallet/", "/mail/", "/calendar/", "/contacts/", "/orders/", "/contracts/", "/corporations/members/", "/corporations/wallets/", "/doctrines/", "/srp/", "/groups/", "/fittings/", "/fittings/saved/", "/fittings/public/", "/fittings/public/fit/1/", "/market/scanner/", "/market/tradefinder/", "/market/leaderboard/", "/market/restock/", "/skills/plans", "/planner/", "/planner/suggest", "/intel/wars/", "/intel/incursions/", "/intel/fw/", "/market/ideas/", "/discord/servers"} {
		if code, _ := getPage(t, app, cookie, path); code != http.StatusNotFound {
			t.Errorf("GET %s in the core-only build: status %d, want 404", path, code)
		}
	}
	if _, body := getPage(t, app, cookie, "/items/"); strings.Contains(body, `data-nav-category="intel"`) {
		t.Error("the menu still has an Intel branch in the core-only build")
	}

	// The notification settings page has no Discord section.
	if _, body := getPage(t, app, cookie, "/notifications/settings"); strings.Contains(body, "/discord/connect") || strings.Contains(strings.ToLower(body), "also send these to me on discord") {
		t.Error("the notification settings still offer Discord in the core-only build")
	}

	sitemap, err := app.buildSitemap(ctx, "https://node.example.org")
	if err != nil {
		t.Fatalf("sitemap: %v", err)
	}
	if strings.Contains(string(sitemap), "/industry/pi/") || strings.Contains(string(sitemap), "/planner/") || strings.Contains(string(sitemap), "/intel/") {
		t.Error("sitemap lists a module page in the core-only build")
	}
}

// TestCoreOnlyHasNoModuleScopes: with no module compiled in, the scope
// surfaces know only the core manifest.
func TestCoreOnlyHasNoModuleScopes(t *testing.T) {
	for _, id := range []string{"buildplanner", "fitting", "publicfits", "corpdata", "doctrines", "srp", "groups", "skillplans", "scanner", "tradefinder", "leaderboard", "restock", "piplanner", "intel", "tradeideas", "discord"} {
		if _, ok := moduleByID(id); ok {
			t.Errorf("the core-only build still knows %s's declaration", id)
		}
	}
}

// TestCoreOnlyHasNoModuleWidgetsOrKinds: with no module built in, the
// home page offers only core's widgets, the default layout leaves out
// the rest, and the notification settings list only core's kinds.
func TestCoreOnlyHasNoModuleWidgetsOrKinds(t *testing.T) {
	absent := map[string]bool{"market": true, "tradeideas": true, "pulse": true, "walletflow": true, "upkeep": true,
		"mail": true, "calendar": true, "srp_new": true, "srp_done": true}
	for _, w := range homeWidgetCatalog() {
		if absent[w.ID] {
			t.Errorf("the widget catalog lists %q in the core-only build", w.ID)
		}
	}
	for _, item := range defaultHomeLayoutItems() {
		if absent[item.ID] {
			t.Errorf("the default layout lists %q in the core-only build", item.ID)
		}
	}
	for _, k := range notifyKindList() {
		if absent[k.ID] {
			t.Errorf("the notification kinds list %q in the core-only build", k.ID)
		}
	}
}

// TestCoreOnlyChooserOffersOnlyCoreFeatures: in the core-only build the
// full link leaves out every module's scopes, and the chooser still works.
func TestCoreOnlyChooserOffersOnlyCoreFeatures(t *testing.T) {
	full := requestableScopes()
	for _, s := range full {
		if !contains(eveScopes, s) {
			t.Errorf("the full link asks for %q, outside the portal list", s)
		}
	}
	for _, moduleScope := range []string{"esi-mail.send_mail.v1", "esi-calendar.read_calendar_events.v1", "esi-characters.read_contacts.v1", "esi-contracts.read_character_contracts.v1"} {
		if contains(full, moduleScope) {
			t.Errorf("the core-only link still asks for %q", moduleScope)
		}
	}
	for _, f := range linkFeatures() {
		if f.ID == "mail" || f.ID == "doctrines" || f.ID == "swarm" {
			t.Errorf("the chooser offers %q in the core-only build", f.ID)
		}
	}
	if len(full) == 0 {
		t.Error("the core-only full link asks for nothing at all")
	}
}
