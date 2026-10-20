package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/apptest"
	db "evesynapse/internal/db/sqlc"
)

// The anonymous front door is the landing page: hero and heritage
// copy verbatim, a sign-in link for existing members (plain login,
// not the sign-up chooser), trust and playstyle sections ahead of
// the alternating feature sections, the stored-data strip, and no
// em dashes anywhere on the page.
func TestLandingAnonymous(t *testing.T) {
	rig := apptest.Build(t, nil)
	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	for _, want := range []string{
		"Stop alt-tabbing between <span class=\"ld-amp\">five different tools.</span>",
		"Fits run on your real skills.",
		"Try it now, no login needed",
		"Only the scopes you pick",
		"Key held in a hardware vault",
		"Built by one pilot.",
		"concurrent pilots on one small server, load-tested",
		"Rebuilt over <em>4 years</em>. Originating in <em>2013</em>. Built for <em>2026</em>.",
		"href=\"/auth/login\"",        // existing members sign in plainly
		"href=\"/auth/link\"",         // new members link characters
		"href=\"/planner/\"",          // the public industry face is the Build Planner
		"href=\"/contracts/search/\"", // the public contract search
		"href=\"/market/appraise/\"",  // the public appraisal tool
		"Appraisal",
		"Contract Search",
		"Fleet Overview",
		"shown with sample data",
		"Every tool in the box.",
		"Developed in Seattle, WA by",
		"BSD-3-Clause-Clear",
		"© 2026 Synap6.io LLC, dba Synap6",
		"intellectual property of CCP Games hf",
		"Ship Replacement",
		"Your keys, your call.",
		"Public data, no login",
		"PKCE on every sign-in",
		"Revoke on unlink",
		"Three alts, or a hundred boxed characters.",
		"Know what your ISK is doing.",
		"Build with the margin already proven.",
		"One stack for the whole corporation.",
		"A hundred pilots, one morning briefing.",
		"Every pilot. One glance.",
		"Desktop grade fitting, in a browser tab.",
		"shot-fitting.webp",
		"ld-browser",
		`<div class="ld-stars"></div><div class="ld-stars2"></div>`, // the sky's star layers, ahead of the header
		"evesynapse.app/fittings/",
		"priced and simulated with its pilot's own skills",
		"Know what it's worth, and what to do next.",
		"Appraisal: paste anything, get a price",
		"The appraisal tool on the live site.",
		"evesynapse.app/market/appraise/",
		"shot-market.webp",
		"Plans that price themselves.",
		"87.6M",
		"449.8M",
		"Greater than the sum of its parts.",
		"Know where New Eden is hot.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing is missing %q", want)
		}
	}
	// The section order is the restructure's whole point: trust
	// first, playstyles next, then the feature sections.
	trustAt := strings.Index(body, "Your keys, your call.")
	stylesAt := strings.Index(body, "Three alts, or a hundred boxed characters.")
	fleetAt := strings.Index(body, "Every pilot. One glance.")
	if !(trustAt >= 0 && trustAt < stylesAt && stylesAt < fleetAt) {
		t.Error("landing sections out of order: want trust, then playstyles, then the feature sections")
	}
	// The tools inventory is audited against the tree: features
	// that do not exist yet are never claimed on the front door.
	for _, unbuilt := range []string{"Universe Map", "Route Planner", "Profit Tracking"} {
		if strings.Contains(body, unbuilt) {
			t.Errorf("landing claims the unbuilt %q", unbuilt)
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
	// The preview's alert cards are live data or nothing: with
	// no snapshots stored, no cards render, and the old canned
	// example alerts never come back.
	if strings.Contains(body, "ld-brief") {
		t.Error("alert cards rendered with no stored snapshots behind them")
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
	if !strings.Contains(body, "syncing in the background") {
		t.Error("landing does not say syncing in the background")
	}
}

// With the global snapshots stored, the preview's alert cards
// carry the live figures from those snapshots (an established
// incursion with its boss up, the faction warfare front), and
// the showcased fit is the newest PRICED public fit, showing
// its stored price: the newer unpriced fit must not take the
// slot and leave the "priced" claim empty.
func TestLandingLiveAlertsAndPricedFit(t *testing.T) {
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

	snapshots := []db.UpsertGlobalSnapshotParams{
		{Kind: "incursions", Payload: `[{"constellation_id":20000417,"faction_id":500019,"has_boss":true,"infested_solar_systems":[30000142,30000143,30000144],"influence":0.42,"staging_solar_system_id":30000142,"state":"established","type":"scout"}]`, FetchedAt: now, CachedUntil: now.Add(time.Hour), Etag: ""},
		{Kind: "fw_systems", Payload: `[{"solar_system_id":30000142,"occupier_faction_id":500001,"owner_faction_id":500001,"contested":"contested","victory_points":10,"victory_points_threshold":100},{"solar_system_id":30000143,"occupier_faction_id":500001,"owner_faction_id":500002,"contested":"contested","victory_points":20,"victory_points_threshold":100},{"solar_system_id":30000144,"occupier_faction_id":500002,"owner_faction_id":500002,"contested":"vulnerable","victory_points":90,"victory_points_threshold":100},{"solar_system_id":30000145,"occupier_faction_id":500002,"owner_faction_id":500002,"contested":"uncontested","victory_points":0,"victory_points_threshold":100}]`, FetchedAt: now, CachedUntil: now.Add(time.Hour), Etag: ""},
	}
	for _, s := range snapshots {
		if err := q.UpsertGlobalSnapshot(ctx, s); err != nil {
			t.Fatalf("seed snapshot %s: %v", s.Kind, err)
		}
	}

	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	for _, want := range []string{
		"The alerts beneath it are live, read from stored data",
		"sev-critical",
		"Incursion in Constellation #20000417.",
		"Sansha forces hold 3 systems at 42% influence.",
		"The final-encounter boss is present.",
		"Faction warfare.",
		"2 systems contested and 1 vulnerable across the warzones.",
		"Priced Rifter",
		"2,160,000,000.00 ISK",
		"priced from stored market data",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing is missing live content %q", want)
		}
	}
	for _, banned := range []string{
		"Skill queues empty", "Planetary extractors halted",
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
	if strings.Contains(body, "Greater than the") {
		t.Error("signed-in member got the landing hero")
	}
}
