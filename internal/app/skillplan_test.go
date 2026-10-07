package app

// Hermetic tests for Phase 4 (skill plans):
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
//     plan-from-fit create, and honest warm states.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

// ---------------------------------------------------------------------------
// Importer fixtures: real rows from the 2026-10-02 Fuzzwork dump
// (quoted CSV; dogma values ride in valueFloat, one valueInt row is
// added to prove both columns parse).
// ---------------------------------------------------------------------------

const fixtureDogma = `"typeID","attributeID","valueInt","valueFloat"
"3300","180","","167.0"
"3300","181","","168.0"
"3300","275","","1.0"
"3300","441","","-2.0"
"12093","180","","168.0"
"12093","181","","167.0"
"12093","182","","3327.0"
"12093","183","","3432.0"
"12093","275","","4.0"
"12093","277","","3.0"
"12093","278","","5.0"
"20533","180","","167.0"
"20533","181","","168.0"
"20533","182","","20342.0"
"20533","275","","14.0"
"20533","277","","5.0"
"24313","180","","167.0"
"24313","181","","168.0"
"24313","182","","20533.0"
"24313","183","","3336.0"
"24313","184","","3442.0"
"24313","275","","14.0"
"24313","277","","4.0"
"24313","278","","3.0"
"24313","279","","5.0"
"24313","1285","","21610.0"
"24313","1286","","4.0"
"24313","1287","","3.0"
"24313","1289","","21611.0"
"54790","180","","165.0"
"54790","181","","166.0"
"54790","275","","1.0"
"999001","275","3",""
`

const fixtureSkillTypes = `"typeID","groupID","typeName","published"
"3300","255","Gunnery","1"
"3327","257","Spaceship Command","1"
"3432","1216","Electronics Upgrades","1"
"12093","257","Covert Ops","1"
"20342","257","Advanced Spaceship Command","1"
"20533","257","Capital Ships","1"
"24313","257","Gallente Carrier","1"
"54790","4321","EDENCOM Frigate (old)","0"
"999001","255","Unreleased Test Skill","0"
`

const fixtureSkillGroups = `"groupID","groupName","categoryID"
"255","Gunnery","16"
"257","Spaceship Command","16"
"1216","Electronic Systems","16"
"4321","Precursor Ship","29"
`

const fixtureSkillCategories = `"categoryID","categoryName"
"16","Skill"
"29","Ship"
`

