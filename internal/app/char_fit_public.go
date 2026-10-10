package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Public fits: the fits anyone here has made public in the fitting
// tool, on one page, found by name, ship or tag. Each opens in the
// fitting tool unsaved; saving it there makes a copy.
//
// A keeper of doctrines who comes here from one (corp_doctrines.go)
// can copy a public fit into it.
// ---------------------------------------------------------------------------

const (
	publicFitsPath  = "/fittings/public/"
	publicFitsShown = 100
)

type publicFitRow struct {
	ID         int64
	Name       string
	Ship       string
	ShipTypeID int64
	Tags       []string
	By         string
	Mine       bool
	OpenURL    string
}

type publicFitsView struct {
	Q, Tag string
	Rows   []publicFitRow
	More   bool // more match than are shown
	// For is the doctrine a keeper is choosing fits for, when the page
	// was opened from one.
	For   *opChoice
	Roles []string
	Self  string // this page's address with its filters, to come back to
}

func (app *Application) handlePublicFits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	query := r.URL.Query()
	view := &publicFitsView{Q: strings.TrimSpace(query.Get("q")), Tag: strings.TrimSpace(query.Get("tag")), Roles: opFleetRoles}
	data.PublicFits = view
	if raw := query.Get("for"); raw != "" {
		view.For = app.doctrineChosenFor(ctx, userID, raw)
	}
	view.Self = browseAddress(publicFitsPath, view.For, map[string]string{"q": view.Q, "tag": view.Tag})

	rows, err := app.queries.BrowsePublicFittings(ctx, db.BrowsePublicFittingsParams{Q: view.Q, Tag: view.Tag, RowLimit: publicFitsShown + 1})
	if err != nil {
		logging.Errorf("public fits: %v", err)
		data.Error = "Could not load the public fits; check the server log."
	}
	if len(rows) > publicFitsShown {
		rows, view.More = rows[:publicFitsShown], true
	}
	for _, row := range rows {
		var doc fitDoc
		_ = json.Unmarshal([]byte(row.ItemsJson), &doc)
		ship := row.ShipName
		if ship == "" {
			ship = app.typeNameOrID(ctx, row.ShipTypeID)
		}
		fit := publicFitRow{
			ID: row.ID, Name: row.Name, Ship: ship, ShipTypeID: row.ShipTypeID, Tags: doc.Tags, By: row.AuthorName,
			Mine: row.UserID == userID, OpenURL: fmt.Sprintf("/fittings/?public=%d#fit-editor", row.ID),
		}
		if fit.Mine {
			fit.OpenURL = fmt.Sprintf("/fittings/?local=%d#fit-editor", row.ID)
		}
		view.Rows = append(view.Rows, fit)
	}
	app.render(ctx, w, http.StatusOK, "fittings_public.html", data)
}
