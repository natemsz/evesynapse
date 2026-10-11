package app

// Tests for trade ideas: config gating, the bounded Forge CSV,
// the Cloudflare call shape (URL, auth, prompt, fail-closed
// parsing), refresh storing while failures keep the old answer,
// and the widget rendering stored text escaped.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

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
	raw, _ := io.ReadAll(io.LimitReader(req.Body, tradeIdeasRespCap))
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

func TestCloudflareAccountIDRule(t *testing.T) {
	for _, id := range []string{"abcdef0123456789abcdef0123456789", "abc", "a-b_c9"} {
		if !validCloudflareAccountID(id) {
			t.Fatalf("valid ID refused: %q", id)
		}
	}
	for _, id := range []string{"", "has space", "has/slash", "has.dot", strings.Repeat("a", 65)} {
		if validCloudflareAccountID(id) {
			t.Fatalf("unsafe ID accepted: %q", id)
		}
	}
}

func TestTruncateRunesKeepsBoundaries(t *testing.T) {
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Fatalf("short text changed: %q", got)
	}
	if got := truncateRunes("abcdef", 4); got != "abcd" {
		t.Fatalf("ascii truncate: %q", got)
	}
	// Two-byte rune at the cut: keep the whole rune or drop it,
	// never split it.
	if got := truncateRunes("abécd", 3); got != "ab" {
		t.Fatalf("rune-boundary truncate: %q", got)
	}
}

