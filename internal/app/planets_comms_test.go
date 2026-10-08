package app

// Hermetic tests for Phase 2 (planetary industry + mail, calendar,
// contacts). Same two layers as the earlier clusters:
//
//   - Render tests: seeded snapshots drive the real router with a
//     counting stub transport, proving the new pages render from
//     local rows, the PI scope-refusal state explains itself, and
//     no handler makes an outbound call.
//   - Worker tests: a path-routing stub transport stands in for
//     ESI, proving the PI pass stores colonies + layouts, a 403 is
//     recorded with backoff (and a re-link bypasses it), and the
//     comms pass warms headers, bodies and event details, then
//     settles to zero calls.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	fixturePlanetA = int64(40100001)
	fixturePlanetB = int64(40100002)
)

// TestPhase2Scopes proves the scope wiring: the planetary scope is
// requested, the fitting write scope is requested (Save to EVE),
// the mail write scopes are requested (Issues 26/27: mark read,
// compose), and the remaining write scopes the app deliberately
// excluded stay excluded.
func TestPhase2Scopes(t *testing.T) {
	foundPlanets := false
	foundFittingsWrite := false
	foundOrganizeMail := false
	foundSendMail := false
	for _, s := range eveScopes {
		if s == "esi-planets.manage_planets.v1" {
			foundPlanets = true
		}
		if s == "esi-fittings.write_fittings.v1" {
			foundFittingsWrite = true
		}
		if s == "esi-mail.organize_mail.v1" {
			foundOrganizeMail = true
		}
		if s == "esi-mail.send_mail.v1" {
			foundSendMail = true
		}
		for _, banned := range []string{"write_contacts", "write_fleet", "open_window", "write_waypoint"} {
			if strings.Contains(s, banned) {
				t.Errorf("eveScopes must stay read-only for contacts/fleet, found %q", s)
			}
		}
	}
	if !foundPlanets {
		t.Error("eveScopes is missing esi-planets.manage_planets.v1 (colony GETs 403 without it)")
	}
	if !foundFittingsWrite {
		t.Error("eveScopes is missing esi-fittings.write_fittings.v1 (Save to EVE needs it)")
	}
	if !foundOrganizeMail {
		t.Error("eveScopes is missing esi-mail.organize_mail.v1 (mark-as-read needs it)")
	}
	if !foundSendMail {
		t.Error("eveScopes is missing esi-mail.send_mail.v1 (compose needs it)")
	}
}

// seedPIFixtures stores one character's colonies + one layout and
// returns the fixture character row.
func seedPIFixtures(t *testing.T, app *Application, q *db.Queries, userID int64, now time.Time) {
	t.Helper()

	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18), (990201, 'Fixture Extractor', 0), (990202, 'Fixture Factory', 0), (990203, 'Fixture Storage', 0)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
	} {
		if _, err := app.db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, esi.Colonies{
		{PlanetID: fixturePlanetA, SolarSystemID: 30000142, PlanetType: "temperate", NumPins: 4, UpgradeLevel: 3, LastUpdate: rfc(now.Add(-time.Hour))},
		{PlanetID: fixturePlanetB, SolarSystemID: 30000142, PlanetType: "barren", NumPins: 2, UpgradeLevel: 1, LastUpdate: rfc(now.Add(-2 * time.Hour))},
	})
	// Planet A: one expired extractor, one factory, two storages.
	// Planet B: no layout yet (warming state).
	seedSnapshot(t, q, fixtureCharA, esi.PlanetLayoutKind(fixturePlanetA), esi.PlanetLayout{
		Pins: []esi.PlanetPin{
			{PinID: 1, TypeID: 990201, ExpiryTime: rfc(now.Add(-time.Hour)),
				ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 34, CycleTime: 1800, QtyPerCycle: 1200, Heads: []esi.PlanetExtractorHead{{HeadID: 0}, {HeadID: 1}}}},
			{PinID: 2, TypeID: 990202, SchematicID: 77, FactoryDetails: &esi.PlanetFactory{SchematicID: 77}},
			{PinID: 3, TypeID: 990203},
			{PinID: 4, TypeID: 990203},
		},
	})

	app.esi.StorePlaceName(fixturePlanetA, "Jita I")
	app.esi.StorePlaceName(fixturePlanetB, "Jita II")
	app.esi.StoreSchematic(77, esi.Schematic{SchematicName: "Fixture Widgets", CycleTime: 3600})
}

