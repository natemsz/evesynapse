package app

// The basket pages (/market/baskets/): the list of presets and the
// reader's own baskets, and one basket's value over time, its items,
// and its top movers in and out. Every figure is read from stored
// rows: the basket's stored daily values, the stat pack for its items
// (loadStatPacks), and one bounded query for movers outside it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

type marketBasketListRow struct {
	ID         int64
	Name       string
	RegionName string
	Preset     bool
	Items      int64
	Value      string // latest stored value at typical sell, "" before the first
	Partial    bool   // the latest value misses some items' prices
	Chg1       trendFigure
	Chg7       trendFigure
	Spark      template.HTML // hover card for the Value figure, when the reader's hover sparklines are on
}

type marketBasketItemRow struct {
	TypeID   int64
	Name     string
	Quantity string
	Unit     string
	Value    string
	Share    string // of the basket's value
	Chg      trendFigure
	Impact   string // ISK the item moved the basket over the period
	ImpactUp bool
	impact   float64
	value    float64
	priced   bool
}

type marketBasketMover struct {
	TypeID int64
	Name   string
	Unit   string
	Chg    trendFigure
	Impact string // inside the basket: ISK effect; outside: ""
}

type marketBasketView struct {
	// list
	List     []marketBasketListRow
	SignedIn bool
	AtLimit  bool
	Regions  []marketRegionChoice

	// detail
	Basket     *marketBasketListRow
	Own        bool
	RegionID   int64
	Period     int
	Periods    []marketTrendChoice
	Value      string
	ValueBuy   string
	Chg1       trendFigure
	Chg7       trendFigure
	Chg30      trendFigure
	Chart      *bookTrendChart
	Items      []marketBasketItemRow
	Unpriced   int
	MoversUp   []marketBasketMover
	MoversDown []marketBasketMover
	Outside    []marketBasketMover
	EditText   string
}

func (app *Application) handleMarketBaskets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	userID := app.userID(ctx)
	view := &marketBasketView{SignedIn: userID != 0}
	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegionChoice{ID: region.ID, Name: region.Name, Active: region.ID == defaultMarketRegion})
	}
	rows, err := app.queries.ListMarketBasketsFor(ctx, userID)
	if err != nil {
		logging.Errorf("baskets: list: %v", err)
	}
	own := 0
	for _, b := range rows {
		row := basketListRow(b.ID, b.Name, b.RegionID, !b.UserID.Valid, b.ItemCount)
		if b.ValueDay != "" {
			row.Value = esi.FormatISK(b.ValueSell)
			row.Partial = b.PricedItems < b.ItemCount
		}
		if b.UserID.Valid {
			own++
		}
		view.List = append(view.List, row)
	}
	// The basic stats: each basket's 1-day and 7-day change, from
	// the same stored daily values and the same change rule the
	// basket page's figures use.
	var valued []int64
	for _, row := range view.List {
		if row.Value != "" {
			valued = append(valued, row.ID)
		}
	}
	if len(valued) > 0 {
		// The hover sparkline reads the same stored series over
		// its 30-day window; with the switch off the read stays
		// at the change figures' 14 days.
		sparkOn := userID != 0 && app.vizPrefsFor(ctx, userID).SparkHover
		window := -14
		if sparkOn {
			window = -31
		}
		series, err := app.queries.ListMarketBasketValuesForBaskets(ctx, db.ListMarketBasketValuesForBasketsParams{
			BasketIds: valued, Since: basketDay(time.Now().AddDate(0, 0, window)),
		})
		if err != nil {
			logging.Errorf("baskets: list values: %v", err)
		} else {
			byBasket := make(map[int64][]db.ListMarketBasketValuesRow, len(valued))
			for _, v := range series {
				byBasket[v.BasketID] = append(byBasket[v.BasketID], db.ListMarketBasketValuesRow{
					Day: v.Day, ValueSell: v.ValueSell, ValueBuy: v.ValueBuy,
					PricedItems: v.PricedItems, ItemCount: v.ItemCount,
				})
			}
			for i := range view.List {
				if s := byBasket[view.List[i].ID]; len(s) > 0 {
					view.List[i].Chg1 = basketChange(s, 1)
					view.List[i].Chg7 = basketChange(s, 7)
					if sparkOn {
						view.List[i].Spark = basketSpark(s)
					}
				}
			}
		}
	}
	view.AtLimit = own >= basketMaxPerUser
	data.MarketBaskets = view
	app.render(ctx, w, http.StatusOK, "market_baskets.html", data)
}

