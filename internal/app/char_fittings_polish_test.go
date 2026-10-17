package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"evesynapse/internal/esi"
)

// Fitting-polish fixtures: on top of fitTestApp's universe, a
// module that overflows the fixture frigate's CPU by a little
// (156 tf against 150) and a five-rung CPU hardwiring ladder in
// slot 6 (+1% .. +5% via a post-percent on cpuOutput), shaped
// like the real 'Gnome' implants in the SDE. 150 * 1.04 = 156
// exactly, so the solver must land on the 4% rung.
const (
	fitSolveHogID   = int64(3010)
	fitSolveHog2ID  = int64(3011)
	fitSolveImplant = int64(7104) // the +4% rung
)

func fitSolveSeed(t *testing.T, app *Application) {
	t.Helper()
	ctx := context.Background()
	seed := []string{
		`INSERT INTO sde_groups (group_id, category_id, name) VALUES (300, 20, 'Fixture Implants')`,
		`INSERT INTO sde_types (type_id, group_id, name, published, market_group_id) VALUES (3010, 100, 'Fixture CPU Hog', 1, 1)`,
		`INSERT INTO sde_types (type_id, group_id, name, published, market_group_id) VALUES (3011, 100, 'Fixture CPU Hog XL', 1, 1)`,
		`INSERT INTO sde_types (type_id, group_id, name, published, market_group_id) VALUES (7101, 300, 'Fixture Gnome CP-601', 1, 1),
		 (7102, 300, 'Fixture Gnome CP-602', 1, 1),
		 (7103, 300, 'Fixture Gnome CP-603', 1, 1),
		 (7104, 300, 'Fixture Gnome CP-604', 1, 1),
		 (7105, 300, 'Fixture Gnome CP-605', 1, 1)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES (3010, 50, 156), (3010, 30, 5), (3011, 50, 200), (3011, 30, 5)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		 (7101, 331, 6), (7101, 9601, 1), (7102, 331, 6), (7102, 9601, 2),
		 (7103, 331, 6), (7103, 9601, 3), (7104, 331, 6), (7104, 9601, 4),
		 (7105, 331, 6), (7105, 9601, 5)`,
		`INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES (3010, 12, 0), (3011, 12, 0),
		 (7101, 9101, 0), (7102, 9101, 0), (7103, 9101, 0), (7104, 9101, 0), (7105, 9101, 0)`,
		`INSERT INTO sde_effects (effect_id, name, category) VALUES (9101, 'fixtureCPUOutputBonus', 0)`,
		`INSERT INTO sde_effect_modifiers (effect_id, domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id)
		 VALUES (9101, 'shipID', 'ItemModifier', 48, 9601, 6, 0, 0)`,
		`INSERT INTO market_region_stats (region_id, type_id, typical_sell, avg_daily_volume, updated_at) VALUES (10000002, 7104, 1234567, 100, '2026-01-01T00:00:00Z')`,
		// The Drones skill for the gauge case: the fixture drone
		// requires it, and charA flies it at V (snapshot below),
		// so both carried drones count as in space (bandwidth
		// actually used).
		`INSERT INTO sde_types (type_id, group_id, name, published, market_group_id) VALUES (3436, 200, 'Fixture Drones', 1, 0)`,
		`INSERT INTO sde_requirements (type_id, skill_type_id, level) VALUES (5001, 3436, 1)`,
	}
	for _, stmt := range seed {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("solve seed: %v", err)
		}
	}
	seedSnapshot(t, app.queries, fixtureCharA, esi.SnapSkills, esi.Skills{Skills: []esi.Skill{
		{SkillID: fitSkillID, ActiveSkillLevel: 1, TrainedSkillLevel: 1, SkillpointsInSkill: 8000},
		{SkillID: 3436, ActiveSkillLevel: 5, TrainedSkillLevel: 5, SkillpointsInSkill: 256000},
	}})
}

type solveResponse struct {
	OK     bool   `json:"ok"`
	Solved bool   `json:"solved"`
	Note   string `json:"note"`
	Error  string `json:"error"`
	Picks  []struct {
		Slot       int     `json:"slot"`
		TypeID     int64   `json:"typeId"`
		Name       string  `json:"name"`
		Resource   string  `json:"resource"`
		Bonus      float64 `json:"bonus"`
		Price      float64 `json:"price"`
		PriceKnown bool    `json:"priceKnown"`
	} `json:"picks"`
}

func postSolve(t *testing.T, app *Application, cookie *http.Cookie, payload string) solveResponse {
	t.Helper()
	code, body := postFitJSON(t, app, cookie, "/fittings/solve-implants/", payload)
	if code != http.StatusOK {
		t.Fatalf("solve status = %d, body %q", code, body)
	}
	var out solveResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("solve response is not JSON: %v (%q)", err, body)
	}
	return out
}

