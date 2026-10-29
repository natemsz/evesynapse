package app

// The basket builder's JSON endpoints: the list page builds and
// edits baskets without a page load, picking items from the
// suggest feed and saving them by type ID. The classic form
// posts keep working beside them (the builder's text-entry mode
// is that form).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

type basketAPIItem struct {
	ID  int64 `json:"id"`
	Qty int64 `json:"qty"`
}

type basketAPISave struct {
	Name   string          `json:"name"`
	Region int64           `json:"region"`
	Items  []basketAPIItem `json:"items"`
}

func writeBasketJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func basketJSONError(w http.ResponseWriter, status int, msg string) {
	writeBasketJSON(w, status, map[string]string{"error": msg})
}

// basketTypicalPrice formats one type's typical sell in a region
// from the stat pack, the price the basket itself carries; ""
// when the region has no swept price for it.
func (app *Application) basketTypicalPrices(ctx context.Context, regionID int64, typeIDs []int64) map[int64]string {
	out := make(map[int64]string, len(typeIDs))
	packs, err := app.loadStatPacks(ctx, regionID, typeIDs)
	if err != nil {
		logging.Errorf("baskets: prices: %v", err)
		return out
	}
	for _, id := range typeIDs {
		if p := packs[id]; p != nil && p.Own != nil && p.Own.TypicalSell > 0 {
			out[id] = esi.FormatISK(p.Own.TypicalSell)
		}
	}
	return out
}

// handleMarketBasketItemsJSON serves GET
// /market/baskets/{basketID}/items.json: the owner's editor seed —
// the basket's name, region and items with current typical prices.
func (app *Application) handleMarketBasketItemsJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, ok := app.loadBasketFor(ctx, chi.URLParam(r, "basketID"), app.userID(ctx))
	if !ok || !b.UserID.Valid {
		http.NotFound(w, r)
		return
	}
	items, err := app.queries.ListMarketBasketItems(ctx, b.ID)
	if err != nil {
		logging.Errorf("baskets: items for %d: %v", b.ID, err)
		basketJSONError(w, http.StatusInternalServerError, "Couldn't read that basket right now.")
		return
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.TypeID)
	}
	prices := app.basketTypicalPrices(ctx, b.RegionID, ids)
	type itemJSON struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Qty   int64  `json:"qty"`
		Price string `json:"price"`
	}
	out := make([]itemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, itemJSON{ID: it.TypeID, Name: app.typeNameOrID(ctx, it.TypeID), Qty: it.Quantity, Price: prices[it.TypeID]})
	}
	writeBasketJSON(w, http.StatusOK, map[string]any{
		"name": b.Name, "region": b.RegionID, "items": out,
	})
}

// handleMarketBasketPrices serves GET
// /market/baskets/prices?region=&types=1,2,3: typical prices for
// items the builder just picked, in the builder's region.
func (app *Application) handleMarketBasketPrices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	regionID := basketRegion(r.URL.Query().Get("region"))
	var ids []int64
	for _, raw := range strings.Split(r.URL.Query().Get("types"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) > basketMaxItems {
		ids = ids[:basketMaxItems]
	}
	prices := app.basketTypicalPrices(ctx, regionID, ids)
	type priceJSON struct {
		ID    int64  `json:"id"`
		Price string `json:"price"`
	}
	out := make([]priceJSON, 0, len(ids))
	for _, id := range ids {
		out = append(out, priceJSON{ID: id, Price: prices[id]})
	}
	writeBasketJSON(w, http.StatusOK, map[string]any{"prices": out})
}

func decodeBasketSave(w http.ResponseWriter, r *http.Request) (basketAPISave, bool) {
	var in basketAPISave
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		basketJSONError(w, http.StatusBadRequest, "That basket didn't come through. Try again.")
		return in, false
	}
	return in, true
}

func basketQtyItems(in []basketAPIItem) []basketItemQty {
	out := make([]basketItemQty, 0, len(in))
	for _, it := range in {
		out = append(out, basketItemQty{TypeID: it.ID, Quantity: it.Qty})
	}
	return out
}

// handleMarketBasketCreateAPI serves POST /market/baskets/api:
// the builder's save. Mirrors the form create's checks.
func (app *Application) handleMarketBasketCreateAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	in, ok := decodeBasketSave(w, r)
	if !ok {
		return
	}
	if n, err := app.queries.CountMarketBasketsForUser(ctx, sql.NullInt64{Int64: userID, Valid: true}); err == nil && n >= basketMaxPerUser {
		basketJSONError(w, http.StatusBadRequest, fmt.Sprintf("You can keep up to %d baskets. Delete one to make another.", basketMaxPerUser))
		return
	}
	name := basketName(in.Name)
	if name == "" {
		basketJSONError(w, http.StatusBadRequest, "Give the basket a name.")
		return
	}
	if len(in.Items) > basketMaxItems {
		basketJSONError(w, http.StatusBadRequest, fmt.Sprintf("A basket holds at most %d items.", basketMaxItems))
		return
	}
	regionID := in.Region
	if _, ok := marketRegionName(regionID); !ok {
		regionID = defaultMarketRegion
	}
	id, err := app.queries.CreateMarketBasket(ctx, db.CreateMarketBasketParams{
		UserID: sql.NullInt64{Int64: userID, Valid: true}, Name: name, RegionID: regionID,
	})
	if err != nil {
		logging.Errorf("baskets: create for user %d: %v", userID, err)
		basketJSONError(w, http.StatusInternalServerError, "Couldn't save the basket right now.")
		return
	}
	if err := app.saveBasketItemsByID(ctx, id, basketQtyItems(in.Items)); err != nil {
		logging.Errorf("baskets: items for %d: %v", id, err)
		basketJSONError(w, http.StatusInternalServerError, "Saved the basket, but not its items. Try editing it.")
		return
	}
	regionName, _ := marketRegionName(regionID)
	writeBasketJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": name, "region_name": regionName, "items": len(in.Items),
	})
}

