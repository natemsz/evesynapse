package app

// Load test for the web tier: drives the real chi router over
// HTTP with many concurrent signed-in sessions against a fixture
// database seeded at market scale (thousands of types of region
// stats, station stats and history), and records throughput,
// latency percentiles and database-lock failures per stage.
//
// Skipped unless EVESYNAPSE_LOADTEST=1: the normal suite must
// never pay for this. Run it directly:
//
//	EVESYNAPSE_LOADTEST=1 go test ./internal/app -run TestLoadWebTier -count=1 -v
//
// The ESI side is a canned in-process stub (no network), so the
// numbers measure handler + template + SQLite cost. The market
// item page in production additionally waits on a live book
// fetch per view; that external wait is not in these figures.
// Client and server share one process here, so the load
// generator competes with the server for CPU — treat the
// absolute rates as conservative and the latency-vs-concurrency
// shape as the transferable result.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	loadTypeBase  = int64(50000) // seeded type IDs run 50000..53999
	loadTypeCount = 4000
	loadForge     = int64(10000002)
	loadDomain    = int64(10000043)
	loadStationA  = int64(60003760) // Jita 4 - Moon 4 (fixture SDE)
	loadStationB  = int64(60008494) // Amarr VIII (fixture SDE)
	loadCharBase  = int64(92000000)
	loadCharCount = 12
)

// loadStubTransport answers every ESI call from canned payloads:
// the price guide as an empty list with a long Expires (so the
// in-memory price cache behaves as it does in production), order
// books as a generated 400-order book, everything else as an
// empty object or list.
type loadStubTransport struct{ calls atomic.Int64 }

