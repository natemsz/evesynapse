package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestItemPageArtVariants: the item page's art is the type's
// render with its icon as fallback — except blueprint-category
// types (blueprints, reaction formulas), which have neither
// asset on the image server and must use the /bp image with no
// fallback.
func TestItemPageArtVariants(t *testing.T) {
	ctx := context.Background()
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (900, 'Fixture Category')`,
		`INSERT INTO sde_categories (category_id, name) VALUES (9, 'Blueprint')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (910, 'Fixture Minerals', 900)`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (920, 'Fixture Blueprints', 9)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 910, 2, 1)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (7001, 'Fixture Blueprint', 920, 0, 1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	code, body := getPage(t, app, cookie, "/items/type/7001/")
	if code != http.StatusOK {
		t.Fatalf("blueprint item page: status %d", code)
	}
	mustContain(t, "/items/type/7001/", body,
		`src="https://images.evetech.net/types/7001/bp?size=128"`)
	for _, bad := range []string{"types/7001/render", "types/7001/icon"} {
		if strings.Contains(body, bad) {
			t.Errorf("blueprint item page references %s, which the image server does not have", bad)
		}
	}

	code, body = getPage(t, app, cookie, "/items/type/34/")
	if code != http.StatusOK {
		t.Fatalf("mineral item page: status %d", code)
	}
	mustContain(t, "/items/type/34/", body,
		`src="https://images.evetech.net/types/34/render?size=128"`,
		`data-fallback-src="https://images.evetech.net/types/34/icon?size=128"`)
}