// handleMarketBasketEditAPI serves POST
// /market/baskets/{basketID}/api-edit: the inline editor's save,
// replacing the basket's items by type ID.
func (app *Application) handleMarketBasketEditAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, ok := app.loadBasketFor(ctx, chi.URLParam(r, "basketID"), app.userID(ctx))
	if !ok || !b.UserID.Valid {
		http.NotFound(w, r)
		return
	}
	in, ok := decodeBasketSave(w, r)
	if !ok {
		return
	}
	if len(in.Items) > basketMaxItems {
		basketJSONError(w, http.StatusBadRequest, fmt.Sprintf("A basket holds at most %d items.", basketMaxItems))
		return
	}
	if err := app.saveBasketItemsByID(ctx, b.ID, basketQtyItems(in.Items)); err != nil {
		logging.Errorf("baskets: items for %d: %v", b.ID, err)
		basketJSONError(w, http.StatusInternalServerError, "Couldn't save the items right now.")
		return
	}
	writeBasketJSON(w, http.StatusOK, map[string]any{"ok": true, "items": len(in.Items)})
}

// handleMarketBasketSuggest serves GET /market/baskets/suggest:
// the basket editor's search. Item rows come from the shared
// market feed; market group rows ride on top of them, labelled
// with the size of the group's whole subtree. Picking a group
// row bulk-adds its types (handleMarketBasketGroupTypes).
func (app *Application) handleMarketBasketSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := []suggestItem{}
	if len(q) < 2 {
		writeSuggestJSON(w, out)
		return
	}
	groups, err := app.queries.SuggestSDEMarketGroups(ctx, db.SuggestSDEMarketGroupsParams{Q: q, Lim: 4})
	if err != nil {
		logging.Errorf("baskets: group suggest %q: %v", q, err)
	}
	for _, g := range groups {
		n, err := app.queries.CountSDETypesInMarketGroupSubtree(ctx, g.MarketGroupID)
		if err != nil {
			logging.Errorf("baskets: group count %d: %v", g.MarketGroupID, err)
			continue
		}
		if n == 0 {
			continue
		}
		label := fmt.Sprintf("Group · %d items", n)
		if n == 1 {
			label = "Group · 1 item"
		}
		out = append(out, suggestItem{ID: g.MarketGroupID, Name: g.Name, Kind: "group", Label: label})
	}
	out = append(out, app.suggestTypes(ctx, q, suggestPoolMarket, 8)...)
	writeSuggestJSON(w, out)
}

type basketGroupTypes struct {
	ID    int64         `json:"id"`
	Name  string        `json:"name"`
	Count int64         `json:"count"`
	Items []suggestItem `json:"items"`
}

// handleMarketBasketGroupTypes serves GET
// /market/baskets/group-types?id=: every marketable type in a
// market group's subtree, name order, for the editor's bulk
// add. The list stops past the basket cap's reach; Count always
// carries the subtree's true size so the editor can refuse a
// group that cannot fit before adding anything.
func (app *Application) handleMarketBasketGroupTypes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		basketJSONError(w, http.StatusBadRequest, "That group could not be loaded.")
		return
	}
	g, err := app.queries.GetSDEMarketGroup(ctx, id)
	if err != nil {
		basketJSONError(w, http.StatusNotFound, "That group could not be loaded.")
		return
	}
	count, err := app.queries.CountSDETypesInMarketGroupSubtree(ctx, id)
	if err != nil {
		logging.Errorf("baskets: group count %d: %v", id, err)
		basketJSONError(w, http.StatusInternalServerError, "That group could not be loaded.")
		return
	}
	rows, err := app.queries.ListSDETypesInMarketGroupSubtree(ctx, db.ListSDETypesInMarketGroupSubtreeParams{
		GroupID: id, RowLimit: 1000,
	})
	if err != nil {
		logging.Errorf("baskets: group types %d: %v", id, err)
		basketJSONError(w, http.StatusInternalServerError, "That group could not be loaded.")
		return
	}
	items := make([]suggestItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, suggestItem{ID: row.TypeID, Name: row.Name})
	}
	writeBasketJSON(w, http.StatusOK, basketGroupTypes{ID: g.MarketGroupID, Name: g.Name, Count: count, Items: items})
}
