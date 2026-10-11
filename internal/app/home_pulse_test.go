package app

// Corp pulse tests: the worker diff records departures and
// structure arrivals/removals without ESI traffic, and the
// widget gates every section on in-game roles.

import (
	"context"
	"net/http"
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
	if n, _ := app.refreshPulseEvents(ctx, []db.Character{alpha}); n != 0 {
		t.Fatalf("adopt cycle recorded %d, want 0", n)
	}
	// Beta leaves, S101 is removed, S102 anchors.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMembers, esi.CorpMembers([]int64{fixtureCharA}))
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpStructures, esi.CorpStructures{{StructureID: 102, Name: "Fortizar Two"}})
	if n, _ := app.refreshPulseEvents(ctx, []db.Character{alpha}); n != 3 {
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
	if n, _ := app.refreshPulseEvents(ctx, []db.Character{alpha}); n != 0 {
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