// TestFitSolveImplantsEndToEnd: the hog fit (156 tf on a 150 tf
// hull) is solved by the +4% hardwiring — not the +3% (154.5,
// still over) and not the +5% — and the pick carries its Forge
// typical price.
func TestFitSolveImplantsEndToEnd(t *testing.T) {
	app, cookie := fitTestApp(t)
	fitSolveSeed(t, app)

	out := postSolve(t, app, cookie,
		`{"shipTypeId":1001,"items":[{"typeId":3010,"qty":1}],"charges":{},"pilot":0}`)
	if !out.OK || out.Error != "" {
		t.Fatalf("solve = %+v, want ok", out)
	}
	if !out.Solved {
		t.Fatalf("solve not solved: note %q", out.Note)
	}
	if len(out.Picks) != 1 {
		t.Fatalf("picks = %+v, want exactly one", out.Picks)
	}
	pick := out.Picks[0]
	if pick.TypeID != fitSolveImplant || pick.Slot != 6 || pick.Resource != "CPU" || pick.Bonus != 4 {
		t.Errorf("pick = %+v, want the +4%% CPU implant %d in slot 6", pick, fitSolveImplant)
	}
	if pick.Name != "Fixture Gnome CP-604" {
		t.Errorf("pick name = %q", pick.Name)
	}
	if !pick.PriceKnown || pick.Price != 1234567 {
		t.Errorf("pick price = %v (known %v), want Forge typical sell 1234567", pick.Price, pick.PriceKnown)
	}
	// The round trip: posting the pick as a document implant
	// override through the ordinary simulator makes the fit
	// legal — the overlay is the whole mechanism.
	code, body := postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3010,"qty":1}],"charges":{},"implants":{"6":7104},"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("simulate with overlay status = %d", code)
	}
	if strings.Contains(body, `class="fit-problems"`) {
		t.Errorf("fit with the solver's implant applied is still flagged:\n%s", body)
	}
}

// TestFitSolveImplantsHonestFailures: a hog no rung can carry
// gets the plain-language dead end, and a legal fit is told it
// needs nothing.
func TestFitSolveImplantsHonestFailures(t *testing.T) {
	app, cookie := fitTestApp(t)
	fitSolveSeed(t, app)

	out := postSolve(t, app, cookie,
		`{"shipTypeId":1001,"items":[{"typeId":3011,"qty":1}],"charges":{},"pilot":0}`)
	if !out.OK || out.Solved {
		t.Fatalf("unsolvable solve = %+v, want ok + not solved", out)
	}
	if !strings.Contains(out.Note, "CPU is over by 50 tf") ||
		!strings.Contains(out.Note, "strongest CPU hardwiring adds 5%") {
		t.Errorf("unsolvable note = %q", out.Note)
	}

	out = postSolve(t, app, cookie,
		`{"shipTypeId":1001,"items":[{"typeId":3001,"qty":1}],"charges":{},"pilot":0}`)
	if !out.OK || out.Solved || !strings.Contains(out.Note, "already fits") {
		t.Errorf("legal-fit solve = %+v, want the already-fits note", out)
	}
}

// TestFitPolishSimRender pins the other two features' render
// contract: the solve action appears in the Doesn't-fit ribbon
// exactly when the fit is over CPU/PG, and a drone fit's stats
// carry the used/max pairs the Drones-card gauges read (the
// drone group itself is in the fragment for the gauges to land
// on).
func TestFitPolishSimRender(t *testing.T) {
	app, cookie := fitTestApp(t)
	fitSolveSeed(t, app)

	// Over budget: the ribbon offers the solver.
	code, body := postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3010,"qty":1}],"charges":{},"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("overfit simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/ (overfit)", body,
		`data-fit-solve`, "Simulate implants to fit")

	// Legal: no ribbon, no solver.
	code, body = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3001,"qty":1}],"charges":{},"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("legal simulate status = %d", code)
	}
	if strings.Contains(body, "data-fit-solve") {
		t.Errorf("legal fit offers the implant solver:\n%s", body)
	}

	// Drones: two fixture drones use 20 of 25 Mbit/s and 10 of
	// 30 m3 of bay — the gauge source rows, with the drone group
	// the gauges attach to.
	code, body = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":5001,"qty":2}],"charges":{},"pilot":90000001}`)
	if code != http.StatusOK {
		t.Fatalf("drone simulate status = %d", code)
	}
	mustContain(t, "/fittings/simulate/ (drones)", body,
		`data-fit-group="drone"`, "Drone bandwidth", "20 / 25 Mbit/s",
		"Drone bay", "10 / 30 m³")
}
