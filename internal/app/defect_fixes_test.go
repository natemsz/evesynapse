package app

// Hermetic tests for the three-defect fix build: player structure
// name resolution (queued, background, cache-only renders), the
// four-state market-history section, and the planets scope-missing
// classification of ESI 401s.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// defectTransport routes the endpoints the defect tests exercise
// and records every request path in order.
type defectTransport struct {
	mu         sync.Mutex
	paths      []string
	struct403  bool // structures answer 403
	planets401 bool // planets endpoints answer 401
}

func (s *defectTransport) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (s *defectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
	switch {
	case strings.Contains(path, "/planets/"):
		if s.planets401 {
			return s.respond(401, `{"error":"token lacks scope"}`), nil
		}
		return s.respond(200, `[]`), nil
	case strings.Contains(path, "/universe/structures/"):
		if s.struct403 {
			return s.respond(403, `{"error":"forbidden"}`), nil
		}
		return s.respond(200, `{"name":"Perimeter >> Tranquility Trading Tower","solar_system_id":30000142,"type_id":35834}`), nil
	case strings.Contains(path, "/history/"):
		return s.respond(200, `[]`), nil
	default:
		return s.respond(404, `{"error":"unexpected path `+path+`"}`), nil
	}
}

func (s *defectTransport) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

// ---------------------------------------------------------------------------
// Defect 1: structure names
// ---------------------------------------------------------------------------

func TestStructureResolutionSuccessAndCacheOnlyRender(t *testing.T) {
	transport := &defectTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	ch.Scopes = "esi-universe.read_structures.v1 esi-skills.read_skills.v1"

	const structureID = int64(1044752365771)
	app.noteStructureIDs(ctx, structureID, 60003760 /* station: ignored */)
	row, err := q.GetStructureName(ctx, structureID)
	if err != nil || row.State != esi.StructurePending {
		t.Fatalf("queued row: %+v err=%v, want pending", row, err)
	}

	resolved, limited := app.resolveStructureNames(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if limited || resolved != 1 {
		t.Fatalf("resolve: resolved=%d limited=%v, want 1/false", resolved, limited)
	}
	row, err = q.GetStructureName(ctx, structureID)
	if err != nil || row.State != esi.StructureResolved || row.Name != "Perimeter >> Tranquility Trading Tower" {
		t.Fatalf("resolved row: %+v err=%v", row, err)
	}
	if got := transport.callCount(); got != 1 {
		t.Fatalf("resolve made %d calls, want 1", got)
	}

	// Render tier: cached name, and the fallback helpers read it
	// without the network.
	if name := app.orderLocation(ctx, structureID, 30000142); name != "Perimeter >> Tranquility Trading Tower" {
		t.Fatalf("orderLocation: %q, want the resolved structure name", name)
	}
	// Fresh cache entry: a second resolve pass fetches nothing.
	resolved, _ = app.resolveStructureNames(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if resolved != 0 || transport.callCount() != 1 {
		t.Fatalf("second pass: resolved=%d calls=%d, want 0/1", resolved, transport.callCount())
	}
}

func TestStructureRenderZeroOutbound(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	const structureID = int64(1044752365771)
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: structureID, Name: "Jita Holding", State: esi.StructureResolved, Source: esi.StructureSourceESI,
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed structure name: %v", err)
	}
	if name := app.orderLocation(ctx, structureID, 30000142); name != "Jita Holding" {
		t.Fatalf("orderLocation: %q, want Jita Holding", name)
	}
	if title := app.locationTitle(ctx, structureID, "structure"); title != "Jita Holding" {
		t.Fatalf("locationTitle: %q, want Jita Holding", title)
	}
	if title := app.econLocationTitle(ctx, 1044752365772); title != "Structure #1044752365772" {
		t.Fatalf("econLocationTitle unknown structure: %q, want fallback", title)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("structure renders made %d outbound calls, want 0", got)
	}
}

func TestStructureResolutionNegativeCache(t *testing.T) {
	transport := &defectTransport{struct403: true}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	ch.Scopes = "esi-universe.read_structures.v1"

	const structureID = int64(1044752365999)
	app.noteStructureIDs(ctx, structureID)
	resolved, _ := app.resolveStructureNames(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("403 resolve: resolved=%d, want 0", resolved)
	}
	row, err := q.GetStructureName(ctx, structureID)
	if err != nil || row.State != esi.StructureMissing {
		t.Fatalf("row after 403: %+v err=%v, want missing", row, err)
	}
	calls := transport.callCount()
	// Fresh negative entry: not re-asked.
	app.resolveStructureNames(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if transport.callCount() != calls {
		t.Fatalf("fresh negative was re-asked: calls %d → %d", calls, transport.callCount())
	}
	// After 24h it is due again.
	stale := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: structureID, Name: "", State: esi.StructureMissing, Source: esi.StructureSourceESI, ResolvedAt: stale,
	}); err != nil {
		t.Fatalf("backdate miss: %v", err)
	}
	app.resolveStructureNames(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if transport.callCount() != calls+1 {
		t.Fatalf("stale negative not retried: calls %d, want %d", transport.callCount(), calls+1)
	}
}

func TestStructureResolutionRenameRefreshAndScopeGate(t *testing.T) {
	transport := &defectTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// No scope on the login: the queue waits, nothing is fetched.
	const structureID = int64(1044752365771)
	app.noteStructureIDs(ctx, structureID)
	resolved, _ := app.resolveStructureNames(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if resolved != 0 || transport.callCount() != 0 {
		t.Fatalf("unscoped resolve: resolved=%d calls=%d, want 0/0", resolved, transport.callCount())
	}

	// A rename-aged entry (31 days) re-resolves once the scope is
	// present.
	stale := time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: structureID, Name: "Old Name", State: esi.StructureResolved, Source: esi.StructureSourceESI, ResolvedAt: stale,
	}); err != nil {
		t.Fatalf("seed stale name: %v", err)
	}
	ch.Scopes = "esi-universe.read_structures.v1"
	resolved, _ = app.resolveStructureNames(ctx, []db.Character{ch}, &fetchBudget{left: 120})
	if resolved != 1 {
		t.Fatalf("stale resolve: resolved=%d, want 1", resolved)
	}
	row, err := q.GetStructureName(ctx, structureID)
	if err != nil || row.Name != "Perimeter >> Tranquility Trading Tower" {
		t.Fatalf("row after rename refresh: %+v err=%v", row, err)
	}
}

