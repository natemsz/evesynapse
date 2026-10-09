package app

// Hermetic tests for per-widget configuration (schema
// 020), the orders widget's character/tag scope and merge modes,
// the Home watchlist module, the stored market guide (schema
// 021) behind always-on asset valuation, and the guide-price
// worker refresh. Same contract as the other home suites: seeded
// stored state drives the real router against a counting
// transport, and handlers must make zero outbound calls.

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
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

// seedWidgetOrders plants open orders for one character.
func seedWidgetOrders(t *testing.T, q *db.Queries, characterID int64, orders []esi.CharOrder) {
	t.Helper()
	if err := q.UpsertTypeName(context.Background(), db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	seedSnapshot(t, q, characterID, esi.SnapOrders, orders)
}

func setHomeLayout(t *testing.T, q *db.Queries, userID int64, layout string) {
	t.Helper()
	if err := q.SetUserHomeLayout(context.Background(), db.SetUserHomeLayoutParams{
		HomeLayout: layout, ID: userID,
	}); err != nil {
		t.Fatalf("set layout: %v", err)
	}
}

func postWidgetConfig(t *testing.T, app *Application, cookie *http.Cookie, form url.Values) int {
	t.Helper()
	code, _, _ := doReq(t, app, http.MethodPost, "/home/widget-config", form, cookie)
	return code
}

// TestOrdersWidgetScopeAndMerge: the Market orders widget reads
// its scope + merge mode from the stored widget config —
// defaults (all/both) out of the box, then character scope, tag
// scope in both merge modes, an invalid scope falling back to
// all, and the config surviving layout moves.
func TestOrdersWidgetScopeAndMerge(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedOverviewCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "Trader")
	seedOverviewCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo", "Trader")
	seedOverviewCharacter(t, q, user.ID, 90000003, "Fixture Charlie", "")

	seedWidgetOrders(t, q, fixtureCharA, []esi.CharOrder{
		{OrderID: 1, TypeID: 34, IsBuyOrder: false, Price: 10, VolumeRemain: 5, VolumeTotal: 5,
			Issued: rfc(now.Add(-time.Hour)), Duration: 90},
	})
	seedWidgetOrders(t, q, fixtureCharB, []esi.CharOrder{
		{OrderID: 2, TypeID: 34, IsBuyOrder: true, Price: 5, VolumeRemain: 10, VolumeTotal: 10,
			Escrow: 432, Issued: rfc(now.Add(-time.Hour)), Duration: 90},
	})
	seedWidgetOrders(t, q, 90000003, []esi.CharOrder{
		{OrderID: 3, TypeID: 34, IsBuyOrder: false, Price: 100, VolumeRemain: 3, VolumeTotal: 3,
			Issued: rfc(now), Duration: 1},
	})
	setHomeLayout(t, q, user.ID, `[{"id":"market"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// Default: all characters, totals + expiring rows.
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		`action="/home/widget-config"`, `name="scope"`,
		"<strong>3</strong> open orders",
		"2 sell (350.00 ISK)", "1 buy (432.00 ISK in escrow)",
	)
	// The tag shows up as a scope choice.
	code, body = getPage(t, app, cookie, "/?customize=1")
	if code != http.StatusOK {
		t.Fatalf("GET /?customize=1: status %d", code)
	}
	mustContain(t, "/?customize=1", body, `value="tag:Trader"`, `value="char:90000001"`)

	stored := func() ordersWidgetConfig {
		t.Helper()
		blob, err := q.GetWidgetConfig(ctx, db.GetWidgetConfigParams{UserID: user.ID, WidgetID: widgetMarket})
		if err != nil {
			t.Fatalf("read widget config: %v", err)
		}
		return parseOrdersWidgetConfig(blob)
	}

	// Character scope: Alpha only (sell 5×10 = 50.00).
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"widget": {widgetMarket}, "scope": {"char:90000001"}, "merge": {mergeBoth}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("config POST: status %d, want 303", code)
	}
	if cfg := stored(); cfg.ScopeType != scopeCharacter || cfg.CharacterID != fixtureCharA {
		t.Fatalf("stored config = %+v, want character scope on Alpha", cfg)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (char scope): status %d", code)
	}
	mustContain(t, "/ (char scope)", body,
		"<strong>1</strong> open orders", "1 sell (50.00 ISK)",
		`value="char:90000001" selected`,
	)
	for _, gone := range []string{"432.00", "300.00", "1 buy"} {
		if strings.Contains(body, gone) {
			t.Errorf("character scope leaked %q from another character", gone)
		}
	}

	// Tag scope, per-character rows: Alpha + Bravo rows, no
	// combined totals line, Charlie (untagged) out.
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"widget": {widgetMarket}, "scope": {"tag:Trader"}, "merge": {mergePerCharacter}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("tag config POST: status %d, want 303", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (tag rows): status %d", code)
	}
	mustContain(t, "/ (tag rows)", body,
		"1 sell · 50.00 ISK", "1 buy · 432.00 ISK in escrow",
		`value="tag:Trader" selected`,
	)
	if strings.Contains(body, "<strong>2</strong> open orders") {
		t.Error("per-character mode rendered the combined totals line")
	}
	if strings.Contains(body, "300.00") {
		t.Error("tag scope leaked untagged Charlie's sell value")
	}

	// Same tag, combined: totals back, per-character rows gone.
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"widget": {widgetMarket}, "scope": {"tag:Trader"}, "merge": {mergeCombined}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("combined config POST: status %d, want 303", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (tag combined): status %d", code)
	}
	mustContain(t, "/ (tag combined)", body,
		"<strong>2</strong> open orders", "1 sell (50.00 ISK)", "1 buy (432.00 ISK in escrow)",
	)
	if strings.Contains(body, "1 sell · 50.00 ISK") {
		t.Error("combined mode rendered per-character rows")
	}

	// The config survives the module leaving and returning:
	// set per-character once more, toggle off, toggle on.
	postWidgetConfig(t, app, cookie, url.Values{
		"widget": {widgetMarket}, "scope": {"tag:Trader"}, "merge": {mergePerCharacter}, "next": {"/"},
	})
	for i := 0; i < 2; i++ {
		code, _, _ := doReq(t, app, http.MethodPost, "/home/layout",
			url.Values{"action": {"toggle"}, "widget": {widgetMarket}}, cookie)
		if code != http.StatusSeeOther {
			t.Fatalf("layout toggle %d: status %d", i, code)
		}
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (after toggles): status %d", code)
	}
	mustContain(t, "/ (after toggles)", body, "1 sell · 50.00 ISK")
	if cfg := stored(); cfg.ScopeType != scopeTag || cfg.Merge != mergePerCharacter {
		t.Errorf("config after toggles = %+v, want tag/per-character", cfg)
	}

	// A scope naming a character that isn't linked falls back
	// to all characters rather than stranding the widget.
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"widget": {widgetMarket}, "scope": {"char:42424299"}, "merge": {mergeBoth}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("invalid scope POST: status %d, want 303", code)
	}
	if cfg := stored(); cfg.ScopeType != scopeAll {
		t.Errorf("invalid-scope config = %+v, want all fallback", cfg)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (fallback): status %d", code)
	}
	mustContain(t, "/ (fallback)", body, "<strong>3</strong> open orders")

	// XHR callers get the bare 200; anonymous ones bounce.
	req := httptest.NewRequest(http.MethodPost, "/home/widget-config",
		strings.NewReader(url.Values{
			"widget": {widgetMarket}, "scope": {"all"}, "merge": {mergeBoth},
		}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Errorf("XHR config POST: status %d body %q, want 200 ok", rec.Code, rec.Body.String())
	}
	code, _, _ = doReq(t, app, http.MethodPost, "/home/widget-config",
		url.Values{"widget": {widgetMarket}, "scope": {"all"}})
	if code != http.StatusSeeOther {
		t.Errorf("anonymous config POST: status %d, want 303", code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestWidgetConfigParsing: stored blobs normalize to defaults
// when missing, corrupt, or half-valid.
func TestWidgetConfigParsing(t *testing.T) {
	if cfg := parseOrdersWidgetConfig(""); cfg.ScopeType != scopeAll || cfg.Merge != mergeBoth {
		t.Errorf("empty blob = %+v, want defaults", cfg)
	}
	if cfg := parseOrdersWidgetConfig("{not json"); cfg.ScopeType != scopeAll || cfg.Merge != mergeBoth {
		t.Errorf("junk blob = %+v, want defaults", cfg)
	}
	if cfg := parseOrdersWidgetConfig(`{"scope_type":"character","character_id":0,"merge":"huge"}`); cfg.ScopeType != scopeAll || cfg.Merge != mergeBoth {
		t.Errorf("half-valid blob = %+v, want defaults", cfg)
	}
	cfg := parseOrdersWidgetConfig(`{"scope_type":"tag","tag":"Trader","merge":"combined"}`)
	if cfg.ScopeType != scopeTag || cfg.Tag != "Trader" || cfg.Merge != mergeCombined {
		t.Errorf("tag blob = %+v, want tag/Trader/combined", cfg)
	}
	if got := decodeScopeValue("char:123").CharacterID; got != 123 {
		t.Errorf("decode char:123 → %d", got)
	}
	if got := decodeScopeValue("tag:PI alt"); got.Tag != "PI alt" {
		t.Errorf("decode tag → %q", got.Tag)
	}
	if got := decodeScopeValue("gibberish"); got.ScopeType != scopeAll {
		t.Errorf("decode gibberish → %+v, want all", got)
	}
}

// TestWatchlistWidgetHome: the Home watchlist module renders the
// stored rows (price, 7d move, breach flag), links items to
// their details page, and speaks plainly when empty.
func TestWatchlistWidgetHome(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	setHomeLayout(t, q, user.ID, `[{"id":"watchlist"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Market watchlist", "Nothing watched yet")

	seedMarketSignals(t, app, q, user.ID, 0) // history + Tritanium watch at 5%
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 35, Name: "Pyroxeres"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
		UserID: user.ID, TypeID: 35, RegionID: 10000002,
		ThresholdPct: 5, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed watch: %v", err)
	}

	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (watched): status %d", code)
	}
	mustContain(t, "/ (watched)", body,
		`<a href="/items/type/34/">Tritanium</a>`,
		"10.80", "up 8.0% over 7 days", // +8% over the threshold: moving
		"Pyroxeres", "Calm", // no history yet: quiet, not blank
	)

	// The module is in the Customize catalog.
	code, body = getPage(t, app, cookie, "/?customize=1")
	if code != http.StatusOK {
		t.Fatalf("GET /?customize=1: status %d", code)
	}
	mustContain(t, "/?customize=1", body, `data-add-widget="watchlist"`, "Market watchlist")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestNetWorthGuidePrices: with only the worker-stored guide
// present (no Market visit, cold in-memory cache) the Net worth
// card shows a real assets number with honest coverage — never
// "prices not loaded yet" — and the daily sampler's net worth
// rides the same fallback.
func TestNetWorthGuidePrices(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1000.5)
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 31, TypeID: 34, Quantity: 100},
		{ItemID: 32, TypeID: 999999, Quantity: 7},
	})
	setHomeLayout(t, q, user.ID, `[{"id":"networth"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// No guide anywhere yet: still a real number (0 priced),
	// with the coverage spelled out — and never the old copy.
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Net worth", "1,000.50", "0 of 2 items priced")
	if strings.Contains(body, "prices not loaded yet") {
		t.Error("net worth fell back to the old not-loaded copy")
	}

	// The worker-stored guide lands: Tritanium prices at 5.00.
	if err := q.UpsertGuidePrice(ctx, db.UpsertGuidePriceParams{
		TypeID: 34, AdjustedPrice: 4, AveragePrice: 5,
	}); err != nil {
		t.Fatalf("seed guide price: %v", err)
	}
	if err := q.UpsertGuidePricesMeta(ctx, db.UpsertGuidePricesMetaParams{
		FetchedAt: time.Now().UTC(), CachedUntil: mustTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed guide meta: %v", err)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (guided): status %d", code)
	}
	mustContain(t, "/ (guided)", body,
		"500.00",   // 100 × 5.00 of assets
		"1,500.50", // wallet + assets total
		"1 of 2 items priced",
	)

	// The sampler's net worth uses the same stored guide.
	app.sampleWalletHistory(ctx, ch, time.Now())
	rows, err := q.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: user.ID, CharacterID: fixtureCharA,
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("samples: %v (%d rows)", err, len(rows))
	}
	if !rows[0].NetWorth.Valid || rows[0].NetWorth.Float64 != 1500.5 {
		t.Errorf("sample net worth = %+v, want 1500.5", rows[0].NetWorth)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// pricesTransport serves the price guide once, counting calls.
type pricesTransport struct {
	calls atomic.Int64
	body  string
}

func (s *pricesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	header := make(http.Header)
	if req.URL.Path == "/markets/prices/" {
		header.Set("Expires", time.Now().Add(2*time.Hour).UTC().Format(http.TimeFormat))
		return &http.Response{
			StatusCode: http.StatusOK, Header: header,
			Body: io.NopCloser(strings.NewReader(s.body)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError, Header: header,
		Body: io.NopCloser(strings.NewReader(`{"error":"fixture"}`)),
	}, nil
}

// TestGuidePricesWorkerRefresh: the worker mirrors the guide
// into the table, honors the ESI cache window on the next call,
// and seeds the live in-memory guide from the same payload.
func TestGuidePricesWorkerRefresh(t *testing.T) {
	transport := &pricesTransport{body: `[
		{"type_id":34,"average_price":5.5,"adjusted_price":4.5},
		{"type_id":35,"average_price":9,"adjusted_price":8}
	]`}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	stored, limited := app.refreshGuidePrices(ctx)
	if limited || !stored {
		t.Fatalf("first refresh = (stored %v, limited %v), want (true, false)", stored, limited)
	}
	rows, err := q.ListGuidePrices(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("guide rows: %v (%d)", err, len(rows))
	}
	if rows[0].TypeID != 34 || rows[0].AveragePrice != 5.5 {
		t.Errorf("guide row 0 = %+v, want Tritanium at 5.5", rows[0])
	}
	if app.cachedPrices() == nil || app.cachedPrices()[34].AveragePrice != 5.5 {
		t.Errorf("live guide not seeded from the refresh: %v", app.cachedPrices())
	}

	// Inside the cache window: no second fetch.
	stored, limited = app.refreshGuidePrices(ctx)
	if limited || stored {
		t.Fatalf("second refresh = (stored %v, limited %v), want (false, false)", stored, limited)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("guide fetches = %d, want exactly 1", got)
	}

	// With the in-memory copies cleared, the stored table still
	// answers valuation reads.
	app.prices = nil
	app.storedPricesCache = nil
	if got := app.storedGuidePrices(ctx); got == nil || got[35].AveragePrice != 9 {
		t.Errorf("stored guide read = %v, want type 35 at 9", got)
	}
}

// TestMigrations020And021Reopen: the new tables' guards are
// idempotent across reopens, like every schema before them.
func TestMigrations020And021Reopen(t *testing.T) {
	dsn := pgtest.FreshDSN(t)
	for i := 0; i < 2; i++ {
		conn, pool, err := store.Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		var tables int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('widget_configs','guide_prices','guide_prices_meta')`).Scan(&tables); err != nil {
			t.Fatalf("count tables: %v", err)
		}
		if tables != 3 {
			t.Fatalf("tables after open %d: %d, want 3", i, tables)
		}
		conn.Close()
		pool.Close()
	}

	// A user with no saved config reads the defaults.
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if got := app.ordersConfigFor(context.Background(), user.ID); got.ScopeType != scopeAll || got.Merge != mergeBoth {
		t.Errorf("fresh-user config = %+v, want defaults", got)
	}
}
