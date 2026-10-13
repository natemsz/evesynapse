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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
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
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
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
	app.prices[1002] = esi.MarketPrice{TypeID: 1002, AveragePrice: 500}
	if err := q.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
		SolarSystemID: 30000142, Activity: "manufacturing", CostIndex: 0.064,
	}); err != nil {
		t.Fatalf("seed cost index: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/planner/?q=Fixture")
	if code != http.StatusOK {
		t.Fatalf("planner search: status %d", code)
	}
	mustContain(t, "/planner/?q=Fixture", body, "/planner/?product=1001", "Fixture Widget")

	// The bare planner opens to a blank planner frame, not just
	// a search box on an empty page.
	code, body = getPage(t, app, cookie, "/planner/")
	if code != http.StatusOK {
		t.Fatalf("planner bare page: status %d", code)
	}
	mustContain(t, "/planner/ (blank)", body,
		"Nothing planned yet", "plan-stats", "Landed cost")

	code, body = getPage(t, app, cookie, "/planner/?product=1001&runs=1")
	if code != http.StatusOK {
		t.Fatalf("planner plan: status %d", code)
	}
	mustContain(t, "/planner/?product=1001", body,
		"Fixture Widget",
		"Fixture Component",
		"95.40 ISK",  // landed: 25.00 materials + 70.40 fees at a 6.4% index
		"904.60 ISK", // margin: 1,000.00 value − 95.40 landed
		"2.89K ISK/hour",
		"materials &#43; fees", // html/template escapes the plus
		"Building saves 904.60 ISK",
		"Landed cost",
		"Build tree",
		"Double-click to expand/collapse a node",
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

// TestPlannerJSONPayload serves the interactive blob: the tree
// with canonical preset totals the client cross-checks.
func TestPlannerJSONPayload(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	seedPlannerSDE(t, app)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 2, TypeID: 34, Quantity: 15, LocationID: 60003760, LocationType: "station"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, `[]`)
	app.prices[34] = esi.MarketPrice{TypeID: 34, AveragePrice: 5}
	app.prices[1001] = esi.MarketPrice{TypeID: 1001, AveragePrice: 1000}
	app.prices[1002] = esi.MarketPrice{TypeID: 1002, AveragePrice: 500}
	if err := q.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
		SolarSystemID: 30000142, Activity: "manufacturing", CostIndex: 0.064,
	}); err != nil {
		t.Fatalf("seed cost index: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1&format=json")
	if code != http.StatusOK {
		t.Fatalf("planner json status = %d, want 200", code)
	}
	var p planPayload
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("planner json decodes: %v", err)
	}
	if p.Root == nil || p.Root.ID != 1001 || len(p.Root.Kids) != 2 {
		t.Fatalf("payload root = %+v, want product 1001 with 2 kids", p.Root)
	}
	// The inspector's blueprint overlay rides the nodes: the
	// owned component BPO reports ME8/TE12 from the snapshot.
	var comp *planNodeDTO
	for _, k := range p.Root.Kids {
		if k.ID == 1002 {
			comp = k
		}
	}
	if comp == nil {
		t.Fatal("component node missing from payload tree")
	}
	if comp.ME != 8 || comp.TE != 12 || comp.MESource != "owned" {
		t.Errorf("component blueprint = ME%v/TE%v %q, want ME8/TE12 owned", comp.ME, comp.TE, comp.MESource)
	}
	// Fifteen mineral cover the direct ten; the component branch
	// (ME8) buys five at 5 ISK. Materials 25, fees 70.40 at a
	// 6.4% index, value 1,000.
	if p.Optimized.Landed != 9540 || p.Optimized.Profit != 90460 {
		t.Errorf("optimized landed/profit = %v/%v, want 9540/90460", p.Optimized.Landed, p.Optimized.Profit)
	}
	if p.Optimized.Fees != 7040 || !p.Optimized.FeesKnown {
		t.Errorf("optimized fees = %v (known %v), want 7040/true", p.Optimized.Fees, p.Optimized.FeesKnown)
	}
	if p.Optimized.Elapsed != 1128 {
		t.Errorf("optimized elapsed = %v, want 1128 (528 + 600)", p.Optimized.Elapsed)
	}
	if len(p.Optimized.Jobs) != 2 {
		t.Errorf("optimized jobs = %d, want 2", len(p.Optimized.Jobs))
	}
	if p.Full.Materials != p.Optimized.Materials || p.Full.Elapsed != p.Optimized.Elapsed {
		t.Error("full and optimized disagree where building saves everywhere")
	}
	// Buying the components prunes the subtree: the two
	// components at 500 against a 1,000 hull (the direct
	// mineral is stock-covered). Building wins, and the preset
	// says exactly that.
	if !p.BuyComp.Complete || !p.BuyComp.ProfKnown {
		t.Error("buycomp should be fully priced")
	}
	if p.BuyComp.Landed != 106720 || p.BuyComp.Profit != -6720 {
		t.Errorf("buycomp landed/profit = %v/%v, want 106720/-6720", p.BuyComp.Landed, p.BuyComp.Profit)
	}
	if p.BuyComp.Bought != 100000 || p.BuyComp.Built != 0 {
		t.Errorf("buycomp bought/built = %v/%v, want 100000/0", p.BuyComp.Bought, p.BuyComp.Built)
	}
	// Every preset's bought+built equals its materials: the
	// unit the client cross-checks.
	for name, preset := range map[string]planPresetTotals{"full": p.Full, "optimized": p.Optimized, "buycomp": p.BuyComp} {
		if preset.Bought+preset.Built != preset.Materials {
			t.Errorf("%s bought+built != materials", name)
		}
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handler made %d outbound calls, want 0", got)
	}
}

