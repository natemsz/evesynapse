package app

// Wallet graphs (v0.3.09): series building from journal +
// sampler + current snapshot, the chart's states, the live-fill
// fragment, and the home net-worth aggregation — all over
// stored rows with a transport that must stay silent.

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func seedWalletSample(t *testing.T, q *db.Queries, userID, characterID int64, day string, balance float64, netWorth sql.NullFloat64) {
	t.Helper()
	if err := q.UpsertWalletHistorySample(context.Background(), db.UpsertWalletHistorySampleParams{
		UserID: userID, CharacterID: characterID, Day: day,
		Balance: balance, NetWorth: netWorth,
		SampledAt: day + "T12:00:00Z",
	}); err != nil {
		t.Fatalf("seed wallet sample %s: %v", day, err)
	}
}

func TestBuildBalanceSeriesJournalOnly(t *testing.T) {
	journal := esi.WalletJournal{
		{ID: 3, Date: "2026-03-03T10:00:00Z", Balance: 300},
		{ID: 1, Date: "2026-03-01T10:00:00Z", Balance: 100},
		{ID: 2, Date: "2026-03-02T10:00:00Z", Balance: 200},
	}
	points := buildBalanceSeries(journal, nil, nil)
	if len(points) != 3 {
		t.Fatalf("points = %d, want 3", len(points))
	}
	for i, want := range []float64{100, 200, 300} {
		if points[i].Balance != want {
			t.Fatalf("points[%d].Balance = %v, want %v (ascending order)", i, points[i].Balance, want)
		}
	}
}

func TestBuildBalanceSeriesSamplerOnly(t *testing.T) {
	samples := []db.WalletHistory{
		{Day: "2026-03-01", Balance: 50, SampledAt: "2026-03-01T09:00:00Z"},
		{Day: "2026-03-02", Balance: 75, SampledAt: "2026-03-02T09:00:00Z"},
	}
	points := buildBalanceSeries(nil, samples, nil)
	if len(points) != 2 || points[0].Balance != 50 || points[1].Balance != 75 {
		t.Fatalf("points = %+v, want [50 75]", points)
	}
}

// The seam: samples before the journal window extend the tail,
// a sample inside the window is dropped (the journal owns that
// ground), and the current snapshot extends the line to its
// fetch time.
func TestBuildBalanceSeriesMergedSeam(t *testing.T) {
	samples := []db.WalletHistory{
		{Day: "2026-02-01", Balance: 10, SampledAt: "2026-02-01T09:00:00Z"},
		{Day: "2026-03-02", Balance: 999, SampledAt: "2026-03-02T09:00:00Z"}, // inside window: dropped
	}
	journal := esi.WalletJournal{
		{ID: 1, Date: "2026-03-01T10:00:00Z", Balance: 100},
		{ID: 2, Date: "2026-03-03T10:00:00Z", Balance: 300},
	}
	now := &balancePoint{At: time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC), Balance: 350}
	points := buildBalanceSeries(journal, samples, now)
	want := []float64{10, 100, 300, 350}
	if len(points) != len(want) {
		t.Fatalf("points = %+v, want balances %v", points, want)
	}
	for i, w := range want {
		if points[i].Balance != w {
			t.Fatalf("points[%d].Balance = %v, want %v", i, points[i].Balance, w)
		}
	}

	// A snapshot stamped at the last journal entry's moment
	// refreshes that point instead of duplicating it.
	same := &balancePoint{At: time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC), Balance: 325}
	points = buildBalanceSeries(journal, nil, same)
	if len(points) != 2 || points[1].Balance != 325 {
		t.Fatalf("equal-timestamp refresh: points = %+v, want last balance 325", points)
	}
}

func TestBuildBalanceChartStates(t *testing.T) {
	at := func(day int, bal float64) balancePoint {
		return balancePoint{At: time.Date(2026, 3, day, 0, 0, 0, 0, time.UTC), Balance: bal}
	}
	if _, ok := buildBalanceChart(nil); ok {
		t.Fatal("no points charted")
	}
	if _, ok := buildBalanceChart([]balancePoint{at(1, 100)}); ok {
		t.Fatal("one point charted — a fact is not a shape")
	}
	chart, ok := buildBalanceChart([]balancePoint{at(1, 100), at(2, 250), at(3, 200)})
	if !ok {
		t.Fatal("three points did not chart")
	}
	if chart.Points == "" || len(chart.Dots) != 3 {
		t.Fatalf("chart geometry missing: points=%q dots=%d", chart.Points, len(chart.Dots))
	}
	if chart.From != "2026-03-01" || chart.To != "2026-03-03" {
		t.Fatalf("axis span = %s..%s, want the real first/last points", chart.From, chart.To)
	}
	// Flat series still draws, mid-band.
	flat, ok := buildBalanceChart([]balancePoint{at(1, 100), at(2, 100)})
	if !ok || flat.Dots[0].Y != flat.Dots[1].Y {
		t.Fatalf("flat series: ok=%v dots=%+v", ok, flat.Dots)
	}
	// Microscopically different values (floating-point summation
	// residue across thousands of assets) draw flat too, not as
	// phantom peaks stretched across the full chart height.
	epsFlat, ok := buildBalanceChart([]balancePoint{
		at(1, 158842302202.96),
		at(2, 158842302202.96002),
		at(3, 158842302202.95999),
	})
	if !ok {
		t.Fatal("epsilon-flat series did not chart")
	}
	for i := 1; i < len(epsFlat.Dots); i++ {
		if epsFlat.Dots[i].Y != epsFlat.Dots[0].Y {
			t.Fatalf("epsilon-flat series not flat: dots=%+v", epsFlat.Dots)
		}
	}
}

