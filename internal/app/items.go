package app

import (
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

// itemsView is the Item Database page body: exactly one of the
// three levels is populated per render.
type itemsView struct {
	Mode string // "categories" | "category" | "group"

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
}

func (app *Application) itemsPageData(r *http.Request) pageData {
	return pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(r.Context(), sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
}

// handleItems renders the explorer's top level: every SDE
// category with the number of types under it.
func (app *Application) handleItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.itemsPageData(r)
	view := &itemsView{Mode: "categories"}

	rows, err := app.queries.ListSDECategoriesWithCounts(ctx)
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

	rows, err := app.queries.ListSDEGroupsInCategory(ctx, categoryID)
	if err != nil {
		log.Printf("items: list groups of category %d: %v", categoryID, err)
		data.Error = "Item database unavailable right now — check the server log."
	} else {
		for _, row := range rows {
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
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		view.Page = p
	}

	total, err := app.queries.CountSDETypesInGroupFiltered(ctx,
		db.CountSDETypesInGroupFilteredParams{GroupID: groupID, LOWER: view.Query})
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
			GroupID: groupID,
			LOWER:   view.Query,
			Limit:   int64(itemsTypesPerPage),
			Offset:  int64((view.Page - 1) * itemsTypesPerPage),
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
