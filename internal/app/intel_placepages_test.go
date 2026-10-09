package app

// Hermetic tests for the solar system & station destination
// pages (v0.3.13) and the place-name linking sweep: the helpers'
// policy, the SDE-backed page renders (fields, security
// formatting, station lists, unknown-id states), and the
// surfaces that now link system/station names — character
// location, home fleet, colonies, assets, orders, courier
// contracts. Every render runs against the counting transport,
// which must stay at zero calls.

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Helper policy: systems and stations resolve to their pages.
// ---------------------------------------------------------------------------

func TestPlaceLinkHelpers(t *testing.T) {
	cases := []struct {
		name string
		got  template.HTML
		want string
	}{
		{"system", systemLink(30000142, "Jita"), `<a href="/system/?system=30000142">Jita</a>`},
		{"system no id", systemLink(0, "Jita"), `Jita`},
		{"system no name", systemLink(30000142, ""), ``},
		{"station", stationLink(60003760, "Jita 4 - Moon 4"), `<a href="/station/?station=60003760">Jita 4 - Moon 4</a>`},
		{"station no id", stationLink(0, "Jita 4 - Moon 4"), `Jita 4 - Moon 4`},
		{"station no name", stationLink(60003760, ""), ``},
		{"system name escaped", systemLink(30000142, `<b>"x"</b>`), `<a href="/system/?system=30000142">&lt;b&gt;&#34;x&#34;&lt;/b&gt;</a>`},
		{"ref station wins", placeLink(placeRef{Name: "Jita 4 - Moon 4", StationID: 60003760, SystemID: 30000142}), `<a href="/station/?station=60003760">Jita 4 - Moon 4</a>`},
		{"ref system", placeLink(placeRef{Name: "Jita", SystemID: 30000142}), `<a href="/system/?system=30000142">Jita</a>`},
		{"ref structure stays text", placeLink(placeRef{Name: "Some Citadel"}), `Some Citadel`},
		{"ref fallback stays text", placeLink(placeRef{Name: "Station #60003760"}), `Station #60003760`},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// seedPlaceSDE plants the Jita neighbourhood: two systems, the
// region, and two stations in Jita (plus one next door, which the
// system page must not list).
func seedPlaceSDE(t *testing.T, app *Application) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946), (30000143, 'Perimeter', 10000002, 0.9)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142), (60003761, 'Jita 4 - Moon 10 - Hyasyoda Corporation Refinery', 30000142), (60003762, 'Perimeter - Tranquility Trading Tower', 30000143)`,
	} {
		if _, err := app.db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// The pages themselves.
// ---------------------------------------------------------------------------

func TestSystemAndStationPages(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	// System page: name, EVE-rounded security (0.946 -> 0.9),
	// region, and its own stations — linked — but not the
	// station next door.
	code, body := getPage(t, app, cookie, "/system/?system=30000142")
	if code != http.StatusOK {
		t.Fatalf("GET /system/: status %d", code)
	}
	mustContain(t, "/system/", body, "Jita", "0.9", "The Forge",
		`<a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a>`,
		`<a href="/station/?station=60003761">Jita 4 - Moon 10 - Hyasyoda Corporation Refinery</a>`)
	if strings.Contains(body, "Tranquility Trading Tower") {
		t.Error("system page listed a station from another system")
	}

	// Station page: name, system linked, region as text.
	code, body = getPage(t, app, cookie, "/station/?station=60003760")
	if code != http.StatusOK {
		t.Fatalf("GET /station/: status %d", code)
	}
	mustContain(t, "/station/", body, "Jita 4 - Moon 4 - Caldari Navy Assembly Plant",
		`<a href="/system/?system=30000142">Jita</a>`, "The Forge")

	// Unknown ids: honest not-in-the-star-map states, still 200.
	code, body = getPage(t, app, cookie, "/system/?system=39999999")
	if code != http.StatusOK {
		t.Fatalf("GET /system/ unknown: status %d", code)
	}
	mustContain(t, "/system/ unknown", body, "isn't in the local star map yet")
	code, body = getPage(t, app, cookie, "/station/?station=69999999")
	if code != http.StatusOK {
		t.Fatalf("GET /station/ unknown: status %d", code)
	}
	mustContain(t, "/station/ unknown", body, "isn't in the local directory yet")

	// Unparseable ids bounce home instead of erroring.
	for _, path := range []string{"/system/", "/system/?system=abc", "/station/?station=-4"} {
		if code, _ := getPage(t, app, cookie, path); code != http.StatusSeeOther {
			t.Errorf("GET %s: status %d, want %d (redirect)", path, code, http.StatusSeeOther)
		}
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Swept surfaces.
// ---------------------------------------------------------------------------

func TestCharacterPageLinksPlaces(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{SolarSystemID: 30000142, StationID: 60003760})
	// The character page's remaining live-state sections sit on
	// snapshots this test leaves empty; seed them so the renders
	// below stay cache-only.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapShip, esi.Ship{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFatigue, esi.Fatigue{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapImplants, esi.Implants{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapClones, esi.Clones{})

	code, body := getPage(t, app, cookie, "/character/")
	if code != http.StatusOK {
		t.Fatalf("GET /character/: status %d", code)
	}
	mustContain(t, "/character/", body,
		`<a href="/system/?system=30000142">Jita</a>`,
		`<a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a>`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestHomeFleetLinksPlaces(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedOverviewCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	seedPlaceSDE(t, app)

	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{
		Name: "Fixture Alpha", CorporationID: fixtureCorpA,
		Birthday: "2009-12-24T00:00:00Z", SecurityStatus: 0.55,
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{SolarSystemID: 30000142, StationID: 60003760})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{Online: true, LastLogin: rfc(now.Add(-time.Hour))})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1000.5)

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		`<a href="/system/?system=30000142">Jita</a>`,
		`<a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a>`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestColoniesLinkSystem(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, esi.Colonies{
		{PlanetID: fixturePlanetA, SolarSystemID: 30000142, PlanetType: "temperate", NumPins: 4, UpgradeLevel: 3, LastUpdate: rfc(now.Add(-time.Hour))},
	})

	code, body := getPage(t, app, cookie, "/planets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /planets/: status %d", code)
	}
	mustContain(t, "/planets/", body, `<a href="/system/?system=30000142">Jita</a>`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestAssetsLinkStation(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 31, TypeID: 34, Quantity: 100, LocationID: 60003760, LocationType: "station"},
	})

	code, body := getPage(t, app, cookie, "/assets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /assets/: status %d", code)
	}
	mustContain(t, "/assets/", body,
		`<a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a>`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestOrdersAndCourierLinkPlaces(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 1, TypeID: 34, LocationID: 60003760, RegionID: 10000002, IsBuyOrder: true, Price: 5, VolumeTotal: 100, VolumeRemain: 50, Range: "station", Issued: rfc(now.Add(-time.Hour)), Duration: 90},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{
		{ContractID: 556, IssuerID: fixtureCharA, Type: "courier", Status: "in_progress", Reward: 500000, Collateral: 2000000, StartLocationID: 60003760, EndLocationID: 60003761, DateIssued: rfc(now.Add(-time.Hour)), DateExpired: rfc(now.Add(48 * time.Hour))},
	})

	code, body := getPage(t, app, cookie, "/orders/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /orders/: status %d", code)
	}
	mustContain(t, "/orders/", body,
		`<a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a>`)

	code, body = getPage(t, app, cookie, "/contracts/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /contracts/: status %d", code)
	}
	mustContain(t, "/contracts/", body,
		`Courier: <a href="/station/?station=60003760">Jita 4 - Moon 4 - Caldari Navy Assembly Plant</a> → <a href="/station/?station=60003761">Jita 4 - Moon 10 - Hyasyoda Corporation Refinery</a>`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}
