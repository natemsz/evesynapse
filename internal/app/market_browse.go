package app

// The market browser on /market/: a category tree (the root groups,
// with the path to the chosen group opened level by level) beside
// every item under the chosen group, its whole subtree included,
// with the region's stat pack figures. Filters (name, traded volume)
// and the sort run in SQL with a LIMIT; navigating and filtering
// swap in /market/browse-fragment (market.js) without reloading the
// page, and the URL follows, so a view is shareable and Back works.
// Local reads only: SDE tables and the stat pack.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const marketBrowseTypesPerPage = 100

// marketBrowseNode is one row of the category tree.
type marketBrowseNode struct {
	marketBrowseGroup
	Depth  int
	Open   bool // on the path to the chosen group
	Chosen bool
}

// marketBrowseType is one item row with its stat pack figures.
type marketBrowseType struct {
	ID          int64
	Name        string
	TypicalSell string
	TypicalBuy  string
	Chg7        trendFigure
	TradedDay   string
	Swing       string
}

// marketBrowseSort is one sort choice.
type marketBrowseSort struct {
	Key, Label string
	Active     bool
}

// marketBrowseView is the browser panel.
type marketBrowseView struct {
	Region      int64
	RegionName  string
	Current     *marketBrowseGroup
	Breadcrumbs []marketBrowseCrumb
	Tree        []marketBrowseNode
	Types       []marketBrowseType
	Sorts       []marketBrowseSort
	Query       string
	MinVolume   int64
	Sort        string
	GroupID     int64
	Page        int
	TotalPages  int
	TotalTypes  int64
	PrevHref    string
	NextHref    string
}

// marketBrowseCrumb is one breadcrumb step (an ancestor group).
type marketBrowseCrumb struct {
	ID   int64
	Name string
}

var marketBrowseSorts = []struct{ key, label string }{
	{"name", "Name"}, {"price", "Priciest"}, {"cheap", "Cheapest"}, {"volume", "Most traded"},
	{"rising", "Rising"}, {"falling", "Falling"}, {"volatile", "Swings most"},
}

// handleMarketBrowseFragment answers the browser's in-page swaps.
func (app *Application) handleMarketBrowseFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	region := defaultMarketRegion
	if id, err := strconv.ParseInt(r.URL.Query().Get("region"), 10, 64); err == nil {
		if _, ok := marketRegionName(id); ok {
			region = id
		}
	}
	app.renderFragment(w, "market.html", "market-browse", app.buildMarketBrowse(ctx, region, r.URL.Query()))
}

// href is the browser's URL for this view with one change.
func (v *marketBrowseView) href(change func(url.Values)) string {
	q := url.Values{}
	if v.GroupID > 0 {
		q.Set("group", strconv.FormatInt(v.GroupID, 10))
	}
	q.Set("region", strconv.FormatInt(v.Region, 10))
	if v.Query != "" {
		q.Set("bq", v.Query)
	}
	if v.MinVolume > 0 {
		q.Set("minvol", strconv.FormatInt(v.MinVolume, 10))
	}
	if v.Sort != "name" {
		q.Set("sort", v.Sort)
	}
	if v.Page > 1 {
		q.Set("page", strconv.Itoa(v.Page))
	}
	change(q)
	return "/market/?" + q.Encode()
}

var marketBrowseCache = newViewCache[marketBrowseView](30*time.Second, 256)

// buildMarketBrowse assembles the browser for one region from the
// query: group, bq (name filter), minvol, sort, page. The view is a
// pure function of the query over stored rows, so a built view is
// reused for a few seconds: the subtree count and ranking under it
// are the market's most expensive anonymous reads.
func (app *Application) buildMarketBrowse(ctx context.Context, region int64, q url.Values) *marketBrowseView {
	key := strconv.FormatInt(region, 10) + "?" + q.Encode()
	if view, ok := marketBrowseCache.get(key); ok {
		return view
	}
	view := app.buildMarketBrowseFresh(ctx, region, q)
	if len(view.Tree) > 0 {
		marketBrowseCache.put(key, view)
	}
	return view
}

