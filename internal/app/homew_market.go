package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
)

// Market's show option: how a scope of several characters reads.
const (
	mergeBoth         = "both"          // totals and a row per character
	mergeCombined     = "combined"      // totals only
	mergePerCharacter = "per-character" // a row per character in place of the totals
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetMarket, Title: "Market", Category: wcatMarket, Feature: "market",
		Requires:    "market",
		Href:        "/orders/",
		Description: "Open orders across characters: counts, sell/buy value, orders expiring soonest.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "Open orders, how many are undercut, and sell against buy."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The figures and the two orders that need you first."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "The figures and the orders that need you: undercut or expiring soon."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The figures and a row for each character."},
			{Name: "large", Sizes: "3x2 3x3", Says: "The figures, the orders that need you and a row for each character."},
		},
		Needs: []string{esi.SnapOrders},
		Options: []widgetOption{
			optScope(),
			withHint(optChoice("show", "Show", mergeBoth,
				mergeBoth+":Totals and each character", mergeCombined+":Totals only", mergePerCharacter+":Each character only"),
				"How orders read when more than one character is shown."),
		},
		Build: buildMarketWidget,
	})
}

// marketOrdersView is what the Market forms draw: the orders summary and
// the parts each form shows.
type marketOrdersView struct {
	*marketWidget
	Lead      wLead
	Undercut  int      // sell orders beaten at their station or in their region
	Split     *wSegbar // sell value against buy escrow
	Tiles     []wTile  // medium and large, unless the show option asks for rows only
	Attention []marketAttention
	Rows      []marketCharRow // medium and large, unless the show option asks for totals only
	Empty     wEmpty
}

// marketAttention is one order that needs its owner: undercut first,
// then the soonest to expire.
type marketAttention struct {
	Char   string
	CharID int64
	Item   string
	TypeID int64
	Why    string // "Undercut in the station", "Expires in 2d 3h"
	Bad    bool   // undercut: the order no longer sells first
}

// marketAttentionMax bounds the attention list; the card shows the
// rows that fit.
const marketAttentionMax = 8

// marketExpiringSoon is how close an expiry must be to need you.
const marketExpiringSoon = 3 * 24 * time.Hour

func buildMarketWidget(ctx context.Context, app *Application, in *widgetInput) any {
	show := in.Opts.Get("show")
	base := app.buildMarket(ctx, in.Bundles, show, in.Scope, in.Shared.market)
	v := &marketOrdersView{marketWidget: base}
	if !base.AnyData {
		v.Empty = wEmpty{Text: "Order data appears here once your characters have synced."}
		return v
	}
	if base.Open == 0 {
		v.Empty = wEmpty{Text: base.EmptyOrders + " Find your next trade in", Step: "Trade Ideas.", Href: "/market/ideas/"}
		return v
	}
	var sell, buy float64
	type expiring struct {
		row marketAttention
		at  time.Time
	}
	var soon []expiring
	for _, b := range in.Bundles {
		for _, o := range b.orders {
			if o.IsBuyOrder {
				buy += o.Escrow
			} else {
				sell += o.Price * float64(o.VolumeRemain)
			}
			if at, ok := orderExpiry(o); ok && at.After(in.Now) && at.Sub(in.Now) <= marketExpiringSoon {
				soon = append(soon, expiring{row: marketAttention{Char: b.ch.Name, CharID: b.ch.CharacterID, TypeID: o.TypeID,
					Why: "Expires in " + humanDuration(at.Sub(in.Now))}, at: at})
			}
		}
	}
	sort.SliceStable(soon, func(i, j int) bool { return soon[i].at.Before(soon[j].at) })

	// Undercut orders from the stored order health, for the
	// characters this card shows.
	list := in.Draws("wide") || in.Draws("tall") || in.Draws("medium") || in.Draws("large")
	mine := viewerChars(in.Chars)
	names := make(map[int64]string, len(in.Chars))
	for _, c := range in.Chars {
		names[c.CharacterID] = c.Name
	}
	for _, h := range in.Shared.market.orderHealth(ctx) {
		if !mine[h.CharacterID] {
			continue
		}
		where := ""
		switch h.Status {
		case "undercut_station":
			where = "in the station"
		case "undercut_region":
			where = "in the region"
		default:
			continue
		}
		v.Undercut++
		if list && len(v.Attention) < marketAttentionMax {
			v.Attention = append(v.Attention, marketAttention{Char: names[h.CharacterID], CharID: h.CharacterID,
				Item: app.typeNameOrID(ctx, h.TypeID), TypeID: h.TypeID, Why: "Undercut " + where, Bad: true})
		}
	}
	for _, e := range soon {
		if !list || len(v.Attention) >= marketAttentionMax {
			break
		}
		e.row.Item = app.typeNameOrID(ctx, e.row.TypeID)
		v.Attention = append(v.Attention, e.row)
	}

	v.Lead = wLead{Label: "Open orders", Value: strconv.Itoa(base.Open), Full: fmt.Sprintf("%d open orders", base.Open),
		Sub: fmt.Sprintf("%d sell, %d buy", base.SellCount, base.BuyCount)}
	v.Split, _ = wSegments("Sell value against buy escrow", []wPart{
		{Name: "Sell orders", Slot: 1, Value: sell, Text: base.SellValue, Full: base.SellValue + " ISK", Short: iskCompact(sell)},
		{Name: "Buy escrow", Slot: 2, Value: buy, Text: base.BuyValue, Full: base.BuyValue + " ISK", Short: iskCompact(buy)},
	})
	if (in.Draws("medium") || in.Draws("large")) && base.Merge != mergePerCharacter {
		v.Tiles = []wTile{
			{Label: "Sell orders", Value: iskCompact(sell), Unit: "ISK", Full: fmt.Sprintf("%d sell (%s ISK)", base.SellCount, base.SellValue)},
			{Label: "Buy escrow", Value: iskCompact(buy), Unit: "ISK", Full: fmt.Sprintf("%d buy (%s ISK in escrow)", base.BuyCount, base.BuyValue)},
			{Label: "Undercut", Value: strconv.Itoa(v.Undercut), Full: fmt.Sprintf("%d sell orders undercut", v.Undercut)},
		}
	}
	if base.Merge != mergeCombined {
		v.Rows = base.PerChar
	}
	return v
}
