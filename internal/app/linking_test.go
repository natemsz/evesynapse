package app

// Hermetic tests for the linking pass: the shared link helpers,
// the public pilot page (+ its worker queue), the item details
// page (+ its description queue), and the zKillboard links in
// kill contexts. Same two-layer approach as the other suites:
// renders run against a counting transport that must stay at zero
// calls; worker drains run against a path-routing stub.

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
)

// ---------------------------------------------------------------------------
// Helper policy: each name class resolves to its page.
// ---------------------------------------------------------------------------

func TestLinkHelpers(t *testing.T) {
	viewer := map[int64]bool{90000001: true}
	const own, stranger, item = 90000001, 93300001, 34

	cases := []struct {
		name string
		got  template.HTML
		want string
	}{
		{"item", itemLink(item, "Tritanium"), `<a href="/items/type/34/">Tritanium</a>`},
		{"item no id", itemLink(0, "Tritanium"), `Tritanium`},
		{"item no name", itemLink(item, ""), ``},
		{"own char", charLink(viewer, own, "Fixture Ceo"), `<a href="/character/?character=90000001">Fixture Ceo</a>`},
		{"stranger", charLink(viewer, stranger, "Member One"), `<a href="/pilot/?character=93300001">Member One</a>`},
		{"char no id", charLink(viewer, 0, "NPC"), `NPC`},
		{"kill own", killCharLink(viewer, own, "Fixture Ceo"), `<a href="/character/?character=90000001">Fixture Ceo</a>`},
		{"kill stranger", killCharLink(viewer, stranger, "Member One"), `<a href="https://zkillboard.com/character/93300001/" target="_blank" rel="noopener noreferrer">Member One</a>`},
		{"kill no id", killCharLink(viewer, 0, "NPC"), `NPC`},
		{"zkill kill", zkillKillLink(7001), `<a href="https://zkillboard.com/kill/7001/" target="_blank" rel="noopener noreferrer">View on zKillboard</a>`},
		{"zkill kill none", zkillKillLink(0), ``},
		{"name escaped", itemLink(item, `<b>"x"</b>`), `<a href="/items/type/34/">&lt;b&gt;&#34;x&#34;&lt;/b&gt;</a>`},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Pilot page: states, own-character redirect, worker drain.
// ---------------------------------------------------------------------------

// pilotStub serves ESI's public pilot endpoints from fixtures.
type pilotStub struct {
	profile  string // 404 when empty
	history  string
	corps    map[int64]string // corporation id -> corporation payload
	types    map[int64]string // type id -> type payload
	factions string
	calls    int
}

func (s *pilotStub) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (s *pilotStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	path := req.URL.Path
	switch {
	case strings.HasPrefix(path, "/characters/") && strings.HasSuffix(path, "/corporationhistory/"):
		if s.history == "" {
			return s.respond(http.StatusNotFound, `{"error":"no history"}`), nil
		}
		return s.respond(http.StatusOK, s.history), nil
	case strings.HasPrefix(path, "/characters/"):
		if s.profile == "" {
			return s.respond(http.StatusNotFound, `{"error":"character not found"}`), nil
		}
		return s.respond(http.StatusOK, s.profile), nil
	case strings.HasPrefix(path, "/corporations/"):
		var id int64
		for _, c := range strings.TrimSuffix(strings.TrimPrefix(path, "/corporations/"), "/") {
			if c >= '0' && c <= '9' {
				id = id*10 + int64(c-'0')
			}
		}
		if body, ok := s.corps[id]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"corporation not found"}`), nil
	case strings.HasPrefix(path, "/alliances/"):
		return s.respond(http.StatusOK, `{"name":"Fixture Alliance","ticker":"FALL","creator_id":1,"creator_corporation_id":1,"date_founded":"2010-01-01T00:00:00Z","executor_corporation_id":1}`), nil
	case path == "/universe/factions/":
		return s.respond(http.StatusOK, s.factions), nil
	case strings.HasPrefix(path, "/universe/types/"):
		var id int64
		for _, c := range strings.TrimSuffix(strings.TrimPrefix(path, "/universe/types/"), "/") {
			if c >= '0' && c <= '9' {
				id = id*10 + int64(c-'0')
			}
		}
		if body, ok := s.types[id]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"type not found"}`), nil
	}
	return s.respond(http.StatusInternalServerError, `{"error":"unexpected path"}`), nil
}

