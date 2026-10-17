package app

// Anonymous public tools: the item browser, the market and the build
// planner serve logged-out traffic. Market and SDE data are shared by
// nature, so an anonymous render depends on no session or character
// state, reads only what is stored, and never asks ESI. The one kind
// of work an anonymous render may queue is a market want (price
// history): the market is public-facing and its ESI allowance is
// huge. Every other want stays signed-in only, and the whole
// allowance is behind the anonymous-market switch (market_anon.go).

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestPublicToolsAnonymous(t *testing.T) {
	transport := &countingTransport{}
	app, conn, _ := buildCorpTestApp(t, transport)
	seedNext1SDE(t, conn)

	// Top-level browse renders real content with no session.
	code, body := getSwapPage(t, app, nil, "/items/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/ (anonymous): status %d, want 200", code)
	}
	mustContain(t, "/items/ (anonymous)", body,
		`<main id="main-content">`,
		"Item Database",
		"Fixture Category",
		`<a href="/items/category/900/">`,
	)

	// Search mode is browse data too.
	code, body = getSwapPage(t, app, nil, "/items/?q=tritanium")
	if code != http.StatusOK {
		t.Fatalf("GET /items/?q=tritanium (anonymous): status %d, want 200", code)
	}
	mustContain(t, "/items/?q=tritanium (anonymous)", body,
		"Tritanium",
		"Fixture Minerals",
	)

	// The type page is public too: it is the item link target
	// site-wide and the sitemap's indexed item page.
	code, body = getSwapPage(t, app, nil, "/items/type/34/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/type/34/ (anonymous): status %d, want 200", code)
	}
	mustContain(t, "/items/type/34/ (anonymous)", body, "Tritanium")

	// The rest of the item browser is public too: categories,
	// groups, search and the description fragment.
	for _, path := range []string{
		"/items/category/900/",
		"/items/group/910/",
		"/items/search.json?q=tri",
		"/items/description-fragment?type=34",
	} {
		if code, _ := getSwapPage(t, app, nil, path); code != http.StatusOK {
			t.Errorf("GET %s (anonymous): status %d, want 200", path, code)
		}
	}

	// The market is public. An anonymous item view with nothing
	// stored yet reads only what is stored: no live order book
	// (it says so), no watch controls. (The stored-book render is
	// pinned in market_book_snapshot_test.go.)
	code, body = getSwapPage(t, app, nil, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("GET /market/?type=34 (anonymous): status %d, want 200", code)
	}
	mustContain(t, "/market/?type=34 (anonymous)", body, "Tritanium", "The live order book loads for signed-in visitors")
	if strings.Contains(body, "Watch this item") {
		t.Error("anonymous market view offers a watch")
	}
	for _, path := range []string{"/market/", "/market/?q=trit", "/market/scanner/", "/market/ideas/",
		"/market/tradefinder/", "/market/leaderboard/", "/market/suggest?q=trit",
		"/market/history-fragment?type=34&region=10000002"} {
		if code, _ := getSwapPage(t, app, nil, path); code != http.StatusOK {
			t.Errorf("GET %s (anonymous): status %d, want 200", path, code)
		}
	}
	// The personal pieces stay signed-in.
	if code, _ := getSwapPage(t, app, nil, "/market/restock/"); code != http.StatusSeeOther {
		t.Errorf("GET /market/restock/ (anonymous): status %d, want 303", code)
	}

	// The build planner judges an anonymous visitor's plans as an
	// All V pilot.
	// (Its own app: the planner's fixture SDE overlaps the browser's.)
	papp, _, _ := buildCorpTestApp(t, transport)
	seedPlannerSDE(t, papp)
	code, body = getSwapPage(t, papp, nil, "/planner/?product=1001&runs=1")
	if code != http.StatusOK {
		t.Fatalf("GET /planner/ (anonymous): status %d, want 200", code)
	}
	mustContain(t, "/planner/ (anonymous)", body, "Can an All V pilot build this?", `value="All V pilot"`)

	// None of it asked ESI, and the only work queued is market
	// wants: the pending market item view, the market search
	// prefetch and the item page's history section all leave
	// market_history_wants. Every other want kind stays empty.
	if n := transport.calls.Load(); n != 0 {
		t.Errorf("anonymous renders made %d outbound calls, want 0", n)
	}
	for _, table := range []string{"pilot_name_wants", "type_details", "typing_guesses"} {
		var n int
		if err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("anonymous renders queued %d rows in %s, want none", n, table)
		}
	}
	var wants int
	if err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM market_history_wants").Scan(&wants); err != nil {
		t.Fatalf("count market_history_wants: %v", err)
	}
	if wants == 0 {
		t.Error("anonymous market renders queued no market_history_wants, want some")
	}
}
