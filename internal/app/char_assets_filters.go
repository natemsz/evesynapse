package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Saved quick filters for the Assets page: a user's own named
// views, each a search text plus a hangar tab plus a character
// scope. They are the user's, not shipped views: the chip row
// holds only what the user saved. Storage follows the notifyPrefs
// precedent: one JSON blob per account in widget_configs.
// ---------------------------------------------------------------------------

const assetsFiltersConfigID = "assets_filters"

// maxAssetFilters caps the saved set, oldest first out.
const maxAssetFilters = 12

type assetSavedFilter struct {
	Name  string `json:"name"`
	Query string `json:"q,omitempty"`
	Tab   string `json:"tab,omitempty"`
	Scope int64  `json:"scope,omitempty"` // 0 = all characters
}

type assetFiltersBlob struct {
	Filters []assetSavedFilter `json:"filters,omitempty"`
}

// href is the page URL this filter applies.
func (f assetSavedFilter) href() string {
	st := assetPageState{tab: "all", sort: "value", scopeAll: f.Scope == 0, scopeID: f.Scope, query: f.Query}
	if f.Tab != "" {
		st.tab = f.Tab
	}
	return st.href(nil)
}

// assetFiltersFor reads and validates the account's saved
// filters. A broken or absent blob reads as none.
func (app *Application) assetFiltersFor(ctx context.Context, userID int64) []assetSavedFilter {
	raw, err := app.queries.GetWidgetConfig(ctx, db.GetWidgetConfigParams{UserID: userID, WidgetID: assetsFiltersConfigID})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("assets: load filters for user %d: %v", userID, err)
		}
		return nil
	}
	var blob assetFiltersBlob
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		return nil
	}
	out := blob.Filters[:0]
	for _, f := range blob.Filters {
		f.Name = strings.TrimSpace(f.Name)
		if f.Name == "" {
			continue
		}
		if !validAssetTab(f.Tab) {
			f.Tab = "all"
		}
		out = append(out, f)
	}
	return out
}

// storeAssetFilters writes the account's saved filters back,
// following the notifyPrefs write: one upsert of the whole blob.
func (app *Application) storeAssetFilters(ctx context.Context, userID int64, filters []assetSavedFilter) error {
	raw, err := json.Marshal(assetFiltersBlob{Filters: filters})
	if err != nil {
		return err
	}
	return app.queries.UpsertWidgetConfig(ctx, db.UpsertWidgetConfigParams{
		UserID: userID, WidgetID: assetsFiltersConfigID, Config: string(raw),
	})
}

// validFilterScope keeps a posted scope honest: all characters,
// or one character the account owns.
func validFilterScope(ctx context.Context, app *Application, userID int64, raw string) int64 {
	if raw == "" || raw == "all" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id == 0 {
		return 0
	}
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return 0
	}
	for _, ch := range characters {
		if ch.CharacterID == id {
			return id
		}
	}
	return 0
}

// handleAssetsFilterSave stores the posted view under the posted
// name (replacing a filter of the same name), then lands on the
// filter it just saved.
func (app *Application) handleAssetsFilterSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/assets/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/assets/", http.StatusSeeOther)
		return
	}
	f := assetSavedFilter{
		Name:  strings.TrimSpace(r.FormValue("name")),
		Query: strings.TrimSpace(r.FormValue("q")),
		Tab:   r.FormValue("tab"),
		Scope: validFilterScope(ctx, app, userID, r.FormValue("scope")),
	}
	if len(f.Name) > 40 {
		f.Name = f.Name[:40]
	}
	if len(f.Query) > 100 {
		f.Query = f.Query[:100]
	}
	if !validAssetTab(f.Tab) {
		f.Tab = "all"
	}
	if f.Name == "" {
		http.Redirect(w, r, "/assets/", http.StatusSeeOther)
		return
	}
	filters := app.assetFiltersFor(ctx, userID)
	next := filters[:0]
	for _, old := range filters {
		if old.Name != f.Name {
			next = append(next, old)
		}
	}
	filters = append(next, f)
	for len(filters) > maxAssetFilters {
		filters = filters[1:]
	}
	if err := app.storeAssetFilters(ctx, userID, filters); err != nil {
		logging.Errorf("assets: save filter for user %d: %v", userID, err)
	}
	http.Redirect(w, r, f.href(), http.StatusSeeOther)
}

// handleAssetsFilterDelete removes one saved filter by name and
// returns to the view the user was looking at.
func (app *Application) handleAssetsFilterDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/assets/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/assets/", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	filters := app.assetFiltersFor(ctx, userID)
	next := filters[:0]
	for _, old := range filters {
		if old.Name != name {
			next = append(next, old)
		}
	}
	if len(next) != len(filters) {
		if err := app.storeAssetFilters(ctx, userID, next); err != nil {
			logging.Errorf("assets: delete filter for user %d: %v", userID, err)
		}
	}
	back := url.Values{}
	if q := strings.TrimSpace(r.FormValue("q")); q != "" {
		back.Set("q", q)
	}
	if tab := r.FormValue("tab"); validAssetTab(tab) && tab != "all" {
		back.Set("tab", tab)
	}
	if scope := validFilterScope(ctx, app, userID, r.FormValue("scope")); scope != 0 {
		back.Set("scope", strconv.FormatInt(scope, 10))
	}
	target := "/assets/"
	if len(back) > 0 {
		target += "?" + back.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
