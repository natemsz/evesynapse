package app

// Hermetic tests for Phase 5 (market history + alerts): change
// math and chart geometry as pure functions, worker warming and
// health computation against a path-routing stub, watchlist CRUD
// through the real router, and the two Home consumers (attention
// feed + Market widget) reading stored state with zero outbound
// calls.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func histRow(date string, avg float64) db.MarketHistory {
	return db.MarketHistory{RegionID: 10000002, TypeID: 34, Date: date, Average: avg, Highest: avg, Lowest: avg, Volume: 1000, OrderCount: 10}
}

func TestHistoryChangePct(t *testing.T) {
	cases := []struct {
		name string
		rows []db.MarketHistory
		days int
		want float64
		ok   bool
	}{
		{"exact week", []db.MarketHistory{histRow("2026-09-24", 100), histRow("2026-09-27", 101), histRow("2026-10-01", 110)}, 7, 10, true},
		{"nearest within tolerance", []db.MarketHistory{histRow("2026-09-25", 100), histRow("2026-10-01", 105)}, 7, 5, true},
		{"gap too wide", []db.MarketHistory{histRow("2026-09-20", 100), histRow("2026-10-01", 110)}, 7, 0, false},
		{"month exact", []db.MarketHistory{histRow("2026-09-01", 100), histRow("2026-10-01", 120)}, 30, 20, true},
		{"month within tolerance", []db.MarketHistory{histRow("2026-08-28", 100), histRow("2026-10-01", 120)}, 30, 20, true},
		{"month gap too wide", []db.MarketHistory{histRow("2026-08-20", 100), histRow("2026-10-01", 120)}, 30, 0, false},
		{"single row", []db.MarketHistory{histRow("2026-10-01", 110)}, 7, 0, false},
		{"zero base", []db.MarketHistory{histRow("2026-09-24", 0), histRow("2026-10-01", 110)}, 7, 0, false},
		{"down move", []db.MarketHistory{histRow("2026-09-24", 100), histRow("2026-10-01", 90)}, 7, -10, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := historyChangePct(tc.rows, tc.days)
			if ok != tc.ok {
				t.Fatalf("ok: got %v, want %v (pct %v)", ok, tc.ok, got)
			}
			if ok && (got-tc.want > 0.001 || tc.want-got > 0.001) {
				t.Fatalf("pct: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildPriceChart(t *testing.T) {
	if _, ok := buildPriceChart(nil); ok {
		t.Fatal("empty rows: got ok, want false")
	}

	flat := []db.MarketHistory{
		histRow("2026-09-29", 10), histRow("2026-09-30", 10), histRow("2026-10-01", 10),
	}
	chart, ok := buildPriceChart(flat)
	if !ok {
		t.Fatal("flat: chart not built")
	}
	if chart.Points == "" {
		t.Fatal("flat: empty polyline")
	}
	y0 := chart.Dots[0].Y
	for _, d := range chart.Dots {
		if d.Y != y0 {
			t.Fatalf("flat: dot y %d, want all %d", d.Y, y0)
		}
		if d.Y <= chart.TopY || d.Y >= chart.BaseY {
			t.Fatalf("flat: dot y %d outside band (%d..%d)", d.Y, chart.TopY, chart.BaseY)
		}
	}
	if len(chart.Bars) != 3 {
		t.Fatalf("flat: got %d volume bars, want 3", len(chart.Bars))
	}

	rising := []db.MarketHistory{histRow("2026-09-30", 5), histRow("2026-10-01", 15)}
	chart, _ = buildPriceChart(rising)
	if chart.Dots[0].Y != chart.BaseY || chart.Dots[1].Y != chart.TopY {
		t.Fatalf("rising: dots at (%d, %d), want (%d, %d)",
			chart.Dots[0].Y, chart.Dots[1].Y, chart.BaseY, chart.TopY)
	}

	single := []db.MarketHistory{histRow("2026-10-01", 42)}
	chart, ok = buildPriceChart(single)
	if !ok || chart.Points != "" || len(chart.Dots) != 1 {
		t.Fatalf("single: ok=%v points=%q dots=%d, want a lone dot", ok, chart.Points, len(chart.Dots))
	}

	noVolume := []db.MarketHistory{
		{RegionID: 10000002, TypeID: 34, Date: "2026-09-30", Average: 5},
		{RegionID: 10000002, TypeID: 34, Date: "2026-10-01", Average: 6},
	}
	chart, _ = buildPriceChart(noVolume)
	if len(chart.Bars) != 0 {
		t.Fatalf("zero volume: got %d bars, want 0", len(chart.Bars))
	}
	if chart.From != "2026-09-30" || chart.To != "2026-10-01" {
		t.Fatalf("labels: %s → %s", chart.From, chart.To)
	}
}

func postForm(t *testing.T, app *Application, cookie *http.Cookie, path string, form url.Values) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestWatchlistCRUD(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, 0, "")

	post := func(form url.Values) {
		t.Helper()
		code, _ := postForm(t, app, cookie, "/market/watch", form)
		if code != http.StatusSeeOther {
			t.Fatalf("watch POST %v: status %d, want 303", form, code)
		}
	}
	count := func() int {
		rows, err := q.ListWatchlistByUser(ctx, user.ID)
		if err != nil {
			t.Fatalf("list watchlist: %v", err)
		}
		return len(rows)
	}

	post(url.Values{"action": {"add"}, "type": {"34"}, "region": {"10000002"}, "threshold": {"7.5"}})
	if count() != 1 {
		t.Fatalf("after add: %d rows, want 1", count())
	}
	post(url.Values{"action": {"add"}, "type": {"34"}, "region": {"10000002"}, "threshold": {"7.5"}})
	if count() != 1 {
		t.Fatalf("after duplicate add: %d rows, want 1", count())
	}
	entry, err := q.GetWatchlistEntry(ctx, db.GetWatchlistEntryParams{UserID: user.ID, TypeID: 34, RegionID: 10000002})
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	if entry.ThresholdPct != 7.5 {
		t.Fatalf("threshold: got %v, want 7.5", entry.ThresholdPct)
	}

	// Clamps: 999 → 50, 0 → 1.
	post(url.Values{"action": {"update"}, "type": {"34"}, "region": {"10000002"}, "threshold": {"999"}})
	entry, _ = q.GetWatchlistEntry(ctx, db.GetWatchlistEntryParams{UserID: user.ID, TypeID: 34, RegionID: 10000002})
	if entry.ThresholdPct != 50 {
		t.Fatalf("clamped high: got %v, want 50", entry.ThresholdPct)
	}
	post(url.Values{"action": {"update"}, "type": {"34"}, "region": {"10000002"}, "threshold": {"0"}})
	entry, _ = q.GetWatchlistEntry(ctx, db.GetWatchlistEntryParams{UserID: user.ID, TypeID: 34, RegionID: 10000002})
	if entry.ThresholdPct != 1 {
		t.Fatalf("clamped low: got %v, want 1", entry.ThresholdPct)
	}

	post(url.Values{"action": {"remove"}, "type": {"34"}, "region": {"10000002"}})
	if count() != 0 {
		t.Fatalf("after remove: %d rows, want 0", count())
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("watch CRUD made %d outbound calls, want 0", got)
	}
}

// marketStubTransport answers the public market endpoints with two
// days of history and empty order books, counting calls.
type marketStubTransport struct {
	calls      atomic.Int64
	history429 atomic.Bool
}

func (s *marketStubTransport) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (s *marketStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	path := req.URL.Path
	switch {
	case strings.Contains(path, "/history/"):
		if s.history429.Load() {
			return s.respond(429, `{"error":"error limited"}`), nil
		}
		return s.respond(200, `[
			{"date":"2026-09-30","average":10.0,"highest":11.0,"lowest":9.0,"volume":123456,"order_count":42},
			{"date":"2026-10-01","average":10.5,"highest":11.5,"lowest":9.5,"volume":100000,"order_count":40}
		]`), nil
	case path == "/markets/prices/":
		return s.respond(200, `[{"type_id":34,"average_price":10.5,"adjusted_price":9.8},{"type_id":35,"average_price":20.0,"adjusted_price":19.0}]`), nil
	case strings.Contains(path, "/orders/"):
		return s.respond(200, `[]`), nil
	default:
		return s.respond(404, `{"error":"unexpected path `+path+`"}`), nil
	}
}

func TestMarketWorkerHistoryDrainAndGate(t *testing.T) {
	transport := &marketStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// 12 watched types: one cycle drains 10, the next the rest.
	for i := int64(0); i < 12; i++ {
		if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
			UserID: user.ID, TypeID: 1000 + i, RegionID: 10000002,
			ThresholdPct: 5, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed watchlist: %v", err)
		}
	}
	allowance := &fetchBudget{left: 120}

	stored, limited := app.warmMarketHistory(ctx, allowance)
	if limited {
		t.Fatal("history pass reported the error limit on a healthy stub")
	}
	if stored != 10 {
		t.Fatalf("first pass stored %d histories, want 10 (drain cap)", stored)
	}
	if got := transport.calls.Load(); got != 10 {
		t.Fatalf("first pass made %d calls, want 10", got)
	}
	rows, err := q.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 1000, RowLimit: 90})
	if err != nil || len(rows) != 2 {
		t.Fatalf("stored history for type 1000: rows=%d err=%v, want 2", len(rows), err)
	}
	state, err := q.GetMarketFetchState(ctx, "history_10000002_1000")
	if err != nil || state.State != fetchStateOK {
		t.Fatalf("fetch state: state=%v err=%v, want ok", state.State, err)
	}

	// Second pass: only the two unfetched pairs go out (the 20h
	// gate holds the first ten).
	stored, _ = app.warmMarketHistory(ctx, allowance)
	if stored != 2 {
		t.Fatalf("second pass stored %d, want 2", stored)
	}
	if got := transport.calls.Load(); got != 12 {
		t.Fatalf("after second pass: %d calls, want 12", got)
	}

	// Third pass: everything gated, nothing fetched.
	stored, _ = app.warmMarketHistory(ctx, allowance)
	if stored != 0 || transport.calls.Load() != 12 {
		t.Fatalf("third pass: stored %d calls %d, want 0/12", stored, transport.calls.Load())
	}
}

