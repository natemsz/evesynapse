package app

// The market change feed (schema 106): what moved enough to tell
// someone, detected from the stat pack in the transaction that
// completes each region's sweep (DetectMarketChanges). The Trends
// page lists a region's feed, the item page shows a type's recent
// changes, and the notification pass tells players about changes to
// what they watch (notifyMarketEvents). Every reader takes the stored
// rows as they are.

import (
	"context"
	"fmt"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
)

const (
	// marketChangeMinMove is the smallest daily typical-sell move, in
	// percent, the feed records; a volatile type needs three times
	// its own daily swing on top.
	marketChangeMinMove = 10.0
	// marketChangeKeepDays is how long feed rows are kept.
	marketChangeKeepDays = 30
)

// marketChangeRow is one feed row, display-ready.
type marketChangeRow struct {
	TypeID   int64
	Name     string
	Day      string
	Kind     string
	Label    string
	Detail   string
	Dir      string
	Detected string
}

// describeMarketChange words one change: its label, the before and
// after, and whether it reads as up or down.
func describeMarketChange(kind string, before, after, pct float64) (label, detail, dir string) {
	switch kind {
	case "price_up", "price_down":
		label = "Price jumped"
		dir = "up"
		if kind == "price_down" {
			label, dir = "Price dropped", "down"
		}
		detail = fmt.Sprintf("Typical sell %s to %s ISK (%+.1f%%)", esi.FormatISK(before), esi.FormatISK(after), pct)
	case "supply_down":
		label, dir = "Supply fell", "down"
		detail = fmt.Sprintf("Units on sale %s to %s (%+.0f%%)", esi.FormatInt(int64(before)), esi.FormatInt(int64(after)), pct)
	case "spread_wide":
		label, dir = "Spread widened", "up"
		detail = fmt.Sprintf("Gap between typical sell and buy %.1f%% to %.1f%%", before, after)
	default:
		label = kind
	}
	return label, detail, dir
}

// marketChangeKinds are the feed's filter choices.
var marketChangeKinds = []struct{ key, label string }{
	{"", "Everything"}, {"price_up", "Price jumps"}, {"price_down", "Price drops"},
	{"supply_down", "Supply falls"}, {"spread_wide", "Spreads widening"},
}

// marketChangesForRegion reads a region's feed over the last days.
func (app *Application) marketChangesForRegion(ctx context.Context, regionID int64, kind string, days int) []marketChangeRow {
	rows, err := app.queries.ListMarketChangesForRegion(ctx, db.ListMarketChangesForRegionParams{
		RegionID: regionID, Since: time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02"), Kind: kind,
	})
	if err != nil {
		logging.Errorf("market changes: region %d: %v", regionID, err)
		return nil
	}
	now := time.Now()
	out := make([]marketChangeRow, 0, len(rows))
	for _, r := range rows {
		label, detail, dir := describeMarketChange(r.Kind, r.Before, r.After, r.Pct)
		out = append(out, marketChangeRow{
			TypeID: r.TypeID, Name: app.typeNameOrID(ctx, r.TypeID), Day: r.Day, Kind: r.Kind,
			Label: label, Detail: detail, Dir: dir, Detected: ageText(now, r.DetectedAt),
		})
	}
	return out
}

// attachMarketChanges fills an item view's recent changes in its region.
func (app *Application) attachMarketChanges(ctx context.Context, item *marketItem) {
	if item == nil || item.Pack == nil {
		return
	}
	rows, err := app.queries.ListMarketChangesForType(ctx, db.ListMarketChangesForTypeParams{RegionID: item.RegionID, TypeID: item.TypeID})
	if err != nil {
		logging.Errorf("market changes: type %d: %v", item.TypeID, err)
		return
	}
	now := time.Now()
	for _, r := range rows {
		label, detail, dir := describeMarketChange(r.Kind, r.Before, r.After, r.Pct)
		item.Pack.Changes = append(item.Pack.Changes, marketChangeRow{
			Day: r.Day, Kind: r.Kind, Label: label, Detail: detail, Dir: dir, Detected: ageText(now, r.DetectedAt),
		})
	}
}

// watchFreshFor is how old a stat pack row may be and still speak
// for a watched item; past it the traded history speaks instead.
const watchFreshFor = 6 * time.Hour

