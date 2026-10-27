package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// themeConfigID is the widget_configs id the account theme lives
// under, the same per-account blob store the notification settings
// use (notify.go's notifyConfigID).
const themeConfigID = "theme"

// themePrefs is an account's theme (Settings, Interface tab): the
// colour scheme (empty is the default ember look), the scheme's
// tier, and the backdrop strength the default look was left at
// (empty is full). The backdrop rides along under a scheme, unused,
// so returning to the default brings back the strength it had,
// exactly as the picker's localStorage copy has always behaved.
type themePrefs struct {
	Scheme   string `json:"scheme,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Backdrop string `json:"backdrop,omitempty"`
}

// themeSchemes are the scheme names the picker can produce; they
// are the values boot.js accepts for the evesynapse-scheme key.
var themeSchemes = map[string]bool{
	"jove": true, "caldari": true, "gallente": true,
	"amarr": true, "triglavian": true, "edencom": true, "soe": true,
}

func validThemeScheme(s string) bool { return s == "" || themeSchemes[s] }

func validThemeTier(s string) bool { return s == "muted" || s == "minimalistic" }

func validThemeBackdrop(s string) bool {
	return s == "" || s == "muted" || s == "glow"
}

// parseThemePrefs decodes a stored theme, dropping any value the
// picker cannot produce, so a stale or hand-edited blob degrades
// to the default instead of painting something unknown.
func parseThemePrefs(blob string) themePrefs {
	var prefs themePrefs
	if err := json.Unmarshal([]byte(blob), &prefs); err != nil {
		return themePrefs{}
	}
	if !validThemeScheme(prefs.Scheme) {
		prefs.Scheme = ""
	}
	if prefs.Scheme == "" {
		prefs.Tier = ""
	} else if !validThemeTier(prefs.Tier) {
		prefs.Tier = "muted"
	}
	if !validThemeBackdrop(prefs.Backdrop) {
		prefs.Backdrop = ""
	}
	return prefs
}

// themePrefsFor reads an account's stored theme; stored is false
// when the account has never saved one, in which case a local
// choice in the browser is adopted on the first signed-in load
// (app.js's account theme sync) instead of being overridden.
func (app *Application) themePrefsFor(ctx context.Context, userID int64) (prefs themePrefs, stored bool) {
	blob, err := app.queries.GetWidgetConfig(ctx, db.GetWidgetConfigParams{UserID: userID, WidgetID: themeConfigID})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("theme: settings for user %d: %v", userID, err)
		}
		return themePrefs{}, false
	}
	return parseThemePrefs(blob), true
}

// storeThemePrefs saves an account's theme.
func (app *Application) storeThemePrefs(ctx context.Context, userID int64, prefs themePrefs) error {
	blob, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	return app.queries.UpsertWidgetConfig(ctx, db.UpsertWidgetConfigParams{
		UserID: userID, WidgetID: themeConfigID, Config: string(blob), UpdatedAt: time.Now().UTC(),
	})
}

// handleSettingsThemeSave stores the account theme (POST
// /settings/theme). The Interface picker posts the whole state on
// every pick, and the browser's first signed-in load posts the
// same shape to adopt a choice made while signed out. Like the
// home layout handler, XHR callers get a bare 200 and a plain form
// post bounces back to the Interface tab.
func (app *Application) handleSettingsThemeSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	prefs := themePrefs{
		Scheme:   r.FormValue("scheme"),
		Tier:     r.FormValue("tier"),
		Backdrop: r.FormValue("backdrop"),
	}
	if !validThemeScheme(prefs.Scheme) || !validThemeBackdrop(prefs.Backdrop) ||
		(prefs.Scheme != "" && !validThemeTier(prefs.Tier)) {
		http.Error(w, "unknown theme", http.StatusBadRequest)
		return
	}
	if prefs.Scheme == "" {
		prefs.Tier = ""
	}
	if err := app.storeThemePrefs(ctx, userID, prefs); err != nil {
		logging.Errorf("theme: save for user %d: %v", userID, err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/settings#interface", http.StatusSeeOther)
}