func TestMarketWorkerHistoryErrorLimit(t *testing.T) {
	transport := &marketStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
		UserID: user.ID, TypeID: 34, RegionID: 10000002,
		ThresholdPct: 5, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed watchlist: %v", err)
	}
	transport.history429.Store(true)
	_, limited := app.warmMarketHistory(ctx, &fetchBudget{left: 120})
	if !limited {
		t.Fatal("429 history fetch: got limited=false, want true")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("error-limit pass made %d calls, want 1", got)
	}
}

func TestComputeOrderHealth(t *testing.T) {
	mine := esi.CharOrder{OrderID: 1, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 10}
	sell := func(id, loc int64, price float64) esi.MarketOrder {
		return esi.MarketOrder{OrderID: id, TypeID: 34, LocationID: loc, SystemID: 30000142, Price: price, VolumeRemain: 100}
	}
	cases := []struct {
		name       string
		book       []esi.MarketOrder
		wantStatus string
	}{
		{"alone at best", []esi.MarketOrder{sell(1, 60003760, 10)}, "best"},
		{"tied at station", []esi.MarketOrder{sell(1, 60003760, 10), sell(2, 60003760, 10)}, "best"},
		{"undercut at station", []esi.MarketOrder{sell(1, 60003760, 10), sell(2, 60003760, 9.50)}, "undercut_station"},
		{"a hair cheaper is not an undercut", []esi.MarketOrder{sell(1, 60003760, 10), sell(2, 60003760, 9.995)}, "best"},
		{"thin station, cheaper in region", []esi.MarketOrder{sell(1, 60003760, 10), sell(3, 60003761, 9.00)}, "undercut_region"},
		{"busy station, cheaper in region", []esi.MarketOrder{sell(1, 60003760, 10), sell(2, 60003760, 10.5), sell(3, 60003761, 9.00)}, "best_region_cheaper"},
		{"empty book", nil, "best"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, _ := computeOrderHealth(mine, tc.book)
			if status != tc.wantStatus {
				t.Fatalf("status: got %q, want %q", status, tc.wantStatus)
			}
		})
	}
	// Recorded prices: station best excludes nothing, region best
	// is the floor of the whole book.
	_, stationBest, regionBest := computeOrderHealth(mine, []esi.MarketOrder{
		sell(1, 60003760, 10), sell(2, 60003760, 9.50), sell(3, 60003761, 9.00),
	})
	if stationBest != 9.50 || regionBest != 9.00 {
		t.Fatalf("bests: station %v region %v, want 9.5/9", stationBest, regionBest)
	}
}

