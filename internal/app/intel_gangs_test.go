package app

// Tracked gangs (intel_gangs.go): the fusion layer's pins are
// the silent-and-costly ones. A merge that joins two gangs
// misstates who is being tracked; a confidence that does not
// decay presents a stale sighting as a live one; a closed gang
// that quietly resurrects rewrites its own history; a likely
// ship attributed to a pilot with no record is a guess wearing
// intel's clothes. This is the one fixture test the fusion
// constants answer to.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/scan"
)

const (
	grpSystem3 = int64(30004761)
	grpPilot4  = int64(2124000404)
	grpPilot5  = int64(2124000405)
)

func (f *groupFixture) gangOf(t *testing.T, reportID int64) *trackedGang {
	t.Helper()
	g := f.app.gangForGroup(context.Background(), f.groupOf(t, reportID))
	if g == nil {
		t.Fatalf("report %d stands in no gang", reportID)
	}
	return g
}

func TestIntelGangFusion(t *testing.T) {
	f := seedGroupFixture(t)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := f.app.db.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	// A third system and the gates the distance reads walk:
	// grpSystem - grpSystem2 - grpSystem3, one jump each.
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30004761, 'F-9P49', 10000060, -0.30) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_system_jumps (from_system_id, to_system_id) VALUES
		(30004759, 30004760), (30004760, 30004761) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (587, 'Rifter') ON CONFLICT DO NOTHING`)
	for _, p := range []struct {
		id, corp int64
		name     string
	}{{grpPilot4, 98000999, "Grp Pilot Four"}, {grpPilot5, 98000777, "Grp Pilot Five"}} {
		if err := f.q.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{
			NameLower: lowerName(p.name), CharacterID: p.id, Name: p.name, CheckedAt: f.base}); err != nil {
			t.Fatal(err)
		}
		if err := f.q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{
			CharacterID: p.id, CorporationID: p.corp, CheckedAt: f.base}); err != nil {
			t.Fatal(err)
		}
	}

	// The same three pilots sighted in one system found a gang;
	// a second report there extends it.
	r1 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000101",
		grpLocal(grpPilots()), "", "3 in local in 1DQ1-A", f.base)
	g1 := f.gangOf(t, r1)
	if g1.Sightings != 1 || g1.showsAsEvent() {
		t.Fatalf("a lone sighting: %d sightings, event %v; want 1 sighting, no event",
			g1.Sightings, g1.showsAsEvent())
	}
	r2 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000102",
		grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 2, Grid: 2}}),
		"22456\tAlpha Sabre\tSabre\t-\n22456\tBeta Sabre\tSabre\t-\n",
		"2 Sabre on d-scan in 1DQ1-A", f.base.Add(10*time.Minute))
	if g := f.gangOf(t, r2); g.ID != g1.ID || g.Sightings != 2 {
		t.Fatalf("the second report: gang %d with %d sightings, want gang %d extended to 2",
			g.ID, g.Sightings, g1.ID)
	}

	// The same pilots one jump over are the same gang: the
	// sighting joins it, and the gang's picture moves with them.
	r3 := f.postScanReport(t, f.chanA, grpSystem2, "grpc00000103",
		grpLocal(grpPilots()), "", "3 in local in GE-8JZ", f.base.Add(30*time.Minute))
	gm := f.gangOf(t, r3)
	if gm.ID != g1.ID {
		t.Fatalf("one jump over founded gang %d, want the same gang %d", gm.ID, g1.ID)
	}
	if len(gm.Groups) != 2 || gm.Sightings != 3 {
		t.Errorf("the merged gang stands on %d groups with %d sightings, want 2 and 3",
			len(gm.Groups), gm.Sightings)
	}
	if !gm.FirstSeen.Equal(f.base) || !gm.LastSeen.Equal(f.base.Add(30*time.Minute)) || gm.LastSystem != grpSystem2 {
		t.Errorf("the merged gang runs %v to %v, last in %d; want %v to %v in %d",
			gm.FirstSeen, gm.LastSeen, gm.LastSystem, f.base, f.base.Add(30*time.Minute), grpSystem2)
	}
	if gm.Strength <= gangFoundStrength {
		t.Errorf("a confirmed re-sighting kept strength %v, want more than a lone sighting's %v",
			gm.Strength, gangFoundStrength)
	}
	if !gm.showsAsEvent() {
		t.Error("a gang sighted across two groups does not read as an event")
	}

	// Different pilots two jumps away are a different gang, and
	// the first gang is untouched by their sighting.
	r4 := f.postScanReport(t, f.chanA, grpSystem3, "grpc00000104",
		grpLocal([]localPilot{
			{ID: grpPilot4, Name: "Grp Pilot Four", CorpID: 98000999, AllianceID: 98000001},
			{ID: grpPilot5, Name: "Grp Pilot Five", CorpID: 98000777},
		}), "", "2 in local in F-9P49", f.base.Add(40*time.Minute))
	gOther := f.gangOf(t, r4)
	if gOther.ID == g1.ID {
		t.Fatal("pilots sharing nobody joined the tracked gang")
	}
	if g := f.gangOf(t, r3); len(g.Groups) != 2 {
		t.Errorf("the tracked gang stands on %d groups after a stranger's sighting, want 2", len(g.Groups))
	}

	// Arrival order: reports land one paste at a time, so the
	// strangers' group first grows a partial picture naming one
	// of the tracked pilots (still no match: one shared pilot is
	// not enough), then the tracked pilots themselves land in
	// it. The fuller picture matches the tracked gang, the
	// group moves to it, and the gang the partial picture
	// founded is gone: a gang standing on one group alone is
	// provisional until its picture settles.
	f.postScanReport(t, f.chanA, grpSystem3, "grpc00000107",
		grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1}}),
		"22456\tAlpha Sabre\tSabre\t-\n", "Sabre on d-scan in F-9P49", f.base.Add(50*time.Minute))
	f.postScanReport(t, f.chanA, grpSystem3, "grpc00000108",
		scanResult{Kind: scan.KindFleet, SystemID: grpSystem3, Fleet: &fleetReading{Members: []scan.FleetRow{
			{Name: "Grp Pilot One", System: "F-9P49", Ship: "Sabre", Class: "Interdictor", Position: "Squad Member"}}}},
		"", "fleet comp posted in F-9P49", f.base.Add(55*time.Minute))
	if g := f.gangOf(t, r4); g.ID != gOther.ID {
		t.Fatalf("one shared pilot moved the group: it stands in gang %d, want its own %d", g.ID, gOther.ID)
	}
	r7 := f.postScanReport(t, f.chanA, grpSystem3, "grpc00000109",
		grpLocal(grpPilots()), "", "3 in local in F-9P49", f.base.Add(60*time.Minute))
	if g := f.gangOf(t, r7); g.ID != g1.ID {
		t.Fatalf("the settled picture stands in gang %d, want the tracked gang %d", g.ID, g1.ID)
	}
	if _, err := f.q.GetIntelGang(ctx, gOther.ID); err == nil {
		t.Error("the provisional gang still stands after its only group moved")
	}
	if g := f.gangOf(t, r3); len(g.Groups) != 3 || g.LastSystem != grpSystem3 {
		t.Errorf("the tracked gang stands on %d groups, last in %d; want 3 groups, last in %d",
			len(g.Groups), g.LastSystem, grpSystem3)
	}

	// Confidence decays with age and with distance, and a
	// closed gang states none.
	now := time.Now().UTC()
	probe := &trackedGang{Strength: 0.8, LastSeen: now, LastSystem: grpSystem}
	fresh := gangConfidenceAt(probe, now, 0, true)
	probe.LastSeen = now.Add(-time.Hour)
	stale := gangConfidenceAt(probe, now, 0, true)
	far := gangConfidenceAt(&trackedGang{Strength: 0.8, LastSeen: now}, now, 4, true)
	if !(stale < fresh && far < fresh) {
		t.Errorf("confidence: fresh %v, an hour old %v, four jumps out %v; want both below fresh",
			fresh, stale, far)
	}
	probe.Closed = true
	if c := gangConfidenceAt(probe, now, 0, true); c != 0 {
		t.Errorf("a closed gang states confidence %v, want none", c)
	}
	quiet := &trackedGang{Strength: 0.6, LastSeen: now.Add(-90 * time.Minute), LastSystem: grpSystem}
	if rv := f.app.reportGangView(ctx, quiet, grpSystem, now); !rv.Stale || rv.Title != "Gang gone quiet" {
		t.Errorf("a 90-minute-old gang reads %q (stale %v), want gone quiet", rv.Title, rv.Stale)
	}
	live := &trackedGang{Strength: 0.9, LastSeen: now, LastSystem: grpSystem}
	if rv := f.app.reportGangView(ctx, live, grpSystem, now); rv.Stale || rv.Title != "Tracked gang" {
		t.Errorf("a fresh gang reads %q (stale %v), want tracked", rv.Title, rv.Stale)
	}

	// The same pilots sighted again after the gang's lifetime
	// found a new gang: the old one is not resurrected, and
	// stands unclosed until time itself passes its last
	// sighting by the lifetime.
	r5 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000105",
		grpLocal(grpPilots()), "", "3 in local in 1DQ1-A", f.base.Add(5*time.Hour))
	gLate := f.gangOf(t, r5)
	if gLate.ID == g1.ID || gLate.ID == gOther.ID {
		t.Fatalf("a sighting past the lifetime joined gang %d, want a new gang", gLate.ID)
	}
	if g := f.gangOf(t, r3); g.Closed {
		t.Error("the first gang closed before its lifetime had passed in real time")
	}

	// A gang whose last sighting is past the lifetime in real
	// time closes where it stands when the channel's next
	// composite lands.
	deadID, err := f.q.InsertIntelGang(ctx, db.InsertIntelGangParams{
		ChannelID: f.chanA, FirstSeenAt: now.Add(-4 * time.Hour), LastSeenAt: now.Add(-3 * time.Hour),
		LastSystemID: grpSystem, Sightings: 5, Strength: 0.6, Snapshot: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	f.postScanReport(t, f.chanA, grpSystem, "grpc00000106",
		grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1}}),
		"22456\tAlpha Sabre\tSabre\t-\n", "Sabre on d-scan in 1DQ1-A", f.base.Add(6*time.Hour))
	dead, err := f.q.GetIntelGang(ctx, deadID)
	if err != nil {
		t.Fatal(err)
	}
	if !dead.ClosedAt.Valid {
		t.Error("a gang past its lifetime did not close on the next sighting")
	} else if want := dead.LastSeenAt.Add(gangLifetime); !dead.ClosedAt.Time.Equal(want) {
		t.Errorf("the gang closed at %v, want its last sighting plus the lifetime, %v",
			dead.ClosedAt.Time, want)
	}

	// Likely ships from profiles: Pilot Two's record is two
	// Guardian final blows in three, and the scan leaves two
	// Guardians unclaimed once the fleet row's Sabre is fact.
	// Pilot Three, with no profile, is attributed nothing.
	page := `[
	  {"killmail_id": 301, "killmail_time": "2026-10-20T21:15:00Z", "solar_system_id": 30004759,
	   "victim": {"character_id": 92000011, "ship_type_id": 587, "damage_taken": 900, "items": []},
	   "attackers": [
	     {"character_id": 2124000402, "ship_type_id": 11987, "damage_done": 900, "final_blow": true},
	     {"character_id": 92000012, "ship_type_id": 587, "damage_done": 100, "final_blow": false}],
	   "zkb": {"hash": "dddddddddddddddddddddddddddddddddddddddd", "totalValue": 9000000, "points": 5, "npc": false, "solo": false, "awox": false, "labels": []}},
	  {"killmail_id": 302, "killmail_time": "2026-10-21T21:15:00Z", "solar_system_id": 30004759,
	   "victim": {"character_id": 92000013, "ship_type_id": 587, "damage_taken": 900, "items": []},
	   "attackers": [
	     {"character_id": 2124000402, "ship_type_id": 11987, "damage_done": 900, "final_blow": true}],
	   "zkb": {"hash": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "totalValue": 9000000, "points": 5, "npc": false, "solo": false, "awox": false, "labels": []}},
	  {"killmail_id": 303, "killmail_time": "2026-10-22T21:15:00Z", "solar_system_id": 30004759,
	   "victim": {"character_id": 92000014, "ship_type_id": 587, "damage_taken": 900, "items": []},
	   "attackers": [
	     {"character_id": 2124000402, "ship_type_id": 22456, "damage_done": 900, "final_blow": true}],
	   "zkb": {"hash": "ffffffffffffffffffffffffffffffffffffffff", "totalValue": 9000000, "points": 5, "npc": false, "solo": false, "awox": false, "labels": []}}
	]`
	var entries []zkillHistoryEntry
	if err := json.Unmarshal([]byte(page), &entries); err != nil {
		t.Fatalf("profile fixture page does not parse: %v", err)
	}
	if err := f.q.ClaimPilotActivityProfile(ctx, db.ClaimPilotActivityProfileParams{
		CharacterID: grpPilot2, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if applied, err := f.app.applyPilotKill(ctx, grpPilot2, e.killmailFull, false); err != nil || !applied {
			t.Fatalf("fold kill %d: applied %v, err %v", e.KillmailID, applied, err)
		}
	}

	gd := f.postScanReport(t, f.chanB, grpSystem, "grpc00000111",
		grpDScan([]scanCount{
			{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1},
			{ID: 11987, Name: "Guardian", Class: "Logistics", N: 2, Grid: 2}}),
		"22456\tAlpha Sabre\tSabre\t-\n11987\tLogi One\tGuardian\t-\n11987\tLogi Two\tGuardian\t-\n",
		"Sabre and 2 Guardian on d-scan in 1DQ1-A", f.base)
	f.postScanReport(t, f.chanB, grpSystem, "grpc00000112",
		scanResult{Kind: scan.KindFleet, SystemID: grpSystem, Fleet: &fleetReading{Members: []scan.FleetRow{
			{Name: "Grp Pilot One", System: "1DQ1-A", Ship: "Sabre", Class: "Interdictor", Position: "Squad Member"}}}},
		"", "fleet comp posted in 1DQ1-A", f.base.Add(5*time.Minute))
	f.postScanReport(t, f.chanB, grpSystem, "grpc00000113",
		grpLocal(grpPilots()), "", "3 in local in 1DQ1-A", f.base.Add(10*time.Minute))

	comp, _, ok := f.app.loadGroupComposite(ctx, gd)
	if !ok {
		t.Fatal("no composite stored for the profile group")
	}
	if len(comp.Links) != 1 {
		t.Fatalf("fact links = %+v, want the one fleet row", comp.Links)
	}
	if len(comp.Likely) != 1 {
		t.Fatalf("likely links = %+v, want exactly the one profile attribution", comp.Likely)
	}
	lk := comp.Likely[0]
	if lk.PilotID != grpPilot2 || lk.ShipName != "Guardian" || lk.TypeID != 11987 {
		t.Errorf("likely link = %+v, want Grp Pilot Two in a Guardian (11987)", lk)
	}
	if lk.Prob < 0.6 || lk.Prob > 0.7 {
		t.Errorf("likely probability = %v, want two of three final blows (0.667)", lk.Prob)
	}
	if !strings.Contains(lk.Basis, "Usually flies Guardian") || !strings.Contains(lk.Basis, "2 Guardian on scan unclaimed") {
		t.Errorf("likely basis = %q, want the record and the unclaimed count stated", lk.Basis)
	}
}
