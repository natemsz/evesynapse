package app

// Upkeep tests: fuel, unanchoring, and state timers render for
// Station Managers and Directors, and stay hidden without the
// role. Sov bills and office rentals have no synced endpoint
// and are not covered.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

func TestUpkeepRendersAndGates(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{Name: "Fixture Alpha", CorporationID: pulseTestCorp})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Station_Manager"}})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpStructures, esi.CorpStructures{
		{StructureID: 201, Name: "Low Fuel Fort", FuelExpires: rfc(now.Add(48 * time.Hour))},
		{StructureID: 202, Name: "Full Fuel Keep", FuelExpires: rfc(now.Add(60 * 24 * time.Hour))},
		{StructureID: 203, Name: "Leaving Astra", UnanchorsAt: rfc(now.Add(72 * time.Hour))},
	})
	setHomeLayout(t, q, user.ID, `[{"id":"upkeep"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Upkeep", "Low Fuel Fort", "fuel expires", "Leaving Astra", "unanchoring")
	if strings.Contains(body, "Full Fuel Keep") {
		t.Fatalf("healthy structure listed in upkeep")
	}

	// Without the role the deadlines stay hidden.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Contract_Taker"}})
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Station Manager or Director role needed")
	if strings.Contains(body, "Low Fuel Fort") {
		t.Fatalf("upkeep leaked without the role")
	}
}