func TestPlanetsPagesRenderFromSnapshots(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedPIFixtures(t, app, q, user.ID, now)

	seedSnapshot(t, q, fixtureCharA, esi.SnapMailLabels, esi.MailLabels{TotalUnreadCount: 3})

	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `["fleet","attention","pi"]`,
		ID:         user.ID,
	}); err != nil {
		t.Fatalf("set home layout: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/planets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("planets page: status %d", code)
	}
	mustContain(t, "/planets/", body,
		"Jita I", "Jita II", "Jita", "Temperate", "command center upgrades 3",
		"Tritanium", "1,200", "30m", "Expired",
		"Fixture Widgets", "Fixture Storage ×2",
		"Colony layout still warming up", // planet B
		"Stored commodity amounts are not shown",
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("planets render made %d outbound calls, want 0", got)
	}

	// The character sheet's PI section is cache-only too, but the
	// legacy sheet fetches its other sections (online, clones,
	// implants) live when their snapshots are absent — pre-existing
	// behavior, deliberately out of scope for this phase.
	code, body = getPage(t, app, cookie, "/character/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("character page: status %d", code)
	}
	mustContain(t, "/character/ (PI summary)", body,
		"Planetary industry", "2 colonies", "1 extractor expired", "view colonies",
	)

	before := transport.calls.Load()
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	mustContain(t, "/ (PI widget + attention + fleet)", body,
		"Planetary industry",
		"2 colonies across your characters", "1 extractor expired",
		"extractor on Jita I has expired",
		"3 unread",
	)
	if got := transport.calls.Load(); got != before {
		t.Fatalf("home render made %d outbound calls, want 0", got-before)
	}
}

// phase2StubTransport serves the Phase 2 endpoints for the worker
// tests. Planets answer 403 while planetsForbidden is set.
type phase2StubTransport struct {
	calls            atomic.Int64
	planetsForbidden atomic.Bool
}

