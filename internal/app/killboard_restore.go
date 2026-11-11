package app

// ---------------------------------------------------------------------------
// Attacker restore: kills whose attacker rows were pruned before the
// stats fold counted their ships are read back from ESI by id and
// hash (killmails never change) and their attacker rows stored
// again. A kill inside the two-year retention also gets its stored
// killmail back. The stats rebuild then counts every kill from
// complete rows.
//
// One background loop, newest kill first, below a keyset cursor so
// a kill no source can serve is passed over rather than retried in
// a loop. ESI is the bulk source: reads go outside the shared GET
// cache (FetchRawOnce), a few at a time, and back off while ESI's
// error budget is low. A kill ESI cannot serve is asked of
// zKillboard by id, one at a time at its requested pace. Once a
// pass finds nothing left the loop rechecks every few hours.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	goccyjson "github.com/goccy/go-json"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	// killRestoreBatch is how many kills one pass lists.
	killRestoreBatch = 400
	// killRestoreReaders is how many ESI reads run at once.
	killRestoreReaders = 8
	// killRestoreRecheck is how long the loop waits once nothing is
	// left to restore before it looks again.
	killRestoreRecheck = 6 * time.Hour
	// killRestoreBackoff is the pause while ESI's error budget is low
	// or the error limit was hit.
	killRestoreBackoff = time.Minute
)

// killPruneEnabled gates the daily retention prune. It stays off
// until the stats rebuild has counted the restored attacker rows.
const killPruneEnabled = false

// runKillAttackerRestore is the restore loop. It returns when ctx
// ends.
func (app *Application) runKillAttackerRestore(ctx context.Context) {
	zkill := &http.Client{Timeout: 30 * time.Second}
	cursor := int64(math.MaxInt64)
	var restored, missing int64
	started := time.Now()
	for ctx.Err() == nil {
		if app.esi.ErrorBudgetLow() {
			if !killfeedSleepCtx(ctx, killRestoreBackoff) {
				return
			}
			continue
		}
		rows, err := app.queries.ListKillsMissingAttackers(ctx, db.ListKillsMissingAttackersParams{BeforeID: cursor, MaxRows: killRestoreBatch})
		if err != nil {
			if ctx.Err() == nil {
				logging.Errorf("killboard restore: list kills missing attackers: %v", err)
			}
			if !killfeedSleepCtx(ctx, killRestoreBackoff) {
				return
			}
			continue
		}
		if len(rows) == 0 {
			if restored+missing > 0 {
				logging.Infof("killboard restore: done, %d kills restored, %d ESI could not serve, in %s",
					restored, missing, time.Since(started).Round(time.Minute))
			}
			restored, missing, cursor, started = 0, 0, math.MaxInt64, time.Now()
			if !killfeedSleepCtx(ctx, killRestoreRecheck) {
				return
			}
			continue
		}
		ok, failed, limited := app.restoreKillAttackers(ctx, rows)
		restored += ok
		for _, r := range failed {
			if err := app.restoreKillFromZKill(ctx, zkill, r); err != nil {
				missing++
				if ctx.Err() == nil {
					logging.Warnf("killboard restore: kill %d: neither ESI nor zKillboard served it: %v", r.KillmailID, err)
				}
			} else {
				restored++
			}
		}
		cursor = rows[len(rows)-1].KillmailID
		logging.Infof("killboard restore: %d kills restored so far (%d unservable), now at kill %d", restored, missing, cursor)
		if limited && !killfeedSleepCtx(ctx, killRestoreBackoff) {
			return
		}
	}
}

// restoreKillAttackers reads one batch back from ESI and stores
// each kill's attacker rows, and its killmail while it is inside
// retention. failed holds the kills ESI could not serve, for the
// zKillboard fallback. limited reports that ESI's error limit cut
// the batch short; the cursor still moves past it, so those kills
// wait for the next full pass.
func (app *Application) restoreKillAttackers(ctx context.Context, rows []db.ListKillsMissingAttackersRow) (restored int64, failed []db.ListKillsMissingAttackersRow, limited bool) {
	var ok atomic.Int64
	var hitLimit atomic.Bool
	var badMu sync.Mutex
	work := make(chan db.ListKillsMissingAttackersRow)
	var wg sync.WaitGroup
	for range killRestoreReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range work {
				if hitLimit.Load() || ctx.Err() != nil {
					continue
				}
				err := app.restoreOneKill(ctx, r)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, esi.ErrErrorLimit):
					hitLimit.Store(true)
				case ctx.Err() == nil:
					badMu.Lock()
					failed = append(failed, r)
					badMu.Unlock()
				}
			}
		}()
	}
	for _, r := range rows {
		work <- r
	}
	close(work)
	wg.Wait()
	return ok.Load(), failed, hitLimit.Load()
}

// restoreOneKill reads one killmail from ESI and stores it.
func (app *Application) restoreOneKill(ctx context.Context, r db.ListKillsMissingAttackersRow) error {
	body, _, err := app.esi.FetchRawOnce(ctx, fmt.Sprintf("/killmails/%d/%s/", r.KillmailID, r.Hash))
	if err != nil {
		return err
	}
	var km killmailFull
	if err := goccyjson.Unmarshal(body, &km); err != nil {
		return fmt.Errorf("decode killmail: %w", err)
	}
	return app.storeRestoredKill(ctx, r, km, body)
}

// restoreKillFromZKill asks zKillboard for one kill by id, at the
// pace its API requests.
func (app *Application) restoreKillFromZKill(ctx context.Context, client *http.Client, r db.ListKillsMissingAttackersRow) error {
	if !killfeedSleepCtx(ctx, pilotBackfillPace) {
		return ctx.Err()
	}
	entries, err := zkillHistoryPage(ctx, client, app.cfg.ZKillAPIBaseURL(), zkillHistoryKill, r.KillmailID, 1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.KillmailID == r.KillmailID {
			return app.storeRestoredKill(ctx, r, e.killmailFull, e.Raw)
		}
	}
	return errors.New("zKillboard has no such kill")
}

// storeRestoredKill stores a read-back kill's attacker rows, and
// its killmail while the kill is inside retention.
func (app *Application) storeRestoredKill(ctx context.Context, r db.ListKillsMissingAttackersRow, km killmailFull, body []byte) error {
	if km.KillmailID != r.KillmailID {
		return fmt.Errorf("ESI answered killmail %d", km.KillmailID)
	}
	if ap := killAttackersParams(km); len(ap.Seqs) > 0 {
		if err := app.queries.InsertKillAttackers(ctx, ap); err != nil {
			return fmt.Errorf("store attackers: %w", err)
		}
	}
	if !r.KillmailTime.Before(time.Now().UTC().Add(-killDetailRetention)) {
		if err := app.queries.InsertKillDetail(ctx, db.InsertKillDetailParams{
			KillmailID: r.KillmailID, Payload: compressKillDetail(body), StoredAt: time.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("store detail: %w", err)
		}
	}
	return nil
}
