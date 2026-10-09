package app

// Hermetic tests for industry build planner:
//
//   - Importer: the five Fuzzwork industry files parse with the
//     manufacturing-activity filter and join into blueprint rows.
//   - Engine: BOM expansion against fixture recipes — the EVE
//     material formula (ME curve, base-1 floor), whole-run child
//     production, cycle guard, node cap, owned-blueprint ME/TE
//     autofill vs explicit overrides, single-allocation stockpile
//     netting, and price honesty (missing prices are unknowns,
//     never invented zeros).
//   - Pages: /planner/ renders from local data only (counting
//     transport proves zero outbound calls), with the warm state
//     before the industry tables land.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

// ---------------------------------------------------------------------------
// Importer fixtures: small but real-shaped slices of the Fuzzwork
// industry files (quoted CSV, manufacturing is activityID 1).
// ---------------------------------------------------------------------------

const fixtureIndustryBlueprints = `"typeID","maxProductionLimit"
"2001","300"
"2002","60"
"2003","0"
`

const fixtureIndustryActivity = `"typeID","activityID","time"
"2001","1","600"
"2001","3","210"
"2002","1","300"
"2003","11","120"
`

const fixtureIndustryProducts = `"typeID","activityID","productTypeID","quantity"
"2001","1","1001","1"
"2002","1","1002","1"
"2003","11","1003","1"
`

const fixtureIndustryMaterials = `"typeID","activityID","materialTypeID","quantity"
"2001","1","1002","2"
"2001","1","34","10"
"2002","1","34","5"
"2003","11","34","7"
`

const fixtureIndustrySkills = `"typeID","activityID","skillID","level"
"2001","1","3380","1"
"2001","8","11442","2"
"2002","1","3380","4"
`

func parseFixtureSDE(t *testing.T, files map[string]string) *parsedSDE {
	t.Helper()
	parsed := &parsedSDE{markers: make(map[string]fileMarker)}
	for name, body := range files {
		if err := parseSDEFile(name, strings.NewReader(body), parsed); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
	}
	parsed.buildIndustryRows()
	return parsed
}

func TestIndustryCSVImportJoinsBlueprints(t *testing.T) {
	parsed := parseFixtureSDE(t, map[string]string{
		"industryBlueprints.csv":        fixtureIndustryBlueprints,
		"industryActivity.csv":          fixtureIndustryActivity,
		"industryActivityProducts.csv":  fixtureIndustryProducts,
		"industryActivityMaterials.csv": fixtureIndustryMaterials,
		sdeSkillsFileName:               fixtureIndustrySkills,
	})

	// Blueprint 2003 only reacts (activity 11): no planner row.
	if len(parsed.blueprints) != 2 {
		t.Fatalf("blueprints: got %d rows, want 2 (2001, 2002)", len(parsed.blueprints))
	}
	bp := parsed.blueprints[0]
	if bp.blueprintTypeID != 2001 || bp.productTypeID != 1001 || bp.productQuantity != 1 ||
		bp.maxProductionLimit != 300 || bp.manufacturingTimeSeconds != 600 {
		t.Errorf("blueprint 2001 joined wrong: %+v", bp)
	}
	if parsed.blueprints[1].manufacturingTimeSeconds != 300 {
		t.Errorf("blueprint 2002 time: got %d, want 300", parsed.blueprints[1].manufacturingTimeSeconds)
	}

	// Materials: manufacturing only — the activity-11 row (34×7
	// under 2003) must not appear.
	mats := map[[2]int64]int64{}
	for _, m := range parsed.bpMaterials {
		mats[[2]int64{m.blueprintTypeID, m.materialTypeID}] = m.quantity
	}
	if mats[[2]int64{2001, 1002}] != 2 || mats[[2]int64{2001, 34}] != 10 || mats[[2]int64{2002, 34}] != 5 {
		t.Errorf("materials wrong: %v", mats)
	}
	if _, bad := mats[[2]int64{2003, 34}]; bad {
		t.Error("reaction material (activity 11) leaked into the planner tables")
	}

	// Skills: manufacturing only — the invention skill (activity
	// 8) under 2001 must not appear.
	skills := map[[2]int64]int64{}
	for _, s := range parsed.bpSkills {
		skills[[2]int64{s.blueprintTypeID, s.skillTypeID}] = s.level
	}
	if len(skills) != 2 || skills[[2]int64{2001, 3380}] != 1 || skills[[2]int64{2002, 3380}] != 4 {
		t.Errorf("skills wrong: %v", skills)
	}
}

