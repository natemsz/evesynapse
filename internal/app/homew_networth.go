package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetNetWorth, Title: "Net Worth", Category: wcatMoney, Feature: "networth", Href: "/wallet/",
		Description: "Wallets, assets and buy-order escrow across your characters, and how the total has moved.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "The total, its 7-day change and a 30-day line."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The total beside its chart."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "The total, how it splits, and its chart."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Wallets, assets and escrow over a chart with dates and values."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds the split by character or by location."},
		},
		Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders},
		Options: []widgetOption{
			optScope(),
			optChoice("range", "History", "30", "30:30 days", "90:90 days", "365:A year"),
			optChoice("split", "Split by", "chars", "chars:Character", "places:Location"),
		},
		Build: buildNetWorthWidget,
	})
}

// netWorthView is what the Net Worth forms draw: the account's
// figures, and the parts each form shows, built only for the forms
// this render draws.
type netWorthView struct {
	*netWorthWidget
	Lead     wLead
	Spark    *wChart  // small: the total over the last month
	Chart    *wChart  // wide, tall, medium, large: wallets and the rest, stacked, as live drew it
	Split    *wSegbar // tall: wallets, assets and escrow
	Tiles    []wTile  // medium, large
	Ranked   *wRanked // large: by character or by location
	RankedBy string   // the ranked list's heading
	Range    string   // the chart's span, "30 days"
	Note     string   // how fresh and how complete the estimate is
	Empty    wEmpty   // what the card says before any balance has synced
	Waiting  wEmpty   // what the chart's place says while history builds
}

// netWorthRanked is how many characters or places the large form
// ranks; its list shows the ones that fit.
const netWorthRanked = 8

func buildNetWorthWidget(ctx context.Context, app *Application, in *widgetInput) any {
	// The whole account's figures are shared with At a Glance; a card
	// scoped to some characters sums its own over them.
	nw := in.Shared.NetWorth(ctx)
	var ids []int64
	if !in.Scope.All() {
		for _, c := range in.Chars {
			ids = append(ids, c.CharacterID)
		}
		nw = app.buildNetWorth(ctx, in.UserID, in.Bundles, ids)
	}
	v := &netWorthView{netWorthWidget: nw}
	if !nw.Any {
		v.Empty = wEmpty{Text: "Balances appear here once your characters have synced."}
		return v
	}
	v.Note = "Estimate"
	if nw.AsOf != "" {
		v.Note += " as of " + nw.AsOf
	}
	v.Note += "."
	if nw.totalItems > 0 && nw.pricedItems < nw.totalItems {
		v.Note += fmt.Sprintf(" %d of %d items priced.", nw.pricedItems, nw.totalItems)
	}
	v.Waiting = wEmpty{Text: "Net worth history has started. A point is recorded each day."}
	v.Lead = wLead{Value: iskCompact(nw.totalNow), Unit: "ISK", Full: nw.Total + " ISK. " + v.Note, UpIsGood: true}
	if t := nw.TotalTrend; t != nil {
		v.Lead.Change, v.Lead.ChangeText, v.Lead.Up, v.Lead.Sub = t.Delta, t.Text, t.Up, "over "+t.Since
	}

	assets := nw.assetsNow - nw.escrowNow
	if in.Draws("tall") {
		var parts []wPart
		if nw.Wallet != "" {
			parts = append(parts, wPart{Name: "Wallets", Slot: 1, Value: nw.walletNow, Text: nw.Wallet, Full: nw.Wallet})
		}
		if nw.AssetsKnown {
			parts = append(parts, wPart{Name: "Assets", Slot: 2, Value: assets, Text: nw.Assets, Full: nw.Assets})
		}
		if nw.Escrow != "" {
			parts = append(parts, wPart{Name: "Order escrow", Slot: 3, Value: nw.escrowNow, Text: nw.Escrow, Full: nw.Escrow})
		}
		v.Split, _ = wSegments("How the total splits", parts)
	}
	if in.Draws("medium") || in.Draws("large") {
		if nw.Wallet != "" {
			v.Tiles = append(v.Tiles, wTile{Label: "Wallets", Value: iskCompact(nw.walletNow), Unit: "ISK", Full: nw.Wallet + " ISK"})
		}
		if nw.AssetsKnown {
			v.Tiles = append(v.Tiles, wTile{Label: "Assets", Value: iskCompact(assets), Unit: "ISK", Full: nw.Assets + " ISK"})
		}
		if nw.Escrow != "" {
			v.Tiles = append(v.Tiles, wTile{Label: "Order escrow", Value: iskCompact(nw.escrowNow), Unit: "ISK", Full: nw.Escrow + " ISK"})
		}
	}

	month := nw.days
	if len(month) > 30 {
		month = month[len(month)-30:]
	}
	if in.Draws("small") {
		days := make([]wDay, len(month))
		for i, d := range month {
			days[i] = wDay{At: d.At, Values: []float64{d.Total()}}
		}
		v.Spark, _ = wLineChart("Net worth over the last 30 days", "Total", 1, days)
	}
	if in.Draws("wide") || in.Draws("tall") || in.Draws("medium") || in.Draws("large") {
		span, hist := 30, month
		if n, err := strconv.Atoi(in.Opts.Get("range")); err == nil && n > 30 {
			span = n
			if more, err := app.netWorthDays(ctx, in.UserID, n, ids); err != nil {
				logging.Errorf("net worth history for user %d: %v", in.UserID, err)
			} else {
				hist = more
			}
		}
		v.Range = fmt.Sprintf("%d days", span)
		if span == 365 {
			v.Range = "a year"
		}
		if in.Draws("wide") || in.Draws("tall") || in.Draws("medium") || in.Draws("large") {
			days := make([]wDay, len(hist))
			for i, d := range hist {
				days[i] = wDay{At: d.At, Values: []float64{d.ISK, d.Assets}}
			}
			v.Chart, _ = wStackedChart("Net worth over "+v.Range+", wallets and the rest stacked", "Total",
				[]wLayer{{Name: "Wallets", Slot: 1}, {Name: "Assets and orders", Slot: 2}}, days)
		}
	}
	if in.Draws("large") {
		if in.Opts.Get("split") == "places" {
			// Always the linked characters, scoped or not: an unlinked
			// character's rows stay in the table but are not the
			// account's any more.
			linked := make([]int64, 0, len(in.Chars))
			for _, c := range in.Chars {
				linked = append(linked, c.CharacterID)
			}
			v.RankedBy, v.Ranked = "Top locations", app.netWorthByPlace(ctx, in.UserID, linked)
		} else {
			v.RankedBy, v.Ranked = "By character", netWorthByChar(nw.chars, viewerChars(in.Chars))
		}
	}
	return v
}