func TestStructureQueuedFromMarketBookView(t *testing.T) {
	// The screenshot scenario: best buy sits at a player
	// structure. Viewing the item renders the fallback today and
	// queues the structure for background resolution.
	transport := &historyStub{
		bodies: map[int64]string{}, fail: map[int64]bool{},
		bookJSON: `[
			{"order_id":1,"type_id":34,"location_id":1044752365771,"system_id":30000142,"price":9.5,"volume_remain":100,"volume_total":100,"is_buy_order":true,"range":"region","issued":"2026-10-01T00:00:00Z","duration":90},
			{"order_id":2,"type_id":34,"location_id":60003760,"system_id":30000142,"price":10.5,"volume_remain":100,"volume_total":100,"is_buy_order":false,"range":"region","issued":"2026-10-01T00:00:00Z","duration":90}
		]`,
	}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed34SDEType(t, app)
	cookie := seedItemPage(t, app, q)

	_, body := getPage(t, app, cookie, "/market/?type=34")
	mustContain(t, "/market/?type=34 (structure book)", body, "Structure #1044752365771")
	row, err := q.GetStructureName(ctx, 1044752365771)
	if err != nil || row.State != esi.StructurePending {
		t.Fatalf("book-view queue row: %+v err=%v, want pending", row, err)
	}
	// The station id was NOT queued.
	if _, err := q.GetStructureName(ctx, 60003760); err == nil {
		t.Fatal("station id 60003760 was queued as a structure")
	}
}

