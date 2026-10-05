package app

// Tests for P4 own-order archaeology: the lifecycle ledger
// distilled from order snapshots inside refreshOrderHealth, and
// the "Your order history" section of the Orders page. Worker
// tests drive the health pass directly with stub transports;
// the render test proves the page reads only stored rows
// (countingTransport at zero calls).

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// lifecycleBookTransport serves a configurable regional book.
// The body is read on every request so a test can change the
// book between passes.
type lifecycleBookTransport struct {
	calls atomic.Int64
	body  *atomic.Value // string
}

func newLifecycleBookTransport(body string) *lifecycleBookTransport {
	t := &lifecycleBookTransport{body: &atomic.Value{}}
	t.body.Store(body)
	return t
}

func (s *lifecycleBookTransport) setBody(body string) {
	s.body.Store(body)
}

func (s *lifecycleBookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	body := "[]"
	if strings.Contains(req.URL.Path, "/orders/") {
		if v, ok := s.body.Load().(string); ok {
			body = v
		}
	}
	return &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestOrderLifecycleUpsertAndFillProgression(t *testing.T) {
	// Empty book: no undercut, health writes nothing interesting,
	// but lifecycle must still track the order's fill.
	transport := newLifecycleBookTransport(`[]`)
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// Pass 1: sell order for 1000, 1000 remaining.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 101, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.5, VolumeTotal: 1000, VolumeRemain: 1000},
	})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err := q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 101})
	if err != nil {
		t.Fatalf("lifecycle after pass 1: %v", err)
	}
	if row.VolumeRemainLast != 1000 || row.VolumeTotal != 1000 || row.ListedPrice != 5.5 || row.ClosedAt != "" || row.FirstSeenAt == "" {
		t.Fatalf("pass 1 row: %+v", row)
	}
	firstSeen := row.FirstSeenAt

	// Pass 2: 400 sold, price stepped to 5.4. FirstSeen must not move.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 101, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.4, VolumeTotal: 1000, VolumeRemain: 600},
	})
	// Book gate would skip the second fetch; that is fine --
	// lifecycle upsert does not depend on the book.
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err = q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 101})
	if err != nil {
		t.Fatalf("lifecycle after pass 2: %v", err)
	}
	if row.VolumeRemainLast != 600 || row.ListedPrice != 5.4 || row.FirstSeenAt != firstSeen || row.ClosedAt != "" {
		t.Fatalf("pass 2 row: %+v (firstSeen was %q)", row, firstSeen)
	}
	if row.LastSeenAt < firstSeen {
		t.Fatalf("lastSeen %q before firstSeen %q", row.LastSeenAt, firstSeen)
	}
}

func TestOrderLifecycleCloseAsFilled(t *testing.T) {
	transport := newLifecycleBookTransport(`[]`)
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 202, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.5, VolumeTotal: 10, VolumeRemain: 3},
	})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})

	// Remain hits 0 while still listed once, then vanishes.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 202, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.5, VolumeTotal: 10, VolumeRemain: 0},
	})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err := q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 202})
	if err != nil {
		t.Fatalf("lifecycle at zero remain: %v", err)
	}
	if row.ClosedAt != "" {
		t.Fatalf("order closed while still in snapshot: %+v", row)
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err = q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 202})
	if err != nil {
		t.Fatalf("lifecycle after vanish: %v", err)
	}
	if row.ClosedAt == "" || row.CloseKind != "filled" {
		t.Fatalf("want filled close, got %+v", row)
	}
}

func TestOrderLifecycleCloseAsEnded(t *testing.T) {
	transport := newLifecycleBookTransport(`[]`)
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// A buy order that vanishes with stock left must be 'ended',
	// never distinguished cancel-vs-expire.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 303, TypeID: 34, LocationID: 60003760, RegionID: 10000002, IsBuyOrder: true, Price: 4.0, VolumeTotal: 500, VolumeRemain: 500},
	})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err := q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 303})
	if err != nil {
		t.Fatalf("buy lifecycle: %v", err)
	}
	if row.IsBuyOrder != 1 {
		t.Fatalf("buy flag: %+v", row)
	}

	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err = q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 303})
	if err != nil {
		t.Fatalf("buy lifecycle after vanish: %v", err)
	}
	if row.ClosedAt == "" || row.CloseKind != "ended" {
		t.Fatalf("want ended close, got %+v", row)
	}
}