func TestCloudflareCallShape(t *testing.T) {
	stub := &cloudflareStub{reply: `{"success":true,"errors":[],"result":{"response":"- idea one"}}`}
	client := &http.Client{Transport: stub}
	prompt := tradeIdeasPrompt + "\n\nItem,Station\nTritanium,Jita\n"
	got, err := callCloudflareAI(context.Background(), client, tradeIdeasAPIBase,
		"abcdef0123456789abcdef0123456789", "test-token-for-trade-ideas-only", tradeIdeasModel, prompt)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got != "- idea one" {
		t.Fatalf("answer: %q", got)
	}
	if stub.calls != 1 {
		t.Fatalf("calls: %d, want 1", stub.calls)
	}
	wantURL := tradeIdeasAPIBase + "/accounts/abcdef0123456789abcdef0123456789/ai/run/" + tradeIdeasModel
	if stub.last.URL.String() != wantURL {
		t.Fatalf("URL: %s, want %s", stub.last.URL, wantURL)
	}
	if auth := stub.last.Header.Get("Authorization"); auth != "Bearer test-token-for-trade-ideas-only" {
		t.Fatalf("auth header: %q", auth)
	}
	var sent struct {
		Messages []cloudflareAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(stub.body, &sent); err != nil || len(sent.Messages) != 2 {
		t.Fatalf("request body: %v (%s)", err, stub.body)
	}
	if sent.Messages[1].Role != "user" || !strings.Contains(sent.Messages[1].Content, tradeIdeasPrompt) || !strings.Contains(sent.Messages[1].Content, "Tritanium") {
		t.Fatalf("prompt not carried: %q", sent.Messages[1].Content)
	}
}

func TestCloudflareCallReadsChoices(t *testing.T) {
	// Chat models (Gemma 4's sync-output schema) answer in
	// result.choices, not result.response.
	stub := &cloudflareStub{reply: `{"success":true,"errors":[],"result":{"choices":[{"message":{"role":"assistant","content":"- idea two"},"finish_reason":"stop"}]}}`}
	got, err := callCloudflareAI(context.Background(), &http.Client{Transport: stub}, tradeIdeasAPIBase,
		"abcdef0123456789abcdef0123456789", "test-token-for-trade-ideas-only", tradeIdeasModel, "hi")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got != "- idea two" {
		t.Fatalf("answer: %q", got)
	}
}

func TestCloudflareCallFailsClosed(t *testing.T) {
	ok := &cloudflareStub{reply: `{"success":true,"errors":[],"result":{"response":"x"}}`}
	client := &http.Client{Transport: ok}
	ctx := context.Background()
	// Malformed account ID never dials.
	if _, err := callCloudflareAI(ctx, client, tradeIdeasAPIBase, "has space", "tok", tradeIdeasModel, "hi"); err == nil {
		t.Fatalf("malformed account ID dialed")
	}
	if ok.calls != 0 {
		t.Fatalf("malformed account ID cost %d calls", ok.calls)
	}
	// Missing token never dials.
	if _, err := callCloudflareAI(ctx, client, tradeIdeasAPIBase, "abcdef0123456789abcdef0123456789", "", tradeIdeasModel, "hi"); err == nil {
		t.Fatalf("missing token dialed")
	}
	// HTTP failure, model failure, and empty answers all error.
	for name, stub := range map[string]*cloudflareStub{
		"status": {code: http.StatusInternalServerError, reply: `oops`},
		"failed": {reply: `{"success":false,"errors":[{"message":"busy"}],"result":{}}`},
		"empty":  {reply: `{"success":true,"errors":[],"result":{"response":"  "}}`},
		"junk":   {reply: `not json`},
	} {
		if _, err := callCloudflareAI(ctx, &http.Client{Transport: stub}, tradeIdeasAPIBase,
			"abcdef0123456789abcdef0123456789", "tok", tradeIdeasModel, "hi"); err == nil {
			t.Fatalf("%s: no error", name)
		}
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
		{LocationID: 60003760, RegionID: 10000002, TypeID: 34, BestSell: 10, BestBuy: 4, SellOrders: 1, BuyOrders: 1, SellVolume: 100, BuyVolume: 50, UpdatedAt: now},
		{LocationID: 60003760, RegionID: 10000002, TypeID: 35, BestSell: 100, BestBuy: 90, SellOrders: 1, BuyOrders: 1, SellVolume: 10, BuyVolume: 5, UpdatedAt: now},
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
	if _, ok := app.buildTradeIdeasCSV(context.Background()); ok {
		t.Fatalf("CSV without a sweep")
	}
	seedTradeIdeasScanner(t, conn, q)
	text, ok := app.buildTradeIdeasCSV(context.Background())
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
	if !strings.Contains(text, "Tritanium") || !strings.Contains(text, "150.0%") {
		t.Fatalf("CSV lacks the seeded opportunity:\n%s", text)
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
	for i := int64(0); i < tradeIdeasCSVRows+5; i++ {
		typeID := int64(2000 + i)
		if _, err := conn.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 18)`, typeID, fmt.Sprintf("Cap Item %d", i)); err != nil {
			t.Fatalf("seed type: %v", err)
		}
		if err := q.UpsertMarketStationStat(ctx, db.UpsertMarketStationStatParams{
			LocationID: 60003760, RegionID: 10000002, TypeID: typeID,
			BestSell: 20, BestBuy: 10, SellOrders: 1, BuyOrders: 1,
			SellVolume: 1000, BuyVolume: 1000, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed stat: %v", err)
		}
		seedScannerVolume(t, q, 10000002, typeID, 10)
	}
	text, ok := app.buildTradeIdeasCSV(context.Background())
	if !ok {
		t.Fatalf("CSV with seeded rows: not ok")
	}
	rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatalf("CSV decode: %v", err)
	}
	if len(rows) != tradeIdeasCSVRows+1 {
		t.Fatalf("CSV rows: %d, want header + %d", len(rows), tradeIdeasCSVRows)
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
	if stored, _ := app.refreshTradeIdeas(ctx); stored != 1 {
		t.Fatalf("refresh stored %d, want 1", stored)
	}
	row, err := q.GetTradeIdeas(ctx)
	if err != nil || !strings.Contains(row.Body, "Buy Tritanium low") || row.Model != tradeIdeasModel {
		t.Fatalf("stored row: %+v err %v", row, err)
	}
	if !strings.Contains(string(mustReadStub(t, stub)), "Tritanium") {
		t.Fatalf("prompt lacked the scanner rows")
	}
	// Fresh enough: the model is not asked again.
	if stored, _ := app.refreshTradeIdeas(ctx); stored != 0 || stub.calls != 1 {
		t.Fatalf("second refresh stored %d with %d calls, want 0 and 1", stored, stub.calls)
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
	if err := q.PutTradeIdeas(ctx, db.PutTradeIdeasParams{Body: "old ideas", AsOf: now, Model: tradeIdeasModel}); err != nil {
		t.Fatalf("seed old answer: %v", err)
	}
	stub := &cloudflareStub{code: http.StatusBadGateway, reply: `bad gateway`}
	app.tradeIdeasHTTP = &http.Client{Transport: stub}
	if stored, _ := app.refreshTradeIdeas(ctx); stored != 0 {
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
	if stored, _ := app.refreshTradeIdeas(context.Background()); stored != 0 {
		t.Fatalf("unconfigured refresh stored %d", stored)
	}
}

func TestTradeIdeasWidgetRendersStored(t *testing.T) {
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
		Body: "- Station flips\n- Regional arb <script>alert(1)</script>", AsOf: time.Now().UTC(), Model: tradeIdeasModel,
	}); err != nil {
		t.Fatalf("store answer: %v", err)
	}
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Trade ideas", "Station flips", "Gemma on the Forge spread scanner")
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("model text rendered unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("model text missing escaped")
	}
}

func TestTradeIdeasWidgetEmptyStates(t *testing.T) {
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
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Trade ideas are not set up", "CLOUDFLARE_ACCOUNT_ID")
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
