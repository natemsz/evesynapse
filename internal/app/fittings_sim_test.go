package app

// Handler tests for the fitting simulator editor (fittings_sim.go):
// the simulate endpoint, the picker feeds, EFT round-trips, local
// save/list/delete, missing skills, and the zero-outbound-call
// rule across every editor endpoint. The SDE fixtures are a small
// synthetic universe (one frigate, a blaster, a plate, a charge,
// a drone, one gunnery skill) whose dogma numbers make the
// expected stats computable by hand:
//
//   blaster: 10 MW / 15 tf, 5s cycle, damage multiplier 5,
//     charge volley 20 (12 thermal + 8 kinetic) -> 20 DPS each
//   ship: 50 MW / 150 tf, 3/3/2 slots, 400 GJ capacitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
)

const (
	fitShipID    = int64(1001)
	fitBlasterID = int64(3001)
	fitPlateID   = int64(3002)
	fitChargeID  = int64(4001)
	fitDroneID   = int64(5001)
	fitSkillID   = int64(2001)
)

// seedFitUniverse writes the synthetic dogma rows via the test DB.
func seedFitUniverse(t *testing.T, exec func(string) error) {
	t.Helper()
	stmts := []string{
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES
			(25, 'Fixture Frigates', 6), (59, 'Fixture Blasters', 7),
			(60, 'Fixture Plates', 7), (901, 'Fixture Charges', 8),
			(100, 'Fixture Drones', 18), (399, 'Fixture Gunnery Group', 16),
			(61, 'Fixture Heaters', 7)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(1001, 'Fixture Frigate', 25, 1, 1),
			(3001, 'Fixture Blaster', 59, 1, 1),
			(3002, 'Fixture Plate', 60, 1, 1),
			(4001, 'Fixture Charge S', 901, 1, 1),
			(5001, 'Fixture Drone', 100, 1, 1),
			(2001, 'Fixture Gunnery', 399, 0, 1),
			(3003, 'Fixture Heater', 61, 1, 1)`,
		`INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES
			(3001, 12, 0), (3001, 42, 0),
			(3002, 11, 0), (3002, 2837, 1),
			(3003, 13, 0), (3003, 9001, 0), (3003, 9002, 0)`,
		`INSERT INTO sde_effects (effect_id, name, category) VALUES (2837, 'armorHPBonusAdd', 4),
			(9001, 'fixtureActiveBonus', 1), (9002, 'fixtureOverloadBonus', 5)`,
		`INSERT INTO sde_effect_modifiers (effect_id, domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id)
			VALUES (2837, 'shipID', 'ItemModifier', 265, 1159, 2, 0, 0),
			(9001, 'shipID', 'ItemModifier', 265, 9000, 2, 0, 0),
			(9002, 'itemID', 'ItemModifier', 9000, 9001, 6, 0, 0)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(1001, 14, 3), (1001, 13, 3), (1001, 12, 2), (1001, 1137, 3),
			(1001, 11, 50), (1001, 48, 150), (1001, 1132, 400),
			(1001, 482, 400), (1001, 55, 200000),
			(1001, 263, 400), (1001, 265, 450), (1001, 9, 500),
			(1001, 271, 1.0), (1001, 274, 0.8), (1001, 273, 0.7), (1001, 272, 0.5),
			(1001, 267, 0.5), (1001, 270, 0.75), (1001, 269, 0.75), (1001, 268, 0.6),
			(1001, 113, 1.0), (1001, 110, 1.0), (1001, 109, 1.0), (1001, 111, 1.0),
			(1001, 479, 400000), (1001, 37, 300), (1001, 552, 40),
			(1001, 76, 50000), (1001, 564, 900), (1001, 208, 12), (1001, 192, 5),
			(1001, 38, 350), (1001, 1271, 25), (1001, 283, 30), (1001, 70, 3.0),
			(3001, 30, 10), (3001, 50, 15), (3001, 73, 5000), (3001, 64, 5),
			(3001, 604, 901), (3001, 128, 1), (3001, 1692, 2),
			(3002, 1159, 200), (3002, 30, 5), (3002, 50, 5),
			(4001, 128, 1), (4001, 118, 12), (4001, 117, 8),
			(5001, 1272, 10), (5001, 51, 2000), (5001, 64, 1.2), (5001, 117, 10),
			(3003, 30, 10), (3003, 50, 10), (3003, 73, 5000), (3003, 6, 5),
			(3003, 9000, 10), (3003, 9001, 20)`,
		`INSERT INTO sde_type_physics (type_id, mass, volume, capacity) VALUES
			(1001, 1100000, 2500, 350), (5001, 0, 5, 0)`,
		`INSERT INTO sde_requirements (type_id, skill_type_id, level) VALUES
			(1001, 2001, 1), (3001, 2001, 3)`,
	}
	for _, stmt := range stmts {
		if err := exec(stmt); err != nil {
			t.Fatalf("seed fit universe: %v", err)
		}
	}
}

// fitTestApp builds the app, one user with two characters, and
// the synthetic universe. Its cleanup asserts the counting
// transport stayed at zero calls across everything the test did.
func fitTestApp(t *testing.T) (*Application, *http.Cookie) {
	t.Helper()
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")
	seedFitUniverse(t, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
	// An empty, fresh EVE fittings snapshot keeps the page render
	// cache-only; the pilot flies Fixture Gunnery at level 1.
	seedSnapshot(t, q, fixtureCharA, esi.SnapFittings, esi.Fittings{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{Skills: []esi.Skill{
		{SkillID: fitSkillID, ActiveSkillLevel: 1, TrainedSkillLevel: 1, SkillpointsInSkill: 8000},
	}})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	t.Cleanup(func() {
		if transport.calls.Load() != 0 {
			t.Errorf("editor endpoints made %d outbound calls, want 0", transport.calls.Load())
		}
	})
	return app, cookie
}

func postFitJSON(t *testing.T, app *Application, cookie *http.Cookie, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func postFitForm(t *testing.T, app *Application, cookie *http.Cookie, path string, form url.Values) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestFitSimulateStatsFragment: a valid fit returns the workbench
// fragment with hand-computable stats, and an over-budget fit is
// flagged as such.
func TestFitSimulateStatsFragment(t *testing.T) {
	app, cookie := fitTestApp(t)

	code, body := postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3001,"qty":2}],"charges":{"3001":4001},"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/", body,
		"Fixture Frigate", "Fixture Blaster", "Fixture Charge S",
		"Damage per second", "Powergrid", "Capacitor")
	// Two blasters at 20 DPS each: the total must read 40.
	mustContain(t, "/fittings/simulate/", body, ">40 — turrets 40 · missiles 0 · drones 0<")
	// Within budget: no over-budget marker anywhere in the bars.
	if strings.Contains(body, "fit-bar-row over") {
		t.Errorf("in-budget fit is flagged over budget:\n%s", body)
	}

	// Six blasters draw 60 MW from a 50 MW ship: flagged.
	code, body = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3001,"qty":6}],"charges":{"3001":4001},"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("overfit simulate status = %d", code)
	}
	mustContain(t, "/fittings/simulate/", body, "fit-bar-row over", "doesn't fit")
}

// TestFitSimulateMissingSkills: the chosen pilot's unmet
// requirements are listed with current -> needed levels.
func TestFitSimulateMissingSkills(t *testing.T) {
	app, cookie := fitTestApp(t)

	body := ""
	code, body := postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3001,"qty":1}],"charges":{"3001":4001},"pilot":90000001}`)
	if code != http.StatusOK {
		t.Fatalf("simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/", body,
		"Skills you're missing", "Fixture Gunnery", "1 → 3")

	// A pilot the user doesn't own is refused outright.
	code, _ = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[],"charges":{},"pilot":42424242}`)
	if code != http.StatusBadRequest {
		t.Fatalf("foreign pilot status = %d, want 400", code)
	}
}

// TestFitEFTRoundTrip: EFT text parses into the document and
// exports back to equivalent text.
func TestFitEFTRoundTrip(t *testing.T) {
	app, _ := fitTestApp(t)
	ctx := context.Background()

	text := "[Fixture Frigate, Test Fit]\n" +
		"Fixture Plate\n\n" +
		"Fixture Blaster, Fixture Charge S\n" +
		"Fixture Blaster, Fixture Charge S\n\n" +
		"Fixture Drone x3\n\n" +
		"Fixture Charge S x100\n" +
		"Not A Real Module\n"
	doc, notes := app.parseEFT(ctx, text)
	if doc.ShipTypeID != fitShipID || doc.Name != "Test Fit" {
		t.Fatalf("parsed header = ship %d name %q", doc.ShipTypeID, doc.Name)
	}
	if got := doc.Charges[fitBlasterID]; got != fitChargeID {
		t.Fatalf("charges[blaster] = %d, want %d", got, fitChargeID)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "Not A Real Module") {
		t.Fatalf("notes = %v, want the unknown line reported", notes)
	}
	qty := map[int64]int{}
	for _, it := range doc.Items {
		qty[it.TypeID] = it.Qty
	}
	if qty[fitBlasterID] != 2 || qty[fitPlateID] != 1 || qty[fitDroneID] != 3 || qty[fitChargeID] != 100 {
		t.Fatalf("parsed quantities = %v", qty)
	}

	out := app.formatEFT(ctx, doc)
	for _, want := range []string{
		"[Fixture Frigate, Test Fit]", "Fixture Plate",
		"Fixture Blaster, Fixture Charge S", "Fixture Drone x3", "Fixture Charge S x100",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("export missing %q in:\n%s", want, out)
		}
	}

	// Parse the export back: the documents must agree.
	doc2, notes2 := app.parseEFT(ctx, out)
	if len(notes2) != 0 {
		t.Fatalf("re-parse notes = %v", notes2)
	}
	if canonFitDoc(doc) != canonFitDoc(doc2) {
		t.Fatalf("round trip changed the fit:\n%s\nvs\n%s", canonFitDoc(doc), canonFitDoc(doc2))
	}
}

// canonFitDoc renders a document canonically for comparison.
func canonFitDoc(doc *fitDoc) string {
	cp := *doc
	sanitizeFitDoc(&cp)
	sort.Slice(cp.Items, func(i, j int) bool { return cp.Items[i].TypeID < cp.Items[j].TypeID })
	raw, _ := json.Marshal(cp)
	return string(raw)
}

// TestFitLocalSaveListDelete: the saved-fit flow over HTTP.
func TestFitLocalSaveListDelete(t *testing.T) {
	app, cookie := fitTestApp(t)

	code, body := postFitJSON(t, app, cookie, "/fittings/save/",
		`{"name":"Saved One","fit":{"name":"Saved One","shipTypeId":1001,"items":[{"typeId":3001,"qty":1}],"charges":{"3001":4001}}}`)
	if code != http.StatusOK {
		t.Fatalf("save status = %d, body %q", code, body)
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &saved); err != nil || saved.ID == 0 {
		t.Fatalf("save response = %q (id %d, err %v)", body, saved.ID, err)
	}

	code, body = getPage(t, app, cookie, "/fittings/")
	if code != http.StatusOK {
		t.Fatalf("page status = %d", code)
	}
	// The editor no longer embeds the your-fits list; the search
	// bar above finds saved fits through mine.json.
	mustContain(t, "/fittings/", body, "Ship Loadouts", "fit-yourfits-search")
	if strings.Contains(body, "Saved One") {
		t.Errorf("editor embeds the saved fit list; it should only offer the search bar")
	}
	_, mbody := getPage(t, app, cookie, "/fittings/mine.json?q=Saved")
	mustContain(t, "/fittings/mine.json", mbody, "Saved One")

	// Opening it loads the fit into the editor.
	_, body = getPage(t, app, cookie, "/fittings/?local="+strconv.FormatInt(saved.ID, 10))
	mustContain(t, "/fittings/", body, "Saved One", "Fixture Blaster", "Damage per second")

	code, _ = postFitForm(t, app, cookie, "/fittings/delete/", url.Values{"id": {strconv.FormatInt(saved.ID, 10)}})
	if code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", code)
	}
	_, mbody = getPage(t, app, cookie, "/fittings/mine.json?q=Saved")
	if strings.Contains(mbody, "Saved One") {
		t.Errorf("deleted fit still listed")
	}
}

// TestFitBuildVisual: the visual fit precomputes one circle per
// slot on arcs around the ship, filled circles carrying modules,
// positions inside the box with highs on top and lows below.
func TestFitBuildVisual(t *testing.T) {
	res := &fit.Result{HighSlots: 3, MediumSlots: 3, LowSlots: 2, RigSlots: 3}
	doc := &fitDoc{ShipTypeID: fitShipID, Items: []fitDocItem{
		{TypeID: fitBlasterID, Qty: 2},
		{TypeID: fitPlateID, Qty: 1},
	}}
	familyOf := map[int64]string{fitBlasterID: fitFamilyHigh, fitPlateID: fitFamilyLow}
	nameOf := func(id int64) string { return "Type " + strconv.FormatInt(id, 10) }

	v := fitBuildVisual(res, doc, nil, familyOf, nameOf, false)
	if v.ShipID != fitShipID {
		t.Errorf("ShipID = %d, want %d", v.ShipID, fitShipID)
	}
	// 3 high + 3 mid + 2 low + 3 rig circles; no subsystems fitted.
	if len(v.Slots) != 11 {
		t.Fatalf("slots = %d, want 11", len(v.Slots))
	}
	filled := 0
	for _, s := range v.Slots {
		if s.X < 0 || s.X > 100 || s.Y < 0 || s.Y > 100 {
			t.Errorf("slot at (%.1f, %.1f) outside the box", s.X, s.Y)
		}
		if s.Filled {
			filled++
			if s.TypeID == 0 || s.Name == "" {
				t.Errorf("filled slot missing type/name")
			}
		}
		switch s.GroupKey {
		case fitFamilyHigh:
			if s.Y >= 50 {
				t.Errorf("high slot at y=%.1f, want above center", s.Y)
			}
		case fitFamilyLow:
			if s.Y <= 50 {
				t.Errorf("low slot at y=%.1f, want below center", s.Y)
			}
		}
	}
	if filled != 3 {
		t.Errorf("filled = %d, want 3", filled)
	}
	// Ember dividers + in-ring captions: highs, mids, lows, rigs
	// render (no subsystems fitted), so 4 labels and 3 dividers
	// (high|mid, mid|low, low|rig).
	wantLabels := []string{"HIGH", "MID", "LOW", "RIG"}
	if len(v.Labels) != len(wantLabels) {
		t.Fatalf("labels = %d, want %d", len(v.Labels), len(wantLabels))
	}
	for i, want := range wantLabels {
		l := v.Labels[i]
		if l.Text != want {
			t.Errorf("label %d = %q, want %q", i, l.Text, want)
		}
		if l.X < 0 || l.X > 100 || l.Y < 0 || l.Y > 100 {
			t.Errorf("label %q at (%.1f, %.1f) outside the box", l.Text, l.X, l.Y)
		}
	}
	if len(v.Separators) != 3 {
		t.Fatalf("separators = %d, want 3", len(v.Separators))
	}
	for i, s := range v.Separators {
		if s.X < 0 || s.X > 100 || s.Y < 0 || s.Y > 100 {
			t.Errorf("separator %d at (%.1f, %.1f) outside the box", i, s.X, s.Y)
		}
	}
}

// TestFitVisualMetaName pins the meta-group vocabulary against the
// SDE metaGroupID attribute (1692): a Tech II module must read
// Tech II, never the Tech I default.
func TestFitVisualMetaName(t *testing.T) {
	cases := map[int]string{
		1: "Tech I",
		2: "Tech II",
		3: "Storyline",
		4: "Faction",
		5: "Officer",
		6: "Deadspace",
	}
	for group, want := range cases {
		if got := fitVisualMetaName(group); got != want {
			t.Errorf("meta group %d = %q, want %q", group, got, want)
		}
	}
	if got := fitVisualMetaName(0); got != "Tech I" {
		t.Errorf("unmarked meta group = %q, want the Tech I default", got)
	}
}

// TestFitBuildVisualTooltips pins the tooltip contract: meta comes
// from the static SDE metaGroupID (a Tech II module reads Tech
// II), CPU/PG come from the engine's effective attributes (skills
// and bonuses applied — not the raw SDE row), and a loaded charge
// rides the slot for the badge and the tooltip.
func TestFitBuildVisualTooltips(t *testing.T) {
	const gunID = int64(3001)
	res := &fit.Result{
		HighSlots: 3,
		ItemAttrs: map[int64]map[int64]float64{
			// Effective (post-dogma) CPU differs from the raw 15:
			// the tooltip must show the effective figure.
			gunID: {fit.AttrCPU: 12, fit.AttrPower: 10, fit.AttrDamageMultiplier: 5},
		},
	}
	doc := &fitDoc{
		ShipTypeID: fitShipID,
		Items:      []fitDocItem{{TypeID: gunID, Qty: 1}},
		Charges:    map[int64]int64{gunID: 4001},
	}
	snap := &fit.Snapshot{
		Attrs: map[int64]map[int64]float64{
			gunID: {fit.AttrCPU: 15, fit.AttrPower: 10, 1692: 2},
			4001:  {},
		},
	}
	familyOf := map[int64]string{gunID: fitFamilyHigh}
	names := map[int64]string{gunID: "Fixture Blaster", 4001: "Fixture Charge S"}
	nameOf := func(id int64) string { return names[id] }

	v := fitBuildVisual(res, doc, snap, familyOf, nameOf, false)
	var slot *fitVisualSlot
	for i := range v.Slots {
		if v.Slots[i].Filled && v.Slots[i].TypeID == gunID {
			slot = &v.Slots[i]
			break
		}
	}
	if slot == nil {
		t.Fatalf("no filled slot for the blaster")
	}
	if slot.Meta != "Tech II" {
		t.Errorf("Meta = %q, want Tech II", slot.Meta)
	}
	if slot.CPU != "12 tf" {
		t.Errorf("CPU = %q, want the effective 12 tf (raw is 15)", slot.CPU)
	}
	if slot.ChargeID != 4001 || slot.ChargeName != "Fixture Charge S" {
		t.Errorf("charge = %d %q, want 4001 Fixture Charge S", slot.ChargeID, slot.ChargeName)
	}
	if slot.GroupLabel != "High slot" {
		t.Errorf("GroupLabel = %q, want High slot", slot.GroupLabel)
	}
	// Bordered segments: highs render, so one annular sector path.
	if len(v.Segs) != 1 {
		t.Fatalf("Segs = %d, want 1", len(v.Segs))
	}
	if !strings.HasPrefix(v.Segs[0].D, "M") || !strings.HasSuffix(v.Segs[0].D, "Z") {
		t.Errorf("segment path = %q, want a closed annular sector", v.Segs[0].D)
	}
}

// TestFitSimulateModuleStates: the simulate fragment carries
// each fitted module's valid states and current state on the
// visual slot (for the tooltip's state row), states re-simulate
// through the endpoint, and the overload note is gone now that
// heat is modeled. The Fixture Heater (3003) adds its bonus attr
// 9000 (=10) to armor HP when active, and its overload effect
// (category 5) raises the bonus to 12 when overheated.
func TestFitSimulateModuleStates(t *testing.T) {
	app, cookie := fitTestApp(t)

	// Default state (active): all four states offered, armor HP
	// 450 + 10 = 460, full PG/CPU.
	code, body := postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3003,"qty":1}],"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/ states",
		body,
		`data-tip-states="offline,online,active,overheated"`,
		`data-tip-state="active"`,
		`data-tip-key="3003:0"`,
		"<th>Armor</th><td>460</td>")
	if strings.Contains(body, "Heating a module") {
		t.Errorf("heat note still shown for a modeled overload effect")
	}

	// Overheated: the overload bonus applies (450 + 12 = 462) and
	// the slot carries the ember class.
	code, body = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3003,"qty":1,"states":["overheated"]}],"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("overheated simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/ overheated",
		body,
		`data-tip-state="overheated"`,
		"st-overheated",
		"<th>Armor</th><td>462</td>")
	if strings.Contains(body, "Heating a module") {
		t.Errorf("heat note shown for an overheated module")
	}

	// Offline: no PG, no CPU, no effect (armor HP back to 450),
	// dimmed slot.
	code, body = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3003,"qty":1,"states":["offline"]}],"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("offline simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/ offline",
		body,
		`data-tip-state="offline"`,
		"st-offline",
		"<th>Armor</th><td>450</td>",
		">0 MW of 50 MW (50 MW left)<")

	// Online but inactive: PG/CPU counted, effect not applied.
	code, body = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[{"typeId":3003,"qty":1,"states":["online"]}],"pilot":0}`)
	if code != http.StatusOK {
		t.Fatalf("online simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/ online",
		body,
		`data-tip-state="online"`,
		"<th>Armor</th><td>450</td>",
		">10 MW of 50 MW (40 MW left)<")
}