func TestOrderLifecycleOutbidOncePerTransition(t *testing.T) {
	// Book where a 9.5 sell at the same station beats my 10.0.
	undercutBook := `[
		{"order_id":1,"type_id":34,"location_id":60003760,"system_id":30000142,"is_buy_order":false,"price":10.0,"volume_remain":5,"volume_total":5},
		{"order_id":999,"type_id":34,"location_id":60003760,"system_id":30000142,"is_buy_order":false,"price":9.5,"volume_remain":100,"volume_total":100}
	]`
	bestBook := `[
		{"order_id":1,"type_id":34,"location_id":60003760,"system_id":30000142,"is_buy_order":false,"price":10.0,"volume_remain":5,"volume_total":5}
	]`
	transport := newLifecycleBookTransport(undercutBook)
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 1, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 10, VolumeTotal: 5, VolumeRemain: 5},
	})

	clearGate := func() {
		if _, err := conn.ExecContext(ctx, `DELETE FROM market_fetch_state WHERE kind LIKE 'book_%'`); err != nil {
			t.Fatalf("clear book gate: %v", err)
		}
	}

	// Poll 1: becomes beaten, one event.
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err := q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 1})
	if err != nil {
		t.Fatalf("lifecycle poll 1: %v", err)
	}
	if row.BeatenNow != 1 || row.OutbidEvents != 1 {
		t.Fatalf("poll 1: beaten=%d events=%d, want 1/1 (%+v)", row.BeatenNow, row.OutbidEvents, row)
	}

	// Poll 2: still beaten, must not count again.
	clearGate()
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err = q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 1})
	if err != nil {
		t.Fatalf("lifecycle poll 2: %v", err)
	}
	if row.BeatenNow != 1 || row.OutbidEvents != 1 {
		t.Fatalf("poll 2: beaten=%d events=%d, want 1/1", row.BeatenNow, row.OutbidEvents)
	}

	// Poll 3: back on top, beaten clears, still one event.
	transport.setBody(bestBook)
	clearGate()
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err = q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 1})
	if err != nil {
		t.Fatalf("lifecycle poll 3: %v", err)
	}
	if row.BeatenNow != 0 || row.OutbidEvents != 1 {
		t.Fatalf("poll 3: beaten=%d events=%d, want 0/1", row.BeatenNow, row.OutbidEvents)
	}

	// Poll 4: beaten again, second event.
	transport.setBody(undercutBook)
	clearGate()
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	row, err = q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: fixtureCharA, OrderID: 1})
	if err != nil {
		t.Fatalf("lifecycle poll 4: %v", err)
	}
	if row.BeatenNow != 1 || row.OutbidEvents != 2 {
		t.Fatalf("poll 4: beaten=%d events=%d, want 1/2", row.BeatenNow, row.OutbidEvents)
	}
}

func TestOrderLifecyclePruneBound(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	_ = ch

	now := time.Now().UTC()
	old := now.AddDate(-2, 0, 0).Format(time.RFC3339) // >365 days ago
	recent := now.AddDate(0, -1, 0).Format(time.RFC3339)
	// 600 old closed rows + 5 recent closed rows, written directly.
	for i := 0; i < 605; i++ {
		orderID := int64(10000 + i)
		if err := q.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
			CharacterID: fixtureCharA, OrderID: orderID, TypeID: 34,
			LocationID: 60003760, RegionID: 10000002, IsBuyOrder: 0,
			ListedPrice: 5.5, VolumeTotal: 10, VolumeRemainLast: 0,
			FirstSeenAt: old, LastSeenAt: old,
		}); err != nil {
			t.Fatalf("seed lifecycle %d: %v", orderID, err)
		}
		closedAt := old
		if i >= 600 {
			closedAt = recent
		}
		if err := q.CloseOrderLifecycle(ctx, db.CloseOrderLifecycleParams{
			ClosedAt: closedAt, CloseKind: "filled", CharacterID: fixtureCharA, OrderID: orderID,
		}); err != nil {
			t.Fatalf("close lifecycle %d: %v", orderID, err)
		}
	}
	countAll := func() int {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM order_lifecycle`).Scan(&n); err != nil {
			t.Fatalf("count lifecycle: %v", err)
		}
		return n
	}
	if got := countAll(); got != 605 {
		t.Fatalf("seeded %d rows, want 605", got)
	}

	// One pass through the health worker's prune (no open orders,
	// so the book pass is idle; the prune still runs).
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	afterFirst := countAll()
	// Bounded: at most lifecyclePrunePerCycle deleted, and the
	// 5 recent rows survive, so at least 105 rows remain.
	if afterFirst < 105 || afterFirst > 605-1 {
		t.Fatalf("after first prune: %d rows, want between 105 and 604 (bounded delete)", afterFirst)
	}

	// A second pass removes the rest of the old rows.
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if got := countAll(); got != 5 {
		t.Fatalf("after second prune: %d rows, want 5 recent", got)
	}
	var recentLeft int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM order_lifecycle WHERE closed_at = $1`, recent).Scan(&recentLeft); err != nil {
		t.Fatalf("count recent: %v", err)
	}
	if recentLeft != 5 {
		t.Fatalf("recent rows left: %d, want 5", recentLeft)
	}
}

