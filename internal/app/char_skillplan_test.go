package app

// Hermetic tests for skill plans:
//
//   - Importer: dgmTypeAttributes parses with the skill-relevant
//     attribute filter; real rows copied from the live dump (Gunnery
//     3300, Covert Ops 12093, Gallente Carrier 24313, Capital Ships
//     20533) flatten into meta + requirements, category-16/published
//     gating included, and the discovered 4th/5th requirement pairs
//     are honored.
//   - Engine: SP table, topo ordering with prerequisite expansion,
//     trained/queued/partial-SP netting, time math, remap optimizer,
//     fit-requirement closure.
//   - Pages: /skills/ browse + /skills/plans render with zero
//     outbound calls (counting transport), plan CRUD round-trips,
//     plan-from-fit create, and warm states.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

// ---------------------------------------------------------------------------
// Importer fixtures: real rows of the SDE's types, groups, categories
// and typeDogma tables, as JSON Lines.
// ---------------------------------------------------------------------------

const fixtureDogma = `{"typeID":3300,"dogmaAttributes":[{"attributeID":180,"value":167},{"attributeID":181,"value":168},{"attributeID":275,"value":1},{"attributeID":441,"value":-2}]}
{"typeID":12093,"dogmaAttributes":[{"attributeID":180,"value":168},{"attributeID":181,"value":167},{"attributeID":182,"value":3327},{"attributeID":183,"value":3432},{"attributeID":275,"value":4},{"attributeID":277,"value":3},{"attributeID":278,"value":5}]}
{"typeID":20533,"dogmaAttributes":[{"attributeID":180,"value":167},{"attributeID":181,"value":168},{"attributeID":182,"value":20342},{"attributeID":275,"value":14},{"attributeID":277,"value":5}]}
{"typeID":24313,"dogmaAttributes":[{"attributeID":180,"value":167},{"attributeID":181,"value":168},{"attributeID":182,"value":20533},{"attributeID":183,"value":3336},{"attributeID":184,"value":3442},{"attributeID":275,"value":14},{"attributeID":277,"value":4},{"attributeID":278,"value":3},{"attributeID":279,"value":5},{"attributeID":1285,"value":21610},{"attributeID":1286,"value":4},{"attributeID":1287,"value":3},{"attributeID":1289,"value":21611}]}
{"typeID":54790,"dogmaAttributes":[{"attributeID":180,"value":165},{"attributeID":181,"value":166},{"attributeID":275,"value":1}]}
{"typeID":999001,"dogmaAttributes":[{"attributeID":275,"value":3}]}
`

const fixtureSkillTypes = `{"typeID":3300,"groupID":255,"name":"Gunnery","published":true}
{"typeID":3327,"groupID":257,"name":"Spaceship Command","published":true}
{"typeID":3432,"groupID":1216,"name":"Electronics Upgrades","published":true}
{"typeID":12093,"groupID":257,"name":"Covert Ops","published":true}
{"typeID":20342,"groupID":257,"name":"Advanced Spaceship Command","published":true}
{"typeID":20533,"groupID":257,"name":"Capital Ships","published":true}
{"typeID":24313,"groupID":257,"name":"Gallente Carrier","published":true}
{"typeID":54790,"groupID":4321,"name":"EDENCOM Frigate (old)","published":false}
{"typeID":999001,"groupID":255,"name":"Unreleased Test Skill","published":false}
`

const fixtureSkillGroups = `{"groupID":255,"categoryID":16,"name":"Gunnery"}
{"groupID":257,"categoryID":16,"name":"Spaceship Command"}
{"groupID":1216,"categoryID":16,"name":"Electronic Systems"}
{"groupID":4321,"categoryID":29,"name":"Precursor Ship"}
`

const fixtureSkillCategories = `{"categoryID":16,"name":"Skill"}
{"categoryID":29,"name":"Ship"}
`

func parseSkillFixture(t *testing.T) *parsedSDE {
	t.Helper()
	parsed := &parsedSDE{}
	files := map[string]string{
		"types":      fixtureSkillTypes,
		"groups":     fixtureSkillGroups,
		"categories": fixtureSkillCategories,
		"typeDogma":  fixtureDogma,
	}
	// Types, groups and categories parse before dogma, as in the
	// importer (buildSkillRows joins over them).
	for _, name := range []string{"types", "groups", "categories", "typeDogma"} {
		if err := parseSDEJSONLTable(name, strings.NewReader(files[name]), parsed); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
	}
	parsed.finishJSONL()
	parsed.buildSkillRows()
	return parsed
}

