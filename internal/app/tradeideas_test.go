package app

// Tests for trade ideas: config gating, the bounded Forge CSV,
// the Cloudflare call shape (URL, auth, prompt, fail-closed
// parsing), refresh storing while failures keep the old answer,
// and the widget rendering stored text escaped.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"evesynapse/internal/app/module"
	"evesynapse/internal/app/tradeideas"
	"evesynapse/internal/ideas"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// csvRows is how many scanner rows the module's own ask carries.
const csvRows = 60

// cloudflareStub plays Workers AI: it checks the request the
// way the contract needs (URL shape, auth header, prompt
// carried) and answers canned JSON.
type cloudflareStub struct {
	calls int
	last  *http.Request
	body  []byte
	code  int
	reply string
}

func (s *cloudflareStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	s.last = req
	raw, _ := io.ReadAll(io.LimitReader(req.Body, (1 << 20)))
	s.body = raw
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return &http.Response{
		StatusCode: s.code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.reply)),
	}, nil
}

func TestTradeIdeasConfigured(t *testing.T) {
	var empty Config
	if empty.TradeIdeasConfigured() {
		t.Fatalf("empty config: configured")
	}
	half := Config{cloudflareAccountID: "abc"}
	if half.TradeIdeasConfigured() {
		t.Fatalf("account without token: configured")
	}
	full := Config{cloudflareAccountID: "abc", cloudflareAuthToken: "tok"}
	if !full.TradeIdeasConfigured() {
		t.Fatalf("both set: not configured")
	}
}

// seedTradeIdeasScanner stores two qualifying Forge rows
// (Tritanium wide, Pyerite narrow) plus SDE names and station.
func seedTradeIdeasScanner(t *testing.T, conn *sql.DB, q *db.Queries) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18), (35, 'Pyerite', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	for _, seed := range []db.UpsertMarketStationStatParams{
		{LocationID: 60003760, RegionID: 10000002, TypeID: 34, BestSell: 10, BestBuy: 4, TypicalSell: 10, TypicalBuy: 4, SellOrders: 5, BuyOrders: 5, SellVolume: 100, BuyVolume: 50, UpdatedAt: now},
		{LocationID: 60003760, RegionID: 10000002, TypeID: 35, BestSell: 100, BestBuy: 90, TypicalSell: 100, TypicalBuy: 90, SellOrders: 5, BuyOrders: 5, SellVolume: 10, BuyVolume: 5, UpdatedAt: now},
	} {
		if err := q.UpsertMarketStationStat(ctx, seed); err != nil {
			t.Fatalf("seed station stat: %v", err)
		}
	}
	seedScannerVolume(t, q, 10000002, 34, 20)
	seedScannerVolume(t, q, 10000002, 35, 100)
}

func TestTradeIdeasCSVBounded(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	// No sweep yet: no CSV, no model question.
	if _, ok := tradeideas.BuildCSV(context.Background(), app, csvRows); ok {
		t.Fatalf("CSV without a sweep")
	}
	seedTradeIdeasScanner(t, conn, q)
	text, ok := tradeideas.BuildCSV(context.Background(), app, csvRows)
	if !ok {
		t.Fatalf("CSV with seeded rows: not ok")
	}
	rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatalf("CSV decode: %v", err)
	}
	if len(rows) != 3 { // header + Tritanium + Pyerite
		t.Fatalf("CSV rows: %d, want 3", len(rows))
	}
	// The leading "#" column numbers the rows 1-based; the model's
	// "(#n)" references name it.
	if rows[0][0] != "#" || rows[1][0] != "1" || rows[2][0] != "2" {
		t.Fatalf("CSV row numbers: header %q, then %q, %q", rows[0][0], rows[1][0], rows[2][0])
	}
	if rows[1][1] != "Tritanium" || rows[0][1] != "Item" {
		t.Fatalf("CSV shifted by the new column: header %q, first row %q", rows[0][1], rows[1][1])
	}
	if !strings.Contains(text, "Tritanium") || !strings.Contains(text, "150.0%") {
		t.Fatalf("CSV lacks the seeded opportunity:\n%s", text)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("CSV build made %d outbound calls, want 0", got)
	}
}

