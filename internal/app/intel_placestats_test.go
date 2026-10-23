package app

// Tests for the place activity collection (schema 075) and the
// system/region pages' activity and sovereignty blocks: the
// collector against ESI-shaped fixtures, the distillation math
// over seeded hourly history, and page renders with and without
// collected data. Page renders run against the counting
// transport, which must stay at zero calls.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// placeStatsStub serves the collector's three public endpoints
// (plus factions) from fixtures, counting calls.
type placeStatsStub struct {
	kills       string
	jumps       string
	sov         string
	factions    string
	killsStatus int // nonzero: the kills endpoint answers this status
	expires     string
	calls       atomic.Int64
}

func (s *placeStatsStub) respond(status int, body string) *http.Response {
	header := http.Header{"Content-Type": []string{"application/json"}}
	if s.expires != "" {
		header.Set("Expires", s.expires)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func (s *placeStatsStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	switch req.URL.Path {
	case "/universe/system_kills/":
		if s.killsStatus != 0 {
			return s.respond(s.killsStatus, `{"error":"fixture failure"}`), nil
		}
		return s.respond(http.StatusOK, s.kills), nil
	case "/universe/system_jumps/":
		return s.respond(http.StatusOK, s.jumps), nil
	case "/sovereignty/map/":
		return s.respond(http.StatusOK, s.sov), nil
	case "/universe/factions/":
		return s.respond(http.StatusOK, s.factions), nil
	}
	return s.respond(http.StatusInternalServerError, `{"error":"unexpected path"}`), nil
}

// ---------------------------------------------------------------------------
// The collector pass.
// ---------------------------------------------------------------------------

func TestPlaceActivityCollector(t *testing.T) {
	ctx := context.Background()
	stub := &placeStatsStub{
		kills: `[{"system_id":30000142,"ship_kills":3,"npc_kills":58,"pod_kills":1},` +
			`{"system_id":30000143,"ship_kills":0,"npc_kills":7,"pod_kills":0}]`,
		jumps:   `[{"system_id":30000142,"ship_jumps":128},{"system_id":30000144,"ship_jumps":9}]`,
		expires: time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat),
	}
	app, conn, q := buildCorpTestApp(t, stub)
	seedPlaceSDE(t, app)

	stored, limited := app.refreshPlaceActivity(ctx)
	if !stored || limited {
		t.Fatalf("refreshPlaceActivity = (%v, %v), want (true, false)", stored, limited)
	}

	// History: one merged row per system named by either payload.
	var rows int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM system_activity_hours`).Scan(&rows); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if rows != 3 {
		t.Fatalf("history rows = %d, want 3", rows)
	}
	var ship, npc, pod, jumps int64
	if err := conn.QueryRowContext(ctx, `SELECT ship_kills, npc_kills, pod_kills, ship_jumps
		FROM system_activity_hours WHERE system_id = 30000142`).Scan(&ship, &npc, &pod, &jumps); err != nil {
		t.Fatalf("read Jita history: %v", err)
	}
	if ship != 3 || npc != 58 || pod != 1 || jumps != 128 {
		t.Errorf("Jita history = %d/%d/%d/%d, want 3/58/1/128", ship, npc, pod, jumps)
	}

	// Distilled figures: the one collected hour is the last hour,
	// the whole 24h sum, and a daily average scaled by the single
	// hour of coverage.
	stats, err := q.GetSystemActivityStats(ctx, 30000142)
	if err != nil {
		t.Fatalf("Jita stats: %v", err)
	}
	if stats.ShipKills1h != 3 || stats.ShipKills24h != 3 || stats.Jumps1h != 128 || stats.Jumps24h != 128 {
		t.Errorf("Jita stats 1h/24h = %d/%d kills, %d/%d jumps, want 3/3, 128/128",
			stats.ShipKills1h, stats.ShipKills24h, stats.Jumps1h, stats.Jumps24h)
	}
	if stats.ShipKillsDay != 72 || stats.JumpsDay != 128*24 {
		t.Errorf("Jita daily averages = %v kills, %v jumps, want 72, %v", stats.ShipKillsDay, stats.JumpsDay, float64(128*24))
	}
	if stats.HoursCovered != 1 {
		t.Errorf("hours covered = %d, want 1", stats.HoursCovered)
	}

	// Region figures sum the region's SDE systems only (the jumps
	// payload's 30000144 is not in the seeded region).
	region, err := q.GetRegionActivityStats(ctx, 10000002)
	if err != nil {
		t.Fatalf("region stats: %v", err)
	}
	if region.ShipKills1h != 3 || region.NpcKills1h != 65 || region.Jumps1h != 128 {
		t.Errorf("region 1h = %d ship, %d npc, %d jumps, want 3/65/128",
			region.ShipKills1h, region.NpcKills1h, region.Jumps1h)
	}

	// The gate: inside ESI's cache window a second pass fetches
	// nothing.
	calls := stub.calls.Load()
	stored, limited = app.refreshPlaceActivity(ctx)
	if stored || limited {
		t.Fatalf("gated refreshPlaceActivity = (%v, %v), want (false, false)", stored, limited)
	}
	if got := stub.calls.Load(); got != calls {
		t.Errorf("gated pass made %d more calls, want 0", got-calls)
	}
	meta, err := q.GetPlaceStatsMeta(ctx, placeStatsKindActivity)
	if err != nil {
		t.Fatalf("activity meta: %v", err)
	}
	if until := time.Until(meta.CachedUntil); until < 90*time.Minute || until > 150*time.Minute {
		t.Errorf("cached until %v from now, want about 2h (the fixture's Expires)", until)
	}
}

func TestPlaceActivityCollectorFailureKeepsStore(t *testing.T) {
	ctx := context.Background()
	stub := &placeStatsStub{killsStatus: http.StatusInternalServerError}
	app, conn, _ := buildCorpTestApp(t, stub)

	stored, limited := app.refreshPlaceActivity(ctx)
	if stored || limited {
		t.Fatalf("failing refreshPlaceActivity = (%v, %v), want (false, false)", stored, limited)
	}
	var rows int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM system_activity_hours`).Scan(&rows); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if rows != 0 {
		t.Errorf("history rows = %d after failed pass, want 0", rows)
	}
	if _, err := app.queries.GetPlaceStatsMeta(ctx, placeStatsKindActivity); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("meta after failed pass: err = %v, want no row (the pass stays due)", err)
	}
}