// ---------------------------------------------------------------------------
// Page rendering.
// ---------------------------------------------------------------------------

// seedPlannerSDE writes the planner's SDE rows straight into the
// tables (importer output without the network).
func seedPlannerSDE(t *testing.T, app *Application) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(34, 'Tritanium', 18, 0, 1), (1001, 'Fixture Widget', 0, 0, 1),
			(1002, 'Fixture Component', 0, 0, 1), (2001, 'Fixture Widget Blueprint', 0, 0, 1),
			(2002, 'Fixture Component Blueprint', 0, 0, 1), (3380, 'Industry', 0, 0, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES
			(2001, 1001, 1, 300, 600), (2002, 1002, 1, 60, 300)`,
		`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES
			(2001, 1002, 2), (2001, 34, 10), (2002, 34, 5)`,
		`INSERT INTO sde_blueprint_skills (blueprint_type_id, skill_type_id, level) VALUES
			(2001, 3380, 1), (2002, 3380, 4)`,
	}
	for _, stmt := range stmts {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed planner sde: %v", err)
		}
	}
}

func TestPlannerWarmState(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/planner/")
	if code != http.StatusOK {
		t.Fatalf("planner warm page: status %d", code)
	}
	mustContain(t, "/planner/", body, "hasn't landed yet")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handler made %d outbound calls, want 0", got)
	}
}

func TestPlannerRendersFromLocalData(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlannerSDE(t, app)

	// Owned Component blueprint (ME 8 / TE 12) and 15 Tritanium
	// in the hangar; the plan must prefill from both.
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 2, TypeID: 34, Quantity: 15, LocationID: 60003760, LocationType: "station"},
	})
	app.prices[34] = esi.MarketPrice{TypeID: 34, AveragePrice: 5}
	app.prices[1001] = esi.MarketPrice{TypeID: 1001, AveragePrice: 1000}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/planner/?q=Fixture")
	if code != http.StatusOK {
		t.Fatalf("planner search: status %d", code)
	}
	mustContain(t, "/planner/?q=Fixture", body, "/planner/?product=1001", "Fixture Widget")

	code, body = getPage(t, app, cookie, "/planner/?product=1001&runs=1")
	if code != http.StatusOK {
		t.Fatalf("planner plan: status %d", code)
	}
	mustContain(t, "/planner/?product=1001", body,
		"Fixture Widget",
		"Fixture Component",
		"Tritanium",
		"ME 8% · TE 12% (owned blueprint)",
		"Requires Industry I", // root blueprint skill line (skill type name from SDE)
		`<a href="/items/type/34/">Tritanium</a> — 5 × 5.00 ISK = 25.00 ISK`, // 20 needed, 15 on hand, 5 left at 5 ISK
		"975.00 ISK", // margin: 1,000.00 value − 25.00 to buy
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handler made %d outbound calls, want 0", got)
	}

	// A direct pick with no blueprint is a friendly dead end.
	code, body = getPage(t, app, cookie, "/planner/?product=999999")
	if code != http.StatusOK {
		t.Fatalf("planner unknown product: status %d", code)
	}
	mustContain(t, "/planner/?product=999999", body, "no manufacturing blueprint")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handler made %d outbound calls, want 0", got)
	}
}

// TestMigration011Reopen proves the schema bootstrap is
// idempotent on reopen, like every schema before it.
func TestMigration011Reopen(t *testing.T) {
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var tables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('sde_blueprints', 'sde_blueprint_materials', 'sde_blueprint_skills')`).Scan(&tables); err != nil {
		t.Fatalf("information_schema: %v", err)
	}
	if tables != 3 {
		t.Fatalf("planner tables: %d, want 3", tables)
	}
	conn.Close()
	pool.Close()
	conn, pool, err = store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	conn.Close()
	pool.Close()
}