// TestTradeIdeasCSVSkipsJokeStationBook: the audit's Offender A
// book -- one fishing bid at 5.25 under one hopeful ask at
// 110.25 at a minor Forge station, for a type trading 40,000 a
// day at Jita typicals -- used to reach the model as a 2000.0%
// "opportunity". Trade Ideas reads the scanner's query, so the
// depth gate keeps it out of the CSV entirely: no rows, no ask.
func TestTradeIdeasCSVSkipsJokeStationBook(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003761, 'Jita 4 - Moon 10 - Food Packaging Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 34,
		TypicalSell: 105.60, TypicalBuy: 104.90,
		AvgDailyVolume: 40000, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}
	if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
		LocationID: 60003761, RegionID: 10000002, TypeID: 34,
		BestSell: 110.25, BestBuy: 5.25, TypicalSell: 110.25, TypicalBuy: 5.25,
		SellOrders: 1, BuyOrders: 1, SellVolume: 800000, BuyVolume: 2000000, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed station stat: %v", err)
	}
	if _, ok := tradeideas.BuildCSV(ctx, app, csvRows); ok {
		t.Fatal("CSV built from a one-order-each-side joke book, want no CSV at all")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("CSV build made %d outbound calls, want 0", got)
	}
}

func TestTradeIdeasCSVRowCap(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4', 30000142)`); err != nil {
		t.Fatalf("seed station: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`); err != nil {
		t.Fatalf("seed system: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatalf("seed region: %v", err)
	}
	for i := int64(0); i < csvRows+5; i++ {
		typeID := int64(2000 + i)
		if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 18)`, typeID, fmt.Sprintf("Cap Item %d", i)); err != nil {
			t.Fatalf("seed type: %v", err)
		}
		if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
			LocationID: 60003760, RegionID: 10000002, TypeID: typeID,
			BestSell: 20, BestBuy: 10, TypicalSell: 20, TypicalBuy: 10, SellOrders: 5, BuyOrders: 5,
			SellVolume: 1000, BuyVolume: 1000, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed stat: %v", err)
		}
		seedScannerVolume(t, q, 10000002, typeID, 10)
	}
	text, ok := tradeideas.BuildCSV(context.Background(), app, csvRows)
	if !ok {
		t.Fatalf("CSV with seeded rows: not ok")
	}
	rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatalf("CSV decode: %v", err)
	}
	if len(rows) != csvRows+1 {
		t.Fatalf("CSV rows: %d, want header + %d", len(rows), csvRows)
	}
}

func TestTradeIdeasRefreshStores(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	app.cfg.cloudflareAccountID = "abcdef0123456789abcdef0123456789"
	app.cfg.cloudflareAuthToken = "test-token-for-trade-ideas-only"
	seedTradeIdeasScanner(t, conn, q)
	stub := &cloudflareStub{reply: `{"success":true,"errors":[],"result":{"response":"- Buy Tritanium low"}}`}
	app.tradeIdeasHTTP = &http.Client{Transport: stub}
	// Two asks: the module's short answer and the page's own long one.
	if stored := tradeideas.Refresh(ctx, app); stored != 2 || stub.calls != 2 {
		t.Fatalf("refresh stored %d with %d calls, want 2 and 2", stored, stub.calls)
	}
	row, err := q.GetTradeIdeas(ctx)
	if err != nil || !strings.Contains(row.Body, "Buy Tritanium low") || row.Model != ideas.GemmaModel {
		t.Fatalf("stored row: %+v err %v", row, err)
	}
	page, err := q.GetTradeIdeasPage(ctx)
	if err != nil || !strings.Contains(page.Body, "Buy Tritanium low") {
		t.Fatalf("stored page row: %+v err %v", page, err)
	}
	// Body and row set are stored together, for both asks: the rows
	// the CSV carried, numbered as sent.
	for name, raw := range map[string]string{"module": row.Rows.String, "page": page.Rows.String} {
		rows := ideas.DecodeRows(raw)
		if len(rows) != 2 || rows[0].N != 1 || rows[1].N != 2 {
			t.Fatalf("%s row set = %+v, want the two seeded rows numbered 1, 2", name, rows)
		}
		items := map[string]bool{rows[0].Item: true, rows[1].Item: true}
		if !items["Tritanium"] || !items["Pyerite"] || rows[0].Spread == "" || rows[0].Profit == "" {
			t.Fatalf("%s row set lacks the seeded figures: %+v", name, rows)
		}
	}
	// The last ask was the page's: its prompt, its larger answer.
	sent := string(mustReadStub(t, stub))
	if !strings.Contains(sent, "Tritanium") || !strings.Contains(sent, "up to twenty ideas") || !strings.Contains(sent, `"max_tokens":6000`) {
		t.Fatalf("page ask lacked its prompt, the scanner rows or its slim token cap: %s", sent)
	}
	if !strings.Contains(sent, "Do NOT repeat any prices") || !strings.Contains(sent, "(#n)") {
		t.Fatalf("page ask is not on the row-reference contract: %s", sent)
	}
	// Fresh enough: the model is not asked again.
	if stored := tradeideas.Refresh(ctx, app); stored != 0 || stub.calls != 2 {
		t.Fatalf("second refresh stored %d with %d calls, want 0 and 2", stored, stub.calls)
	}
}

func mustReadStub(t *testing.T, stub *cloudflareStub) []byte {
	t.Helper()
	if stub.body == nil {
		t.Fatalf("stub saw no request body")
	}
	return stub.body
}

func TestTradeIdeasRefreshKeepsOldOnFailure(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	app.cfg.cloudflareAccountID = "abcdef0123456789abcdef0123456789"
	app.cfg.cloudflareAuthToken = "test-token-for-trade-ideas-only"
	seedTradeIdeasScanner(t, conn, q)
	now := time.Now().UTC().Add(-25 * time.Hour) // past the refresh age
	if err := q.PutTradeIdeas(ctx, db.PutTradeIdeasParams{Body: "old ideas", AsOf: now, Model: ideas.GemmaModel}); err != nil {
		t.Fatalf("seed old answer: %v", err)
	}
	stub := &cloudflareStub{code: http.StatusBadGateway, reply: `bad gateway`}
	app.tradeIdeasHTTP = &http.Client{Transport: stub}
	if stored := tradeideas.Refresh(ctx, app); stored != 0 {
		t.Fatalf("failed refresh stored %d, want 0", stored)
	}
	row, err := q.GetTradeIdeas(ctx)
	if err != nil || row.Body != "old ideas" {
		t.Fatalf("old answer lost: %+v err %v", row, err)
	}
}

func TestTradeIdeasRefreshOffWithoutCreds(t *testing.T) {
	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	// No credentials: no dial even with a client that errors.
	app.tradeIdeasHTTP = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("must not dial")
	})}
	if stored := tradeideas.Refresh(context.Background(), app); stored != 0 {
		t.Fatalf("unconfigured refresh stored %d", stored)
	}
}

func TestTradeIdeasPageRendersStored(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	setHomeLayout(t, q, user.ID, `[{"id":"tradeideas"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	if err := q.PutTradeIdeas(ctx, db.PutTradeIdeasParams{
		Body: "- Station flips\n- Regional arb <script>alert(1)</script>", AsOf: time.Now().UTC(), Model: ideas.GemmaModel,
	}); err != nil {
		t.Fatalf("store answer: %v", err)
	}
	code, body := getPage(t, app, cookie, "/market/ideas/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/ideas/: status %d", code)
	}
	mustContain(t, "/market/ideas/", body, "Trade Ideas", "Station flips", "Gemma on the Forge spread scanner", "ti-a1")
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("model text rendered unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("model text missing escaped")
	}
	// An optional home module: a layout that names it shows the
	// same ideas, tabbed by strategy.
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ trade ideas module", body, `data-widget="tradeideas"`, "Station flips", `role="tablist"`, `aria-controls="ti-1"`)
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("model text rendered unescaped on the home page")
	}

	// Once the page's own answer lands, the page shows it — every
	// idea with its figures — while the module keeps its short one.
	if err := q.PutTradeIdeasPage(ctx, db.PutTradeIdeasPageParams{
		Body:  "### Hauling Arbitrage\n* **Pyerite** · Jita IV-4 · buy 9.10 · sell 10.40 · 14.3% · 2,000/day · 2.6M ISK/day — thin spread, deep volume",
		AsOf:  time.Now().UTC(),
		Model: ideas.GemmaModel,
	}); err != nil {
		t.Fatalf("store page answer: %v", err)
	}
	code, body = getPage(t, app, cookie, "/market/ideas/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/ideas/ (page answer): status %d", code)
	}
	mustContain(t, "/market/ideas/ (page answer)", body, "Hauling Arbitrage", "<strong>Pyerite</strong>", "2.6M ISK/day", "thin spread, deep volume", "ti-detail")
	if strings.Contains(body, "Station flips") {
		t.Error("page still shows the module's answer after its own landed")
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (page answer): status %d", code)
	}
	if !strings.Contains(body, "Station flips") || strings.Contains(body, "Hauling Arbitrage") {
		t.Error("home module should keep the module's own answer")
	}
}