func TestMigration014Reopen(t *testing.T) {
	transport := &countingTransport{}
	_, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	var tables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'structure_names'`).Scan(&tables); err != nil {
		t.Fatalf("count 014 table: %v", err)
	}
	if tables != 1 {
		t.Fatalf("structure_names tables: %d, want 1", tables)
	}
	if err := q.UpsertStructureSeen(ctx, 42); err != nil {
		t.Fatalf("queue on fresh DB: %v", err)
	}
	row, err := q.GetStructureName(ctx, 42)
	if err != nil || row.State != esi.StructurePending {
		t.Fatalf("fresh queue row: %+v err=%v", row, err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("migration test made %d outbound calls, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Defect 2: history states + drain priority
// ---------------------------------------------------------------------------

// historyStub serves per-type history bodies (default empty) and
// records the order types were fetched in.
type historyStub struct {
	mu       sync.Mutex
	bodies   map[int64]string
	fail     map[int64]bool
	fetched  []int64
	calls    atomic.Int64
	bookJSON string // orders response body (default "[]")
}

func (s *historyStub) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (s *historyStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	path := req.URL.Path
	switch {
	case strings.Contains(path, "/history/"):
		var typeID int64
		if v := req.URL.Query().Get("type_id"); v != "" {
			for _, c := range v {
				if c >= '0' && c <= '9' {
					typeID = typeID*10 + int64(c-'0')
				}
			}
		}
		s.mu.Lock()
		s.fetched = append(s.fetched, typeID)
		s.mu.Unlock()
		if s.fail[typeID] {
			return s.respond(500, `{"error":"boom"}`), nil
		}
		if body, ok := s.bodies[typeID]; ok {
			return s.respond(200, body), nil
		}
		return s.respond(200, `[]`), nil
	case path == "/markets/prices/":
		return s.respond(200, `[{"type_id":34,"average_price":10.5,"adjusted_price":9.8}]`), nil
	case strings.Contains(path, "/orders/"):
		if s.bookJSON != "" {
			return s.respond(200, s.bookJSON), nil
		}
		return s.respond(200, `[]`), nil
	default:
		return s.respond(404, `{"error":"unexpected path `+path+`"}`), nil
	}
}

func TestHistoryWantsDrainPriority(t *testing.T) {
	transport := &historyStub{bodies: map[int64]string{}, fail: map[int64]bool{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	now := time.Now().UTC().Format(time.RFC3339)
	// A viewed-item want (5001), a watchlist entry (5002), and an
	// open order for 5003.
	if err := q.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: 10000002, TypeID: 5001, LastRequestedAt: now,
	}); err != nil {
		t.Fatalf("seed want: %v", err)
	}
	if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
		UserID: user.ID, TypeID: 5002, RegionID: 10000002, ThresholdPct: 5, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed watchlist: %v", err)
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 900, TypeID: 5003, LocationID: 60003760, RegionID: 10000002, Price: 10,
			VolumeRemain: 5, VolumeTotal: 5, Issued: "2026-10-01T00:00:00Z", Duration: 90},
	})

	stored, limited := app.warmMarketHistory(ctx, &fetchBudget{left: 120})
	if limited || stored != 3 {
		t.Fatalf("drain: stored=%d limited=%v, want 3/false", stored, limited)
	}
	want := []int64{5001, 5002, 5003}
	for i := range want {
		if transport.fetched[i] != want[i] {
			t.Fatalf("fetch order: got %v, want %v (wants first, then watchlist, then orders)", transport.fetched, want)
		}
	}
}

func TestHistoryFailedFirstWantDoesNotStarveOthers(t *testing.T) {
	transport := &historyStub{
		bodies: map[int64]string{5002: `[{"date":"2026-10-01","average":10.0,"highest":11.0,"lowest":9.0,"volume":100,"order_count":5}]`},
		fail:   map[int64]bool{5001: true},
	}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range []int64{5001, 5002} {
		if err := q.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: 10000002, TypeID: id, LastRequestedAt: now,
		}); err != nil {
			t.Fatalf("seed want %d: %v", id, err)
		}
	}
	stored, _ := app.warmMarketHistory(ctx, &fetchBudget{left: 120})
	if stored != 1 {
		t.Fatalf("drain with first want failing: stored=%d, want 1 (the healthy want)", stored)
	}
	rows := app.recentHistoryRows(ctx, 10000002, 5002, 90)
	if len(rows) != 1 {
		t.Fatalf("healthy want rows: %d, want 1", len(rows))
	}
	state, err := q.GetMarketFetchState(ctx, "history_10000002_5001")
	if err != nil || state.State != fetchStateError {
		t.Fatalf("failed want state: %+v err=%v, want error", state, err)
	}
}

// itemPageApp builds the market item page fixture: a signed-in
// user and SDE type 34 (Tritanium) so /market/?type=34 renders.
func seedItemPage(t *testing.T, app *Application, q *db.Queries) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	return sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
}

func seed34SDEType(t *testing.T, app *Application) {
	t.Helper()
	if _, err := app.db.Exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 1, 1, 1)`); err != nil {
		t.Fatalf("seed SDE type: %v", err)
	}
}

