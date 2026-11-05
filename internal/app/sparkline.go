package app

import (
	"context"
	"fmt"
	"html/template"

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

// basketSparkCard formats a basket's stored 30-day worth line
// (schema 123) as its hover card.
func basketSparkCard(points string, lo, hi, mean float64) template.HTML {
	if points == "" {
		return ""
	}
	return template.HTML(fmt.Sprintf(
		`<span class="spark-cap">Basket value at typical sell, 30 days</span><svg class="spark-svg" viewBox="0 0 100 28" preserveAspectRatio="none" aria-hidden="true"><polygon class="kpi-spark-area" points="%s 98.0,28.0 2.0,28.0"/><polyline class="kpi-spark-line" points="%s"/></svg><span class="spark-range">%s – %s</span><span class="spark-range">30-day average %s ISK</span>`,
		template.HTMLEscapeString(points), template.HTMLEscapeString(points),
		template.HTMLEscapeString(esi.FormatISK(lo)), template.HTMLEscapeString(esi.FormatISK(hi)),
		template.HTMLEscapeString(esi.FormatISK(mean))))
}

// basketLineSVG draws a basket's stored 30-day worth line (schema
// 123) for the list's trend column, coloured by its direction.
func basketLineSVG(points, dir string) template.HTML {
	if points == "" {
		return ""
	}
	return template.HTML(`<svg class="bk-line ` + template.HTMLEscapeString(dir) + `" viewBox="0 0 60 16" width="60" height="16" aria-hidden="true"><polyline points="` + template.HTMLEscapeString(points) + `"/></svg>`)
}

// sparkKey is one priced figure's history: a type in a region.
type sparkKey = module.SparkKey

// priceSparks is the hover card for every figure a table lists (see
// priceSparkPairs).
func (app *Application) priceSparks(ctx context.Context, keys []sparkKey) map[sparkKey]template.HTML {
	pairs := app.priceSparkPairs(ctx, keys)
	out := make(map[sparkKey]template.HTML, len(pairs))
	for k, p := range pairs {
		out[k] = p.Card
	}
	return out
}

// priceSparkPairs reads the stat pack's stored 30-day line for
// every figure a table lists (schema 121, written by the sweep): one
// bounded read per region, nothing derived here. Each key gets its
// hover card and the same line drawn small for a column of its own.
// A type traded everywhere at one price (PLEX) reads from its one
// market. Keys without a stored line are absent, so their figures
// render plain; lines exist for the hub regions the sweep stores.
func (app *Application) priceSparkPairs(ctx context.Context, keys []sparkKey) map[sparkKey]module.PriceSpark {
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
	pairs := make(map[sparkKey]module.PriceSpark, len(seen))
	for regionID, typeIDs := range byRegion {
		rows, err := app.queries.ListMarketStatSparks(ctx, db.ListMarketStatSparksParams{RegionID: regionID, TypeIds: typeIDs})
		if err != nil {
			logging.Errorf("market: price sparks for region %d: %v", regionID, err)
			continue
		}
		regionName, _ := marketRegionName(regionID)
		for _, r := range rows {
			if r.SellSpark30d == "" {
				continue
			}
			pairs[sparkKey{RegionID: regionID, TypeID: r.TypeID}] = module.PriceSpark{
				Card: storedSparkCard(r.SellSpark30d, r.SellLow30d, r.SellHigh30d, r.SellMean30d, regionName),
				Line: priceLineSVG(r.SellSpark30d),
			}
		}
	}
	out := make(map[sparkKey]module.PriceSpark, len(keys))
	for _, k := range keys {
		if p, ok := pairs[sparkKey{RegionID: marketRegionForType(k.TypeID, k.RegionID), TypeID: k.TypeID}]; ok {
			out[k] = p
		}
	}
	return out
}

// priceLineSVG draws a stored 30-day line (the hover card's own
// 100 by 28 geometry) small, for a table's trend column.
func priceLineSVG(points string) template.HTML {
	return template.HTML(`<svg class="px-line" viewBox="0 0 100 28" preserveAspectRatio="none" aria-hidden="true"><polyline points="` + template.HTMLEscapeString(points) + `"/></svg>`)
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

// PriceSparkLines is the trend column's line for each listed figure,
// with the hover card for when the table reads as cards and drops the
// column. Signed out, none; the card follows the reader's hover-card
// switch, the line does not.
func (app *Application) PriceSparkLines(ctx context.Context, keys []sparkKey) map[sparkKey]module.PriceSpark {
	if app.UserID(ctx) == 0 || len(keys) == 0 {
		return nil
	}
	pairs := app.priceSparkPairs(ctx, keys)
	if !app.sparksOn(ctx, app.UserID(ctx)) {
		for k, p := range pairs {
			p.Card = ""
			pairs[k] = p
		}
	}
	return pairs
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
