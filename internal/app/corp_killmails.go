package app

import (
	"net/http"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Corporation killmails: the corp's recent kills and losses. The
// recent list is a corp_* snapshot; the details behind it share
// the killmail_details store and the Killmails page rendering with
// the character view — only the kill/loss test changes (a victim
// in this corporation is a loss).
// ---------------------------------------------------------------------------

func (app *Application) handleCorpKillmails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		// This page renders killmails.html, which on its own maps
		// to the Character branch; the corp view belongs to the
		// Corporation branch of the top nav.
		Section: "corporation",
	}

	sel, ok, err := app.pickCorpPage(ctx, r)
	if err != nil {
		logging.Errorf("corp killmails: pick character: %v", err)
		data.Error = "Could not load killmail data; check the server log."
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}
	if !ok {
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}
	data.KillmailChars = sel.Links

	view := &killmailsView{
		CharacterName: sel.Active.Name,
		Title:         "Corporation Killmails",
		BasePath:      "/corporations/killmails/",
		Subject:       "this corporation",
		CorpID:        sel.Base.CorpID,
	}
	data.Killmails = view

	var refs []esi.KillmailRef
	state := app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpKillmails, &refs)
	switch {
	case state.Loaded:
		view.Loaded = true
	case state.Forbidden:
		view.RoleMissing = state.RoleMissing
	default:
		view.Warming = true
	}
	if !view.Loaded {
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}

	beforeTime, beforeID, cursored := parseKillmailCursor(r.URL.Query())
	if cursored {
		// A cursored page reads the board alone: the recent
		// list is a newest-window snapshot and has nothing
		// older to offer.
		refs = nil
	} else if len(refs) > maxKillmailsShown {
		refs = refs[:maxKillmailsShown]
	}

	// Viewing the page offers the viewing character and this
	// corporation to the history backfill, so the board's
	// coverage of both completes in the background.
	app.notePilotsEncountered([]int64{sel.Active.CharacterID})
	if sel.Base.CorpID > 0 {
		app.noteCorpsEncountered([]int64{sel.Base.CorpID})
	}

	viewer := killmailViewer{corporationID: sel.Base.CorpID}
	view.Rows, view.olderCursor = app.killmailUnionRows(ctx, viewer, refs, beforeTime, beforeID)
	app.shapeKillmails(ctx, r, view, viewer)

	app.render(ctx, w, http.StatusOK, "killmails.html", data)
}
