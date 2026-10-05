package app

// Cross-character asset search: the Assets page answers "which
// of my characters has an X, and where" from cached snapshots
// only. Hermetic: fixture snapshots, zero outbound calls.

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/esi"
)

func TestAssetSearchAcrossCharacters(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Alpha Pilot")
	seedCharacter(t, q, user.ID, fixtureCharB, "Beta Pilot")
	const gammaID = int64(93300002)
	seedCharacter(t, q, user.ID, gammaID, "Gamma Pilot")

	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 1, TypeID: 587, Quantity: 3, LocationID: 60003760, LocationType: "station", IsSingleton: true},
		{ItemID: 2, TypeID: 34, Quantity: 5000, LocationID: 60003760, LocationType: "station"},
	})
	seedSnapshot(t, q, fixtureCharB, esi.SnapAssets, []esi.Asset{
		{ItemID: 3, TypeID: 587, Quantity: 1, LocationID: 60003760, LocationType: "station", IsSingleton: true},
		{ItemID: 4, TypeID: 34, Quantity: 10, LocationID: 60003760, LocationType: "station"},
	})
	// Gamma Pilot deliberately has no asset snapshot yet.

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Alpha Pilot")

	// Case-insensitive match across both synced characters.
	code, body := getPage(t, app, cookie, "/assets/?q=RIFTER")
	if code != 200 {
		t.Fatalf("search status = %d", code)
	}
	mustContain(t, "/assets/?q=RIFTER", body,
		"2 stacks matching", "Rifter",
		"Alpha Pilot", "Beta Pilot",
		"Jita 4 - Moon 4 - Caldari Navy Assembly Plant",
		"Not included yet", "Gamma Pilot",
	)
	if strings.Contains(body, "Tritanium") {
		t.Errorf("rifter search leaked a non-matching item (Tritanium)")
	}

	// Item held by both, different quantities.
	_, body = getPage(t, app, cookie, "/assets/?q=tritanium")
	mustContain(t, "/assets/?q=tritanium", body, "Tritanium", "5,000", "Alpha Pilot", "Beta Pilot")

	// No matches anywhere.
	_, body = getPage(t, app, cookie, "/assets/?q=nyx")
	mustContain(t, "/assets/?q=nyx", body, "No stacks matching")

	// The plain page is untouched: single-character view, no
	// search chrome in the result block.
	_, body = getPage(t, app, cookie, "/assets/")
	mustContain(t, "/assets/", body, "Alpha Pilot", "Rifter", "Tritanium")
	if strings.Contains(body, "stacks matching") {
		t.Errorf("plain assets page shows search results without a query")
	}

	if n := transport.calls.Load(); n != 0 {
		t.Errorf("asset search made %d outbound calls, want 0", n)
	}
}
