package app

import (
	"context"
	"evesynapse/internal/htmlsanitize"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/contracts"
	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Item database explorer: browse the local SDE category → group →
// type tree. Every read is a local SDE query — no ESI, no token.
// Types link into the Market item view.
// ---------------------------------------------------------------------------

// itemsTypesPerPage paginates a group's type list.
const itemsTypesPerPage = 100

// itemCategoryRow is one category with its total type count
// and its published type count: the browse tree renders both,
// so the published-only switch can prune the listing in place.
type itemCategoryRow struct {
	ID    int64
	Name  string
	Types string // formatted count

	Published      int64  // published types under this category
	PublishedTypes string // formatted published count
}

// itemGroupRow is one group with its type count.
type itemGroupRow struct {
	ID    int64
	Name  string
	Types string // formatted count

	Published      int64  // published types in this group
	PublishedTypes string // formatted published count
}

// itemTypeRow is one type in a group listing.
type itemTypeRow struct {
	ID        int64
	Name      string
	OnMarket  bool
	Published bool
}

// itemSearchRow is one global-search hit: the type plus where
// it sits in the browse tree.
type itemSearchRow struct {
	ID           int64
	Name         string
	GroupID      int64
	GroupName    string
	CategoryID   int64
	CategoryName string
	OnMarket     bool
	Published    bool
}

// itemSearchSection is one Category › Group heading with the matching
// items under it, so search and filter results keep the browse tree's
// organisation instead of reading as one flat list.
type itemSearchSection struct {
	CategoryID   int64
	CategoryName string
	GroupID      int64
	GroupName    string
	Rows         []itemSearchRow
}

// groupSearchRows folds rows that arrive ordered by category and group
// (SearchSDETypesFiltered) into sections: a new section starts whenever
// the group changes. Rows with no group share one section.
func groupSearchRows(rows []itemSearchRow) []itemSearchSection {
	var sections []itemSearchSection
	for _, row := range rows {
		if n := len(sections); n == 0 || sections[n-1].GroupID != row.GroupID || sections[n-1].CategoryID != row.CategoryID {
			sections = append(sections, itemSearchSection{
				CategoryID: row.CategoryID, CategoryName: row.CategoryName,
				GroupID: row.GroupID, GroupName: row.GroupName,
			})
		}
		n := len(sections) - 1
		sections[n].Rows = append(sections[n].Rows, row)
	}
	return sections
}

// itemsView is the Item Database page body: exactly one of the
// levels (or the global search) is populated per render.
type itemsView struct {
	Mode string // "categories" | "category" | "group" | "type" | "search"

	Categories []itemCategoryRow

	Category *itemCategoryRow
	Groups   []itemGroupRow

	Group      *itemGroupRow
	Types      []itemTypeRow
	Query      string // within-group name filter
	TotalTypes int64
	Page       int
	TotalPages int
	HasPrev    bool
	HasNext    bool
	PrevPage   int
	NextPage   int

	// Global search state (Mode "search", also the browse
	// tree's published-only toggle): shareable through GET
	// parameters, exactly like the planner's plan URLs.
	SearchQuery    string
	PublishedOnly  bool
	CategoryID     int64
	GroupID        int64
	SearchResults  []itemSearchRow
	SearchSections []itemSearchSection // SearchResults folded under their Category › Group headings

	// TypeDetail is the Mode "type" body: one item's details
	// page, the target of every item link in the app.
	TypeDetail *itemTypeDetail
}

// itemTypeDetail is one item's details page: identity, guide
// prices, its (worker-warmed) description, and where it fits —
// what it builds into and what builds it.
type itemTypeDetail struct {
	ID       int64
	Name     string
	Category *itemCategoryRow
	Group    *itemGroupRow
	OnMarket bool

	// The page's art: the type's render, with its icon as the
	// fallback the template wires through data-fallback-src.
	// Blueprint-category types have neither on the image
	// server — their art is the /bp image and there is no
	// fallback (ArtFallback "").
	ArtURL      string
	ArtFallback string

	AveragePrice  string // guide prices, "" when unknown
	AdjustedPrice string

	// The worker's true value (truevalue.go), present only
	// for types the market cannot price: the build cost and
	// the estimated sell with its basis in plain words. When
	// an estimated sell exists and the guide has no average,
	// it replaces the adjusted price as the page's price --
	// CCP's number for such a type is a fossil.
	BuildCost         string // "" when no true value is stored
	EstimatedSell     string // "" when no true value is stored
	EstimatedSellNote string // "from N public contracts" | "anchored to build cost"

	HasDescription     bool
	Description        template.HTML // sanitized like mail bodies
	DescriptionPending bool          // asked for; fills in on a coming sync cycle
	DescState          string        // "ready" | "empty" | "pending" — drives the live-region fragment

	BlueprintID int64           // != 0: manufacturable — link the planner
	UsedIn      []itemUsedInRow // blueprints consuming this type, capped
	UsedInMore  bool            // more blueprints use it than are listed

	// ContractSearchURL is the onward link to Contract Search
	// filtered to exactly this type: the trade path for types
	// the market does not carry, and a companion to the market
	// link for the rest. Every item type can appear in a
	// contract, so every type page carries it.
	ContractSearchURL string

	// Market sections, filled by the type-page handler for
	// market-traded types only: swept hub prices and the price
	// history section — the same marketItem view the Market page
	// renders its history from, so both pages chart identically
	// and warm the same way.
	Hubs        []publicHubPrice
	HistoryView *marketItem
}

type itemUsedInRow struct {
	ProductID   int64
	ProductName string
	PerRun      string // formatted material quantity per production run
}

// itemsHeadMeta is the Item Database's head metadata: one
// hand-written description for the explorer, the pattern (with
// breadcrumbs from the trail the page itself draws) for a type.
func (app *Application) itemsHeadMeta(view *itemsView) template.HTML {
	title := "Item Database"
	desc := "Browse EVE Online items by category and group, with market prices, history and trade data for every item, refreshed from ESI."
	path := "/items/"
	var crumbs []module.Breadcrumb
	switch view.Mode {
	case "category":
		if view.Category != nil {
			path = "/items/category/" + idString(view.Category.ID) + "/"
		}
	case "group":
		if view.Group != nil {
			path = "/items/group/" + idString(view.Group.ID) + "/"
		}
	case "type":
		if d := view.TypeDetail; d != nil {
			title = d.Name
			desc = "Market orders, price history and trade data for " + d.Name + ", refreshed from ESI. Part of EveSynapse's EVE Online market tools."
			path = "/items/type/" + idString(d.ID) + "/"
			crumbs = []module.Breadcrumb{{Name: "Item Database", Path: "/items/"}}
			if d.Category != nil {
				crumbs = append(crumbs, module.Breadcrumb{Name: d.Category.Name, Path: "/items/category/" + idString(d.Category.ID) + "/"})
			}
			if d.Group != nil {
				crumbs = append(crumbs, module.Breadcrumb{Name: d.Group.Name, Path: "/items/group/" + idString(d.Group.ID) + "/"})
			}
			crumbs = append(crumbs, module.Breadcrumb{Name: d.Name, Path: path})
		}
	}
	return app.HeadMeta(module.PageMeta{Title: title, Description: desc, CanonicalPath: path, Breadcrumbs: crumbs})
}

func (app *Application) itemsPageData(r *http.Request) pageData {
	// The browser is public: start from the session's login
	// state, never assumed. Personalized bits render only
	// when signed in; signed-in renders are unchanged.
	return app.publicPage(r.Context())
}

// handleItems renders the explorer's top level: every SDE
// category with the number of types under it. With any search
// parameter present it renders the global search instead — the
// two share the URL the way the planner shares its plan URLs.
// publishedOnlyParam reads the published-only switch from a
// request: published=1 today; market=1 is still accepted from
// links shared before the rename (it filtered on marketability
// then; the switch means published-only now).
func publishedOnlyParam(q url.Values) bool {
	return q.Get("published") == "1" || q.Get("market") == "1"
}

func (app *Application) handleItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// A bare published=1 is the browse tree's own switch state
	// (the tree prunes itself in place; the parameter only
	// records the state for links). It joins a flat search
	// only alongside a non-empty query or an explicit
	// category/group pick.
	if q.Get("q") != "" || q.Get("category") != "" || q.Get("group") != "" {
		app.handleItemsSearch(w, r)
		return
	}
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "categories"}
	view.PublishedOnly = publishedOnlyParam(q)

	rows, err := app.queries.ListSDECategoriesWithCounts(ctx)
	if err != nil {
		logging.Errorf("items: list categories: %v", err)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range rows {
			view.Categories = append(view.Categories, itemCategoryRow{
				ID: row.CategoryID, Name: row.Name, Types: esi.FormatInt(row.TypeCount),
				Published: row.PublishedCount, PublishedTypes: esi.FormatInt(row.PublishedCount),
			})
		}
	}
	data.Items = view
	data.HeadMeta = app.itemsHeadMeta(view)
	app.render(ctx, w, http.StatusOK, "items.html", data)
}

