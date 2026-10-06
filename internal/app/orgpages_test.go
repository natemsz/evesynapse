package app

// Hermetic tests for the corporation & alliance destination
// pages (v0.3.12): the link helpers, the page states, the worker
// drains behind the organization queues, and the swept surfaces
// (pilot page, contacts) that now link organization names. Same
// two-layer approach as the pilot suite: renders run against a
// counting transport that must stay at zero calls; worker drains
// run against a path-routing stub.

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
)

// ---------------------------------------------------------------------------
// Helper policy: organizations resolve to their pages.
// ---------------------------------------------------------------------------

func TestOrgLinkHelpers(t *testing.T) {
	cases := []struct {
		name string
		got  template.HTML
		want string
	}{
		{"corp", corpLink(98000001, "Fixture Corp"), `<a href="/corporation/?corporation=98000001">Fixture Corp</a>`},
		{"corp no id", corpLink(0, "Fixture Corp"), `Fixture Corp`},
		{"corp no name", corpLink(98000001, ""), ``},
		{"alliance", allianceLink(99000001, "Fixture Alliance"), `<a href="/alliance/?alliance=99000001">Fixture Alliance</a>`},
		{"alliance no id", allianceLink(0, "Fixture Alliance"), `Fixture Alliance`},
		{"alliance no name", allianceLink(99000001, ""), ``},
		{"corp name escaped", corpLink(98000001, `<b>"x"</b>`), `<a href="/corporation/?corporation=98000001">&lt;b&gt;&#34;x&#34;&lt;/b&gt;</a>`},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// orgStub serves ESI's public organization endpoints from
// fixtures: corporation profiles, alliance profiles, and alliance
// member-corporation lists. IDs absent from a map answer 404.
type orgStub struct {
	corps     map[int64]string // corporation id -> profile payload
	alliances map[int64]string // alliance id -> profile payload
	members   map[int64]string // alliance id -> [corporation ids] payload
	calls     int
}

func (s *orgStub) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func pathID(segment string) int64 {
	id, _ := strconv.ParseInt(segment, 10, 64)
	return id
}

func (s *orgStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	path := req.URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "corporations":
		if body, ok := s.corps[pathID(parts[1])]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"corporation not found"}`), nil
	case len(parts) == 3 && parts[0] == "alliances" && parts[2] == "corporations":
		if body, ok := s.members[pathID(parts[1])]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"alliance not found"}`), nil
	case len(parts) == 2 && parts[0] == "alliances":
		if body, ok := s.alliances[pathID(parts[1])]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"alliance not found"}`), nil
	}
	return s.respond(http.StatusInternalServerError, `{"error":"unexpected path"}`), nil
}

// ---------------------------------------------------------------------------
// Corporation page: states + worker drain.
// ---------------------------------------------------------------------------

func TestCorporationPageStatesAndDrain(t *testing.T) {
	ctx := context.Background()

	// --- Render layer: loading state enqueues, bad ids redirect,
	// and nothing calls out. ---
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/corporation/?corporation=98000001")
	if code != http.StatusOK {
		t.Fatalf("corporation loading page: status %d", code)
	}
	mustContain(t, "/corporation/ (loading)", body, "Loading this corporation's public record")
	rec, err := q.GetCorporationRecord(ctx, 98000001)
	if err != nil || rec.State != orgStatePending {
		t.Fatalf("corporation want: %+v err=%v, want pending", rec, err)
	}

	for _, path := range []string{"/corporation/", "/corporation/?corporation=-5", "/corporation/?corporation=abc"} {
		res := newAuthedRequest(t, app, cookie, path)
		if res.code != http.StatusSeeOther {
			t.Fatalf("%s: code %d, want 303 redirect", path, res.code)
		}
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("corporation renders made %d outbound calls, want 0", got)
	}

	// --- Worker layer: the drain assembles the public record. ---
	stub := &orgStub{
		corps: map[int64]string{
			98000001: `{"name":"Fixture Corp","ticker":"FXC","member_count":42,"ceo_id":90000001,` +
				`"alliance_id":99000001,"home_station_id":60003760,"tax_rate":0.075,` +
				`"date_founded":"2012-03-04T05:06:07Z","description":"Hello <b>capsuleer</b><script>alert(1)</script>"}`,
		},
		alliances: map[int64]string{
			99000001: `{"name":"Fixture Alliance","ticker":"FALL","creator_id":1,"creator_corporation_id":1,"date_founded":"2010-01-02T00:00:00Z","executor_corporation_id":1}`,
		},
	}
	app2, conn2, q2 := buildCorpTestApp(t, stub)
	user2, err := q2.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user 2: %v", err)
	}
	seedCharacter(t, q2, user2.ID, fixtureCharA, "Fixture Ceo")
	cookie2 := sessionCookie(t, app2, user2.ID, fixtureCharA, "Fixture Ceo")
	if _, err := conn2.ExecContext(ctx, `INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita IV - Moon 4 - Caldari Navy Assembly Plant', 30000142)`); err != nil {
		t.Fatalf("seed sde station: %v", err)
	}

	if code, _ := getPage(t, app2, cookie2, "/corporation/?corporation=98000001"); code != http.StatusOK {
		t.Fatalf("corporation enqueue page: status %d", code)
	}
	drained, limited := app2.refreshCorporationRecords(ctx, &fetchBudget{left: 120})
	if limited || drained != 1 {
		t.Fatalf("corporation drain: drained=%d limited=%v, want 1/false", drained, limited)
	}
	rec, err = q2.GetCorporationRecord(ctx, 98000001)
	if err != nil || rec.State != orgStateReady {
		t.Fatalf("corporation record after drain: %+v err=%v, want ready", rec, err)
	}

	code, body = getPage(t, app2, cookie2, "/corporation/?corporation=98000001")
	if code != http.StatusOK {
		t.Fatalf("corporation page: status %d", code)
	}
	mustContain(t, "/corporation/", body,
		"Fixture Corp", "[FXC]", "42", "7.5%", "2012-03-04",
		"Jita IV - Moon 4 - Caldari Navy Assembly Plant",
		`<a href="/character/?character=90000001">Fixture Ceo</a>`,
		`<a href="/alliance/?alliance=99000001">Fixture Alliance [FALL]</a>`,
		"Hello", "capsuleer",
	)
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "alert(1)") {
		t.Fatal("corporation description rendered unsanitized")
	}

	// The live-region fragment re-renders the same stored record.
	code, body = getPage(t, app2, cookie2, "/corporation/fragment?corporation=98000001")
	if code != http.StatusOK {
		t.Fatalf("corporation fragment: status %d", code)
	}
	mustContain(t, "/corporation/fragment", body, `data-poll-state="ready"`, "Fixture Corp")

	// Unknown corporation id: drain settles 'missing', page says so.
	if err := q2.UpsertCorporationWant(ctx, 98000999); err != nil {
		t.Fatalf("enqueue missing corporation: %v", err)
	}
	drained, _ = app2.refreshCorporationRecords(ctx, &fetchBudget{left: 120})
	if drained != 1 {
		t.Fatalf("missing corporation drain: drained=%d, want 1", drained)
	}
	rec, err = q2.GetCorporationRecord(ctx, 98000999)
	if err != nil || rec.State != orgStateMissing {
		t.Fatalf("missing corporation record: %+v err=%v, want missing", rec, err)
	}
	code, body = getPage(t, app2, cookie2, "/corporation/?corporation=98000999")
	if code != http.StatusOK {
		t.Fatalf("corporation missing page: status %d", code)
	}
	mustContain(t, "/corporation/ (missing)", body, "EVE has no public record for that corporation")
}

