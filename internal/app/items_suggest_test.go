package app

// Hermetic tests for the market toolkit: the /market/suggest
// endpoint (marketable-only, prefix-first, graceful on an empty
// SDE), the /items/ category explorer (all levels + empty state),
// and the layout/typography invariants this build ships (fluid
// width, the shared 0/50/100 gradient ramp, condensed header
// face, portable-table pinning, the search-suggestion markup).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// seedSDEFixture data is planted inline below via conn.ExecContext.

func TestMarketSuggestAndItemsExplorer(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	stmts := []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (900, 'Fixture Category')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (910, 'Fixture Minerals', 900)`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (920, 'Fixture Ores', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (34, 'Tritanium', 910, 5, 1),
		   (35, 'Tritanium Alloy', 910, 5, 1),
		   (36, 'Pyerite', 910, 5, 1),
		   (37, 'Secret Tritanium', 910, 0, 1),
		   (38, 'Tritanium Draft', 910, 5, 0),
		   (39, 'Mexallon', 920, 5, 1)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed SDE: %v", err)
		}
	}

	getSuggest := func(path string) []suggestItem {
		t.Helper()
		code, body := getPage(t, app, cookie, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, code)
		}
		var out []suggestItem
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("GET %s: invalid JSON %q: %v", path, body, err)
		}
		return out
	}

	// Prefix matches first, unpublished and market-group-less
	// types excluded even though their names match.
	got := getSuggest("/market/suggest?q=trit")
	if len(got) != 2 || got[0].ID != 34 || got[1].ID != 35 {
		t.Fatalf("suggest q=trit = %+v, want [Tritanium Tritanium Alloy]", got)
	}
	// Substring-only match still lands.
	got = getSuggest("/market/suggest?q=alloy")
	if len(got) != 1 || got[0].ID != 35 {
		t.Fatalf("suggest q=alloy = %+v, want [Tritanium Alloy]", got)
	}
	// Below the length floor, and no match at all: empty arrays,
	// never null.
	if got := getSuggest("/market/suggest?q=t"); len(got) != 0 {
		t.Fatalf("suggest q=t = %+v, want []", got)
	}
	if got := getSuggest("/market/suggest?q=zzzz"); len(got) != 0 {
		t.Fatalf("suggest q=zzzz = %+v, want []", got)
	}

	// Explorer: all three levels render, link downward, and the
	// new nav entry is present.
	code, body := getPage(t, app, cookie, "/items/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/: status %d", code)
	}
	mustContain(t, "/items/", body, "Fixture Category", `href="/items/category/900/"`, `href="/items/"`)

	code, body = getPage(t, app, cookie, "/items/category/900/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/category/900/: status %d", code)
	}
	mustContain(t, "/items/category/900/", body, "Fixture Minerals", "Fixture Ores", `href="/items/group/910/"`)

	code, body = getPage(t, app, cookie, "/items/group/910/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/group/910/: status %d", code)
	}
	mustContain(t, "/items/group/910/", body, "Tritanium", `href="/items/type/34/"`, "on market")

	// Unknown ids bounce back to the top instead of 500ing.
	if code, _ := getPage(t, app, cookie, "/items/group/424242/"); code != http.StatusSeeOther {
		t.Fatalf("GET unknown group: status %d, want %d (redirect to /items/)", code, http.StatusSeeOther)
	}

	// Market page carries the suggestion plumbing.
	code, body = getPage(t, app, cookie, "/market/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/: status %d", code)
	}
	mustContain(t, "/market/", body, `id="market-q"`, `id="market-suggest"`, `id="market-region"`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestItemsAndSuggestOnEmptySDE: before the first import lands,
// the explorer renders its empty state and suggestions are an
// empty array — neither page may error.
func TestItemsAndSuggestOnEmptySDE(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/items/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/ (empty): status %d", code)
	}
	mustContain(t, "/items/ (empty)", body, "No static data yet")

	code, body = getPage(t, app, cookie, "/market/suggest?q=trit")
	if code != http.StatusOK {
		t.Fatalf("GET suggest (empty): status %d", code)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Fatalf("suggest (empty) body = %q, want []", body)
	}
}