// TestFitESIFittingBody: the editor document maps to the ESI
// fitting body — one flag per module instance (HiSlot0-7 etc.),
// charges riding their weapon's slot, drones to DroneBay and cargo
// to Cargo. Over-slot documents are an error, never silently
// truncated.
func TestFitESIFittingBody(t *testing.T) {
	const (
		droneID  = int64(9001)
		cargoID  = int64(9002)
		chargeID = int64(9003)
	)
	doc := &fitDoc{
		Name:       "Test fit",
		ShipTypeID: fitShipID,
		Items: []fitDocItem{
			{TypeID: fitBlasterID, Qty: 2},
			{TypeID: fitPlateID, Qty: 1},
			{TypeID: droneID, Qty: 3},
			{TypeID: cargoID, Qty: 10},
		},
		Charges: map[int64]int64{fitBlasterID: chargeID},
	}
	familyOf := map[int64]string{
		fitBlasterID: fitFamilyHigh,
		fitPlateID:   fitFamilyLow,
		droneID:      fitFamilyDrone,
		cargoID:      fitFamilyCargo,
	}
	body, err := fitESIFittingBody("Test fit", doc, familyOf)
	if err != nil {
		t.Fatalf("fitESIFittingBody: %v", err)
	}
	if body.Name != "Test fit" || body.ShipTypeID != fitShipID {
		t.Errorf("body name/ship = %q/%d", body.Name, body.ShipTypeID)
	}
	if body.Description == "" {
		t.Error("body description should mark EveSynapse as the creator")
	}
	got := map[string][]int64{}
	qty := map[string]int64{}
	for _, it := range body.Items {
		got[it.Flag] = append(got[it.Flag], it.TypeID)
		qty[it.Flag] = it.Quantity
	}
	// One flag per module instance; the charge rides the first
	// blaster's slot (HiSlot0 carries both the module and its ammo).
	want := map[string][]int64{
		"HiSlot0":  {fitBlasterID, chargeID},
		"HiSlot1":  {fitBlasterID},
		"LoSlot0":  {fitPlateID},
		"DroneBay": {droneID},
		"Cargo":    {cargoID},
	}
	for flag, ids := range want {
		if fmt.Sprint(got[flag]) != fmt.Sprint(ids) {
			t.Errorf("flag %s = %v, want %v", flag, got[flag], ids)
		}
	}
	if qty["DroneBay"] != 3 || qty["Cargo"] != 10 {
		t.Errorf("drone/cargo quantities = %d/%d, want 3/10", qty["DroneBay"], qty["Cargo"])
	}
	if len(body.Items) != 6 { // 2 blasters + plate + drone + cargo + charge
		t.Errorf("items = %d, want 6", len(body.Items))
	}

	// Nine high-slot modules: no HiSlot8 exists, so this is an
	// error rather than a silent drop.
	doc.Items = []fitDocItem{{TypeID: fitBlasterID, Qty: 9}}
	if _, err := fitESIFittingBody("Test fit", doc, familyOf); err == nil {
		t.Error("9 high-slot modules should be an error, got nil")
	}
}