// ---------------------------------------------------------------------------
// Alliance page: states + worker drain (profile + member list).
// ---------------------------------------------------------------------------

func TestAlliancePageStatesAndDrain(t *testing.T) {
	ctx := context.Background()

	// --- Render layer. ---
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/alliance/?alliance=99000001")
	if code != http.StatusOK {
		t.Fatalf("alliance loading page: status %d", code)
	}
	mustContain(t, "/alliance/ (loading)", body, "Loading this alliance's public record")
	rec, err := q.GetAllianceRecord(ctx, 99000001)
	if err != nil || rec.State != orgStatePending {
		t.Fatalf("alliance want: %+v err=%v, want pending", rec, err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("alliance renders made %d outbound calls, want 0", got)
	}

	// --- Worker layer. ---
	stub := &orgStub{
		corps: map[int64]string{
			98000001: `{"name":"Alpha Corp","ticker":"ALPH","member_count":10,"ceo_id":1,"tax_rate":0.1}`,
			98000002: `{"name":"Beta Corp","ticker":"BETA","member_count":20,"ceo_id":1,"tax_rate":0.1}`,
		},
		alliances: map[int64]string{
			99000001: `{"name":"Fixture Alliance","ticker":"FALL","creator_id":93300001,` +
				`"creator_corporation_id":98000001,"executor_corporation_id":98000002,` +
				`"date_founded":"2010-01-02T03:04:05Z"}`,
		},
		members: map[int64]string{
			99000001: `[98000001, 98000002, 98000003]`,
		},
	}
	app2, _, q2 := buildCorpTestApp(t, stub)
	user2, err := q2.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user 2: %v", err)
	}
	seedCharacter(t, q2, user2.ID, fixtureCharA, "Fixture Ceo")
	cookie2 := sessionCookie(t, app2, user2.ID, fixtureCharA, "Fixture Ceo")
	app2.esi.StoreCharacterName(fixtureMember, "Member One")

	if code, _ := getPage(t, app2, cookie2, "/alliance/?alliance=99000001"); code != http.StatusOK {
		t.Fatalf("alliance enqueue page: status %d", code)
	}
	drained, limited := app2.refreshAllianceRecords(ctx, &fetchBudget{left: 120})
	if limited || drained != 1 {
		t.Fatalf("alliance drain: drained=%d limited=%v, want 1/false", drained, limited)
	}
	rec, err = q2.GetAllianceRecord(ctx, 99000001)
	if err != nil || rec.State != orgStateReady {
		t.Fatalf("alliance record after drain: %+v err=%v, want ready", rec, err)
	}

	code, body = getPage(t, app2, cookie2, "/alliance/?alliance=99000001")
	if code != http.StatusOK {
		t.Fatalf("alliance page: status %d", code)
	}
	mustContain(t, "/alliance/", body,
		"Fixture Alliance", "[FALL]", "2010-01-02",
		`<a href="/pilot/?character=93300001">Member One</a>`,         // creator
		`<a href="/corporation/?corporation=98000001">Alpha Corp</a>`, // creator + member corporation
		`<a href="/corporation/?corporation=98000002">Beta Corp</a>`,  // executor + member corporation
		"Corporation #98000003", // member whose name never resolved
	)
	// Member corporations total.
	if !strings.Contains(body, "<th>Member corporations</th><td>3</td>") {
		t.Error("alliance page does not show 3 member corporations")
	}

	// The live-region fragment re-renders the same stored record.
	code, body = getPage(t, app2, cookie2, "/alliance/fragment?alliance=99000001")
	if code != http.StatusOK {
		t.Fatalf("alliance fragment: status %d", code)
	}
	mustContain(t, "/alliance/fragment", body, `data-poll-state="ready"`, "Fixture Alliance")

	// Unknown alliance id: drain settles 'missing', page says so.
	if err := q2.UpsertAllianceWant(ctx, 99000999); err != nil {
		t.Fatalf("enqueue missing alliance: %v", err)
	}
	drained, _ = app2.refreshAllianceRecords(ctx, &fetchBudget{left: 120})
	if drained != 1 {
		t.Fatalf("missing alliance drain: drained=%d, want 1", drained)
	}
	rec, err = q2.GetAllianceRecord(ctx, 99000999)
	if err != nil || rec.State != orgStateMissing {
		t.Fatalf("missing alliance record: %+v err=%v, want missing", rec, err)
	}
	code, body = getPage(t, app2, cookie2, "/alliance/?alliance=99000999")
	if code != http.StatusOK {
		t.Fatalf("alliance missing page: status %d", code)
	}
	mustContain(t, "/alliance/ (missing)", body, "EVE has no public record for that alliance")
}

