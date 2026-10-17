package app

// The stored market book (schema 050): a completed region sweep
// keeps the top orders per side, and the Market page serves them
// to anonymous visitors -- figures over the whole book from the
// sweep's stats row, never recomputed from the truncated top.
// The anonymous-market switch (market_anon.go) turns all of it
// off again, live.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedForgeBook sweeps a one-page Forge book for type 34 through
// the real sweep: seven sells (5..11) and seven buys (4..10), so
// the stored top-five and the whole-book figures disagree if the
// page ever confuses them. Other regions are marked fresh.
func seedForgeBook(t *testing.T, app *Application, q *db.Queries, transport *regionBookTransport) {
	t.Helper()
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	page := []esi.MarketOrder{}
	for i := int64(0); i < 7; i++ {
		page = append(page, esi.MarketOrder{
			OrderID: 100 + i, TypeID: 34, IsBuyOrder: false,
			Price: float64(5 + i), VolumeRemain: 100 * (i + 1),
			LocationID: 60003760, SystemID: 30000142,
		})
		page = append(page, esi.MarketOrder{
			OrderID: 200 + i, TypeID: 34, IsBuyOrder: true,
			Price: float64(4 + i), VolumeRemain: 10 * (i + 1),
			LocationID: 60003760, SystemID: 30000142,
		})
	}
	transport.books[10000002] = [][]esi.MarketOrder{page}
	sweepUntilIdle(t, app)
}

