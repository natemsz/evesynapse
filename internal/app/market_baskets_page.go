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
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
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
	Chg30      trendFigure   // the change across the line's window: the trend column's sort key
	Spark      template.HTML // hover card for the Value figure, when the reader's hover sparklines are on; it stands in for the line where the line's column is dropped
	Line       template.HTML // the 30-day value line, in its own column
	ValueShort string
}

type marketBasketItemRow struct {
	TypeID                             int64
	Name                               string
	Quantity                           string
	Unit                               string
	Value                              string
	Share                              string // of the basket's value
	SharePct                           string // the same, 0 to 100, for its meter
	UnitShort, ValueShort, ImpactShort string
	Chg                                trendFigure
	Impact                             string // ISK the item moved the basket over the period
	ImpactUp                           bool
	impact                             float64
	value                              float64
	priced                             bool
}

type marketBasketMover struct {
	TypeID int64
	Name   string
	Unit   string
	Chg    trendFigure
	Impact string // inside the basket: ISK effect; outside: ""
	Qty    string // inside the basket: the quantity held
}

type marketBasketView struct {
	// list
	List     []marketBasketListRow
	SignedIn bool
	AtLimit  bool
	Regions  []marketRegionChoice

	// detail
	Basket        *marketBasketListRow
	Own           bool
	RegionID      int64
	Period        int
	Periods       []marketTrendChoice
	Value         string
	ValueBuy      string
	Chg1          trendFigure
	Chg7          trendFigure
	Chg30         trendFigure
	Delta1        string // the ISK behind each change, signed and compact
	Delta7        string
	Delta30       string
	ValueShort    string
	ValueBuyShort string
	Chart         *bookTrendChart
	Items         []marketBasketItemRow
	YourOrders    []yourOrderRow
	Unpriced      int
	MoversUp      []marketBasketMover
	MoversDown    []marketBasketMover
	Outside       []marketBasketMover
	EditText      string
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
			row.ValueShort = formatISKShort(b.ValueSell)
			row.Partial = b.PricedItems < b.ItemCount
		}
		if b.UserID.Valid {
			own++
		}
		view.List = append(view.List, row)
	}
	// Each basket's stored figures (schema 123): its changes, the
	// trend column's line and the worth's hover card.
	var valued []int64
	for _, row := range view.List {
		if row.Value != "" {
			valued = append(valued, row.ID)
		}
	}
	if len(valued) > 0 {
		sums, err := app.queries.ListMarketBasketSummaries(ctx, valued)
		if err != nil {
			logging.Errorf("baskets: list figures: %v", err)
		}
		byBasket := make(map[int64]db.ListMarketBasketSummariesRow, len(sums))
		for _, s := range sums {
			byBasket[s.BasketID] = s
		}
		sparkOn := app.sparksOn(ctx, userID)
		for i := range view.List {
			s, ok := byBasket[view.List[i].ID]
			if !ok {
				continue
			}
			view.List[i].Chg1, view.List[i].Chg7, view.List[i].Chg30 = newTrendFigure(s.Chg1d), newTrendFigure(s.Chg7d), newTrendFigure(s.Chg30d)
			view.List[i].Line = basketLineSVG(s.Line30d, s.LineDir)
			if sparkOn {
				view.List[i].Spark = basketSparkCard(s.Spark30d, s.Low30d, s.High30d, s.Mean30d)
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
		if !errors.Is(err, pgx.ErrNoRows) {
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
	if userID > 0 && len(view.Items) > 0 {
		types := make(map[int64]bool, len(view.Items))
		for _, it := range view.Items {
			types[it.TypeID] = true
		}
		view.YourOrders = app.buildYourOrders(ctx, userID, types)
	}
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

	// The stored figures (schema 123): worth, changes and deltas.
	if sums, err := app.queries.ListMarketBasketSummaries(ctx, []int64{b.ID}); err != nil {
		logging.Errorf("baskets: figures of %d: %v", b.ID, err)
	} else if len(sums) == 1 && sums[0].ItemCount > 0 {
		s := sums[0]
		view.Value, view.ValueBuy = esi.FormatISK(s.ValueSell), esi.FormatISK(s.ValueBuy)
		view.ValueShort, view.ValueBuyShort = formatISKShort(s.ValueSell), formatISKShort(s.ValueBuy)
		row.Partial = s.PricedItems < s.ItemCount
		view.Chg1, view.Chg7, view.Chg30 = newTrendFigure(s.Chg1d), newTrendFigure(s.Chg7d), newTrendFigure(s.Chg30d)
		view.Delta1, view.Delta7, view.Delta30 = signedISKShort(s.Delta1d), signedISKShort(s.Delta7d), signedISKShort(s.Delta30d)
	}
	// The value chart plots the stored daily values.
	values, err := app.queries.ListMarketBasketValues(ctx, db.ListMarketBasketValuesParams{
		BasketID: b.ID, Since: basketDay(time.Now().AddDate(0, 0, -bookTrendDays)),
	})
	if err != nil {
		logging.Errorf("baskets: values of %d: %v", b.ID, err)
	}
	if len(values) > 0 {
		daily := make([]db.ListMarketRegionStatsDailyRecentRow, 0, len(values))
		for _, v := range values {
			daily = append(daily, db.ListMarketRegionStatsDailyRecentRow{Day: v.Day, TypicalSell: v.ValueSell, TypicalBuy: v.ValueBuy})
		}
		view.Chart = buildBookTrendChart(daily)
	}

	// Items, worth first, and the movers in: their stored figures.
	qty := make(map[int64]int64, len(items))
	var editLines []string
	for _, it := range items {
		qty[it.TypeID] = it.Quantity
		editLines = append(editLines, fmt.Sprintf("%s %d", app.typeNameOrID(ctx, it.TypeID), it.Quantity))
	}
	view.EditText = strings.Join(editLines, "\n")
	stats, err := app.queries.ListMarketBasketItemStats(ctx, b.ID)
	if err != nil {
		logging.Errorf("baskets: item figures of %d: %v", b.ID, err)
	}
	pick := func(c1, c7, c30 sql.NullFloat64) sql.NullFloat64 {
		switch view.Period {
		case 1:
			return c1
		case 30:
			return c30
		}
		return c7
	}
	for _, s := range stats {
		line := marketBasketItemRow{TypeID: s.TypeID, Name: app.typeNameOrID(ctx, s.TypeID), Quantity: esi.FormatInt(qty[s.TypeID])}
		if s.Priced {
			line.priced, line.value = true, s.Value
			line.Unit, line.Value = esi.FormatISK(s.Unit), esi.FormatISK(s.Value)
			line.UnitShort, line.ValueShort = formatISKShort(s.Unit), formatISKShort(s.Value)
			line.Share = fmt.Sprintf("%.1f%%", s.SharePct)
			line.SharePct = strconv.FormatFloat(math.Round(s.SharePct), 'f', 0, 64)
			line.Chg = newTrendFigure(pick(s.Chg1d, s.Chg7d, s.Chg30d))
			if imp := pick(s.Impact1d, s.Impact7d, s.Impact30d); imp.Valid {
				line.impact = imp.Float64
				line.Impact = esi.FormatISK(math.Abs(imp.Float64))
				line.ImpactUp = imp.Float64 >= 0
				line.ImpactShort = formatISKShort(math.Abs(imp.Float64))
			}
		} else {
			view.Unpriced++
		}
		view.Items = append(view.Items, line)
	}
	for _, sign := range []int32{1, -1} {
		movers, err := app.queries.ListMarketBasketMovers(ctx, db.ListMarketBasketMoversParams{BasketID: b.ID, Period: int32(view.Period), Sign: sign})
		if err != nil {
			logging.Errorf("baskets: movers of %d: %v", b.ID, err)
			continue
		}
		for _, m := range movers {
			mv := marketBasketMover{TypeID: m.TypeID, Name: app.typeNameOrID(ctx, m.TypeID), Unit: esi.FormatISK(m.Unit), Chg: newTrendFigure(sql.NullFloat64{Float64: m.Chg, Valid: !math.IsNaN(m.Chg)}),
				Impact: formatISKShort(math.Abs(m.Impact)), Qty: esi.FormatInt(qty[m.TypeID])}
			if sign > 0 {
				view.MoversUp = append(view.MoversUp, mv)
			} else {
				view.MoversDown = append(view.MoversDown, mv)
			}
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
	last, then, ok := basketChangeBase(values, days)
	if !ok {
		return trendFigure{}
	}
	return newTrendFigure(sql.NullFloat64{Float64: (last - then) / then * 100, Valid: true})
}

// signedISKShort is a stored ISK change, signed and compact, "" where
// the change has no base.
func signedISKShort(d sql.NullFloat64) string {
	if !d.Valid {
		return ""
	}
	if d.Float64 < 0 {
		return "−" + formatISKShort(-d.Float64)
	}
	return "+" + formatISKShort(d.Float64)
}

// basketChangeBase finds the two worths a change compares: the
// latest, and a fully priced day of the same basket within three
// days before the period's start.
func basketChangeBase(values []db.ListMarketBasketValuesRow, days int) (last, then float64, ok bool) {
	if len(values) == 0 {
		return 0, 0, false
	}
	end := values[len(values)-1]
	if end.PricedItems < end.ItemCount || end.ValueSell <= 0 {
		return 0, 0, false
	}
	endDay, err := time.Parse("2006-01-02", end.Day)
	if err != nil {
		return 0, 0, false
	}
	from, to := basketDay(endDay.AddDate(0, 0, -days-3)), basketDay(endDay.AddDate(0, 0, -days))
	for i := len(values) - 2; i >= 0; i-- {
		v := values[i]
		if v.Day > to {
			continue
		}
		if v.Day < from {
			break
		}
		if v.PricedItems == v.ItemCount && v.ItemCount == end.ItemCount && v.ValueSell > 0 {
			return end.ValueSell, v.ValueSell, true
		}
	}
	return 0, 0, false
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
	// The basket page's name-and-region form sends no items field;
	// only a form carrying one replaces the basket's contents.
	_, hasItems := r.PostForm["items"]
	var lines []basketLine
	if hasItems {
		lines = parseBasketLines(r.Form.Get("items"))
		if len(lines) > basketMaxItems {
			back(fmt.Sprintf("A basket holds at most %d items.", basketMaxItems))
			return
		}
	}
	if err := app.queries.UpdateMarketBasket(ctx, db.UpdateMarketBasketParams{
		ID: b.ID, UserID: b.UserID, Name: name, RegionID: basketRegion(r.Form.Get("region")),
	}); err != nil {
		logging.Errorf("baskets: update %d: %v", b.ID, err)
		back("Couldn't save the basket right now.")
		return
	}
	if !hasItems {
		// A new region reprices the basket: its values and figures
		// are rebuilt from that region's stored rows.
		if region := basketRegion(r.Form.Get("region")); region != b.RegionID {
			if err := app.rebuildBasketValues(ctx, b.ID); err != nil {
				logging.Errorf("baskets: reprice %d: %v", b.ID, err)
			}
		}
		back("Saved.")
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

// handleMarketBasketAddItem serves POST /market/baskets/{basketID}/add:
// the basket page's Add button on a similar item, appending that
// type at quantity 1 through the same replace-and-rebuild path
// every other basket save uses.
func (app *Application) handleMarketBasketAddItem(w http.ResponseWriter, r *http.Request) {
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
	typeID, err := strconv.ParseInt(r.Form.Get("type"), 10, 64)
	if err != nil || typeID <= 0 {
		back("That item didn't come through. Try again.")
		return
	}
	items, err := app.queries.ListMarketBasketItems(ctx, b.ID)
	if err != nil {
		logging.Errorf("baskets: items of %d: %v", b.ID, err)
		back("Couldn't read that basket right now.")
		return
	}
	next := make([]basketItemQty, 0, len(items)+1)
	for _, it := range items {
		if it.TypeID == typeID {
			back("Already in the basket.")
			return
		}
		next = append(next, basketItemQty{TypeID: it.TypeID, Quantity: it.Quantity})
	}
	if len(next) >= basketMaxItems {
		back(fmt.Sprintf("A basket holds at most %d items.", basketMaxItems))
		return
	}
	next = append(next, basketItemQty{TypeID: typeID, Quantity: 1})
	if err := app.saveBasketItemsByID(ctx, b.ID, next); err != nil {
		logging.Errorf("baskets: add item for %d: %v", b.ID, err)
		back("Couldn't add that item right now.")
		return
	}
	back("Added " + app.typeNameOrID(ctx, typeID) + ".")
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
