package app

// Hermetic tests for the corporation cluster (module sweep,
// cluster 2). Two layers:
//
//   - Render tests: seeded corp_* snapshots, killmail_details rows
//     and snapshot_fetch_state records drive the real chi router
//     with a counting stub transport, proving every new page
//     renders fixture content, that role-missing empty states
//     appear where the worker recorded a 403, and that no handler
//     makes an outbound call (cluster rule: pages render cache-only).
//   - Worker tests: a path-routing stub transport stands in for
//     ESI, proving refreshCorpSnapshots stores snapshots, records
//     a 403 as role-missing state without writing a snapshot, and
//     settles to zero calls once everything is fresh.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

const (
	fixtureCharA  = int64(90000001)
	fixtureCharB  = int64(90000002)
	fixtureCorpA  = int64(98000001)
	fixtureCorpB  = int64(98000002)
	fixtureMember = int64(93300001)
)

// countingTransport fails every request and counts calls; handlers
// must never reach it.
type countingTransport struct{ calls atomic.Int64 }

func (s *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"must not be called"}`)),
	}, nil
}

// buildCorpTestApp builds an Application over a fresh embedded-
// Postgres test database with a stub ESI transport (no worker
// goroutine — tests drive the worker functions directly).
func buildCorpTestApp(t *testing.T, transport http.RoundTripper) (*Application, *sql.DB, *db.Queries) {
	t.Helper()
	conn, pool, err := store.Open(context.Background(), pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close(); pool.Close() })
	queries := db.New(conn)

	sessionManager := scs.New()
	sessionManager.Store = pgxstore.New(pool)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"

	client := esi.New(&http.Client{Transport: transport}, queries,
		func(context.Context, db.Character) (string, error) { return "fixture", nil })

	app := &Application{
		cfg:           Config{adminCharIDs: map[int64]bool{}},
		sessions:      sessionManager,
		queries:       queries,
		esi:           client,
		db:            conn,
		pool:          pool,
		corpCache:     make(map[int64]corpCacheEntry),
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
	}
	return app, conn, queries
}

// grantTestAdmin marks characterID as an admin in the test app's
// config (Issues 23/24): /admin/ and /sync/ now require admin
// character identity, so tests hitting those pages must opt in.
func grantTestAdmin(app *Application, characterID int64) {
	if app.cfg.adminCharIDs == nil {
		app.cfg.adminCharIDs = map[int64]bool{}
	}
	app.cfg.adminCharIDs[characterID] = true
}

// seedSnapshot stores one snapshot payload marked fresh until 2999.
func seedSnapshot(t *testing.T, q *db.Queries, characterID int64, kind string, payload any) {
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
	if err := q.UpsertSnapshot(context.Background(), db.UpsertSnapshotParams{
		CharacterID: characterID,
		Kind:        kind,
		Payload:     raw,
		FetchedAt:   mustTime("2026-01-01T00:00:00Z"),
		CachedUntil: mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed snapshot %s: %v", kind, err)
	}
}

func seedCharacter(t *testing.T, q *db.Queries, userID, characterID int64, name string) db.Character {
	t.Helper()
	ch, err := q.UpsertCharacter(context.Background(), db.UpsertCharacterParams{
		CharacterID:  characterID,
		UserID:       userID,
		Name:         name,
		AccessToken:  "fixture",
		RefreshToken: "fixture",
		TokenExpiry:  mustNullTime("2999-01-01T00:00:00Z"),
		LinkState:    "ok",
	})
	if err != nil {
		t.Fatalf("seed character %d: %v", characterID, err)
	}
	return ch
}

// sessionCookie mints a signed-in session for the user.
func sessionCookie(t *testing.T, app *Application, userID, characterID int64, name string) *http.Cookie {
	t.Helper()
	ctx, err := app.sessions.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	app.sessions.Put(ctx, sessionAuthenticated, true)
	app.sessions.Put(ctx, sessionUserID, int(userID))
	app.sessions.Put(ctx, sessionCharacterID, int(characterID))
	app.sessions.Put(ctx, sessionCharacterName, name)
	token, _, err := app.sessions.Commit(ctx)
	if err != nil {
		t.Fatalf("session commit: %v", err)
	}
	return &http.Cookie{Name: "evesynapse_session", Value: token, Path: "/"}
}

func getPage(t *testing.T, app *Application, cookie *http.Cookie, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func mustContain(t *testing.T, path, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("GET %s: body missing %q", path, want)
		}
	}
}

// TestCorpPagesRenderFromSnapshots is the render-layer proof:
// every corporation subpage serves fixture content from local
// rows, the role-missing character sees plain role states, and
// the transport stays at zero calls throughout.
func TestCorpPagesRenderFromSnapshots(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	grantTestAdmin(app, fixtureCharA)
	grantTestAdmin(app, fixtureCharB)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")

	// SDE fixtures: two types, a station, a system, a region.
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18), (35832, 'Astrahus', 1404)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	// Character -> corporation mappings.
	for _, m := range []struct{ charID, corpID int64 }{{fixtureCharA, fixtureCorpA}, {fixtureCharB, fixtureCorpB}} {
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
			CharacterID:   m.charID,
			CorporationID: m.corpID,
			UpdatedAt:     now,
		}); err != nil {
			t.Fatalf("seed mapping: %v", err)
		}
	}

	// Character A: the full corporation dataset.
	recently := now.Add(-time.Hour).Format(time.RFC3339)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpInfo, `{"name":"Fixture Corp","ticker":"FXC","member_count":3,"tax_rate":0.05,"ceo_id":90000001}`)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMembers, []int64{fixtureCharA, fixtureMember, 93300002})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMemberTracking, []esi.CorpMemberTracking{
		{CharacterID: fixtureMember, StartDate: "2020-05-01T00:00:00Z", LogonDate: recently, ShipTypeID: 587, LocationID: 60003760},
		{CharacterID: 93300002, StartDate: "2021-06-15T00:00:00Z", LogonDate: recently, ShipTypeID: 587, LocationID: 60003760},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpWallets, []esi.CorpWalletDivision{
		{Division: 1, Balance: 123456789.5},
		{Division: 2, Balance: 5000},
	})
	seedSnapshot(t, q, fixtureCharA, esi.CorpJournalKind(1), []esi.CorpJournalEntry{
		{ID: 1, Date: recently, RefType: "player_trading", Amount: -1500000.25, Balance: 121956789.25, Description: "Fixture purchase"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.CorpTxnsKind(1), []esi.CorpWalletTransaction{
		{TransactionID: 1, Date: recently, TypeID: 34, LocationID: 60003760, UnitPrice: 5.5, Quantity: 1000, ClientID: fixtureMember, IsBuy: true, JournalRefID: 1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpOrders, []esi.CorpOrder{
		{OrderID: 1, TypeID: 587, LocationID: 60003760, RegionID: 10000002, Price: 1234567.89, VolumeTotal: 10, VolumeRemain: 7, Duration: 90, Issued: now.AddDate(0, 0, -10).Format(time.RFC3339), IssuedBy: fixtureMember},
	})
	structureID := int64(1022734985671)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpStructures, []esi.CorpStructure{
		{StructureID: structureID, CorporationID: fixtureCorpA, TypeID: 35832, SystemID: 30000142, Name: "Fixture Astra", State: "shield_full", FuelExpires: now.Add(72 * time.Hour).Format(time.RFC3339), Services: []esi.CorpStructureService{{Name: "Market", State: "online"}}},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpAssets, []esi.Asset{
		{ItemID: 1001, TypeID: 587, LocationID: 60003760, LocationType: "station", LocationFlag: "CorpSAG1", IsSingleton: true, Quantity: 1},
		{ItemID: 1002, TypeID: 34, LocationID: 60003760, LocationType: "station", LocationFlag: "CorpSAG1", Quantity: 5000},
		{ItemID: 1003, TypeID: 587, LocationID: structureID, LocationType: "item", LocationFlag: "CorpHangar", Quantity: 2},
	})
	// The character's own recent list (for the cluster-1 page
	// regression render below): same two mails, both kills from
	// the character's point of view.
	seedSnapshot(t, q, fixtureCharA, esi.SnapKillmails, []esi.KillmailRef{
		{KillmailID: 7001, KillmailHash: "aaa"},
		{KillmailID: 7002, KillmailHash: "bbb"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpKillmails, []esi.KillmailRef{
		{KillmailID: 7001, KillmailHash: "aaa"},
		{KillmailID: 7002, KillmailHash: "bbb"},
	})
	if err := q.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: 1001, Name: "Fixture Flagship"}); err != nil {
		t.Fatalf("seed item name: %v", err)
	}

	// Killmail details behind the corp list: one corp loss (victim
	// in the fixture corp), one corp kill (victim elsewhere).
	loss := esi.Killmail{
		KillmailID: 7001, KillmailTime: recently, SolarSystemID: 30000142,
		Victim:    esi.KillmailVictim{CharacterID: fixtureMember, CorporationID: fixtureCorpA, ShipTypeID: 587},
		Attackers: []esi.KillmailAttacker{{CharacterID: 93300077, CorporationID: 99000001, ShipTypeID: 587, FinalBlow: true}},
	}
	kill := esi.Killmail{
		KillmailID: 7002, KillmailTime: recently, SolarSystemID: 30000142,
		Victim:    esi.KillmailVictim{CharacterID: 93300088, CorporationID: 99000001, ShipTypeID: 587},
		Attackers: []esi.KillmailAttacker{{CharacterID: fixtureMember, CorporationID: fixtureCorpA, ShipTypeID: 587, FinalBlow: true}},
	}
	for _, km := range []esi.Killmail{loss, kill} {
		raw, err := json.Marshal(km)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
			KillmailID:  km.KillmailID,
			CharacterID: fixtureCharA,
			Hash:        "fixture",
			Payload:     string(raw),
			FetchedAt:   mustTime("2026-01-01T00:00:00Z"),
		}); err != nil {
			t.Fatalf("seed killmail detail %d: %v", km.KillmailID, err)
		}
	}

	// Character names in the worker-warmed in-process cache.
	for id, name := range map[int64]string{
		fixtureCharA:  "Fixture Ceo",
		fixtureMember: "Member One",
		93300002:      "Member Two",
		93300077:      "Attacker SevenSeven",
		93300088:      "Victim EightyEight",
	} {
		app.esi.StoreCharacterName(id, name)
	}

	// Character B: roster only; tracking and wallets refused for
	// want of roles, structures never attempted.
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpInfo, `{"name":"Other Corp","ticker":"OTHR","member_count":2}`)
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpMembers, []int64{fixtureCharB})
	for kind, detail := range map[string]string{
		esi.SnapCorpMemberTracking: "Director",
		esi.SnapCorpWallets:        "Accountant or Junior Accountant",
	} {
		if err := q.UpsertSnapshotFetchState(ctx, db.UpsertSnapshotFetchStateParams{
			CharacterID: fixtureCharB,
			Kind:        kind,
			State:       fetchStateRoleMissing,
			Detail:      detail,
			AttemptedAt: now,
		}); err != nil {
			t.Fatalf("seed fetch state %s: %v", kind, err)
		}
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/corporations/members/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("members page: status %d", code)
	}
	mustContain(t, "/corporations/members/", body,
		"Fixture Corp [FXC]", "Member One", "Member Two", "Rifter",
		"Jita 4 - Moon 4 - Caldari Navy Assembly Plant", "2020-05-01")

	code, body = getPage(t, app, cookie, "/corporations/wallets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("wallets page: status %d", code)
	}
	mustContain(t, "/corporations/wallets/", body,
		"Master Wallet", "123,456,789.50", "player trading", "121,956,789.25",
		"Fixture purchase", "Tritanium", "Buy", "Member One")

	code, body = getPage(t, app, cookie, "/corporations/orders/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("orders page: status %d", code)
	}
	mustContain(t, "/corporations/orders/", body,
		"Rifter", "1,234,567.89", "7 / 10", "The Forge", "Member One")

	code, body = getPage(t, app, cookie, "/corporations/assets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("assets page: status %d", code)
	}
	mustContain(t, "/corporations/assets/", body,
		"Fixture Flagship", "Tritanium", "Fixture Astra",
		"Jita 4 - Moon 4 - Caldari Navy Assembly Plant")

	code, body = getPage(t, app, cookie, "/corporations/structures/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("structures page: status %d", code)
	}
	mustContain(t, "/corporations/structures/", body,
		"Fixture Astra", "Astrahus", "Jita", "shield full", "Market (online)")

	code, body = getPage(t, app, cookie, "/corporations/killmails/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("corp killmails page: status %d", code)
	}
	mustContain(t, "/corporations/killmails/", body,
		"Fixture Corp [FXC] — Killmails", "LOSS", "KILL", "Member One", "Victim EightyEight")

	// The cluster-1 character view renders the same stored details
	// (both kills from this character's point of view).
	code, body = getPage(t, app, cookie, "/killmails/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("character killmails page: status %d", code)
	}
	mustContain(t, "/killmails/", body, "Fixture Ceo — Killmails", "KILL", "Member One")
	if strings.Contains(body, "LOSS") {
		t.Error("character killmails view shows a LOSS badge for mails the character only won")
	}

	// Character B: role-missing empty states, not endless warming.
	code, body = getPage(t, app, cookie, "/corporations/members/?character=90000002")
	if code != http.StatusOK {
		t.Fatalf("members page (B): status %d", code)
	}
	mustContain(t, "/corporations/members/ (B)", body,
		"Other Corp [OTHR]", "Second Pilot", "Member tracking needs the Director role in-game")

	code, body = getPage(t, app, cookie, "/corporations/wallets/?character=90000002")
	if code != http.StatusOK {
		t.Fatalf("wallets page (B): status %d", code)
	}
	mustContain(t, "/corporations/wallets/ (B)", body,
		"Needs the", "Accountant or Junior Accountant", "role in-game")

	// The Sync page explains the same states.
	code, body = getPage(t, app, cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("sync page: status %d", code)
	}
	mustContain(t, "/sync/", body, "corp_membertracking", "Role missing", "needs the Director role")

	// The whole point: not one outbound call served any of this.
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// corpStubTransport routes the worker's ESI calls for the fetch
// test: everything succeeds except membertracking, which refuses
// with 403 (no Director role).
type corpStubTransport struct{ calls atomic.Int64 }

func (s *corpStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	path := req.URL.Path
	respond := func(status int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	switch {
	case strings.HasSuffix(path, "/membertracking/"):
		return respond(http.StatusForbidden, `{"error":"forbidden"}`)
	case path == fmt.Sprintf("/characters/%d/", fixtureCharA):
		return respond(http.StatusOK, `{"name":"Fixture Ceo","corporation_id":98000001,"birthday":"2010-01-01T00:00:00Z"}`)
	case path == fmt.Sprintf("/corporations/%d/", fixtureCorpA):
		return respond(http.StatusOK, `{"name":"Fixture Corp","ticker":"FXC","member_count":3,"tax_rate":0.05,"ceo_id":90000001}`)
	case strings.HasSuffix(path, "/members/"):
		return respond(http.StatusOK, `[90000001,93300001]`)
	case strings.HasSuffix(path, "/wallets/"):
		return respond(http.StatusOK, `[{"division":1,"balance":1000.5}]`)
	case strings.Contains(path, "/journal/"):
		return respond(http.StatusOK, `[]`)
	case strings.Contains(path, "/transactions/"):
		return respond(http.StatusOK, `[]`)
	case strings.HasSuffix(path, "/orders/"):
		return respond(http.StatusOK, `[]`)
	case strings.HasSuffix(path, "/assets/names/"):
		return respond(http.StatusOK, `[]`)
	case strings.HasSuffix(path, "/assets/"):
		return respond(http.StatusOK, `[]`)
	case strings.HasSuffix(path, "/structures/"):
		return respond(http.StatusOK, `[]`)
	case strings.HasSuffix(path, "/killmails/recent/"):
		return respond(http.StatusOK, `[]`)
	default:
		return respond(http.StatusNotFound, `{"error":"unexpected path `+path+`"}`)
	}
}

// TestRefreshCorpSnapshotsRoleMissing proves the worker contract:
// a 403 becomes recorded role-missing state (no snapshot written),
// everything else is stored, and a second pass with fresh
// snapshots makes no calls at all.
func TestRefreshCorpSnapshotsRoleMissing(t *testing.T) {
	transport := &corpStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	if got := app.refreshCorpSnapshots(ctx, ch); got == 0 {
		t.Fatal("first pass: no snapshots stored")
	}

	// The mapping resolved from the public character sheet.
	mapping, err := q.GetCharacterCorporation(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("corporation mapping: %v", err)
	}
	if mapping.CorporationID != fixtureCorpA {
		t.Fatalf("corporation mapping: got %d, want %d", mapping.CorporationID, fixtureCorpA)
	}

	// Member tracking: role-missing recorded, and crucially no
	// snapshot row — a 403 must not poison the cache.
	state, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: fixtureCharA, Kind: esi.SnapCorpMemberTracking})
	if err != nil {
		t.Fatalf("membertracking fetch state: %v", err)
	}
	if state.State != fetchStateRoleMissing || state.Detail != "Director" {
		t.Fatalf("membertracking fetch state: got %q/%q, want role_missing/Director", state.State, state.Detail)
	}
	if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapCorpMemberTracking}); err == nil {
		t.Fatal("membertracking snapshot exists after a 403; the cache was poisoned")
	}

	// Everything else stored and marked ok.
	for _, kind := range []string{esi.SnapCorpInfo, esi.SnapCorpMembers, esi.SnapCorpWallets, esi.SnapCorpOrders, esi.SnapCorpKillmails} {
		if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: kind}); err != nil {
			t.Errorf("snapshot %s missing after pass: %v", kind, err)
		}
	}
	if st, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: fixtureCharA, Kind: esi.SnapCorpMembers}); err != nil || st.State != fetchStateOK {
		t.Errorf("members fetch state: %+v, %v; want ok", st, err)
	}

	// Second pass: fresh snapshots + role-missing backoff mean
	// no further ESI calls.
	before := transport.calls.Load()
	app.refreshCorpSnapshots(ctx, ch)
	if got := transport.calls.Load(); got != before {
		t.Fatalf("second pass made %d calls, want 0 (fresh/backoff)", got-before)
	}
}