// TestTradeIdeasPageComposesFromRows: a new-format answer and its
// stored row set render the composed line — figures from the rows,
// in the legacy format, no reference shown.
func TestTradeIdeasPageComposesFromRows(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	raw, err := ideas.EncodeRows([]ideas.Row{
		{N: 1, Item: "Tritanium", Station: "Jita IV - Moon 4", Buy: "4.00", Sell: "10.00", Spread: "150.0%", Sold: "20", Profit: "120.00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.PutTradeIdeasPage(ctx, db.PutTradeIdeasPageParams{
		Body:  "### Station Flips\n* **Tritanium** (#1) — deep volume at the hub",
		Rows:  sql.NullString{String: raw, Valid: true},
		AsOf:  time.Now().UTC(),
		Model: ideas.GemmaModel,
	}); err != nil {
		t.Fatalf("store answer with rows: %v", err)
	}
	code, body := getPage(t, app, cookie, "/market/ideas/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/ideas/: status %d", code)
	}
	mustContain(t, "/market/ideas/ (composed)", body,
		"<strong>Tritanium</strong>", "Typical buy 4.00", "Typical sell 10.00",
		"Spread 150.0%", "20 sold per day", "Estimated daily profit 120.00", "deep volume at the hub")
	if strings.Contains(body, "(#1)") {
		t.Error("the row reference is rendered on the page")
	}
}

