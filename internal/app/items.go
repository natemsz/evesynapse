package app

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Item database explorer: browse the local SDE category → group →
// type tree. Every read is a local SDE query — no ESI, no token.
// Types link into the Market item view.
// ---------------------------------------------------------------------------

// itemsTypesPerPage paginates a group's type list.
const itemsTypesPerPage = 100

// itemCategoryRow is one category with its total type count.
type itemCategoryRow struct {
	ID    int64
	Name  string
	Types string // formatted count
}

// itemGroupRow is one group with its type count.
type itemGroupRow struct {
	ID    int64
	Name  string
	Types string // formatted count
}

// itemTypeRow is one type in a group listing.
type itemTypeRow struct {
	ID       int64
	Name     string
	OnMarket bool
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

	// Global search state (Mode "search", also the group page's
	// market-only toggle): shareable through GET parameters,
	// exactly like the planner's plan URLs.
	SearchQuery   string
	MarketOnly    bool
	CategoryID    int64
	GroupID       int64
	SearchResults []itemSearchRow

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

	AveragePrice  string // guide prices, "" when unknown
	AdjustedPrice string

	HasDescription     bool
	Description        template.HTML // sanitized like mail bodies
	DescriptionPending bool          // asked for; fills in on a coming sync cycle
	DescState          string        // "ready" | "empty" | "pending" — drives the live-region fragment

	BlueprintID int64           // != 0: manufacturable — link the planner
	UsedIn      []itemUsedInRow // blueprints consuming this type
}

type itemUsedInRow struct {
	ProductID   int64
	ProductName string
	PerRun      string // formatted material quantity per production run
}

func (app *Application) itemsPageData(r *http.Request) pageData {
	return pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(r.Context(), sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
}

// handleItems renders the explorer's top level: every SDE
// category with the number of types under it. With any search
// parameter present it renders the global search instead — the
// two share the URL the way the planner shares its plan URLs.
func (app *Application) handleItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Issue 22: market=1 alone filters the browse tree; it only
	// joins a flat search when there's a non-empty query or an
	// explicit category/group pick.
	if q.Get("q") != "" || q.Get("category") != "" || q.Get("group") != "" {
		app.handleItemsSearch(w, r)
		return
	}
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "categories"}
	view.MarketOnly = q.Get("market") == "1"

	var rows []db.ListSDECategoriesWithCountsFilteredRow
	var err error
	if view.MarketOnly {
		rows, err = app.queries.ListSDECategoriesWithCountsFiltered(ctx, 1)
	} else {
		var unfiltered []db.ListSDECategoriesWithCountsRow
		unfiltered, err = app.queries.ListSDECategoriesWithCounts(ctx)
		// Normalize to the filtered row type for the shared loop.
		for _, row := range unfiltered {
			rows = append(rows, db.ListSDECategoriesWithCountsFilteredRow{
				CategoryID: row.CategoryID, Name: row.Name, TypeCount: row.TypeCount,
			})
		}
	}
	if err != nil {
		log.Printf("items: list categories: %v", err)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range rows {
			view.Categories = append(view.Categories, itemCategoryRow{
				ID: row.CategoryID, Name: row.Name, Types: esi.FormatInt(row.TypeCount),
			})
		}
	}
	data.Items = view
	app.render(ctx, w, http.StatusOK, "items.html", data)
}

