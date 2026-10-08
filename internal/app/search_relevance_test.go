package app

import (
	"context"
	"reflect"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestSearchFeedsPutPrefixMatchesFirst: every search feed that orders by
// relevance lists the names that start with the query before the names
// that merely contain it, then alphabetically inside each tier. The
// relevance term used to be written with a raw $1 beside the named @q, which
// sqlc turned into a second parameter nobody set; it stayed empty, every
// name "started with" it, and the ordering collapsed to plain alphabetical.
// The fixture is built so that alphabetical and relevance order disagree:
// "Fleet Rifter" sorts before "Rifter" but only contains the query (and the
// module and drone names are chosen the same way).
func TestSearchFeedsPutPrefixMatchesFirst(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship'), (7, 'Module'), (18, 'Drone')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (950, 'Frigate', 6), (951, 'Projectile Weapon', 7), (952, 'Combat Drone', 18)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(5001, 'Fleet Rifter', 950, 1, 1),
			(5002, 'Rifter', 950, 1, 1),
			(5003, 'Rifter Navy Issue', 950, 1, 1),
			(6001, 'Autocannon Gun', 951, 1, 1),
			(6002, 'Gun Mk II', 951, 1, 1),
			(6003, 'Gunnery Array', 951, 1, 1),
			(7001, 'Advanced Drone', 952, 1, 1),
			(7002, 'Drone Mk II', 952, 1, 1)`,
		// effect 12 is the high slot; attribute 1272 is a drone's bandwidth use
		`INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES (6001, 12, 1), (6002, 12, 1), (6003, 12, 1)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES (7001, 1272, 10), (7002, 1272, 5)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	check := func(feed string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v (names starting with the query first)", feed, got, want)
		}
	}

	ships, err := q.SuggestSDEShips(ctx, db.SuggestSDEShipsParams{Q: "rifter", Lim: 10})
	if err != nil {
		t.Fatalf("SuggestSDEShips: %v", err)
	}
	var got []string
	for _, r := range ships {
		got = append(got, r.Name)
	}
	check("fit editor ship picker", got, []string{"Rifter", "Rifter Navy Issue", "Fleet Rifter"})

	slots, err := q.ListFitSlotTypes(ctx, db.ListFitSlotTypesParams{EffectID: 12, Q: "gun", Meta: 0, Lim: 10})
	if err != nil {
		t.Fatalf("ListFitSlotTypes: %v", err)
	}
	got = nil
	for _, r := range slots {
		got = append(got, r.Name)
	}
	check("fit editor module picker", got, []string{"Gun Mk II", "Gunnery Array", "Autocannon Gun"})

	drones, err := q.ListFitDroneTypes(ctx, db.ListFitDroneTypesParams{Q: "drone", Meta: 0, Lim: 10})
	if err != nil {
		t.Fatalf("ListFitDroneTypes: %v", err)
	}
	got = nil
	for _, r := range drones {
		got = append(got, r.Name)
	}
	check("fit editor drone picker", got, []string{"Drone Mk II", "Advanced Drone"})

	// The shared suggestion feed behind every autocomplete box.
	got = nil
	for _, s := range app.suggestTypes(ctx, "rifter", suggestPoolAll, 10) {
		got = append(got, s.Name)
	}
	check("shared suggestions", got, []string{"Rifter", "Rifter Navy Issue", "Fleet Rifter"})
}