// ---------------------------------------------------------------------------
// Swept surfaces: the pilot page and the contacts page link
// organization names to the new pages.
// ---------------------------------------------------------------------------

func TestPilotPageLinksToOrgPages(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	payload := pilotPayload{
		Profile: esi.Character{
			Name: "Stranger One", CorporationID: 98000001, AllianceID: 99000001,
			Birthday: "2011-03-04T05:06:07Z", SecurityStatus: 1.5,
		},
		Corp:     esi.Corporation{Name: "Current Corp", Ticker: "CURR"},
		Alliance: esi.Alliance{Name: "Fixture Alliance", Ticker: "FALL"},
		History: []pilotHistoryRow{
			{CorpID: 98000002, CorpName: "Old Corp", Start: "2015-01-01T00:00:00Z"},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal pilot payload: %v", err)
	}
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: fixtureMember, Payload: string(raw), State: pilotStateReady, FetchedAt: "2999-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed pilot record: %v", err)
	}

	code, body := getPage(t, app, cookie, "/pilot/?character=93300001")
	if code != http.StatusOK {
		t.Fatalf("pilot page: status %d", code)
	}
	mustContain(t, "/pilot/ (org links)", body,
		`<a href="/corporation/?corporation=98000001">Current Corp [CURR]</a>`,
		`<a href="/alliance/?alliance=99000001">Fixture Alliance [FALL]</a>`,
		`<a href="/corporation/?corporation=98000002">Old Corp</a>`,
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("pilot renders made %d outbound calls, want 0", got)
	}
}