func (s *phase2StubTransport) respond(status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (s *phase2StubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	base := fmt.Sprintf("/characters/%d", fixtureCharA)
	path := req.URL.Path
	switch {
	case path == base+"/planets/" && s.planetsForbidden.Load():
		return s.respond(http.StatusForbidden, `{"error":"forbidden"}`)
	case path == base+"/planets/":
		return s.respond(http.StatusOK, `[{"planet_id":40100001,"owner_id":90000001,"solar_system_id":30000142,"planet_type":"temperate","num_pins":2,"upgrade_level":3,"last_update":"2026-10-01T00:00:00Z"}]`)
	case path == base+"/planets/40100001/":
		return s.respond(http.StatusOK, `{"pins":[{"pin_id":1,"type_id":990201,"expiry_time":"2999-01-01T00:00:00Z","extractor_details":{"product_type_id":34,"cycle_time":1800,"qty_per_cycle":100,"heads":[{"head_id":0,"latitude":0,"longitude":0}]}},{"pin_id":2,"type_id":990202,"schematic_id":77,"factory_details":{"schematic_id":77}}],"links":[],"routes":[]}`)
	case path == base+"/mail/":
		return s.respond(http.StatusOK, `[{"mail_id":501,"from":93300001,"subject":"Unread one","timestamp":"2026-10-01T00:00:00Z","is_read":false,"labels":[2],"recipients":[{"recipient_id":90000001,"recipient_type":"character"}]},{"mail_id":502,"from":93300001,"subject":"Read one","timestamp":"2026-09-30T00:00:00Z","is_read":true,"labels":[],"recipients":[{"recipient_id":90000001,"recipient_type":"character"}]}]`)
	case path == base+"/mail/labels/":
		return s.respond(http.StatusOK, `{"labels":[{"label_id":2,"name":"Deals","color":"#ffffff","unread_count":1}],"total_unread_count":1}`)
	case path == base+"/mail/lists/":
		return s.respond(http.StatusOK, `[{"mailing_list_id":700,"name":"Fixture List"}]`)
	case path == base+"/mail/501/":
		return s.respond(http.StatusOK, `{"from":93300001,"subject":"Unread one","body":"<p>Body one</p>","timestamp":"2026-10-01T00:00:00Z","read":false,"labels":[2],"recipients":[{"recipient_id":90000001,"recipient_type":"character"}]}`)
	case path == base+"/mail/502/":
		return s.respond(http.StatusOK, `{"from":93300001,"subject":"Read one","body":"<p>Body two</p>","timestamp":"2026-09-30T00:00:00Z","read":true,"labels":[],"recipients":[{"recipient_id":90000001,"recipient_type":"character"}]}`)
	case path == base+"/calendar/":
		return s.respond(http.StatusOK, `[{"event_id":88,"event_date":"2999-01-01T00:00:00Z","title":"Fixture Op","importance":1,"event_response":"accepted"}]`)
	case path == base+"/calendar/88/":
		return s.respond(http.StatusOK, `{"event_id":88,"date":"2999-01-01T00:00:00Z","duration":90,"importance":1,"owner_id":98000001,"owner_name":"Fixture Corp","owner_type":"corporation","response":"accepted","title":"Fixture Op","text":"Bring snacks."}`)
	case path == base+"/calendar/88/attendees/":
		return s.respond(http.StatusOK, `[{"character_id":93300001,"event_response":"accepted"}]`)
	case path == base+"/contacts/":
		return s.respond(http.StatusOK, `[{"contact_id":93300001,"contact_type":"character","standing":10,"is_blocked":false,"is_watched":true,"label_ids":[]}]`)
	default:
		return s.respond(http.StatusNotFound, `{"error":"unexpected path `+path+`"}`)
	}
}

// TestPlanetsWorkerAndScopeRefusal proves the PI worker contract:
// a 403 is recorded (no snapshot written) and backs off; the
// Characters page flags it; after a re-link grants the scope the
// very next attempt fetches, and a second cycle goes quiet.
func TestPlanetsWorkerAndScopeRefusal(t *testing.T) {
	transport := &phase2StubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	allowance := &fetchBudget{left: 120}

	// Scope not granted: 403, recorded, nothing stored.
	transport.planetsForbidden.Store(true)
	if _, limited := app.refreshPlanetarySnapshots(ctx, ch, allowance); limited {
		t.Fatal("403 must not report the error limit")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("forbidden pass made %d calls, want 1", got)
	}
	if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapPlanets}); !errorIsNoRows(err) {
		t.Fatalf("planets snapshot after 403: err=%v, want no row", err)
	}
	state, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: fixtureCharA, Kind: esi.SnapPlanets})
	if err != nil {
		t.Fatalf("planets fetch state: %v", err)
	}
	if state.State != fetchStateError || !strings.HasPrefix(state.Detail, piScopeDetail) {
		t.Fatalf("planets fetch state: got %q/%q, want error/%s…", state.State, state.Detail, piScopeDetail)
	}
	if !app.piNotEnabled(ctx, ch) {
		t.Fatal("piNotEnabled: got false after a recorded colonies refusal without the scope")
	}

	// Inside the backoff: no further calls.
	app.refreshPlanetarySnapshots(ctx, ch, allowance)
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("backoff pass made %d more calls, want 0", got-1)
	}

	// The Planets page and the Characters page both say so.
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/planets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("planets page: status %d", code)
	}
	mustContain(t, "/planets/ (refused)", body, "Planetary industry is not enabled for this character yet", "re-link")
	code, body = getPage(t, app, cookie, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters page: status %d", code)
	}
	mustContain(t, "/characters/ (PI flag)", body, "Planetary industry not enabled", "re-link to enable")

	// Re-link: the scope lands on the character row, the next
	// attempt bypasses the backoff and fetches.
	transport.planetsForbidden.Store(false)
	if _, err := q.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID:  fixtureCharA,
		UserID:       user.ID,
		Name:         "Fixture Ceo",
		AccessToken:  "fixture",
		RefreshToken: "fixture",
		TokenExpiry:  mustNullTime("2999-01-01T00:00:00Z"),
		Scopes:       "esi-planets.manage_planets.v1 esi-mail.read_mail.v1",
		LinkState:    "ok",
	}); err != nil {
		t.Fatalf("re-link upsert: %v", err)
	}
	ch, err = q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("re-read character: %v", err)
	}
	if app.piNotEnabled(ctx, ch) {
		t.Fatal("piNotEnabled: got true after the scope landed")
	}
	refreshed, limited := app.refreshPlanetarySnapshots(ctx, ch, allowance)
	if limited || refreshed != 2 {
		t.Fatalf("post-relink pass: refreshed=%d limited=%v, want 2/false (colonies + layout)", refreshed, limited)
	}
	if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapPlanets}); err != nil {
		t.Fatalf("planets snapshot after re-link: %v", err)
	}
	if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.PlanetLayoutKind(fixturePlanetA)}); err != nil {
		t.Fatalf("planet layout snapshot after re-link: %v", err)
	}

	// Fresh snapshots: the next pass is silent.
	before := transport.calls.Load()
	app.refreshPlanetarySnapshots(ctx, ch, allowance)
	if got := transport.calls.Load(); got != before {
		t.Fatalf("fresh pass made %d calls, want 0", got-before)
	}
}

