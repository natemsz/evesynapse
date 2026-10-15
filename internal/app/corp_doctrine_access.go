package app

import (
	"context"
	"encoding/json"
	"strconv"

	"evesynapse/internal/app/module"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// What the other modules ask of a corporation's doctrines: which doctrine
// a keeper is choosing fits for (public fits), and a doctrine fit read as
// a fit document (the fitting tool). The access rule, a corporation's
// doctrine permission, is core's.

// doctrineChosenFor reads the "for" of an address: the doctrine a
// keeper is choosing fits for. Nil unless the account keeps it.
func (app *Application) doctrineChosenFor(ctx context.Context, userID int64, raw string) *opChoice {
	id, _ := strconv.ParseInt(raw, 10, 64)
	if id <= 0 {
		return nil
	}
	d, err := app.queries.GetDoctrine(ctx, id)
	if err != nil {
		return nil
	}
	st, err := app.standingFor(ctx, userID)
	if err != nil || !app.corpPermits(ctx, st, d.CorporationID, permDoctrines) {
		return nil
	}
	return &opChoice{ID: d.ID, Name: d.Name}
}

// doctrineFitDoc reads a doctrine's fit for an account with a
// character in the doctrine's corporation.
func (app *Application) doctrineFitDoc(ctx context.Context, userID, fitID int64) (*fitview.FitDoc, bool) {
	row, err := app.queries.GetDoctrineFit(ctx, fitID)
	if err != nil {
		return nil, false
	}
	inside := false
	for _, corp := range app.UserCorps(ctx, userID) {
		inside = inside || corp == row.CorporationID
	}
	var doc fitview.FitDoc
	if !inside || json.Unmarshal([]byte(row.ItemsJson), &doc) != nil {
		return nil, false
	}
	doc.Name = row.Name
	return &doc, true
}

// doctrineFitView, doctrineURL and doctrineFitViews: a doctrine's fits, read by the ops
// page (core) and the Doctrines module alike.
type doctrineFitView = module.DoctrineFitView

func doctrineURL(id int64) string { return module.DoctrineURL(id) }

func (app *Application) doctrineFitViews(ctx context.Context, doctrineIDs []int64) map[int64][]doctrineFitView {
	out := map[int64][]doctrineFitView{}
	if len(doctrineIDs) == 0 {
		return out
	}
	fits, err := app.queries.ListDoctrineFits(ctx, doctrineIDs)
	if err != nil {
		logging.Errorf("doctrines: fits: %v", err)
		return out
	}
	for _, fit := range fits {
		out[fit.DoctrineID] = append(out[fit.DoctrineID], doctrineFitView{
			ID: fit.ID, Name: fit.Name, Ship: app.typeNameOrID(ctx, fit.ShipTypeID), ShipTypeID: fit.ShipTypeID,
			Role: fit.FleetRole, Note: fit.Note,
		})
	}
	return out
}

func (app *Application) DoctrineFitViews(ctx context.Context, doctrineIDs []int64) map[int64][]module.DoctrineFitView {
	return app.doctrineFitViews(ctx, doctrineIDs)
}
