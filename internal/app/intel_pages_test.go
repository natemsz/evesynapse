package app

// Hermetic tests for the intel cluster (module sweep, cluster 4),
// following corp_pages_test.go's two-layer approach:
//
//   - Render tests: seeded global snapshots, war_details rows and
//     in-process name caches drive the real chi router with a
//     counting stub transport, proving the Intel pages and the
//     Home status line render fixture content — including a
//     highlighted user-corporation war and an incursion influence
//     bar — and that no handler makes an outbound call.
//   - Worker tests: a path-routing stub transport stands in for
//     ESI, proving refreshIntel stores the global snapshots, warms
//     war details and public names through the shared budget, and
//     settles to zero calls once everything is fresh (with active
//     wars re-checked once their detail ages).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedGlobalSnapshot stores one global payload marked fresh until
// 2999 (the intel counterpart of seedSnapshot).
func seedGlobalSnapshot(t *testing.T, q *db.Queries, kind string, payload any) {
	t.Helper()
	var raw string
	switch p := payload.(type) {
	case string:
		raw = p
	default:
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	if err := q.UpsertGlobalSnapshot(context.Background(), db.UpsertGlobalSnapshotParams{
		Kind:        kind,
		Payload:     raw,
		FetchedAt:   mustTime("2026-01-01T00:00:00Z"),
		CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed global snapshot %s: %v", kind, err)
	}
}

func TestIntelPagesRenderFromStore(t *testing.T) {
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
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID:   fixtureCharA,
		CorporationID: fixtureCorpA,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	// SDE fixtures: systems for incursion staging/affected and
	// the FW frontier.
	for _, stmt := range []string{
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9), (30000143, 'Perimeter', 10000002, 0.9), (30000144, 'Urlen', 10000002, 0.9)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	// Global store: status, the war list (one undetailed war at
	// the head), one incursion, FW systems/stats, faction names.
	seedGlobalSnapshot(t, q, esi.GlobalStatus, `{"players":23456,"server_version":"1132976","start_time":"2026-10-01T11:00:00Z","vip":false}`)
	seedGlobalSnapshot(t, q, esi.GlobalWars, []int64{9003, 9002, 9001})
	seedGlobalSnapshot(t, q, esi.GlobalIncursions, []esi.Incursion{
		{ConstellationID: 20000001, FactionID: 500019, HasBoss: true,
			InfestedSystems: []int64{30000142, 30000143}, Influence: 0.73,
			StagingSystemID: 30000142, State: "established", Type: "Incursion"},
	})
	seedGlobalSnapshot(t, q, esi.GlobalFWSystems, []esi.FWSystem{
		{SolarSystemID: 30000142, OccupierFactionID: 500002, OwnerFactionID: 500001, Contested: "contested", VictoryPoints: 500000, VictoryPointsThreshold: 1000000},
		{SolarSystemID: 30000143, OccupierFactionID: 500001, OwnerFactionID: 500002, Contested: "contested", VictoryPoints: 100000, VictoryPointsThreshold: 1000000},
		{SolarSystemID: 30000144, OccupierFactionID: 500001, OwnerFactionID: 500001, Contested: "uncontested", VictoryPoints: 0, VictoryPointsThreshold: 1000000},
	})
	seedGlobalSnapshot(t, q, esi.GlobalFWStats, []esi.FWStat{
		{FactionID: 500001, Pilots: 1200, SystemsControlled: 20,
			Kills:         esi.FWPeriod{Yesterday: 42, LastWeek: 300, Total: 9999},
			VictoryPoints: esi.FWPeriod{Yesterday: 123456, LastWeek: 800000, Total: 7777777}},
		{FactionID: 500002, Pilots: 900, SystemsControlled: 15,
			Kills:         esi.FWPeriod{Yesterday: 17, LastWeek: 120, Total: 5555},
			VictoryPoints: esi.FWPeriod{Yesterday: 65432, LastWeek: 400000, Total: 3333333}},
	})
	seedGlobalSnapshot(t, q, esi.GlobalFactions, []esi.Faction{
		{FactionID: 500001, Name: "Caldari State"},
		{FactionID: 500002, Name: "Minmatar Republic"},
		{FactionID: 500019, Name: "Sansha's Nation"},
	})

	// War details: 9002 is live with the fixture corporation
	// defending (the highlighted row); 9001 has ended. 9003 is
	// still warming (no detail row).
	for _, war := range []esi.War{
		{ID: 9002, Declared: recently, Started: recently, OpenForAllies: true,
			Aggressor: esi.WarParty{CorporationID: 99000001, ISKDestroyed: 1500000.5, ShipsKilled: 3},
			Defender:  esi.WarParty{CorporationID: fixtureCorpA, ISKDestroyed: 500000, ShipsKilled: 1}},
		{ID: 9001, Declared: recently, Started: recently, Finished: recently, Mutual: true,
			Aggressor: esi.WarParty{AllianceID: 99000002, ISKDestroyed: 2000000, ShipsKilled: 5},
			Defender:  esi.WarParty{CorporationID: 99000003},
			Allies:    []esi.WarAlly{{CorporationID: 99000004}}},
	} {
		raw, err := json.Marshal(war)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertWarDetail(ctx, db.UpsertWarDetailParams{
			WarID:     war.ID,
			Payload:   string(raw),
			FetchedAt: mustTime("2026-01-01T00:00:00Z"),
		}); err != nil {
			t.Fatalf("seed war detail %d: %v", war.ID, err)
		}
	}

	// Public names in the worker-warmed in-process caches.
	app.esi.StoreCorpName(fixtureCorpA, "Fixture Corp")
	app.esi.StoreCorpName(99000001, "Aggressor Corp")
	app.esi.StoreCorpName(99000003, "Defender Corp")
	app.esi.StoreCorpName(99000004, "Ally Corp")
	app.esi.StoreAllianceName(99000002, "Aggressor Alliance")
	app.esi.StoreConstellationName(20000001, "Kimotoro")

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/intel/wars/")
	if code != http.StatusOK {
		t.Fatalf("wars page: status %d", code)
	}
	mustContain(t, "/intel/wars/", body,
		"#9002", "Aggressor Corp", "Fixture Corp", "Your corporation",
		"Open for allies", "Active", "3 ships", "1,500,000.50",
		"#9001", "Aggressor Alliance", "Defender Corp", "Ally Corp", "Ended", "Mutual",
		"#9003", "details warming")

	code, body = getPage(t, app, cookie, "/intel/incursions/")
	if code != http.StatusOK {
		t.Fatalf("incursions page: status %d", code)
	}
	mustContain(t, "/intel/incursions/", body,
		"Kimotoro", "Jita", "Perimeter", "Established", "Sansha&#39;s Nation",
		"73%", "width:73%", "final-encounter boss")

	code, body = getPage(t, app, cookie, "/intel/fw/")
	if code != http.StatusOK {
		t.Fatalf("fw page: status %d", code)
	}
	mustContain(t, "/intel/fw/", body,
		"Caldari State", "Minmatar Republic", "20", "1,200", "42",
		"Jita", "50%", "10%")

	// Home with a name-only session (no character to load): the
	// Tranquility line renders from the global store.
	anon := sessionCookie(t, app, user.ID, 0, "")
	code, body = getPage(t, app, anon, "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	mustContain(t, "/", body, "Tranquility: 23,456 players online")

	// Signed out, the line renders too (status is public data).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("home (signed out): status %d", rec.Code)
	}
	mustContain(t, "/ (signed out)", rec.Body.String(), "Tranquility: 23,456 players online")

	// Intel pages require sign-in.
	req = httptest.NewRequest(http.MethodGet, "/intel/wars/", nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("wars page unauthenticated: status %d, want 303", rec.Code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestIntelPagesEmptyStates(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	for _, path := range []string{"/intel/wars/", "/intel/incursions/", "/intel/fw/"} {
		code, body := getPage(t, app, cookie, path)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", path, code)
		}
		mustContain(t, path, body, "Still warming up")
	}

	// Home keeps rendering exactly as before without the status
	// snapshot: no Tranquility line, no errors.
	code, body := getPage(t, app, sessionCookie(t, app, user.ID, 0, ""), "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	if strings.Contains(body, "Tranquility:") {
		t.Error("home shows a Tranquility line with no status snapshot stored")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// intelStubTransport routes the intel worker's public ESI calls.
// Every response carries a far-future Expires so a second pass
// has nothing to refresh.
type intelStubTransport struct{ calls atomic.Int64 }

func (s *intelStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	path := req.URL.Path
	respond := func(status int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
			},
			Body: io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	switch {
	case path == "/status/":
		return respond(http.StatusOK, `{"players":23456,"server_version":"1132976","start_time":"2026-10-01T11:00:00Z","vip":false}`)
	case path == "/wars/":
		return respond(http.StatusOK, `[9002,9001]`)
	case path == "/wars/9002/":
		return respond(http.StatusOK, `{"id":9002,"declared":"2026-09-30T00:00:00Z","started":"2026-10-01T00:00:00Z","aggressor":{"corporation_id":99000001,"isk_destroyed":1500000.5,"ships_killed":3},"defender":{"corporation_id":98000001,"isk_destroyed":500000,"ships_killed":1},"mutual":false,"open_for_allies":true}`)
	case path == "/wars/9001/":
		return respond(http.StatusOK, `{"id":9001,"declared":"2026-09-01T00:00:00Z","started":"2026-09-02T00:00:00Z","finished":"2026-09-20T00:00:00Z","aggressor":{"alliance_id":99000002,"isk_destroyed":2000000,"ships_killed":5},"defender":{"corporation_id":99000003,"isk_destroyed":0,"ships_killed":0},"mutual":true,"open_for_allies":false}`)
	case path == "/incursions/":
		return respond(http.StatusOK, `[{"constellation_id":20000001,"faction_id":500019,"has_boss":true,"infested_solar_systems":[30000142],"influence":0.5,"staging_solar_system_id":30000142,"state":"established","type":"Incursion"}]`)
	case path == "/fw/systems/":
		return respond(http.StatusOK, `[]`)
	case path == "/fw/stats/":
		return respond(http.StatusOK, `[]`)
	case path == "/universe/factions/":
		return respond(http.StatusOK, `[{"faction_id":500001,"name":"Caldari State"}]`)
	case path == fmt.Sprintf("/corporations/%d/", fixtureCorpA):
		return respond(http.StatusOK, `{"name":"Fixture Corp","ticker":"FXC"}`)
	case path == "/corporations/99000001/":
		return respond(http.StatusOK, `{"name":"Aggressor Corp","ticker":"AGG"}`)
	case path == "/corporations/99000003/":
		return respond(http.StatusOK, `{"name":"Defender Corp","ticker":"DEF"}`)
	case path == "/alliances/99000002/":
		return respond(http.StatusOK, `{"name":"Aggressor Alliance","ticker":"AGA"}`)
	case path == "/universe/constellations/20000001/":
		return respond(http.StatusOK, `{"constellation_id":20000001,"name":"Kimotoro","region_id":10000002}`)
	default:
		return respond(http.StatusNotFound, `{"error":"unexpected path `+path+`"}`)
	}
}

// TestRefreshIntel proves the worker contract: one pass stores
// the global snapshots, both war details and the public names;
// a second pass settles to zero calls; an aged active-war detail
// is the one thing the third pass refetches.
func TestRefreshIntel(t *testing.T) {
	transport := &intelStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	budget := &warmBudget{left: maxWarmLookupsPerCycle}

	stored, names, limited := app.refreshIntel(ctx, budget)
	if limited {
		t.Fatal("first pass: unexpectedly error-limited")
	}
	if stored != 8 { // 6 global snapshots + 2 war details
		t.Fatalf("first pass: stored %d payloads, want 8", stored)
	}
	if names != 5 { // 3 corporations + 1 alliance + 1 constellation
		t.Fatalf("first pass: resolved %d names, want 5", names)
	}

	for _, kind := range globalKindOrder {
		if _, err := q.GetGlobalSnapshot(ctx, kind); err != nil {
			t.Errorf("global snapshot %s missing: %v", kind, err)
		}
	}
	for _, warID := range []int64{9002, 9001} {
		if _, err := q.GetWarDetail(ctx, warID); err != nil {
			t.Errorf("war detail %d missing: %v", warID, err)
		}
	}
	if name, ok := app.esi.CachedCorpName(fixtureCorpA); !ok || name != "Fixture Corp" {
		t.Errorf("corp name cache: %q, %v", name, ok)
	}
	if name, ok := app.esi.CachedAllianceName(99000002); !ok || name != "Aggressor Alliance" {
		t.Errorf("alliance name cache: %q, %v", name, ok)
	}
	if name, ok := app.esi.CachedConstellationName(20000001); !ok || name != "Kimotoro" {
		t.Errorf("constellation name cache: %q, %v", name, ok)
	}

	// Second pass: fresh snapshots, fresh/finished details and
	// cached names — no calls at all.
	before := transport.calls.Load()
	if _, _, limited := app.refreshIntel(ctx, budget); limited {
		t.Fatal("second pass: unexpectedly error-limited")
	}
	if got := transport.calls.Load(); got != before {
		t.Fatalf("second pass made %d calls, want 0", got-before)
	}

	// Age the active war's detail past the staleness window: the
	// next pass refetches exactly that one.
	row, err := q.GetWarDetail(ctx, 9002)
	if err != nil {
		t.Fatalf("read war detail 9002: %v", err)
	}
	if err := q.UpsertWarDetail(ctx, db.UpsertWarDetailParams{
		WarID:     9002,
		Payload:   row.Payload,
		FetchedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("age war detail 9002: %v", err)
	}
	stored, _, limited = app.refreshIntel(ctx, budget)
	if limited {
		t.Fatal("third pass: unexpectedly error-limited")
	}
	if stored != 1 {
		t.Fatalf("third pass: stored %d payloads, want 1 (the aged active war)", stored)
	}
	if got := transport.calls.Load(); got != before+1 {
		t.Fatalf("third pass made %d calls, want 1", got-before)
	}
}

// TestUserCorporationIDsAreScopedToTheUser: the wars page flags
// wars involving the signed-in user's corporations. The lookup
// must never leak another user's corporations into that set:
// user B's corp is not user A's "yours".
func TestUserCorporationIDsAreScopedToTheUser(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	userA, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	seedCharacter(t, q, userA.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, userB.ID, fixtureCharB, "Fixture Beta")
	now := time.Now().UTC()
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID: fixtureCharA, CorporationID: 98000001, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed corp A: %v", err)
	}
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID: fixtureCharB, CorporationID: 98000002, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed corp B: %v", err)
	}

	corpsA := app.userCorporationIDs(ctx, userA.ID)
	if len(corpsA) != 1 || !corpsA[98000001] {
		t.Fatalf("user A corps: %v, want only 98000001", corpsA)
	}
	corpsB := app.userCorporationIDs(ctx, userB.ID)
	if len(corpsB) != 1 || !corpsB[98000002] {
		t.Fatalf("user B corps: %v, want only 98000002", corpsB)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("corporation lookup made %d outbound calls, want 0", got)
	}
}