// handleItemsSearch renders the global item search: a name
// substring over the whole local type table, narrowable to
// published items only (the SDE published flag — unpublished
// types drop out, marketable or not) and to one category or
// group of the browse tree. Every control is a plain GET
// parameter, so a search is a shareable link and the form
// works untouched without JavaScript.
func (app *Application) handleItemsSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "search", Page: 1}
	q := r.URL.Query()

	view.SearchQuery = q.Get("q")
	view.PublishedOnly = publishedOnlyParam(q)
	if id, err := strconv.ParseInt(q.Get("category"), 10, 64); err == nil && id > 0 {
		if _, err := app.queries.GetSDECategory(ctx, id); err == nil {
			view.CategoryID = id
		}
	}
	if id, err := strconv.ParseInt(q.Get("group"), 10, 64); err == nil && id > 0 {
		if grp, err := app.queries.GetSDEGroup(ctx, id); err == nil {
			view.GroupID = id
			// The group decides its category when the two
			// disagree (a hand-built link can pair anything).
			view.CategoryID = grp.CategoryID
		}
	}
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		view.Page = p
	}

	// Filter options: every category, and the chosen category's
	// groups. Counts ride along from the browse queries; the
	// selects only need names.
	if rows, err := app.queries.ListSDECategoriesWithCounts(ctx); err == nil {
		for _, row := range rows {
			view.Categories = append(view.Categories, itemCategoryRow{ID: row.CategoryID, Name: row.Name})
		}
	} else {
		logging.Errorf("items: search categories: %v", err)
	}
	if view.CategoryID > 0 {
		if rows, err := app.queries.ListSDEGroupsInCategory(ctx, view.CategoryID); err == nil {
			for _, row := range rows {
				view.Groups = append(view.Groups, itemGroupRow{ID: row.GroupID, Name: row.Name})
			}
		} else {
			logging.Errorf("items: search groups of category %d: %v", view.CategoryID, err)
		}
	}

	publishedOnly := int64(0)
	if view.PublishedOnly {
		publishedOnly = 1
	}
	total, err := app.queries.CountSDETypesFiltered(ctx, db.CountSDETypesFilteredParams{
		Q: view.SearchQuery, PublishedOnly: publishedOnly, CategoryID: view.CategoryID, GroupID: view.GroupID,
	})
	if err != nil {
		logging.Errorf("items: count search %q: %v", view.SearchQuery, err)
		data.Error = "Item database unavailable right now — check the server log."
		data.Items = view
		app.render(ctx, w, http.StatusOK, "items.html", data)
		return
	}
	view.TotalTypes = total
	view.TotalPages = int((total + itemsTypesPerPage - 1) / itemsTypesPerPage)
	if view.TotalPages < 1 {
		view.TotalPages = 1
	}
	if view.Page > view.TotalPages {
		view.Page = view.TotalPages
	}
	view.HasPrev = view.Page > 1
	view.HasNext = view.Page < view.TotalPages
	view.PrevPage = view.Page - 1
	view.NextPage = view.Page + 1

	rows, err := app.queries.SearchSDETypesFiltered(ctx, db.SearchSDETypesFilteredParams{
		Q: view.SearchQuery, PublishedOnly: publishedOnly, CategoryID: view.CategoryID, GroupID: view.GroupID,
		Lim: itemsTypesPerPage, Off: int64((view.Page - 1) * itemsTypesPerPage),
	})
	if err != nil {
		logging.Errorf("items: search %q: %v", view.SearchQuery, err)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range rows {
			view.SearchResults = append(view.SearchResults, itemSearchRow{
				ID: row.TypeID, Name: row.Name,
				GroupID: row.GroupID, GroupName: row.GroupName,
				CategoryID: row.CategoryID, CategoryName: row.CategoryName,
				OnMarket: row.MarketGroupID > 0, Published: row.Published > 0,
			})
		}
		view.SearchSections = groupSearchRows(view.SearchResults)
	}
	data.Items = view
	data.HeadMeta = app.itemsHeadMeta(view)
	app.render(ctx, w, http.StatusOK, "items.html", data)
}

