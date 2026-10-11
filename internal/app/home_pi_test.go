package app

// PI home module filter: the Soonest table carries the generic
// client-side filter wiring (a search input plus data-search
// rows), so typing filters the extractors with no page reload.
// The generic handler lives in app.js; this pins the markup it
// needs.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

func TestPIWidgetFilterWiring(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	planetID := int64(40123456)
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, []esi.Colony{{PlanetID: planetID, SolarSystemID: 30000142}})
	seedSnapshot(t, q, fixtureCharA, esi.PlanetLayoutKind(planetID), esi.PlanetLayout{Pins: []esi.PlanetPin{
		{PinID: 1, ExpiryTime: rfc(now.Add(-time.Hour)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 2393}},
		{PinID: 2, ExpiryTime: rfc(now.Add(6 * time.Hour)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 2393}},
	}})
	setHomeLayout(t, q, user.ID, `[{"id":"pi"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		`data-table-filter="pi-table"`,
		`data-row-class="pi-row"`,
		`id="pi-table"`,
		`class="pi-row"`,
		`data-search="Fixture Alpha Planet #40123456 expired"`,
		`data-search="Fixture Alpha Planet #40123456"`,
	)
}