// TestPlannerJitaMarket switches the price source to Forge
// sell rows; missing rows stay missing, never silent.
func TestPlannerJitaMarket(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	seedPlannerSDE(t, app)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 2, TypeID: 34, Quantity: 15, LocationID: 60003760, LocationType: "station"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, `[]`)
	for _, row := range []struct {
		typeID int64
		sell   float64
	}{
		{34, 6}, {1001, 1100},
	} {
		if err := q.UpsertMarketTypePrice(ctx, db.UpsertMarketTypePriceParams{
			RegionID: 10000002, TypeID: row.typeID, SellPrice: row.sell,
		}); err != nil {
			t.Fatalf("seed jita price: %v", err)
		}
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1&market=jita&format=json")
	if code != http.StatusOK {
		t.Fatalf("planner jita json status = %d, want 200", code)
	}
	var p planPayload
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("planner jita json decodes: %v", err)
	}
	// Five mineral at 6 ISK Forge sell; the finished hull at
	// 1,100. The component has no Forge row, so it layers over
	// its guide price instead of going unpriced.
	if p.Optimized.Landed != 3000 || p.Optimized.Value != 110000 {
		t.Errorf("jita landed/value = %v/%v, want 3000/110000", p.Optimized.Landed, p.Optimized.Value)
	}
	if p.Market != "10000002" || !strings.Contains(p.PricesNote, "The Forge sell rows as of") || !strings.Contains(p.PricesNote, "cluster average elsewhere") {
		t.Errorf("market/note = %q/%q, want region id + layered note", p.Market, p.PricesNote)
	}
	code, body = getPage(t, app, cookie, "/planner/?product=1001&runs=1&market=jita")
	if code != http.StatusOK {
		t.Fatalf("planner jita page status = %d, want 200", code)
	}
	mustContain(t, "/planner/", body, "30.00 ISK", "The Forge")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handler made %d outbound calls, want 0", got)
	}
}

// TestPlannerRegionWithoutRowsFallsBackToGuide pins the
// capital-plan bug: picking a region with no sell rows on file
// must degrade to guide prices, never zero the plan (the old
// all-or-nothing Jita mode left every unpriced item at zero and
// the header at "partial" until a preset was clicked).
func TestPlannerRegionWithoutRowsFallsBackToGuide(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	seedPlannerSDE(t, app)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 2, TypeID: 34, Quantity: 15, LocationID: 60003760, LocationType: "station"},
	})
	app.prices[34] = esi.MarketPrice{TypeID: 34, AveragePrice: 5}
	app.prices[1001] = esi.MarketPrice{TypeID: 1001, AveragePrice: 1000}
	app.prices[1002] = esi.MarketPrice{TypeID: 1002, AveragePrice: 500}
	if err := q.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
		SolarSystemID: 30000142, Activity: "manufacturing", CostIndex: 0.064,
	}); err != nil {
		t.Fatalf("seed cost index: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1&market=10000043&format=json")
	if code != http.StatusOK {
		t.Fatalf("planner region json status = %d, want 200", code)
	}
	var p planPayload
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("planner region json decodes: %v", err)
	}
	if p.Market != "10000043" {
		t.Errorf("market = %q, want the picked region id", p.Market)
	}
	if !strings.Contains(p.PricesNote, "no sell rows on file yet; cluster average shown.") {
		t.Errorf("prices note = %q, want the guide fallback note", p.PricesNote)
	}
	// Same totals as the plain guide plan: 95.40 landed
	// (25.00 materials + 70.40 fees), 1,000.00 value.
	if p.Optimized.Landed != 9540 || p.Optimized.Value != 100000 || !p.Optimized.Known {
		t.Errorf("fallback landed/value/known = %v/%v/%v, want 9540/100000/true",
			p.Optimized.Landed, p.Optimized.Value, p.Optimized.Known)
	}
}