func TestPlaceSovereigntyCollector(t *testing.T) {
	ctx := context.Background()
	stub := &placeStatsStub{
		sov: `[{"system_id":30000142,"alliance_id":99000001,"corporation_id":98000001},` +
			`{"system_id":30000143,"corporation_id":98000002},` +
			`{"system_id":30000144,"faction_id":500001}]`,
		factions: `[{"faction_id":500001,"name":"Caldari State","corporation_id":98000003}]`,
		expires:  time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat),
	}
	app, _, q := buildCorpTestApp(t, stub)

	stored, limited := app.refreshPlaceSovereignty(ctx)
	if !stored || limited {
		t.Fatalf("refreshPlaceSovereignty = (%v, %v), want (true, false)", stored, limited)
	}
	jita, err := q.GetSystemSovereignty(ctx, 30000142)
	if err != nil {
		t.Fatalf("Jita sovereignty: %v", err)
	}
	if jita.AllianceID != 99000001 || jita.CorporationID != 98000001 || jita.FactionID != 0 {
		t.Errorf("Jita holder = alliance %d, corp %d, faction %d, want 99000001/98000001/0",
			jita.AllianceID, jita.CorporationID, jita.FactionID)
	}
	perimeter, err := q.GetSystemSovereignty(ctx, 30000143)
	if err != nil {
		t.Fatalf("Perimeter sovereignty: %v", err)
	}
	if perimeter.AllianceID != 0 || perimeter.CorporationID != 98000002 {
		t.Errorf("Perimeter holder = alliance %d, corp %d, want 0/98000002", perimeter.AllianceID, perimeter.CorporationID)
	}
	faction, err := q.GetSystemSovereignty(ctx, 30000144)
	if err != nil {
		t.Fatalf("faction sovereignty: %v", err)
	}
	if faction.FactionID != 500001 || faction.FactionName != "Caldari State" {
		t.Errorf("faction holder = %d %q, want 500001 Caldari State", faction.FactionID, faction.FactionName)
	}

	// Gated inside the cache window.
	calls := stub.calls.Load()
	if stored, _ := app.refreshPlaceSovereignty(ctx); stored {
		t.Fatal("gated refreshPlaceSovereignty stored, want false")
	}
	if got := stub.calls.Load(); got != calls {
		t.Errorf("gated pass made %d more calls, want 0", got-calls)
	}

	// Wholesale replacement: reopen the gate by hand, drop Jita
	// from the payload, and its row must go.
	if _, err := app.db.ExecContext(ctx, `UPDATE place_stats_meta SET cached_until = $1 WHERE kind = $2`,
		time.Now().Add(-time.Hour), placeStatsKindSovereignty); err != nil {
		t.Fatalf("reopen gate: %v", err)
	}
	stub.sov = `[{"system_id":30000143,"corporation_id":98000002}]`
	if stored, _ := app.refreshPlaceSovereignty(ctx); !stored {
		t.Fatal("second refreshPlaceSovereignty did not store")
	}
	if _, err := q.GetSystemSovereignty(ctx, 30000142); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Jita sovereignty after replacement: err = %v, want no row", err)
	}
}