// bookStubTransport serves one fixed regional book.
type bookStubTransport struct{ calls atomic.Int64 }

func (s *bookStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	body := `[]`
	if strings.Contains(req.URL.Path, "/orders/") {
		body = `[
			{"order_id":1,"type_id":34,"location_id":60003760,"system_id":30000142,"is_buy_order":false,"price":10.0,"volume_remain":5,"volume_total":5},
			{"order_id":999,"type_id":34,"location_id":60003760,"system_id":30000142,"is_buy_order":false,"price":9.5,"volume_remain":100,"volume_total":100}
		]`
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

func TestOrderHealthWorkerAndPrune(t *testing.T) {
	transport := &bookStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 1, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 10, VolumeRemain: 5, VolumeTotal: 5},
	})

	stored, limited := app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if limited || stored != 1 {
		t.Fatalf("health pass: stored=%d limited=%v, want 1/false", stored, limited)
	}
	rows, err := q.ListOrderHealthByCharacter(ctx, fixtureCharA)
	if err != nil || len(rows) != 1 {
		t.Fatalf("health rows: %d err=%v, want 1", len(rows), err)
	}
	if rows[0].Status != "undercut_station" || rows[0].StationBest != 9.5 {
		t.Fatalf("health row: status %q station best %v, want undercut_station/9.5", rows[0].Status, rows[0].StationBest)
	}

	// The order fills: the next pass prunes the verdict (the book
	// itself is gated, so no new fetch is needed to prove it).
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{})
	app.refreshOrderHealth(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	rows, err = q.ListOrderHealthByCharacter(ctx, fixtureCharA)
	if err != nil || len(rows) != 0 {
		t.Fatalf("after close: %d health rows, want 0", len(rows))
	}
}