func basketListRow(id int64, name string, regionID int64, preset bool, items int64) marketBasketListRow {
	regionName, _ := marketRegionName(regionID)
	return marketBasketListRow{ID: id, Name: name, RegionName: regionName, Preset: preset, Items: items}
}

// loadBasketFor reads a basket the reader may see: a preset, or
// their own. Anything else reads as missing.
func (app *Application) loadBasketFor(ctx context.Context, idRaw string, userID int64) (db.MarketBasket, bool) {
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil || id <= 0 {
		return db.MarketBasket{}, false
	}
	b, err := app.queries.GetMarketBasket(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("baskets: read %d: %v", id, err)
		}
		return db.MarketBasket{}, false
	}
	if b.UserID.Valid && b.UserID.Int64 != userID {
		return db.MarketBasket{}, false
	}
	return b, true
}

func (app *Application) handleMarketBasket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	userID := app.userID(ctx)
	b, ok := app.loadBasketFor(ctx, chi.URLParam(r, "basketID"), userID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	view := app.buildBasketView(ctx, b, userID, module.QueryValue(r.URL.Query(), "period"))
	data.MarketBaskets = view
	app.render(ctx, w, http.StatusOK, "market_basket.html", data)
}

func (app *Application) buildBasketView(ctx context.Context, b db.MarketBasket, userID int64, periodRaw string) *marketBasketView {
	view := &marketBasketView{SignedIn: userID != 0, Own: b.UserID.Valid && b.UserID.Int64 == userID, RegionID: b.RegionID, Period: 7}
	switch periodRaw {
	case "1":
		view.Period = 1
	case "30":
		view.Period = 30
	}
	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegionChoice{ID: region.ID, Name: region.Name, Active: region.ID == b.RegionID})
	}
	for _, p := range []struct {
		days  int
		label string
	}{{1, "1 day"}, {7, "7 days"}, {30, "30 days"}} {
		view.Periods = append(view.Periods, marketTrendChoice{
			Label: p.label, Href: fmt.Sprintf("/market/baskets/%d/?period=%d", b.ID, p.days), Active: p.days == view.Period,
		})
	}
	items, err := app.queries.ListMarketBasketItems(ctx, b.ID)
	if err != nil {
		logging.Errorf("baskets: items of %d: %v", b.ID, err)
	}
	row := basketListRow(b.ID, b.Name, b.RegionID, !b.UserID.Valid, int64(len(items)))
	view.Basket = &row

	// Value over time: the stored daily values.
	values, err := app.queries.ListMarketBasketValues(ctx, db.ListMarketBasketValuesParams{
		BasketID: b.ID, Since: basketDay(time.Now().AddDate(0, 0, -bookTrendDays)),
	})
	if err != nil {
		logging.Errorf("baskets: values of %d: %v", b.ID, err)
	}
	if n := len(values); n > 0 {
		last := values[n-1]
		view.Value = esi.FormatISK(last.ValueSell)
		view.ValueBuy = esi.FormatISK(last.ValueBuy)
		row.Partial = last.PricedItems < last.ItemCount
		view.Chg1 = basketChange(values, 1)
		view.Chg7 = basketChange(values, 7)
		view.Chg30 = basketChange(values, 30)
		daily := make([]db.ListMarketRegionStatsDailyRecentRow, 0, n)
		for _, v := range values {
			daily = append(daily, db.ListMarketRegionStatsDailyRecentRow{Day: v.Day, TypicalSell: v.ValueSell, TypicalBuy: v.ValueBuy})
		}
		view.Chart = buildBookTrendChart(daily)
	}

	// Items and movers in: the stat pack for the basket's items.
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.TypeID)
	}
	packs, err := app.loadStatPacks(ctx, b.RegionID, ids)
	if err != nil {
		logging.Errorf("baskets: stat pack for %d: %v", b.ID, err)
		packs = statPackSet{}
	}
	var total float64
	var lines []marketBasketItemRow
	var editLines []string
	for _, it := range items {
		name := app.typeNameOrID(ctx, it.TypeID)
		editLines = append(editLines, fmt.Sprintf("%s %d", name, it.Quantity))
		line := marketBasketItemRow{TypeID: it.TypeID, Name: name, Quantity: esi.FormatInt(it.Quantity)}
		if p := packs[it.TypeID]; p != nil && p.Own != nil && p.Own.TypicalSell > 0 {
			unit := p.Own.TypicalSell
			line.priced = true
			line.value = unit * float64(it.Quantity)
			line.Unit = esi.FormatISK(unit)
			line.Value = esi.FormatISK(line.value)
			chg := periodChange(p.Own, view.Period)
			line.Chg = newTrendFigure(chg)
			if chg.Valid && chg.Float64 > -100 {
				// ISK the item moved the basket: now less then.
				then := line.value / (1 + chg.Float64/100)
				line.impact = line.value - then
				line.Impact = esi.FormatISK(math.Abs(line.impact))
				line.ImpactUp = line.impact >= 0
			}
			total += line.value
		} else {
			view.Unpriced++
		}
		lines = append(lines, line)
	}
	for i := range lines {
		if lines[i].priced && total > 0 {
			lines[i].Share = fmt.Sprintf("%.1f%%", lines[i].value/total*100)
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].value > lines[j].value })
	view.Items = lines
	view.EditText = strings.Join(editLines, "\n")

	movers := make([]marketBasketItemRow, 0, len(lines))
	for _, l := range lines {
		if l.Impact != "" {
			movers = append(movers, l)
		}
	}
	sort.Slice(movers, func(i, j int) bool { return movers[i].impact > movers[j].impact })
	for _, m := range movers {
		if m.impact > 0 && len(view.MoversUp) < 5 {
			view.MoversUp = append(view.MoversUp, marketBasketMover{TypeID: m.TypeID, Name: m.Name, Unit: m.Unit, Chg: m.Chg, Impact: m.Impact})
		}
	}
	for i := len(movers) - 1; i >= 0; i-- {
		if m := movers[i]; m.impact < 0 && len(view.MoversDown) < 5 {
			view.MoversDown = append(view.MoversDown, marketBasketMover{TypeID: m.TypeID, Name: m.Name, Unit: m.Unit, Chg: m.Chg, Impact: m.Impact})
		}
	}

	// Movers out: the same market groups, outside the basket.
	if len(items) > 0 {
		out, err := app.queries.ListMarketBasketOutsideMovers(ctx, db.ListMarketBasketOutsideMoversParams{
			RegionID: b.RegionID, BasketID: b.ID, Period: int32(view.Period),
		})
		if err != nil {
			logging.Errorf("baskets: outside movers for %d: %v", b.ID, err)
		}
		for _, o := range out {
			chg := o.SellChg7d
			switch view.Period {
			case 1:
				chg = o.SellChg1d
			case 30:
				chg = o.SellChg30d
			}
			view.Outside = append(view.Outside, marketBasketMover{
				TypeID: o.TypeID, Name: app.typeNameOrID(ctx, o.TypeID), Unit: esi.FormatISK(o.TypicalSell), Chg: newTrendFigure(chg),
			})
		}
	}
	return view
}