func TestContactsOrgLinks(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	seedSnapshot(t, q, fixtureCharA, esi.SnapContacts, esi.Contacts{
		{ContactID: 98000001, ContactType: "corporation", Standing: 5.0},
		{ContactID: 99000001, ContactType: "alliance", Standing: -5.0},
	})
	app.esi.StoreCorpName(98000001, "Fixture Corp")
	app.esi.StoreAllianceName(99000001, "Fixture Alliance")

	code, body := getPage(t, app, cookie, "/contacts/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("contacts page: status %d", code)
	}
	mustContain(t, "/contacts/ (org links)", body,
		`<a href="/corporation/?corporation=98000001">Fixture Corp</a>`,
		`<a href="/alliance/?alliance=99000001">Fixture Alliance</a>`,
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("contacts renders made %d outbound calls, want 0", got)
	}
}

// TestMigration026Reopen proves the guarded corporation_records /
// alliance_records migration is idempotent: a database created
// before this build reopens cleanly and the queues work.
func TestMigration026Reopen(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	for i := 0; i < 2; i++ {
		conn, pool, err := openDB(context.Background(), dsn)
		if err != nil {
			t.Fatalf("openDB (pass %d): %v", i, err)
		}
		q := db.New(conn)
		if err := q.UpsertCorporationWant(ctx, 98000001); err != nil {
			t.Fatalf("corporation want (pass %d): %v", i, err)
		}
		if err := q.UpsertAllianceWant(ctx, 99000001); err != nil {
			t.Fatalf("alliance want (pass %d): %v", i, err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close (pass %d): %v", i, err)
		}
		pool.Close()
	}
}

// TestOrgLabelLiveRegions (v0.3.35.001): names the organization
// pages reference but have not cached yet — a corporation's CEO,
// an alliance's creator and member corporations, a home station —
// render a live "Loading name…" region that polls its label
// fragment and swaps the linked name in place, instead of
// sitting as a bare "#<id>" until a manual reload.
func TestOrgLabelLiveRegions(t *testing.T) {
	ctx := context.Background()
	stub := &orgStub{
		corps: map[int64]string{
			98000001: `{"name":"Boars on Parade","ticker":"BOAR.","member_count":36,"ceo_id":2117407084,` +
				`"alliance_id":99000001,"home_station_id":60003760,"tax_rate":0.1,` +
				`"date_founded":"2014-04-14T00:00:00Z"}`,
		},
		alliances: map[int64]string{
			99000001: `{"name":"The Tuskers Co.","ticker":"TUSK.","creator_id":1,"creator_corporation_id":1,"date_founded":"2010-01-02T00:00:00Z","executor_corporation_id":1}`,
		},
	}
	app, conn, q := buildCorpTestApp(t, stub)
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita IV - Moon 4 - Caldari Navy Assembly Plant', 30000142)`); err != nil {
		t.Fatalf("seed sde station: %v", err)
	}

	if code, _ := getPage(t, app, cookie, "/corporation/?corporation=98000001"); code != http.StatusOK {
		t.Fatalf("corporation enqueue page: status %d", code)
	}
	if drained, limited := app.refreshCorporationRecords(ctx, &fetchBudget{left: 120}); limited || drained != 1 {
		t.Fatalf("corporation drain: drained=%d limited=%v, want 1/false", drained, limited)
	}

	// The CEO is nobody the app knows yet: the cell must offer
	// the live region, not a bare id. The home station resolves
	// from the SDE on the spot, so it renders plain.
	code, body := getPage(t, app, cookie, "/corporation/?corporation=98000001")
	if code != http.StatusOK {
		t.Fatalf("corporation page: status %d", code)
	}
	mustContain(t, "/corporation/ ceo pending", body,
		`data-poll-url="/labels/character-fragment?id=2117407084"`,
		"Loading name for Character #2117407084",
		"Jita IV - Moon 4 - Caldari Navy Assembly Plant",
	)
	if strings.Contains(body, `data-poll-url="/labels/place-fragment?id=60003760"`) {
		t.Error("home station rendered a poll region even though its name is known")
	}

	// The character fragment still waits… then swaps the linked
	// name in once a local tier learns it.
	code, body = getPage(t, app, cookie, "/labels/character-fragment?id=2117407084")
	if code != http.StatusOK || !strings.Contains(body, `data-poll-state="pending"`) {
		t.Fatalf("character fragment before warm: %d %q", code, body)
	}
	app.esi.StoreCharacterName(2117407084, "Boars Ceo")
	code, body = getPage(t, app, cookie, "/labels/character-fragment?id=2117407084")
	if code != http.StatusOK {
		t.Fatalf("character fragment after warm: status %d", code)
	}
	mustContain(t, "character fragment after warm", body,
		`data-poll-state="ready"`,
		`<a href="/pilot/?character=2117407084">Boars Ceo</a>`,
	)
	code, body = getPage(t, app, cookie, "/corporation/?corporation=98000001")
	if code != http.StatusOK || !strings.Contains(body, `<a href="/pilot/?character=2117407084">Boars Ceo</a>`) {
		t.Fatalf("corporation page after warm: %d, CEO not linked", code)
	}

	// Corporation fragment: waits, then serves the linked name
	// once the record lands.
	code, body = getPage(t, app, cookie, "/labels/corporation-fragment?id=98000077")
	if code != http.StatusOK || !strings.Contains(body, "Loading name for Corporation #98000077") {
		t.Fatalf("corporation fragment before record: %d %q", code, body)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000077,
		Payload:       `{"corp":{"name":"Late Corp","ticker":"LATE","member_count":3,"ceo_id":1,"tax_rate":0.1},"alliance":{}}`,
		State:         orgStateReady, FetchedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed corporation record: %v", err)
	}
	code, body = getPage(t, app, cookie, "/labels/corporation-fragment?id=98000077")
	if code != http.StatusOK {
		t.Fatalf("corporation fragment after record: status %d", code)
	}
	mustContain(t, "corporation fragment after record", body,
		`data-poll-state="ready"`,
		`<a href="/corporation/?corporation=98000077">Late Corp</a>`,
	)

	// Place fragment answers a known station immediately, linked.
	code, body = getPage(t, app, cookie, "/labels/place-fragment?id=60003760")
	if code != http.StatusOK {
		t.Fatalf("place fragment: status %d", code)
	}
	mustContain(t, "place fragment", body,
		`data-poll-state="ready"`,
		`<a href="/station/?station=60003760">Jita IV - Moon 4 - Caldari Navy Assembly Plant</a>`,
	)

	// A settled miss stops the polling: the alliance that does
	// not exist renders its plain fallback as ready.
	if err := q.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: 99000099, Payload: "", State: orgStateMissing, FetchedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed missing alliance: %v", err)
	}
	code, body = getPage(t, app, cookie, "/labels/alliance-fragment?id=99000099")
	if code != http.StatusOK || !strings.Contains(body, `data-poll-state="ready">Alliance #99000099<`) {
		t.Fatalf("alliance fragment (missing): %d %q", code, body)
	}
}
