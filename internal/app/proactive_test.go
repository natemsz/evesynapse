package app

// Hermetic tests for the proactive-warming build: the market
// coverage set (tiers, liquid core, orbit), search prefetch,
// the pilot counterparty orbit with drain priority, the urgent
// want drain, the live-region fragments, and migration 016.
// Renders run against a counting transport that must stay at
// zero calls; worker drains run against path-routing stubs.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
)

const forge = defaultMarketRegion

// pathCountingTransport records every request path and answers
// 500 to everything: renders must never touch it, and tests can
// assert which endpoint classes a handler stayed away from.
type pathCountingTransport struct {
	paths []string
}

func (s *pathCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.paths = append(s.paths, req.URL.Path)
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"must not be called"}`)),
	}, nil
}

// errLimitTransport answers 429 to everything and counts calls:
// the urgent drain must stop after the first pushback.
type errLimitTransport struct{ calls atomic.Int64 }

func (s *errLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"error limited"}`)),
	}, nil
}

func keyIndex(keys []marketKey, regionID, typeID int64) int {
	for i, k := range keys {
		if k.RegionID == regionID && k.TypeID == typeID {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Market coverage: wants → watchlist → orders → liquid core →
// orbit; assets only ever enter at The Forge.
// ---------------------------------------------------------------------------

func TestHistoryCandidatesCoverageTiers(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	// Tier 1: a fresh want.
	if err := q.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: forge, TypeID: 1008, LastRequestedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed want: %v", err)
	}
	// Tier 2: a watchlist pair in its own region.
	if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
		UserID: user.ID, TypeID: 1007, RegionID: 10000030, ThresholdPct: 5,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed watchlist: %v", err)
	}
	// Tier 3: an open order in Heimatar.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 1, TypeID: 1001, RegionID: 10000030, Price: 10, VolumeTotal: 5, VolumeRemain: 5},
	})
	// Tier 5 (orbit, all at The Forge): an asset wherever it sits,
	// a job product, an owned blueprint's product, a fitting's
	// ship and module, a contract's item.
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 1, TypeID: 1002, LocationID: 60003760, LocationType: "station"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 1, ProductTypeID: 1003},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, []esi.Blueprint{
		{ItemID: 9, TypeID: 5000},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFittings, []esi.Fitting{
		{FittingID: 1, ShipTypeID: 1005, Items: []esi.FittingItem{{TypeID: 1006}}},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{
		{ContractID: 777, IssuerID: 93300009},
	})
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES (5000, 1004, 1, 100, 600)`); err != nil {
		t.Fatalf("seed blueprint sde: %v", err)
	}
	itemsJSON, _ := json.Marshal([]esi.ContractItem{{RecordID: 1, TypeID: 1009}})
	if err := q.UpsertContractDetail(ctx, db.UpsertContractDetailParams{
		ContractID: 777, CharacterID: fixtureCharA, Payload: string(itemsJSON), FetchedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed contract detail: %v", err)
	}

	keys, err := app.historyCandidates(ctx)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}

	wantAt := keyIndex(keys, forge, 1008)
	watchAt := keyIndex(keys, 10000030, 1007)
	orderAt := keyIndex(keys, 10000030, 1001)
	for name, idx := range map[string]int{"want": wantAt, "watchlist": watchAt, "order": orderAt} {
		if idx < 0 {
			t.Fatalf("candidates missing %s key; got %v", name, keys)
		}
	}
	if !(wantAt < watchAt && watchAt < orderAt) {
		t.Fatalf("tier order broken: want=%d watch=%d order=%d", wantAt, watchAt, orderAt)
	}
	for _, typeID := range []int64{1002, 1003, 1004, 1005, 1006, 1009} {
		if keyIndex(keys, forge, typeID) < orderAt {
			t.Fatalf("orbit type %d not after orders tier", typeID)
		}
	}
	// Forge-only rule: the asset type must not appear anywhere
	// but The Forge.
	for _, k := range keys {
		if k.TypeID == 1002 && k.RegionID != forge {
			t.Fatalf("asset type 1002 covered outside The Forge at region %d", k.RegionID)
		}
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("candidate derivation made %d outbound calls, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Liquid core: ranked from stored history (ISK velocity over the
// region's newest seven stored days), bounded, and supplemented
// from recent killmail details only while the ranking is short.
// ---------------------------------------------------------------------------

func seedForgeHistory(t *testing.T, q *db.Queries, ctx context.Context, typeID, volume int64, average float64) {
	t.Helper()
	if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
		RegionID: forge, TypeID: typeID, Date: "2026-10-02",
		Average: average, Highest: average * 1.1, Lowest: average * 0.9,
		Volume: volume, OrderCount: 7,
	}); err != nil {
		t.Fatalf("seed history %d: %v", typeID, err)
	}
}

func TestLiquidCoreRankingAndBound(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	// 205 ranked types: velocity strictly increasing with id.
	for id := int64(3000); id < 3205; id++ {
		seedForgeHistory(t, q, ctx, id, 10, float64(id))
	}
	core := app.liquidCoreTypeIDs(ctx)
	if len(core) != liquidCoreSize {
		t.Fatalf("core size = %d, want %d", len(core), liquidCoreSize)
	}
	highest := core[0]
	if highest != 3204 {
		t.Fatalf("core leader = %d, want 3204 (highest velocity)", highest)
	}
	if core[len(core)-1] != 3204-liquidCoreSize+1 {
		t.Fatalf("core tail = %d, want %d", core[len(core)-1], 3204-liquidCoreSize+1)
	}
}

func TestLiquidCoreKillmailSupplement(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	seedForgeHistory(t, q, ctx, 2001, 10, 100) // velocity 1000
	seedForgeHistory(t, q, ctx, 2002, 5, 10)   // velocity 50
	seedForgeHistory(t, q, ctx, 2003, 1, 1)    // velocity 1

	km := esi.Killmail{KillmailID: 42, Victim: esi.KillmailVictim{
		ShipTypeID: 2004,
		Items:      []esi.KillmailVictimItem{{ItemTypeID: 2005}, {ItemTypeID: 2001}},
	}}
	payload, _ := json.Marshal(km)
	if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
		KillmailID: 42, CharacterID: fixtureCharA, Hash: "h", Payload: string(payload),
		FetchedAt: "2026-10-02T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed killmail detail: %v", err)
	}

	core := app.liquidCoreTypeIDs(ctx)
	want := []int64{2001, 2002, 2003, 2004, 2005}
	if len(core) != len(want) {
		t.Fatalf("core = %v, want %v", core, want)
	}
	for i := range want {
		if core[i] != want[i] {
			t.Fatalf("core[%d] = %d, want %d (full: %v)", i, core[i], want[i], core)
		}
	}
}

// ---------------------------------------------------------------------------
// Search prefetch: the handler records wants for every rendered
// result and fetches nothing itself.
// ---------------------------------------------------------------------------

func TestMarketSearchPrefetchEnqueuesWants(t *testing.T) {
	transport := &pathCountingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 18, 1, 1), (35, 'Pyerite', 18, 1, 1)`); err != nil {
		t.Fatalf("seed sde types: %v", err)
	}

	code, _ := getPage(t, app, cookie, "/market/?q=tritanium")
	if code != http.StatusOK {
		t.Fatalf("search page: status %d", code)
	}
	wants, err := q.ListMarketHistoryWants(ctx, "")
	if err != nil {
		t.Fatalf("list wants: %v", err)
	}
	found := false
	for _, wn := range wants {
		if wn.TypeID == 34 && wn.RegionID == forge {
			found = true
		}
	}
	if !found {
		t.Fatalf("search results did not enqueue a Forge want for 34; wants=%v", wants)
	}
	// The handler itself must not fetch history: no /history/
	// path may appear among the calls it made.
	for _, p := range transport.paths {
		if strings.Contains(p, "/history/") {
			t.Fatalf("search handler fetched history itself: %s", p)
		}
	}
}