// TestFitEditorEndpointsAndImport: the picker feeds, the EFT
// import landing, and the EVE fit "View stats" deep link, all
// with the transport pinned at zero calls (see fitTestApp's
// cleanup assertion).
func TestFitEditorEndpointsAndImport(t *testing.T) {
	app, cookie := fitTestApp(t)

	// Picker feeds.
	_, body := getPage(t, app, cookie, "/fittings/picker.json?family=ship&q=Fixture")
	mustContain(t, "picker ship", body, "Fixture Frigate")
	_, body = getPage(t, app, cookie, "/fittings/picker.json?family=high&q=")
	mustContain(t, "picker high", body, "Fixture Blaster")
	_, body = getPage(t, app, cookie, "/fittings/picker.json?family=charge&weapon=3001")
	mustContain(t, "picker charge", body, "Fixture Charge S")
	_, body = getPage(t, app, cookie, "/fittings/picker.json?family=drone&q=")
	mustContain(t, "picker drone", body, "Fixture Drone")

	// Unified search: ships and modules together, kinded.
	_, body = getPage(t, app, cookie, "/fittings/picker.json?family=all&q=Fixture")
	mustContain(t, "picker all", body,
		`"kind":"ship"`, `"kind":"high"`, `"kind":"low"`, `"kind":"drone"`,
		"Fixture Frigate", "Fixture Blaster", "Fixture Drone")
	if strings.Contains(body, `"kind":"charge"`) {
		t.Errorf("unified search should not offer charges, got %s", body)
	}

	// Meta filter: the blaster is Tech II (meta group 2).
	_, body = getPage(t, app, cookie, "/fittings/picker.json?family=high&q=&meta=2")
	mustContain(t, "picker meta=2", body, "Fixture Blaster")
	_, body = getPage(t, app, cookie, "/fittings/picker.json?family=high&q=&meta=4")
	if strings.Contains(body, "Fixture Blaster") {
		t.Errorf("meta=4 should exclude the Tech II blaster, got %s", body)
	}

	// Usable-by-pilot without a pilot is a no-op, not an empty list.
	_, body = getPage(t, app, cookie, "/fittings/picker.json?family=high&q=&usable=1&pilot=0")
	mustContain(t, "picker usable no pilot", body, "Fixture Blaster")

	// Import lands the fit (and its notes) in the editor.
	code, _ := postFitForm(t, app, cookie, "/fittings/import/", url.Values{
		"eft": {"[Fixture Frigate, Imported]\nFixture Blaster, Fixture Charge S\nBogus Module X9\n"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("import status = %d, want 303", code)
	}
	_, body = getPage(t, app, cookie, "/fittings/")
	mustContain(t, "/fittings/ after import", body,
		"Fixture Frigate", "place: Bogus Module X9", "Damage per second")
}

// TestFitESIViewStats: a saved EVE fitting loaded through the
// page's View stats link renders its stats, cargo ammo loaded
// into the matching weapon group.
func TestFitESIViewStats(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedFitUniverse(t, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFittings, esi.Fittings{{
		FittingID: 42, Name: "EVE Saved", ShipTypeID: fitShipID,
		Items: []esi.FittingItem{
			{TypeID: fitBlasterID, Quantity: 1, Flag: "HiSlot0"},
			{TypeID: fitChargeID, Quantity: 100, Flag: "Cargo"},
		},
	}})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/fittings/?character=90000001&esi=42")
	if code != http.StatusOK {
		t.Fatalf("page status = %d", code)
	}
	mustContain(t, "/fittings/?esi=42", body,
		"EVE Saved", "Fixture Blaster", "Damage per second", "selected")
	if transport.calls.Load() != 0 {
		t.Errorf("made %d outbound calls, want 0", transport.calls.Load())
	}
}

// seedSnakeUniverse adds the High-grade Snake implant set to the
// synthetic universe with the real SDE values (see
// snakeSetFixture): the per-implant velocityBonus (315), the set
// attribute implantSetSerpentis (802), the ship postPercent
// effect (394), and the charID set-bonus effect (1261).
func seedSnakeUniverse(t *testing.T, exec func(string) error) {
	t.Helper()
	stmts := []string{
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (300, 'Fixture Snake Implants', 20)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(19540, 'High-grade Snake Alpha', 300, 1, 1),
			(19551, 'High-grade Snake Beta', 300, 1, 1),
			(19553, 'High-grade Snake Gamma', 300, 1, 1),
			(19554, 'High-grade Snake Delta', 300, 1, 1),
			(19555, 'High-grade Snake Epsilon', 300, 1, 1),
			(19556, 'High-grade Snake Omega', 300, 1, 1)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
			(19540, 315, 0.5), (19540, 802, 1.15),
			(19551, 315, 0.625), (19551, 802, 1.15),
			(19553, 315, 0.75), (19553, 802, 1.15),
			(19554, 315, 0.875), (19554, 802, 1.15),
			(19555, 315, 1.0), (19555, 802, 1.15),
			(19556, 802, 3.0)`,
		`INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES
			(19540, 394, 0), (19540, 1261, 0),
			(19551, 394, 0), (19551, 1261, 0),
			(19553, 394, 0), (19553, 1261, 0),
			(19554, 394, 0), (19554, 1261, 0),
			(19555, 394, 0), (19555, 1261, 0),
			(19556, 394, 0), (19556, 1261, 0)`,
		`INSERT INTO sde_effects (effect_id, name, category) VALUES
			(394, 'navigationVelocityBonusPostPercentMaxVelocityShip', 0),
			(1261, 'setBonusSerpentis', 0)`,
		`INSERT INTO sde_effect_modifiers (effect_id, domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id)
			VALUES (394, 'shipID', 'ItemModifier', 37, 315, 6, 0, 0),
				(1261, 'charID', 'LocationGroupModifier', 315, 802, 0, 300, 0)`,
	}
	for _, stmt := range stmts {
		if err := exec(stmt); err != nil {
			t.Fatalf("seed snake universe: %v", err)
		}
	}
}

// TestFitSimulateImplants: the full request path with a pilot
// whose active clone holds a full High-grade Snake set. The
// fixture frigate flies 300 m/s bare; the set bonus must land in
// the fragment's Speed row (374 m/s), the implant list must name
// the set, and the clone picker must offer the jump clone.
func TestFitSimulateImplants(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	exec := func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	}
	seedFitUniverse(t, exec)
	seedSnakeUniverse(t, exec)
	seedSnapshot(t, q, fixtureCharA, esi.SnapFittings, esi.Fittings{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapImplants,
		esi.Implants{19540, 19551, 19553, 19554, 19555, 19556})
	seedSnapshot(t, q, fixtureCharA, esi.SnapClones, esi.Clones{
		JumpClones: []esi.JumpClone{
			{JumpCloneID: 11, LocationID: 60000001, LocationType: "station",
				Name: "PvP clone", Implants: []int64{19540}},
			// No custom name (ESI leaves it empty): the label must
			// still distinguish this clone from others.
			{JumpCloneID: 98765432, LocationID: 60000001, LocationType: "station",
				Implants: []int64{19540, 19551}},
			// Generic ESI name gets the same fallback treatment.
			{JumpCloneID: 13579, LocationID: 60000001, LocationType: "station",
				Name: "Jump clone"},
		},
	})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	t.Cleanup(func() {
		if transport.calls.Load() != 0 {
			t.Errorf("editor endpoints made %d outbound calls, want 0", transport.calls.Load())
		}
	})

	// Active clone (default): full set -> 374 m/s.
	code, body := postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[],"charges":{},"pilot":90000001}`)
	if code != http.StatusOK {
		t.Fatalf("simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/", body, ">374 m/s<")
	mustContain(t, "/fittings/simulate/", body,
		"High-grade Snake Alpha", "High-grade Snake Omega")

	// Jump clone with a lone Alpha: set multiplier 1.15 on 0.5% ->
	// 300 * 1.00575 = 301.725 -> "302 m/s".
	code, body = postFitJSON(t, app, cookie, "/fittings/simulate/",
		`{"shipTypeId":1001,"items":[],"charges":{},"pilot":90000001,"clone":11}`)
	if code != http.StatusOK {
		t.Fatalf("clone simulate status = %d, body %q", code, body)
	}
	mustContain(t, "/fittings/simulate/", body, ">302 m/s<")

	// The clone picker lists the active clone and the jump clone.
	req := httptest.NewRequest(http.MethodGet, "/fittings/clones.json?pilot=90000001", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clones.json status = %d", rec.Code)
	}
	mustContain(t, "/fittings/clones.json", rec.Body.String(),
		"Active clone", "PvP clone", "Jump clone #5432", "Jump clone #3579")

	// A pilot the user doesn't own is refused.
	req = httptest.NewRequest(http.MethodGet, "/fittings/clones.json?pilot=42424242", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("foreign clones.json status = %d, want 200 with ok:false", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("foreign clones.json body = %q, want ok:false", rec.Body.String())
	}
}
