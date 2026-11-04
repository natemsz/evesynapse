package app

import (
	"context"
	"net/url"
	"testing"
)

// The add form's item box resolves a typed name when no pick filled
// the type id (no script, or the name typed out): the exact name
// wins, and the target lands on that type.
func TestRestockSaveResolvesTypedName(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (1, 'Charges', 8) ON CONFLICT DO NOTHING`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 1, 1, 1), (35, 'Tritanium Bar', 1, 1, 1) ON CONFLICT DO NOTHING`,
	} {
		if _, err := app.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%v: %s", err, s)
		}
	}
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Tester")
	postForm(t, app, cookie, "/market/restock/save/", url.Values{"item": {"tritanium"}, "type_id": {""}, "target_qty": {"500"}})
	targets, err := q.ListRestockTargets(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].TypeID != 34 || targets[0].TargetQty != 500 {
		t.Fatalf("targets = %+v, want Tritanium (34) at 500", targets)
	}
}
