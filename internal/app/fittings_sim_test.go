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
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"evesynapse/internal/esi"
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
			(100, 'Fixture Drones', 18), (399, 'Fixture Gunnery Group', 16)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(1001, 'Fixture Frigate', 25, 1, 1),
			(3001, 'Fixture Blaster', 59, 1, 1),
			(3002, 'Fixture Plate', 60, 1, 1),
			(4001, 'Fixture Charge S', 901, 1, 1),
			(5001, 'Fixture Drone', 100, 1, 1),
			(2001, 'Fixture Gunnery', 399, 0, 1)`,
		`INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES
			(3001, 12, 0), (3001, 42, 0),
			(3002, 11, 0), (3002, 2837, 1)`,
		`INSERT INTO sde_effects (effect_id, name, category) VALUES (2837, 'armorHPBonusAdd', 4)`,
		`INSERT INTO sde_effect_modifiers (effect_id, domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id)
			VALUES (2837, 'shipID', 'ItemModifier', 265, 1159, 2, 0, 0)`,
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
			(3001, 604, 901), (3001, 128, 1),
			(3002, 1159, 200), (3002, 30, 5), (3002, 50, 5),
			(4001, 128, 1), (4001, 118, 12), (4001, 117, 8),
			(5001, 1272, 10), (5001, 51, 2000), (5001, 64, 1.2), (5001, 117, 10)`,
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
	mustContain(t, "/fittings/", body, "Your saved fits", "Saved One", "Fixture Frigate")

	// Opening it loads the fit into the editor.
	_, body = getPage(t, app, cookie, "/fittings/?local="+strconv.FormatInt(saved.ID, 10))
	mustContain(t, "/fittings/", body, "Saved One", "Fixture Blaster", "Damage per second")

	code, _ = postFitForm(t, app, cookie, "/fittings/delete/", url.Values{"id": {strconv.FormatInt(saved.ID, 10)}})
	if code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", code)
	}
	_, body = getPage(t, app, cookie, "/fittings/")
	if strings.Contains(body, "Saved One") {
		t.Errorf("deleted fit still listed")
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