// seedMarketSignals plants one of everything the market consumers
// read: an undercut verdict and a watched type up 8% in a week.
func seedMarketSignals(t *testing.T, app *Application, q *db.Queries, userID int64, undercutOrders int) {
	t.Helper()
	ctx := context.Background()
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < undercutOrders; i++ {
		if err := q.UpsertOrderHealth(ctx, db.UpsertOrderHealthParams{
			CharacterID: fixtureCharA, OrderID: int64(700 + i), TypeID: 34,
			RegionID: 10000002, LocationID: 60003760, MyPrice: 10,
			StationBest: 9.5, RegionBest: 9.5, Status: "undercut_station",
			ComputedAt: now,
		}); err != nil {
			t.Fatalf("seed health: %v", err)
		}
	}
	// 7 days ago at 10.00, today at 10.80 → +8.0% over 7 days.
	for _, d := range []struct {
		ago int
		avg float64
	}{{7, 10.0}, {3, 10.4}, {0, 10.8}} {
		day := now.AddDate(0, 0, -d.ago).Format(historyDateLayout)
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 10000002, TypeID: 34, Date: day, Average: d.avg,
			Highest: d.avg, Lowest: d.avg, Volume: 5000, OrderCount: 90,
		}); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
		UserID: userID, TypeID: 34, RegionID: 10000002,
		ThresholdPct: 5, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed watchlist: %v", err)
	}
}

func TestHomeAttentionMarketRules(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	// A ready industry job (rank 2) so the market rules (ranks 6/7)
	// provably sort after the character rules.
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 587, Name: "Rifter"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 11, ActivityID: 1, Status: "ready", ProductTypeID: 587, BlueprintTypeID: 691,
			EndDate: rfc(now.Add(-time.Hour)), CompletedDate: rfc(now.Add(-time.Hour))},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 700, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 10,
			VolumeRemain: 5, VolumeTotal: 5, Issued: rfc(now.Add(-time.Hour)), Duration: 90},
	})
	seedMarketSignals(t, app, q, user.ID, 1)

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	mustContain(t, "/", body,
		"Fixture Alpha — Tritanium sell order: undercut by 0.50 ISK (5.0%) at this station.",
		"Tritanium up 8.0% over 7 days in The Forge.",
		"1 order undercut · watchlist: 1 moving",
	)
	// Scope the order check to the attention feed: the Briefing
	// above it reports the same market moves in its own order.
	attStart := strings.Index(body, "<h3>Needs attention</h3>")
	attBody := body[attStart:]
	jobAt := strings.Index(attBody, "job ready for delivery")
	undercutAt := strings.Index(attBody, "sell order: undercut")
	moveAt := strings.Index(attBody, "over 7 days in The Forge")
	if !(jobAt >= 0 && jobAt < undercutAt && undercutAt < moveAt) {
		t.Fatalf("attention order: job %d, undercut %d, move %d — want job < undercut < move", jobAt, undercutAt, moveAt)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home render made %d outbound calls, want 0", got)
	}
}

