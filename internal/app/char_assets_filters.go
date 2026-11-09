package app

import (
	"context"
	json "github.com/goccy/go-json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
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
	Scope int64  `json:"scope,omitempty"` // legacy: one character, 0 = all
	// Scopes is the character set the scope selector picks; it
	// supersedes Scope when present. Filters saved before the
	// selector carried only Scope.
	Scopes []int64 `json:"scopes,omitempty"`
}

// scopeSet is the filter's character scope as a set: the saved
// set when there is one, else the legacy single character, else
// nothing (all characters).
func (f assetSavedFilter) scopeSet() []int64 {
	if len(f.Scopes) > 0 {
		return f.Scopes
	}
	if f.Scope > 0 {
		return []int64{f.Scope}
	}
	return nil
}

type assetFiltersBlob struct {
	Filters []assetSavedFilter `json:"filters,omitempty"`
}

// href is the page URL this filter applies.
func (f assetSavedFilter) href() string {
	set := f.scopeSet()
	st := assetPageState{tab: "all", sort: "value", scopeAll: len(set) == 0, scopeIDs: set, query: f.Query}
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
		if !errors.Is(err, pgx.ErrNoRows) {
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
// or the subset of the posted character IDs the account owns,
// in account order.
func validFilterScope(ctx context.Context, app *Application, userID int64, raw string) []int64 {
	if raw == "" || raw == "all" {
		return nil
	}
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return nil
	}
	picked := make(map[int64]bool)
	for _, part := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			picked[id] = true
		}
	}
	var out []int64
	for _, ch := range characters {
		if picked[ch.CharacterID] {
			out = append(out, ch.CharacterID)
		}
	}
	if len(out) == len(characters) {
		return nil
	}
	return out
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
		Name:   strings.TrimSpace(r.FormValue("name")),
		Query:  strings.TrimSpace(r.FormValue("q")),
		Tab:    r.FormValue("tab"),
		Scopes: validFilterScope(ctx, app, userID, r.FormValue("scope")),
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
	if set := validFilterScope(ctx, app, userID, r.FormValue("scope")); len(set) > 0 {
		back.Set("scope", joinIDList(set))
	}
	target := "/assets/"
	if len(back) > 0 {
		target += "?" + back.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