// ---------------------------------------------------------------------------
// The distillation math, over seeded history at fixed hours.
// ---------------------------------------------------------------------------

func seedActivityHistory(t *testing.T, app *Application, rows []struct {
	SystemID int64
	Age      time.Duration // before the distillation moment
	Ship     int64
	NPC      int64
	Pod      int64
	Jumps    int64
}, now time.Time) {
	t.Helper()
	for _, r := range rows {
		if _, err := app.db.ExecContext(context.Background(), `
			INSERT INTO system_activity_hours (system_id, hour, ship_kills, npc_kills, pod_kills, ship_jumps)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			r.SystemID, now.Add(-r.Age), r.Ship, r.NPC, r.Pod, r.Jumps); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
}

func TestPlaceActivityDistillMath(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	seedPlaceSDE(t, app)
	now := time.Now().UTC().Truncate(time.Hour)

	seedActivityHistory(t, app, []struct {
		SystemID int64
		Age      time.Duration
		Ship     int64
		NPC      int64
		Pod      int64
		Jumps    int64
	}{
		{30000142, 1 * time.Hour, 10, 100, 1, 500},
		{30000142, 2 * time.Hour, 20, 200, 2, 700},
		{30000142, 25 * time.Hour, 5, 50, 0, 300},          // inside 7d, outside 24h
		{30000142, 8 * 24 * time.Hour, 999, 999, 999, 999}, // outside 7d entirely
		{30000142, 31 * 24 * time.Hour, 555, 0, 0, 0},      // outside the retention
		{30000143, 1 * time.Hour, 1, 0, 0, 10},
	}, now)

	if err := distillPlaceActivity(ctx, q, now); err != nil {
		t.Fatalf("distill: %v", err)
	}

	jita, err := q.GetSystemActivityStats(ctx, 30000142)
	if err != nil {
		t.Fatalf("Jita stats: %v", err)
	}
	if jita.ShipKills1h != 10 || jita.NpcKills1h != 100 || jita.PodKills1h != 1 || jita.Jumps1h != 500 {
		t.Errorf("Jita 1h = %d/%d/%d/%d, want 10/100/1/500", jita.ShipKills1h, jita.NpcKills1h, jita.PodKills1h, jita.Jumps1h)
	}
	if jita.ShipKills24h != 30 || jita.NpcKills24h != 300 || jita.PodKills24h != 3 || jita.Jumps24h != 1200 {
		t.Errorf("Jita 24h = %d/%d/%d/%d, want 30/300/3/1200", jita.ShipKills24h, jita.NpcKills24h, jita.PodKills24h, jita.Jumps24h)
	}
	// Coverage counts the distinct collected hours inside 7 days:
	// 1h, 2h and 25h back -- not the 8d row.
	if jita.HoursCovered != 3 {
		t.Errorf("hours covered = %d, want 3", jita.HoursCovered)
	}
	if jita.ShipKillsDay != 280 || jita.NpcKillsDay != 2800 || jita.PodKillsDay != 24 || jita.JumpsDay != 12000 {
		t.Errorf("Jita daily = %v/%v/%v/%v, want 280/2800/24/12000",
			jita.ShipKillsDay, jita.NpcKillsDay, jita.PodKillsDay, jita.JumpsDay)
	}

	perimeter, err := q.GetSystemActivityStats(ctx, 30000143)
	if err != nil {
		t.Fatalf("Perimeter stats: %v", err)
	}
	if perimeter.ShipKillsDay != 8 { // 1 kill over 3 covered hours, scaled to a day
		t.Errorf("Perimeter ship daily = %v, want 8", perimeter.ShipKillsDay)
	}

	region, err := q.GetRegionActivityStats(ctx, 10000002)
	if err != nil {
		t.Fatalf("region stats: %v", err)
	}
	if region.ShipKills1h != 11 || region.Jumps1h != 510 {
		t.Errorf("region 1h = %d ship, %d jumps, want 11/510", region.ShipKills1h, region.Jumps1h)
	}
	if region.ShipKills24h != 31 || region.Jumps24h != 1210 {
		t.Errorf("region 24h = %d ship, %d jumps, want 31/1210", region.ShipKills24h, region.Jumps24h)
	}
	if region.ShipKillsDay != 288 {
		t.Errorf("region ship daily = %v, want 288", region.ShipKillsDay)
	}
	if region.HoursCovered != 3 {
		t.Errorf("region hours covered = %d, want 3", region.HoursCovered)
	}

	// Retention: the 31-day-old row prunes, the 8-day-old stays.
	pruned, err := q.PruneSystemActivityHours(ctx, now.Add(-placeActivityRetention))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d rows, want 1", pruned)
	}
}

func TestPlaceActivityDistillStaleLatest(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	seedPlaceSDE(t, app)
	now := time.Now().UTC().Truncate(time.Hour)

	// The system's newest collected hour is five hours old: its
	// values are history, not a "last hour".
	seedActivityHistory(t, app, []struct {
		SystemID int64
		Age      time.Duration
		Ship     int64
		NPC      int64
		Pod      int64
		Jumps    int64
	}{
		{30000142, 5 * time.Hour, 7, 70, 0, 70},
	}, now)
	if err := distillPlaceActivity(ctx, q, now); err != nil {
		t.Fatalf("distill: %v", err)
	}
	stats, err := q.GetSystemActivityStats(ctx, 30000142)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.ShipKills1h != 0 || stats.Jumps1h != 0 {
		t.Errorf("stale 1h = %d ship, %d jumps, want 0/0", stats.ShipKills1h, stats.Jumps1h)
	}
	if stats.ShipKills24h != 7 || stats.Jumps24h != 70 {
		t.Errorf("24h = %d ship, %d jumps, want 7/70", stats.ShipKills24h, stats.Jumps24h)
	}
	if stats.ShipKillsDay != 7*24 {
		t.Errorf("ship daily = %v, want %v (one covered hour)", stats.ShipKillsDay, float64(7*24))
	}
}

// ---------------------------------------------------------------------------
// The pages, with and without collected data.
// ---------------------------------------------------------------------------

func seedPlaceStatsRows(t *testing.T, app *Application) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO system_activity_stats (system_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
			ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
			ship_kills_day, pod_kills_day, npc_kills_day, jumps_day, hours_covered, updated_at)
		 VALUES (30000142, 3, 1, 58, 128, 30, 4, 400, 2000, 25.5, 3.0, 380.5, 1900.5, 30, NOW())`,
		`INSERT INTO region_activity_stats (region_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
			ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
			ship_kills_day, pod_kills_day, npc_kills_day, jumps_day, hours_covered, updated_at)
		 VALUES (10000002, 9, 2, 65, 140, 90, 8, 500, 2400, 80.5, 7.5, 450.5, 2300.5, 200, NOW())`,
		`INSERT INTO system_sovereignty (system_id, alliance_id, corporation_id, faction_id, faction_name, updated_at)
		 VALUES (30000142, 99000001, 98000001, 0, '', NOW()), (30000143, 0, 98000002, 0, '', NOW())`,
	} {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed place stats: %v", err)
		}
	}
}