func TestTradeIdeasPageEmptyStates(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	setHomeLayout(t, q, user.ID, `[{"id":"tradeideas"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/ideas/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/ideas/: status %d", code)
	}
	mustContain(t, "/market/ideas/", body, "Trade Ideas are not set up", "CLOUDFLARE_ACCOUNT_ID")
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestTradeIdeasFailureBacksOff: a failed ask is recorded, and the
// next cycle does not ask again (it used to, every cycle).
func TestTradeIdeasFailureBacksOff(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	app.cfg.cloudflareAccountID = "abcdef0123456789abcdef0123456789"
	app.cfg.cloudflareAuthToken = "test-token-for-trade-ideas-only"
	seedTradeIdeasScanner(t, conn, q)
	stub := &cloudflareStub{code: http.StatusBadGateway, reply: `bad gateway`}
	app.tradeIdeasHTTP = &http.Client{Transport: stub}
	for i := 0; i < 3; i++ {
		tradeideas.Refresh(ctx, app)
	}
	if stub.calls != 1 {
		t.Errorf("three cycles after a failure asked the model %d times, want 1", stub.calls)
	}
	a, err := q.GetTradeIdeasAttempt(ctx)
	if err != nil || a.Failures != 1 {
		t.Errorf("recorded attempt = %+v (err %v), want one failure", a, err)
	}
}

// TestTradeIdeasRefreshBesideCycle: the worker starts the refresh
// and moves on; it does not wait for the model.
func TestTradeIdeasRefreshBesideCycle(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	app.cfg.cloudflareAccountID = "abcdef0123456789abcdef0123456789"
	app.cfg.cloudflareAuthToken = "test-token-for-trade-ideas-only"
	seedTradeIdeasScanner(t, conn, q)
	release := make(chan struct{})
	app.tradeIdeasHTTP = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		<-release // a slow model
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"success":true,"result":{"response":"- late"}}`))}, nil
	})}
	start := time.Now()
	app.runModuleJobs(ctx, module.AfterMarket, nil)
	app.runModuleJobs(ctx, module.AfterMarket, nil) // one at a time: a no-op while busy
	if took := time.Since(start); took > time.Second {
		t.Errorf("starting the refresh took %v; the cycle waited on the model", took)
	}
	close(release)
	app.moduleWG.Wait()
	if row, err := q.GetTradeIdeas(ctx); err != nil || !strings.Contains(row.Body, "late") {
		t.Errorf("the refresh beside the cycle did not land: %+v %v", row, err)
	}
}
