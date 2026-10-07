package app

// Hermetic tests for Phase 3 (industry build planner):
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
// Engine fixtures.
//
//   1001 Widget     ← bp 2001: 2× Component(1002) + 10× Mineral(34)
//   1002 Component  ← bp 2002: 5× Mineral(34)
//   1005 Bulk Part  ← bp 2004: 100 units per run from 1× Mineral
//   1006 Bulk Thing ← bp 2005: 250× Bulk Part(1005)
//   1007 Loop A     ← bp 2006: 1× Loop B(1008)
//   1008 Loop B     ← bp 2007: 1× Loop A(1007)
//   1009 Solo       ← bp 2008: 1× Mineral(34)   (base-1 floor)
// ---------------------------------------------------------------------------

type fixturePlannerSource map[int64]*plannerBlueprint

func (f fixturePlannerSource) BlueprintForProduct(productTypeID int64) (*plannerBlueprint, bool) {
	bp, ok := f[productTypeID]
	return bp, ok
}

func fixtureRecipes() fixturePlannerSource {
	return fixturePlannerSource{
		1001: {BlueprintTypeID: 2001, ProductTypeID: 1001, ProductQuantity: 1, TimeSeconds: 600,
			Materials: []plannerMaterial{{TypeID: 1002, Quantity: 2}, {TypeID: 34, Quantity: 10}}},
		1002: {BlueprintTypeID: 2002, ProductTypeID: 1002, ProductQuantity: 1, TimeSeconds: 300,
			Materials: []plannerMaterial{{TypeID: 34, Quantity: 5}}},
		1005: {BlueprintTypeID: 2004, ProductTypeID: 1005, ProductQuantity: 100, TimeSeconds: 60,
			Materials: []plannerMaterial{{TypeID: 34, Quantity: 1}}},
		1006: {BlueprintTypeID: 2005, ProductTypeID: 1006, ProductQuantity: 1, TimeSeconds: 900,
			Materials: []plannerMaterial{{TypeID: 1005, Quantity: 250}}},
		1007: {BlueprintTypeID: 2006, ProductTypeID: 1007, ProductQuantity: 1, TimeSeconds: 10,
			Materials: []plannerMaterial{{TypeID: 1008, Quantity: 1}}},
		1008: {BlueprintTypeID: 2007, ProductTypeID: 1008, ProductQuantity: 1, TimeSeconds: 10,
			Materials: []plannerMaterial{{TypeID: 1007, Quantity: 1}}},
		1009: {BlueprintTypeID: 2008, ProductTypeID: 1009, ProductQuantity: 1, TimeSeconds: 10,
			Materials: []plannerMaterial{{TypeID: 34, Quantity: 1}}},
	}
}

func findPlanNode(root *planNode, typeID int64) *planNode {
	var hit *planNode
	walkPlan(root, func(n *planNode) {
		if hit == nil && n.TypeID == typeID {
			hit = n
		}
	})
	return hit
}

func leafQty(root *planNode, typeID int64) (required, toBuy int64) {
	walkPlan(root, func(n *planNode) {
		if n.TypeID == typeID && n.Blueprint == nil {
			required += n.RequiredQty
			toBuy += n.Shortfall
		}
	})
	return required, toBuy
}

func TestMaterialForRunsFormula(t *testing.T) {
	cases := []struct {
		base, runs int64
		me         int
		want       int64
	}{
		{10, 1, 0, 10},    // no ME
		{10, 1, 10, 9},    // full ME discount
		{2, 1, 10, 2},     // ceil(1.8)
		{5, 2, 10, 9},     // ceil(5×2×0.9)
		{1, 100, 10, 100}, // base-1 floor: never below runs
		{1, 1, 10, 1},
		{3, 7, 5, 20}, // ceil(3×7×0.95) = ceil(19.95)
		{86, 3, 0, 258},
		{86, 3, 10, 233}, // ceil(232.2)
	}
	for _, c := range cases {
		if got := materialForRuns(c.base, c.runs, c.me); got != c.want {
			t.Errorf("materialForRuns(%d, %d, %d) = %d, want %d", c.base, c.runs, c.me, got, c.want)
		}
	}
	if got := jobSeconds(600, 2, 20); got != 960 { // 600×2×0.8
		t.Errorf("jobSeconds(600,2,20) = %d, want 960", got)
	}
	if got := jobSeconds(300, 3, 0); got != 900 {
		t.Errorf("jobSeconds(300,3,0) = %d, want 900", got)
	}
}