func TestHistoryEmptySettlesToNoHistoryCopy(t *testing.T) {
	transport := &historyStub{bodies: map[int64]string{}, fail: map[int64]bool{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed34SDEType(t, app)
	cookie := seedItemPage(t, app, q)

	// First view: nothing stored, section says it's loading and
	// leaves a want.
	_, body := getPage(t, app, cookie, "/market/?type=34")
	mustContain(t, "/market/?type=34 (cold)", body, "This one's queued")

	// The worker drains the want; ESI answers with no trades.
	stored, _ := app.warmMarketHistory(ctx, &fetchBudget{left: 120})
	if stored != 1 {
		t.Fatalf("empty drain: stored=%d, want 1", stored)
	}

	// The section now settles to the no-history copy — not
	// loading, not blank.
	_, body = getPage(t, app, cookie, "/market/?type=34")
	mustContain(t, "/market/?type=34 (settled empty)", body,
		"No trade history in The Forge for this item yet.")
	if strings.Contains(body, "This one's queued") {
		t.Fatal("settled-empty page still claims loading")
	}
	if idx := strings.Index(body, "Price history — The Forge"); idx >= 0 {
		section := body[idx:]
		if end := strings.Index(section, "</section>"); end >= 0 {
			section = section[:end]
		}
		if !strings.Contains(section, "No trade history") {
			t.Fatal("history section does not carry the settled copy")
		}
	} else {
		t.Fatal("history section missing entirely")
	}
}

func TestHistoryFewRowsSummaryAndStaleChart(t *testing.T) {
	transport := &historyStub{bodies: map[int64]string{}, fail: map[int64]bool{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed34SDEType(t, app)
	cookie := seedItemPage(t, app, q)
	now := time.Now().UTC()

	seed := func(daysAgo int, avg float64) {
		t.Helper()
		day := now.AddDate(0, 0, -daysAgo).Format(historyDateLayout)
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 10000002, TypeID: 34, Date: day, Average: avg,
			Highest: avg, Lowest: avg, Volume: 5000, OrderCount: 90,
		}); err != nil {
			t.Fatalf("seed history %s: %v", day, err)
		}
	}

	// One recorded day: honest summary, no chart.
	seed(0, 10.8)
	_, body := getPage(t, app, cookie, "/market/?type=34")
	mustContain(t, "/market/?type=34 (one row)", body,
		"Only 1 day of recorded trades in The Forge")
	// (v0.3.07.003: the nav carries inline SVG icons, so the
	// no-chart check scopes to the chart container itself.)
	if strings.Contains(body, `class="pchart"`) {
		t.Fatal("one-row page drew a chart")
	}
	if strings.Contains(body, "This one's queued") {
		t.Fatal("one-row page claims loading")
	}

	// Two rows, both older than the old 95-day calendar window
	// and newest >14 days old: the row window still charts them
	// and captions the last trade date.
	seed(120, 10.0)
	seed(150, 9.0)
	_, body = getPage(t, app, cookie, "/market/?type=34")
	mustContain(t, "/market/?type=34 (stale sparse)", body, `class="pchart"`)

	// A type whose newest trades are 120 days old: caption shows.
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 35, Name: "Mexallon"}); err != nil {
		t.Fatalf("seed type 35 name: %v", err)
	}
	for _, d := range []struct {
		ago int
		avg float64
	}{{120, 20.0}, {150, 19.0}} {
		day := now.AddDate(0, 0, -d.ago).Format(historyDateLayout)
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 10000002, TypeID: 35, Date: day, Average: d.avg,
			Highest: d.avg, Lowest: d.avg, Volume: 5000, OrderCount: 90,
		}); err != nil {
			t.Fatalf("seed history 35 %s: %v", day, err)
		}
	}
	_, body = getPage(t, app, cookie, "/market/?type=35")
	mustContain(t, "/market/?type=35 (stale)", body,
		`class="pchart"`, "Last trades "+now.AddDate(0, 0, -120).Format(historyDateLayout))
}

