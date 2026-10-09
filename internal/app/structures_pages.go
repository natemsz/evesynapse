package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Player structure page (/structure/): the destination every
// resolved structure name now links to -- the last place kind to
// get one. There is no public ESI record for a structure (the
// lookup endpoint is authenticated-only and stays in the worker's
// resolution path), so the page renders what this instance already
// knows: the resolved name (structure_names, via the tiered
// resolution in structures.go) and the context corporation
// structure snapshots reported (structure_context: owning
// corporation, solar system, structure type). A first visit notes
// the want and shows the loading state, which live-fills through
// the same never-give-up poller as the organization pages once the
// worker lands the name. Nothing is fabricated: a structure nobody
// can name and no snapshot describes stays honestly pending, and a
// settled 'missing' answer says the name isn't available rather
// than inventing one. Renders read stored rows only.
// ---------------------------------------------------------------------------

// structurePageView is the /structure/ page body.
type structurePageView struct {
	StructureID   int64
	State         string // "loading" | "missing" | "ready"
	Name          string // resolved name, "" until resolution lands
	IconURL       string // structure type icon, "" when the type is unknown
	TypeID        int64
	TypeName      string
	OwnerCorpID   int64
	OwnerCorpName string
	SystemID      int64
	SystemName    string
	HasContext    bool // any of type/owner/system known
}

// handleStructurePage renders one player structure's local record.
func (app *Application) handleStructurePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	id, err := strconv.ParseInt(r.URL.Query().Get("structure"), 10, 64)
	if err != nil || !isStructureID(id) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data.Structure = app.loadStructureView(ctx, id)
	if data.Structure != nil && data.Structure.State == "loading" {
		app.notePageWant(ctx, pageWantStructure, id, 0)
	}
	app.render(ctx, w, http.StatusOK, "structure.html", data)
}

// handleStructureFragment re-renders just the structure record
// body from the local stores.
func (app *Application) handleStructureFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.URL.Query().Get("structure"), 10, 64)
	if err != nil || !isStructureID(id) {
		http.Error(w, "bad structure fragment request", http.StatusBadRequest)
		return
	}
	app.renderFragment(w, "structure.html", "structure-body", app.loadStructureView(ctx, id))
}

// loadStructureView builds the render model for a structure's
// record from the local stores, noting the resolution want so the
// worker fills the name in. Cache-only; shared by the page and
// its live-region fragment.
func (app *Application) loadStructureView(ctx context.Context, id int64) *structurePageView {
	view := &structurePageView{StructureID: id, State: "loading"}

	// Join the durable resolution queue (idempotent): viewing the
	// page is the strongest signal someone wants this name.
	app.noteStructureIDs(ctx, id)

	if name := app.resolvedStructureTitle(ctx, id); name != "" {
		view.State = "ready"
		view.Name = name
	} else if row, err := app.queries.GetStructureName(ctx, id); err == nil && row.State == esi.StructureMissing {
		// Every candidate character answered no: the name isn't
		// available to this instance (private, or gone). Context,
		// when a snapshot reported any, still renders below.
		view.State = "missing"
	}

	if sc, err := app.queries.GetStructureContext(ctx, id); err == nil {
		view.TypeID = sc.TypeID
		view.OwnerCorpID = sc.OwnerCorporationID
		view.SystemID = sc.SystemID
	} else if !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("structure page: read context for %d: %v", id, err)
	}
	if view.TypeID > 0 {
		view.TypeName = app.typeNameOrID(ctx, view.TypeID)
		view.IconURL = fmt.Sprintf("https://images.evetech.net/types/%d/icon?size=64", view.TypeID)
	}
	if view.OwnerCorpID > 0 {
		view.OwnerCorpName = app.corpDisplayName(ctx, view.OwnerCorpID)
	}
	if view.SystemID > 0 {
		if sys, err := app.queries.GetSDESystem(ctx, view.SystemID); err == nil {
			view.SystemName = sys.Name
		}
	}
	view.HasContext = view.TypeID > 0 || view.OwnerCorpID > 0 || view.SystemID > 0
	return view
}
