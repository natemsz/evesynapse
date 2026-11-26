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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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

// TestOrdersWidgetScopeAndMerge: the Market widget's settings sheet
// (scope and show) saves through /home/widget-config and the card
// draws by it: all characters both ways out of the box, then a
// character scope, a tag scope read per character and combined, the
// settings surviving layout moves, and a scope that names a character
// not on the account, saved or stored, reading as all characters.
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
	setHomeLayout(t, q, user.ID, `[{"id":"market","w":2,"h":2}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// Default: all characters, totals + a row per character.
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		`title="3 open orders"`,
		"2 sell (350.00 ISK)", "1 buy (432.00 ISK in escrow)",
		"1 sell · 50.00 ISK",
	)
	// Customize carries the sheet: every character and every tag in
	// use are scope choices.
	code, body = getPage(t, app, cookie, "/?customize=1")
	if code != http.StatusOK {
		t.Fatalf("GET /?customize=1: status %d", code)
	}
	mustContain(t, "/?customize=1", body, `action="/home/widget-config"`, `name="key" value="market"`,
		`name="scope"`, `value="tag:Trader"`, `value="char:90000001"`, `name="show"`)

	stored := func() map[string]string {
		t.Helper()
		blob, err := q.GetWidgetConfig(ctx, db.GetWidgetConfigParams{UserID: user.ID, WidgetID: widgetMarket})
		if err != nil {
			t.Fatalf("read widget config: %v", err)
		}
		var cfg map[string]string
		if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
			t.Fatalf("decode widget config %q: %v", blob, err)
		}
		return cfg
	}

	// Character scope: Alpha only (sell 5×10 = 50.00).
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"key": {widgetMarket}, "scope": {"char:90000001"}, "show": {mergeBoth}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("config POST: status %d, want 303", code)
	}
	if cfg := stored(); cfg["scope"] != "char:90000001" {
		t.Fatalf("stored config = %v, want character scope on Alpha", cfg)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (char scope): status %d", code)
	}
	mustContain(t, "/ (char scope)", body,
		`title="1 open orders"`, "1 sell (50.00 ISK)",
		"Market · Fixture Alpha",
	)
	for _, gone := range []string{"432.00", "300.00", "1 buy"} {
		if strings.Contains(body, gone) {
			t.Errorf("character scope leaked %q from another character", gone)
		}
	}

	// Tag scope, per-character rows: Alpha + Bravo rows, no
	// combined totals tiles, Charlie (untagged) out.
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"key": {widgetMarket}, "scope": {"tag:Trader"}, "show": {mergePerCharacter}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("tag config POST: status %d, want 303", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (tag rows): status %d", code)
	}
	mustContain(t, "/ (tag rows)", body,
		"1 sell · 50.00 ISK", "1 buy · 432.00 ISK in escrow",
		"Market · Tag: Trader",
	)
	if strings.Contains(body, "1 sell (50.00 ISK)") {
		t.Error("per-character mode rendered the combined totals tiles")
	}
	if strings.Contains(body, "300.00") {
		t.Error("tag scope leaked untagged Charlie's sell value")
	}

	// Same tag, combined: totals back, per-character rows gone.
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"key": {widgetMarket}, "scope": {"tag:Trader"}, "show": {mergeCombined}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("combined config POST: status %d, want 303", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (tag combined): status %d", code)
	}
	mustContain(t, "/ (tag combined)", body,
		`title="2 open orders"`, "1 sell (50.00 ISK)", "1 buy (432.00 ISK in escrow)",
	)
	if strings.Contains(body, "1 sell · 50.00 ISK") {
		t.Error("combined mode rendered per-character rows")
	}

	// The config survives the module leaving and returning:
	// set per-character once more, toggle off, toggle on.
	postWidgetConfig(t, app, cookie, url.Values{
		"key": {widgetMarket}, "scope": {"tag:Trader"}, "show": {mergePerCharacter}, "next": {"/"},
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
	mustContain(t, "/ (after toggles)", body, "Market · Tag: Trader")
	if cfg := stored(); cfg["scope"] != "tag:Trader" || cfg["show"] != mergePerCharacter {
		t.Errorf("config after toggles = %v, want tag/per-character", cfg)
	}

	// A form naming a character that isn't linked is refused, and
	// the stored settings stand: the widget is never stranded.
	postWidgetConfig(t, app, cookie, url.Values{
		"key": {widgetMarket}, "scope": {scopeAll}, "show": {mergeBoth}, "next": {"/"},
	})
	if code := postWidgetConfig(t, app, cookie, url.Values{
		"key": {widgetMarket}, "scope": {"char:42424299"}, "show": {mergePerCharacter}, "next": {"/"},
	}); code != http.StatusSeeOther {
		t.Fatalf("invalid scope POST: status %d, want 303", code)
	}
	if cfg := stored(); cfg["scope"] != scopeAll || cfg["show"] != mergeBoth {
		t.Errorf("config after a refused POST = %v, want the stored all/both", cfg)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (fallback): status %d", code)
	}
	mustContain(t, "/ (fallback)", body, `title="3 open orders"`)

	// A stored scope whose character has left the account reads as
	// all characters too.
	if err := q.UpsertWidgetConfig(ctx, db.UpsertWidgetConfigParams{
		UserID: user.ID, WidgetID: widgetMarket, Config: `{"scope":"char:42424299","show":"both"}`, UpdatedAt: now.UTC(),
	}); err != nil {
		t.Fatalf("store stale scope: %v", err)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (stale scope): status %d", code)
	}
	mustContain(t, "/ (stale scope)", body, `title="3 open orders"`)
	if strings.Contains(body, "Market · ") {
		t.Error("a stale scope still named itself in the card title")
	}

	// XHR callers get the bare 200; anonymous ones bounce.
	req := httptest.NewRequest(http.MethodPost, "/home/widget-config",
		strings.NewReader(url.Values{
			"key": {widgetMarket}, "scope": {"all"}, "show": {mergeBoth},
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
		url.Values{"key": {widgetMarket}, "scope": {"all"}})
	if code != http.StatusSeeOther {
		t.Errorf("anonymous config POST: status %d, want 303", code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
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
	setHomeLayout(t, q, user.ID, `[{"id":"watchlist","w":2,"h":2}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Market Watchlist", "Nothing watched yet")

	seedMarketSignals(t, q, user.ID, 0) // history + Tritanium watch at 5%
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
	mustContain(t, "/?customize=1", body, `data-add-widget="watchlist"`, "Market Watchlist")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestNetWorthHousePrices: net worth prices assets in the house
// order, The Forge's swept typical sells, then stored estimated
// sells. The guide's average prices nothing (my ruling,
// 2026-10-17): with only the guide present the card shows a
// real assets number with honest coverage, never "prices not
// loaded yet", and the daily sampler's net worth rides the
// same sources.
func TestNetWorthHousePrices(t *testing.T) {
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

	// No price source anywhere yet: still a real number
	// (0 priced), with the coverage spelled out.
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Net worth", "1,000.50", "0 of 2 items priced")

	// The worker-stored guide lands, Tritanium's average at
	// 5.00: the average is banned as a price source, so the
	// stack still prices nothing.
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
		t.Fatalf("GET / (guide only): status %d", code)
	}
	mustContain(t, "/ (guide only)", body, "1,000.50", "0 of 2 items priced")

	// The Forge sweep lands: Tritanium prices at its typical
	// sell of 5.00.
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 34, TypicalSell: 5, AvgDailyVolume: 1_000_000,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}
	// The sweep landing mid-test: the cache window lapses.
	app.forgeTypicalsMu.Lock()
	app.forgeTypicalsCache = nil
	app.forgeTypicalsMu.Unlock()
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (swept): status %d", code)
	}
	mustContain(t, "/ (swept)", body,
		"500.00",   // 100 × 5.00 of assets
		"1,500.50", // wallet + assets total
		"1 of 2 items priced",
	)

	// The sampler's net worth uses the same house sources.
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

