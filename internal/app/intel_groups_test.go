package app

// Channel report groups: reports in one channel on one system
// inside the group window belong to one group, the group's
// composite is computed at ingest from the member readings, and
// the workbench renders the composite with its related rail.
// The pins here are the ones whose breakage would be silent:
// a join that stops joining, a window that leaks across
// channel, system or time, a composite that keeps a superseded
// d-scan's hulls, a fact link that never lands.

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
	grpSystem  = int64(30004759) // 1DQ1-A
	grpSystem2 = int64(30004760) // a second system
	grpPilot1  = int64(2124000401)
	grpPilot2  = int64(2124000402)
	grpPilot3  = int64(2124000403)
)

type groupFixture struct {
	app     *Application
	q       *db.Queries
	ownerID int64
	charID  int64
	chanA   int64
	chanB   int64
	base    time.Time
}

func seedGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000060, 'Catch') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30004759, '1DQ1-A', 10000060, -0.38), (30004760, 'GE-8JZ', 10000060, -0.21) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (541, 'Interdictor', 6), (547, 'Logistics', 6) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name, group_id, published) VALUES
		(22456, 'Sabre', 541, 1), (11987, 'Guardian', 547, 1) ON CONFLICT DO NOTHING`)
	u, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const charID = int64(90000401)
	seedCharacter(t, q, u.ID, charID, "Group Owner")
	now := time.Now().UTC()
	for _, p := range []struct {
		id, corp int64
		name     string
	}{{grpPilot1, 98000999, "Grp Pilot One"}, {grpPilot2, 98000777, "Grp Pilot Two"}, {grpPilot3, 98000999, "Grp Pilot Three"}} {
		if err := q.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{
			NameLower: lowerName(p.name), CharacterID: p.id, Name: p.name, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{
			CharacterID: p.id, CorporationID: p.corp, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name string) int64 {
		id, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: name, CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	return &groupFixture{app: app, q: q, ownerID: u.ID, charID: charID,
		chanA: mk("Group A"), chanB: mk("Group B"), base: now.Truncate(time.Second)}
}

// lowerName is a pilot name lower-cased as the name cache keys it.
func lowerName(s string) string { return strings.ToLower(s) }

// postScanReport stores one scan reading and posts it to a
// channel as a report at the given moment, through the shared
// insert path every door uses.
func (f *groupFixture) postScanReport(t *testing.T, channelID, systemID int64, scanID string, res scanResult, raw, text string, seenAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.InsertScan(ctx, db.InsertScanParams{ID: scanID, Kind: string(res.Kind), SystemID: systemID,
		Raw: raw, Result: string(body), CreatedAt: seenAt}); err != nil {
		t.Fatal(err)
	}
	id, err := f.app.insertIntelReport(ctx, db.InsertIntelReportParams{
		ChannelID: channelID, SystemID: systemID, AuthorUserID: f.ownerID, AuthorCharacterID: f.charID,
		Reporter: "Grp Scout", Text: text, Pilots: text, ScanID: scanID, Source: "form",
		SeenAt: seenAt, CreatedAt: seenAt}, false, nil)
	if err != nil {
		t.Fatalf("insert report %s: %v", scanID, err)
	}
	return id
}

func grpDScan(types []scanCount) scanResult {
	d := &dscanReading{Items: 0, Types: types}
	for _, c := range types {
		d.Items += c.N
		d.Ships += c.N
		d.ShipsGrid += c.Grid
	}
	d.Classes = types
	return scanResult{Kind: scan.KindDScan, SystemID: grpSystem, DScan: d}
}

func grpLocal(pilots []localPilot) scanResult {
	return scanResult{Kind: scan.KindLocal, SystemID: grpSystem, Local: &localReading{
		Pilots: pilots,
		Alliances: []localGroup{
			{ID: 98000001, Name: "Fixture Alliance", N: 2, Corps: []localGroup{{ID: 98000999, Name: "Fixture Corp", N: 2}}},
			{ID: 98000777, Name: "Fixture Corp Two", N: 1, Corp: true},
		},
	}}
}

func grpPilots() []localPilot {
	return []localPilot{
		{ID: grpPilot1, Name: "Grp Pilot One", CorpID: 98000999, AllianceID: 98000001},
		{ID: grpPilot2, Name: "Grp Pilot Two", CorpID: 98000777},
		{ID: grpPilot3, Name: "Grp Pilot Three", CorpID: 98000999, AllianceID: 98000001},
	}
}

func (f *groupFixture) groupOf(t *testing.T, reportID int64) int64 {
	t.Helper()
	rep, err := f.q.GetIntelReportFull(context.Background(), reportID)
	if err != nil {
		t.Fatal(err)
	}
	return rep.GroupID
}

// TestIntelReportGroups pins the window rule and its edges: a
// report joins inside the window, and not across a channel, a
// system, or the window's far side.
func TestIntelReportGroups(t *testing.T) {
	f := seedGroupFixture(t)
	ctx := context.Background()

	r1 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000001",
		grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 2, Grid: 2}}),
		"22456\tAlpha Sabre\tSabre\t-\n22456\tBeta Sabre\tSabre\t-\n",
		"2 Sabre on d-scan in 1DQ1-A", f.base)
	r2 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000002",
		grpLocal(grpPilots()), "", "3 in local in 1DQ1-A", f.base.Add(10*time.Minute))
	r3 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000003",
		grpDScan([]scanCount{
			{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1},
			{ID: 11987, Name: "Guardian", Class: "Logistics", N: 1, Grid: 1}}),
		"22456\tAlpha Sabre\tSabre\t-\n11987\tLogi One\tGuardian\t-\n",
		"Sabre and Guardian on d-scan in 1DQ1-A", f.base.Add(20*time.Minute))

	if g := f.groupOf(t, r1); g != r1 {
		t.Errorf("the founder names itself: group %d, report %d", g, r1)
	}
	if g := f.groupOf(t, r2); g != r1 {
		t.Errorf("a local inside the window joins: group %d, want %d", g, r1)
	}
	if g := f.groupOf(t, r3); g != r1 {
		t.Errorf("a second d-scan inside the window joins: group %d, want %d", g, r1)
	}

	r4 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000004",
		grpDScan([]scanCount{{ID: 11987, Name: "Guardian", Class: "Logistics", N: 3, Grid: 3}}),
		"11987\tLogi One\tGuardian\t-\n", "Guardians on d-scan in 1DQ1-A", f.base.Add(3*time.Hour))
	if g := f.groupOf(t, r4); g != r4 {
		t.Errorf("past the window a report founds its own group: group %d, report %d", g, r4)
	}
	r5 := f.postScanReport(t, f.chanB, grpSystem, "grpc00000005",
		grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1}}),
		"22456\tAlpha Sabre\tSabre\t-\n", "Sabre on d-scan in 1DQ1-A", f.base.Add(15*time.Minute))
	if g := f.groupOf(t, r5); g != r5 {
		t.Errorf("another channel never joins: group %d, report %d", g, r5)
	}
	r6 := f.postScanReport(t, f.chanA, grpSystem2, "grpc00000006",
		grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1}}),
		"22456\tAlpha Sabre\tSabre\t-\n", "Sabre on d-scan in GE-8JZ", f.base.Add(15*time.Minute))
	if g := f.groupOf(t, r6); g != r6 {
		t.Errorf("another system never joins: group %d, report %d", g, r6)
	}

	// The composite of the three: the newest d-scan governs
	// (the first scan's second Sabre is gone), the local is
	// the union of its pilots.
	comp, _, ok := f.app.loadGroupComposite(ctx, r1)
	if !ok {
		t.Fatal("no composite stored for the group")
	}
	if len(comp.Members) != 3 {
		t.Fatalf("composite members: %d, want 3", len(comp.Members))
	}
	if comp.DScan == nil {
		t.Fatal("composite has no d-scan face")
	}
	tallies := map[string]int{}
	for _, c := range comp.DScan.Types {
		tallies[c.Name] = c.N
	}
	if tallies["Sabre"] != 1 || tallies["Guardian"] != 1 {
		t.Errorf("composite tallies = %v, want the newest d-scan's {Sabre:1, Guardian:1}", tallies)
	}
	if comp.DScanFrom != r3 {
		t.Errorf("composite d-scan from report %d, want the newest, %d", comp.DScanFrom, r3)
	}
	if comp.Local == nil || len(comp.Local.Pilots) != 3 {
		t.Errorf("composite local = %+v, want the union of 3 pilots", comp.Local)
	}
}

// TestIntelGroupFactAndLikelyLinks pins the attribution rules:
// a fleet row is a fact link in the composite, and elimination
// over what remains yields a likely link, stored as likely.
func TestIntelGroupFactAndLikelyLinks(t *testing.T) {
	f := seedGroupFixture(t)
	ctx := context.Background()

	g := f.postScanReport(t, f.chanA, grpSystem, "grpc00000011",
		grpDScan([]scanCount{
			{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1},
			{ID: 11987, Name: "Guardian", Class: "Logistics", N: 1, Grid: 1}}),
		"22456\tAlpha Sabre\tSabre\t-\n11987\tLogi One\tGuardian\t-\n",
		"Sabre and Guardian on d-scan in 1DQ1-A", f.base)
	f.postScanReport(t, f.chanA, grpSystem, "grpc00000012",
		scanResult{Kind: scan.KindFleet, SystemID: grpSystem, Fleet: &fleetReading{Members: []scan.FleetRow{
			{Name: "Grp Pilot One", System: "1DQ1-A", Ship: "Sabre", Class: "Interdictor", Position: "Squad Member"}}}},
		"", "fleet comp posted in 1DQ1-A", f.base.Add(5*time.Minute))
	f.postScanReport(t, f.chanA, grpSystem, "grpc00000013",
		grpLocal(grpPilots()[:2]), "", "2 in local in 1DQ1-A", f.base.Add(10*time.Minute))

	comp, _, ok := f.app.loadGroupComposite(ctx, g)
	if !ok {
		t.Fatal("no composite stored for the group")
	}
	if len(comp.Links) != 1 {
		t.Fatalf("fact links = %+v, want the one fleet row", comp.Links)
	}
	l := comp.Links[0]
	if l.PilotName != "Grp Pilot One" || l.ShipName != "Sabre" || l.TypeID != 22456 || l.PilotID != grpPilot1 {
		t.Errorf("fact link = %+v, want Grp Pilot One in a Sabre (22456), resolved", l)
	}
	// Pilot Two is the only unlinked pilot, the Guardian the
	// only unclaimed hull: a likely link, with its basis.
	if len(comp.Likely) != 1 {
		t.Fatalf("likely links = %+v, want the one elimination", comp.Likely)
	}
	lk := comp.Likely[0]
	if lk.PilotName != "Grp Pilot Two" || lk.ShipName != "Guardian" || lk.Basis == "" {
		t.Errorf("likely link = %+v, want Grp Pilot Two in the Guardian, with its basis", lk)
	}
}

// TestWorkbenchGroupRail pins the grouped workbench: the page
// renders the composite (its hulls and pilots, not the opened
// report's alone), the rail lists the members newest first
// with the viewed one marked, and the diff runs against the
// previous composite. A singleton renders ungrouped.
func TestWorkbenchGroupRail(t *testing.T) {
	f := seedGroupFixture(t)
	ctx := context.Background()

	r1 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000021",
		grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 2, Grid: 2}}),
		"22456\tAlpha Sabre\tSabre\t-\n22456\tBeta Sabre\tSabre\t-\n",
		"2 Sabre on d-scan in 1DQ1-A", f.base)
	r2 := f.postScanReport(t, f.chanA, grpSystem, "grpc00000022",
		grpLocal(grpPilots()), "", "3 in local in 1DQ1-A", f.base.Add(10*time.Minute))
	f.postScanReport(t, f.chanA, grpSystem, "grpc00000023",
		grpDScan([]scanCount{
			{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1},
			{ID: 11987, Name: "Guardian", Class: "Logistics", N: 1, Grid: 1}}),
		"22456\tAlpha Sabre\tSabre\t-\n11987\tLogi One\tGuardian\t-\n",
		"Sabre and Guardian on d-scan in 1DQ1-A", f.base.Add(20*time.Minute))

	rep2, err := f.q.GetIntelReportFull(ctx, r2)
	if err != nil {
		t.Fatal(err)
	}
	c := accessChannel{ListAccessibleChannelsRow: db.ListAccessibleChannelsRow{ID: f.chanA, Name: "Group A", CanPost: true}}
	v := f.app.buildWorkbench(ctx, rep2, c)
	if !v.Grouped {
		t.Fatal("the workbench does not render the group picture")
	}
	if v.Hulls != 2 || v.PilotsN != 3 {
		t.Errorf("composite faces: %d hulls, %d pilots, want 2 and 3", v.Hulls, v.PilotsN)
	}
	if len(v.Rail) != 3 {
		t.Fatalf("rail: %d items, want 3", len(v.Rail))
	}
	here := 0
	for _, item := range v.Rail {
		if item.Here {
			here++
			if item.ReportID != r2 {
				t.Errorf("the rail marks report %d, want the viewed %d", item.ReportID, r2)
			}
		}
	}
	if here != 1 {
		t.Errorf("rail marks %d reports as here, want 1", here)
	}
	if v.Rail[0].Kind != "D-Scan" || v.Rail[1].Kind != "Local" || v.Rail[2].Kind != "D-Scan" {
		t.Errorf("rail order/kinds: %q, %q, %q, want D-Scan, Local, D-Scan (newest first)",
			v.Rail[0].Kind, v.Rail[1].Kind, v.Rail[2].Kind)
	}
	if !v.HasPrev || !v.DiffGroup {
		t.Errorf("diff: HasPrev %v, DiffGroup %v, want both", v.HasPrev, v.DiffGroup)
	}
	var sabreLeft, guardianAdded bool
	for _, d := range v.HullDeltas {
		if d.Name == "Sabre" && d.Dir == "left" && d.N == 1 {
			sabreLeft = true
		}
		if d.Name == "Guardian" && d.Dir == "added" && d.N == 1 {
			guardianAdded = true
		}
	}
	if !sabreLeft || !guardianAdded {
		t.Errorf("composite diff = %+v, want Sabre left x1 and Guardian added x1", v.HullDeltas)
	}

	// A report in no group renders exactly as before.
	solo := f.postScanReport(t, f.chanA, grpSystem, "grpc00000024",
		grpDScan([]scanCount{{ID: 11987, Name: "Guardian", Class: "Logistics", N: 1, Grid: 1}}),
		"11987\tLogi One\tGuardian\t-\n", "Guardian on d-scan in 1DQ1-A", f.base.Add(5*time.Hour))
	repSolo, err := f.q.GetIntelReportFull(ctx, solo)
	if err != nil {
		t.Fatal(err)
	}
	vs := f.app.buildWorkbench(ctx, repSolo, c)
	if vs.Grouped || len(vs.Rail) != 0 {
		t.Errorf("a singleton renders grouped: %+v", vs.Rail)
	}
	_ = r1
}
