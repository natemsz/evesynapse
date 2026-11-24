package app

// Planet name resolution: the durable planet_names queue behind the
// PI surfaces. Renders note wants and never fetch; the worker resolves
// them from the static data (sde_celestials); resolved names stick
// across restarts, and an id the static data lacks backs off for days.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedPlanet adds one planet to the static data.
func seedPlanet(t *testing.T, app *Application, id int64, name string) {
	t.Helper()
	if _, err := app.pool.Exec(context.Background(), `INSERT INTO sde_celestials (item_id, system_id, name, type_id, radius) VALUES ($1, 30000142, $2, 11, 5000000)`, id, name); err != nil {
		t.Fatalf("seed planet %d: %v", id, err)
	}
}

func TestPlanetResolveSuccessAndCacheHit(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedPlanet(t, app, 40152063, "Jita IV")

	app.notePlanetIDs(ctx, 40152063)
	resolved, limited := app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if limited || resolved != 1 {
		t.Fatalf("resolve: resolved=%d limited=%v, want 1/false", resolved, limited)
	}
	row, err := q.GetPlanetName(ctx, 40152063)
	if err != nil || row.State != esi.PlanetResolved || row.Name != "Jita IV" {
		t.Fatalf("resolved row: %+v err=%v", row, err)
	}
	// The render tier answers from the durable row now.
	if got := app.planetDisplayName(ctx, 40152063); got != "Jita IV" {
		t.Fatalf("planetDisplayName: %q, want Jita IV", got)
	}
	// A fresh resolved row is not due: nothing to do next pass.
	resolved, _ = app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("second pass resolved %d, want 0", resolved)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("resolution made %d outbound calls, want 0", got)
	}
}

func TestPlanetResolveNegativeBackoff(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	const badID = int64(40999999)
	app.notePlanetIDs(ctx, badID)
	resolved, _ := app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("unknown planet resolve: resolved=%d, want 0", resolved)
	}
	row, err := q.GetPlanetName(ctx, badID)
	if err != nil || row.State != esi.PlanetMissing {
		t.Fatalf("row after a miss: %+v err=%v, want missing", row, err)
	}
	// The fresh negative is not re-asked within the window, and the
	// fallback stays honest meanwhile.
	if resolved, _ := app.resolvePlanetNames(ctx, &fetchBudget{left: 120}); resolved != 0 {
		t.Fatalf("second pass resolved %d, want 0", resolved)
	}
	if got := app.planetDisplayName(ctx, badID); got != "Planet #40999999" {
		t.Fatalf("planetDisplayName: %q, want the id fallback", got)
	}
}

func TestPlanetResolvePersistsWarmedNameWithoutFetch(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	// The colonies harvest warmed the name in-process; the
	// durable pass persists it without spending a fetch.
	app.esi.StorePlaceName(40152063, "Jita IV")
	app.notePlanetIDs(ctx, 40152063)
	resolved, _ := app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if resolved != 1 {
		t.Fatalf("persist-warmed resolve: resolved=%d, want 1", resolved)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("persist made %d outbound calls, want 0", got)
	}
	row, err := q.GetPlanetName(ctx, 40152063)
	if err != nil || row.State != esi.PlanetResolved || row.Name != "Jita IV" {
		t.Fatalf("persisted row: %+v err=%v", row, err)
	}
}

