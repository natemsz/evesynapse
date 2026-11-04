package app

import (
	"context"
	"net/http"
	"strconv"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Blueprint pins: the production lines an account keeps. A pin is
// set from the Blueprints page (/industry/), one toggle on each
// blueprint row, and stored per account (blueprint_pins, schema
// 118) — the same set whichever character's library set it. The
// home Production Lines widget reads the pins; nothing else does.
// ---------------------------------------------------------------------------

// blueprintPinSet loads the account's pinned blueprint type ids.
func (app *Application) blueprintPinSet(ctx context.Context, userID int64) map[int64]bool {
	pins := map[int64]bool{}
	if userID == 0 {
		return pins
	}
	rows, err := app.queries.ListBlueprintPins(ctx, userID)
	if err != nil {
		logging.Errorf("blueprint pins: list for user %d: %v", userID, err)
		return pins
	}
	for _, row := range rows {
		pins[row.BlueprintTypeID] = true
	}
	return pins
}

// handleBlueprintPin sets or clears one pin. The form carries the
// blueprint type id, the desired end state ("pinned" is "1" to
// pin, "0" to unpin) and the character whose library is on screen,
// so the redirect lands back on that library. Like the briefing
// dismissal: a scripted caller gets a bare 204, a browser the
// page again.
func (app *Application) handleBlueprintPin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := "/industry/"
	if err := r.ParseForm(); err == nil && userID != 0 {
		if r.FormValue("character") == "all" {
			back = "/industry/?character=all"
		} else if ch, err := strconv.ParseInt(r.FormValue("character"), 10, 64); err == nil && ch > 0 {
			back = "/industry/?character=" + strconv.FormatInt(ch, 10)
		}
		if bp, err := strconv.ParseInt(r.FormValue("blueprint"), 10, 64); err == nil && bp > 0 {
			var err error
			if r.FormValue("pinned") == "1" {
				err = app.queries.PinBlueprint(ctx, db.PinBlueprintParams{UserID: userID, BlueprintTypeID: bp})
			} else {
				err = app.queries.UnpinBlueprint(ctx, db.UnpinBlueprintParams{UserID: userID, BlueprintTypeID: bp})
			}
			if err != nil {
				logging.Errorf("blueprint pins: change for user %d: %v", userID, err)
			}
		}
	}
	if r.Header.Get("X-Requested-With") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