func TestPlacePagesWithActivity(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)
	seedPlaceStatsRows(t, app)
	app.esi.StoreAllianceName(99000001, "Fixture Alliance")
	app.esi.StoreCorpName(98000002, "Fixture Corp")

	code, body := getPage(t, app, cookie, "/system/?system=30000142")
	if code != http.StatusOK {
		t.Fatalf("GET /system/: status %d", code)
	}
	mustContain(t, "/system/", body,
		`<a class="h-action" href="/map/?system=30000142" title="Open Jita on the universe map"><svg viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="1.8" stroke-linecap="round"><path d="M6.2 7.4 11.6 5.6M12.8 6.6l4.6 5.2M16.6 13.4l-5.4 4.4M9.6 17.2 6.6 9"/></g><g fill="url(#nav-glyph-gradient)"><circle cx="5.6" cy="7.8" r="2"/><circle cx="12.2" cy="5.2" r="1.7"/><circle cx="18.2" cy="12.6" r="2.2"/><circle cx="10.2" cy="18.6" r="1.9"/></g></svg>Map</a>`,
		`<a href="/map/?region=10000002">Map</a>`,
		`<tr><th>Sovereignty</th><td><a href="/alliance/?alliance=99000001">Fixture Alliance</a></td></tr>`,
		`<th>Daily average (last day)</th>`,
		`<tr><th>Ship kills</th><td>3</td><td>30</td><td>25.5</td></tr>`,
		`<tr><th>Pod kills</th><td>1</td><td>4</td><td>3.0</td></tr>`,
		`<tr><th>NPC kills</th><td>58</td><td>400</td><td>380.5</td></tr>`,
		`<tr><th>Jumps</th><td>128</td><td>2,000</td><td>1,900.5</td></tr>`)
	if strings.Contains(body, "evemaps") {
		t.Error("system page still carries an external map link")
	}

	code, body = getPage(t, app, cookie, "/region/?region=10000002")
	if code != http.StatusOK {
		t.Fatalf("GET /region/: status %d", code)
	}
	mustContain(t, "/region/", body,
		`<a class="h-action" href="/map/?region=10000002" title="Open The Forge on the universe map">Map</a>`,
		`<th>Daily average</th>`,
		`<tr><th>Ship kills</th><td>9</td><td>90</td><td>80.5</td></tr>`,
		`<tr><th>Jumps</th><td>140</td><td>2,400</td><td>2,300.5</td></tr>`,
		`<a href="/alliance/?alliance=99000001">Fixture Alliance</a>`,
		`<a href="/corporation/?corporation=98000002">Fixture Corp</a>`,
		`<th>Kills (24h)</th><th>Jumps (24h)</th>`,
		`<tr><td><a href="/system/?system=30000142">Jita</a></td><td><span class="sec sec-9">0.9</span></td><td>30</td><td>2,000</td></tr>`,
		`<tr><td><a href="/system/?system=30000143">Perimeter</a></td><td><span class="sec sec-9">0.9</span></td><td>0</td><td>0</td></tr>`)
	if strings.Contains(body, "evemaps") {
		t.Error("region page still carries an external map link")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestPlacePagesWithoutActivity(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	code, body := getPage(t, app, cookie, "/system/?system=30000142")
	if code != http.StatusOK {
		t.Fatalf("GET /system/: status %d", code)
	}
	mustContain(t, "/system/ without stats", body,
		`<a class="h-action" href="/map/?system=30000142" title="Open Jita on the universe map"><svg viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="1.8" stroke-linecap="round"><path d="M6.2 7.4 11.6 5.6M12.8 6.6l4.6 5.2M16.6 13.4l-5.4 4.4M9.6 17.2 6.6 9"/></g><g fill="url(#nav-glyph-gradient)"><circle cx="5.6" cy="7.8" r="2"/><circle cx="12.2" cy="5.2" r="1.7"/><circle cx="18.2" cy="12.6" r="2.2"/><circle cx="10.2" cy="18.6" r="1.9"/></g></svg>Map</a>`,
		"No activity data collected yet.")
	if strings.Contains(body, "Sovereignty") {
		t.Error("system page shows a sovereignty row with none collected")
	}

	code, body = getPage(t, app, cookie, "/region/?region=10000002")
	if code != http.StatusOK {
		t.Fatalf("GET /region/: status %d", code)
	}
	mustContain(t, "/region/ without stats", body,
		`<a class="h-action" href="/map/?region=10000002" title="Open The Forge on the universe map">Map</a>`,
		"No activity data collected yet.")
	if strings.Contains(body, "<h2>Sovereignty</h2>") {
		t.Error("region page shows a sovereignty block with none collected")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Label and format helpers.
// ---------------------------------------------------------------------------

func TestAvgWindowLabel(t *testing.T) {
	cases := []struct {
		hours int32
		want  string
	}{
		{200, "Daily average"},
		{168, "Daily average"},
		{167, "Daily average (last 6 days)"},
		{48, "Daily average (last 2 days)"},
		{47, "Daily average (last day)"},
		{24, "Daily average (last day)"},
		{23, "Daily average (last 23 hours)"},
		{1, "Daily average (last hour)"},
	}
	for _, c := range cases {
		if got := avgWindowLabel(c.hours); got != c.want {
			t.Errorf("avgWindowLabel(%d) = %q, want %q", c.hours, got, c.want)
		}
	}
}

func TestFormatDayAvg(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{280, "280.0"},
		{12053.33, "12,053.3"},
		{0.5, "0.5"},
		{25.5, "25.5"},
		{1900.5, "1,900.5"},
	}
	for _, c := range cases {
		if got := formatDayAvg(c.v); got != c.want {
			t.Errorf("formatDayAvg(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}
