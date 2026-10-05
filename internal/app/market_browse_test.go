package app

// Hermetic tests for the v0.3.10 market category tree:
//
//   - invMarketGroups parsing (parent/child via both parent
//     column spellings, root normalization, icon/hasTypes ride-
//     along) and the store round-trip into sde_market_groups.
//   - The Market page browser: top-level groups with glyphs,
//     drill-down with breadcrumbs, and leaf type listings held
//     to the published/marketable floor — all cache-only.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestSDEMarketGroupsParseAndStore(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	// Current dump spelling: parentMarketGroupID.
	csvCurrent := "\"marketGroupID\",\"parentMarketGroupID\",\"marketGroupName\",\"iconID\",\"hasTypes\"\n" +
		"\"4\",\"\",\"Ships\",\"42\",\"0\"\n" +
		"\"404\",\"4\",\"Frigates\",\"43\",\"0\"\n" +
		"\"405\",\"404\",\"Standard Frigates\",\"44\",\"1\"\n"
	var parsed parsedSDE
	if err := parseSDEFile("invMarketGroups.csv", strings.NewReader(csvCurrent), &parsed); err != nil {
		t.Fatalf("parse market groups: %v", err)
	}
	if len(parsed.marketGroups) != 3 {
		t.Fatalf("parsed %d market groups, want 3", len(parsed.marketGroups))
	}
	byID := map[int64]sdeMarketGroupRow{}
	for _, row := range parsed.marketGroups {
		byID[row.marketGroupID] = row
	}
	if got := byID[4]; got.parentGroupID != 0 || got.name != "Ships" || got.iconID != 42 || got.hasTypes != 0 {
		t.Fatalf("root group = %+v, want parent 0 Ships icon 42 hasTypes 0", got)
	}
	if got := byID[404]; got.parentGroupID != 4 {
		t.Fatalf("child group parent = %d, want 4", got.parentGroupID)
	}
	if got := byID[405]; got.parentGroupID != 404 || got.hasTypes != 1 {
		t.Fatalf("leaf group = %+v, want parent 404 hasTypes 1", got)
	}

	// Older spelling: parentGroupID parses the same way.
	csvLegacy := "\"marketGroupID\",\"parentGroupID\",\"marketGroupName\"\n" +
		"\"4\",\"0\",\"Ships\"\n\"404\",\"4\",\"Frigates\"\n"
	var legacy parsedSDE
	if err := parseSDEFile("invMarketGroups.csv", strings.NewReader(csvLegacy), &legacy); err != nil {
		t.Fatalf("parse market groups (legacy parent column): %v", err)
	}
	if len(legacy.marketGroups) != 2 || legacy.marketGroups[1].parentGroupID != 4 {
		t.Fatalf("legacy parse = %+v, want child parented to 4", legacy.marketGroups)
	}

	// The store round-trips the tree and stamps import version 7.
	if _, err := app.storeSDE(ctx, "fixture", &parsed); err != nil {
		t.Fatalf("store SDE: %v", err)
	}
	tops, err := q.ListSDEMarketGroupsByParent(ctx, 0)
	if err != nil || len(tops) != 1 || tops[0].Name != "Ships" {
		t.Fatalf("top groups = %+v err=%v, want [Ships]", tops, err)
	}
	children, err := q.ListSDEMarketGroupsByParent(ctx, 4)
	if err != nil || len(children) != 1 || children[0].MarketGroupID != 404 {
		t.Fatalf("children of Ships = %+v err=%v, want [Frigates]", children, err)
	}
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != "7" {
		t.Fatalf("sde_import_version = %q, want 7", ver)
	}
}