func (app *Application) buildMarketBrowseFresh(ctx context.Context, region int64, q url.Values) *marketBrowseView {
	view := &marketBrowseView{Region: region, Sort: "name", Page: 1}
	view.RegionName, _ = marketRegionName(region)
	view.GroupID, _ = strconv.ParseInt(q.Get("group"), 10, 64)
	view.Query = strings.TrimSpace(q.Get("bq"))
	if len([]rune(view.Query)) > 60 {
		view.Query = string([]rune(view.Query)[:60])
	}
	if v, err := strconv.ParseInt(q.Get("minvol"), 10, 64); err == nil && v > 0 {
		view.MinVolume = v
	}
	for _, s := range marketBrowseSorts {
		if q.Get("sort") == s.key {
			view.Sort = s.key
		}
	}
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		view.Page = p
	}

	// The path from the root to the chosen group (cycle-guarded;
	// real trees are a handful of levels deep).
	var path []db.SdeMarketGroup
	if view.GroupID > 0 {
		if g, err := app.queries.GetSDEMarketGroup(ctx, view.GroupID); err == nil {
			path = append(path, g)
			seen := map[int64]bool{g.MarketGroupID: true}
			for p := g.ParentGroupID; p > 0 && !seen[p] && len(path) < 16; {
				seen[p] = true
				parent, err := app.queries.GetSDEMarketGroup(ctx, p)
				if err != nil {
					break
				}
				path = append([]db.SdeMarketGroup{parent}, path...)
				p = parent.ParentGroupID
			}
		} else {
			view.GroupID = 0
		}
	}
	if n := len(path); n > 0 {
		cur := marketBrowseGroupRow(path[n-1])
		view.Current = &cur
		for _, g := range path[:n-1] {
			view.Breadcrumbs = append(view.Breadcrumbs, marketBrowseCrumb{ID: g.MarketGroupID, Name: g.Name})
		}
	}
	onPath := map[int64]bool{}
	for _, g := range path {
		onPath[g.MarketGroupID] = true
	}
	var walk func(parent int64, depth int)
	walk = func(parent int64, depth int) {
		rows, err := app.queries.ListSDEMarketGroupsByParent(ctx, parent)
		if err != nil {
			logging.Errorf("market: list market groups under %d: %v", parent, err)
			return
		}
		for _, row := range rows {
			node := marketBrowseNode{marketBrowseGroup: marketBrowseGroupRow(row), Depth: depth,
				Open: onPath[row.MarketGroupID], Chosen: row.MarketGroupID == view.GroupID}
			view.Tree = append(view.Tree, node)
			if node.Open && depth < 16 {
				walk(row.MarketGroupID, depth+1)
			}
		}
	}
	walk(0, 0)

	for _, s := range marketBrowseSorts {
		view.Sorts = append(view.Sorts, marketBrowseSort{Key: s.key, Label: s.label, Active: s.key == view.Sort})
	}
	if len(view.Tree) == 0 {
		return view // no static data yet
	}

	like := ""
	if view.Query != "" {
		like = "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(view.Query) + "%"
	}
	total, err := app.queries.CountMarketBrowseTypes(ctx, db.CountMarketBrowseTypesParams{
		GroupID: view.GroupID, RegionID: region, NameLike: like, MinVolume: float64(view.MinVolume),
	})
	if err != nil {
		logging.Errorf("market: count browse types under %d: %v", view.GroupID, err)
		return view
	}
	view.TotalTypes = total
	view.TotalPages = max(int((total+marketBrowseTypesPerPage-1)/marketBrowseTypesPerPage), 1)
	view.Page = min(view.Page, view.TotalPages)
	if view.Page > 1 {
		view.PrevHref = view.href(func(q url.Values) { q.Set("page", strconv.Itoa(view.Page-1)) })
	}
	if view.Page < view.TotalPages {
		view.NextHref = view.href(func(q url.Values) { q.Set("page", strconv.Itoa(view.Page+1)) })
	}
	rows, err := app.queries.ListMarketBrowseTypes(ctx, db.ListMarketBrowseTypesParams{
		GroupID: view.GroupID, RegionID: region, NameLike: like, MinVolume: float64(view.MinVolume),
		Sort: view.Sort, RowLimit: marketBrowseTypesPerPage, RowOffset: int64((view.Page - 1) * marketBrowseTypesPerPage),
	})
	if err != nil {
		logging.Errorf("market: browse types under %d: %v", view.GroupID, err)
		return view
	}
	for _, r := range rows {
		row := marketBrowseType{ID: r.TypeID, Name: r.Name, Chg7: newTrendFigure(r.SellChg7d)}
		if r.TypicalSell > 0 {
			row.TypicalSell = esi.FormatISK(r.TypicalSell)
		}
		if r.TypicalBuy > 0 {
			row.TypicalBuy = esi.FormatISK(r.TypicalBuy)
		}
		if r.TradedPerDay > 0 {
			row.TradedDay = esi.FormatInt(int64(r.TradedPerDay + 0.5))
		}
		if r.Volatility30d.Valid {
			row.Swing = strconv.FormatFloat(r.Volatility30d.Float64, 'f', 1, 64) + "%"
		}
		view.Types = append(view.Types, row)
	}
	return view
}