// handleItemsSearch renders the global item search: a name
// substring over the whole local type table, narrowable to
// market items only (market group + published — everything
// else, unpublished types included, drops out) and to one
// category or group of the browse tree. Every control is a
// plain GET parameter, so a search is a shareable link and the
// form works untouched without JavaScript.
func (app *Application) handleItemsSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "search", Page: 1}
	q := r.URL.Query()

	view.SearchQuery = q.Get("q")
	view.MarketOnly = q.Get("market") == "1"
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
		log.Printf("items: search categories: %v", err)
	}
	if view.CategoryID > 0 {
		if rows, err := app.queries.ListSDEGroupsInCategory(ctx, view.CategoryID); err == nil {
			for _, row := range rows {
				view.Groups = append(view.Groups, itemGroupRow{ID: row.GroupID, Name: row.Name})
			}
		} else {
			log.Printf("items: search groups of category %d: %v", view.CategoryID, err)
		}
	}

	marketOnly := int64(0)
	if view.MarketOnly {
		marketOnly = 1
	}
	total, err := app.queries.CountSDETypesFiltered(ctx, db.CountSDETypesFilteredParams{
		Q: view.SearchQuery, MarketOnly: marketOnly, CategoryID: view.CategoryID, GroupID: view.GroupID,
	})
	if err != nil {
		log.Printf("items: count search %q: %v", view.SearchQuery, err)
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
		Q: view.SearchQuery, MarketOnly: marketOnly, CategoryID: view.CategoryID, GroupID: view.GroupID,
		Lim: itemsTypesPerPage, Off: int64((view.Page - 1) * itemsTypesPerPage),
	})
	if err != nil {
		log.Printf("items: search %q: %v", view.SearchQuery, err)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range rows {
			view.SearchResults = append(view.SearchResults, itemSearchRow{
				ID: row.TypeID, Name: row.Name,
				GroupID: row.GroupID, GroupName: row.GroupName,
				CategoryID: row.CategoryID, CategoryName: row.CategoryName,
				OnMarket: row.MarketGroupID > 0,
			})
		}
	}
	data.Items = view
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
	view.MarketOnly = r.URL.Query().Get("market") == "1"

	var grows []db.ListSDEGroupsInCategoryFilteredRow
	var gerr error
	if view.MarketOnly {
		grows, gerr = app.queries.ListSDEGroupsInCategoryFiltered(ctx, db.ListSDEGroupsInCategoryFilteredParams{
			CategoryID: categoryID, MarketOnly: 1,
		})
	} else {
		var unfiltered []db.ListSDEGroupsInCategoryRow
		unfiltered, gerr = app.queries.ListSDEGroupsInCategory(ctx, categoryID)
		for _, row := range unfiltered {
			grows = append(grows, db.ListSDEGroupsInCategoryFilteredRow{
				GroupID: row.GroupID, Name: row.Name, TypeCount: row.TypeCount,
			})
		}
	}
	if gerr != nil {
		log.Printf("items: list groups of category %d: %v", categoryID, gerr)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range grows {
			view.Groups = append(view.Groups, itemGroupRow{
				ID: row.GroupID, Name: row.Name, Types: esi.FormatInt(row.TypeCount),
			})
		}
	}
	data.Items = view
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
	view.MarketOnly = q.Get("market") == "1"
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		view.Page = p
	}

	marketOnly := int64(0)
	if view.MarketOnly {
		marketOnly = 1
	}
	total, err := app.queries.CountSDETypesInGroupFiltered(ctx,
		db.CountSDETypesInGroupFilteredParams{GroupID: groupID, Lower: view.Query, MarketOnly: marketOnly})
	if err != nil {
		log.Printf("items: count types of group %d: %v", groupID, err)
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
			GroupID:    groupID,
			Lower:      view.Query,
			MarketOnly: marketOnly,
			RowLimit:   int64(itemsTypesPerPage),
			RowOffset:  int64((view.Page - 1) * itemsTypesPerPage),
		})
	if err != nil {
		log.Printf("items: list types of group %d: %v", groupID, err)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range rows {
			view.Types = append(view.Types, itemTypeRow{
				ID: row.TypeID, Name: row.Name, OnMarket: row.MarketGroupID > 0,
			})
		}
	}
	data.Items = view
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
	if err == nil && td.FetchedAt != "" {
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
		return "ready", sanitizeMailHTML(desc)
	}
	td, err := app.queries.GetTypeDetail(ctx, typeID)
	switch {
	case err == nil && td.FetchedAt != "" && td.Description != "":
		return "ready", sanitizeMailHTML(td.Description)
	case err == nil && td.FetchedAt != "":
		return "empty", ""
	default:
		if qerr := app.queries.UpsertTypeDetailWant(ctx, typeID); qerr != nil {
			log.Printf("items: note type detail want for %d: %v", typeID, qerr)
		}
		return "pending", ""
	}
}

// handleItemType renders one item's details page — the target of
// every item link in EveSynapse (planner rows, wallet entries,
// market watchlists, skill names…). Identity and "used in" facts
// are local SDE reads; the description comes from the type_details
// warm queue (public ESI type payloads), so a first visit notes
// the want and shows an honest filling-in state. Onward links:
// orders & price history on the Market page, and the Industry
// planner when the item can be built.
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

	detail := &itemTypeDetail{
		ID:       t.TypeID,
		Name:     t.Name,
		OnMarket: t.MarketGroupID > 0,
	}
	if grp, gerr := app.queries.GetSDEGroup(ctx, t.GroupID); gerr == nil {
		detail.Group = &itemGroupRow{ID: grp.GroupID, Name: grp.Name}
		if cat, cerr := app.queries.GetSDECategory(ctx, grp.CategoryID); cerr == nil {
			detail.Category = &itemCategoryRow{ID: cat.CategoryID, Name: cat.Name}
		}
	}

	// Description: the local SDE dump first; the worker's
	// type-details drain is only the fallback for types the dump
	// has no text for.
	detail.DescState, detail.Description = app.itemDescription(ctx, typeID)
	if detail.DescState == "pending" {
		app.notePageWant(ctx, pageWantTypeDescription, typeID, 0)
	}
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

	if bp, berr := app.queries.GetSDEBlueprintForProduct(ctx, typeID); berr == nil {
		detail.BlueprintID = bp.BlueprintTypeID
	}

	if rows, uerr := app.queries.ListSDEBlueprintsUsingMaterial(ctx, typeID); uerr == nil {
		for _, row := range rows {
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
		log.Printf("items: list blueprints using %d: %v", typeID, uerr)
	}

	view.TypeDetail = detail
	data.Items = view
	app.render(ctx, w, http.StatusOK, "items.html", data)
}