func TestMarketBrowseTreeRendersCacheOnly(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	stmts := []string{
		`INSERT INTO sde_market_groups (market_group_id, parent_group_id, name, icon_id, has_types) VALUES
		   (4, 0, 'Ships', 42, 0),
		   (24, 0, 'Implants & Boosters', 43, 0),
		   (404, 4, 'Frigates', 44, 0),
		   (405, 404, 'Standard Frigates', 45, 1)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (587, 'Fixture Rifter', 25, 405, 1),
		   (588, 'Fixture Draft Hull', 25, 405, 0),
		   (589, 'Fixture Ghost Hull', 25, 0, 1)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed market tree: %v", err)
		}
	}

	// Top level: both root groups, glyph-marked, linking down.
	code, body := getPage(t, app, cookie, "/market/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/: status %d", code)
	}
	mustContain(t, "/market/ browse", body,
		"Browse categories", "Ships", "Implants &amp; Boosters",
		`href="/market/?group=4&region=10000002"`,
		`data-market-icon="ships"`, `data-market-icon="implants"`,
		`url(#nav-glyph-gradient)`,
	)
	if got := strings.Count(body, `id="nav-glyph-gradient"`); got != 1 {
		t.Fatalf("page defines nav-glyph-gradient %d times, want exactly 1 (base layout)", got)
	}

	// Drill-down: children plus breadcrumb back-path.
	code, body = getPage(t, app, cookie, "/market/?group=4")
	if code != http.StatusOK {
		t.Fatalf("GET /market/?group=4: status %d", code)
	}
	mustContain(t, "/market/ ships", body,
		"Frigates", `href="/market/?group=404&region=10000002"`,
		`href="/market/"`, // breadcrumb back to the browser root
	)

	// Leaf: breadcrumb chain and the type listing, held to the
	// floor (the unpublished and group-less types stay out).
	code, body = getPage(t, app, cookie, "/market/?group=405")
	if code != http.StatusOK {
		t.Fatalf("GET /market/?group=405: status %d", code)
	}
	mustContain(t, "/market/ leaf", body,
		"Standard Frigates", "Fixture Rifter",
		`href="/market/?type=587&region=10000002"`,
		`href="/market/?group=4&region=10000002"`,
		`href="/market/?group=404&region=10000002"`,
	)
	if strings.Contains(body, "Fixture Draft Hull") || strings.Contains(body, "Fixture Ghost Hull") {
		t.Fatal("leaf listing leaked a type below the published/marketable floor")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("market browse handlers made %d outbound calls, want 0", got)
	}
}

func TestMarketGroupIconMapping(t *testing.T) {
	cases := []struct {
		name string
		id   int64
		want string
	}{
		{"Ships", 4, "ships"},
		{"Ship Equipment", 0, "modules"},
		{"Ship Modifications", 0, "rigs"},
		{"Ship and Module Modifications", 0, "rigs"},
		{"Ammunition & Charges", 0, "charges"},
		{"Drones", 0, "drones"},
		{"Implants & Boosters", 24, "implants"},
		{"Anything At All", 24, "implants"}, // verified ID anchor wins
		{"Apparel", 0, "apparel"},
		{"Structures", 0, "structures"},
		{"Structure Equipment", 0, "structures"},
		{"Manufacture & Research", 0, "materials"},
		{"Raw Materials", 0, "materials"},
		{"Combat Drones", 0, "drones"},         // child-appropriate reuse
		{"Armor Rigs", 0, "rigs"},              // child-appropriate reuse
		{"Unmapped Oddity", 999999, "generic"}, // fallback
	}
	for _, c := range cases {
		if got := marketGroupIconKey(c.name, c.id); got != c.want {
			t.Errorf("marketGroupIconKey(%q, %d) = %q, want %q", c.name, c.id, got, c.want)
		}
		svg := string(marketGroupIconSVG(c.name, c.id))
		for _, want := range []string{`viewBox="0 0 24 24"`, `url(#nav-glyph-gradient)`, `data-market-icon="` + c.want + `"`} {
			if !strings.Contains(svg, want) {
				t.Errorf("icon SVG for %q missing %q: %s", c.name, want, svg)
			}
		}
		if strings.Contains(svg, `<linearGradient`) {
			t.Errorf("icon SVG for %q redefines the shared gradient: %s", c.name, svg)
		}
	}
}