func TestPlanExpansionMath(t *testing.T) {
	res, err := buildPlan(fixtureRecipes(), 1001, plannerInput{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	root := res.Root
	if root.Runs != 1 || root.ProducedQty != 1 {
		t.Errorf("root: runs %d produced %d, want 1/1", root.Runs, root.ProducedQty)
	}
	comp := findPlanNode(root, 1002)
	if comp == nil || comp.Runs != 2 {
		t.Fatalf("component node: %+v, want 2 runs", comp)
	}
	// Mineral: 10 direct + 2 runs × 5 under the component = 20.
	if req, buy := leafQty(root, 34); req != 20 || buy != 20 {
		t.Errorf("mineral: required %d to-buy %d, want 20/20", req, buy)
	}
	// Total time: root 600 + component 2×300 = 1200s sequential.
	if res.TotalSeconds != 1200 {
		t.Errorf("total seconds: got %d, want 1200", res.TotalSeconds)
	}
	if len(res.Shopping) != 1 || res.Shopping[0].TypeID != 34 || res.Shopping[0].Quantity != 20 {
		t.Errorf("shopping: %+v, want one 20× Mineral line", res.Shopping)
	}
}

func TestPlanMEOverridesAtConsumingNode(t *testing.T) {
	// ME 10 on the Widget blueprint: component need ceil(2×0.9)=2,
	// mineral need ceil(10×0.9)=9, component branch unchanged.
	res, err := buildPlan(fixtureRecipes(), 1001, plannerInput{
		Runs:       1,
		MEOverride: map[int64]int{2001: 10},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp := findPlanNode(res.Root, 1002)
	if comp.RequiredQty != 2 {
		t.Errorf("component required: got %d, want 2 (ceil of 1.8)", comp.RequiredQty)
	}
	if req, _ := leafQty(res.Root, 34); req != 19 { // 9 direct + 10 under component
		t.Errorf("mineral required: got %d, want 19", req)
	}

	// ME 10 on the Component blueprint instead: its mineral need
	// becomes ceil(5×2×0.9)=9; the root's direct 10 stays.
	res, err = buildPlan(fixtureRecipes(), 1001, plannerInput{
		Runs:       1,
		MEOverride: map[int64]int{2002: 10},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if req, _ := leafQty(res.Root, 34); req != 19 { // 10 direct + 9 under component
		t.Errorf("mineral required: got %d, want 19", req)
	}
	if comp := findPlanNode(res.Root, 1002); comp.ME != 10 || comp.MESource != "override" {
		t.Errorf("component ME/source: %d/%q, want 10/override", comp.ME, comp.MESource)
	}
}

func TestPlanOwnedBlueprintAutofillAndOverridePrecedence(t *testing.T) {
	res, err := buildPlan(fixtureRecipes(), 1001, plannerInput{
		Runs:  1,
		Owned: map[int64]ownedBlueprint{2002: {ME: 8, TE: 12}},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp := findPlanNode(res.Root, 1002)
	if comp.ME != 8 || comp.TE != 12 || comp.MESource != "owned" {
		t.Fatalf("component ME/TE/source: %d/%d/%q, want 8/12/owned", comp.ME, comp.TE, comp.MESource)
	}
	// Mineral under component: ceil(5×2×0.92) = 10; time with TE:
	// ceil(300×2×0.88) = 528.
	if comp.TimeSeconds != 528 {
		t.Errorf("component time: got %d, want 528", comp.TimeSeconds)
	}

	// An explicit ME override beats the owned copy's ME but keeps
	// its TE (same physical blueprint).
	res, err = buildPlan(fixtureRecipes(), 1001, plannerInput{
		Runs:       1,
		Owned:      map[int64]ownedBlueprint{2002: {ME: 8, TE: 12}},
		MEOverride: map[int64]int{2002: 3},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp = findPlanNode(res.Root, 1002)
	if comp.ME != 3 || comp.TE != 12 || comp.MESource != "override" {
		t.Errorf("override: ME/TE/source %d/%d/%q, want 3/12/override", comp.ME, comp.TE, comp.MESource)
	}
}

func TestPlanStockpileSingleAllocation(t *testing.T) {
	// 15 Mineral on hand: the root's own line (materials walk in
	// type-ID order) claims 10, the component's line gets the
	// remaining 5 — the stockpile is never promised twice.
	res, err := buildPlan(fixtureRecipes(), 1001, plannerInput{
		Runs:  1,
		Stock: map[int64]int64{34: 15, 1002: 1},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp := findPlanNode(res.Root, 1002)
	if comp.Have != 1 || comp.Shortfall != 1 || comp.Runs != 1 {
		t.Errorf("component: have %d shortfall %d runs %d, want 1/1/1", comp.Have, comp.Shortfall, comp.Runs)
	}
	totalHave := int64(0)
	walkPlan(res.Root, func(n *planNode) { totalHave += n.Have })
	if totalHave != 16 { // 15 mineral + 1 component, never more
		t.Errorf("allocated %d units from a 16-unit stockpile set", totalHave)
	}
	if req, buy := leafQty(res.Root, 34); req != 15 || buy != 0 {
		t.Errorf("mineral: required %d to-buy %d, want 15/0 (10 at the root, 5 under the single component run the stock leaves; all covered)", req, buy)
	}

	// Fully stocked component: no runs, no subtree expansion.
	res, err = buildPlan(fixtureRecipes(), 1001, plannerInput{
		Runs:  1,
		Stock: map[int64]int64{1002: 2},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp = findPlanNode(res.Root, 1002)
	if comp.Blueprint != nil || comp.Shortfall != 0 || len(comp.Children) != 0 {
		t.Errorf("stocked component should be an unexpanded have-leaf: %+v", comp)
	}
}

func TestPlanWholeRunChildProduction(t *testing.T) {
	// Bulk Thing needs 250 Bulk Parts; the part blueprint makes
	// 100 per run, so 3 runs / 300 produced / 50 surplus.
	res, err := buildPlan(fixtureRecipes(), 1006, plannerInput{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	part := findPlanNode(res.Root, 1005)
	if part == nil || part.Runs != 3 || part.ProducedQty != 300 || part.SurplusQty != 50 {
		t.Fatalf("bulk part node: %+v, want 3 runs / 300 / 50 surplus", part)
	}
	// Each part run eats 1 Mineral (base-1 floor holds at 3).
	if req, _ := leafQty(res.Root, 34); req != 3 {
		t.Errorf("mineral: got %d, want 3", req)
	}

	// Solo at 100 runs, ME 10: base-1 floor keeps the need at 100.
	res, err = buildPlan(fixtureRecipes(), 1009, plannerInput{
		Runs:       100,
		MEOverride: map[int64]int{2008: 10},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if req, _ := leafQty(res.Root, 34); req != 100 {
		t.Errorf("base-1 floor: got %d, want 100", req)
	}
}

func TestPlanCycleGuard(t *testing.T) {
	res, err := buildPlan(fixtureRecipes(), 1007, plannerInput{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	// Root A builds B; B's own A requirement loops back and must
	// come out as a flagged buy leaf, not infinite recursion.
	var cyc *planNode
	walkPlan(res.Root, func(n *planNode) {
		if n.Cycle {
			cyc = n
		}
	})
	if cyc == nil || cyc.TypeID != 1007 || cyc.Blueprint != nil {
		t.Fatalf("cycle node: %+v, want a flagged 1007 buy leaf", cyc)
	}
}

func TestPlanNotManufacturable(t *testing.T) {
	if _, err := buildPlan(fixtureRecipes(), 424242, plannerInput{Runs: 1}); err == nil {
		t.Error("unknown product should error")
	}
}

func TestPlanPriceHonesty(t *testing.T) {
	// No prices at all: costs stay unknown, quantities don't.
	res, err := buildPlan(fixtureRecipes(), 1001, plannerInput{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if res.UnpricedLines != 1 || res.Shopping[0].PriceKnown {
		t.Errorf("unpriced shopping line not flagged: %+v", res.Shopping)
	}
	if res.Root.CostComplete {
		t.Error("root cost must be incomplete when a price is missing")
	}

	// Adjusted price stands in when the average is absent.
	res, err = buildPlan(fixtureRecipes(), 1001, plannerInput{
		Runs:   1,
		Prices: map[int64]esi.MarketPrice{34: {TypeID: 34, AdjustedPrice: 3.5}},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !res.Root.CostComplete || res.Root.LineCost != 70 { // 20 × 3.5
		t.Errorf("root line cost: got %v (complete %v), want 70/true", res.Root.LineCost, res.Root.CostComplete)
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
