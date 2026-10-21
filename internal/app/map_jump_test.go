package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestMapJump: the jump data gives every system's true position in
// light years and each jump-capable ship's range from the SDE (its
// fatigue multiplier falling back on the ship's group); the reader's
// side carries their pilots' Jump Drive Calibration and fatigue, and
// the Ansiblex bridges named in their corporation's structures.
func TestMapJump(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security, x, y, z) VALUES
		(30004759, '1DQ1-A', 10000060, -0.38, 9460730472580800, 0, 0),
		(30004758, 'Y-OMTZ', 10000060, -0.3, 0, 18921460945161600, 0) ON CONFLICT (system_id) DO UPDATE SET x = EXCLUDED.x, y = EXCLUDED.y, z = EXCLUDED.z`)
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (902, 'Jump Freighter', 6) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name, group_id, published) VALUES (28850, 'Ark', 902, 1) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_attribute_types (attribute_id, name) VALUES (867, 'jumpDriveRange') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES (28850, 867, 5) ON CONFLICT DO NOTHING`)
	if err := q.UpsertMapUniverse(ctx, db.UpsertMapUniverseParams{Version: 1, Etag: "u1", Payload: []byte{}, Systems: 2, BuiltAt: time.Now()}); err != nil {
		t.Fatalf("map meta: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/map/jump.json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jump.json: %d %s", rec.Code, rec.Body.String())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	var p jumpPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Ships) != 1 || p.Ships[0].Range != 5 || p.Ships[0].Fatigue != 0.1 || p.Ships[0].Group != "Jump Freighter" {
		t.Errorf("ships: %+v", p.Ships)
	}
	at := map[int64]int{}
	for i, id := range p.ID {
		at[id] = i
	}
	if i, ok := at[30004759]; !ok || p.X[i] != 1 {
		t.Errorf("1DQ1-A sits 1 LY out: %+v", p)
	}
	if i, ok := at[30004758]; !ok || p.Y[i] != 2 {
		t.Errorf("Y-OMTZ sits 2 LY up: %+v", p)
	}

	// The reader's side.
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000401, "Jump Pilot")
	cookie := sessionCookie(t, app, u.ID, 90000401, "Jump Pilot")
	seedSnapshot(t, q, 90000401, esi.SnapSkills, esi.Skills{Skills: []esi.Skill{{SkillID: skillJDC, ActiveSkillLevel: 4, TrainedSkillLevel: 4}}})
	seedSnapshot(t, q, 90000401, esi.SnapFatigue, esi.Fatigue{JumpFatigueExpireDate: "2099-01-01T00:00:00Z"})
	seedSnapshot(t, q, 90000401, esi.SnapCorpStructures, esi.CorpStructures{
		{StructureID: 1, TypeID: ansiblexTypeID, SystemID: 30004759, Name: "1DQ1-A » Y-OMTZ - Home Bridge", State: "shield_vulnerable"},
		{StructureID: 2, TypeID: 35832, SystemID: 30004759, Name: "1DQ1-A - Keepstar"},
	})
	_, body := getPage(t, app, cookie, "/map/mine.json?fresh=1")
	var d minePayload
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Jumpers) != 1 || d.Jumpers[0].JDC != 4 || !d.Jumpers[0].Known || d.Jumpers[0].Fatigue == 0 {
		t.Errorf("jumpers: %+v", d.Jumpers)
	}
	if len(d.Bridges) != 1 || d.Bridges[0].From != 30004759 || d.Bridges[0].To != 30004758 {
		t.Errorf("the Ansiblex, read off its name: %+v", d.Bridges)
	}
}