// TestFitPricesFollowStoredPrices: a saved fit is priced when it
// is saved, and each price refresh reprices the stored fits in the
// house order: hull
// plus modules at The Forge's swept typical sells, the same sum
// the save handler makes. The guide fixture below carries decoy
// adjusted prices, if anything priced from the guide, the
// totals would change.
func TestFitPricesFollowStoredPrices(t *testing.T) {
	transport := &pricesTransport{body: `[
		{"type_id":1001,"average_price":0,"adjusted_price":9000},
		{"type_id":3001,"average_price":0,"adjusted_price":8000},
		{"type_id":3002,"average_price":0,"adjusted_price":7000}
	]`}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	saved := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	mk := func(name string, hull int64, items string) int64 {
		row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
			UserID: user.ID, Name: name, ShipTypeID: hull, IsPublic: true,
			CreatedAt: saved, UpdatedAt: saved,
			ItemsJson: `{"name":"` + name + `","shipTypeId":` + strconv.FormatInt(hull, 10) + `,"items":[` + items + `]}`,
		})
		if err != nil {
			t.Fatalf("create fitting %s: %v", name, err)
		}
		return row.ID
	}
	stale := mk("Stale", 1001, `{"typeId":3001,"qty":2},{"typeId":3002,"qty":3}`)
	unknown := mk("Unknown", 9999, `{"typeId":9998,"qty":1}`)
	// Priced long ago, at prices that no longer hold.
	for _, id := range []int64{stale, unknown} {
		if err := q.SetLocalFittingPrice(ctx, db.SetLocalFittingPriceParams{TotalPrice: 5, ID: id, UserID: user.ID}); err != nil {
			t.Fatal(err)
		}
	}

	// The Forge sweep has typical sells for the hull and both
	// modules: 1,000 / 250 / 40.
	for typeID, typical := range map[int64]float64{1001: 1000, 3001: 250, 3002: 40} {
		if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
			RegionID: 10000002, TypeID: typeID, TypicalSell: typical,
			AvgDailyVolume: 100, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed region stat %d: %v", typeID, err)
		}
	}

	if stored, limited := app.refreshGuidePrices(ctx); limited || !stored {
		t.Fatalf("guide refresh = (stored %v, limited %v), want (true, false)", stored, limited)
	}
	got, err := q.GetPublicFitting(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	// 1,000 hull + 2 x 250 + 3 x 40, all at typical sells.
	if got.TotalPrice != 1620 {
		t.Errorf("repriced fit = %v ISK, want 1620", got.TotalPrice)
	}
	// Repricing is not an edit: the fit keeps its place in the
	// newest-first list.
	if !got.UpdatedAt.Equal(saved) {
		t.Errorf("repricing moved updated_at to %v, want it left at %v", got.UpdatedAt, saved)
	}
	// No price source knows this hull or its module: unknown (0),
	// not the old figure.
	if got, _ := q.GetPublicFitting(ctx, unknown); got.TotalPrice != 0 {
		t.Errorf("fit with no known prices = %v ISK, want 0 (unknown)", got.TotalPrice)
	}

	// Empty price sources price nothing: the stored figures stand.
	if _, err := app.db.ExecContext(ctx, `DELETE FROM market_region_stats`); err != nil {
		t.Fatal(err)
	}
	if n := app.repriceFittings(ctx); n != 0 {
		t.Errorf("repricing against empty price sources rewrote %d fits, want 0", n)
	}
	if got, _ := q.GetPublicFitting(ctx, stale); got.TotalPrice != 1620 {
		t.Errorf("empty price sources changed the price to %v, want 1620 kept", got.TotalPrice)
	}
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

// A user with no saved widget settings reads the widget's defaults.
func TestHomeWidgetSettingsDefaultForANewUser(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	def := homeWidgetRegistry[widgetMarket]
	if got := app.storedOptions(context.Background(), user.ID, def, widgetMarket, nil); got.Get("scope") != scopeAll || got.Get("show") != mergeBoth {
		t.Errorf("fresh-user settings = %v, want defaults", got)
	}
}
