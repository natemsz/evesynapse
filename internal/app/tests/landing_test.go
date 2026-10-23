package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/apptest"
)

// The anonymous front door is the landing page: hero and heritage
// copy verbatim, the hero's three doors (sign up, the demo, the
// public tools), a sign-in link for existing members (plain login,
// not the sign-up chooser), the tools inventory, then what sets
// EveSynapse apart with the load-test stats and their fine print,
// then the trust points, and no em dashes anywhere on the page.
func TestLandingAnonymous(t *testing.T) {
	rig := apptest.Build(t, nil)
	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	for _, want := range []string{
		"One connected system for <span class=\"ld-amp\">your whole fleet.</span>",
		"Market, fitting, industry, intel and corporation tools in one place, all reading the same stored data.",
		"Sign up free",
		"Try the demo",
		"data-open-tools",
		"Try the demo runs on a sample fleet. No sign-in needed.",
		"Linking asks only for the scopes behind the features you pick",
		"EveSynapse is free to use forever.",
		"No donation pop-ups.",
		"concurrent pilots on one small server, load-tested",
		"pilots kept fully current, sync load-tested",
		"rosters of 12 to 14,800 pilots driven through the real sync cycle",
		"Run on a 8 core ARM server with 24 GB of RAM",
		"Rebuilt over <em>4 years</em>. Originating in <em>2013</em>. Built for <em>2026</em>.",
		"href=\"/auth/login\"",        // existing members sign in plainly
		"href=\"/auth/link\"",         // new members link characters
		"href=\"/auth/demo\"",         // the demo door
		"href=\"/planner/\"",          // the public industry face is the Build Planner
		"href=\"/contracts/search/\"", // the public contract search
		"href=\"/market/appraise/\"",  // the public appraisal tool
		"href=\"/map/\"",              // the public universe map
		"href=\"/intel/kills/\"",      // the public killboard
		"Appraisal",
		"Contract Search",
		"Fleet Overview",
		"Every tool in the box.",
		"Twenty-three modules plus the core pages, all reading the same stored data.",
		"Developed in Seattle, WA by",
		"BSD-3-Clause-Clear",
		"© 2026 Synap6.io LLC, dba Synap6",
		"intellectual property of CCP Games hf",
		"Ship Replacement",
		"What sets us apart",
		"One system that does its own math.",
		"every character syncs in the background",
		"a medium-sized group gets doctrines, ship replacement and intel channels",
		"full dogma simulation",
		"Save to EVE built in",
		"priced from confirmed contract sales",
		"appraised line by line, and deals are flagged on the row",
		"Gate camps are counted from ship kills only.",
		"author doctrines in the fitting tool itself",
		"Free. No ads. No tracking.",
		"Free to use forever, visitor and member alike.",
		"No donation pop-ups, no tip jar, no nag screens.",
		"solo small-gang low-sec pilot",
		"No coalition funding",
		"Intel and reference",
		"/static/landing.js",
		"Universe Map",
		"Killboard",
		"Route Planner",
		"D-Scan Parser",
		"Intel Channels",
		"Axon",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing is missing %q", want)
		}
	}
	// The section order: the tools inventory sits above what
	// sets EveSynapse apart, then the trust points.
	toolsAt := strings.Index(body, "Every tool in the box.")
	apartAt := strings.Index(body, "One system that does its own math.")
	trustAt := strings.Index(body, "Free. No ads. No tracking.")
	if !(toolsAt >= 0 && toolsAt < apartAt && apartAt < trustAt) {
		t.Error("landing sections out of order: want tools, what sets us apart, then trust")
	}
	// The tools inventory is audited against the tree: features
	// that do not exist yet are never claimed on the front door.
	for _, unbuilt := range []string{"Profit Tracking"} {
		if strings.Contains(body, unbuilt) {
			t.Errorf("landing claims the unbuilt %q", unbuilt)
		}
	}
	// Scope claims belong to the entry point they describe: the
	// picked-scopes wording is the link chooser's, and the page
	// never states it as a property of every EVE login.
	for _, misplaced := range []string{"the EVE login asks for exactly those scopes", "The login asks only for the scopes"} {
		if strings.Contains(body, misplaced) {
			t.Errorf("landing still states the chooser claim as a login property %q", misplaced)
		}
	}
	// The antithesis slogans the copy pass removed stay removed:
	// the landing speaks in the playstyle cards' direct voice.
	for _, tell := range []string{"not a dashboard dump", "Critical first, noise never", "not price lists", "calculated, not drawn", "not a hack", "Data in. Decisions out.", "knows itself", "Each point integrates with the other.", "JavaScript frameworks", "design target", "It turns it into insight"} {
		if strings.Contains(body, tell) {
			t.Errorf("landing still carries the removed slogan %q", tell)
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
	// The landing renders no alert cards: the old canned
	// example alerts never come back.
	if strings.Contains(body, "ld-brief") {
		t.Error("alert cards rendered on the landing")
	}
	for _, canned := range []string{"Skill queues empty", "Planetary extractors halted"} {
		if strings.Contains(body, canned) {
			t.Errorf("landing still shows the canned example alert %q", canned)
		}
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
//
// Regression guard (2026-10-16): type_names deliberately
// carries NONE of the watched rows, matching production —
// it is a sparse, lazily-warmed name cache, and the strip
// once built its watched set from it, so history flowed for
// all three items while the live strip stayed empty (twice,
// behind green tests that seeded the cache). Display names
// resolve from sde_types, the complete catalog, so the
// watched types are seeded there instead.
func TestLandingStripFromStoredRows(t *testing.T) {
	rig := apptest.Build(t, nil)
	conn := rig.DB()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (18, 'Mineral', 4)`,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (44992, 'PLEX', 18), (40520, 'Large Skill Injector', 18), (45635, 'Small Skill Injector', 18), (34, 'Tritanium', 18)`,
		`INSERT INTO type_names (type_id, name) VALUES (34, 'Tritanium')`,
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
	if !strings.Contains(body, "syncs in the background") {
		t.Error("landing does not say syncs in the background")
	}
}

// The strip's showcased fit is the newest PRICED public fit,
// showing its stored price: the newer unpriced fit must not
// take the slot and leave the "priced" claim empty.
func TestLandingStripPricedFit(t *testing.T) {
	rig := apptest.Build(t, nil)
	q := rig.Queries()
	ctx := context.Background()
	seedPublicSDE(t, rig.DB())

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedPublicFit(t, q, user.ID, "Newest Unpriced", 587, `{"typeId":1238,"qty":1}`, `"pvp"`, now)
	pricedID := seedPublicFit(t, q, user.ID, "Priced Rifter", 587, `{"typeId":1238,"qty":1}`, `"pvp"`, now.Add(-time.Hour))
	if _, err := rig.DB().ExecContext(ctx, `UPDATE local_fittings SET total_price = 2160000000 WHERE id = $1`, pricedID); err != nil {
		t.Fatalf("price fit: %v", err)
	}

	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	for _, want := range []string{
		"Priced Rifter",
		"2,160,000,000.00 ISK",
		"priced from stored market data",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing is missing live content %q", want)
		}
	}
	for _, banned := range []string{
		"Newest Unpriced",   // the unpriced fit must not take the showcase
		"Newest public fit", // the priced pick is not the newest fit
	} {
		if strings.Contains(body, banned) {
			t.Errorf("landing unexpectedly shows %q", banned)
		}
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
	if strings.Contains(body, "One connected system for") {
		t.Error("signed-in member got the landing hero")
	}
}