func TestHistoryChartRowWindow(t *testing.T) {
	transport := &historyStub{bodies: map[int64]string{}, fail: map[int64]bool{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 100; i++ {
		day := now.AddDate(0, 0, -i).Format(historyDateLayout)
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 10000002, TypeID: 34, Date: day, Average: 10,
			Highest: 10, Lowest: 10, Volume: 100, OrderCount: 5,
		}); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	rows := app.recentHistoryRows(ctx, 10000002, 34, historyChartRows)
	if len(rows) != historyChartRows {
		t.Fatalf("window: %d rows, want %d", len(rows), historyChartRows)
	}
	oldest := now.AddDate(0, 0, -(historyChartRows - 1)).Format(historyDateLayout)
	if rows[0].Date != oldest || rows[len(rows)-1].Date != now.Format(historyDateLayout) {
		t.Fatalf("window span: %s → %s, want %s → %s", rows[0].Date, rows[len(rows)-1].Date, oldest, now.Format(historyDateLayout))
	}
	// Ascending for the chart math.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Date > rows[i].Date {
			t.Fatalf("rows not ascending at %d: %s > %s", i, rows[i-1].Date, rows[i].Date)
		}
	}
}

func TestHistorySectionNeverBlank(t *testing.T) {
	// Every state renders a deliberate body; drive attachHistory
	// directly and check the state machine's edges.
	transport := &historyStub{bodies: map[int64]string{}, fail: map[int64]bool{}}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	item := &marketItem{TypeID: 34, RegionID: 10000002, RegionName: "The Forge"}
	app.attachHistory(ctx, item, 34, 10000002, 0)
	if item.HistoryState != historyStatePending || !item.HistoryPending {
		t.Fatalf("cold state: %q pending=%v, want pending", item.HistoryState, item.HistoryPending)
	}

	// Settled empty: worker completed the fetch, zero rows.
	app.recordMarketFetch(ctx, "history_10000002_34", fetchStateOK, "")
	item = &marketItem{TypeID: 34, RegionID: 10000002, RegionName: "The Forge"}
	app.attachHistory(ctx, item, 34, 10000002, 0)
	if item.HistoryState != historyStateEmpty {
		t.Fatalf("settled state: %q, want empty", item.HistoryState)
	}

	// Failed fetch: back to pending, never empty-by-silence.
	app.recordMarketFetch(ctx, "history_10000002_34", fetchStateError, "boom")
	item = &marketItem{TypeID: 34, RegionID: 10000002, RegionName: "The Forge"}
	app.attachHistory(ctx, item, 34, 10000002, 0)
	if item.HistoryState != historyStatePending {
		t.Fatalf("errored state: %q, want pending", item.HistoryState)
	}

	// Two recent rows chart.
	for i := 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format(historyDateLayout)
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 10000002, TypeID: 34, Date: day, Average: 10,
			Highest: 10, Lowest: 10, Volume: 100, OrderCount: 5,
		}); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}
	item = &marketItem{TypeID: 34, RegionID: 10000002, RegionName: "The Forge"}
	app.attachHistory(ctx, item, 34, 10000002, 0)
	if item.HistoryState != historyStateChart || item.Chart == nil {
		t.Fatalf("chart state: %q chart=%v, want chart", item.HistoryState, item.Chart != nil)
	}
	if item.HistoryLastDay != "" {
		t.Fatalf("fresh chart flagged stale: %q", item.HistoryLastDay)
	}
}

