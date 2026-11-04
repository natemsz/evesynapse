package app

import (
	"context"
	"fmt"
	"html/template"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Hover sparklines: a listed figure anywhere can carry a miniature
// chart of its own history, shown in a small card while the figure
// is hovered or focused. The server renders the figure wrapped in
// .spark-hover with a hidden .spark-pop child; app.js lifts a copy
// into one fixed layer on hover or focus, so a scrollable table can
// never clip the card and in-app navigation needs no re-wiring. The chart
// geometry and line styling are the KPI sparkline's (sparklineSVG
// in home_networth_chart.go, .kpi-spark classes), so a hover chart
// reads exactly like the sparklines already on the site.
// ---------------------------------------------------------------------------

// sparkPop builds one hover card's contents: a caption naming the
// series, the sparkline, and the series' low to high range. An
// empty result (under two points) means the figure renders plain.
func sparkPop(values []float64, caption, loText, hiText string) template.HTML {
	line, area, ok := sparklineSVG(values)
	if !ok {
		return ""
	}
	return template.HTML(fmt.Sprintf(
		`<span class="spark-cap">%s</span><svg class="spark-svg" viewBox="0 0 100 28" preserveAspectRatio="none" aria-hidden="true"><polygon class="kpi-spark-area" points="%s"/><polyline class="kpi-spark-line" points="%s"/></svg><span class="spark-range">%s – %s</span>`,
		template.HTMLEscapeString(caption), area, line,
		template.HTMLEscapeString(loText), template.HTMLEscapeString(hiText)))
}

// watchSpark builds the hover card for one watched type's Current
// figure: the stat pack's typical sell over the last 30 days, one
// bounded stored read. Empty when the pack holds under two priced
// days for the type in that region.
func (app *Application) watchSpark(ctx context.Context, typeID, regionID int64) template.HTML {
	rows, err := app.queries.ListMarketRegionStatsDailyRecent(ctx, db.ListMarketRegionStatsDailyRecentParams{
		RegionID: regionID, TypeID: typeID,
		Since:    time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02"),
		RowLimit: 31,
	})
	if err != nil {
		logging.Errorf("market: watch spark for type %d: %v", typeID, err)
		return ""
	}
	values := make([]float64, 0, len(rows))
	for _, r := range rows {
		if r.TypicalSell > 0 {
			values = append(values, r.TypicalSell)
		}
	}
	if len(values) < 2 {
		return ""
	}
	lo, hi := values[0], values[0]
	for _, v := range values {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return sparkPop(values, "Typical sell, 30 days", esi.FormatISK(lo), esi.FormatISK(hi))
}

// basketSpark builds the hover card for one basket's Value
// figure from the daily values the list page already read for
// its change figures: the value at typical sell over the last
// 30 days. Empty when under two priced days are stored.
func basketSpark(series []db.ListMarketBasketValuesRow) template.HTML {
	values := make([]float64, 0, len(series))
	for _, v := range series {
		if v.ValueSell > 0 {
			values = append(values, v.ValueSell)
		}
	}
	if len(values) < 2 {
		return ""
	}
	lo, hi := values[0], values[0]
	for _, v := range values {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return sparkPop(values, "Basket value at typical sell, 30 days", esi.FormatISK(lo), esi.FormatISK(hi))
}

// basketLine30 is a basket's 30-day worth as a small inline line
// for the list, coloured by its direction over the window; "" with
// fewer than two valued days.
func basketLine30(series []db.ListMarketBasketValuesRow) template.HTML {
	values := make([]float64, 0, len(series))
	for _, v := range series {
		if v.ValueSell > 0 {
			values = append(values, v.ValueSell)
		}
	}
	if len(values) < 2 {
		return ""
	}
	lo, hi := values[0], values[0]
	for _, v := range values {
		lo, hi = min(lo, v), max(hi, v)
	}
	var b strings.Builder
	for i, v := range values {
		x := float64(i) * 60 / float64(len(values)-1)
		y := 8.0
		if hi > lo {
			y = 14 - (v-lo)/(hi-lo)*12
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	dir := "flat"
	if last, first := values[len(values)-1], values[0]; last > first {
		dir = "up"
	} else if last < first {
		dir = "down"
	}
	return template.HTML(`<svg class="bk-line ` + dir + `" viewBox="0 0 60 16" width="60" height="16" aria-hidden="true"><polyline points="` + b.String() + `"/></svg>`)
}