func TestPilotPageStatesAndDrain(t *testing.T) {
	ctx := context.Background()

	// --- Render layer: loading state enqueues, own char redirects,
	// and nothing calls out. ---
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/pilot/?character=93300001")
	if code != http.StatusOK {
		t.Fatalf("pilot loading page: status %d", code)
	}
	mustContain(t, "/pilot/ (loading)", body, "Loading this pilot's public record")
	rec, err := q.GetPilotRecord(ctx, 93300001)
	if err != nil || rec.State != "pending" {
		t.Fatalf("pilot want: %+v err=%v, want pending", rec, err)
	}

	// Own character: redirect to the full sheet.
	req := newAuthedRequest(t, app, cookie, "/pilot/?character=90000001")
	if req.code != http.StatusSeeOther || !strings.Contains(req.location, "/character/?character=90000001") {
		t.Fatalf("own pilot: code %d location %q, want 303 to the character sheet", req.code, req.location)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("pilot renders made %d outbound calls, want 0", got)
	}

	// --- Worker layer: the drain assembles the public record. ---
	stub := &pilotStub{
		profile: `{"name":"Stranger One","corporation_id":98000001,"alliance_id":99000001,"faction_id":500001,` +
			`"birthday":"2011-03-04T05:06:07Z","security_status":-3.456,` +
			`"description":"Hello <b>capsuleer</b><script>alert(1)</script>","title":"Fixture Title"}`,
		history: `[{"corporation_id":98000002,"start_date":"2015-01-01T00:00:00Z","record_id":2},` +
			`{"corporation_id":98000001,"start_date":"2020-06-01T00:00:00Z","record_id":3}]`,
		corps: map[int64]string{
			98000001: `{"name":"Current Corp","ticker":"CURR","member_count":10,"ceo_id":1,"tax_rate":0.1}`,
			98000002: `{"name":"Old Corp","ticker":"OLD","member_count":5,"ceo_id":1,"tax_rate":0.1}`,
		},
		factions: `[{"faction_id":500001,"name":"Caldari State","corporation_id":1000035}]`,
	}
	app2, _, q2 := buildCorpTestApp(t, stub)
	user2, err := q2.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user 2: %v", err)
	}
	seedCharacter(t, q2, user2.ID, fixtureCharA, "Fixture Ceo")
	cookie2 := sessionCookie(t, app2, user2.ID, fixtureCharA, "Fixture Ceo")

	// Enqueue through the page (loading state), then drain.
	if code, _ := getPage(t, app2, cookie2, "/pilot/?character=93300001"); code != http.StatusOK {
		t.Fatalf("pilot enqueue page: status %d", code)
	}
	drained, limited := app2.refreshPilotRecords(ctx, &fetchBudget{left: 120})
	if limited || drained != 1 {
		t.Fatalf("pilot drain: drained=%d limited=%v, want 1/false", drained, limited)
	}
	rec, err = q2.GetPilotRecord(ctx, 93300001)
	if err != nil || rec.State != "ready" {
		t.Fatalf("pilot record after drain: %+v err=%v, want ready", rec, err)
	}

	code, body = getPage(t, app2, cookie2, "/pilot/?character=93300001")
	if code != http.StatusOK {
		t.Fatalf("pilot page: status %d", code)
	}
	mustContain(t, "/pilot/", body,
		"Stranger One",
		"Current Corp [CURR]",
		"Fixture Alliance [FALL]",
		"Caldari State",
		"-3.46",
		"2011-03-04",
		"Old Corp", "2015-01-01", "2020-06-01", "Present",
		"Hello", "capsuleer",
	)
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "alert(1)") {
		t.Fatal("pilot description rendered unsanitized")
	}

	// Unknown character id: drain settles 'missing', page says so.
	if err := q2.UpsertPilotWant(ctx, 93300099); err != nil {
		t.Fatalf("enqueue missing pilot: %v", err)
	}
	stub.profile = "" // ESI 404s every profile now
	drained, _ = app2.refreshPilotRecords(ctx, &fetchBudget{left: 120})
	if drained != 1 {
		t.Fatalf("missing pilot drain: drained=%d, want 1", drained)
	}
	rec, err = q2.GetPilotRecord(ctx, 93300099)
	if err != nil || rec.State != "missing" {
		t.Fatalf("missing pilot record: %+v err=%v, want missing", rec, err)
	}
	code, body = getPage(t, app2, cookie2, "/pilot/?character=93300099")
	if code != http.StatusOK {
		t.Fatalf("pilot missing page: status %d", code)
	}
	mustContain(t, "/pilot/ (missing)", body, "EVE has no public record for that character")
}

type authedResponse struct {
	code     int
	location string
}