// ---------------------------------------------------------------------------
// Defect 3: planets 401 classification
// ---------------------------------------------------------------------------

func TestPlanetsScopeMissing401(t *testing.T) {
	transport := &defectTransport{planets401: true}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	// No planetary scope on this login (pre-Phase-2 link).
	ch.Scopes = "esi-skills.read_skills.v1"

	outcome := app.fetchPlanetsKind(ctx, ch, &fetchBudget{left: 120})
	if outcome != corpFetchFailed {
		t.Fatalf("planets 401 outcome: %v, want failed (recorded, backed off)", outcome)
	}
	state, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{
		CharacterID: fixtureCharA, Kind: esi.SnapPlanets,
	})
	if err != nil {
		t.Fatalf("fetch state: %v", err)
	}
	if !strings.HasPrefix(state.Detail, piScopeDetail) {
		t.Fatalf("planets 401 detail: %q, want the PI scope-missing detail", state.Detail)
	}
	// The flag the pages render from:
	if !app.piNotEnabled(ctx, ch) {
		t.Fatal("piNotEnabled: got false after a 401, want true")
	}
	// And the token was NOT parked.
	row, err := q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("get character: %v", err)
	}
	if row.LinkState != linkStateOK {
		t.Fatalf("link state after planets 401: %q, want ok", row.LinkState)
	}

	// Layouts classify the same way.
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, []esi.Colony{
		{PlanetID: 40123456, SolarSystemID: 30000142},
	})
	fetched, limited := app.warmPlanetLayouts(ctx, ch, &fetchBudget{left: 120})
	if fetched != 0 || limited {
		t.Fatalf("layout 401: fetched=%d limited=%v, want 0/false", fetched, limited)
	}
	layoutState, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{
		CharacterID: fixtureCharA, Kind: esi.PlanetLayoutKind(40123456),
	})
	if err != nil || !strings.HasPrefix(layoutState.Detail, piScopeDetail) {
		t.Fatalf("layout fetch state: %+v err=%v, want PI scope-missing detail", layoutState, err)
	}

	// Backoff: a second list attempt inside the backoff window
	// makes no new ESI call.
	before := transport.callCount()
	if outcome := app.fetchPlanetsKind(ctx, ch, &fetchBudget{left: 120}); outcome != corpFetchSkipped {
		t.Fatalf("backed-off planets fetch: outcome %v, want skipped", outcome)
	}
	if got := transport.callCount(); got != before {
		t.Fatalf("backed-off fetch still called ESI: %d → %d", before, got)
	}
}

func TestPlanets401ClassifierBoundary(t *testing.T) {
	// The classifier itself: 401 and 403 on the planets path are
	// scope refusals; other statuses and transport errors are not.
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&esi.StatusError{Method: "GET", Path: "/characters/1/planets/", Code: 401}, true},
		{&esi.StatusError{Method: "GET", Path: "/characters/1/planets/", Code: 403}, true},
		{&esi.StatusError{Method: "GET", Path: "/characters/1/planets/", Code: 404}, false},
		{&esi.StatusError{Method: "GET", Path: "/characters/1/planets/", Code: 500}, false},
		{io.ErrUnexpectedEOF, false},
	} {
		if got := piScopeRefusal(tc.err); got != tc.want {
			t.Errorf("piScopeRefusal(%v): got %v, want %v", tc.err, got, tc.want)
		}
	}
	// Regression: generic 401 handling elsewhere is untouched —
	// the shared token-death test still classifies a 401 as
	// definitive for the endpoints it serves.
	if !isDefinitiveTokenFailure(&esi.StatusError{Method: "GET", Path: "/characters/1/skills/", Code: 401}) {
		t.Fatal("isDefinitiveTokenFailure(401): got false, want true (unchanged)")
	}
}