func (s *loadStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	path := req.URL.Path
	body := "{}"
	header := http.Header{"Content-Type": []string{"application/json"}}
	switch {
	case strings.Contains(path, "/markets/prices/"):
		body = "[]"
		header.Set("Expires", time.Now().Add(6*time.Hour).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	case strings.Contains(path, "/orders/"):
		body = loadStubBook
	case strings.HasSuffix(path, "/assets/"):
		body = "[]"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// loadStubBook is one type's canned order book: 200 sells and
// 200 buys around a base price, at the fixture station and its
// solar system.
var loadStubBook = func() string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 200; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"order_id":%d,"type_id":50001,"location_id":60003760,"system_id":30000142,"is_buy_order":false,"price":%.2f,"volume_remain":%d,"volume_total":%d,"range":"region","duration":90,"issued":"2026-01-01T00:00:00Z"}`,
			1000000+i, 100+float64(i)*0.37, 1000+i, 2000+i)
	}
	for i := 0; i < 200; i++ {
		b.WriteString(",")
		fmt.Fprintf(&b, `{"order_id":%d,"type_id":50001,"location_id":60003760,"system_id":30000142,"is_buy_order":true,"price":%.2f,"volume_remain":%d,"volume_total":%d,"range":"region","duration":90,"issued":"2026-01-01T00:00:00Z"}`,
			2000000+i, 92-float64(i)*0.31, 900+i, 1500+i)
	}
	b.WriteString("]")
	return b.String()
}()

// loadItemCursor rotates market-item views across the seeded
// type range so no single item page is cache-warm for everyone.
var loadItemCursor atomic.Int64

// loadUserID is filled by seedLoadFixture (one fixture user owns
// all twelve load characters).
var loadUserID int64

// loadMixName names the page mix of the current run for the
// report ("full" or "light").
var loadMixName = "full"

// loadDiagnostics carries end-of-run observations (snapshot
// rewrites and the loudest handler log lines) into the report.
var loadDiagnostics string

type loadPage struct {
	key  string
	path func(worker int) string
}

// loadStageResult is one concurrency stage's measured outcome.
type loadStageResult struct {
	Concurrency int
	Requests    int
	Errors      int
	Non200      int
	Elapsed     time.Duration
	RPS         float64
	P50, P95    time.Duration
	P99         time.Duration
	SlowestPage string
	SlowestP95  time.Duration
	SlowestP99  time.Duration
	LockedErrs  int
	PeakRSSMB   float64
	Goroutines  int
	Pages       map[string]loadPageStat
}

// loadPageStat is one page type's latency summary in a stage.
type loadPageStat struct {
	Count         int
	P50, P95, P99 time.Duration
}

// lockedLogBuf is a goroutine-safe sink for the standard logger
// during load stages, so database-lock errors that handlers only
// log (rather than surface in a page) are still counted.
type lockedLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuf) lockedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	n := 0
	for _, needle := range []string{"database is locked", "database table is locked", "SQLITE_BUSY", "sqlite_busy"} {
		n += strings.Count(s, needle)
	}
	return n
}

func (b *lockedLogBuf) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// topPrefixes summarizes the buffer's most frequent log-line
// prefixes (timestamp stripped), so the report can show which
// handlers were logging failures under load.
func (b *lockedLogBuf) topPrefixes(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	counts := map[string]int{}
	for _, line := range strings.Split(b.buf.String(), "\n") {
		// Standard log lines start "2006/01/02 15:04:05 ".
		if len(line) > 20 {
			line = line[20:]
		}
		if len(line) > 70 {
			line = line[:70]
		}
		if line != "" {
			counts[line]++
		}
	}
	type kv struct {
		k string
		v int
	}
	var pairs []kv
	for k, v := range counts {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	var out []string
	for i, p := range pairs {
		if i >= n {
			break
		}
		out = append(out, fmt.Sprintf("%dx %s", p.v, p.k))
	}
	return strings.Join(out, "\n")
}

// loadRSSMB returns the test process's peak resident set in MB.
func loadRSSMB() float64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseFloat(fields[1], 64)
				if err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}

// loadLatency summarizes a duration sample.
func loadLatency(samples []time.Duration) (p50, p95, p99 time.Duration) {
	if len(samples) == 0 {
		return 0, 0, 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	at := func(p float64) time.Duration {
		idx := int(p * float64(len(samples)))
		if idx >= len(samples) {
			idx = len(samples) - 1
		}
		return samples[idx]
	}
	return at(0.50), at(0.95), at(0.99)
}

func loadMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestLoadWebTier(t *testing.T) {
	if os.Getenv("EVESYNAPSE_LOADTEST") != "1" {
		t.Skip("load test disabled; set EVESYNAPSE_LOADTEST=1 to run")
	}

	transport := &loadStubTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	seedLoadFixture(t, conn, q)

	server := httptest.NewServer(app.Handler())
	defer server.Close()
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConns: 2048, MaxIdleConnsPerHost: 2048},
		Timeout:   120 * time.Second,
	}

	// One session per seeded character; workers use the session
	// matching their own character so per-character pages render
	// that character's data.
	cookies := make([]*http.Cookie, loadCharCount)
	for i := 0; i < loadCharCount; i++ {
		cookies[i] = sessionCookie(t, app, loadUserID, loadCharBase+int64(i)+1,
			fmt.Sprintf("Load Pilot %02d", i+1))
	}

	pages := []loadPage{
		{"home", func(w int) string { return "/" }},
		{"market-item", func(w int) string {
			typeID := loadTypeBase + loadItemCursor.Add(1)%loadTypeCount
			return fmt.Sprintf("/market/?type=%d", typeID)
		}},
		{"scanner", func(w int) string { return "/market/scanner/" }},
		{"tradefinder", func(w int) string { return "/market/tradefinder/" }},
		{"orders", func(w int) string {
			return fmt.Sprintf("/orders/?character=%d", loadCharBase+int64(w%loadCharCount)+1)
		}},
		{"character", func(w int) string {
			return fmt.Sprintf("/character/?character=%d", loadCharBase+int64(w%loadCharCount)+1)
		}},
	}
	// Weighted mix summing to 20 draws: home 4, item 4, scanner 5,
	// tradefinder 3, orders 2, character 2. The "light" mix drops
	// the two aggregate-market pages so the rest of the site's
	// ceiling can be measured without them (they are measured on
	// their own in the full mix).
	weights := []int{4, 4, 5, 3, 2, 2}
	loadMixName = "full"
	if os.Getenv("EVESYNAPSE_LOADTEST_MIX") == "light" {
		loadMixName = "light (scanner and tradefinder excluded)"
		weights = []int{5, 5, 0, 0, 3, 3}
	}
	var draw []int
	for i, n := range weights {
		for j := 0; j < n; j++ {
			draw = append(draw, i)
		}
	}

	logs := &lockedLogBuf{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	runStage := func(concurrency int, dur time.Duration) loadStageResult {
		var (
			mu        sync.Mutex
			requests  int64
			errorsN   int64
			non200    int64
			lockedAt0 = logs.lockedCount()
		)
		samples := map[string][]time.Duration{}
		deadline := time.Now().Add(dur)
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < concurrency; w++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(worker)*7919))
				for time.Now().Before(deadline) {
					page := pages[draw[rng.Intn(len(draw))]]
					path := page.path(worker)
					req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
					if err != nil {
						atomic.AddInt64(&errorsN, 1)
						continue
					}
					req.AddCookie(cookies[worker%loadCharCount])
					t0 := time.Now()
					resp, err := client.Do(req)
					elapsed := time.Since(t0)
					if err != nil {
						atomic.AddInt64(&errorsN, 1)
						continue
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					atomic.AddInt64(&requests, 1)
					if resp.StatusCode != http.StatusOK {
						atomic.AddInt64(&non200, 1)
					}
					mu.Lock()
					samples[page.key] = append(samples[page.key], elapsed)
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		elapsed := time.Since(start)

		var all []time.Duration
		slowest, slowestP95, slowestP99 := "", time.Duration(0), time.Duration(0)
		pageStats := map[string]loadPageStat{}
		for key, ss := range samples {
			all = append(all, ss...)
			p50, p95, p99 := loadLatency(ss)
			pageStats[key] = loadPageStat{Count: len(ss), P50: p50, P95: p95, P99: p99}
			if p95 > slowestP95 {
				slowest, slowestP95, slowestP99 = key, p95, p99
			}
		}
		p50, p95, p99 := loadLatency(all)
		return loadStageResult{
			Concurrency: concurrency,
			Requests:    int(requests),
			Errors:      int(errorsN),
			Non200:      int(non200),
			Elapsed:     elapsed,
			RPS:         float64(requests) / elapsed.Seconds(),
			P50:         p50, P95: p95, P99: p99,
			SlowestPage: slowest, SlowestP95: slowestP95, SlowestP99: slowestP99,
			LockedErrs: logs.lockedCount() - lockedAt0,
			PeakRSSMB:  loadRSSMB(),
			Goroutines: runtime.NumGoroutine(),
			Pages:      pageStats,
		}
	}

	// Warmup so template compilation and first-touch caches are
	// not charged to the first measured stage.
	_ = runStage(16, 3*time.Second)

	var results []loadStageResult
	for _, conc := range []int{10, 50, 100, 200} {
		r := runStage(conc, 15*time.Second)
		results = append(results, r)
		t.Logf("stage %3d: %5.0f req/s, p50 %v p95 %v p99 %v, non200 %d, errors %d, locked %d, slowest %s p95 %v",
			r.Concurrency, r.RPS, r.P50, r.P95, r.P99, r.Non200, r.Errors, r.LockedErrs, r.SlowestPage, r.SlowestP95)
	}
	last := results[len(results)-1]
	badRate := float64(last.Non200+last.Errors) / float64(loadMaxInt(last.Requests+last.Errors, 1))
	if badRate < 0.01 {
		r := runStage(400, 15*time.Second)
		results = append(results, r)
		t.Logf("stage %3d: %5.0f req/s, p50 %v p95 %v p99 %v, non200 %d, errors %d, locked %d, slowest %s p95 %v",
			r.Concurrency, r.RPS, r.P50, r.P95, r.P99, r.Non200, r.Errors, r.LockedErrs, r.SlowestPage, r.SlowestP95)
	} else {
		t.Logf("stage 400 skipped: stage 200 bad-response rate %.2f%% >= 1%%", badRate*100)
	}

	// Diagnostics: snapshots were seeded with FetchedAt
	// 2026-01-01T00:00:00Z, so any row stamped otherwise was
	// rewritten by a render during the run — a direct count of
	// render-path writes. The loudest handler log lines show
	// which pages were logging failures.
	var overwritten int64
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM character_snapshots WHERE fetched_at != '2026-01-01T00:00:00Z'`).Scan(&overwritten); err != nil {
		t.Logf("snapshot rewrite count: %v", err)
	}
	loadDiagnostics = fmt.Sprintf("Snapshot rows rewritten by renders: %d\n\nMost frequent handler log lines:\n%s",
		overwritten, logs.topPrefixes(8))

	writeLoadReport(t, results, transport.calls.Load())
}

