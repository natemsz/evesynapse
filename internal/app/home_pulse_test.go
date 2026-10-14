package app

// Corp pulse tests: the worker diff records departures and
// structure arrivals/removals without ESI traffic, and the
// widget gates every section on in-game roles.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const pulseTestCorp = int64(98000100)

// seedPulseCorp stores one corporation's snapshots behind
// Alpha: profile, roles, members, tracking, wallets, one
// journal division, and structures.
func seedPulseCorp(t *testing.T, q *db.Queries, roles []string, members []int64, tracking esi.CorpMemberTrackings, journal esi.CorpJournal, structures esi.CorpStructures) {
	t.Helper()
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{Name: "Fixture Alpha", CorporationID: pulseTestCorp})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpRoles, esi.CharacterRoles{Roles: roles})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMembers, esi.CorpMembers(members))
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMemberTracking, tracking)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpWallets, esi.CorpWallets{{Division: 1, Balance: 1000000}})
	seedSnapshot(t, q, fixtureCharA, esi.CorpJournalKind(1), journal)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpStructures, structures)
}

func TestPulseDiffRecordsChanges(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	alpha := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedPulseCorp(t, q,
		[]string{"Director"},
		[]int64{fixtureCharA, 93300005},
		esi.CorpMemberTrackings{{CharacterID: 93300005, StartDate: rfc(now.Add(-48 * time.Hour))}},
		esi.CorpJournal{{ID: 1, Date: rfc(now.Add(-time.Hour)), RefType: "player_trading", Amount: 5000000, Balance: 6000000}},
		esi.CorpStructures{{StructureID: 101, Name: "Keepstar One"}})

	// First cycle adopts the current sets silently: nothing
	// present reports as newly arrived or departed.
	if n, _ := app.refreshPulseEvents(ctx, []db.Character{alpha}, &fetchBudget{}); n != 0 {
		t.Fatalf("adopt cycle recorded %d, want 0", n)
	}
	// Beta leaves, S101 is removed, S102 anchors.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMembers, esi.CorpMembers([]int64{fixtureCharA}))
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpStructures, esi.CorpStructures{{StructureID: 102, Name: "Fortizar Two"}})
	if n, _ := app.refreshPulseEvents(ctx, []db.Character{alpha}, &fetchBudget{}); n != 3 {
		t.Fatalf("change cycle recorded %d, want 3 (left, removed, added)", n)
	}
	events, err := app.queries.ListPulseEvents(ctx, db.ListPulseEventsParams{CorpID: pulseTestCorp, At: now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	seen := make(map[string][]string)
	for _, e := range events {
		seen[e.Kind] = append(seen[e.Kind], e.Name)
	}
	contains := func(kind, name string) bool {
		for _, n := range seen[kind] {
			if n == name {
				return true
			}
		}
		return false
	}
	if !contains(pulseMemberLeft, "Character #93300005") || !contains(pulseStructureGone, "Keepstar One") || !contains(pulseStructureAdded, "Fortizar Two") {
		t.Fatalf("events missing: %v", seen)
	}
	// Steady state records nothing further.
	if n, _ := app.refreshPulseEvents(ctx, []db.Character{alpha}, &fetchBudget{}); n != 0 {
		t.Fatalf("steady cycle recorded %d, want 0", n)
	}
}

func TestPulseWidgetGatesAndRenders(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedPulseCorp(t, q,
		[]string{"Director"},
		[]int64{fixtureCharA},
		esi.CorpMemberTrackings{{CharacterID: 93300006, StartDate: rfc(now.Add(-24 * time.Hour))}},
		esi.CorpJournal{{ID: 2, Date: rfc(now.Add(-2 * time.Hour)), RefType: "bounty_prizes", Amount: 9000000, Balance: 10000000, Description: "rats"}},
		esi.CorpStructures{{StructureID: 103, Name: "Astrahus Three", UnanchorsAt: rfc(now.Add(48 * time.Hour))}})
	setHomeLayout(t, q, user.ID, `[{"id":"pulse"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Corp pulse", "Character #93300006", "bounty_prizes", "Astrahus Three", "unanchoring")

	// Without a qualifying role the widget shows the gated note.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Contract_Taker"}})
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "needs a Director, Personnel Manager, Accountant, or Station Manager role")
}

// TestRoleSetsUseESINames: every in-game role a widget gates on
// is spelled the way ESI spells it (underscores, not spaces), by
// being one of the known corporation roles. "Junior Accountant"
// with a space matched nobody.
func TestRoleSetsUseESINames(t *testing.T) {
	for name, set := range map[string][]string{
		"pulse members":    pulseMemberRoles,
		"pulse wallet":     pulseWalletRoles,
		"pulse structures": pulseStructureRoles,
		"upkeep":           upkeepRoles,
	} {
		for _, role := range set {
			if !validEVECorpRole(role) {
				t.Errorf("%s gates on %q, which is not an ESI corporation role name", name, role)
			}
		}
	}
}

// TestPulseWalletShowsForJuniorAccountant: a Junior Accountant
// may read the corporation wallet journal in game, so the pulse
// shows them its movements.
func TestPulseWalletShowsForJuniorAccountant(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedPulseCorp(t, q,
		[]string{"Junior_Accountant"},
		[]int64{fixtureCharA},
		nil,
		esi.CorpJournal{{ID: 2, Date: rfc(now.Add(-2 * time.Hour)), RefType: "bounty_prizes", Amount: 9000000, Balance: 10000000}},
		nil)
	w := app.buildPulse(ctx, user.ID)
	if len(w.Corps) != 1 || len(w.Corps[0].Moves) != 1 {
		t.Fatalf("pulse for a Junior Accountant: %+v, want one corporation with its wallet movement", w.Corps)
	}
}

// TestPulseReadsFromTheCharacterHoldingTheRole: two characters in
// one corporation, and the one listed first holds no role and so
// has no corporation snapshots. The pulse, the worker's diff and
// the upkeep widget all read from the second, who is the
// Director. Reading through the first character found nothing.
func TestPulseReadsFromTheCharacterHoldingTheRole(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	alpha := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	beta := seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Beta")
	// Alpha: a plain member. A profile, a role list with nothing useful, no corporation data.
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{Name: "Fixture Alpha", CorporationID: pulseTestCorp})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Contract_Taker"}})
	// Beta: the Director, whose token fetched the corporation's data.
	seedSnapshot(t, q, fixtureCharB, esi.SnapProfile, esi.Character{Name: "Fixture Beta", CorporationID: pulseTestCorp})
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Director"}})
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpMembers, esi.CorpMembers([]int64{fixtureCharA, fixtureCharB, 93300005}))
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpMemberTracking, esi.CorpMemberTrackings{{CharacterID: 93300005, StartDate: rfc(now.Add(-24 * time.Hour))}})
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpWallets, esi.CorpWallets{{Division: 1, Balance: 1000000}})
	seedSnapshot(t, q, fixtureCharB, esi.CorpJournalKind(1), esi.CorpJournal{{ID: 2, Date: rfc(now.Add(-2 * time.Hour)), RefType: "bounty_prizes", Amount: 9000000, Balance: 10000000}})
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpStructures, esi.CorpStructures{{StructureID: 103, Name: "Astrahus Three", FuelExpires: rfc(now.Add(24 * time.Hour))}})

	w := app.buildPulse(ctx, user.ID)
	if len(w.Corps) != 1 {
		t.Fatalf("pulse shows %d corporations, want 1", len(w.Corps))
	}
	if c := w.Corps[0]; len(c.Joined) != 1 || len(c.Moves) != 1 {
		t.Fatalf("pulse read nothing through the Director: joined %d, moves %d", len(c.Joined), len(c.Moves))
	}
	up := app.buildUpkeep(ctx, user.ID)
	if len(up.Corps) != 1 || len(up.Corps[0].Items) != 1 {
		t.Fatalf("upkeep read nothing through the Director: %+v", up.Corps)
	}

	// The worker's diff reads the same way: first pass adopts,
	// then a departure in the Director's member list is recorded.
	chars := []db.Character{alpha, beta}
	if n, _ := app.refreshPulseEvents(ctx, chars, &fetchBudget{}); n != 0 {
		t.Fatalf("adopt cycle recorded %d events, want 0", n)
	}
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpMembers, esi.CorpMembers([]int64{fixtureCharA, fixtureCharB}))
	if n, _ := app.refreshPulseEvents(ctx, chars, &fetchBudget{}); n != 1 {
		t.Fatalf("a member left and the diff recorded %d events, want 1", n)
	}
}

// namesTransport answers the bulk names endpoint from a table
// and fails everything else, counting the names calls.
type namesTransport struct {
	names map[int64]string
	calls int
}

func (s *namesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(status int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/universe/names/") {
		return respond(http.StatusInternalServerError, `{"error":"must not be called"}`)
	}
	s.calls++
	var ids []int64
	if err := json.NewDecoder(req.Body).Decode(&ids); err != nil {
		return respond(http.StatusBadRequest, `{"error":"bad body"}`)
	}
	var out []universeName
	for _, id := range ids {
		if name, ok := s.names[id]; ok {
			out = append(out, universeName{ID: id, Name: name, Category: "character"})
		}
	}
	raw, _ := json.Marshal(out)
	return respond(http.StatusOK, string(raw))
}

// TestPulseNamesMembersThroughTheBulkResolver: a member list is
// IDs. The worker names members it has not seen before in one
// bulk call, keeps the name with the member, and does not ask
// again; a departure is then recorded, and shown, under the
// name rather than "Character #id".
func TestPulseNamesMembersThroughTheBulkResolver(t *testing.T) {
	transport := &namesTransport{names: map[int64]string{93300005: "Resolved Pilot"}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	alpha := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedPulseCorp(t, q,
		[]string{"Director"},
		[]int64{fixtureCharA, 93300005},
		esi.CorpMemberTrackings{{CharacterID: 93300005, StartDate: rfc(now.Add(-48 * time.Hour))}},
		nil, nil)
	chars := []db.Character{alpha}

	if n, _ := app.refreshPulseEvents(ctx, chars, &fetchBudget{left: 5}); n != 0 {
		t.Fatalf("adopt cycle recorded %d events, want 0", n)
	}
	if transport.calls != 1 {
		t.Fatalf("naming one unknown member took %d bulk calls, want 1", transport.calls)
	}
	// The linked character needed no lookup, and nothing is asked twice.
	if n, _ := app.refreshPulseEvents(ctx, chars, &fetchBudget{left: 5}); n != 0 || transport.calls != 1 {
		t.Fatalf("a quiet cycle recorded %d events and made %d bulk calls in all, want 0 and 1", n, transport.calls)
	}
	w := app.buildPulse(ctx, user.ID)
	if len(w.Corps) != 1 || len(w.Corps[0].Joined) != 1 || w.Corps[0].Joined[0].Name != "Resolved Pilot" {
		t.Fatalf("joined list %+v, want the member by name", w.Corps)
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMembers, esi.CorpMembers([]int64{fixtureCharA}))
	if n, _ := app.refreshPulseEvents(ctx, chars, &fetchBudget{left: 5}); n != 1 {
		t.Fatalf("a departure recorded %d events, want 1", n)
	}
	w = app.buildPulse(ctx, user.ID)
	if len(w.Corps) != 1 || len(w.Corps[0].Left) != 1 || w.Corps[0].Left[0].Text != "Resolved Pilot" {
		t.Fatalf("left list %+v, want the departed member by name", w.Corps)
	}
}

// TestPulseQuietCycleWritesNothing: with no change in the member
// list the diff leaves every seen row alone. The row versions
// (xmin moves on any update) are the same after the cycle as
// before; a member arriving writes exactly its own row.
func TestPulseQuietCycleWritesNothing(t *testing.T) {
	transport := &namesTransport{names: map[int64]string{93300005: "Resolved Pilot", 93300006: "Second Pilot"}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	alpha := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedPulseCorp(t, q, []string{"Director"}, []int64{fixtureCharA, 93300005}, nil, nil, nil)
	chars := []db.Character{alpha}
	versions := func() map[string]string {
		t.Helper()
		rows, err := app.db.QueryContext(ctx, `SELECT key, xmin::text FROM pulse_seen WHERE kind = 'member'`)
		if err != nil {
			t.Fatalf("read row versions: %v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var key, version string
			if err := rows.Scan(&key, &version); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[key] = version
		}
		return out
	}
	app.refreshPulseEvents(ctx, chars, &fetchBudget{left: 5})
	before := versions()
	if len(before) != 2 {
		t.Fatalf("seen set has %d members, want 2", len(before))
	}
	for cycle := 0; cycle < 3; cycle++ {
		app.refreshPulseEvents(ctx, chars, &fetchBudget{left: 5})
	}
	after := versions()
	for key, version := range before {
		if after[key] != version {
			t.Fatalf("member %s was rewritten on a quiet cycle", key)
		}
	}
	// A new member writes one row and leaves the others alone.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMembers, esi.CorpMembers([]int64{fixtureCharA, 93300005, 93300006}))
	app.refreshPulseEvents(ctx, chars, &fetchBudget{left: 5})
	grown := versions()
	if len(grown) != 3 {
		t.Fatalf("seen set has %d members after an arrival, want 3", len(grown))
	}
	for key, version := range before {
		if grown[key] != version {
			t.Fatalf("member %s was rewritten when another member arrived", key)
		}
	}
}