// The wallet graph's states over a live app: pending while the
// journal warms, few at one settled point, empty when settled
// with nothing, chart once the journal lands — and the fragment
// fills in without a single outbound call.
func TestWalletGraphStatesAndFragment(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// Nothing stored: pending, wired to poll its own fragment.
	code, body := getPage(t, app, cookie, "/wallet/")
	if code != http.StatusOK {
		t.Fatalf("wallet page: status %d", code)
	}
	mustContain(t, "/wallet/ (pending graph)", body,
		"Balance history", `data-poll-state="pending"`,
		`data-poll-url="/wallet/graph-fragment?character=90000001"`)
	code, body = getPage(t, app, cookie, "/wallet/graph-fragment?character=90000001")
	if code != http.StatusOK || !strings.Contains(body, `data-poll-state="pending"`) {
		t.Fatalf("graph fragment (pending): status %d body %.200s", code, body)
	}

	// One sampled day, journal settled-empty: the "few" state.
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, esi.WalletJournal{})
	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-01", 1234, sql.NullFloat64{})
	g := app.attachWalletGraph(ctx, user.ID, fixtureCharA, nil, true)
	if g.State != walletGraphFew {
		t.Fatalf("one settled point: state = %q, want few", g.State)
	}

	// Settled and bare: empty.
	app2, _, q2 := buildCorpTestApp(t, &countingTransport{})
	user2, err := q2.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user 2: %v", err)
	}
	seedCharacter(t, q2, user2.ID, fixtureCharB, "Second Pilot")
	seedSnapshot(t, q2, fixtureCharB, esi.SnapWalletJournal, esi.WalletJournal{})
	g = app2.attachWalletGraph(ctx, user2.ID, fixtureCharB, nil, true)
	if g.State != walletGraphEmpty {
		t.Fatalf("settled bare: state = %q, want empty", g.State)
	}

	// The journal lands: the fragment now serves the chart.
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, esi.WalletJournal{
		{ID: 1, Date: "2026-03-01T10:00:00Z", Balance: 100},
		{ID: 2, Date: "2026-03-02T10:00:00Z", Balance: 250},
		{ID: 3, Date: "2026-03-03T10:00:00Z", Balance: 200},
	})
	code, body = getPage(t, app, cookie, "/wallet/graph-fragment?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("graph fragment (chart): status %d", code)
	}
	mustContain(t, "graph fragment (chart)", body,
		`data-poll-state="chart"`, `class="pchart"`, "data-balance=")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("wallet graph rendering made %d outbound calls, want 0", got)
	}
}

func TestNetWorthHistoryPoints(t *testing.T) {
	rows := []db.WalletHistory{
		{CharacterID: 1, Day: "2026-03-01", NetWorth: sql.NullFloat64{Float64: 100, Valid: true}},
		{CharacterID: 2, Day: "2026-03-01", NetWorth: sql.NullFloat64{Float64: 50, Valid: true}},
		{CharacterID: 1, Day: "2026-03-02", NetWorth: sql.NullFloat64{}}, // unknown: day stays off
		{CharacterID: 1, Day: "2026-03-03", NetWorth: sql.NullFloat64{Float64: 400, Valid: true}},
	}
	points := netWorthHistoryPoints(rows)
	if len(points) != 2 || points[0].Balance != 150 || points[1].Balance != 400 {
		t.Fatalf("points = %+v, want [150 400] with the unknown day skipped", points)
	}
}

// Home module: two sampled days chart; one day reads "building".
func TestAttachNetWorthHistory(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-01", 100,
		sql.NullFloat64{Float64: 1000, Valid: true})
	w := &netWorthWidget{Any: true}
	app.attachNetWorthHistory(ctx, w, user.ID)
	if w.History != nil || !w.HistoryBuilding {
		t.Fatalf("one day: history=%v building=%v, want chart nil + building", w.History != nil, w.HistoryBuilding)
	}

	seedWalletSample(t, q, user.ID, fixtureCharA, "2026-03-02", 120,
		sql.NullFloat64{Float64: 1400, Valid: true})
	w = &netWorthWidget{Any: true}
	app.attachNetWorthHistory(ctx, w, user.ID)
	if w.History == nil || w.HistoryBuilding {
		t.Fatalf("two days: history=%v building=%v, want chart + no building note", w.History != nil, w.HistoryBuilding)
	}
	if w.History.Label != "Net worth over time" {
		t.Fatalf("chart label = %q", w.History.Label)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("net worth history made %d outbound calls, want 0", got)
	}

	// Full-widget path: buildNetWorth carries the chart too.
	w2 := app.buildNetWorth(ctx, user.ID, nil)
	if w2.History == nil {
		t.Fatal("buildNetWorth did not attach the history chart")
	}
}
