package app

// ---------------------------------------------------------------------------
// Watched systems: the one account-level list of solar systems the
// gate camp alerts follow (notifyPrefs.CampSystems, stored with the
// notification settings). The list is edited in one modal, opened
// from the notification settings and from the Intel watch home
// widget; the map's star is a later surface on the same list. The
// modal's saves go through the same prefs load and store as every
// other notification setting, and names resolve through the same
// SDE lookup the settings box used.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"evesynapse/internal/logging"
)

// watchedSystemRow is one watched system in the modal: its id (the
// remove form's value) and its name.
type watchedSystemRow struct {
	ID   int64
	Name string
}

// watchedSystemsView is the modal's model: the current list in
// stored order, whether it is at the cap, and where a save returns
// to ("/" from the home widget, the settings page from Settings).
type watchedSystemsView struct {
	Systems []watchedSystemRow
	Full    bool
	Return  string
}

// watchedSystemsViewFor builds the modal's model from stored prefs.
func (app *Application) watchedSystemsViewFor(ctx context.Context, prefs notifyPrefs, returnTo string) *watchedSystemsView {
	view := &watchedSystemsView{Return: returnTo, Full: len(prefs.CampSystems) >= campSystemsMax}
	for _, id := range prefs.CampSystems {
		view.Systems = append(view.Systems, watchedSystemRow{ID: id, Name: app.campSystemName(ctx, id)})
	}
	return view
}

// handleWatchedSystemsSave applies one edit from the watched
// systems modal: a remove (a system id) or an add (a system name,
// resolved through the SDE like the settings box resolved it).
// Only CampSystems changes; the rest of the account's notification
// settings ride through untouched. An unknown name, a duplicate and
// a full list are reported back, never silently dropped.
func (app *Application) handleWatchedSystemsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	ret := settingsPath
	if err := r.ParseForm(); err == nil {
		if v := r.Form.Get("return"); v == "/" || v == settingsPath {
			ret = v
		}
	}
	if userID == 0 {
		http.Redirect(w, r, ret, http.StatusSeeOther)
		return
	}
	prefs := app.notifyPrefsFor(ctx, userID)
	changed := false
	msg := ""
	if raw := r.Form.Get("remove"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && prefs.watchesCampSystem(id) {
			name := app.campSystemName(ctx, id)
			kept := make([]int64, 0, len(prefs.CampSystems)-1)
			for _, watched := range prefs.CampSystems {
				if watched != id {
					kept = append(kept, watched)
				}
			}
			prefs.CampSystems = kept
			changed = true
			msg = fmt.Sprintf("%s is no longer watched.", name)
		}
	}
	if name := strings.TrimSpace(r.Form.Get("add_name")); name != "" {
		row, err := app.queries.GetSDESystemByName(ctx, name)
		switch {
		case err != nil || row.SystemID <= 0:
			msg = fmt.Sprintf("Unknown system: %s.", name)
		case prefs.watchesCampSystem(row.SystemID):
			msg = fmt.Sprintf("%s is already watched.", row.Name)
		case len(prefs.CampSystems) >= campSystemsMax:
			msg = fmt.Sprintf("The watched list is full at %d systems. Remove one to add another.", campSystemsMax)
		default:
			prefs.CampSystems = append(prefs.CampSystems, row.SystemID)
			changed = true
			msg = fmt.Sprintf("%s added to your watched systems.", row.Name)
		}
	}
	if changed {
		if err := app.storeNotifyPrefs(ctx, userID, prefs); err != nil {
			logging.Errorf("watched systems: save for user %d: %v", userID, err)
			app.flash(ctx, "The watched systems could not be saved; check the server log.")
			http.Redirect(w, r, ret, http.StatusSeeOther)
			return
		}
	}
	if msg != "" {
		app.flash(ctx, msg)
	}
	http.Redirect(w, r, ret, http.StatusSeeOther)
}