func errorIsNoRows(err error) bool {
	return err != nil && err.Error() == sql.ErrNoRows.Error()
}

// TestCommsWorkerWarm proves the mail/calendar/contacts worker
// contract: list kinds stored, bodies and event details warmed
// behind them, second pass silent.
func TestCommsWorkerWarm(t *testing.T) {
	transport := &phase2StubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	allowance := &fetchBudget{left: 120}

	refreshed, limited := app.refreshCommsSnapshots(ctx, ch, allowance)
	if limited {
		t.Fatal("comms pass hit the error limit against the stub")
	}
	// 5 list kinds + 2 bodies + 1 event detail + 1 attendees = 9.
	if refreshed != 9 {
		t.Fatalf("comms pass: refreshed=%d, want 9", refreshed)
	}
	for _, probe := range []struct {
		name string
		get  func() error
	}{
		{"mail headers", func() error {
			_, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapMail})
			return err
		}},
		{"mail labels", func() error {
			_, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapMailLabels})
			return err
		}},
		{"mail body 501", func() error {
			_, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.MailBodyKind(501)})
			return err
		}},
		{"mail body 502", func() error {
			_, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.MailBodyKind(502)})
			return err
		}},
		{"calendar event 88", func() error {
			_, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.CalendarEventKind(88)})
			return err
		}},
		{"calendar attendees 88", func() error {
			_, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.CalendarAttendeesKind(88)})
			return err
		}},
		{"contacts", func() error {
			_, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapContacts})
			return err
		}},
	} {
		if err := probe.get(); err != nil {
			t.Errorf("%s snapshot: %v", probe.name, err)
		}
	}

	before := transport.calls.Load()
	refreshed, _ = app.refreshCommsSnapshots(ctx, ch, allowance)
	if refreshed != 0 {
		t.Fatalf("second comms pass refreshed %d, want 0", refreshed)
	}
	if got := transport.calls.Load(); got != before {
		t.Fatalf("second comms pass made %d calls, want 0", got-before)
	}
}