func TestDogmaImportBuildsSkillGraph(t *testing.T) {
	parsed := parseSkillFixture(t)

	meta := make(map[int64]sdeSkillMetaRow, len(parsed.skillMeta))
	for _, m := range parsed.skillMeta {
		meta[m.typeID] = m
	}
	// Only published category-16 skills: the cat-29 leftover and
	// both unpublished rows are out.
	if len(meta) != 4 {
		t.Fatalf("skill meta rows = %d, want 4 (only fixture skills carrying attribute 275) (%v)", len(meta), parsed.skillMeta)
	}
	gunnery := meta[3300]
	if gunnery.rank != 1 || gunnery.primaryAttr != 167 || gunnery.secondaryAttr != 168 {
		t.Errorf("Gunnery meta = %+v, want rank 1, Per 167, Wil 168", gunnery)
	}
	covert := meta[12093]
	if covert.rank != 4 || covert.primaryAttr != 168 || covert.secondaryAttr != 167 {
		t.Errorf("Covert Ops meta = %+v, want rank 4, Wil 168, Per 167", covert)
	}
	if _, ok := meta[54790]; ok {
		t.Error("category-29 type leaked into skill meta")
	}
	if _, ok := meta[999001]; ok {
		t.Error("unpublished skill leaked into skill meta")
	}

	reqs := make(map[int64]map[int64]int64)
	for _, r := range parsed.skillReqs {
		if reqs[r.typeID] == nil {
			reqs[r.typeID] = make(map[int64]int64)
		}
		reqs[r.typeID][r.skillTypeID] = r.level
	}
	wantCarrier := map[int64]int64{20533: 4, 3336: 3, 3442: 5, 21610: 4, 21611: 3}
	got := reqs[24313]
	if len(got) != len(wantCarrier) {
		t.Fatalf("Gallente Carrier requirements = %v, want %v", got, wantCarrier)
	}
	for skill, level := range wantCarrier {
		if got[skill] != level {
			t.Errorf("Gallente Carrier requirement %d = %d, want %d", skill, got[skill], level)
		}
	}
	if got := reqs[12093]; got[3327] != 3 || got[3432] != 5 {
		t.Errorf("Covert Ops requirements = %v, want 3327→3, 3432→5", got)
	}
}

// ---------------------------------------------------------------------------
// Pages + CRUD.
// ---------------------------------------------------------------------------

func seedSkillGraphRows(t *testing.T, app *Application) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(1001, 'Alpha Skill', 300, 0, 1),
			(1002, 'Beta Skill', 300, 0, 1),
			(1003, 'Gamma Skill', 301, 0, 1),
			(2001, 'Test Blaster', 400, 0, 1),
			(3001, 'Test Hull', 401, 0, 1)`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES
			(300, 'Test Gunnery', 16), (301, 'Test Navigation', 16),
			(400, 'Test Modules', 7), (401, 'Test Ships', 6)`,
		`INSERT INTO sde_skill_meta (type_id, rank, primary_attr, secondary_attr) VALUES
			(1001, 1, 165, 166), (1002, 2, 165, 166), (1003, 1, 167, 168)`,
		`INSERT INTO sde_requirements (type_id, skill_type_id, level) VALUES
			(1002, 1001, 3), (2001, 1002, 2)`,
	}
	for _, stmt := range stmts {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed SDE: %v", err)
		}
	}
}