// handleItemsCategory renders one category's groups.
func (app *Application) handleItemsCategory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "category"}

	categoryID, err := strconv.ParseInt(chi.URLParam(r, "categoryID"), 10, 64)
	if err != nil || categoryID <= 0 {
		http.Redirect(w, r, "/items/", http.StatusSeeOther)
		return
	}
	cat, err := app.queries.GetSDECategory(ctx, categoryID)
	if err != nil {
		http.Redirect(w, r, "/items/", http.StatusSeeOther)
		return
	}
	view.Category = &itemCategoryRow{ID: cat.CategoryID, Name: cat.Name}
	view.PublishedOnly = publishedOnlyParam(r.URL.Query())

	grows, gerr := app.queries.ListSDEGroupsInCategory(ctx, categoryID)
	if gerr != nil {
		logging.Errorf("items: list groups of category %d: %v", categoryID, gerr)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range grows {
			view.Groups = append(view.Groups, itemGroupRow{
				ID: row.GroupID, Name: row.Name, Types: esi.FormatInt(row.TypeCount),
				Published: row.PublishedCount, PublishedTypes: esi.FormatInt(row.PublishedCount),
			})
		}
	}
	data.Items = view
	data.HeadMeta = app.itemsHeadMeta(view)
	app.render(ctx, w, http.StatusOK, "items.html", data)
}

