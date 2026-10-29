package app

// The market browser on /market/: a category tree (the root groups,
// with the path to the chosen group opened level by level) beside
// every item under the chosen group, its whole subtree included,
// with the region's stat pack figures. The tree lives in the page:
// opening a category fetches just its children from
// /market/browse-children and unfolds them in place, and picking a
// group swaps only the item list from /market/browse-fragment, so
// the tree never re-renders or jumps. With nothing picked yet the
// list stays empty until a category or a search is chosen. Filters
// (name, traded volume) and the sort run in SQL with a LIMIT, and
// the URL follows picks, so a view is shareable and Back works.
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
	Depth       int
	Expanded    bool // on the path to the chosen group
	Chosen      bool
	HasChildren bool
	Region      int64
	Children    []marketBrowseNode
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

// marketBrowseView is the browser panel.
type marketBrowseView struct {
	Region      int64
	RegionName  string
	Current     *marketBrowseGroup
	Breadcrumbs []marketBrowseCrumb
	Tree        []marketBrowseNode
	Types       []marketBrowseType
	Query       string
	MinVolume   int64
	Sort        string
	GroupID     int64
	Page        int
	TotalPages  int
	TotalTypes  int64
	PrevHref     string
	NextHref     string
	AwaitingPick bool // nothing chosen or searched yet: no item list
}

// marketBrowseCrumb is one breadcrumb step (an ancestor group).
type marketBrowseCrumb struct {
	ID   int64
	Name string
}

// marketBrowseSortKeys are the item list's sort keys, picked by
// clicking the table headers (see SortHref and its toggles).
var marketBrowseSortKeys = []string{
	"name", "price", "cheap", "buy", "buycheap", "volume", "rising", "falling", "volatile",
}

// handleMarketBrowseFragment answers the browser's item-list swaps:
// only the list column, never the tree around it.
func (app *Application) handleMarketBrowseFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	region := defaultMarketRegion
	if id, err := strconv.ParseInt(r.URL.Query().Get("region"), 10, 64); err == nil {
		if _, ok := marketRegionName(id); ok {
			region = id
		}
	}
	app.renderFragment(w, "market.html", "market-browse-items", app.buildMarketBrowse(ctx, region, r.URL.Query()))
}

// marketBrowseChildren carries one level of the tree for the
// children endpoint.
type marketBrowseChildren struct {
	Region int64
	Nodes  []marketBrowseNode
}

var marketBrowseChildrenCache = newViewCache[[]marketBrowseNode](5*time.Minute, 512)

// handleMarketBrowseChildren answers one tree level: the children
// of one group, for market.js to unfold in place.
func (app *Application) handleMarketBrowseChildren(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	region := defaultMarketRegion
	if id, err := strconv.ParseInt(r.URL.Query().Get("region"), 10, 64); err == nil {
		if _, ok := marketRegionName(id); ok {
			region = id
		}
	}
	groupID, err := strconv.ParseInt(r.URL.Query().Get("group"), 10, 64)
	if err != nil || groupID <= 0 {
		http.Error(w, "unknown group", http.StatusBadRequest)
		return
	}
	key := strconv.FormatInt(groupID, 10)
	var nodes []marketBrowseNode
	if cached, ok := marketBrowseChildrenCache.get(key); ok {
		nodes = *cached
	} else {
		parents, err := app.marketGroupParents(ctx)
		if err != nil {
			logging.Errorf("market: browse children parents: %v", err)
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		nodes = app.buildMarketBrowseLevel(ctx, groupID, 1, region, map[int64]bool{}, 0, parents)
		marketBrowseChildrenCache.put(key, &nodes)
	}
	app.renderFragment(w, "market.html", "market-browse-children", marketBrowseChildren{Region: region, Nodes: nodes})
}

// marketGroupParents is the set of group IDs that have child groups.
func (app *Application) marketGroupParents(ctx context.Context) (map[int64]bool, error) {
	rows, err := app.queries.ListMarketGroupParentIDs(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[int64]bool, len(rows))
	for _, id := range rows {
		set[id] = true
	}
	return set, nil
}

// buildMarketBrowseLevel builds one tree level; nodes on the path
// to the chosen group recurse into their children, expanded.
func (app *Application) buildMarketBrowseLevel(ctx context.Context, parent int64, depth int, region int64, onPath map[int64]bool, chosen int64, parents map[int64]bool) []marketBrowseNode {
	rows, err := app.queries.ListSDEMarketGroupsByParent(ctx, parent)
	if err != nil {
		logging.Errorf("market: list market groups under %d: %v", parent, err)
		return nil
	}
	nodes := make([]marketBrowseNode, 0, len(rows))
	for _, row := range rows {
		node := marketBrowseNode{
			marketBrowseGroup: marketBrowseGroupRow(row),
			Depth:             depth,
			Expanded:          onPath[row.MarketGroupID],
			Chosen:            row.MarketGroupID == chosen,
			HasChildren:       parents[row.MarketGroupID],
			Region:            region,
		}
		if node.Expanded && depth < 16 {
			node.Children = app.buildMarketBrowseLevel(ctx, row.MarketGroupID, depth+1, region, onPath, chosen, parents)
		}
		nodes = append(nodes, node)
	}
	return nodes
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

// SortHref is the list URL under another sort, back on page one.
func (v *marketBrowseView) SortHref(key string) string {
	return v.href(func(q url.Values) {
		q.Set("sort", key)
		q.Del("page")
	})
}

// SellSortHref toggles the sell column: priciest first, then
// cheapest first.
func (v *marketBrowseView) SellSortHref() string {
	if v.Sort == "price" {
		return v.SortHref("cheap")
	}
	return v.SortHref("price")
}

// BuySortHref toggles the buy column the same way.
func (v *marketBrowseView) BuySortHref() string {
	if v.Sort == "buy" {
		return v.SortHref("buycheap")
	}
	return v.SortHref("buy")
}

// ChgSortHref toggles the 7-day column: biggest risers first, then
// biggest fallers.
func (v *marketBrowseView) ChgSortHref() string {
	if v.Sort == "rising" {
		return v.SortHref("falling")
	}
	return v.SortHref("rising")
}

// ClearQueryHref is the list URL with the name filter dropped.
func (v *marketBrowseView) ClearQueryHref() string {
	return v.href(func(q url.Values) {
		q.Del("bq")
		q.Del("page")
	})
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
	for _, key := range marketBrowseSortKeys {
		if q.Get("sort") == key {
			view.Sort = key
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
	parents, err := app.marketGroupParents(ctx)
	if err != nil {
		logging.Errorf("market: browse tree parents: %v", err)
		return view
	}
	view.Tree = app.buildMarketBrowseLevel(ctx, 0, 0, region, onPath, view.GroupID, parents)

	if len(view.Tree) == 0 {
		return view // no static data yet
	}

	if view.GroupID == 0 && view.Query == "" && view.MinVolume == 0 {
		// Open on the tree alone: listing every item in the game
		// under no category at all is noise, not a starting point.
		view.AwaitingPick = true
		view.TotalPages = 1
		return view
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
