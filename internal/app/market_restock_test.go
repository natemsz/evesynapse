package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The add form's item box resolves a typed name when no pick filled
// the type id (no script, or the name typed out): the exact name
// wins, and the target lands on that type. The planner prices
// targets from the selected hub and displays the prices' as-of.
func TestRestockSaveResolvesTypedName(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	stamp := time.Date(2026, 10, 5, 13, 4, 0, 0, time.UTC)
	for _, s := range []string{
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (1, 'Charges', 8) ON CONFLICT DO NOTHING`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 1, 1, 1), (35, 'Tritanium Bar', 1, 1, 1) ON CONFLICT DO NOTHING`,
	} {
		if _, err := app.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%v: %s", err, s)
		}
	}
	if _, err := app.db.ExecContext(ctx, `INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, updated_at) VALUES
		(10000002, 34, 6.50, 5.00, $1),
		(10000043, 34, 10.00, 8.00, $1)`, stamp); err != nil {
		t.Fatalf("seed market stats: %v", err)
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

	// Default hub is The Forge (6.50 * 500 = 3,250.00) with as-of.
	code, body := getPage(t, app, cookie, "/market/restock/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/restock/: status %d", code)
	}
	mustContain(t, "restock forge", body, "The Forge sell prices", "prices as of Oct 5, 1:04 PM", "6.50", "3,250.00", "Oct 5, 1:04 PM")

	// Selecting Domain prices from Domain (10.00 * 500 = 5,000.00), not The Forge.
	code, body = getPage(t, app, cookie, "/market/restock/?region=10000043")
	if code != http.StatusOK {
		t.Fatalf("GET /market/restock/?region=10000043: status %d", code)
	}
	mustContain(t, "restock domain", body, "Domain sell prices", "prices as of Oct 5, 1:04 PM", "10.00", "5,000.00")
	if strings.Contains(body, "3,250.00") {
		t.Error("Domain restock page showed The Forge's restock total")
	}
}