// watchMove is a watched item's price and its 7- and 30-day moves:
// the stat pack's typical sell where a sweep covers the region, else
// the recorded traded averages. Watch rows and watch alerts both read
// it, so the number a player sees is the number that alerts them.
type watchMove struct {
	Current          float64
	Chg7, Chg30      float64
	Has7, Has30, Has bool
	FromSweep        bool
}

func (app *Application) watchMove(ctx context.Context, typeID, regionID int64) watchMove {
	var m watchMove
	if s, err := app.queries.GetMarketStatChange7(ctx, db.GetMarketStatChange7Params{RegionID: regionID, TypeID: typeID}); err == nil &&
		s.TypicalSell > 0 && s.SellChg7d.Valid && time.Since(s.UpdatedAt) < watchFreshFor {
		m = watchMove{Current: s.TypicalSell, Has: true, Chg7: s.SellChg7d.Float64, Has7: true, FromSweep: true}
		if s.SellChg30d.Valid {
			m.Chg30, m.Has30 = s.SellChg30d.Float64, true
		}
		return m
	}
	rows := app.recentHistoryRows(ctx, regionID, typeID, markethistory.ChartRows)
	if len(rows) > 0 {
		m.Current, m.Has = rows[len(rows)-1].Average, true
	}
	m.Chg7, m.Has7 = markethistory.ChangePct(rows, 7)
	m.Chg30, m.Has30 = markethistory.ChangePct(rows, 30)
	return m
}

// basketMoveAlertPct is the daily move of a player's own basket, in
// percent, that earns a notification.
const basketMoveAlertPct = 5.0

// notifyMarketEvents: today's change feed rows on the items a player
// watches (each in its watched region), and their own baskets moving
// sharply over a day on complete days. Each is an event, told once.
func (app *Application) notifyMarketEvents(ctx context.Context, c *notifyCollector, userID int64) {
	src := c.source(notifyMarket, userID)
	today := time.Now().UTC().Format("2006-01-02")
	changes, err := app.queries.ListMarketChangesForWatchlist(ctx, db.ListMarketChangesForWatchlistParams{UserID: userID, Since: today})
	if err != nil {
		logging.Errorf("notify: market changes for user %d: %v", userID, err)
	} else {
		for _, ch := range changes {
			label, detail, _ := describeMarketChange(ch.Kind, ch.Before, ch.After, ch.Pct)
			c.add(notifyEvent{
				Kind: notifyMarket, Source: src,
				Key: fmt.Sprintf("mchg|%d|%d|%s|%s", ch.TypeID, ch.RegionID, ch.Day, ch.Kind),
				Title: fmt.Sprintf("%s in %s: %s. %s", app.typeNameOrID(ctx, ch.TypeID), app.marketRegionLabel(ctx, ch.RegionID),
					label, detail),
				URL: fmt.Sprintf("/market/?type=%d&region=%d", ch.TypeID, ch.RegionID),
			})
		}
	}
	baskets, err := app.queries.ListMarketBasketsFor(ctx, userID)
	if err != nil {
		logging.Errorf("notify: baskets for user %d: %v", userID, err)
		return
	}
	for _, b := range baskets {
		if !b.UserID.Valid {
			continue // presets are everyone's; only a player's own baskets alert
		}
		values, err := app.queries.ListMarketBasketValues(ctx, db.ListMarketBasketValuesParams{BasketID: b.ID, Since: time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02")})
		if err != nil || len(values) == 0 || values[len(values)-1].Day != today {
			continue
		}
		chg := basketChange(values, 1)
		if chg.Text == "" {
			continue
		}
		last := values[len(values)-1]
		prev := 0.0
		for i := len(values) - 2; i >= 0; i-- {
			if values[i].PricedItems == values[i].ItemCount && values[i].ValueSell > 0 {
				prev = values[i].ValueSell
				break
			}
		}
		if prev <= 0 || absFloat((last.ValueSell-prev)/prev*100) < basketMoveAlertPct {
			continue
		}
		c.add(notifyEvent{
			Kind: notifyMarket, Source: src,
			Key:   fmt.Sprintf("basket|%d|%s|%s", b.ID, today, chg.Dir),
			Title: fmt.Sprintf("Your basket %s moved %s in a day, to %s ISK", b.Name, chg.Text, esi.FormatISK(last.ValueSell)),
			URL:   fmt.Sprintf("/market/baskets/%d/", b.ID),
		})
	}
}
