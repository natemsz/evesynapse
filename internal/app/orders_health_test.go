package app

// v0.3.14 consolidation tests: the character Orders page carries
// the same order-health verdicts as the Market page's "Your
// orders" section (and the corporation orders page where the
// stored rows cover the issuer), killmail values warm on view
// through the durable guide-price want (schema 027), and the
// place-link stragglers from the v0.3.13 pass (killmail system
// lines, industry locations, intel systems, corp orders
// locations) link exactly when the SDE knows the place.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func TestOrdersPageOrderHealth(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	issued := now.AddDate(0, 0, -10).Format(time.RFC3339)

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	// Open: an undercut sell, a best-priced sell, an unchecked
	// sell, and a buy (buys carry no verdict by design).
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 700, TypeID: 587, LocationID: 60003760, RegionID: 10000002, Price: 1000000, VolumeTotal: 5, VolumeRemain: 3, Range: "station", Issued: issued, Duration: 90},
		{OrderID: 701, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.5, VolumeTotal: 1000, VolumeRemain: 400, Range: "station", Issued: issued, Duration: 90},
		{OrderID: 702, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.6, VolumeTotal: 1000, VolumeRemain: 900, Range: "station", Issued: issued, Duration: 90},
		{OrderID: 703, TypeID: 587, LocationID: 60003760, RegionID: 10000002, IsBuyOrder: true, Price: 900000, VolumeTotal: 5, VolumeRemain: 5, Range: "station", Issued: issued, Duration: 90},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrdersHistory, []esi.CharOrderHistoryEntry{
		{CharOrder: esi.CharOrder{OrderID: 704, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.5, VolumeTotal: 1000, VolumeRemain: 400, Issued: now.AddDate(0, 0, -40).Format(time.RFC3339)}, State: "expired"},
	})
	stamp := now.Format(time.RFC3339)
	for _, h := range []db.UpsertOrderHealthParams{
		{CharacterID: fixtureCharA, OrderID: 700, TypeID: 587, RegionID: 10000002, LocationID: 60003760,
			MyPrice: 1000000, StationBest: 990000, RegionBest: 990000, Status: "undercut_station", ComputedAt: stamp},
		{CharacterID: fixtureCharA, OrderID: 701, TypeID: 34, RegionID: 10000002, LocationID: 60003760,
			MyPrice: 5.5, StationBest: 5.5, RegionBest: 5.5, Status: "best", ComputedAt: stamp},
	} {
		if err := q.UpsertOrderHealth(ctx, h); err != nil {
			t.Fatalf("seed health: %v", err)
		}
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/orders/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /orders/: status %d", code)
	}
	mustContain(t, "/orders/", body,
		"<strong>Undercut", "at this station",
		"Best price here",
		"Not checked against the order book yet",
	)
	if n := strings.Count(body, "Not checked against the order book yet"); n != 1 {
		t.Errorf("not-checked lines = %d, want exactly 1 (unchecked sell only; buys and history carry none)", n)
	}
	if n := strings.Count(body, "Best price here"); n != 1 {
		t.Errorf("best-price lines = %d, want exactly 1 (open sell only)", n)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Errorf("handler made %d outbound calls, want 0 (cache-only)", got)
	}
}

