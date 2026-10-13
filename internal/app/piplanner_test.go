package app

import (
	"context"
	"net/http"
	"testing"

	"evesynapse/internal/esi"
)

// TestPIPlannerRendersChain: the PI Planner page renders the full
// simulator from local data (no outbound calls), with the chain
// map, requirements and colony suggestions for a P2 pick, and
// pre-fills planet types from the character's colonies.
func TestPIPlannerRendersChain(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	// Colonies on Gas + Storm: Coolant (Water+Electrolytes) needs
	// Aqueous Liquids + Ionic Solutions, both on Gas — covered.
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, esi.Colonies{
		{PlanetID: 401, SolarSystemID: 30000142, PlanetType: "gas", UpgradeLevel: 5, NumPins: 10},
		{PlanetID: 402, SolarSystemID: 30000142, PlanetType: "storm", UpgradeLevel: 5, NumPins: 8},
	})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/industry/pi/?product=Coolant&amount=240")
	if code != http.StatusOK {
		t.Fatalf("pi planner: status %d", code)
	}
	mustContain(t, "/industry/pi/", body,
		"PI Planner",
		"Production map",
		"Coolant",
		"Suggested colonies",
		"Covered",
		"Extractor",
		// The live-coverage hooks pi.js drives: the verdict
		// element's id and each raw's planet sources.
		`id="pi-coverage"`,
		`data-pi-planets="Gas"`,
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handler made %d outbound calls, want 0", got)
	}

	// Uncovered pick with explicit planets reports the gap.
	code, body = getPage(t, app, cookie, "/industry/pi/?product=Coolant&planets=Lava")
	if code != http.StatusOK {
		t.Fatalf("pi planner uncovered: status %d", code)
	}
	mustContain(t, "/industry/pi/ uncovered", body, "Not covered", "Missing:")
}

// TestPIPlannerDefaults: bare /industry/pi/ opens on the default
// P4 target with a complete plan (no character required beyond login).
func TestPIPlannerDefaults(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/industry/pi/")
	if code != http.StatusOK {
		t.Fatalf("pi planner default: status %d", code)
	}
	mustContain(t, "/industry/pi/ default", body, "Broadcast Node", "Wetware Mainframe", "High-Tech")
}