// netWorthByChar ranks the characters by their live net worth.
func netWorthByChar(chars []netWorthChar, viewer map[int64]bool) *wRanked {
	ranked := append([]netWorthChar(nil), chars...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Total > ranked[j].Total })
	items := make([]wRankItem, 0, len(ranked))
	for _, c := range ranked {
		items = append(items, wRankItem{Name: c.Name, Link: charLink(viewer, c.ID, c.Name),
			Value: c.Total, Text: iskCompact(c.Total), Full: esi.FormatISK(c.Total) + " ISK"})
	}
	r, _ := wRankedBars("Net worth by character", items, netWorthRanked)
	return r
}

// netWorthByPlace ranks the places holding the most asset value: one
// bounded read of the stored per-place values, highest first, over
// charIDs: the characters linked to the account, or some of them.
func (app *Application) netWorthByPlace(ctx context.Context, userID int64, charIDs []int64) *wRanked {
	if len(charIDs) == 0 {
		return nil
	}
	rows, err := app.queries.ListUserTopAssetLocations(ctx, db.ListUserTopAssetLocationsParams{UserID: userID, CharacterIds: charIDs})
	if err != nil {
		logging.Errorf("net worth by location for user %d: %v", userID, err)
		return nil
	}
	memo := make(map[int64]placeRef)
	items := make([]wRankItem, 0, len(rows))
	for _, row := range rows {
		title := app.assetLocationTitle(ctx, row.LocationID, row.LocationType, nil, func(typeID int64) string {
			return fmt.Sprintf("Type #%d", typeID)
		}, nil)
		items = append(items, wRankItem{Name: title, Link: placeLink(app.linkPlaceMemo(ctx, memo, row.LocationID, title)),
			Value: row.EstValue, Text: iskCompact(row.EstValue), Full: esi.FormatISK(row.EstValue) + " ISK"})
	}
	r, _ := wRankedBars("Asset value by location", items, netWorthRanked)
	return r
}
