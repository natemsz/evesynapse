package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestSEOFrontPage: the anonymous front page carries the
// descriptive title, its description, canonical and the
// WebApplication block; the signed-in overview keeps "Home" and
// gets no public head metadata.
func TestSEOFrontPage(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	rec := get(app, nil, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous GET /: status %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, "/ (anonymous)", body,
		"<title>EVE Online Fleet, Market & Fitting Tools · EveSynapse</title>",
		`<meta name="description" content="Free EVE Online tools`,
		`<link rel="canonical" href="https://evesynapse.app/">`,
		`"@type":"WebApplication"`, `"name":"EveSynapse"`, `"operatingSystem":"Web"`, `"price":0`)

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, signed := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("signed-in GET /: status %d", code)
	}
	mustContain(t, "/ (signed in)", signed, "<title>Home · EveSynapse</title>")
	if strings.Contains(signed, `rel="canonical"`) || strings.Contains(signed, "WebApplication") {
		t.Error("the signed-in overview carries public head metadata")
	}
}

// TestSEOItemAndShipPages: the item explorer, an item detail (its
// breadcrumb trail becomes the BreadcrumbList) and a ship detail
// each carry their title, the pattern description and the
// canonical.
func TestSEOItemAndShipPages(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (900, 'Fixture Category'), (6, 'Ship')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (910, 'Fixture Minerals', 900), (25, 'Frigate', 6)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 910, 2, 1), (587, 'Rifter', 25, 2, 1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	rec := get(app, nil, "/items/")
	body := rec.Body.String()
	mustContain(t, "/items/", body,
		"<title>Item Database · EveSynapse</title>",
		`<link rel="canonical" href="https://evesynapse.app/items/">`,
		"Browse EVE Online items by category and group")

	rec = get(app, nil, "/items/type/34/")
	body = rec.Body.String()
	mustContain(t, "/items/type/34/", body,
		"<title>Tritanium · EveSynapse</title>",
		"Market orders, price history and trade data for Tritanium, refreshed from ESI.",
		`<link rel="canonical" href="https://evesynapse.app/items/type/34/">`,
		`"@type":"BreadcrumbList"`, `"name":"Fixture Category"`, `"name":"Fixture Minerals"`,
		`"item":"https://evesynapse.app/items/type/34/"`)

	rec = get(app, nil, "/ships/587/")
	body = rec.Body.String()
	mustContain(t, "/ships/587/", body,
		"<title>Rifter · EveSynapse</title>",
		"Rifter stats, market prices and community fittings",
		`<link rel="canonical" href="https://evesynapse.app/ships/587/">`)
	if strings.Contains(body, "BreadcrumbList") {
		t.Error("the ship page displays no breadcrumb trail but emitted one")
	}
}

// TestSEOSystemAndToolPages: a system detail and the build planner
// hub carry their title, description and canonical.
func TestSEOSystemAndToolPages(t *testing.T) {
	transport := &countingTransport{}
	app, conn, _ := buildCorpTestApp(t, transport)
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	rec := get(app, nil, "/system/?system=30000142")
	body := rec.Body.String()
	mustContain(t, "/system/", body,
		"<title>Jita · EveSynapse</title>",
		"Jita in The Forge: security, activity, stations and market presence.",
		`<link rel="canonical" href="https://evesynapse.app/system/">`)

	rec = get(app, nil, "/planner/")
	body = rec.Body.String()
	mustContain(t, "/planner/", body,
		"<title>Build Planner · EveSynapse</title>",
		"Plan EVE Online industry builds: materials, build steps and costs",
		`<link rel="canonical" href="https://evesynapse.app/planner/">`)
}