// TestPlannerStructureBonuses pins the facility math end to
// end: a Raitaru with T1 rigs in a high-sec system applies the
// SDE role bonus (materials ×0.99, time ×0.85, cost ×0.97) and
// the rig bonuses at the high-sec multiplier (materials −2%,
// time −20%), all read from sde_type_attributes.
func TestPlannerStructureBonuses(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	seedPlannerSDE(t, app)
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9457)`,
		// Raitaru role bonus: mat 0.99, cost 0.97, time 0.85.
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(35825, 2600, 0.99), (35825, 2601, 0.97), (35825, 2602, 0.85)`,
		// T1 ME rig: −2% materials; sec multipliers 1.0/1.9/2.1.
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(37146, 2594, -2), (37146, 2355, 1.0), (37146, 2356, 1.9), (37146, 2357, 2.1)`,
		// T1 TE rig: −20% time.
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(37162, 2593, -20), (37162, 2355, 1.0), (37162, 2356, 1.9), (37162, 2357, 2.1)`,
	} {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed facility sde: %v", err)
		}
	}
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, esi.Blueprints{
		{ItemID: 1, TypeID: 2002, Quantity: -1, MaterialEfficiency: 8, TimeEfficiency: 12, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 2, TypeID: 34, Quantity: 15, LocationID: 60003760, LocationType: "station"},
	})
	app.prices[34] = esi.MarketPrice{TypeID: 34, AveragePrice: 5}
	app.prices[1001] = esi.MarketPrice{TypeID: 1001, AveragePrice: 1000}
	app.prices[1002] = esi.MarketPrice{TypeID: 1002, AveragePrice: 500}
	if err := q.UpsertIndustryCostIndex(ctx, db.UpsertIndustryCostIndexParams{
		SolarSystemID: 30000142, Activity: "manufacturing", CostIndex: 0.064,
	}); err != nil {
		t.Fatalf("seed cost index: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	shoppingQty := func(url string) (planPayload, int64) {
		code, body := getPage(t, app, cookie, url)
		if code != http.StatusOK {
			t.Fatalf("planner %s status = %d, want 200", url, code)
		}
		var p planPayload
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("planner json decodes: %v", err)
		}
		for _, line := range p.Optimized.Shopping {
			if line.TypeID == 34 {
				return p, line.Qty
			}
		}
		return p, -1
	}

	// Baseline (NPC station): 20 needed, 15 on hand, 5 to buy.
	_, qty := shoppingQty("/planner/?product=1001&runs=1&format=json")
	if qty != 5 {
		t.Fatalf("station tritanium qty = %d, want 5", qty)
	}
	// Raitaru + T1 rigs: widget ceil(10 × 0.9702) = 10, component
	// ceil(5×2×0.92 × 0.9702) = 9 → 19 needed, 4 to buy; the job
	// also lands cheaper and faster than the station plan.
	p, qty := shoppingQty("/planner/?product=1001&runs=1&structure=35825&rig=t1&format=json")
	if qty != 4 {
		t.Errorf("raitatru tritanium qty = %d, want 4", qty)
	}
	if p.Optimized.Landed >= 9540 {
		t.Errorf("raitatru landed = %d, want below the station plan's 9540", p.Optimized.Landed)
	}
	code, body := getPage(t, app, cookie, "/planner/?product=1001&runs=1&structure=35825&rig=t1")
	if code != http.StatusOK {
		t.Fatalf("planner structure page status = %d, want 200", code)
	}
	mustContain(t, "/planner/ structure", body, "Building in Raitaru · T1 rigs", `value="35825" selected`)
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

// An empty preset (the mask buys the finished product, or stock
// covers everything) must marshal its lists as [], never null —
// the client dereferences both on first load, and a null shopping
// list is what blanked capital plans until a preset was clicked.
func TestPlannerEmptyPresetMarshalsLists(t *testing.T) {
	r := &planRollup{preset: planPresetOptimized, shop: make(map[int64]int64)}
	r.tot.Complete = true
	r.tot.FeesKnown = true
	root := &buildplan.Node{TypeID: 999, ProducedQty: 1}
	planFinishPreset(r, root, map[int64]esi.MarketPrice{}, 1,
		func(int64) string { return "X" },
		func(int64) (float64, bool) { return 0, false })
	raw, err := json.Marshal(r.tot)
	if err != nil {
		t.Fatalf("marshal totals: %v", err)
	}
	for _, want := range []string{`"shopping":[]`, `"jobs":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty preset marshaled %s missing %s (null lists break first load)", raw, want)
		}
	}
}
