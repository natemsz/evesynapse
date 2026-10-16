package app

import (
	"context"
	"encoding/json"
	"evesynapse/internal/balancechart"
	"fmt"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Wallet graphs: balance over time from data the app already stores —
// the wallet journal's running balances for the recent window and the
// daily wallet-history samples for the longer tail — plus an account
// net-worth history summed from the same samples. Pure functions over
// stored rows and cache-only handlers. A thin history says it is
// building.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The chart: the market history chart's geometry and language
// (720×240 viewBox, ember line, compact-ISK axis, date ticks,
// scrub dots) with the volume band left out — a balance has one
// series, so the line gets the whole plot.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Wallet page balance-history section: states mirror the market
// history discipline (pending / empty / few / chart), and the
// pending state rides the live-region poller so the graph fills
// in on its own while the worker warms the journal.
// ---------------------------------------------------------------------------

const (
	walletGraphPending = "pending" // journal still warming — live region
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
}

// attachWalletGraph builds the section from stored rows: the
// journal snapshot the page already decoded, the character's
// daily samples, and the current wallet snapshot as the "now"
// point. journalLoaded says the journal snapshot itself landed;
// a recorded journal fetch failure also settles the section (the
// pending state must not poll forever behind a dead scope).
func (app *Application) attachWalletGraph(ctx context.Context, userID, characterID int64, journal esi.WalletJournal, journalLoaded bool) *walletGraphView {
	g := &walletGraphView{CharID: characterID}

	var samples []db.WalletHistory
	if rows, err := app.queries.ListWalletHistorySamples(ctx, db.ListWalletHistorySamplesParams{
		UserID: userID, CharacterID: characterID,
	}); err != nil {
		logging.Errorf("wallet graph: samples for character %d: %v", characterID, err)
	} else {
		samples = rows
	}

	var now *balancechart.BalancePoint
	if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: characterID, Kind: esi.SnapWallet,
	}); err == nil {
		var bal float64
		if json.Unmarshal([]byte(snap.Payload), &bal) == nil {
			now = &balancechart.BalancePoint{At: snap.FetchedAt, Balance: bal}
		}
	}

	settled := journalLoaded
	if !settled {
		if state, _, found := app.corpKindState(ctx, characterID, esi.SnapWalletJournal); found &&
			(state == fetchStateError || state == fetchStateRoleMissing) {
			settled = true
		}
	}

	points := balancechart.BuildBalanceSeries(journal, samples, now)
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
		Delta: fmt.Sprintf("%s (%+.1f%%)", compactISKDelta(now-then), pct),
		Text:  fmt.Sprintf("%s ISK (%+.1f%%) over %s", compactISKDelta(now-then), pct, since),
	}
}

// compactISKDelta writes a signed ISK change short: "-1.74B", "+820.5M".
func compactISKDelta(v float64) string {
	sign := "+"
	if v < 0 {
		sign, v = "-", -v
	}
	for _, u := range []struct {
		div    float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if v >= u.div {
			return fmt.Sprintf("%s%.2f%s", sign, v/u.div, u.suffix)
		}
	}
	return fmt.Sprintf("%s%.0f", sign, v)
}

// attachNetWorthHistory fills the home Net worth module's
// history from the user's sampler rows: a chart once two daily
// totals exist, the "building" state before that.
func (app *Application) attachNetWorthHistory(ctx context.Context, w *netWorthWidget, userID int64) {
	rows, err := app.queries.ListUserWalletHistory(ctx, userID)
	if err != nil {
		logging.Errorf("net worth history for user %d: %v", userID, err)
		return
	}
	days := netWorthBreakdownDays(rows)
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
	if len(days) >= 2 {
		if chart, ok := buildNetWorthChart(days); ok {
			c := chart
			w.History = &c
		}
		return
	}
	if w.Any {
		w.HistoryBuilding = true
	}
}
