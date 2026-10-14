package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"evesynapse/internal/esi"
	"evesynapse/internal/piplan"
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

	// A product just picked (cover=1) ticks the planet types it
	// needs, whatever was ticked before and whatever is owned.
	code, body = getPage(t, app, cookie, "/industry/pi/?product=Coolant&planets=Lava&cover=1")
	if code != http.StatusOK {
		t.Fatalf("pi planner cover: status %d", code)
	}
	for _, p := range piplan.PlanetTypes {
		want := false
		for _, m := range piplan.MinimalPlanets("Coolant") {
			want = want || m == p
		}
		box := `name="planets" value="` + p + `" checked`
		if got := strings.Contains(body, box); got != want {
			t.Errorf("cover=1: %s ticked = %v, want %v (Coolant needs %v)", p, got, want, piplan.MinimalPlanets("Coolant"))
		}
	}
	mustContain(t, "/industry/pi/ cover", body, "Covered", `id="pi-wrap"`,
		// Every chain input names where it is made: the target's
		// factory line, and a raw's or P1's ticked planet type.
		`<small class="pi-on">factory on any planet</small>`,
		`<small class="pi-on">on `+piplan.MinimalPlanets("Coolant")[0])
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

// TestPIPlannerVolumesComeFromTheSDE: the page takes each
// commodity's volume from the imported SDE, and the engine's
// built-in figure only where the SDE has none.
func TestPIPlannerVolumesComeFromTheSDE(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	// An SDE in which Coolant takes 2 m3 a unit, unlike the built-in 0.75.
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name) VALUES (9832, 'Coolant') ON CONFLICT (type_id) DO UPDATE SET name = excluded.name`,
		`INSERT INTO sde_type_physics (type_id, mass, volume, capacity) VALUES (9832, 0, 2, 0)
			ON CONFLICT (type_id) DO UPDATE SET volume = excluded.volume`,
	} {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed SDE: %v", err)
		}
	}
	volumes := app.piVolumesByName(ctx)
	if volumes["Coolant"] != 2 {
		t.Fatalf("Coolant volume from the SDE = %v, want 2", volumes["Coolant"])
	}
	if _, ok := volumes["Water"]; ok {
		t.Fatalf("a commodity the SDE does not carry got a volume: %v", volumes["Water"])
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/industry/pi/?product=Coolant&amount=240")
	if code != http.StatusOK {
		t.Fatalf("pi planner: status %d", code)
	}
	// 240 Coolant at the SDE's 2 m3; 1,920 Water at the built-in 0.19.
	mustContain(t, "/industry/pi/", body, "480.0 m³", "364.8 m³")
}