// periodChange picks a stat pack row's typical-sell change for 1, 7
// or 30 days.
func periodChange(s *db.MarketRegionStat, days int) sql.NullFloat64 {
	switch days {
	case 1:
		return s.SellChg1d
	case 30:
		return s.SellChg30d
	}
	return s.SellChg7d
}

// basketChange compares the newest stored value with the newest one at
// least days older (within three extra days), over complete days only.
func basketChange(values []db.ListMarketBasketValuesRow, days int) trendFigure {
	last := values[len(values)-1]
	if last.PricedItems < last.ItemCount || last.ValueSell <= 0 {
		return trendFigure{}
	}
	end, err := time.Parse("2006-01-02", last.Day)
	if err != nil {
		return trendFigure{}
	}
	from, to := basketDay(end.AddDate(0, 0, -days-3)), basketDay(end.AddDate(0, 0, -days))
	for i := len(values) - 2; i >= 0; i-- {
		v := values[i]
		if v.Day > to {
			continue
		}
		if v.Day < from {
			break
		}
		if v.PricedItems == v.ItemCount && v.ItemCount == last.ItemCount && v.ValueSell > 0 {
			return newTrendFigure(sql.NullFloat64{Float64: (last.ValueSell - v.ValueSell) / v.ValueSell * 100, Valid: true})
		}
	}
	return trendFigure{}
}