// ---------------------------------------------------------------------------
// Pilot orbit: counterparties from the user's own snapshots,
// own characters excluded, killmail participants never touched,
// viewed wants outrank the orbit in the drain.
// ---------------------------------------------------------------------------

func TestPilotOrbitDerivationAndPriority(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")

	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, []esi.WalletJournalEntry{
		{ID: 1, FirstPartyID: 93300001, SecondPartyID: fixtureCharB},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMail, []esi.MailHeader{
		{MailID: 1, From: 93300002},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{
		{ContractID: 5, IssuerID: 93300004, AssigneeID: 99000001},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapContacts, []esi.Contact{
		{ContactID: 93300003, ContactType: "character"},
		{ContactID: 98000002, ContactType: "corporation"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpMembers, []int64{fixtureCharA, 93300005})

	// A killmail participant: never parted of the orbit.
	km := esi.Killmail{KillmailID: 7, Victim: esi.KillmailVictim{CharacterID: 93300099, ShipTypeID: 587},
		Attackers: []esi.KillmailAttacker{{CharacterID: 93300098, FinalBlow: true}}}
	payload, _ := json.Marshal(km)
	if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
		KillmailID: 7, CharacterID: fixtureCharA, Hash: "h", Payload: string(payload),
		FetchedAt: "2026-10-02T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed killmail detail: %v", err)
	}

	app.notePilotOrbit(ctx)

	recorded, err := q.ListPilotRecordIDs(ctx)
	if err != nil {
		t.Fatalf("list pilot records: %v", err)
	}
	got := make(map[int64]bool)
	for _, id := range recorded {
		got[id] = true
	}
	for _, id := range []int64{93300001, 93300002, 93300004, 93300003, 93300005} {
		if !got[id] {
			t.Fatalf("orbit missed counterparty %d; recorded=%v", id, recorded)
		}
	}
	for _, id := range []int64{fixtureCharA, fixtureCharB, 93300099, 93300098, 98000002} {
		if got[id] {
			t.Fatalf("orbit wrongly noted %d", id)
		}
	}

	// Orbit rows are background priority; a viewed pilot bumps
	// ahead of the whole orbit in the drain.
	var priority int64
	if err := conn.QueryRowContext(ctx,
		`SELECT priority FROM pilot_records WHERE character_id = 93300001`).Scan(&priority); err != nil {
		t.Fatalf("read orbit priority: %v", err)
	}
	if priority != 0 {
		t.Fatalf("orbit priority = %d, want 0", priority)
	}
	if err := q.UpsertPilotWant(ctx, 93300003); err != nil {
		t.Fatalf("viewed want: %v", err)
	}
	ids, err := q.ListPilotDrains(ctx, db.ListPilotDrainsParams{
		StaleCutoff: time.Now().UTC().Add(-pilotStaleAfter).Format(time.RFC3339),
		DrainLimit:  5,
	})
	if err != nil {
		t.Fatalf("list drains: %v", err)
	}
	if len(ids) == 0 || ids[0] != 93300003 {
		t.Fatalf("drain order = %v, viewed want 93300003 should lead", ids)
	}
}

// ---------------------------------------------------------------------------
// Urgent drain: a fresh want is fetched within one nudge, the
// 20h gate stops a repeat, and an error limit silences the loop.
// ---------------------------------------------------------------------------

func TestUrgentDrainFetchesAndGates(t *testing.T) {
	stub := &historyStub{bodies: map[int64]string{}, fail: map[int64]bool{}}
	stub.bodies[34] = `[{"date":"2026-10-01","average":10.0,"highest":11.0,"lowest":9.0,"volume":100,"order_count":5},{"date":"2026-10-02","average":10.5,"highest":11.5,"lowest":9.5,"volume":90,"order_count":4}]`
	app, _, q := buildCorpTestApp(t, stub)
	ctx := context.Background()
	if err := q.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: forge, TypeID: 34, LastRequestedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed want: %v", err)
	}

	app.urgentDrain(ctx)
	rows, err := q.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: forge, TypeID: 34, RowLimit: 90})
	if err != nil || len(rows) != 2 {
		t.Fatalf("urgent drain stored %d rows (err=%v), want 2", len(rows), err)
	}
	before := stub.calls.Load()
	app.urgentDrain(ctx)
	if got := stub.calls.Load(); got != before {
		t.Fatalf("second nudge fetched again inside the 20h gate (%d extra calls)", got-before)
	}
}