func parseSkillFixture(t *testing.T) *parsedSDE {
	t.Helper()
	parsed := &parsedSDE{markers: make(map[string]fileMarker)}
	files := map[string]string{
		"invTypes.csv":          fixtureSkillTypes,
		"invGroups.csv":         fixtureSkillGroups,
		"invCategories.csv":     fixtureSkillCategories,
		"dgmTypeAttributes.csv": fixtureDogma,
	}
	// Types/groups/categories parse before dogma in the real
	// importer too (buildSkillRows joins over them).
	for _, name := range []string{"invTypes.csv", "invGroups.csv", "invCategories.csv", "dgmTypeAttributes.csv"} {
		if err := parseSDEFile(name, strings.NewReader(files[name]), parsed); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
	}
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
// Engine.
// ---------------------------------------------------------------------------

func TestSPTable(t *testing.T) {
	cases := []struct {
		rank  float64
		level int
		want  int64
	}{
		{1, 1, 250}, {1, 2, 1414}, {1, 3, 8000}, {1, 4, 45255}, {1, 5, 256000},
		{5, 3, 40000}, {5, 2, 7071}, {14, 5, 3584000}, {2, 4, 90510},
	}
	for _, c := range cases {
		if got := spForLevel(c.rank, c.level); got != c.want {
			t.Errorf("spForLevel(%v, %d) = %d, want %d", c.rank, c.level, got, c.want)
		}
	}
	if got := levelForSP(1, 5000); got != 2 {
		t.Errorf("levelForSP(1, 5000) = %d, want 2 (partial level III progress)", got)
	}
	if got := levelForSP(1, 256000); got != 5 {
		t.Errorf("levelForSP(1, 256000) = %d, want 5", got)
	}
}

// fixtureGraph is a tiny hand-built skill graph:
//
//	A (rank 1, Int/Mem) — no prereqs
//	B (rank 2, Int/Mem) — requires A III
//	C (rank 1, Per/Wil) — no prereqs
//	M (a module type)   — requires B II
type fixtureGraph struct{}

func (fixtureGraph) Meta(id int64) (skillMeta, bool) {
	switch id {
	case 1001:
		return skillMeta{Rank: 1, Primary: attrIntelligence, Secondary: attrMemory}, true
	case 1002:
		return skillMeta{Rank: 2, Primary: attrIntelligence, Secondary: attrMemory}, true
	case 1003:
		return skillMeta{Rank: 1, Primary: attrPerception, Secondary: attrWillpower}, true
	}
	return skillMeta{}, false
}

func (fixtureGraph) Requirements(id int64) []skillRequirement {
	switch id {
	case 1002:
		return []skillRequirement{{SkillID: 1001, Level: 3}}
	case 2001: // a module: requires B II
		return []skillRequirement{{SkillID: 1002, Level: 2}}
	}
	return nil
}

var testAttrs = attrSet{Charisma: 17, Intelligence: 30, Memory: 20, Perception: 25, Willpower: 20}

func TestComputePlanExpandsPrereqsAndOrders(t *testing.T) {
	char := charTraining{SP: map[int64]int64{1001: 1414}, QueuedTo: map[int64]int{}} // A at II
	out := computePlan(fixtureGraph{}, []planTarget{{SkillID: 1002, Level: 1, Intent: 1}}, char, testAttrs, time.Now())

	if len(out.Steps) != 2 {
		t.Fatalf("steps = %v, want A then B", out.Steps)
	}
	a, b := out.Steps[0], out.Steps[1]
	if a.SkillID != 1001 || !a.Prereq || a.FromLevel != 2 || a.ToLevel != 3 {
		t.Errorf("prereq step = %+v, want A II→III marked prereq", a)
	}
	if a.SPRemaining != 8000-1414 {
		t.Errorf("A remaining SP = %d, want %d", a.SPRemaining, 8000-1414)
	}
	if b.SkillID != 1002 || b.Prereq || b.FromLevel != 0 || b.ToLevel != 1 {
		t.Errorf("target step = %+v, want B 0→I", b)
	}
	if b.SPRemaining != 500 { // rank 2 level I = 500
		t.Errorf("B remaining SP = %d, want 500", b.SPRemaining)
	}
	if out.TotalSP != a.SPRemaining+b.SPRemaining {
		t.Errorf("total SP = %d, want %d", out.TotalSP, a.SPRemaining+b.SPRemaining)
	}
}

func TestComputePlanNetting(t *testing.T) {
	t.Run("partial SP counts", func(t *testing.T) {
		char := charTraining{SP: map[int64]int64{1001: 5000}, QueuedTo: map[int64]int{}}
		out := computePlan(fixtureGraph{}, []planTarget{{SkillID: 1001, Level: 3, Intent: 1}}, char, testAttrs, time.Now())
		if len(out.Steps) != 1 {
			t.Fatalf("steps = %v, want one", out.Steps)
		}
		if out.Steps[0].SPRemaining != 3000 || out.Steps[0].FromLevel != 2 {
			t.Errorf("step = %+v, want 3000 SP from level II", out.Steps[0])
		}
	})

	t.Run("queued levels net out as in queue", func(t *testing.T) {
		now := time.Now()
		char := charTraining{
			SP:       map[int64]int64{1001: 1414},
			QueuedTo: map[int64]int{1001: 3},
			QueueEnd: now.Add(2 * time.Hour),
		}
		out := computePlan(fixtureGraph{}, []planTarget{{SkillID: 1001, Level: 3, Intent: 1}}, char, testAttrs, now)
		if len(out.Steps) != 0 {
			t.Fatalf("steps = %v, want none (queued)", out.Steps)
		}
		if len(out.Dropped) != 1 || out.Dropped[0].Reason != "already in queue" {
			t.Fatalf("dropped = %v, want already in queue", out.Dropped)
		}
		if !out.StartsAt.Equal(char.QueueEnd) {
			t.Errorf("StartsAt = %v, want queue end %v", out.StartsAt, char.QueueEnd)
		}
	})

	t.Run("target at trained level drops as trained", func(t *testing.T) {
		char := charTraining{SP: map[int64]int64{1001: 8000}, QueuedTo: map[int64]int{}}
		out := computePlan(fixtureGraph{}, []planTarget{{SkillID: 1001, Level: 2, Intent: 1}}, char, testAttrs, time.Now())
		if len(out.Steps) != 0 || len(out.Dropped) != 1 || out.Dropped[0].Reason != "already trained" {
			t.Fatalf("out = %+v, want dropped as already trained", out)
		}
	})

	t.Run("step times accumulate from queue end", func(t *testing.T) {
		now := time.Now()
		char := charTraining{SP: map[int64]int64{}, QueuedTo: map[int64]int{}, QueueEnd: now.Add(time.Hour)}
		out := computePlan(fixtureGraph{}, []planTarget{{SkillID: 1001, Level: 1, Intent: 1}}, char, testAttrs, now)
		if len(out.Steps) != 1 {
			t.Fatalf("steps = %v", out.Steps)
		}
		// 250 SP at Int 30 + Mem 20/2 = 40 SP/min → 6.25 min.
		wantFinish := char.QueueEnd.Add(375 * time.Second)
		if !out.Steps[0].Finish.Equal(wantFinish) {
			t.Errorf("finish = %v, want %v", out.Steps[0].Finish, wantFinish)
		}
	})
}

func TestRemapAdvisorPicksDominantAttributes(t *testing.T) {
	steps := []planStep{
		{SkillID: 1003, SPRemaining: 100000}, // Per/Wil skill
	}
	advice := adviseRemap(fixtureGraph{}, steps, flatAttrSet)
	if advice.Best.Perception != 27 || advice.Best.Willpower != 21 {
		t.Errorf("best spread = %+v, want Perception 27 / Willpower 21 (primary takes the 10-point cap, secondary the rest)", advice.Best)
	}
	if advice.BestSeconds >= advice.CurSeconds {
		t.Errorf("remap should beat flat 20s: best %v, current %v", advice.BestSeconds, advice.CurSeconds)
	}
}

func TestFitClosureExpandsThroughSkills(t *testing.T) {
	targets := fitSkillClosure(fixtureGraph{}, []int64{2001})
	if len(targets) != 2 {
		t.Fatalf("closure = %v, want A then B", targets)
	}
	if targets[0].SkillID != 1001 || targets[0].Level != 3 {
		t.Errorf("first = %+v, want A III (prereq of B)", targets[0])
	}
	if targets[1].SkillID != 1002 || targets[1].Level != 2 {
		t.Errorf("second = %+v, want B II (the module's requirement)", targets[1])
	}
	if st := sortedTargets(targets); st[0].SkillID != 1001 {
		t.Errorf("sorted sanity: %v", st)
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
	mustContain(t, "/character/", body, "Browse skills", "Alpha Skill", "Test Gunnery", "2x")

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
		"Road to Beta", "Alpha Skill", "(prerequisite)", "Beta Skill", "Training order", "Total:")

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

	// Magic 14 template (names absent from this fixture SDE —
	// it must create nothing rather than crash).
	code, _ = post("/skills/plans/from-template", url.Values{
		"character": {"90000001"}, "template": {"magic14"},
	})
	if code != 200 && code != 303 {
		t.Fatalf("POST from-template = %d", code)
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

	// No SDE graph at all: honest loading state.
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