// seedLoadRows writes one multi-row INSERT inside tx. rowSQL
// holds one parenthesized values tuple per row.
func seedLoadRows(t *testing.T, tx *sql.Tx, table string, rowSQL []string) {
	t.Helper()
	for i := 0; i < len(rowSQL); i += 400 {
		end := i + 400
		if end > len(rowSQL) {
			end = len(rowSQL)
		}
		stmt := "INSERT INTO " + table + " VALUES " + strings.Join(rowSQL[i:end], ",")
		if _, err := tx.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("seed %s rows %d-%d: %v", table, i, end, err)
		}
	}
}

// seedLoadFixture builds the load dataset: one user with twelve
// characters and their snapshots, the SDE rows the market pages
// join against, and market stats/history at regional scale.
func seedLoadFixture(t *testing.T, conn *sql.DB, q *db.Queries) {
	t.Helper()
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	loadUserID = user.ID

	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	defer tx.Rollback()

	// SDE anchors the market pages join against.
	sde := []string{
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge'), (10000043, 'Domain')`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9), (30002187, 'Amarr', 10000043, 0.9)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142), (60008494, 'Amarr VIII (Oris) - Emperor Family Academy', 30002187)`,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (34, 'Tritanium', 18), (35, 'Pyerite', 18)`,
	}
	for _, stmt := range sde {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	var typeRows []string
	for i := int64(0); i < loadTypeCount; i++ {
		typeRows = append(typeRows, fmt.Sprintf("(%d, 'Load Item %d', 18)", loadTypeBase+i, i))
	}
	seedLoadRows(t, tx, "sde_types (type_id, name, group_id)", typeRows)

	// Region stats for both hubs across every seeded type. Forge
	// buys sit ~8% under Forge sells; Domain sells sit ~6% over
	// Forge buys, so the tradefinder finds real margins. The
	// stored avg_daily_volume (schema 035) matches the history
	// seeded below, so the scanner and tradefinder read the same
	// sold-per-day figure a completed sweep would have stored.
	var regionRows []string
	for i := int64(0); i < loadTypeCount; i++ {
		typeID := loadTypeBase + i
		sell := 100 + float64(i%500) + float64(i%7)*0.13
		avgVol := 0.0
		for d := 0; d < 7; d++ {
			avgVol += float64(50 + (i*37+int64(d)*11)%4000)
		}
		avgVol /= 7
		for _, region := range []struct {
			id    int64
			scale float64
		}{{loadForge, 1.0}, {loadDomain, 1.11}} {
			s := sell * region.scale
			regionRows = append(regionRows, fmt.Sprintf(
				"(%d, %d, %.2f, %.2f, %.2f, %.2f, %.2f, %.2f, %d, %d, %d, %d, '%s', %.2f)",
				region.id, typeID,
				s*0.99, s, s*1.05, // best_sell, typical_sell, sell_band
				s*0.91, s*0.92, s*0.88, // best_buy, typical_buy, buy_band
				40+i%60, 25+i%40, 100000+i*10, 80000+i*7, nowText, avgVol))
		}
	}
	seedLoadRows(t, tx, "market_region_stats (region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume)", regionRows)

	// Station stats at each hub's fixture station; spreads are
	// wide (sell ~9% over buy) so the scanner fills its 100 rows.
	var stationRows []string
	for i := int64(0); i < loadTypeCount; i++ {
		typeID := loadTypeBase + i
		sell := 100 + float64(i%500) + float64(i%7)*0.13
		stationRows = append(stationRows, fmt.Sprintf(
			"(%d, %d, %d, %.2f, %.2f, %d, %d, %d, %d, '%s')",
			loadStationA, loadForge, typeID, sell, sell*0.91, 3+i%20, 2+i%9, 5000+i*3, 4000+i*2, nowText))
		stationRows = append(stationRows, fmt.Sprintf(
			"(%d, %d, %d, %.2f, %.2f, %d, %d, %d, %d, '%s')",
			loadStationB, loadDomain, typeID, sell*1.11, sell*1.02, 2+i%15, 1+i%7, 3000+i*2, 2500+i, nowText))
	}
	seedLoadRows(t, tx, "market_station_stats (location_id, region_id, type_id, best_sell, best_buy, sell_orders, buy_orders, sell_volume, buy_volume, updated_at)", stationRows)

	// Seven days of history for every type in both regions: the
	// window the scanner and tradefinder average over.
	var historyRows []string
	for d := 0; d < 7; d++ {
		day := now.AddDate(0, 0, -d).Format("2006-01-02")
		for i := int64(0); i < loadTypeCount; i++ {
			typeID := loadTypeBase + i
			vol := 50 + (i*37+int64(d)*11)%4000
			avg := 100 + float64(i%500)
			for _, region := range []int64{loadForge, loadDomain} {
				historyRows = append(historyRows, fmt.Sprintf(
					"(%d, %d, '%s', %.2f, %.2f, %.2f, %d, %d)",
					region, typeID, day, avg, avg*1.05, avg*0.95, vol, 20+vol/50))
			}
		}
	}
	seedLoadRows(t, tx, "market_history (region_id, type_id, date, average, highest, lowest, volume, order_count)", historyRows)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}

	// Characters and their snapshots.
	for c := 0; c < loadCharCount; c++ {
		charID := loadCharBase + int64(c) + 1
		seedCharacter(t, q, loadUserID, charID, fmt.Sprintf("Load Pilot %02d", c+1))

		seedSnapshot(t, q, charID, esi.SnapWallet, `987654321.12`)
		seedSnapshot(t, q, charID, esi.SnapSkills, fmt.Sprintf(`{"total_sp":%d,"unallocated_sp":0,"skills":[]}`, 40000000+c*1000000))
		seedSnapshot(t, q, charID, esi.SnapOnline, `{"is_online":true,"last_login":"2026-01-01T00:00:00Z","last_logout":"2026-01-01T00:00:00Z","logins":128}`)
		seedSnapshot(t, q, charID, esi.SnapLocation, `{"solar_system_id":30000142,"station_id":60003760}`)
		seedSnapshot(t, q, charID, esi.SnapShip, `{"ship_type_id":50001,"ship_item_id":1,"ship_name":"Load Ship"}`)
		seedSnapshot(t, q, charID, esi.SnapSkillqueue, `[]`)
		seedSnapshot(t, q, charID, esi.SnapClones, `{"home_location":{"location_id":60003760,"location_type":"station"},"jump_clones":[]}`)
		seedSnapshot(t, q, charID, esi.SnapImplants, `[]`)
		seedSnapshot(t, q, charID, esi.SnapFatigue, `{}`)

		// 120 asset stacks per character.
		var assets strings.Builder
		assets.WriteString("[")
		for i := 0; i < 120; i++ {
			if i > 0 {
				assets.WriteString(",")
			}
			fmt.Fprintf(&assets, `{"item_id":%d,"type_id":%d,"location_id":60003760,"location_type":"station","location_flag":"Hangar","quantity":%d,"is_singleton":false}`,
				int64(10000000+c*1000+i), loadTypeBase+int64(i%200), 100+i)
		}
		assets.WriteString("]")
		seedSnapshot(t, q, charID, esi.SnapAssets, assets.String())

		// 15 open orders per character.
		var orders strings.Builder
		orders.WriteString("[")
		for i := 0; i < 15; i++ {
			if i > 0 {
				orders.WriteString(",")
			}
			fmt.Fprintf(&orders, `{"order_id":%d,"type_id":%d,"location_id":60003760,"region_id":10000002,"is_buy_order":%t,"price":%.2f,"volume_total":%d,"volume_remain":%d,"range":"station","duration":90,"issued":"2026-01-01T00:00:00Z"}`,
				int64(5000000+c*100+i), loadTypeBase+int64(i*3), i%2 == 0, 100+float64(i)*3.7, 1000+i*10, 700+i*5)
		}
		orders.WriteString("]")
		seedSnapshot(t, q, charID, esi.SnapOrders, orders.String())
	}

	// Order lifecycle history: 40 closed + 10 open per character
	// for the Orders page's "Your order history" section.
	tx2, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin lifecycle tx: %v", err)
	}
	defer tx2.Rollback()
	var lifeRows []string
	for c := 0; c < loadCharCount; c++ {
		charID := loadCharBase + int64(c) + 1
		for i := 0; i < 50; i++ {
			typeID := loadTypeBase + int64((i*13+c)%loadTypeCount)
			first := now.Add(-time.Duration(20+i) * 24 * time.Hour).Format(time.RFC3339)
			last := now.Add(-time.Duration(i) * 24 * time.Hour).Format(time.RFC3339)
			closedAt, kind := last, "filled"
			if i >= 40 {
				closedAt, kind = "", ""
			} else if i%3 == 0 {
				kind = "ended"
			}
			lifeRows = append(lifeRows, fmt.Sprintf(
				"(%d, %d, %d, %d, %d, %d, %.2f, %d, %d, '%s', '%s', '%s', '%s', %d, %d)",
				charID, int64(7000000+c*1000+i), typeID, loadStationA, loadForge, i%2,
				100+float64(i)*2.3, 1000, 0, first, last, closedAt, kind, i%5, 0))
		}
	}
	seedLoadRows(t, tx2, "order_lifecycle (character_id, order_id, type_id, location_id, region_id, is_buy_order, listed_price, volume_total, volume_remain_last, first_seen_at, last_seen_at, closed_at, close_kind, outbid_events, beaten_now)", lifeRows)
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit lifecycle tx: %v", err)
	}
}