func TestUrgentDrainErrorLimitBacksOff(t *testing.T) {
	stub := &errLimitTransport{}
	app, _, q := buildCorpTestApp(t, stub)
	ctx := context.Background()
	if err := q.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: forge, TypeID: 34, LastRequestedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed want: %v", err)
	}
	app.urgentDrain(ctx)
	before := stub.calls.Load()
	if before == 0 {
		t.Fatal("error-limit drain made no call at all")
	}
	app.urgentDrain(ctx)
	if got := stub.calls.Load(); got != before {
		t.Fatalf("urgent drain kept nudging after an error limit (%d more calls)", got-before)
	}
}

// ---------------------------------------------------------------------------
// Live-region fragments: cache-only renders with the poll wiring;
// the served JS/CSS carry the mechanism; the pending states in
// the real pages point at the fragments.
// ---------------------------------------------------------------------------

func TestLiveRegionFragments(t *testing.T) {
	transport := &marketStubTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seed34SDEType(t, app)

	// frag renders one fragment and proves the transport did not
	// move while it did.
	frag := func(path string) (int, string) {
		t.Helper()
		before := transport.calls.Load()
		code, body := getPage(t, app, cookie, path)
		if got := transport.calls.Load(); got != before {
			t.Fatalf("GET %s made %d outbound calls, want 0", path, got-before)
		}
		return code, body
	}

	// History fragment: pending first, with the live-region attrs.
	code, body := frag("/market/history-fragment?region=10000002&type=34")
	if code != http.StatusOK {
		t.Fatalf("history fragment: status %d", code)
	}
	mustContain(t, "history fragment (pending)", body,
		`data-live-region`, `data-poll-state="pending"`,
		`data-poll-url="/market/history-fragment?region=10000002&amp;type=34"`,
		"This one's queued")

	// Trader fragment: the averages carry the same live-fill
	// contract as the chart. Pending shows the loading
	// treatment; the bests ride in the poll URL so the margin
	// survives the swap.
	code, body = frag("/market/trader-fragment?region=10000002&type=34&bs=12&bb=10")
	if code != http.StatusOK {
		t.Fatalf("trader fragment: status %d", code)
	}
	mustContain(t, "trader fragment (pending)", body,
		`data-live-region`, `data-poll-state="pending"`,
		`data-poll-url="/market/trader-fragment?region=10000002&amp;type=34&amp;bs=12&amp;bb=10"`,
		"loading-pulse", "the averages fill in")

	// Then the chart state once rows exist.
	for _, stmt := range []string{
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count) VALUES (10000002, 34, '2026-09-30', 10, 11, 9, 100, 5)`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count) VALUES (10000002, 34, '2026-10-01', 10.5, 11.5, 9.5, 90, 4)`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count) VALUES (10000002, 34, '2026-10-02', 11, 12, 10, 80, 3)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	code, body = frag("/market/history-fragment?region=10000002&type=34")
	if code != http.StatusOK {
		t.Fatalf("history fragment (chart): status %d", code)
	}
	mustContain(t, "history fragment (chart)", body, `data-poll-state="chart"`, "<svg",
		"3 days of recorded trades in The Forge", "Last recorded days")
	if strings.Contains(body, "data-live-region") {
		t.Fatal("settled history fragment still carries a live region")
	}

	// Trader fragment once rows exist: the averages populate
	// and the margin comes from the passed-along bests — the
	// whole snapshot settles with the chart, no refresh needed.
	code, body = frag("/market/trader-fragment?region=10000002&type=34&bs=12&bb=10")
	if code != http.StatusOK {
		t.Fatalf("trader fragment (chart): status %d", code)
	}
	mustContain(t, "trader fragment (chart)", body,
		`data-poll-state="chart"`, ">10.50 ISK<", "16.7% of the sell price")
	if strings.Contains(body, "data-live-region") {
		t.Fatal("settled trader fragment still carries a live region")
	}

	// Pilot fragment: loading, then ready once the record lands.
	code, body = frag("/pilot/fragment?character=93300077")
	if code != http.StatusOK {
		t.Fatalf("pilot fragment: status %d", code)
	}
	mustContain(t, "pilot fragment (loading)", body,
		`data-live-region`, `data-poll-state="loading"`,
		`data-poll-url="/pilot/fragment?character=93300077"`)
	ready, _ := json.Marshal(pilotPayload{Profile: esi.Character{Name: "Fixture Stranger", SecurityStatus: 0.5}})
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: 93300077, Payload: string(ready), State: pilotStateReady,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed pilot record: %v", err)
	}
	code, body = frag("/pilot/fragment?character=93300077")
	if code != http.StatusOK {
		t.Fatalf("pilot fragment (ready): status %d", code)
	}
	mustContain(t, "pilot fragment (ready)", body, `data-poll-state="ready"`, "Fixture Stranger")

	// Description fragment: pending, cache-only.
	code, body = frag("/items/description-fragment?type=34")
	if code != http.StatusOK {
		t.Fatalf("description fragment: status %d", code)
	}
	mustContain(t, "description fragment", body,
		`data-live-region`, `data-poll-state="pending"`, "This description is queued")

	// The poll mechanism ships in the served assets.
	code, js := getPage(t, app, cookie, "/static/app.js")
	if code != http.StatusOK {
		t.Fatalf("/static/app.js status = %d", code)
	}
	mustContain(t, "/static/app.js", js, "[data-live-region]", "data-poll-state", "setInterval")
	code, css := getPage(t, app, cookie, "/static/style.css")
	if code != http.StatusOK {
		t.Fatalf("/static/style.css status = %d", code)
	}
	mustContain(t, "/static/style.css", css, ".loading-pulse", "prefers-reduced-motion")

	// The market page's pending state points at the fragment.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (35, 'Pyerite', 1, 1, 1)`); err != nil {
		t.Fatalf("seed sde 35: %v", err)
	}
	code, body = getPage(t, app, cookie, "/market/?type=35")
	if code != http.StatusOK {
		t.Fatalf("market item page: status %d", code)
	}
	mustContain(t, "/market/?type=35 (pending)", body, `data-poll-url="/market/history-fragment?region=10000002&amp;type=35"`,
		`class="trader-body"`, `data-poll-url="/market/trader-fragment?region=10000002&amp;type=35`)
}

// ---------------------------------------------------------------------------
// Migration 016: pilot priority column lands idempotently.
// ---------------------------------------------------------------------------

func TestMigration016Reopen(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	for i := 0; i < 2; i++ {
		conn, pool, err := openDB(context.Background(), dsn)
		if err != nil {
			t.Fatalf("openDB (pass %d): %v", i, err)
		}
		var cols int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'pilot_records' AND column_name = 'priority'`).Scan(&cols); err != nil {
			t.Fatalf("priority column check (pass %d): %v", i, err)
		}
		if cols != 1 {
			t.Fatalf("priority column count = %d (pass %d), want 1", cols, i)
		}
		q := db.New(conn)
		if err := q.UpsertPilotWant(ctx, 93300001); err != nil {
			t.Fatalf("pilot want (pass %d): %v", i, err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close (pass %d): %v", i, err)
		}
		pool.Close()
	}
}
