package tests

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
)

// The anonymous front door is the landing page: hero and heritage
// copy verbatim, a sign-in link for existing members (plain login,
// not the sign-up chooser), the stored-data strip, and no
// em dashes anywhere on the page.
func TestLandingAnonymous(t *testing.T) {
	rig := apptest.Build(t, nil)
	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	for _, want := range []string{
		"Greater than the <span class=\"ld-amp\">sum of its parts.</span>",
		"Rebuilt over <em>4 years</em>. Originating in <em>2013</em>. Built for <em>2026</em>.",
		"href=\"/auth/login\"", // existing members sign in plainly
		"href=\"/auth/link\"",  // new members link characters
		"href=\"/planner/\"",   // the public industry face is the Build Planner
		"Fleet Overview",
		"shown with sample data",
		"Every tool in the box.",
		"Developed in Seattle, WA by",
		"BSD-3-Clause-Clear",
		"© 2026 Synap6.io LLC, dba Synap6",
		"intellectual property of CCP Games hf",
		"Ship replacement manager",
		"Universe Map",
		"Desktop app grade accuracy and features.",
		"shot-fitting.webp",
		"ld-browser",
		"evesynapse.app/fittings/",
		"Plans that price themselves.",
		"shot-planner.webp",
		"evesynapse.app/planner/",
		"Building it saves 362 million.",
		"Every pilot. One glance.",
		"shot-overview.webp",
		"evesynapse.app/",
		"flown by test characters",
		"An analyst reads every sweep.",
		"shot-ideas.webp",
		"evesynapse.app/market/ideas/",
		"Prices with a memory.",
		"shot-market.webp",
		"ninety days of Forge history",
		"priced and simulated with its pilot's own skills",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing is missing %q", want)
		}
	}
	if strings.Contains(body, "—") {
		t.Error("landing contains an em dash")
	}
	// The industry page itself is members-only (it bounces anonymous
	// visitors home); the nav must never link a visitor into a wall.
	if strings.Contains(body, "href=\"/industry/\"") {
		t.Error("landing nav links to the members-only industry page")
	}
	// No stored rows in the fixture: the strip hides entirely.
	if strings.Contains(body, "NEW EDEN, RIGHT NOW") {
		t.Error("strip rendered with no stored data behind it")
	}
	// The signed-in chrome never leaks onto the landing.
	if strings.Contains(body, "sidebar") {
		t.Error("landing rendered the signed-in sidebar chrome")
	}
}

// With stored market history seeded, the strip shows the three
// watched items (PLEX, Large Skill Injector, Small Skill
// Injector) priced from the two most recent history days, in
// that order, with color-coded deltas. PLEX prices come from
// the global market region (19000001), the injectors from The
// Forge. Nothing else makes the strip, however hard it moved.
// The Small Injector's seeded days deliberately lag the
// region's freshest days: each item prices from its OWN two
// most recent days, not the region-wide latest pair.
func TestLandingStripFromStoredRows(t *testing.T) {
	rig := apptest.Build(t, nil)
	conn := rig.DB()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO type_names (type_id, name) VALUES (44992, 'PLEX'), (40520, 'Large Skill Injector'), (45635, 'Small Skill Injector'), (34, 'Tritanium')`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count) VALUES
		   (19000001, 44992, '2026-10-14', 5900000.00, 6000000, 5800000, 100000, 900),
		   (19000001, 44992, '2026-10-15', 6200000.00, 6300000, 6100000, 110000, 950),
		   (10000002, 40520, '2026-10-14', 800000000.00, 810000000, 790000000, 5000, 300),
		   (10000002, 40520, '2026-10-15', 760000000.00, 770000000, 750000000, 5200, 310),
		   (10000002, 45635, '2026-10-12', 300000000.00, 305000000, 295000000, 8000, 400),
		   (10000002, 45635, '2026-10-13', 320000000.00, 325000000, 315000000, 8100, 405),
		   (10000002, 34, '2026-10-14', 4.00, 5, 3, 1000000, 100),
		   (10000002, 34, '2026-10-15', 400.00, 500, 300, 1200000, 110)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	if !strings.Contains(body, "NEW EDEN, RIGHT NOW") {
		t.Fatal("strip missing with stored rows seeded")
	}
	plex := strings.Index(body, "PLEX")
	large := strings.Index(body, "Large Skill Injector")
	small := strings.Index(body, "Small Skill Injector")
	if plex < 0 || large < 0 || small < 0 {
		t.Fatalf("strip does not show all three watched items (plex %d, large %d, small %d)", plex, large, small)
	}
	if !(plex < large && large < small) {
		t.Error("watched items out of order: want PLEX, Large Skill Injector, Small Skill Injector")
	}
	// PLEX rose 5.1% (green up) on the global market (its shown
	// price is that region's), the Large Skill Injector fell
	// 5.0% (red down); the deltas carry their color classes.
	if !strings.Contains(body, "6.20M") {
		t.Error("strip does not show PLEX's global-market price")
	}
	if !strings.Contains(body, `ld-up">▲ 5.1%`) {
		t.Error("strip does not show PLEX's rise as a green up delta")
	}
	if !strings.Contains(body, `ld-down">▼ 5.0%`) {
		t.Error("strip does not show the injector's fall as a red down delta")
	}
	// Tritanium moved 100x and is still not a watched item.
	if strings.Contains(body, "Tritanium") {
		t.Error("strip shows an item outside the watched three")
	}
	// The workers blurb is gone, and the page says syncing, not
	// warming.
	if strings.Contains(body, "precomputed by the workers") {
		t.Error("landing still carries the precomputed-by-the-workers blurb")
	}
	if strings.Contains(body, "warming") {
		t.Error("landing still says warming")
	}
	if !strings.Contains(body, "syncing in the background") {
		t.Error("landing does not say syncing in the background")
	}
}

// Signed-in members skip the landing entirely: they get the
// overview, never the hero.
func TestLandingSkippedWhenSignedIn(t *testing.T) {
	rig := apptest.Build(t, nil)
	q := rig.Queries()
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	code, body := apptest.GetPage(t, rig, cookie, "/")
	if code != 200 {
		t.Fatalf("signed-in / status = %d", code)
	}
	if strings.Contains(body, "Greater than the") {
		t.Error("signed-in member got the landing hero")
	}
}
