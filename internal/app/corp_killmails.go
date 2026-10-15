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
		Title:         sel.Base.CorpTitle + " — Killmails",
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

	if len(refs) > maxKillmailsShown {
		refs = refs[:maxKillmailsShown]
	}

	// Values read the price guide wherever the app holds it
	// (see handleKillmails); with none anywhere, the view notes
	// the durable guide-price want so the worker fills it in.
	prices := app.valuationPrices(ctx)
	if prices == nil {
		app.notePageWant(ctx, pageWantGuidePrices, 1, 0)
	}
	viewer := killmailViewer{corporationID: sel.Base.CorpID}
	placeMemo := make(map[int64]placeRef)
	for _, ref := range refs {
		view.Rows = append(view.Rows, app.killmailRow(ctx, viewer, ref, prices, placeMemo))
	}

	app.render(ctx, w, http.StatusOK, "killmails.html", data)
}