// handleItemsGroup renders one group's types, paginated, with an
// optional within-group name filter.
func (app *Application) handleItemsGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "group", Page: 1}

	groupID, err := strconv.ParseInt(chi.URLParam(r, "groupID"), 10, 64)
	if err != nil || groupID <= 0 {
		http.Redirect(w, r, "/items/", http.StatusSeeOther)
		return
	}
	grp, err := app.queries.GetSDEGroup(ctx, groupID)
	if err != nil {
		http.Redirect(w, r, "/items/", http.StatusSeeOther)
		return
	}
	view.Group = &itemGroupRow{ID: grp.GroupID, Name: grp.Name}
	if cat, err := app.queries.GetSDECategory(ctx, grp.CategoryID); err == nil {
		view.Category = &itemCategoryRow{ID: cat.CategoryID, Name: cat.Name}
	}

	q := r.URL.Query()
	view.Query = q.Get("q")
	view.PublishedOnly = publishedOnlyParam(q)
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		view.Page = p
	}

	total, err := app.queries.CountSDETypesInGroup(ctx,
		db.CountSDETypesInGroupParams{GroupID: groupID, Lower: view.Query})
	if err != nil {
		logging.Errorf("items: count types of group %d: %v", groupID, err)
		data.Error = "Item database unavailable right now — check the server log."
		data.Items = view
		app.render(ctx, w, http.StatusOK, "items.html", data)
		return
	}
	view.TotalTypes = total
	view.TotalPages = int((total + itemsTypesPerPage - 1) / itemsTypesPerPage)
	if view.TotalPages < 1 {
		view.TotalPages = 1
	}
	if view.Page > view.TotalPages {
		view.Page = view.TotalPages
	}
	view.HasPrev = view.Page > 1
	view.HasNext = view.Page < view.TotalPages
	view.PrevPage = view.Page - 1
	view.NextPage = view.Page + 1

	rows, err := app.queries.ListSDETypesInGroup(ctx,
		db.ListSDETypesInGroupParams{
			GroupID:   groupID,
			Lower:     view.Query,
			RowLimit:  int64(itemsTypesPerPage),
			RowOffset: int64((view.Page - 1) * itemsTypesPerPage),
		})
	if err != nil {
		logging.Errorf("items: list types of group %d: %v", groupID, err)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range rows {
			view.Types = append(view.Types, itemTypeRow{
				ID: row.TypeID, Name: row.Name, OnMarket: row.MarketGroupID > 0, Published: row.Published > 0,
			})
		}
	}
	data.Items = view
	data.HeadMeta = app.itemsHeadMeta(view)
	app.render(ctx, w, http.StatusOK, "items.html", data)
}

// sdeTypeDescription is the bulk-cached invTypes flavor text
// for one type ("" when the dump carries none for it).
func (app *Application) sdeTypeDescription(ctx context.Context, typeID int64) string {
	t, err := app.queries.GetSDEType(ctx, typeID)
	if err != nil {
		return ""
	}
	return t.Description
}

