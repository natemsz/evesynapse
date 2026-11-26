package app

import (
	"context"
	"evesynapse/internal/balancechart"
	"fmt"
	json "github.com/goccy/go-json"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
)

// ---------------------------------------------------------------------------
// Wallet graphs: balance over time from data the app already stores,
// the wallet journal's running balances for the recent window and the
// daily wallet-history samples for the longer tail, plus an account
// net-worth history summed from the same samples. Pure functions over
// stored rows and cache-only handlers. A thin history says it is
// building.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The chart: the market history chart's geometry and language
// (720×240 viewBox, ember line, compact-ISK axis, date ticks,
// scrub dots) with the volume band left out, a balance has one
// series, so the line gets the whole plot.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Wallet page balance-history section: states mirror the market
// history discipline (pending / empty / few / chart), and the
// pending state rides the live-region poller so the graph fills
// in on its own while the worker warms the journal.
// ---------------------------------------------------------------------------

const (
	walletGraphPending = "pending" // journal still warming, live region
	walletGraphEmpty   = "empty"   // settled, nothing recorded yet
	walletGraphFew     = "few"     // one point: a fact, not a shape
	walletGraphChart   = "chart"   // enough points to draw
)

// walletGraphView is the wallet page's balance-history section.
type walletGraphView struct {
	State   string
	Chart   *balancechart.BalanceChart
	Points  int    // recorded points behind the state copy
	Latest  string // newest balance, formatted (few state)
	CharID  int64
	PollURL string // set only while pending
	Range   string // the active range key ("" = all recorded history)
	Ranges  []walletGraphRange
}

// walletGraphRange is one range choice on the balance-history chart.
type walletGraphRange struct {
	Key   string
	Label string
	Href  string
	On    bool
}

// walletRangeCutoff resolves a range key to its window start;
// ok=false means all recorded history. Unknown keys are the
// default (all), so a stale or hand-typed range never errors.
func walletRangeCutoff(key string, now time.Time) (time.Time, bool) {
	switch key {
	case "30d":
		return now.Add(-30 * 24 * time.Hour), true
	case "12w":
		return now.Add(-12 * 7 * 24 * time.Hour), true
	case "ytd":
		return time.Date(now.Year(), time.January, 1, 0, 0, 0, 0, time.UTC), true
	}
	return time.Time{}, false
}

func walletGraphRanges(characterID int64, active string) []walletGraphRange {
	href := func(key string) string {
		if key == "" {
			return fmt.Sprintf("/wallet/?character=%d", characterID)
		}
		return fmt.Sprintf("/wallet/?character=%d&range=%s", characterID, key)
	}
	out := make([]walletGraphRange, 0, 4)
	for _, r := range []struct{ key, label string }{
		{"30d", "30 days"}, {"12w", "12 weeks"}, {"ytd", "Year to date"}, {"", "All"},
	} {
		out = append(out, walletGraphRange{Key: r.key, Label: r.label, Href: href(r.key), On: r.key == active})
	}
	return out
}

// attachWalletGraph builds the section from stored rows: the
// journal snapshot the page already decoded, the character's
// daily samples, and the current wallet snapshot as the "now"
// point. journalLoaded says the journal snapshot itself landed;
// a recorded journal fetch failure also settles the section (the
// pending state must not poll forever behind a dead scope).
func (app *Application) attachWalletGraph(ctx context.Context, userID, characterID int64, journal esi.WalletJournal, journalLoaded bool, rangeKey string) *walletGraphView {
	now := time.Now()
	cutoff, bounded := walletRangeCutoff(rangeKey, now)
	if !bounded {
		rangeKey = ""
	}
	g := &walletGraphView{CharID: characterID, Range: rangeKey, Ranges: walletGraphRanges(characterID, rangeKey)}

	var samples []db.WalletHistory
	var err error
	if bounded {
		samples, err = app.queries.ListWalletHistorySamplesSince(ctx, db.ListWalletHistorySamplesSinceParams{
			UserID: userID, CharacterID: characterID, Day: cutoff.Format("2006-01-02"),
		})
	} else {
		samples, err = app.queries.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
			UserID: userID, CharacterID: characterID,
		})
	}
	if err != nil {
		logging.Errorf("wallet graph: samples for character %d: %v", characterID, err)
		samples = nil
	}

	var nowPoint *balancechart.BalancePoint
	if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: characterID, Kind: esi.SnapWallet,
	}); err == nil {
		var bal float64
		if json.Unmarshal([]byte(snap.Payload), &bal) == nil {
			nowPoint = &balancechart.BalancePoint{At: snap.FetchedAt, Balance: bal}
		}
	}

	settled := journalLoaded
	if !settled {
		if state, _, found := app.corpKindState(ctx, characterID, esi.SnapWalletJournal); found &&
			(state == fetchStateError || state == fetchStateRoleMissing) {
			settled = true
		}
	}

	points := balancechart.BuildBalanceSeries(journal, samples, nowPoint)
	if bounded {
		kept := points[:0]
		for _, p := range points {
			if !p.At.Before(cutoff) {
				kept = append(kept, p)
			}
		}
		points = kept
	}
	g.Points = len(points)
	switch {
	case len(points) >= 2:
		if chart, ok := balancechart.BuildBalanceChart(points); ok {
			c := chart
			g.Chart = &c
			g.State = walletGraphChart
		} else {
			g.State = walletGraphFew
			g.Latest = esi.FormatISK(points[len(points)-1].Balance)
		}
	case len(points) == 1 && settled:
		g.State = walletGraphFew
		g.Latest = esi.FormatISK(points[0].Balance)
	case len(points) == 0 && settled:
		g.State = walletGraphEmpty
	default:
		g.State = walletGraphPending
		g.PollURL = fmt.Sprintf("/wallet/graph-fragment?character=%d", characterID)
		if rangeKey != "" {
			g.PollURL += "&range=" + rangeKey
		}
	}
	return g
}