// newAuthedRequest issues one GET without following redirects.
func newAuthedRequest(t *testing.T, app *Application, cookie *http.Cookie, path string) authedResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return authedResponse{code: rec.Code, location: rec.Header().Get("Location")}
}

// TestAllTemplatesParse parses every page template with the
// production FuncMap, so a helper-name typo (or a field added to
// one side only) fails here rather than at first render.
func TestAllTemplatesParse(t *testing.T) {
	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "base.html" || !strings.HasSuffix(name, ".html") {
			continue
		}
		if _, err := template.New("base").Funcs(linkFuncMap()).ParseFS(templatesFS,
			"templates/base.html", "templates/"+name); err != nil {
			t.Errorf("parse %s: %v", name, err)
		}
	}
}

// TestMigration015Reopen proves the guarded pilot_records /
// type_details migration is idempotent: a database created before
// this build reopens cleanly and the queues work.
func TestMigration015Reopen(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	for i := 0; i < 2; i++ {
		conn, pool, err := openDB(context.Background(), dsn)
		if err != nil {
			t.Fatalf("openDB (pass %d): %v", i, err)
		}
		q := db.New(conn)
		if err := q.UpsertPilotWant(ctx, 93300001); err != nil {
			t.Fatalf("pilot want (pass %d): %v", i, err)
		}
		if err := q.UpsertTypeDetailWant(ctx, 34); err != nil {
			t.Fatalf("type want (pass %d): %v", i, err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close (pass %d): %v", i, err)
		}
		pool.Close()
	}
}

// ---------------------------------------------------------------------------
// Item details page + type-description queue.
// ---------------------------------------------------------------------------

func TestItemDetailsPageAndTypeDetailDrain(t *testing.T) {
	ctx := context.Background()

	// Render layer: unknown type bounces, known type renders the
	// filling-in state and notes the want — zero outbound calls.
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (900, 'Fixture Category')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (910, 'Fixture Minerals', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 910, 2, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES (990, 34, 1, 100, 600)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (1001, 'Fixture Widget', 910, 2, 1)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES (991, 1001, 1, 100, 600)`,
		`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES (991, 34, 7)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	if code, _ := getPage(t, app, cookie, "/items/type/424242/"); code != http.StatusSeeOther {
		t.Fatalf("unknown item type: status %d, want redirect", code)
	}

	code, body := getPage(t, app, cookie, "/items/type/34/")
	if code != http.StatusOK {
		t.Fatalf("item details page: status %d", code)
	}
	mustContain(t, "/items/type/34/", body,
		"Tritanium", "Fixture Category", "Fixture Minerals",
		"Traded on the market",
		"Orders &amp; price history", `href="/market/?type=34"`,
		"Plan manufacturing", `href="/planner/?product=34"`,
		"This description is queued",
		"Fixture Widget", `href="/items/type/1001/"`,
	)
	if _, err := q.GetTypeDetail(ctx, 34); err != nil {
		t.Fatalf("type detail want not noted: %v", err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("item details renders made %d outbound calls, want 0", got)
	}

	// Worker layer: the drain stores the public description.
	stub := &pilotStub{types: map[int64]string{
		34: `{"name":"Tritanium","group_id":910,"description":"The building block <b>of everything</b>.","published":true}`,
	}}
	app2, conn2, q2 := buildCorpTestApp(t, stub)
	user2, err := q2.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user 2: %v", err)
	}
	seedCharacter(t, q2, user2.ID, fixtureCharA, "Fixture Ceo")
	cookie2 := sessionCookie(t, app2, user2.ID, fixtureCharA, "Fixture Ceo")
	if _, err := conn2.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 910, 2, 1)`); err != nil {
		t.Fatalf("seed sde 2: %v", err)
	}

	if code, _ := getPage(t, app2, cookie2, "/items/type/34/"); code != http.StatusOK {
		t.Fatalf("item details page 2: status %d", code)
	}
	drained, limited := app2.refreshTypeDetails(ctx, &fetchBudget{left: 120})
	if limited || drained != 1 {
		t.Fatalf("type detail drain: drained=%d limited=%v, want 1/false", drained, limited)
	}
	code, body = getPage(t, app2, cookie2, "/items/type/34/")
	if code != http.StatusOK {
		t.Fatalf("item details page after drain: status %d", code)
	}
	mustContain(t, "/items/type/34/ (described)", body, "The building block", "of everything")
	if strings.Contains(body, "This description is queued") {
		t.Fatal("described item page still claims the description is loading")
	}
}

// ---------------------------------------------------------------------------
// Kill contexts: strangers to zKillboard, own chars in-app, ships
// to item details — and item links are uniform across pages.
// ---------------------------------------------------------------------------

func TestKillmailAndWalletLinks(t *testing.T) {
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
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	// Two mails: 7001 lost by a stranger (own char lands the final
	// blow), 7002 lost by the viewer's own character.
	seedSnapshot(t, q, fixtureCharA, esi.SnapKillmails, []esi.KillmailRef{
		{KillmailID: 7001, KillmailHash: "aaa"},
		{KillmailID: 7002, KillmailHash: "bbb"},
	})
	mails := []esi.Killmail{
		{
			KillmailID: 7001, KillmailTime: recently, SolarSystemID: 30000142,
			Victim:    esi.KillmailVictim{CharacterID: 93300088, CorporationID: 99000001, ShipTypeID: 587},
			Attackers: []esi.KillmailAttacker{{CharacterID: fixtureCharA, CorporationID: 98000001, ShipTypeID: 587, FinalBlow: true}},
		},
		{
			KillmailID: 7002, KillmailTime: recently, SolarSystemID: 30000142,
			Victim:    esi.KillmailVictim{CharacterID: fixtureCharA, CorporationID: 98000001, ShipTypeID: 587},
			Attackers: []esi.KillmailAttacker{{CharacterID: 93300077, CorporationID: 99000001, ShipTypeID: 587, FinalBlow: true}},
		},
	}
	for _, km := range mails {
		raw, err := json.Marshal(km)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
			KillmailID: km.KillmailID, CharacterID: fixtureCharA, Hash: "fixture",
			Payload: string(raw), FetchedAt: mustTime("2026-01-01T00:00:00Z"),
		}); err != nil {
			t.Fatalf("seed killmail detail %d: %v", km.KillmailID, err)
		}
	}
	app.esi.StoreCharacterName(fixtureCharA, "Fixture Ceo")
	app.esi.StoreCharacterName(93300088, "Victim EightyEight")
	app.esi.StoreCharacterName(93300077, "Attacker SevenSeven")
	app.esi.StoreCharacterName(fixtureMember, "Member One")

	code, body := getPage(t, app, cookie, "/killmails/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("killmails page: status %d", code)
	}
	mustContain(t, "/killmails/", body,
		// Strangers in a kill context go to zKillboard.
		`<a href="https://zkillboard.com/character/93300088/" target="_blank" rel="noopener noreferrer">Victim EightyEight</a>`,
		`<a href="https://zkillboard.com/character/93300077/" target="_blank" rel="noopener noreferrer">Attacker SevenSeven</a>`,
		// Every displayed kill carries its zKillboard kill link.
		`<a href="https://zkillboard.com/kill/7001/" target="_blank" rel="noopener noreferrer">View on zKillboard</a>`,
		`<a href="https://zkillboard.com/kill/7002/" target="_blank" rel="noopener noreferrer">View on zKillboard</a>`,
		// Victim ships go to item details.
		`<a href="/items/type/587/">Rifter</a>`,
	)
	// The own character — victim of 7002, final blow of 7001 —
	// stays in-app even in a kill context.
	ownLink := `<a href="/character/?character=90000001">Fixture Ceo</a>`
	if n := strings.Count(body, ownLink); n != 2 {
		t.Errorf("own character linked in-app %d times on the kill page, want 2 (victim + final blow)", n)
	}
	if strings.Contains(body, "https://zkillboard.com/character/90000001/") {
		t.Error("own character sent to zKillboard from a kill page")
	}

	// Wallet: the same item name, the same item-details target,
	// and the counterparty character to the pilot page.
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletTxns, []esi.WalletTransaction{
		{TransactionID: 9, Date: recently, TypeID: 34, LocationID: 60003760, UnitPrice: 5.5, Quantity: 1000, ClientID: fixtureMember, IsBuy: true},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, []esi.WalletJournalEntry{
		{ID: 1, Date: recently, RefType: "player_donation", Amount: 1000, Balance: 2000,
			FirstPartyID: fixtureMember, FirstPartyType: "character",
			SecondPartyID: fixtureCharA, SecondPartyType: "character", Description: "thanks"},
	})

	code, body = getPage(t, app, cookie, "/wallet/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("wallet page: status %d", code)
	}
	mustContain(t, "/wallet/", body,
		`<a href="/items/type/34/">Tritanium</a>`,                  // transaction item
		`<a href="/pilot/?character=93300001">Member One</a>`,      // transaction counterparty + journal sender
		`<a href="/character/?character=90000001">Fixture Ceo</a>`, // journal recipient is the viewer
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("link sweep renders made %d outbound calls, want 0", got)
	}
}