func TestRegionSweepStoresBookSnapshot(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedForgeBook(t, app, q, transport)

	rows, err := q.ListMarketBookOrders(ctx, db.ListMarketBookOrdersParams{RegionID: 10000002, TypeID: 34})
	if err != nil {
		t.Fatalf("list stored book: %v", err)
	}
	if len(rows) != 2*topMarketOrders {
		t.Fatalf("stored book rows = %d, want %d (top %d per side)", len(rows), 2*topMarketOrders, topMarketOrders)
	}
	var sells, buys []db.MarketBookOrder
	for _, row := range rows {
		if row.IsBuyOrder == 1 {
			buys = append(buys, row)
		} else {
			sells = append(sells, row)
		}
	}
	// Sells ranked cheapest first: 5, 6, 7, 8, 9 (of 5..11).
	for i, row := range sells {
		if row.Rank != int64(i) || row.Price != float64(5+i) {
			t.Fatalf("sell row %d: rank %d price %v, want %d / %v", i, row.Rank, row.Price, i, 5+i)
		}
		if row.LocationID != 60003760 || row.SystemID != 30000142 {
			t.Fatalf("sell row %d: location %d system %d, want the staged 60003760 / 30000142", i, row.LocationID, row.SystemID)
		}
		if row.UpdatedAt.IsZero() {
			t.Fatal("stored book rows carry no updated_at")
		}
	}
	// Buys ranked dearest first: 10, 9, 8, 7, 6 (of 4..10).
	for i, row := range buys {
		if row.Rank != int64(i) || row.Price != float64(10-i) {
			t.Fatalf("buy row %d: rank %d price %v, want %d / %v", i, row.Rank, row.Price, i, 10-i)
		}
	}

	// A re-sweep whose book no longer holds type 34 drops its
	// snapshot rows with its stats (wholesale replace).
	transport.books[10000002] = [][]esi.MarketOrder{
		{{OrderID: 300, TypeID: 35, IsBuyOrder: false, Price: 42.0, VolumeRemain: 3, LocationID: 60003760, SystemID: 30000142}},
	}
	if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: regionSweepKind(10000002), State: fetchStateOK,
		AttemptedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("age sweep record: %v", err)
	}
	sweepUntilIdle(t, app)
	var left int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM market_book_orders WHERE region_id = 10000002 AND type_id = 34`).Scan(&left); err != nil {
		t.Fatalf("count stored book after re-sweep: %v", err)
	}
	if left != 0 {
		t.Fatalf("stored book rows for the departed type 34 = %d, want 0", left)
	}
	rows, err = q.ListMarketBookOrders(ctx, db.ListMarketBookOrdersParams{RegionID: 10000002, TypeID: 35})
	if err != nil || len(rows) != 1 || rows[0].Price != 42.0 {
		t.Fatalf("stored book for type 35: rows=%+v err=%v, want the one 42.0 sell", rows, err)
	}
}

func TestAnonymousMarketStoredBookAndLiveToggle(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seedNext1SDE(t, conn)
	seedForgeBook(t, app, q, transport)

	// Anonymous: the stored book renders -- bests and typicals
	// from the sweep's whole-book figures (median sell of
	// 5..11 is 8, not the top-five's 7), the top orders in the
	// tables, the full depth from the stats row, and the sweep
	// note instead of the signed-in notice.
	code, body := getSwapPage(t, app, nil, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("GET /market/?type=34 (anonymous, stored book): status %d, want 200", code)
	}
	mustContain(t, "/market/?type=34 (anonymous, stored book)", body,
		"Tritanium",
		"Order book from the latest region sweep",
		"Best sell",
		"5.00 <small>ISK</small>",
		"Typical sell <b>8.00</b>",
		"Typical buy <b>7.00</b>",
		"Sell orders — The Forge",
		"Showing top 5 · 7 sell orders",
		"Showing top 5 · 7 buy orders",
	)
	if strings.Contains(body, "The live order book loads for signed-in visitors") {
		t.Error("anonymous stored-book view still shows the signed-in book notice")
	}
	// And the pending history left a market want.
	var wants int
	countWants := func() int {
		t.Helper()
		if err := conn.QueryRow(`SELECT COUNT(*) FROM market_history_wants WHERE region_id = 10000002 AND type_id = 34`).Scan(&wants); err != nil {
			t.Fatalf("count market wants: %v", err)
		}
		return wants
	}
	if countWants() == 0 {
		t.Error("anonymous stored-book view queued no market_history_want")
	}

	// An admin flips the switch off, live, through the panel:
	// the stored book closes and the wants stop, with no restart.
	grantTestAdmin(app, fixtureCharA)
	admin, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create admin account: %v", err)
	}
	seedCharacter(t, q, admin.ID, fixtureCharA, "Admin Main")
	adminCookie := sessionCookie(t, app, admin.ID, fixtureCharA, "Admin Main")

	_, adminBody := getPage(t, app, adminCookie, "/admin/")
	mustContain(t, "/admin/ (switch on)", adminBody, "Anonymous market access", "stored order book")

	if code, _ := postForm(t, app, adminCookie, "/admin/market-anon", url.Values{"state": {"off"}}); code != http.StatusSeeOther {
		t.Fatalf("POST /admin/market-anon off: status %d, want 303", code)
	}
	if !app.anonMarketBlocked() {
		t.Fatal("switch off did not land in the live state")
	}
	if row, err := q.GetSiteSetting(ctx, siteSettingMarketAnon); err != nil || row.Value != "off" {
		t.Fatalf("stored setting after off: %+v err=%v, want value off", row, err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM market_history_wants`); err != nil {
		t.Fatalf("clear wants: %v", err)
	}
	code, body = getSwapPage(t, app, nil, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("GET /market/?type=34 (anonymous, switch off): status %d, want 200", code)
	}
	mustContain(t, "/market/?type=34 (anonymous, switch off)", body, "The live order book loads for signed-in visitors")
	if strings.Contains(body, "Order book from the latest region sweep") {
		t.Error("switch off still serves the stored book")
	}
	if countWants() != 0 {
		t.Error("switch off still queues market wants for anonymous views")
	}

	// A non-admin cannot flip it.
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	seedCharacter(t, q, other.ID, fixtureMember, "Plain Member")
	otherCookie := sessionCookie(t, app, other.ID, fixtureMember, "Plain Member")
	if code, _ := postForm(t, app, otherCookie, "/admin/market-anon", url.Values{"state": {"on"}}); code != http.StatusForbidden {
		t.Fatalf("POST /admin/market-anon from a non-admin: status %d, want 403", code)
	}
	if !app.anonMarketBlocked() {
		t.Fatal("a non-admin POST changed the switch")
	}

	// Flipping it back on reopens the stored book at once.
	if code, _ := postForm(t, app, adminCookie, "/admin/market-anon", url.Values{"state": {"on"}}); code != http.StatusSeeOther {
		t.Fatalf("POST /admin/market-anon on: status %d, want 303", code)
	}
	if app.anonMarketBlocked() {
		t.Fatal("switch on did not land in the live state")
	}
	_, body = getSwapPage(t, app, nil, "/market/?type=34")
	mustContain(t, "/market/?type=34 (anonymous, switch back on)", body, "Order book from the latest region sweep")
	if countWants() == 0 {
		t.Error("switch back on did not resume market wants")
	}
}