// trendWindow is how far back the At a Glance arrows look.
const trendWindow = 7 * 24 * time.Hour

// trendBaseline is the sampled day the trends compare against: the
// newest at least a week old, or failing that the oldest that is at
// least a day old (a young history still shows a direction).
func trendBaseline(days []netWorthDay, now time.Time) (netWorthDay, bool) {
	var base netWorthDay
	found := false
	for _, d := range days { // ascending
		if now.Sub(d.At) >= trendWindow {
			base, found = d, true
		}
	}
	if found {
		return base, true
	}
	if len(days) > 0 && now.Sub(days[0].At) >= 24*time.Hour {
		return days[0], true
	}
	return netWorthDay{}, false
}

// trendSpan names how far back the baseline is: "7 days", "3 days".
func trendSpan(at, now time.Time) string {
	n := int(now.Sub(at).Hours() / 24)
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

// newKPITrend compares a figure with its baseline; nil when there is
// no baseline to speak of or the change rounds to nothing.
func newKPITrend(now, then float64, since string) *kpiTrend {
	if then <= 0 {
		return nil
	}
	pct := (now - then) / then * 100
	if pct > -0.05 && pct < 0.05 {
		return nil
	}
	return &kpiTrend{
		Up:    pct > 0,
		Delta: fmt.Sprintf("%s (%+.1f%%)", iskCompactSigned(now-then), pct),
		Text:  fmt.Sprintf("%s ISK (%+.1f%%) over %s", iskCompactSigned(now-then), pct, since),
		Since: since,
	}
}

// netWorthHistoryDays is how many sampled days the home reads by
// default: a month, and the day before it for the 30-day change.
const netWorthHistoryDays = 31

// netWorthDays reads the account's last n sampled days (at most a
// year), oldest first, each split into ISK on hand and the rest.
func (app *Application) netWorthDays(ctx context.Context, userID int64, n int, charIDs []int64) ([]netWorthDay, error) {
	rows, err := app.queries.ListUserNetWorthSplitDays(ctx, db.ListUserNetWorthSplitDaysParams{UserID: userID, Days: int32(n), CharacterIds: charIDs})
	if err != nil {
		return nil, err
	}
	out := make([]netWorthDay, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- { // newest first
		t, err := time.Parse(markethistory.DateLayout, rows[i].Day)
		if err != nil {
			continue
		}
		assets := rows[i].Total - rows[i].Isk
		if assets < 0 { // a balance read after the net worth was computed
			assets = 0
		}
		out = append(out, netWorthDay{At: t, ISK: rows[i].Isk, Assets: assets})
	}
	return out, nil
}

// attachNetWorthHistory fills the home Net Worth widget's history from
// the account's daily samples: the last month of days and the trends
// beside the live figures, or the "building" state before two days.
func (app *Application) attachNetWorthHistory(ctx context.Context, w *netWorthWidget, userID int64, charIDs []int64) {
	days, err := app.netWorthDays(ctx, userID, netWorthHistoryDays, charIDs)
	if err != nil {
		logging.Errorf("net worth history for user %d: %v", userID, err)
		return
	}
	if base, ok := trendBaseline(days, time.Now()); ok && w.Any {
		since := trendSpan(base.At, time.Now())
		w.TotalTrend = newKPITrend(w.totalNow, base.Total(), since)
		if w.Wallet != "" {
			w.WalletTrend = newKPITrend(w.walletNow, base.ISK, since)
		}
		if w.AssetsKnown {
			w.AssetsTrend = newKPITrend(w.assetsNow, base.Assets, since)
		}
	}
	w.days = days
	w.HistoryBuilding = w.Any && len(days) < 2
}
