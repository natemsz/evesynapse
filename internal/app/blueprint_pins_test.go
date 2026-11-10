package app

// Tests for blueprint pins and the Production Lines home widget:
// the pin toggle round-trips through the Blueprints page's own
// POST route, and the widget judges each pinned line from the
// seeded job snapshots (a ready job is due, a running job is
// healthy, no job needs a restart), leaves unpinned blueprints
// out, and builds its buy list from the seeded SDE recipes less
// the seeded asset stock. How much shows follows the widget's
// stored size. Same hermetic contract as the other suites:
// seeded stored state, zero outbound calls.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestBlueprintPinRoundTrip pins and unpins a blueprint through
// the Blueprints page: the row's toggle posts to /industry/pin/,
// the pin lands in the account's pin set, the re-rendered page
// shows the pinned state, and unpinning clears it.
func TestBlueprintPinRoundTrip(t *testing.T) {
	app, q, cookie := plannerScopeApp(t)
	ctx := context.Background()
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2001, LocationID: 60003760, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, `[]`)

	char, err := q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("get character: %v", err)
	}

	_, body := getPage(t, app, cookie, "/industry/")
	mustContain(t, "/industry/ (unpinned)", body,
		`action="/industry/pin/"`, `name="blueprint" value="2001"`,
		`name="pinned" value="1"`, `aria-pressed="false"`, `>Pin</button>`)

	code, _ := postForm(t, app, cookie, "/industry/pin/", url.Values{
		"blueprint": {"2001"},
		"character": {strconv.FormatInt(fixtureCharA, 10)},
		"pinned":    {"1"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("pin POST: status %d, want %d", code, http.StatusSeeOther)
	}
	pins, err := q.ListBlueprintPins(ctx, char.UserID)
	if err != nil {
		t.Fatalf("list pins: %v", err)
	}
	if len(pins) != 1 || pins[0].BlueprintTypeID != 2001 {
		t.Fatalf("pins after pin POST = %v, want [2001]", pins)
	}

	_, body = getPage(t, app, cookie, "/industry/")
	mustContain(t, "/industry/ (pinned)", body,
		`bp-pin-btn on`, `name="pinned" value="0"`, `aria-pressed="true"`, `>Pinned</button>`)

	code, _ = postForm(t, app, cookie, "/industry/pin/", url.Values{
		"blueprint": {"2001"},
		"character": {strconv.FormatInt(fixtureCharA, 10)},
		"pinned":    {"0"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("unpin POST: status %d, want %d", code, http.StatusSeeOther)
	}
	pins, err = q.ListBlueprintPins(ctx, char.UserID)
	if err != nil {
		t.Fatalf("list pins: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("pins after unpin POST = %v, want none", pins)
	}
}

// seedProdLineWidget plants the widget's world on one character:
// the planner SDE fixture (blueprint 2001 makes a Fixture Widget
// from 2 Fixture Components and 10 Tritanium; 2002 makes a
// Fixture Component from 5 Tritanium) plus two more blueprints,
// 2003 (a Fixture Sprocket from 7 Tritanium) and 2005 (a Fixture
// Doohickey from 3 Tritanium). Jobs: 2001 ready (due), 2005
// finishing in two hours (due), 2002 running for three more days
// (healthy), 2003 none (needs a restart), and an unpinned 2004
// with a ready job the widget must leave out. Stock: 4 Tritanium
// and 1 Fixture Component, so the buy list for the due and idle
// lines is 1 Fixture Component and 16 Tritanium.
func seedProdLineWidget(t *testing.T, app *Application, q *db.Queries, userID int64) {
	t.Helper()
	ctx := context.Background()
	seedPlannerSDE(t, app)
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(1003, 'Fixture Sprocket', 0, 0, 1), (2003, 'Fixture Sprocket Blueprint', 0, 0, 1),
			(1005, 'Fixture Doohickey', 0, 0, 1), (2005, 'Fixture Doohickey Blueprint', 0, 0, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES
			(2003, 1003, 1, 60, 300), (2005, 1005, 1, 60, 300)`,
		`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES
			(2003, 34, 7), (2005, 34, 3)`,
	} {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed prodline sde: %v", err)
		}
	}
	now := time.Now()
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 1, ActivityID: 1, Status: "ready", BlueprintTypeID: 2001, ProductTypeID: 1001,
			EndDate: rfc(now.Add(-time.Hour))},
		{JobID: 2, ActivityID: 1, Status: "active", BlueprintTypeID: 2002, ProductTypeID: 1002,
			EndDate: rfc(now.Add(72 * time.Hour))},
		{JobID: 3, ActivityID: 1, Status: "ready", BlueprintTypeID: 2004, ProductTypeID: 1004,
			EndDate: rfc(now.Add(-time.Hour))},
		{JobID: 4, ActivityID: 1, Status: "active", BlueprintTypeID: 2005, ProductTypeID: 1005,
			EndDate: rfc(now.Add(2 * time.Hour))},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 1, TypeID: 34, Quantity: 4},
		{ItemID: 2, TypeID: 1002, Quantity: 1},
	})
	for _, bp := range []int64{2001, 2002, 2003, 2005} {
		if err := q.PinBlueprint(ctx, db.PinBlueprintParams{UserID: userID, BlueprintTypeID: bp}); err != nil {
			t.Fatalf("pin %d: %v", bp, err)
		}
	}
}

func TestProdLineWidgetSizes(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedOverviewCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "")
	seedProdLineWidget(t, app, q, user.ID)
	cookie := app.SessionCookie(t, user.ID, fixtureCharA, "Fixture Alpha")

	// Default size (2x2): every line with its state, and the buy
	// list for the due and idle lines.
	setHomeLayout(t, q, user.ID, `[{"id":"prodline"}]`)
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (default): status %d", code)
	}
	mustContain(t, "/ (prodline default)", body,
		`data-widget="prodline"`, "Production Lines",
		"Fixture Widget", "Ready for delivery",
		"Fixture Doohickey", "Delivers in",
		"Fixture Sprocket", "Needs a restart",
		"Fixture Component", "healthy",
		"Buy for the next run",
		`<span class="prodline-buyname">Fixture Component</span> <span class="prodline-qty">1</span>`,
		`<span class="prodline-buyname">Tritanium</span> <span class="prodline-qty">16</span>`,
	)
	if strings.Contains(body, "Fixture Alpha · ends") {
		t.Error("default size shows the runner detail, want it at the largest size only")
	}

	// Smallest size (1x1): the lines needing attention only, no
	// healthy line and no buy list.
	setHomeLayout(t, q, user.ID, `[{"id":"prodline","w":1,"h":1}]`)
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (small): status %d", code)
	}
	mustContain(t, "/ (prodline small)", body,
		"Fixture Widget", "Ready for delivery",
		"Fixture Doohickey", "Delivers in",
		"Fixture Sprocket", "Needs a restart",
	)
	for _, absent := range []string{"healthy", "Fixture Component", "Buy for the next run"} {
		if strings.Contains(body, absent) {
			t.Errorf("small widget shows %q, want attention lines only", absent)
		}
	}

	// Largest size (3x2): every line, the runner detail on the
	// healthy line, the aggregate buy list, and each attention
	// line's own missing materials (the due line needs 6
	// Tritanium of its own run after the 4 in stock).
	setHomeLayout(t, q, user.ID, `[{"id":"prodline","w":3,"h":2}]`)
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (large): status %d", code)
	}
	mustContain(t, "/ (prodline large)", body,
		"Fixture Widget", "Ready for delivery",
		"Fixture Doohickey", "Delivers in",
		"Fixture Sprocket", "Needs a restart",
		"Fixture Component", "healthy",
		"Fixture Alpha · ends",
		"prodline-missing",
		`<li class="prodline-buyrow"><img class="prodline-buyart" src="https://images.evetech.net/types/1002/icon?size=64" alt="" width="24" height="24" loading="lazy"><span class="prodline-buyname">Fixture Component</span> <span class="prodline-qty">1</span></li>`,
		`<li class="prodline-buyrow"><img class="prodline-buyart" src="https://images.evetech.net/types/34/icon?size=64" alt="" width="24" height="24" loading="lazy"><span class="prodline-buyname">Tritanium</span> <span class="prodline-qty">6</span></li>`,
		`<span class="prodline-buyname">Tritanium</span> <span class="prodline-qty">16</span>`,
	)
}

// TestProdLineWidgetNoPins: with nothing pinned the widget says
// where pins are set.
func TestProdLineWidgetNoPins(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedOverviewCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "")
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{})
	cookie := app.SessionCookie(t, user.ID, fixtureCharA, "Fixture Alpha")
	setHomeLayout(t, q, user.ID, `[{"id":"prodline"}]`)
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (no pins): status %d", code)
	}
	mustContain(t, "/ (prodline no pins)", body, `data-widget="prodline"`, "No pinned lines yet")
}
