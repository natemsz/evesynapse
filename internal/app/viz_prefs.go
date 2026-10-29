package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Visualization preferences: one account's chart display switches,
// stored as a JSON object in users.viz_prefs. The price history
// switches default on; book_depth defaults off. Later chart slices
// add their keys to the same object.
// ---------------------------------------------------------------------------

type vizPrefs struct {
	MAChannel   bool `json:"ma_channel"`
	Wicks       bool `json:"wicks"`
	VolumeStrip bool `json:"volume_strip"`
	BookDepth   bool `json:"book_depth"`
	SparkHover  bool `json:"spark_hover"`
}

func defaultVizPrefs() vizPrefs {
	return vizPrefs{MAChannel: true, Wicks: true, VolumeStrip: true, SparkHover: true}
}

// parseVizPrefs reads a stored blob over the defaults: a switch
// is off only when the blob explicitly says so. Unknown keys are
// ignored and unparseable content falls back to the defaults, so
// a corrupt row can never blank a chart.
func parseVizPrefs(blob string) vizPrefs {
	prefs := defaultVizPrefs()
	if strings.TrimSpace(blob) == "" {
		return prefs
	}
	var raw struct {
		MAChannel   *bool `json:"ma_channel"`
		Wicks       *bool `json:"wicks"`
		VolumeStrip *bool `json:"volume_strip"`
		BookDepth   *bool `json:"book_depth"`
		SparkHover  *bool `json:"spark_hover"`
	}
	if err := json.Unmarshal([]byte(blob), &raw); err != nil {
		return defaultVizPrefs()
	}
	if raw.MAChannel != nil {
		prefs.MAChannel = *raw.MAChannel
	}
	if raw.Wicks != nil {
		prefs.Wicks = *raw.Wicks
	}
	if raw.VolumeStrip != nil {
		prefs.VolumeStrip = *raw.VolumeStrip
	}
	if raw.BookDepth != nil {
		prefs.BookDepth = *raw.BookDepth
	}
	if raw.SparkHover != nil {
		prefs.SparkHover = *raw.SparkHover
	}
	return prefs
}

// vizPrefsFor reads an account's stored visualization switches.
func (app *Application) vizPrefsFor(ctx context.Context, userID int64) vizPrefs {
	blob, err := app.queries.GetUserVizPrefs(ctx, userID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("viz: prefs for user %d: %v", userID, err)
		}
		return defaultVizPrefs()
	}
	return parseVizPrefs(blob)
}

// storeVizPrefs saves an account's visualization switches.
func (app *Application) storeVizPrefs(ctx context.Context, userID int64, prefs vizPrefs) error {
	blob, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	return app.queries.SetUserVizPrefs(ctx, db.SetUserVizPrefsParams{
		VizPrefs: string(blob), ID: userID,
	})
}

// handleSettingsVisualizationsSave stores the chart switches
// (POST /settings/visualizations). The settings modal posts one
// field per switch: present and "1" means on, absent means off.
// A plain post returns to the page the form sits on; demo
// sessions never reach the write (the demo write gate turns the
// POST away first).
func (app *Application) handleSettingsVisualizationsSave(w http.ResponseWriter, r *http.Request) {
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
	prefs := vizPrefs{
		MAChannel:   r.FormValue("ma_channel") == "1",
		Wicks:       r.FormValue("wicks") == "1",
		VolumeStrip: r.FormValue("volume_strip") == "1",
		BookDepth:   r.FormValue("book_depth") == "1",
		SparkHover:  r.FormValue("spark_hover") == "1",
	}
	if err := app.storeVizPrefs(ctx, userID, prefs); err != nil {
		logging.Errorf("viz: save prefs for user %d: %v", userID, err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, vizReturnPath(r), http.StatusSeeOther)
}

// vizReturnPath is where a settings post lands: the Referer's
// path and query when the Referer is a local path, else /market/.
func vizReturnPath(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return "/market/"
	}
	u, err := url.Parse(ref)
	if err != nil || u.Path == "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/market/"
	}
	if u.Host != "" && u.Host != r.Host {
		return "/market/"
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}