// TestPlanetRenderFallbackThenName drives the colonies page the
// way the user sees it: "Planet #40152063" before the worker has
// resolved anything (with the want noted and zero outbound
// calls), the real name once the durable cache holds it.
func TestPlanetRenderFallbackThenName(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, esi.Colonies{
		{PlanetID: 40152063, SolarSystemID: 30000142, PlanetType: "temperate", NumPins: 1, UpgradeLevel: 0, LastUpdate: rfc(now.Add(-time.Hour))},
	})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/planets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("planets page: status %d", code)
	}
	mustContain(t, "/planets/ (unresolved)", body, "Planet #40152063")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("planets render made %d outbound calls, want 0", got)
	}
	// The render noted the want for the worker.
	row, err := q.GetPlanetName(ctx, 40152063)
	if err != nil || row.State != esi.PlanetPending {
		t.Fatalf("want after render: %+v err=%v, want pending", row, err)
	}

	// The worker's answer lands in the durable cache...
	if err := q.SetPlanetName(ctx, db.SetPlanetNameParams{
		PlanetID: 40152063, Name: "Jita IV", State: esi.PlanetResolved,
		ResolvedAt: timeSet(now.UTC()),
	}); err != nil {
		t.Fatalf("seed resolved planet: %v", err)
	}
	// ...and the next render shows it, still without fetching.
	code, body = getPage(t, app, cookie, "/planets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("planets page (resolved): status %d", code)
	}
	mustContain(t, "/planets/ (resolved)", body, "Jita IV")
	if strings.Contains(body, "Planet #40152063") {
		t.Fatal("resolved render still shows the id fallback")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("resolved render made %d outbound calls, want 0", got)
	}
}

// TestPlanetHomeNameResolves covers the user's screenshot: the
// home's Planetary Industry module shows "Planet #40152063" until
// the name lands, then names the planet.
func TestPlanetHomeNameResolves(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, esi.Colonies{
		{PlanetID: 40152063, SolarSystemID: 30000142, PlanetType: "temperate", NumPins: 1, UpgradeLevel: 0, LastUpdate: rfc(now.Add(-time.Hour))},
	})
	seedSnapshot(t, q, fixtureCharA, esi.PlanetLayoutKind(40152063), esi.PlanetLayout{
		Pins: []esi.PlanetPin{
			{PinID: 1, TypeID: 990201, ExpiryTime: rfc(now.Add(-time.Hour)),
				ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 34, CycleTime: 1800, QtyPerCycle: 1200, Heads: []esi.PlanetExtractorHead{{HeadID: 0}}}},
		},
	})
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `["pi"]`,
		ID:         user.ID,
	}); err != nil {
		t.Fatalf("set home layout: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	mustContain(t, "/ (unresolved planet)", body, "Planet #40152063")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home render made %d outbound calls, want 0", got)
	}

	if err := q.SetPlanetName(ctx, db.SetPlanetNameParams{
		PlanetID: 40152063, Name: "Jita IV", State: esi.PlanetResolved,
		ResolvedAt: timeSet(now.UTC()),
	}); err != nil {
		t.Fatalf("seed resolved planet: %v", err)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home (resolved): status %d", code)
	}
	mustContain(t, "/ (resolved planet)", body, "Jita IV")
	if strings.Contains(body, "Planet #40152063") {
		t.Error("resolved planet still shows its placeholder name")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home (resolved) render made %d outbound calls, want 0", got)
	}
}

// TestWarmPlanetNamesReadStaticData pins the per-character warm pass's
// share of the planet_names store: a planet the static data names is
// stored resolved, an id it lacks is recorded as a miss, id 0 is never
// looked up, and none of it calls out.
func TestWarmPlanetNamesReadStaticData(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedPlanet(t, app, 40009077, "Jita I")

	wants := map[int64]bool{0: true, 40026205: true, 40009077: true}
	app.warmMissingPlanetNames(ctx, wants, &warmBudget{left: 100})

	row, err := q.GetPlanetName(ctx, 40009077)
	if err != nil || row.State != esi.PlanetResolved || row.Name != "Jita I" {
		t.Fatalf("named planet row: %+v err=%v, want resolved Jita I", row, err)
	}
	row, err = q.GetPlanetName(ctx, 40026205)
	if err != nil || row.State != esi.PlanetMissing {
		t.Fatalf("unknown planet row: %+v err=%v, want state %q", row, err, esi.PlanetMissing)
	}
	if _, err := q.GetPlanetName(ctx, 0); err == nil {
		t.Fatal("planet 0 got a planet_names row, want none")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("warm pass made %d outbound calls, want 0", got)
	}
}
