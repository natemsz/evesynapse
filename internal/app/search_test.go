package app

// Hermetic tests for Next-1 (search & findability): the Items DB
// global search and its published-only filter semantics, the
// shared suggestion feed behind every autocomplete box, the top
// banner search (items / own characters / warmed pilots), the
// widget markup on each consumer page, and the daily
// wallet-history sampler's one-row-per-day rule. The transport
// must stay silent throughout: every read here is local.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedNext1SDE plants the type fixture the search tests share:
// two tradeable Tritaniums, an unpublished tradeable draft, a
// published non-market relic-to-be, an unpublished non-market
// one, a second category, a skill wearing a Tritanium name, and
// a category whose only type is unpublished.
func seedNext1SDE(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (900, 'Fixture Category'), (901, 'Far Category'), (902, 'Hidden Category')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES
		   (910, 'Fixture Minerals', 900), (920, 'Fixture Ores', 900), (930, 'Far Group', 901), (940, 'Hidden Group', 902)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (34, 'Tritanium', 910, 5, 1),
		   (35, 'Tritanium Alloy', 910, 5, 1),
		   (38, 'Tritanium Draft', 910, 5, 0),
		   (37, 'Secret Tritanium', 910, 0, 1),
		   (41, 'Lost Tritanium Relic', 920, 0, 0),
		   (42, 'Tritanium of Elsewhere', 930, 5, 1),
		   (43, 'Hidden Relic', 940, 0, 0),
		   (1001, 'Tritanium Skill', 910, 5, 1)`,
		`INSERT INTO sde_skill_meta (type_id, rank, primary_attr, secondary_attr) VALUES (1001, 1, 165, 166)`,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id) VALUES (5000, 35), (5001, 38)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed SDE: %v", err)
		}
	}
}

func TestItemsGlobalSearchAndPublishedOnly(t *testing.T) {
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

	// Unfiltered search sees every published state and market
	// state — the database is the whole database.
	code, body := getPage(t, app, cookie, "/items/?q=tritanium")
	if code != http.StatusOK {
		t.Fatalf("GET search: status %d", code)
	}
	for _, name := range []string{"Tritanium", "Tritanium Alloy", "Tritanium Draft", "Secret Tritanium", "Lost Tritanium Relic", "Tritanium of Elsewhere", "Tritanium Skill"} {
		if !strings.Contains(body, name) {
			t.Errorf("search without filters missing %q", name)
		}
	}

	// Published-only: the SDE published flag alone decides.
	// Unpublished types drop out; Secret Tritanium stays — it
	// is published even though it has no market group, which
	// the old marketable criterion used to exclude.
	code, body = getPage(t, app, cookie, "/items/?q=tritanium&published=1")
	if code != http.StatusOK {
		t.Fatalf("GET search published-only: status %d", code)
	}
	for _, name := range []string{"Tritanium Alloy", "Secret Tritanium", "Tritanium of Elsewhere", "Tritanium Skill"} {
		if !strings.Contains(body, name) {
			t.Errorf("published-only search missing %q", name)
		}
	}
	for _, name := range []string{"Tritanium Draft", "Lost Tritanium Relic"} {
		if strings.Contains(body, name) {
			t.Errorf("published-only search must exclude %q", name)
		}
	}
	// The control itself carries the published wording now.
	if !strings.Contains(body, "Published items only") || !strings.Contains(body, `name="published"`) {
		t.Error("search page: published-only control missing or mislabeled")
	}
	if strings.Contains(body, "Market items only") {
		t.Error("search page still labels the switch Market items only")
	}

	// A link shared before the rename (market=1) filters the
	// same way under the new meaning.
	code, body = getPage(t, app, cookie, "/items/?q=tritanium&market=1")
	if code != http.StatusOK {
		t.Fatalf("GET search legacy market: status %d", code)
	}
	if !strings.Contains(body, "Secret Tritanium") || strings.Contains(body, "Tritanium Draft") {
		t.Error("legacy market=1 link should filter by published, like published=1")
	}

	// Category and group narrowing ride the same parameters.
	code, body = getPage(t, app, cookie, "/items/?q=tritanium&category=901")
	if code != http.StatusOK {
		t.Fatalf("GET search category: status %d", code)
	}
	if !strings.Contains(body, "Tritanium of Elsewhere") || strings.Contains(body, "Tritanium Alloy") {
		t.Error("category=901 should keep only the far type")
	}
	code, body = getPage(t, app, cookie, "/items/?q=tritanium&group=920")
	if code != http.StatusOK {
		t.Fatalf("GET search group: status %d", code)
	}
	if !strings.Contains(body, "Lost Tritanium Relic") || strings.Contains(body, "Tritanium Alloy") {
		t.Error("group=920 should keep only the relic")
	}

	// The browse tree renders every branch with BOTH counts, so
	// the switch can prune in place without a reload: the
	// fixture category holds 6 types, 4 of them published; the
	// far category 1 of 1; the hidden category 0 of 1.
	code, body = getPage(t, app, cookie, "/items/")
	if code != http.StatusOK {
		t.Fatalf("GET categories: status %d", code)
	}
	for _, want := range []string{`data-pubcount="4"`, `data-pubcount="1"`, `data-pubcount="0"`, `data-total="6" data-pub="4"`, "data-published-toggle"} {
		if !strings.Contains(body, want) {
			t.Errorf("categories page missing %s", want)
		}
	}
	// Landed with the switch on (a shared link): branches with
	// no published types start hidden; mixed branches stay.
	code, body = getPage(t, app, cookie, "/items/?published=1")
	if code != http.StatusOK {
		t.Fatalf("GET categories published: status %d", code)
	}
	if !strings.Contains(body, `data-pubcount="0" hidden`) {
		t.Error("categories with published=1: unpublished-only category should start hidden")
	}
	if strings.Contains(body, `data-pubcount="4" hidden`) || strings.Contains(body, `data-pubcount="1" hidden`) {
		t.Error("categories with published=1: branches with published types must stay visible")
	}

	// Category page: the same counts ride on the group rows —
	// the mixed group at 4, the unpublished-only group at 0.
	code, body = getPage(t, app, cookie, "/items/category/900/")
	if code != http.StatusOK {
		t.Fatalf("GET category: status %d", code)
	}
	if !strings.Contains(body, `data-pubcount="4"`) || !strings.Contains(body, `data-pubcount="0"`) {
		t.Error("category page: group rows missing their published counts")
	}
	code, body = getPage(t, app, cookie, "/items/category/900/?published=1")
	if code != http.StatusOK {
		t.Fatalf("GET category published: status %d", code)
	}
	if !strings.Contains(body, `data-pubcount="0" hidden`) {
		t.Error("category with published=1: unpublished-only group should start hidden")
	}

	// The group page no longer drops rows server-side at all:
	// every type renders with its published flag, and with the
	// switch on the unpublished rows merely start hidden, so
	// the client can restore them without a reload.
	code, body = getPage(t, app, cookie, "/items/group/910/")
	if code != http.StatusOK {
		t.Fatalf("GET group plain: status %d", code)
	}
	if !strings.Contains(body, "Tritanium Draft") || !strings.Contains(body, `data-published="0"`) {
		t.Error("group listing keeps unpublished types, flagged unpublished")
	}
	if strings.Contains(body, `data-published="0" hidden`) {
		t.Error("group listing without the switch: nothing starts hidden")
	}
	code, body = getPage(t, app, cookie, "/items/group/910/?published=1")
	if code != http.StatusOK {
		t.Fatalf("GET group published: status %d", code)
	}
	if !strings.Contains(body, "Tritanium Draft") || !strings.Contains(body, `data-published="0" hidden`) {
		t.Error("group listing with published=1: unpublished rows render, starting hidden")
	}
	if !strings.Contains(body, "Secret Tritanium") {
		t.Error("group listing with published=1: published marketless type must stay")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestItemsPublishedToggleWiring pins the in-place half of the
// published-only switch: the app.js handler that prunes the
// rendered tree, and the display rule that keeps a hidden table
// row hidden.
func TestItemsPublishedToggleWiring(t *testing.T) {
	appJS, err := fs.ReadFile(staticFS, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"data-published-toggle", `querySelectorAll("tr[data-pubcount]")`, `querySelectorAll("tr[data-published]")`} {
		if !strings.Contains(string(appJS), want) {
			t.Errorf("app.js: in-place published filter missing %s", want)
		}
	}
	bg, err := fs.ReadFile(staticFS, "static/bg.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bg), ".tw tr[hidden]") {
		t.Error("bg.css: hidden tree rows need the display rule")
	}
}

func TestSharedSuggestPools(t *testing.T) {
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

	get := func(path string) []suggestItem {
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
	ids := func(rows []suggestItem) map[int64]bool {
		m := map[int64]bool{}
		for _, r := range rows {
			m[r.ID] = true
		}
		return m
	}

	// Every pool shares the tradeable floor now:
	// published with a market group — the skill type carries a
	// market group too (skill books trade), so it clears the
	// floor in the broad pools.
	all := ids(get("/items/search.json?q=trit"))
	for _, id := range []int64{34, 35, 42, 1001} {
		if !all[id] {
			t.Errorf("pool=all missing tradeable type %d", id)
		}
	}
	for _, id := range []int64{38, 37, 41} {
		if all[id] {
			t.Errorf("pool=all must exclude untradeable type %d", id)
		}
	}
	for _, row := range get("/items/search.json?q=trit") {
		if row.ID == 34 && row.Label != "Fixture Category · Fixture Minerals" {
			t.Errorf("type 34 label = %q, want category · group", row.Label)
		}
	}

	// Market pool: the floor alone.
	market := ids(get("/items/search.json?q=trit&pool=market"))
	for _, id := range []int64{34, 35, 42, 1001} {
		if !market[id] {
			t.Errorf("pool=market missing type %d", id)
		}
	}
	for _, id := range []int64{38, 37, 41} {
		if market[id] {
			t.Errorf("pool=market must exclude type %d", id)
		}
	}

	// Skills pool: skill types only, whatever their market state.
	skills := ids(get("/items/search.json?q=trit&pool=skills"))
	if !skills[1001] || len(skills) != 1 {
		t.Errorf("pool=skills = %v, want only the skill", skills)
	}

	// Planner pool: published blueprint products only — the
	// unpublished product (38, blueprint seeded) stays out.
	planner := ids(get("/items/search.json?q=trit&pool=planner"))
	if !planner[35] || len(planner) != 1 {
		t.Errorf("pool=planner = %v, want only the published product", planner)
	}

	// Below the length floor: an empty array, never null.
	code, body := getPage(t, app, cookie, "/items/search.json?q=t")
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("short query: status %d body %q, want 200 []", code, body)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestAutocompleteWidgetMarkup(t *testing.T) {
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

	// Market: the original box keeps its ids; the watchlist
	// finder gains the widget beside it.
	code, body := getPage(t, app, cookie, "/market/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/: status %d", code)
	}
	mustContain(t, "/market/", body, `id="market-q"`, `id="watch-q"`, `id="watch-suggest"`)

	// Planner: the product box gains the widget (blueprints are
	// seeded, so the search form renders).
	code, body = getPage(t, app, cookie, "/planner/")
	if code != http.StatusOK {
		t.Fatalf("GET /planner/: status %d", code)
	}
	mustContain(t, "/planner/", body, `id="planner-q"`, `id="planner-suggest"`)

	// Items DB: the new global search box.
	code, body = getPage(t, app, cookie, "/items/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/: status %d", code)
	}
	mustContain(t, "/items/", body, `id="items-q"`, `id="items-suggest"`)

	// Skill plans: create one, open it, the add box has the widget.
	post := func(path string, form url.Values) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("/skills/plans/create", url.Values{
		"character": {"90000001"}, "name": {"Autocomplete Plan"},
	}); code != 200 && code != 303 {
		t.Fatalf("POST create plan = %d", code)
	}
	plans, err := q.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: user.ID, CharacterID: fixtureCharA})
	if err != nil || len(plans) != 1 {
		t.Fatalf("list plans: %v (%d plans)", err, len(plans))
	}
	code, body = getPage(t, app, cookie, "/skills/plans?character=90000001&plan="+strconv.FormatInt(plans[0].ID, 10))
	if code != http.StatusOK {
		t.Fatalf("GET plan: status %d", code)
	}
	mustContain(t, "/skills/plans", body, `id="skill-q"`, `id="skill-suggest"`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestTopbarSearch(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Hauler")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedNext1SDE(t, conn)

	// A warmed stranger; plus a record for an OWN character,
	// which must surface once (as the character), never twice.
	payload, err := json.Marshal(pilotPayload{
		Profile: esi.Character{Name: "Fixture Stranger"},
		Corp:    esi.Corporation{Name: "Stranger Corp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: fixtureMember, Payload: string(payload),
		State: pilotStateReady, FetchedAt: mustNullTime("2026-10-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed pilot record: %v", err)
	}
	ownPayload, err := json.Marshal(pilotPayload{Profile: esi.Character{Name: "Fixture Ceo"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: fixtureCharA, Payload: string(ownPayload),
		State: pilotStateReady, FetchedAt: mustNullTime("2026-10-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed own pilot record: %v", err)
	}

	code, body := getPage(t, app, cookie, "/search.json?q=fixture")
	if code != http.StatusOK {
		t.Fatalf("GET /search.json: status %d", code)
	}
	var hits []searchHit
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("decode hits: %v (%q)", err, body)
	}
	byKind := map[string][]searchHit{}
	for _, h := range hits {
		byKind[h.Kind] = append(byKind[h.Kind], h)
	}
	if len(byKind["character"]) != 2 {
		t.Errorf("character hits = %+v, want both own characters", byKind["character"])
	}
	if len(byKind["pilot"]) != 1 || byKind["pilot"][0].ID != fixtureMember {
		t.Errorf("pilot hits = %+v, want only the stranger", byKind["pilot"])
	}

	// Items ride the same box: a Tritanium query surfaces the
	// type through the shared feed.
	code, body = getPage(t, app, cookie, "/search.json?q=tritanium")
	if code != http.StatusOK {
		t.Fatalf("GET /search.json (items): status %d", code)
	}
	hits = nil
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("decode item hits: %v (%q)", err, body)
	}
	itemFound := false
	for _, h := range hits {
		if h.Kind == "item" && h.ID == 34 {
			itemFound = true
		}
	}
	if !itemFound {
		t.Errorf("item hits = %+v, want Tritanium among them", hits)
	}

	// Below the floor: empty array.
	code, body = getPage(t, app, cookie, "/search.json?q=f")
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("short query: status %d body %q, want 200 []", code, body)
	}

	// The banner carries the box on every signed-in page.
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, `id="topbar-q"`, `id="topbar-suggest"`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestWalletHistorySamplerOneRowPerDay(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1234.5)
	day1 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	app.sampleWalletHistory(ctx, ch, day1)

	rows, err := q.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: user.ID, CharacterID: fixtureCharA,
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("samples after first write: %v (%d rows)", err, len(rows))
	}
	if rows[0].Day != "2026-10-03" || rows[0].Balance != 1234.5 {
		t.Errorf("row = %+v, want 2026-10-03 at 1234.5", rows[0])
	}
	if rows[0].NetWorth.Valid {
		t.Errorf("net worth = %v, want unset while the price cache is cold", rows[0].NetWorth.Float64)
	}

	// Same snapshot, same day: the sampler leaves the row alone.
	app.sampleWalletHistory(ctx, ch, day1.Add(time.Hour))
	rows, _ = q.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: user.ID, CharacterID: fixtureCharA,
	})
	if len(rows) != 1 {
		t.Fatalf("samples after repeat: %d rows, want 1", len(rows))
	}

	// The wallet refreshes with a new balance: still one row for
	// the day, now carrying the fresher number.
	if err := q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: fixtureCharA, Kind: esi.SnapWallet,
		Payload: "2000.25", FetchedAt: mustTime("2026-10-03T13:00:00Z"),
		CachedUntil: mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("refetch wallet: %v", err)
	}
	app.sampleWalletHistory(ctx, ch, day1.Add(65*time.Minute))
	rows, _ = q.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: user.ID, CharacterID: fixtureCharA,
	})
	if len(rows) != 1 || rows[0].Balance != 2000.25 {
		t.Fatalf("samples after refetch: %+v, want one row at 2000.25", rows)
	}

	// With a house price stored and stored assets/orders, the
	// next wallet refresh also lands the net-worth estimate:
	// 2000.25 + 100 × 10 + 50.5 escrow. The Forge's sweep has
	// Tritanium at a typical sell of 10.00.
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 34, TypicalSell: 10, AvgDailyVolume: 1_000_000,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}
	// The sweep landing mid-test: the cache window lapses.
	app.forgeTypicalsMu.Lock()
	app.forgeTypicalsCache = nil
	app.forgeTypicalsMu.Unlock()
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{{TypeID: 34, Quantity: 100}})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{{IsBuyOrder: true, Escrow: 50.5}})
	if err := q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: fixtureCharA, Kind: esi.SnapWallet,
		Payload: "2000.25", FetchedAt: mustTime("2026-10-03T15:00:00Z"),
		CachedUntil: mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("refetch wallet again: %v", err)
	}
	app.sampleWalletHistory(ctx, ch, day1.Add(3*time.Hour+5*time.Minute))
	rows, _ = q.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: user.ID, CharacterID: fixtureCharA,
	})
	if len(rows) != 1 {
		t.Fatalf("samples after priced refresh: %d rows, want 1", len(rows))
	}
	if !rows[0].NetWorth.Valid || rows[0].NetWorth.Float64 != 3050.75 {
		t.Errorf("net worth = %+v, want 3050.75", rows[0].NetWorth)
	}

	// Tomorrow is a new row, written from the last known balance.
	app.sampleWalletHistory(ctx, ch, day1.Add(24*time.Hour))
	rows, _ = q.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: user.ID, CharacterID: fixtureCharA,
	})
	if len(rows) != 2 || rows[1].Day != "2026-10-04" {
		t.Fatalf("samples next day: %+v, want a second row for 2026-10-04", rows)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("sampler made %d outbound calls, want 0", got)
	}
}

// Audit 2.5: the record name searches used to substring-match
// the whole JSON payload and take the first 100 by ID, so a
// query like "pro" (which the payload's own "profile" key
// matches) filled the window with the lowest IDs and a real
// name match beyond them never surfaced. The match now runs on
// the payload's name field: noise payloads that merely mention
// the text elsewhere no longer crowd out a genuine match.
func TestTopbarSearchRecordNameWindow(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	fetched := mustNullTime("2026-10-01T00:00:00Z")

	// 105 noise pilots whose payloads mention "pro" outside the
	// name field (their corporation), with IDs below the target.
	for i := 0; i < 105; i++ {
		payload, err := json.Marshal(pilotPayload{
			Profile: esi.Character{Name: "Noise Pilot"},
			Corp:    esi.Corporation{Name: "Pro Noise Corp"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
			CharacterID: 91000100 + int64(i), Payload: string(payload),
			State: pilotStateReady, FetchedAt: fetched,
		}); err != nil {
			t.Fatalf("seed noise pilot record: %v", err)
		}
	}
	// The genuine name match, past the old window.
	target, err := json.Marshal(pilotPayload{Profile: esi.Character{Name: "Pro Target"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: 91000300, Payload: string(target),
		State: pilotStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed target pilot record: %v", err)
	}

	// Both stored corporation shapes: the wrapped record and a
	// bare ESI corporation as a peer would share it.
	wrapped, err := json.Marshal(corporationRecordPayload{Corp: esi.Corporation{Name: "Pro Holdings", Ticker: "PROH"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 92000001, Payload: string(wrapped),
		State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed wrapped corp record: %v", err)
	}
	bare, err := json.Marshal(esi.Corporation{Name: "Pro Bare", Ticker: "PB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 92000002, Payload: string(bare),
		State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed bare corp record: %v", err)
	}

	code, body := getPage(t, app, cookie, "/search.json?q=pro")
	if code != http.StatusOK {
		t.Fatalf("GET /search.json: status %d", code)
	}
	var hits []searchHit
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("decode hits: %v (%q)", err, body)
	}
	pilotFound := false
	corpsFound := map[int64]bool{}
	for _, h := range hits {
		if h.Kind == "pilot" {
			if h.ID >= 91000100 && h.ID < 91000300 {
				t.Errorf("noise pilot %d surfaced; its name does not match", h.ID)
			}
			if h.ID == 91000300 {
				pilotFound = true
			}
		}
		if h.Kind == "corporation" {
			corpsFound[h.ID] = true
		}
	}
	if !pilotFound {
		t.Errorf("pilot hits = %+v, want Pro Target (91000300) among them", hits)
	}
	if !corpsFound[92000001] || !corpsFound[92000002] {
		t.Errorf("corporation hits = %+v, want both the wrapped and bare Pro records", hits)
	}
}