// writeLoadReport writes the stage table and methodology notes
// to the workspace logs directory and mirrors it into the test
// log.
func writeLoadReport(t *testing.T, results []loadStageResult, stubCalls int64) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# EveSynapse web-tier load test\n\n")
	b.WriteString(fmt.Sprintf("Run: %s\n\n", time.Now().UTC().Format("2006-01-02 15:04 UTC")))
	b.WriteString("## Environment\n\n")
	b.WriteString(fmt.Sprintf("- Sandbox host: %d CPUs visible to the test process, %s/%s, %s\n", runtime.NumCPU(), runtime.GOOS, runtime.GOARCH, runtime.Version()))
	b.WriteString("- Database: PostgreSQL 16 (embedded test server on this host) via the app's own openDB; session store and handlers are production code paths.\n")
	b.WriteString("- ESI: in-process canned stub, no network. Item-page figures exclude the production live book fetch's network wait; stub book is 400 orders.\n")
	b.WriteString("- Load generator and server share this process, so generator CPU competes with the server: absolute rates are conservative; the latency-vs-concurrency shape is the transferable result.\n")
	b.WriteString("- Dataset: 12 characters with wallet/skills/assets (120 stacks each)/orders snapshots; 4,000 types x 2 regions of region stats and station stats; 56,000 history rows (7 days x 4,000 types x 2 regions); 600 order-lifecycle rows.\n")
	b.WriteString(fmt.Sprintf("- Outbound ESI stub calls during the whole test: %d (prices once per cache expiry, one book fetch per item view).\n\n", stubCalls))
	b.WriteString("## Page mix (weighted)\n\n")
	b.WriteString(fmt.Sprintf("Mix: %s. Full mix: home 20%%, market item 20%%, spread scanner 25%%, tradefinder 15%%, orders 10%%, character 10%%. Light mix: home, item, orders and character only. Each worker uses its own character's session.\n\n", loadMixName))
	b.WriteString("## Stages (15 s each, after a 3 s warmup)\n\n")
	b.WriteString("| Concurrency | Requests | Req/s | p50 | p95 | p99 | Slowest page (p95 / p99) | Non-200 | Errors | DB-locked log hits | Peak RSS (MB) | Goroutines |\n")
	b.WriteString("|---:|---:|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %d | %d | %.0f | %v | %v | %v | %s (%v / %v) | %d | %d | %d | %.0f | %d |\n",
			r.Concurrency, r.Requests, r.RPS, r.P50.Round(time.Millisecond), r.P95.Round(time.Millisecond), r.P99.Round(time.Millisecond),
			r.SlowestPage, r.SlowestP95.Round(time.Millisecond), r.SlowestP99.Round(time.Millisecond),
			r.Non200, r.Errors, r.LockedErrs, r.PeakRSSMB, r.Goroutines)
	}
	b.WriteString("\nDB-locked hits are occurrences of 'database is locked'/'SQLITE_BUSY' in handler log output during the stage (pages log such failures rather than always failing the request).\n")

	b.WriteString("\n## Per-page latencies by stage\n")
	for _, r := range results {
		b.WriteString(fmt.Sprintf("\nConcurrency %d:\n\n", r.Concurrency))
		b.WriteString("| Page | Views | p50 | p95 | p99 |\n|---|---:|---:|---:|---:|\n")
		keys := make([]string, 0, len(r.Pages))
		for key := range r.Pages {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			s := r.Pages[key]
			fmt.Fprintf(&b, "| %s | %d | %v | %v | %v |\n", key, s.Count,
				s.P50.Round(time.Millisecond), s.P95.Round(time.Millisecond), s.P99.Round(time.Millisecond))
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	if loadDiagnostics != "" {
		b.WriteString("\n## Diagnostics\n\n")
		b.WriteString(loadDiagnostics)
		b.WriteString("\n")
	}
	reportName := os.Getenv("EVESYNAPSE_LOADTEST_REPORT")
	if reportName == "" {
		reportName = "loadtest-2026-10-04.md"
	}
	path := filepath.Join(home, "workspace", "evesynapse", "logs", reportName)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("load report written to %s", path)
}
