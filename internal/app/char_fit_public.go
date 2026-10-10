package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Public fits: the fits anyone here has made public in the fitting
// tool, in the fit browser (fit_browser.go). Each opens in the fitting
// tool unsaved; saving it there makes a copy.
//
// A keeper of doctrines who comes here from one (corp_doctrines.go)
// can copy a public fit into it.
// ---------------------------------------------------------------------------

const (
	publicFitsPath = "/fittings/public/"
	// publicFitsRead is how many public fits, newest first, the page
	// works from, and publicFitsShown how many it draws at once.
	publicFitsRead  = 500
	publicFitsShown = 100
)

type publicFitsView struct {
	Browser *fitBrowserView
}

// publicFitBrowser sets up the browser for the public fits, as an
// address filters it, and loads the fits.
func (app *Application) publicFitBrowser(ctx context.Context, userID int64, chosen *opChoice, query url.Values) (*fitBrowser, []fitCard) {
	rows, err := app.queries.BrowsePublicFittings(ctx, db.BrowsePublicFittingsParams{RowLimit: publicFitsRead})
	if err != nil {
		logging.Errorf("public fits: %v", err)
	}
	cards := make([]fitCard, 0, len(rows))
	for _, row := range rows {
		var doc fitDoc
		_ = json.Unmarshal([]byte(row.ItemsJson), &doc)
		ship := row.ShipName
		if ship == "" {
			ship = app.typeNameOrID(ctx, row.ShipTypeID)
		}
		card := fitCard{
			ID: row.ID, Name: row.Name, Ship: ship, ShipTypeID: row.ShipTypeID, By: row.AuthorName, Mine: row.UserID == userID,
			OpenURL: "/fittings/?public=" + idString(row.ID) + "#fit-editor", AddField: "public", Updated: row.UpdatedAt, tags: doc.Tags,
		}
		if card.Mine {
			card.OpenURL = "/fittings/?local=" + idString(row.ID) + "#fit-editor"
		}
		cards = append(cards, card)
	}
	app.hullClasses(ctx, cards)

	fixed := url.Values{}
	if chosen != nil {
		fixed.Set("for", idString(chosen.ID))
	}
	return &fitBrowser{
		Path: publicFitsPath, Fixed: fixed, Q: strings.TrimSpace(query.Get("q")), Sort: query.Get("sort"),
		Sorts: []browseSort{
			{"new", "Newest", func(a, b *fitCard) bool { return a.Updated.After(b.Updated) }},
			sortFitName, sortFitShip,
		},
		Filters: []fitFilter{
			filterHull(strings.TrimSpace(query.Get("hull"))),
			filterTag(strings.TrimSpace(query.Get("tag"))),
			{Key: "by", Title: "By", Value: strings.TrimSpace(query.Get("by")), Most: 10, Of: func(c *fitCard) []string { return []string{c.By} }},
		},
	}, cards
}

func (app *Application) handlePublicFits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	chosen := app.doctrineChosenFor(ctx, userID, r.URL.Query().Get("for"))
	browser, cards := app.publicFitBrowser(ctx, userID, chosen, r.URL.Query())
	view := browser.run(cards)
	if len(view.Cards) > publicFitsShown {
		view.Cards, view.More = view.Cards[:publicFitsShown], true
	}
	view.For = chosen
	view.Placeholder = "Search fits and ships…"
	view.SuggestURL = "/fittings/public/suggest"
	if len(browser.Fixed) > 0 {
		view.SuggestURL += "?" + browser.Fixed.Encode()
	}
	view.Empty = `Nobody has made a fit public yet. To share one of yours, tick "Make public" on it in the fitting tool.`
	data.PublicFits = &publicFitsView{Browser: view}
	app.render(ctx, w, http.StatusOK, "fittings_public.html", data)
}

// handlePublicFitSuggest feeds the public fits search box
// (GET /fittings/public/suggest?q=).
func (app *Application) handlePublicFitSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeSuggestJSON(w, nil)
		return
	}
	browser, cards := app.publicFitBrowser(ctx, userID, app.doctrineChosenFor(ctx, userID, r.URL.Query().Get("for")), url.Values{})
	writeSuggestJSON(w, browser.suggestFits(cards, q))
}
