package app

// ---------------------------------------------------------------------------
// Corporation killboard coverage (schema 126): the pilot
// backfill's twin, minus the aggregates. A corporation met on its
// public profile or on a signed-in killmail page is backfilled
// once from zKillboard's history API: up to ten pages of its most
// recent kills and losses, each ingested into the kills store,
// public, so the corporation's board feed and killmail lists show
// every participation. Corporations keep no activity profile, so
// the pass folds nothing: the coverage marker (one row per
// corporation, set when a pass completes) is its whole record,
// and a failed pass leaves the corporation unmarked, so a later
// visit offers it again.
//
// The worker mirrors the pilot backfill's bounds exactly: a
// fixed-size queue, drop-and-log when full, the same page cap and
// the same pace between history requests.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// corpBackfillQueueChan returns the corp backfill queue, making
// it on first use.
func (app *Application) corpBackfillQueueChan() chan int64 {
	app.corpBackfillMu.Lock()
	defer app.corpBackfillMu.Unlock()
	if app.corpBackfillCh == nil {
		app.corpBackfillCh = make(chan int64, pilotBackfillQueueSize)
		app.corpBackfillQueued = map[int64]bool{}
	}
	return app.corpBackfillCh
}

// noteCorpsEncountered offers corporations met on a profile or a
// killmail page to the backfill worker. It never blocks the page:
// a full queue drops the offer and logs it, and the corporation's
// next visit offers it again.
func (app *Application) noteCorpsEncountered(ids []int64) {
	if !app.cfg.PilotBackfillEnabled() || len(ids) == 0 {
		return
	}
	ch := app.corpBackfillQueueChan()
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		app.corpBackfillMu.Lock()
		if !app.corpBackfillQueued[id] {
			select {
			case ch <- id:
				app.corpBackfillQueued[id] = true
			default:
				logging.Warnf("corp coverage: backfill queue full; corporation %d waits for a later visit", id)
			}
		}
		app.corpBackfillMu.Unlock()
	}
}

// runCorpBackfill is the corp backfill worker: one corporation at
// a time, at the history API's pace, until ctx ends.
func (app *Application) runCorpBackfill(ctx context.Context) {
	if !app.cfg.PilotBackfillEnabled() {
		return
	}
	ch := app.corpBackfillQueueChan()
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-ch:
			app.backfillCorp(ctx, client, id)
			app.corpBackfillMu.Lock()
			delete(app.corpBackfillQueued, id)
			app.corpBackfillMu.Unlock()
		}
	}
}

// backfillCorp ingests a corporation's recent kill history from
// zKillboard into the kills store, once. A corporation already
// covered is left alone; a failed pass leaves it unmarked, so a
// later visit offers it again.
func (app *Application) backfillCorp(ctx context.Context, client *http.Client, id int64) {
	if _, err := app.queries.GetCorpKillCoverage(ctx, id); err == nil {
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		logging.Warnf("corp coverage: read coverage %d: %v", id, err)
		return
	}
	base := app.cfg.ZKillAPIBaseURL()
	seen := 0
	for page := 1; page <= pilotBackfillMaxPages; page++ {
		if !killfeedSleepCtx(ctx, pilotBackfillPace) {
			return
		}
		entries, err := zkillHistoryPage(ctx, client, base, zkillHistoryCorporation, id, page)
		if err != nil {
			logging.Warnf("corp coverage: backfill corporation %d, page %d: %v", id, page, err)
			return
		}
		for _, e := range entries {
			if e.KillmailID <= 0 || e.KillmailTime.IsZero() || len(e.Raw) == 0 {
				continue
			}
			seen++
			if _, err := app.ingestKill(ctx, e.Raw, e.ZKB.Hash, &e.ZKB, "zkill", true); err != nil {
				logging.Warnf("corp coverage: ingest history kill %d for corporation %d: %v", e.KillmailID, id, err)
			}
		}
		if len(entries) < pilotBackfillPageSize {
			break
		}
	}
	if err := app.queries.MarkCorpKillCoverage(ctx, db.MarkCorpKillCoverageParams{
		CorporationID: id, BackfilledAt: time.Now().UTC(),
	}); err != nil {
		logging.Warnf("corp coverage: mark corporation %d covered: %v", id, err)
		return
	}
	logging.Infof("corp coverage: backfilled corporation %d from %d kills and losses", id, seen)
}