func TestHomeAttentionMarketAggregation(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	var orders []esi.CharOrder
	for i := 0; i < 4; i++ {
		orders = append(orders, esi.CharOrder{
			OrderID: int64(700 + i), TypeID: 34, LocationID: 60003760, RegionID: 10000002,
			Price: 10, VolumeRemain: 5, VolumeTotal: 5, Issued: rfc(now.Add(-time.Hour)), Duration: 90,
		})
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, orders)
	seedMarketSignals(t, app, q, user.ID, 4)

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	_, body := getPage(t, app, cookie, "/")
	mustContain(t, "/", body, "4 of your sell orders are undercut right now.")
	if strings.Contains(body, "sell order: undercut by") {
		t.Fatal("aggregated attention still lists per-order undercut lines")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home render made %d outbound calls, want 0", got)
	}
}

func TestMarketPageSectionsZeroOutbound(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 700, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 10,
			VolumeRemain: 5, VolumeTotal: 5, Issued: rfc(now.Add(-time.Hour)), Duration: 90},
	})
	seedMarketSignals(t, app, q, user.ID, 1)

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/")
	if code != http.StatusOK {
		t.Fatalf("market: status %d", code)
	}
	mustContain(t, "/market/", body,
		"Watchlist", "Tritanium", "&#43;8.0%", "up 8.0% over 7 days",
		"Your orders", "Undercut by 0.50 ISK (5.0%) at this station",
	)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("market render made %d outbound calls, want 0", got)
	}
}

func TestMarketItemPageChartAndWant(t *testing.T) {
	transport := &marketStubTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	if _, err := conn.Exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 1, 1, 1)`); err != nil {
		t.Fatalf("seed SDE type: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// No history yet: the chart says it's loading and the view
	// leaves a want for the worker.
	code, body := getPage(t, app, cookie, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("item page: status %d", code)
	}
	mustContain(t, "/market/?type=34 (no history)", body, "This one's queued")
	wants, err := q.ListMarketHistoryWants(ctx, time.Time{})
	if err != nil || len(wants) != 1 || wants[0].TypeID != 34 {
		t.Fatalf("wants after view: %v err=%v, want one row for type 34", wants, err)
	}

	// With rows stored, the chart renders and no new want is left.
	seedMarketSignals(t, app, q, user.ID, 0)
	code, body = getPage(t, app, cookie, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("item page: status %d", code)
	}
	mustContain(t, "/market/?type=34 (with history)", body,
		"<svg", "<polyline", "Price history — The Forge", "&#43;8.0%",
	)
	if strings.Contains(body, "This one's queued") {
		t.Fatal("item page with history still shows the loading state")
	}
	wants, err = q.ListMarketHistoryWants(ctx, time.Time{})
	if err != nil || len(wants) != 1 {
		t.Fatalf("wants after charted view: %v err=%v, want the original single want", wants, err)
	}
}

func TestMigration013Reopen(t *testing.T) {
	transport := &countingTransport{}
	_, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	// The schema guard already ran once via store.Open; every 013
	// table answers queries on a fresh database.
	if _, err := q.ListAllWatchlistEntries(ctx); err != nil {
		t.Fatalf("watchlist on fresh DB: %v", err)
	}
	if _, err := q.ListMarketHistoryWants(ctx, time.Time{}); err != nil {
		t.Fatalf("wants on fresh DB: %v", err)
	}
	if _, err := q.ListOrderHealthByUser(ctx, 1); err != nil {
		t.Fatalf("order health on fresh DB: %v", err)
	}
	var tables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('market_history','market_history_wants','market_fetch_state','market_watchlist','order_health')`).Scan(&tables); err != nil {
		t.Fatalf("count 013 tables: %v", err)
	}
	if tables != 5 {
		t.Fatalf("013 tables: got %d, want 5", tables)
	}
}