// TestCorpOrdersHealthAndLocation proves the corporation orders
// page shares the treatment where the stored health rows reach:
// a verdict for an order placed by a synced character of the
// account, the honest not-checked line while such an order waits
// for its first check, and silence for orders placed by people
// the app doesn't sync. Locations link by the place policy.
func TestCorpOrdersHealthAndLocation(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	issued := now.AddDate(0, 0, -10).Format(time.RFC3339)

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")
	for _, m := range []struct{ charID, corpID int64 }{{fixtureCharA, fixtureCorpA}, {fixtureCharB, fixtureCorpA}} {
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
			CharacterID: m.charID, CorporationID: m.corpID, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed mapping: %v", err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpInfo, `{"name":"Fixture Corp","ticker":"FXC","member_count":3}`)
	// 800: placed by the CEO char, health-checked (undercut).
	// 801: placed by a corp mate the app doesn't sync — blank.
	// 802: placed by the account's second char, not checked yet.
	// 803: a buy from the CEO char — no verdict by design.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpOrders, []esi.CorpOrder{
		{OrderID: 800, TypeID: 587, LocationID: 60003760, RegionID: 10000002, Price: 1000000, VolumeTotal: 5, VolumeRemain: 3, Duration: 90, Issued: issued, IssuedBy: fixtureCharA},
		{OrderID: 801, TypeID: 587, LocationID: 60003760, RegionID: 10000002, Price: 1000000, VolumeTotal: 5, VolumeRemain: 3, Duration: 90, Issued: issued, IssuedBy: fixtureMember},
		{OrderID: 802, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.6, VolumeTotal: 1000, VolumeRemain: 900, Duration: 90, Issued: issued, IssuedBy: fixtureCharB},
		{OrderID: 803, TypeID: 587, LocationID: 60003760, RegionID: 10000002, IsBuyOrder: true, Price: 900000, VolumeTotal: 5, VolumeRemain: 5, Duration: 90, Issued: issued, IssuedBy: fixtureCharA},
	})
	if err := q.UpsertOrderHealth(ctx, db.UpsertOrderHealthParams{
		CharacterID: fixtureCharA, OrderID: 800, TypeID: 587, RegionID: 10000002, LocationID: 60003760,
		MyPrice: 1000000, StationBest: 990000, RegionBest: 990000, Status: "undercut_station",
		ComputedAt: now.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed health: %v", err)
	}
	app.esi.StoreCharacterName(fixtureCharA, "Fixture Ceo")
	app.esi.StoreCharacterName(fixtureCharB, "Second Pilot")
	app.esi.StoreCharacterName(fixtureMember, "Member One")

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/corporations/orders/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /corporations/orders/: status %d", code)
	}
	mustContain(t, "/corporations/orders/", body,
		`<a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a>`,
		"<strong>Undercut", "at this station",
		"Not checked against the order book yet",
	)
	if n := strings.Count(body, "Not checked against the order book yet"); n != 1 {
		t.Errorf("not-checked lines = %d, want exactly 1 (own unchecked sell only; non-synced issuers and buys stay blank)", n)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Errorf("handler made %d outbound calls, want 0 (cache-only)", got)
	}
}

// TestKillViewNotesGuidePriceWant renders kill content with no
// price guide anywhere: the values show the honest dash, the
// view leaves the durable guide-price want behind (schema 027),
// and once the worker-stored guide exists the same render prices
// the mail — all with zero outbound calls from the handler.
func TestKillViewNotesGuidePriceWant(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	recently := now.Add(-time.Hour).Format(time.RFC3339)

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapKillmails, []esi.KillmailRef{
		{KillmailID: 7001, KillmailHash: "fixture"},
	})
	km := esi.Killmail{
		KillmailID: 7001, KillmailTime: recently, SolarSystemID: 30000142,
		Victim: esi.KillmailVictim{
			CharacterID: fixtureMember, CorporationID: fixtureCorpA, ShipTypeID: 587,
			Items: []esi.KillmailVictimItem{{ItemTypeID: 34, QuantityDestroyed: 100}},
		},
		Attackers: []esi.KillmailAttacker{{CharacterID: fixtureCharA, CorporationID: fixtureCorpA, ShipTypeID: 587, FinalBlow: true}},
	}
	raw, err := json.Marshal(km)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
		KillmailID: 7001, CharacterID: fixtureCharA, Hash: "fixture",
		Payload: string(raw), FetchedAt: mustTime("2026-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed killmail detail: %v", err)
	}
	app.esi.StoreCharacterName(fixtureCharA, "Fixture Ceo")
	app.esi.StoreCharacterName(fixtureMember, "Member One")

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/killmails/")
	if code != http.StatusOK {
		t.Fatalf("GET /killmails/: status %d", code)
	}
	// The killmail system line follows the place-link policy.
	mustContain(t, "/killmails/", body, `<a href="/system/?system=30000142">Jita</a>`)
	if strings.Contains(body, "550") {
		t.Error("value rendered before any price guide exists")
	}
	if _, err := q.GetGuidePriceWant(ctx); err != nil {
		t.Errorf("guide-price want not noted after a priceless kill view: %v", err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handler made %d outbound calls, want 0 (cache-only)", got)
	}

	// The stored guide lands (the worker's answer to the want):
	// the same render now prices the mail — 100 destroyed at an
	// average of 5.50.
	if err := q.UpsertGuidePrice(ctx, db.UpsertGuidePriceParams{
		TypeID: 34, AdjustedPrice: 5, AveragePrice: 5.5,
	}); err != nil {
		t.Fatalf("seed guide price: %v", err)
	}
	if err := q.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt: now.Format(time.RFC3339), CachedUntil: "2999-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed guide meta: %v", err)
	}
	code, body = getPage(t, app, cookie, "/killmails/")
	if code != http.StatusOK {
		t.Fatalf("GET /killmails/ (guided): status %d", code)
	}
	mustContain(t, "/killmails/ (guided)", body, "550.00")
	if got := transport.calls.Load(); got != 0 {
		t.Errorf("handler made %d outbound calls across renders, want 0", got)
	}
}

