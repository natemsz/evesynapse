//go:build !core_only

package app

import (
	"context"
	"evesynapse/internal/app/mail"
	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"fmt"
	"strings"
	"testing"
)

// TestDefaultBuildEnablesModules: the default build compiles in every
// module, and a module's nav entry, page and sitemap line come from its
// registration.
func TestDefaultBuildEnablesModules(t *testing.T) {
	for _, id := range []string{"buildplanner", "fitting", "publicfits", "wallet", "mail", "calendar", "contacts", "market", "contracts", "swarm", "corpdata", "doctrines", "srp", "groups", "skillplans", "scanner", "tradefinder", "leaderboard", "restock", "piplanner", "intel", "tradeideas", "discord"} {
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

// TestModuleNavKeepsItsPlace: a module's menu entry sits where the menu
// has always had it (module.NavEntry.After), not at the end of its
// section.
func TestModuleNavKeepsItsPlace(t *testing.T) {
	follows := func(section, href, prev string) {
		t.Helper()
		list := sectionNav[section]
		for i, e := range list {
			if e.Href == href {
				if i == 0 || list[i-1].Href != prev {
					t.Errorf("%s: %s follows %q, want %q", section, href, list[max(i-1, 0)].Href, prev)
				}
				return
			}
		}
		t.Errorf("%s: no %s entry", section, href)
	}
	follows("market", "/market/ideas/", "/market/scanner/")
	follows("corporation", "/discord/servers", "/srp/")
	follows("industry", "/planner/", "/industry/")
	follows("fitting", "/fittings/saved/", "/fittings/")
	follows("fitting", "/fittings/public/", "/fittings/saved/")
	follows("pilot", "/skills/plans", "/characters/")
	follows("market", "/market/scanner/", "/market/")
	follows("market", "/market/tradefinder/", "/market/ideas/")
	follows("market", "/market/leaderboard/", "/market/tradefinder/")
	follows("market", "/market/restock/", "/market/leaderboard/")
	follows("market", "/orders/", "/market/restock/")

	// The whole pilot and market menus, in the order they had before the modules.
	for section, want := range map[string][]string{
		"pilot":  {"/character/", "/characters/", "/skills/plans", "/assets/", "/wallet/", "/mail/", "/calendar/", "/contacts/", "/killmails/"},
		"market": {"/market/", "/market/scanner/", "/market/ideas/", "/market/tradefinder/", "/market/leaderboard/", "/market/restock/", "/orders/", "/contracts/"},
	} {
		var got []string
		for _, e := range sectionNav[section] {
			got = append(got, e.Href)
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s menu = %v, want %v", section, got, want)
		}
	}

	// The whole corporation menu, in the order it had before the modules.
	want := []string{"/corporations/", "/corporations/members/", "/corporations/wallets/", "/corporations/orders/",
		"/corporations/assets/", "/corporations/structures/", "/corporations/killmails/", "/doctrines/", "/doctrines/fits/",
		"/srp/", "/discord/servers", "/groups/", "/corporations/settings/"}
	var got []string
	for _, e := range sectionNav["corporation"] {
		got = append(got, e.Href)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("corporation menu = %v, want %v", got, want)
	}
}

// TestRegisteredFeaturesAgreeWithTheManifest: a module that takes over a
// manifest feature (mail, wallet, ...) declares the same scopes and
// snapshot kinds core's manifest names for the data feed it keeps fetched,
// so the two cannot drift apart.
func TestRegisteredFeaturesAgreeWithTheManifest(t *testing.T) {
	for _, m := range registeredModules {
		var manifest *moduleDef
		for i := range moduleManifest {
			if moduleManifest[i].ID == m.ID {
				manifest = &moduleManifest[i]
			}
		}
		if manifest == nil {
			continue
		}
		got := moduleDefFromRegistry(m)
		if fmt.Sprint(got.Scopes) != fmt.Sprint(manifest.Scopes) {
			t.Errorf("%s: module scopes %v differ from the manifest's %v", m.ID, got.Scopes, manifest.Scopes)
		}
		if fmt.Sprint(got.Snapshots) != fmt.Sprint(manifest.Snapshots) {
			t.Errorf("%s: module snapshots %v differ from the manifest's %v", m.ID, got.Snapshots, manifest.Snapshots)
		}
	}
}

// TestModuleDeclaredCatalogsKeepTheirOrder: the home widgets and the
// notification kinds the modules declare land where they were before
// the modules, so the customize list and the notification settings read
// the same.
func TestModuleDeclaredCatalogsKeepTheirOrder(t *testing.T) {
	var widgets []string
	for _, w := range homeWidgetCatalog() {
		widgets = append(widgets, w.ID)
	}
	wantWidgets := "glance briefing fleet networth industry pi market watchlist tradeideas training pulse walletflow upkeep"
	if got := strings.Join(widgets, " "); got != wantWidgets {
		t.Errorf("widget catalog = %q, want %q", got, wantWidgets)
	}
	var kinds []string
	for _, k := range notifyKindList() {
		kinds = append(kinds, k.ID)
	}
	wantKinds := "watch skill mail calendar op op_reminder srp_new srp_done killmail pi industry"
	if got := strings.Join(kinds, " "); got != wantKinds {
		t.Errorf("notification kinds = %q, want %q", got, wantKinds)
	}
}

// TestBriefingTopicsAreDeclarations: the briefing line families a module
// owns are declared on that module, once each, and the briefing reads
// them from the registry — a topic no built-in module declares is off.
func TestBriefingTopicsAreDeclarations(t *testing.T) {
	owners := map[string]string{}
	for _, m := range registeredModules {
		for _, topic := range m.BriefingTopics {
			if other, dup := owners[topic.ID]; dup {
				t.Errorf("briefing topic %q is declared by both %s and %s", topic.ID, other, m.ID)
			}
			owners[topic.ID] = m.ID
			if topic.Title == "" {
				t.Errorf("briefing topic %q from %s has no title", topic.ID, m.ID)
			}
		}
	}
	for topic, owner := range map[string]string{"orders": "market", "contracts": "contracts", "calendar": "calendar", "mail": "mail"} {
		if owners[topic] != owner {
			t.Errorf("briefing topic %q is owned by %q, want %q", topic, owners[topic], owner)
		}
		if !briefingTopicBuiltIn(topic) {
			t.Errorf("briefing topic %q is declared but not read as built in", topic)
		}
	}
	if briefingTopicBuiltIn("nosuchtopic") {
		t.Error("an undeclared briefing topic reads as built in")
	}
}

// TestModuleOnlyFeaturesHaveNoManifestEntry: a feature only a module's pages
// read is declared once, by the module. Only "wallet" and "market" are in
// both, because core's net worth reads the wallet and the open orders.
func TestModuleOnlyFeaturesHaveNoManifestEntry(t *testing.T) {
	shared := map[string]bool{"wallet": true, "market": true}
	for _, m := range registeredModules {
		for _, def := range moduleManifest {
			if def.ID == m.ID && !shared[m.ID] {
				t.Errorf("%q is declared by its module and again in the manifest", m.ID)
			}
		}
	}
}

// TestKindsOfAbsentModulesAreNotFetched: the feed fetches a kind that only a
// module reads only where the module is built in.
func TestKindsOfAbsentModulesAreNotFetched(t *testing.T) {
	has := func(kinds []string, want string) bool {
		for _, k := range kinds {
			if k == want {
				return true
			}
		}
		return false
	}
	withModules(t)
	if has(commsSnapshotKinds(), esi.SnapMail) || has(commsSnapshotKinds(), esi.SnapContacts) || has(economySnapshotKinds(), esi.SnapContracts) {
		t.Error("with no module built in the worker would still fetch mail, contacts or contracts")
	}
	if !has(economySnapshotKinds(), esi.SnapWalletJournal) || !has(economySnapshotKinds(), esi.SnapOrders) {
		t.Error("the wallet and order feeds are core's (net worth) and must stay")
	}
	withModules(t, mail.Module)
	if !has(commsSnapshotKinds(), esi.SnapMail) || has(commsSnapshotKinds(), esi.SnapCalendar) {
		t.Error("with only the mail module, mail should be fetched and the calendar not")
	}
}