// handleMarketBasketCreate makes a basket from the form, or copies
// a preset (or the reader's own basket) when from is set.
func (app *Application) handleMarketBasketCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/market/baskets/")
	if err := r.ParseForm(); err != nil {
		back("That form didn't come through. Try again.")
		return
	}
	if n, err := app.queries.CountMarketBasketsForUser(ctx, sql.NullInt64{Int64: userID, Valid: true}); err == nil && n >= basketMaxPerUser {
		back(fmt.Sprintf("You can keep up to %d baskets. Delete one to make another.", basketMaxPerUser))
		return
	}
	name := basketName(r.Form.Get("name"))
	regionID := basketRegion(r.Form.Get("region"))
	lines := parseBasketLines(r.Form.Get("items"))
	if from := r.Form.Get("from"); from != "" {
		src, ok := app.loadBasketFor(ctx, from, userID)
		if !ok {
			back("That basket isn't available.")
			return
		}
		name, regionID = basketName(src.Name+" (copy)"), src.RegionID
		items, err := app.queries.ListMarketBasketItems(ctx, src.ID)
		if err != nil {
			back("Couldn't copy that basket right now.")
			return
		}
		lines = lines[:0]
		for _, it := range items {
			lines = append(lines, basketLine{Name: app.typeNameOrID(ctx, it.TypeID), Quantity: it.Quantity})
		}
	}
	if name == "" {
		back("Give the basket a name.")
		return
	}
	if len(lines) > basketMaxItems {
		back(fmt.Sprintf("A basket holds at most %d items.", basketMaxItems))
		return
	}
	id, err := app.queries.CreateMarketBasket(ctx, db.CreateMarketBasketParams{
		UserID: sql.NullInt64{Int64: userID, Valid: true}, Name: name, RegionID: regionID,
	})
	if err != nil {
		logging.Errorf("baskets: create for user %d: %v", userID, err)
		back("Couldn't save the basket right now.")
		return
	}
	unknown, err := app.saveBasketItems(ctx, id, lines)
	path := fmt.Sprintf("/market/baskets/%d/", id)
	switch {
	case err != nil:
		logging.Errorf("baskets: items for %d: %v", id, err)
		app.flashBack(w, r, path)("Saved the basket, but not its items. Try editing it.")
	case len(unknown) > 0:
		app.flashBack(w, r, path)("Left out items with no exact match: " + strings.Join(unknown, ", ") + ".")
	default:
		http.Redirect(w, r, path, http.StatusSeeOther)
	}
}

func (app *Application) handleMarketBasketEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	b, ok := app.loadBasketFor(ctx, chi.URLParam(r, "basketID"), userID)
	if !ok || !b.UserID.Valid {
		http.NotFound(w, r)
		return
	}
	path := fmt.Sprintf("/market/baskets/%d/", b.ID)
	back := app.flashBack(w, r, path)
	if err := r.ParseForm(); err != nil {
		back("That form didn't come through. Try again.")
		return
	}
	name := basketName(r.Form.Get("name"))
	if name == "" {
		back("Give the basket a name.")
		return
	}
	lines := parseBasketLines(r.Form.Get("items"))
	if len(lines) > basketMaxItems {
		back(fmt.Sprintf("A basket holds at most %d items.", basketMaxItems))
		return
	}
	if err := app.queries.UpdateMarketBasket(ctx, db.UpdateMarketBasketParams{
		ID: b.ID, UserID: b.UserID, Name: name, RegionID: basketRegion(r.Form.Get("region")),
	}); err != nil {
		logging.Errorf("baskets: update %d: %v", b.ID, err)
		back("Couldn't save the basket right now.")
		return
	}
	unknown, err := app.saveBasketItems(ctx, b.ID, lines)
	switch {
	case err != nil:
		logging.Errorf("baskets: items for %d: %v", b.ID, err)
		back("Couldn't save the items right now.")
	case len(unknown) > 0:
		back("Saved. Left out items with no exact match: " + strings.Join(unknown, ", ") + ".")
	default:
		back("Saved.")
	}
}

func (app *Application) handleMarketBasketDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	b, ok := app.loadBasketFor(ctx, chi.URLParam(r, "basketID"), userID)
	if !ok || !b.UserID.Valid {
		http.NotFound(w, r)
		return
	}
	if err := app.queries.DeleteMarketBasket(ctx, db.DeleteMarketBasketParams{ID: b.ID, UserID: b.UserID}); err != nil {
		logging.Errorf("baskets: delete %d: %v", b.ID, err)
	}
	app.flashBack(w, r, "/market/baskets/")("Deleted " + b.Name + ".")
}

func basketName(raw string) string {
	name := strings.TrimSpace(raw)
	if r := []rune(name); len(r) > basketNameMax {
		name = string(r[:basketNameMax])
	}
	return name
}

func basketRegion(raw string) int64 {
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if _, ok := marketRegionName(id); ok {
			return id
		}
	}
	return defaultMarketRegion
}