// TestMailCalendarContactsRender proves the comms pages render
// from snapshots, the mail body is sanitized on the way out, and
// no handler makes an outbound call.
func TestMailCalendarContactsRender(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	app.esi.StoreCharacterName(fixtureMember, "Member One")

	seedSnapshot(t, q, fixtureCharA, esi.SnapMail, esi.MailHeaders{
		{MailID: 501, From: fixtureMember, Subject: "Fixture secrets", Timestamp: rfc(now.Add(-time.Hour)), IsRead: false, Labels: []int64{2}},
		{MailID: 502, From: fixtureMember, Subject: "Old news", Timestamp: rfc(now.Add(-2 * time.Hour)), IsRead: true},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMailLabels, esi.MailLabels{
		Labels:           []esi.MailLabel{{LabelID: 2, Name: "Deals", UnreadCount: 1}},
		TotalUnreadCount: 1,
	})
	seedSnapshot(t, q, fixtureCharA, esi.MailBodyKind(501), esi.Mail{
		From: fixtureMember, Subject: "Fixture secrets", Timestamp: rfc(now.Add(-time.Hour)),
		Recipients: []esi.MailRecipient{{RecipientID: fixtureCharA, RecipientType: "character"}},
		Body: `<p>Hello <b>capsuleer</b>,</p><script>alert('pwn')</script>` +
			`<img src="https://evil.example/track.png" onerror="alert(1)">` +
			`<a href="javascript:alert(1)">bad link</a> ` +
			`<a href="https://example.com/fit">good link</a><br>` +
			`Tom &amp; Jerry approved this.`,
	})

	seedSnapshot(t, q, fixtureCharA, esi.SnapCalendar, esi.CalendarEventSummaries{
		{EventID: 88, EventDate: rfc(now.Add(48 * time.Hour)), Title: "Fixture Op", Importance: 1, EventResponse: "accepted"},
		{EventID: 89, EventDate: rfc(now.Add(72 * time.Hour)), Title: "Later Thing", Importance: 0, EventResponse: "not_responded"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.CalendarEventKind(88), esi.CalendarEvent{
		EventID: 88, Date: rfc(now.Add(48 * time.Hour)), Duration: 90,
		OwnerName: "Fixture Corp", Title: "Fixture Op", Text: "Bring snacks.",
	})
	seedSnapshot(t, q, fixtureCharA, esi.CalendarAttendeesKind(88), esi.CalendarAttendees{
		{CharacterID: fixtureMember, EventResponse: "accepted"},
	})

	seedSnapshot(t, q, fixtureCharA, esi.SnapContacts, esi.Contacts{
		{ContactID: fixtureCorpA, ContactType: "corporation", Standing: -5.0},
		{ContactID: fixtureMember, ContactType: "character", Standing: 10.0, IsWatched: true},
		{ContactID: 500001, ContactType: "faction", Standing: 5.0},
	})
	if err := q.UpsertGlobalSnapshot(ctx, db.UpsertGlobalSnapshotParams{
		Kind:        esi.GlobalFactions,
		Payload:     `[{"faction_id":500001,"name":"Caldari State","corporation_id":1000006}]`,
		FetchedAt:   mustTime("2026-01-01T00:00:00Z"),
		CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed factions: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/mail/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("mail page: status %d", code)
	}
	mustContain(t, "/mail/", body, "Fixture secrets", "Old news", "Member One", "1 unread", "Deals")

	code, body = getPage(t, app, cookie, "/mail/?character=90000001&label=2")
	if code != http.StatusOK {
		t.Fatalf("mail page (label): status %d", code)
	}
	mustContain(t, "/mail/?label=2", body, "Fixture secrets")
	if strings.Contains(body, "Old news") {
		t.Error("label-filtered mail list leaked an unlabeled mail")
	}

	code, body = getPage(t, app, cookie, "/mail/?character=90000001&mail=501")
	if code != http.StatusOK {
		t.Fatalf("mail body page: status %d", code)
	}
	mustContain(t, "/mail/ body", body,
		"Fixture secrets", "Member One",
		"<b>capsuleer</b>", "https://example.com/fit", "good link",
		"Tom &amp; Jerry approved this.",
	)
	// (The page chrome legitimately carries <script> and portrait
	// <img> tags, so the leak check targets the attack payloads
	// themselves; the sanitizer's tag-level contract is pinned in
	// TestSanitizeMailHTML.)
	for _, hostile := range []string{"alert(", "onerror", "javascript:", "evil.example"} {
		if strings.Contains(body, hostile) {
			t.Errorf("mail body leaked hostile content %q", hostile)
		}
	}

	code, body = getPage(t, app, cookie, "/calendar/?character=90000001&event=88")
	if code != http.StatusOK {
		t.Fatalf("calendar page: status %d", code)
	}
	mustContain(t, "/calendar/", body,
		"Fixture Op", "Later Thing", "Bring snacks.", "1h 30m", "Fixture Corp", "Member One", "accepted",
	)

	code, body = getPage(t, app, cookie, "/contacts/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("contacts page: status %d", code)
	}
	mustContain(t, "/contacts/", body, "Member One", "+10.0", "-5.0", "Caldari State", "Watched")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestSanitizeMailHTML pins the sanitizer's contract directly:
// allowlisted formatting survives, active content and external
// references don't, and text is never reinterpreted as markup.
func TestSanitizeMailHTML(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		notWant []string
	}{
		{
			name: "formatting survives",
			in:   `<p>Hi <b>there</b>, <i>capsuleer</i>.</p>`,
			want: `<p>Hi <b>there</b>, <i>capsuleer</i>.</p>`,
		},
		{
			name:    "script removed with contents",
			in:      `<script>alert(1)</script>after`,
			want:    `after`,
			notWant: []string{"alert", "script"},
		},
		{
			name:    "unterminated script swallows the rest",
			in:      `kept<script>alert(1)`,
			want:    `kept`,
			notWant: []string{"alert"},
		},
		{
			name:    "image and handler removed",
			in:      `<img src="https://evil.example/x.png" onerror="alert(1)">text`,
			want:    `text`,
			notWant: []string{"img", "onerror", "evil.example"},
		},
		{
			name: "https link kept and fenced",
			in:   `<a href="https://example.com/fit">fit</a>`,
			want: `<a href="https://example.com/fit" rel="nofollow noopener noreferrer" target="_blank">fit</a>`,
		},
		{
			name:    "showinfo link drops to text",
			in:      `<a href="showinfo:34">Tritanium</a>`,
			want:    `Tritanium`,
			notWant: []string{"showinfo", "href"},
		},
		{
			name:    "javascript link drops to text",
			in:      `<a href="javascript:alert(1)">click</a>`,
			want:    `click`,
			notWant: []string{"javascript", "alert"},
		},
		{
			name: "attributes stripped from allowed tags",
			in:   `<div class="x" onclick="evil()">t</div>`,
			want: `<div>t</div>`,
		},
		{
			name: "text escaped, entities normalized",
			in:   `5 < 6 &amp; 7 &gt; 2`,
			want: `5 &lt; 6 &amp; 7 &gt; 2`,
		},
		{
			name: "font loses its attributes",
			in:   `<font color="#ff0000" size="14">red</font>`,
			want: `<font>red</font>`,
		},
		{
			name:    "iframe removed with contents",
			in:      `<iframe src="https://evil.example"></iframe>ok`,
			want:    `ok`,
			notWant: []string{"iframe", "evil.example"},
		},
		{
			name: "style removed with contents",
			in:   `<style>body{display:none}</style>seen`,
			want: `seen`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(sanitizeMailHTML(tc.in))
			if got != tc.want {
				t.Errorf("sanitizeMailHTML(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("sanitizeMailHTML(%q) = %q, must not contain %q", tc.in, got, nw)
				}
			}
		})
	}
}
