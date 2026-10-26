package balancechart

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/markethistory"
)

// BalancePoint is one (time, balance) observation. The chart
// series is a run of these, ascending.
type BalancePoint struct {
	At      time.Time
	Balance float64
}

// BuildBalanceSeries merges the three stored sources into one
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
func BuildBalanceSeries(journal esi.WalletJournal, samples []db.WalletHistory, now *BalancePoint) []BalancePoint {
	var points []BalancePoint

	journalPts := make([]BalancePoint, 0, len(journal))
	for _, e := range journal {
		t, err := time.Parse(time.RFC3339, e.Date)
		if err != nil {
			continue
		}
		journalPts = append(journalPts, BalancePoint{At: t, Balance: e.Balance})
	}
	sort.SliceStable(journalPts, func(i, j int) bool { return journalPts[i].At.Before(journalPts[j].At) })

	if len(journalPts) > 0 {
		windowStart := journalPts[0].At
		for _, s := range samples {
			p := SamplePoint(s)
			if !p.At.Before(windowStart) {
				continue
			}
			points = append(points, p)
		}
	} else {
		for _, s := range samples {
			points = append(points, SamplePoint(s))
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

// SamplePoint turns one daily history row into a series point,
// dated when the value was last written that day.
func SamplePoint(s db.WalletHistory) BalancePoint {
	return BalancePoint{At: s.SampledAt, Balance: s.Balance}
}

// NetWorthHistoryPoints sums the sampler's net-worth column
// across one user's characters, one point per sampled day. A
// day contributes the characters that have a computed net worth
// that day; days with none stay off the chart rather than
// reading as a crash to zero.
func NetWorthHistoryPoints(rows []db.WalletHistory) []BalancePoint {
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
	points := make([]BalancePoint, 0, len(days))
	for _, d := range days {
		t, err := time.Parse(markethistory.DateLayout, d)
		if err != nil {
			continue
		}
		points = append(points, BalancePoint{At: t, Balance: byDay[d]})
	}
	return points
}

// WalletChartMaxDots caps the rendered points: a busy journal
// window holds thousands of entries, and past a few hundred
// dots the polyline reads the same while the page bloats.
// Stride sampling keeps the first, the last, and the shape.
const WalletChartMaxDots = 240

// BalanceChartInset keeps the first and last points off the SVG's edge:
// a point drawn exactly on it loses half its dot (and its scrub target) to
// the clip. The plot spans the width less this on each side.
const BalanceChartInset = 4

// BalanceChart is a fully-computed, template-ready SVG chart.
type BalanceChart struct {
	Width     int
	Height    int
	Label     string // svg aria-label, set by the caller
	Points    string // polyline points for the balance line
	Dots      []BalanceDot
	Ticks     []markethistory.AxisTick
	DateTicks []markethistory.DateTick
	From      string // oldest date label
	To        string // newest date label

	// Stacked charts (the Net worth history) draw filled bands under
	// the line and name them in a legend of their own.
	Stacked    bool
	Bands      []BalanceBand
	Legend     []ChartLegendItem
	LegendNote string
}

// BalanceBand is one filled polygon of a stacked chart.
type BalanceBand struct {
	Label   string
	Class   string // stylesheet hook, so a colour scheme can repaint the band
	Fill    string // colour
	Opacity string
	Points  string // polygon points
}

// ChartLegendItem is one swatch and its name (Class picks the swatch
// colour in the stylesheet).
type ChartLegendItem struct {
	Class string
	Label string
}

type BalanceDot struct {
	X, Y    int
	Date    string // "2006-01-02", the tooltip's date line
	Balance string // esi.FormatISK
	Title   string // "<date time>: balance X ISK"

	// Stacked charts also break the day down (formatted ISK).
	ISK    string
	Assets string
}

// BuildBalanceChart turns an ascending series into SVG
// geometry. ok=false under two points — one point is a fact,
// not a shape, and the UI says so instead of drawing one.
func BuildBalanceChart(points []BalancePoint) (BalanceChart, bool) {
	if len(points) < 2 {
		return BalanceChart{}, false
	}
	chart := BalanceChart{Width: markethistory.ChartWidth, Height: markethistory.ChartHeight, Label: "Balance over time"}
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
	if len(points) > WalletChartMaxDots {
		stride = (len(points) + WalletChartMaxDots - 1) / WalletChartMaxDots
	}
	var kept []BalancePoint
	for i, p := range points {
		if i%stride == 0 || i == len(points)-1 {
			kept = append(kept, p)
		}
	}

	x := func(i int) int {
		if len(kept) == 1 {
			return markethistory.ChartWidth / 2
		}
		return BalanceChartInset + i*(markethistory.ChartWidth-1-2*BalanceChartInset)/(len(kept)-1)
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
		chart.Dots = append(chart.Dots, BalanceDot{
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
