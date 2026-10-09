package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
)

// ---------------------------------------------------------------------------
// Wallet graphs: balance-over-time from data the app
// already stores — the wallet journal's running balances for the
// recent window and the daily wallet-history samples (schema
// 019) for the longer tail — plus an account net-worth history
// summed from the same samples. Everything here is a pure
// function over stored rows or a cache-only handler: no ESI
// call ever leaves this file's readers, and the chart axes
// start at the earliest real point. Nothing is fabricated — a
// thin history says it is building instead of drawing a lie.
// ---------------------------------------------------------------------------

// balancePoint is one (time, balance) observation. The chart
// series is a run of these, ascending.
type balancePoint struct {
	At      time.Time
	Balance float64
}

// buildBalanceSeries merges the three stored sources into one
// ascending series:
//
//   - journal entries contribute their post-transaction balance
//     (fine-grained, recent window);
//   - daily samples contribute one point per sampled day, but
//     only for the stretch *before* the journal window opens —
//     inside the window the journal's own points are strictly
//     better, so a sample landing there is dropped (the seam);
//   - the current wallet snapshot ("now") extends the line to
//     the balance's fetch time when it reaches past the last
//     point.
//
// Equal timestamps collapse to the freshest source (journal
// beats sample, the wallet snapshot beats both).
func buildBalanceSeries(journal esi.WalletJournal, samples []db.WalletHistory, now *balancePoint) []balancePoint {
	var points []balancePoint

	journalPts := make([]balancePoint, 0, len(journal))
	for _, e := range journal {
		t, err := time.Parse(time.RFC3339, e.Date)
		if err != nil {
			continue
		}
		journalPts = append(journalPts, balancePoint{At: t, Balance: e.Balance})
	}
	sort.SliceStable(journalPts, func(i, j int) bool { return journalPts[i].At.Before(journalPts[j].At) })

	if len(journalPts) > 0 {
		windowStart := journalPts[0].At
		for _, s := range samples {
			p := samplePoint(s)
			if !p.At.Before(windowStart) {
				continue
			}
			points = append(points, p)
		}
	} else {
		for _, s := range samples {
			points = append(points, samplePoint(s))
		}
	}
	points = append(points, journalPts...)

	sort.SliceStable(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
	// Collapse equal timestamps, later writers winning.
	deduped := points[:0]
	for _, p := range points {
		if n := len(deduped); n > 0 && deduped[n-1].At.Equal(p.At) {
			deduped[n-1] = p
			continue
		}
		deduped = append(deduped, p)
	}
	points = deduped

	if now != nil && !now.At.IsZero() {
		if n := len(points); n == 0 {
			points = append(points, *now)
		} else if last := &points[n-1]; now.At.After(last.At) {
			points = append(points, *now)
		} else if now.At.Equal(last.At) {
			last.Balance = now.Balance
		}
	}
	return points
}

// samplePoint turns one daily history row into a series point,
// dated when the value was last written that day.
func samplePoint(s db.WalletHistory) balancePoint {
	return balancePoint{At: s.SampledAt, Balance: s.Balance}
}

// netWorthHistoryPoints sums the sampler's net-worth column
// across one user's characters, one point per sampled day. A
// day contributes the characters that have a computed net worth
// that day; days with none stay off the chart rather than
// reading as a crash to zero.
func netWorthHistoryPoints(rows []db.WalletHistory) []balancePoint {
	byDay := make(map[string]float64)
	for _, r := range rows {
		if !r.NetWorth.Valid {
			continue
		}
		byDay[r.Day] += r.NetWorth.Float64
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	points := make([]balancePoint, 0, len(days))
	for _, d := range days {
		t, err := time.Parse(markethistory.DateLayout, d)
		if err != nil {
			continue
		}
		points = append(points, balancePoint{At: t, Balance: byDay[d]})
	}
	return points
}

// ---------------------------------------------------------------------------
// The chart: the market history chart's geometry and language
// (720×240 viewBox, ember line, compact-ISK axis, date ticks,
// scrub dots) with the volume band left out — a balance has one
// series, so the line gets the whole plot.
// ---------------------------------------------------------------------------

// walletChartMaxDots caps the rendered points: a busy journal
// window holds thousands of entries, and past a few hundred
// dots the polyline reads the same while the page bloats.
// Stride sampling keeps the first, the last, and the shape.
const walletChartMaxDots = 240

// balanceChartInset keeps the first and last points off the SVG's edge:
// a point drawn exactly on it loses half its dot (and its scrub target) to
// the clip. The plot spans the width less this on each side.
const balanceChartInset = 4

// balanceChart is a fully-computed, template-ready SVG chart.
type balanceChart struct {
	Width     int
	Height    int
	Label     string // svg aria-label, set by the caller
	Points    string // polyline points for the balance line
	Dots      []balanceDot
	Ticks     []markethistory.AxisTick
	DateTicks []markethistory.DateTick
	From      string // oldest date label
	To        string // newest date label

	// Stacked charts (the Net worth history) draw filled bands under
	// the line and name them in a legend of their own.
	Stacked    bool
	Bands      []balanceBand
	Legend     []chartLegendItem
	LegendNote string
}

// balanceBand is one filled polygon of a stacked chart.
type balanceBand struct {
	Label   string
	Fill    string // colour
	Opacity string
	Points  string // polygon points
}

// chartLegendItem is one swatch and its name (Class picks the swatch
// colour in the stylesheet).
type chartLegendItem struct {
	Class string
	Label string
}

type balanceDot struct {
	X, Y    int
	Date    string // "2006-01-02", the tooltip's date line
	Balance string // esi.FormatISK
	Title   string // "<date time>: balance X ISK"

	// Stacked charts also break the day down (formatted ISK).
	ISK    string
	Assets string
}

// buildBalanceChart turns an ascending series into SVG
// geometry. ok=false under two points — one point is a fact,
// not a shape, and the UI says so instead of drawing one.
func buildBalanceChart(points []balancePoint) (balanceChart, bool) {
	if len(points) < 2 {
		return balanceChart{}, false
	}
	chart := balanceChart{Width: markethistory.ChartWidth, Height: markethistory.ChartHeight, Label: "Balance over time"}
	plotH := markethistory.ChartHeight - markethistory.ChartPadTop - markethistory.ChartPadBottom
	priceTop := markethistory.ChartPadTop
	priceBottom := priceTop + plotH

	minB, maxB := math.Inf(1), math.Inf(-1)
	for _, p := range points {
		if p.Balance < minB {
			minB = p.Balance
		}
		if p.Balance > maxB {
			maxB = p.Balance
		}
	}
	// Flat series draw on a padded scale so the line sits
	// mid-band instead of pegged to an edge. The epsilon is
	// relative: summing thousands of asset values can leave
	// microscopic floating-point differences between logically
	// identical totals, and stretching that epsilon across the
	// full chart height draws phantom peaks and valleys.
	if maxB <= minB || (maxB != 0 && (maxB-minB)/math.Abs(maxB) < 1e-9) {
		pad := minB * 0.05
		if pad <= 0 {
			pad = 1
		}
		maxB, minB = maxB+pad, minB-pad
	}
	tick := func(value float64, y int, class string) markethistory.AxisTick {
		return markethistory.AxisTick{
			Label:  markethistory.FormatCompactAxisNumber(value) + " ISK",
			Y:      y,
			LabelY: y + 3,
			Class:  class,
		}
	}
	chart.Ticks = []markethistory.AxisTick{
		tick(maxB, priceTop, ""),
		tick((minB+maxB)/2, priceTop+plotH/2, "tick-mid"),
		tick(minB, priceBottom, ""),
	}

	// Stride-sample the dots + polyline down to the cap.
	stride := 1
	if len(points) > walletChartMaxDots {
		stride = (len(points) + walletChartMaxDots - 1) / walletChartMaxDots
	}
	var kept []balancePoint
	for i, p := range points {
		if i%stride == 0 || i == len(points)-1 {
			kept = append(kept, p)
		}
	}

	x := func(i int) int {
		if len(kept) == 1 {
			return markethistory.ChartWidth / 2
		}
		return balanceChartInset + i*(markethistory.ChartWidth-1-2*balanceChartInset)/(len(kept)-1)
	}
	y := func(b float64) int {
		frac := (b - minB) / (maxB - minB)
		// Round, don't truncate: a flat series sits at frac=0.5,
		// and microscopic floating-point residue must not push
		// adjacent dots across a pixel boundary.
		return priceBottom - int(math.Round(frac*float64(plotH)))
	}
	var pts []string
	for i, p := range kept {
		px, py := x(i), y(p.Balance)
		chart.Dots = append(chart.Dots, balanceDot{
			X: px, Y: py,
			Date:    p.At.Format(markethistory.DateLayout),
			Balance: esi.FormatISK(p.Balance),
			Title: fmt.Sprintf("%s UTC: balance %s ISK",
				p.At.Format("2006-01-02 15:04"), esi.FormatISK(p.Balance)),
		})
		pts = append(pts, fmt.Sprintf("%d,%d", px, py))
	}
	if len(pts) > 1 {
		chart.Points = strings.Join(pts, " ")
	}
	chart.From = points[0].At.Format(markethistory.DateLayout)
	chart.To = points[len(points)-1].At.Format(markethistory.DateLayout)
	chart.DateTicks = []markethistory.DateTick{
		{Label: chart.From, X: x(0), Anchor: "start"},
	}
	if len(kept) > 2 {
		mid := len(kept) / 2
		chart.DateTicks = append(chart.DateTicks, markethistory.DateTick{
			Label: kept[mid].At.Format(markethistory.DateLayout), X: x(mid), Anchor: "middle", Class: "tick-mid",
		})
	}
	chart.DateTicks = append(chart.DateTicks, markethistory.DateTick{
		Label: chart.To, X: x(len(kept) - 1), Anchor: "end",
	})
	return chart, true
}

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
	Chart   *balanceChart
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

	var now *balancePoint
	if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: characterID, Kind: esi.SnapWallet,
	}); err == nil {
		var bal float64
		if json.Unmarshal([]byte(snap.Payload), &bal) == nil {
			now = &balancePoint{At: snap.FetchedAt, Balance: bal}
		}
	}

	settled := journalLoaded
	if !settled {
		if state, _, found := app.corpKindState(ctx, characterID, esi.SnapWalletJournal); found &&
			(state == fetchStateError || state == fetchStateRoleMissing) {
			settled = true
		}
	}

	points := buildBalanceSeries(journal, samples, now)
	g.Points = len(points)
	switch {
	case len(points) >= 2:
		if chart, ok := buildBalanceChart(points); ok {
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

// attachNetWorthHistory fills the home Net worth module's
// history from the user's sampler rows: a chart once two daily
// totals exist, the honest "building" state before that.
func (app *Application) attachNetWorthHistory(ctx context.Context, w *netWorthWidget, userID int64) {
	rows, err := app.queries.ListUserWalletHistory(ctx, userID)
	if err != nil {
		logging.Errorf("net worth history for user %d: %v", userID, err)
		return
	}
	days := netWorthBreakdownDays(rows)
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