// descriptionState reports one type's description state without
// noting any want: SDE-bulk text first, then the worker-stored
// ESI answer. settled=false means a fetch is still owed.
func (app *Application) descriptionState(ctx context.Context, typeID int64) (state string, settled bool) {
	if app.sdeTypeDescription(ctx, typeID) != "" {
		return "ready", true
	}
	td, err := app.queries.GetTypeDetail(ctx, typeID)
	if err == nil && td.FetchedAt.Valid {
		if td.Description != "" {
			return "ready", true
		}
		return "empty", true
	}
	return "pending", false
}

// itemDescription resolves one type's description state: the
// local SDE dump first (bulk-cached for every type by the weekly
// import), then the type_details queue — "ready" with sanitized
// HTML when text exists, "empty" once ESI settled with none, and
// "pending" while the description is still on its way (noting the
// want so the worker fills it). Shared by the item page and its
// live-region fragment.
func (app *Application) itemDescription(ctx context.Context, typeID int64) (string, template.HTML) {
	if desc := app.sdeTypeDescription(ctx, typeID); desc != "" {
		return "ready", htmlsanitize.Sanitize(desc)
	}
	td, err := app.queries.GetTypeDetail(ctx, typeID)
	switch {
	case err == nil && td.FetchedAt.Valid && td.Description != "":
		return "ready", htmlsanitize.Sanitize(td.Description)
	case err == nil && td.FetchedAt.Valid:
		return "empty", ""
	case app.userID(ctx) == 0:
		// Anonymous renders queue no work: what isn't stored yet
		// reads as no description, and nothing polls for it.
		return "empty", ""
	default:
		if qerr := app.queries.UpsertTypeDetailWant(ctx, db.UpsertTypeDetailWantParams{
			TypeID: typeID, Priority: viewedPriority(ctx), NotedAt: timeSet(time.Now().UTC()),
		}); qerr != nil {
			logging.Errorf("items: note type detail want for %d: %v", typeID, qerr)
		}
		return "pending", ""
	}
}

// handleItemType renders one item's details page — the target of
// every item link in EveSynapse (planner rows, wallet entries,
// market watchlists, skill names…). Identity and "used in" facts
// are local SDE reads; the description comes from the type_details
// warm queue (public ESI type payloads), so a first visit notes
// the want and shows a filling-in state. Onward links:
// orders & price history on the Market page, the Industry
// planner when the item can be built, and Contract Search
// filtered to this type for every item.
func (app *Application) handleItemType(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "type"}

	typeID, err := strconv.ParseInt(chi.URLParam(r, "typeID"), 10, 64)
	if err != nil || typeID <= 0 {
		http.Redirect(w, r, "/items/", http.StatusSeeOther)
		return
	}
	t, err := app.queries.GetSDEType(ctx, typeID)
	if err != nil {
		http.Redirect(w, r, "/items/", http.StatusSeeOther)
		return
	}
	// Anonymous renders never queue work: the description
	// warms on signed-in visits, crawlers get stored rows.
	if data.LoggedIn {
		app.markGuessHit(ctx, pageWantTypeDescription, typeID, 0)
	}

	detail := app.buildItemTypeDetail(ctx, t)
	if data.LoggedIn && detail.DescState == "pending" {
		app.notePageWant(ctx, pageWantTypeDescription, typeID, 0)
	}

	if detail.OnMarket {
		hubs, forge := app.publicTypeMarket(ctx, typeID)
		detail.Hubs = hubs
		if forge <= 0 {
			// No swept stats anywhere yet: chart the default
			// region like the Market page does, so the section
			// can ask for its history and fill in when it lands.
			forge = defaultMarketRegion
		}
		regionName, _ := marketRegionName(forge)
		history := &marketItem{
			TypeID: typeID, RegionID: forge, RegionName: regionName,
			Anonymous: !data.LoggedIn,
		}
		app.attachHistory(ctx, history, typeID, forge, app.userID(ctx))
		detail.HistoryView = history
		// The same warming contract as the Market item view:
		// a visit whose history is still pending leaves a page
		// want, and the section's live region polls the shared
		// fragment until the chart swaps in. This is a market
		// want, so anonymous visits leave it too while the
		// anonymous-market switch is on (market_anon.go); the
		// type-description want above stays signed-in only.
		if data.LoggedIn {
			app.markGuessHit(ctx, pageWantHistory, typeID, forge)
		}
		if (data.LoggedIn || !app.anonMarketBlocked()) && history.HistoryPending {
			app.notePageWant(ctx, pageWantHistory, typeID, forge)
		}
	}

	view.TypeDetail = detail
	data.Items = view
	data.HeadMeta = app.itemsHeadMeta(view)
	app.render(ctx, w, http.StatusOK, "items.html", data)
}

