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

// With stored market history and a public fit seeded, the strip
// shows real rows shaped from them (movers from the two most
// recent history days, the newest public fit).
func TestLandingStripFromStoredRows(t *testing.T) {
	rig := apptest.Build(t, nil)
	conn := rig.DB()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO type_names (type_id, name) VALUES (34, 'Tritanium'), (35, 'Pyerite'), (36, 'Junk Relic')`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count) VALUES
		   (10000002, 34, '2026-10-14', 40.00, 50, 30, 1000000, 100),
		   (10000002, 34, '2026-10-15', 50.00, 60, 40, 1200000, 110),
		   (10000002, 35, '2026-10-14', 80.00, 90, 70, 500000, 60),
		   (10000002, 35, '2026-10-15', 60.00, 70, 50, 700000, 70),
		   (10000002, 36, '2026-10-14', 0.01, 1, 0.01, 5, 1),
		   (10000002, 36, '2026-10-15', 999.00, 999, 999, 5, 1)`,
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
	// Pyerite fell 25%, Tritanium rose 25%: both movers listed,
	// Pyerite (same |pct|, stable order) present by name either way.
	if !strings.Contains(body, "Tritanium") || !strings.Contains(body, "Pyerite") {
		t.Error("strip does not show the seeded movers")
	}
	if !strings.Contains(body, "25.0%") {
		t.Error("strip does not show the seeded move percentage")
	}
	// The junk row (near-zero baseline, no volume) is sanity-
	// bounded out of the strip entirely.
	if strings.Contains(body, "Junk Relic") {
		t.Error("strip shows an illiquid junk mover")
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
