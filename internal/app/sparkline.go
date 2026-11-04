package app

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	"evesynapse/internal/app/module"
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

// sparkKey is one priced figure's history: a type in a region.
type sparkKey = module.SparkKey

// priceSparks reads the hover card for every figure a table lists
// from the stat pack's stored 30-day line (schema 121, written by
// the sweep): one bounded read per region, nothing derived here. A
// type traded everywhere at one price (PLEX) reads from its one
// market. Keys without a stored line are absent, so their figures
// render plain; lines exist for the hub regions the sweep stores.
func (app *Application) priceSparks(ctx context.Context, keys []sparkKey) map[sparkKey]template.HTML {
	byRegion := make(map[int64][]int64)
	seen := make(map[sparkKey]bool, len(keys))
	for _, k := range keys {
		if k.TypeID == 0 || k.RegionID == 0 {
			continue
		}
		read := sparkKey{RegionID: marketRegionForType(k.TypeID, k.RegionID), TypeID: k.TypeID}
		if seen[read] {
			continue
		}
		seen[read] = true
		byRegion[read.RegionID] = append(byRegion[read.RegionID], read.TypeID)
	}
	cards := make(map[sparkKey]template.HTML, len(seen))
	for regionID, typeIDs := range byRegion {
		rows, err := app.queries.ListMarketStatSparks(ctx, db.ListMarketStatSparksParams{RegionID: regionID, TypeIds: typeIDs})
		if err != nil {
			logging.Errorf("market: price sparks for region %d: %v", regionID, err)
			continue
		}
		regionName, _ := marketRegionName(regionID)
		for _, r := range rows {
			cards[sparkKey{RegionID: regionID, TypeID: r.TypeID}] = storedSparkCard(r.SellSpark30d, r.SellLow30d, r.SellHigh30d, r.SellMean30d, regionName)
		}
	}
	out := make(map[sparkKey]template.HTML, len(keys))
	for _, k := range keys {
		if card, ok := cards[sparkKey{RegionID: marketRegionForType(k.TypeID, k.RegionID), TypeID: k.TypeID}]; ok {
			out[k] = card
		}
	}
	return out
}

// storedSparkCard formats one price hover card from the stored
// line and figures: the region's typical sell over 30 days, its
// range and its average.
func storedSparkCard(points string, lo, hi, mean float64, regionName string) template.HTML {
	if points == "" {
		return ""
	}
	caption := "Typical sell, 30 days"
	if regionName != "" {
		caption = "Typical sell in " + regionName + ", 30 days"
	}
	return template.HTML(fmt.Sprintf(
		`<span class="spark-cap">%s</span><svg class="spark-svg" viewBox="0 0 100 28" preserveAspectRatio="none" aria-hidden="true"><polygon class="kpi-spark-area" points="%s 98.0,28.0 2.0,28.0"/><polyline class="kpi-spark-line" points="%s"/></svg><span class="spark-range">%s – %s</span><span class="spark-range">30-day average %s ISK</span>`,
		template.HTMLEscapeString(caption), template.HTMLEscapeString(points), template.HTMLEscapeString(points),
		template.HTMLEscapeString(esi.FormatISK(lo)), template.HTMLEscapeString(esi.FormatISK(hi)),
		template.HTMLEscapeString(esi.FormatISK(mean))))
}

// PriceSparks is priceSparks for the signed-in reader, gated on
// their hover-card switch (module.Deps).
func (app *Application) PriceSparks(ctx context.Context, keys []sparkKey) map[sparkKey]template.HTML {
	if !app.sparksOn(ctx, app.UserID(ctx)) || len(keys) == 0 {
		return nil
	}
	return app.priceSparks(ctx, keys)
}

// sparksOn reports whether the signed-in reader wants hover cards
// (the spark_hover viz switch, on by default); signed out, never.
func (app *Application) sparksOn(ctx context.Context, userID int64) bool {
	return userID != 0 && app.vizPrefsFor(ctx, userID).SparkHover
}

// sparkFig renders a listed figure with its price hover card: the
// .spark-hover wrapper app.js lifts the card from on hover or focus.
// fig is the figure as text (escaped) or as already-built HTML (an
// iskFig); an empty card leaves the figure bare.
func sparkFig(fig any, card template.HTML) template.HTML {
	return sparkWrap(fig, card, false)
}

// sparkFigFallback is sparkFig for a figure whose row also draws the
// line in a column of its own: the card is a fallback, live only once
// the table reads as cards and drops that column (data-spark-fallback).
func sparkFigFallback(fig any, card template.HTML) template.HTML {
	return sparkWrap(fig, card, true)
}

func sparkWrap(fig any, card template.HTML, fallback bool) template.HTML {
	var inner template.HTML
	switch f := fig.(type) {
	case template.HTML:
		inner = f
	case string:
		inner = template.HTML(template.HTMLEscapeString(f))
	default:
		inner = template.HTML(template.HTMLEscapeString(fmt.Sprint(f)))
	}
	if card == "" {
		return inner
	}
	attr := template.HTML("")
	if fallback {
		attr = " data-spark-fallback"
	}
	return `<span class="spark-hover"` + attr + ` tabindex="0">` + inner + `<span class="spark-pop">` + card + `</span></span>`
}