func TestSkillPlanPagesAndCRUD(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	_ = user
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// buildCorpTestApp's user row: CreateUser returns it.
	userID := user.ID
	seedCharacter(t, q, userID, fixtureCharA, "Plan Tester")
	seedSkillGraphRows(t, app)

	// Snapshots: A at II (1414 SP), queue empty, attributes warm.
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 1414,
		Skills:  []esi.Skill{{SkillID: 1001, SkillpointsInSkill: 1414, ActiveSkillLevel: 2, TrainedSkillLevel: 2}},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAttributes, esi.Attributes{
		Charisma: 17, Intelligence: 30, Memory: 20, Perception: 25, Willpower: 20,
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFittings, esi.Fittings{
		{FittingID: 42, Name: "Test Fit", ShipTypeID: 3001, Items: []esi.FittingItem{{TypeID: 2001, Quantity: 1, Flag: "HiSlot0"}}},
	})
	// Unified character sheet also reads these; seed empty so GetCached
	// doesn't fall through to live ESI (test expects zero outbound calls).
	seedSnapshot(t, q, fixtureCharA, esi.SnapClones, esi.Clones{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapImplants, esi.Implants{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapShip, esi.Ship{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFatigue, esi.Fatigue{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 0.0)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpInfo, esi.Corporation{})

	cookie := sessionCookie(t, app, userID, fixtureCharA, "Plan Tester")

	post := func(path string, form url.Values) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// Character sheet: the skill catalog (browse) now lives on the
	// unified character page and renders trained state there.
	code, body := getPage(t, app, cookie, "/character/?character=90000001")
	if code != 200 {
		t.Fatalf("GET /character/ = %d", code)
	}
	mustContain(t, "/character/", body, "Skill browse", "Alpha Skill", "Test Gunnery")

	// The catalog itself renders on the sheet's browse surface,
	// with each skill's rank.
	code, body = getPage(t, app, cookie, "/character/?character=90000001&surface=browse")
	if code != 200 {
		t.Fatalf("GET /character/?surface=browse = %d", code)
	}
	mustContain(t, "/character/?surface=browse", body, "Beta Skill", "Rank 2x")

	// Create a plan through the form endpoint.
	code, _ = post("/skills/plans/create", url.Values{
		"character": {"90000001"}, "name": {"Road to Beta"},
	})
	if code != 200 && code != 303 {
		t.Fatalf("POST create = %d", code)
	}
	plans, err := q.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: userID, CharacterID: fixtureCharA})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %v, err %v", plans, err)
	}
	planID := plans[0].ID

	// Add Beta at I; the computed order must lead with Alpha III.
	code, _ = post("/skills/plans/item-add", url.Values{
		"character": {"90000001"}, "plan": {fmt.Sprint(planID)}, "skill": {"1002"}, "level": {"1"},
	})
	if code != 200 && code != 303 {
		t.Fatalf("POST item-add = %d", code)
	}

	code, body = getPage(t, app, cookie, fmt.Sprintf("/skills/plans?character=90000001&plan=%d", planID))
	if code != 200 {
		t.Fatalf("GET /skills/plans = %d", code)
	}
	mustContain(t, "plan editor", body,
		"Road to Beta", "Alpha Skill", `class="plan-prereq"`, "for Beta Skill", "Beta Skill", "in training order", "Total:",
		// the level boxes the plan trains, drawn like the character page's
		`class="lvl lvl-plan"`, `<i class="plan">`)

	// Fit preview + create.
	code, body = getPage(t, app, cookie, "/skills/plans/fit?character=90000001&fitting=42")
	if code != 200 {
		t.Fatalf("GET fit preview = %d", code)
	}
	mustContain(t, "fit preview", body, "Test Fit", "Alpha Skill", "Beta Skill")

	code, _ = post("/skills/plans/from-fit", url.Values{
		"character": {"90000001"}, "fitting": {"42"},
	})
	if code != 200 && code != 303 {
		t.Fatalf("POST from-fit = %d", code)
	}
	plans, err = q.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: userID, CharacterID: fixtureCharA})
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans after fit create = %v, err %v", plans, err)
	}
	var fitPlan *db.SkillPlan
	for i := range plans {
		if strings.HasPrefix(plans[i].Name, "Fit: ") {
			fitPlan = &plans[i]
		}
	}
	if fitPlan == nil {
		t.Fatalf("no fit plan in %v", plans)
	}
	items, err := q.ListSkillPlanItems(ctx, fitPlan.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("fit plan items = %v, err %v", items, err)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Errorf("outbound calls = %d, want 0", got)
	}
}

func TestSkillPlanWarmStates(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)

	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Cold Character")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Cold Character")

	// No SDE graph at all: loading state.
	code, body := getPage(t, app, cookie, "/skills/plans?character=90000001")
	if code != 200 {
		t.Fatalf("GET /skills/plans = %d", code)
	}
	mustContain(t, "plans warm state", body, "Skill data is still loading")
	if got := transport.calls.Load(); got != 0 {
		t.Errorf("outbound calls = %d, want 0", got)
	}
}

func TestMigration012Reopen(t *testing.T) {
	// Mirror of the 011 reopen test: a second open over the
	// same database applies nothing twice (and nothing breaks).
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'skill_plans'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("skill_plans table count = %d, err %v", n, err)
	}
	conn.Close()
	pool.Close()
	conn, pool, err = store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'skill_plans'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("skill_plans table count after reopen = %d, err %v", n, err)
	}
	conn.Close()
	pool.Close()
}
