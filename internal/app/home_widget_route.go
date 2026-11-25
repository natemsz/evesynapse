package app

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// newHomeShared starts one render's cache for an account: its
// characters, and their snapshots of the kinds the widgets drawn call
// for. The full home and the card routes all build it here.
func newHomeShared(ctx context.Context, app *Application, userID int64, chars []db.Character, needs []string) *homeShared {
	return &homeShared{
		app: app, userID: userID, chars: chars, now: time.Now(),
		bundles: app.loadCharSnaps(ctx, userID, chars, needs),
		// The Briefing's market lines, the market widget's health
		// line and the watchlist widget all read the same two stored
		// tables; the render fetches each at most once, lazily, so
		// every reader sees one snapshot of them.
		market: &homeMarketShared{app: app, userID: userID},
	}
}

// handleHomeWidget serves one card of the signed-in account's home
// (GET /home/widget/{key}): at its stored size, at w and h when the
// widget declares that size, or with view=expand at its largest
// form. It reads only the snapshots that widget needs, and it never
// moves the Briefing's "since you last looked" anchor: only a full
// home render does. A widget that is not on the account's home is not
// served; a quiet one answers 204, so the caller drops the card.
func (app *Application) handleHomeWidget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Error(w, "Not signed in", http.StatusUnauthorized)
		return
	}
	key := chi.URLParam(r, "key")
	def, ok := widgetDefFor(key)
	if !ok {
		http.NotFound(w, r)
		return
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home widget: list characters for user %d: %v", userID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	raw, err := app.queries.GetUserHomeLayout(ctx, userID)
	if err != nil {
		raw = ""
	}
	var item homeLayoutItem
	found := false
	for _, it := range app.effectiveHomeLayout(ctx, userID, raw) {
		if it.ID == def.ID {
			item, found = it, true
			break
		}
	}
	if !found || len(chars) == 0 {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if q.Get("view") == "expand" {
		big := def.largest()
		item.W, item.H = big.W, big.H
	} else if wv, werr := strconv.Atoi(q.Get("w")); werr == nil {
		if hv, herr := strconv.Atoi(q.Get("h")); herr == nil && def.Allows(wSize{wv, hv}) {
			item.W, item.H = wv, hv
		}
	}

	shared := newHomeShared(ctx, app, userID, chars, def.Needs)
	ts, err := homeTemplateSet()
	if err != nil {
		logging.Errorf("home widget: parse templates: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	card, shown := app.buildHomeWidget(ctx, ts, def, item, shared, q.Get("customize") == "1", viewerChars(chars))
	w.Header().Set("Cache-Control", "no-store")
	if !shown {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var buf bytes.Buffer
	if err := ts.ExecuteTemplate(&buf, "w-card", card); err != nil {
		logging.Errorf("home widget: render %s: %v", def.ID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
