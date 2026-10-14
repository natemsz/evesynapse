package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Stored public fit pages. The parts of a public fit page that cost
// work (module sections, the All-V simulation, the EFT text) are
// built once per version of the fit and stored (public_fit_pages):
// when the owner saves the fit public, and by the worker for any page
// missing or out of date. A page view reads the stored parts and
// never runs the fitting engine. Views are counted in memory and
// written by the worker in batches, so a visitor costs no write.
// ---------------------------------------------------------------------------

// publicFitPagesPerCycle bounds the worker's page builds per cycle.
const publicFitPagesPerCycle = 25

// publicFitStored is what a stored page holds.
type publicFitStored struct {
	Sections []fittingGroup
	Sim      *fitSimView
	EFT      string
}

// buildPublicFitStored builds a page's stored parts from the fit
// document. complete is false when a type name or the ship's data
// was still missing, so the page is built again later.
func (app *Application) buildPublicFitStored(ctx context.Context, doc *fitDoc) (publicFitStored, bool) {
	stored := publicFitStored{
		Sections: app.publicFitSections(ctx, doc),
		Sim:      app.buildFitSimView(ctx, doc, 0, "All V", 0),
		EFT:      app.formatEFT(ctx, doc),
	}
	complete := stored.Sim == nil || stored.Sim.DataNote == ""
	for _, g := range stored.Sections {
		for _, it := range g.Items {
			if strings.HasPrefix(it.Name, "Type #") {
				complete = false
			}
		}
	}
	return stored, complete
}

// publicFitSDEStamp names the static data a page is built from: the
// last import's time ("" before any import).
func (app *Application) publicFitSDEStamp(ctx context.Context) string {
	stamp, err := app.sdeMeta(ctx, "imported_at")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("public fit pages: SDE stamp: %v", err)
	}
	return stamp
}

// storePublicFitPage builds and stores one fit's page.
func (app *Application) storePublicFitPage(ctx context.Context, fitID int64, itemsJSON string, updatedAt time.Time, sdeStamp string) error {
	var doc fitDoc
	if err := json.Unmarshal([]byte(itemsJSON), &doc); err != nil {
		return err
	}
	stored, complete := app.buildPublicFitStored(ctx, &doc)
	body, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return app.queries.UpsertPublicFitPage(ctx, db.UpsertPublicFitPageParams{
		FitID: fitID, FitUpdatedAt: updatedAt, SdeStamp: sdeStamp, Complete: complete, Body: body,
	})
}

// storePublicFitPageNow builds a page right after its owner saved
// the fit public: the work lands on the owner's request, and the
// page is ready for its first visitor.
func (app *Application) storePublicFitPageNow(ctx context.Context, fitID int64) {
	row, err := app.queries.GetPublicFitting(ctx, fitID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("public fit %d: load for its page: %v", fitID, err)
		}
		return
	}
	if err := app.storePublicFitPage(ctx, fitID, row.ItemsJson, row.UpdatedAt, app.publicFitSDEStamp(ctx)); err != nil {
		logging.Errorf("public fit %d: store page: %v", fitID, err)
	}
}

// refreshPublicFitPages builds the pages that are missing or out of
// date, at most publicFitPagesPerCycle a cycle. Reports how many.
func (app *Application) refreshPublicFitPages(ctx context.Context) int {
	stamp := app.publicFitSDEStamp(ctx)
	rows, err := app.queries.ListPublicFitsNeedingPages(ctx, db.ListPublicFitsNeedingPagesParams{
		SdeStamp: stamp, Rlimit: publicFitPagesPerCycle,
	})
	if err != nil {
		if ctx.Err() == nil {
			logging.Errorf("worker: public fit pages: %v", err)
		}
		return 0
	}
	built := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		if err := app.storePublicFitPage(ctx, row.ID, row.ItemsJson, row.UpdatedAt, stamp); err != nil {
			logging.Errorf("worker: public fit %d page: %v", row.ID, err)
			continue
		}
		built++
	}
	if built > 0 {
		logging.Infof("worker: built %d public fit pages", built)
	}
	return built
}

// loadPublicFitStored reads a fit's stored page when it was built
// from this version of the fit; ok is false otherwise.
func (app *Application) loadPublicFitStored(ctx context.Context, fitID int64, updatedAt time.Time) (publicFitStored, bool) {
	var stored publicFitStored
	page, err := app.queries.GetPublicFitPage(ctx, fitID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("public fit %d: stored page: %v", fitID, err)
		}
		return stored, false
	}
	if !page.FitUpdatedAt.Equal(updatedAt) {
		return stored, false
	}
	if err := json.Unmarshal(page.Body, &stored); err != nil {
		logging.Errorf("public fit %d: stored page: %v", fitID, err)
		return stored, false
	}
	return stored, true
}

// fitViewCounter holds the views counted since the last flush.
type fitViewCounter struct {
	mu      sync.Mutex
	pending map[int64]int64
}

// count adds one view of fitID and returns the views not yet written.
func (c *fitViewCounter) count(fitID int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = make(map[int64]int64)
	}
	c.pending[fitID]++
	return c.pending[fitID]
}

// peek returns the views of fitID not yet written, counting none.
func (c *fitViewCounter) peek(fitID int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending[fitID]
}

// take empties the counter.
func (c *fitViewCounter) take() map[int64]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.pending
	c.pending = nil
	return out
}

// flushFitViews writes the views counted since the last flush, one
// statement per fit. A failed write puts its views back.
func (app *Application) flushFitViews(ctx context.Context) {
	for fitID, n := range app.fitViews.take() {
		if err := app.queries.AddFitViews(ctx, db.AddFitViewsParams{ViewCount: n, ID: fitID}); err != nil {
			if ctx.Err() == nil {
				logging.Errorf("worker: public fit %d views: %v", fitID, err)
			}
			app.fitViews.mu.Lock()
			if app.fitViews.pending == nil {
				app.fitViews.pending = make(map[int64]int64)
			}
			app.fitViews.pending[fitID] += n
			app.fitViews.mu.Unlock()
		}
	}
}