func TestOrdersPageLifecycleRender(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")

	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18), (587, 'Rifter', 25)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	// Open + history snapshots so the page's other sections render.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrdersHistory, []esi.CharOrderHistoryEntry{})

	now := time.Now().UTC()
	// Closed history for the active character:
	// - Order 11: sell 1000, sold 800 (remain 200 -> ended),
	//   first seen 2026-09-27, closed 2026-09-30 => 3 days.
	// - Order 12: sell 500, fully filled, 2026-09-28 to 2026-09-30 => 2 days.
	// - Order 13: buy 200, fully bought, 2026-09-29 to 2026-09-30 => 1 day.
	// Median filled duration = 1.5 days => "1 day 12 hours".
	// Fill share: 2 filled of 3 closed => 66%.
	// Outbid events: 2 + 1 + 0 = 3.
	seeds := []struct {
		orderID int64
		typeID  int64
		isBuy   int64
		price   float64
		total   int64
		remain  int64
		first   string
		closed  string
		kind    string
		outbid  int64
	}{
		{11, 34, 0, 5.5, 1000, 200, "2026-09-27T00:00:00Z", "2026-09-30T00:00:00Z", "ended", 2},
		{12, 587, 0, 350000, 500, 0, "2026-09-28T00:00:00Z", "2026-09-30T00:00:00Z", "filled", 1},
		{13, 34, 1, 4.25, 200, 0, "2026-09-29T00:00:00Z", "2026-09-30T00:00:00Z", "filled", 0},
	}
	for _, s := range seeds {
		if err := q.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
			CharacterID: fixtureCharA, OrderID: s.orderID, TypeID: s.typeID,
			LocationID: 60003760, RegionID: 10000002, IsBuyOrder: s.isBuy,
			ListedPrice: s.price, VolumeTotal: s.total, VolumeRemainLast: s.remain,
			FirstSeenAt: s.first, LastSeenAt: s.closed,
		}); err != nil {
			t.Fatalf("seed lifecycle %d: %v", s.orderID, err)
		}
		// Upsert resets outbid to 0; set the fixture values directly.
		if _, err := conn.ExecContext(ctx, `UPDATE order_lifecycle SET outbid_events = $1 WHERE character_id = $2 AND order_id = $3`, s.outbid, fixtureCharA, s.orderID); err != nil {
			t.Fatalf("seed outbid %d: %v", s.orderID, err)
		}
		if err := q.CloseOrderLifecycle(ctx, db.CloseOrderLifecycleParams{
			ClosedAt: s.closed, CloseKind: s.kind, CharacterID: fixtureCharA, OrderID: s.orderID,
		}); err != nil {
			t.Fatalf("close lifecycle %d: %v", s.orderID, err)
		}
	}
	// One still-open tracked order (counts toward tracked, not closed).
	if err := q.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
		CharacterID: fixtureCharA, OrderID: 14, TypeID: 34,
		LocationID: 60003760, RegionID: 10000002, IsBuyOrder: 0,
		ListedPrice: 6.0, VolumeTotal: 100, VolumeRemainLast: 100,
		FirstSeenAt: now.Format(time.RFC3339), LastSeenAt: now.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed open lifecycle: %v", err)
	}
	// A closed row for the other character must not leak in.
	if err := q.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
		CharacterID: fixtureCharB, OrderID: 99, TypeID: 34,
		LocationID: 60003760, RegionID: 10000002, IsBuyOrder: 0,
		ListedPrice: 9.9, VolumeTotal: 10, VolumeRemainLast: 0,
		FirstSeenAt: "2026-09-29T00:00:00Z", LastSeenAt: "2026-09-30T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed other lifecycle: %v", err)
	}
	if err := q.CloseOrderLifecycle(ctx, db.CloseOrderLifecycleParams{
		ClosedAt: "2026-09-30T00:00:00Z", CloseKind: "filled", CharacterID: fixtureCharB, OrderID: 99,
	}); err != nil {
		t.Fatalf("close other lifecycle: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/orders/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("orders page: status %d", code)
	}
	mustContain(t, "/orders/", body,
		"Your order history",
		"Orders tracked: 4",
		"66% (2 of 3)",
		"1 day 12 hours",
		"Times beaten at your station: 3",
		"Tritanium",
		"Rifter",
		"Jita 4 - Moon 4 - Caldari Navy Assembly Plant",
		"sold 800 of 1,000",
		"sold 500 of 500",
		"bought 200 of 200",
		"3 days",
		"Ended before filling",
		"Filled",
	)
	if strings.Contains(body, "9.90") {
		t.Error("orders page leaked the other character's lifecycle row")
	}
	// Newest closed first: order 13 and 12 share a close time;
	// order 11 (ended) also closes 09-30 -- just check both item
	// names appear before any leak check above; ordering among
	// equal timestamps is by order id in the handler.
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}

	// Empty state: the second pilot has only the one closed row
	// seeded above, so clear it and re-render.
	if _, err := conn.ExecContext(ctx, `DELETE FROM order_lifecycle WHERE character_id = $1`, fixtureCharB); err != nil {
		t.Fatalf("clear B lifecycle: %v", err)
	}
	seedSnapshot(t, q, fixtureCharB, esi.SnapOrders, []esi.CharOrder{})
	seedSnapshot(t, q, fixtureCharB, esi.SnapOrdersHistory, []esi.CharOrderHistoryEntry{})
	code, body = getPage(t, app, cookie, "/orders/?character=90000002")
	if code != http.StatusOK {
		t.Fatalf("orders page (B): status %d", code)
	}
	mustContain(t, "/orders/ (B)", body,
		"Your order history",
		"No finished orders yet",
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls after empty render, want 0", got)
	}
}