// TestUrgentDrainAnswersGuidePriceWant: the worker's urgent
// drain sees the noted want, refreshes the stored guide through
// the usual self-gating call, and clears the note.
func TestUrgentDrainAnswersGuidePriceWant(t *testing.T) {
	transport := &pricesTransport{body: `[{"type_id":34,"average_price":5.5,"adjusted_price":5}]`}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	if err := q.NoteGuidePriceWant(ctx, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("note want: %v", err)
	}
	app.drainUrgentWants(ctx)

	if _, err := q.GetGuidePriceWant(ctx); err == nil {
		t.Error("guide-price want still on file after the drain answered it")
	}
	if prices := app.storedGuidePrices(ctx); len(prices) == 0 {
		t.Error("stored guide empty after the drain's refresh")
	}
	if got := transport.calls.Load(); got == 0 {
		t.Error("drain made no price-guide call")
	}
}

// TestPlaceLinkStragglers covers the surfaces the v0.3.13 pass
// left behind: industry job facilities, blueprint locations and
// mining systems, incursion staging/affected systems, and FW
// contested systems link when the SDE knows the place and stay
// honest text when it doesn't.
func TestPlaceLinkStragglers(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	recently := now.Add(-time.Hour).Format(time.RFC3339)

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (1230, 'Veldspar', 0), (990001, 'Fixture Blueprint', 0)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 1, ActivityID: 1, BlueprintTypeID: 990001, ProductTypeID: 587, FacilityID: 60003760, InstallerID: fixtureCharA, Runs: 5, Status: "active", StartDate: recently, EndDate: now.Add(2 * time.Hour).Format(time.RFC3339), Cost: 100},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, []esi.Blueprint{
		{ItemID: 1, TypeID: 990001, LocationID: 60003760, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMining, []esi.MiningEntry{
		{Date: "2026-09-30", TypeID: 1230, SolarSystemID: 30000142, Quantity: 5000},
		{Date: "2026-09-29", TypeID: 1230, SolarSystemID: 39999999, Quantity: 2500},
	})

	seedGlobalSnapshot(t, q, esi.GlobalIncursions, []esi.Incursion{
		{ConstellationID: 20000001, FactionID: 500019, HasBoss: true,
			InfestedSystems: []int64{30000142, 39999999}, Influence: 0.73,
			StagingSystemID: 30000142, State: "established", Type: "Incursion"},
	})
	seedGlobalSnapshot(t, q, esi.GlobalFWSystems, []esi.FWSystem{
		{SolarSystemID: 30000142, OccupierFactionID: 500002, OwnerFactionID: 500001, Contested: "contested", VictoryPoints: 500000, VictoryPointsThreshold: 1000000},
	})
	seedGlobalSnapshot(t, q, esi.GlobalFactions, []esi.Faction{
		{FactionID: 500001, Name: "Caldari State"},
		{FactionID: 500002, Name: "Minmatar Republic"},
		{FactionID: 500019, Name: "Sansha's Nation"},
	})

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	stationLink := `<a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a>`
	systemLink := `<a href="/system/?system=30000142">Jita</a>`

	code, body := getPage(t, app, cookie, "/industry/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /industry/: status %d", code)
	}
	mustContain(t, "/industry/", body, stationLink, systemLink, "System #39999999")
	if strings.Contains(body, `<a href="/system/?system=39999999">`) {
		t.Error("unknown mining system rendered as a link")
	}

	code, body = getPage(t, app, cookie, "/intel/incursions/")
	if code != http.StatusOK {
		t.Fatalf("GET /intel/incursions/: status %d", code)
	}
	mustContain(t, "/intel/incursions/", body, systemLink, "System #39999999")
	if strings.Contains(body, `<a href="/system/?system=39999999">`) {
		t.Error("unknown infested system rendered as a link")
	}

	code, body = getPage(t, app, cookie, "/intel/fw/")
	if code != http.StatusOK {
		t.Fatalf("GET /intel/fw/: status %d", code)
	}
	mustContain(t, "/intel/fw/", body, systemLink)

	if got := transport.calls.Load(); got != 0 {
		t.Errorf("handlers made %d outbound calls, want 0 (cache-only)", got)
	}
}
