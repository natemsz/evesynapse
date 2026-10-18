package app

// The item <-> contract discovery loop: the top banner search
// finds contract-only item types (blueprints and reaction
// formulas carry no market group, so the suggestion feed's
// market floor used to hide them), every item page links into
// Contract Search filtered to its type, and the contract
// search honors that item_type deep link with an exact type
// match (the name search stays a substring convenience).

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
)

// seedContractOnlySDE plants the type classes that trade only
// through contracts: a published blueprint and a published
// reaction formula (SDE category 9, no market group), plus an
// unpublished blueprint that must stay hidden everywhere.
func seedContractOnlySDE(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (9, 'Blueprint')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES
		   (960, 'Fixture Blueprints', 9), (961, 'Fixture Reaction Formulas', 9)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (991, 'Kryos Blueprint', 960, 0, 1),
		   (992, 'Kryos Reaction Formula', 961, 0, 1),
		   (993, 'Unreleased Kryos Blueprint', 960, 0, 0)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed contract-only SDE: %v", err)
		}
	}
}

func TestTopbarSearchFindsContractOnlyTypes(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedNext1SDE(t, conn)
	seedContractOnlySDE(t, conn)

	hitNames := func(path string) map[string]bool {
		t.Helper()
		names := map[string]bool{}
		for _, h := range getSearchHits(t, app, cookie, path) {
			if h.Kind == "item" {
				names[h.Name] = true
			}
		}
		return names
	}

	// The banner search offers the blueprint and the formula:
	// published category-9 types with no market group.
	items := hitNames("/search.json?q=kryos")
	if !items["Kryos Blueprint"] {
		t.Error("topbar search must find the Kryos Blueprint (contract-only type)")
	}
	if !items["Kryos Reaction Formula"] {
		t.Error("topbar search must find the Kryos Reaction Formula (contract-only type)")
	}
	if items["Unreleased Kryos Blueprint"] {
		t.Error("topbar search must never offer an unpublished type")
	}

	// Ordinary tradeables still answer, and a published
	// non-market type outside category 9 stays out: the floor
	// widened for contract-only blueprints, not for every
	// untradeable row.
	items = hitNames("/search.json?q=tritanium")
	if !items["Tritanium"] {
		t.Error("topbar search lost the ordinary tradeable Tritanium")
	}
	if items["Secret Tritanium"] {
		t.Error("topbar search must still exclude a published non-market type outside the blueprint category")
	}

	// The Items DB suggestion feed shares the 'all' pool: same
	// widening there...
	code, body := getPage(t, app, cookie, "/items/search.json?q=kryos")
	if code != http.StatusOK {
		t.Fatalf("GET items search.json: status %d", code)
	}
	mustContain(t, "items suggest kryos", body, "Kryos Blueprint", "Kryos Reaction Formula")
	if strings.Contains(body, "Unreleased Kryos Blueprint") {
		t.Error("items suggest must never offer an unpublished type")
	}
	// ...but the Market page's pool keeps the market floor
	// alone: contract-only types are not market suggestions.
	code, body = getPage(t, app, cookie, "/items/search.json?q=kryos&pool=market")
	if code != http.StatusOK {
		t.Fatalf("GET items search.json pool=market: status %d", code)
	}
	if strings.Contains(body, "Kryos Blueprint") || strings.Contains(body, "Kryos Reaction Formula") {
		t.Error("market pool must keep the market-group floor: no contract-only types")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestItemPageLinksToContractSearch(t *testing.T) {
	transport := &countingTransport{}
	app, conn, _ := buildCorpTestApp(t, transport)
	seedNext1SDE(t, conn)
	seedContractOnlySDE(t, conn)

	// A contract-only type: no market link exists to lean on,
	// so the contract link is the page's trade path.
	code, body := getSwapPage(t, app, nil, "/items/type/991/")
	if code != http.StatusOK {
		t.Fatalf("GET kryos blueprint page: status %d", code)
	}
	mustContain(t, "kryos blueprint page", body,
		"Kryos Blueprint",
		"Not traded on the market",
		`<a href="/contracts/search/?item_type=991">Find contracts for this item</a>`,
	)

	// A market-traded type carries the contract link beside
	// its market link: the loop closes for every item.
	code, body = getSwapPage(t, app, nil, "/items/type/34/")
	if code != http.StatusOK {
		t.Fatalf("GET tritanium page: status %d", code)
	}
	mustContain(t, "tritanium page", body,
		`<a href="/market/?type=34">Orders &amp; price history</a>`,
		`<a href="/contracts/search/?item_type=34">Find contracts for this item</a>`,
	)
}

func TestContractSearchItemTypeFilter(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	// The deep link from the Test Blueprint's item page: both
	// contracts holding that exact type, and nothing else.
	_, body := getAnon(t, app, "/contracts/search/?item_type=9999")
	mustContain(t, "item_type=9999", body,
		"Original For Sale", "Copy For Sale", "Item: Test Blueprint")
	for _, title := range []string{"Cheap Tritanium", "Swap Meet", "Rare Relic", "Half Known"} {
		if strings.Contains(body, title) {
			t.Errorf("item_type=9999 must not match %q", title)
		}
	}

	// Direction rides along, exactly as it does for the name
	// search: offered-only and wanted-only split the two
	// Tritanium contracts.
	_, body = getAnon(t, app, "/contracts/search/?item_type=34")
	mustContain(t, "item_type=34 either", body, "Cheap Tritanium", "Swap Meet")
	_, body = getAnon(t, app, "/contracts/search/?item_type=34&item_dir=offered")
	mustContain(t, "item_type=34 offered", body, "Cheap Tritanium")
	if strings.Contains(body, "Swap Meet") {
		t.Error("offered-only item_type search matched a contract merely wanting the type")
	}
	_, body = getAnon(t, app, "/contracts/search/?item_type=34&item_dir=wanted")
	mustContain(t, "item_type=34 wanted", body, "Swap Meet")
	if strings.Contains(body, "Cheap Tritanium") {
		t.Error("wanted-only item_type search matched a contract merely offering the type")
	}

	// An unknown type id filters to nothing rather than
	// falling back to the whole corpus.
	_, body = getAnon(t, app, "/contracts/search/?item_type=424242")
	for _, title := range []string{"Cheap Tritanium", "Swap Meet", "Original For Sale", "Copy For Sale"} {
		if strings.Contains(body, title) {
			t.Errorf("item_type=424242 must match nothing, found %q", title)
		}
	}
}