// buildItemTypeDetail assembles one type's SDE identity,
// description (local dump first, worker-stored details second),
// guide prices and manufacturing links. UsedIn caps at 50 rows:
// bulk materials feed thousands of blueprints. Shared by the
// signed-in item page and the public one; callers own want
// queueing (the public page queues nothing itself).
func (app *Application) buildItemTypeDetail(ctx context.Context, t db.SdeType) *itemTypeDetail {
	typeID := t.TypeID
	detail := &itemTypeDetail{
		ID:       typeID,
		Name:     t.Name,
		OnMarket: t.MarketGroupID > 0,
	}
	detail.ContractSearchURL = "/contracts/search/?item_type=" + idString(typeID)
	if grp, gerr := app.queries.GetSDEGroup(ctx, t.GroupID); gerr == nil {
		detail.Group = &itemGroupRow{ID: grp.GroupID, Name: grp.Name}
		if cat, cerr := app.queries.GetSDECategory(ctx, grp.CategoryID); cerr == nil {
			detail.Category = &itemCategoryRow{ID: cat.CategoryID, Name: cat.Name}
		}
	}

	// Art: the render, falling back to the icon — except the
	// blueprint category (blueprints, reaction formulas),
	// which has neither asset and serves /bp instead. The
	// item page shows the type itself, never a copy, so the
	// /bpc variant does not apply here.
	detail.ArtURL = "https://images.evetech.net/types/" + strconv.FormatInt(typeID, 10) + "/render?size=128"
	detail.ArtFallback = "https://images.evetech.net/types/" + strconv.FormatInt(typeID, 10) + "/icon?size=128"
	if detail.Category != nil && detail.Category.ID == contracts.BlueprintCategoryID {
		detail.ArtURL = contracts.TypeIconURL(typeID, true, false, 128)
		detail.ArtFallback = ""
	}

	// Description: the local SDE dump first; the worker's
	// type-details drain is only the fallback for types the dump
	// has no text for.
	detail.DescState, detail.Description = app.itemDescription(ctx, typeID)
	detail.HasDescription = detail.DescState == "ready"
	detail.DescriptionPending = detail.DescState == "pending"

	if p, ok := app.cachedPrices()[typeID]; ok {
		if p.AveragePrice > 0 {
			detail.AveragePrice = esi.FormatISK(p.AveragePrice)
		}
		if p.AdjustedPrice > 0 {
			detail.AdjustedPrice = esi.FormatISK(p.AdjustedPrice)
		}
	}

	// True value: the stored bottom-up price, shown for the
	// types it exists for (the worker stores an estimated
	// sell only where the market cannot price the type).
	// Without a guide average it is the price of record, so
	// the adjusted row steps aside rather than posing as one.
	if tv, ok := app.storedTrueValues(ctx)[typeID]; ok {
		if est, ok := storedEstimate(tv); ok {
			if tv.BuildCost > 0 {
				detail.BuildCost = esi.FormatISK(tv.BuildCost)
			}
			detail.EstimatedSell = esi.FormatISK(est)
			detail.EstimatedSellNote = trueValueBasis(tv)
			if detail.AveragePrice == "" {
				detail.AdjustedPrice = ""
			}
		}
	}

	if bp, berr := app.queries.GetSDEBlueprintForProduct(ctx, typeID); berr == nil {
		detail.BlueprintID = bp.BlueprintTypeID
	}

	if rows, uerr := app.queries.ListSDEBlueprintsUsingMaterial(ctx, typeID); uerr == nil {
		for _, row := range rows {
			if len(detail.UsedIn) >= 50 {
				detail.UsedInMore = true
				break
			}
			product, perr := app.queries.GetSDEType(ctx, row.ProductTypeID)
			if perr != nil || product.Name == "" {
				continue
			}
			detail.UsedIn = append(detail.UsedIn, itemUsedInRow{
				ProductID:   row.ProductTypeID,
				ProductName: product.Name,
				PerRun:      esi.FormatInt(row.MaterialQuantity),
			})
		}
	} else {
		logging.Errorf("items: list blueprints using %d: %v", typeID, uerr)
	}
	return detail
}
