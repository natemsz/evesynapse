package app

// ---------------------------------------------------------------------------
// The one time killboard statistics rebuild. The daily aggregates
// were folded kill by kill since schema 131; kills the fold reached
// after the retention prune had taken their attacker rows lost their
// ships flown, and millions of older kills were never folded at all.
// Once the attacker restore (killboard_restore.go) has read every
// missing row back, the rebuild counts every public kill again from
// complete rows, a chunk of killmail ids at a time, into the
// _rebuild tables (schema 146), then swaps them in for the live
// ones in one short transaction.
//
// While it runs the fold writes feed rows only and leaves kills in
// kill_stats_pending; a chunk skips queued kills, so each kill is
// counted exactly once, by the rebuild or by the fold after the
// swap. Progress is stored after every chunk, so a restart resumes
// where it stopped. After the swap the retention prune runs again.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// killStatsRebuildChunk is how many killmail ids one rebuild
	// transaction counts.
	killStatsRebuildChunk = 10000
	// killStatsRebuildTimeout is the statement timeout inside a
	// rebuild transaction; the server default suits page reads.
	killStatsRebuildTimeout = "120s"
	// killStatsLockKey is the advisory lock folds hold shared while
	// they decide to count a kill.
	killStatsLockKey int64 = 0x6b696c6c73746174 // "killstat"
)

// killStatsRebuilt reports whether the one time rebuild has
// swapped in. The retention prune waits for it.
func (app *Application) killStatsRebuilt(ctx context.Context) bool {
	st, err := app.queries.GetKillStatsRebuildState(ctx)
	return err == nil && st.DoneAt.Valid
}

// rebuildKillStats runs the rebuild to the swap, resuming from its
// stored cursor. It returns nil at once when the rebuild is done.
func (app *Application) rebuildKillStats(ctx context.Context) error {
	st, err := app.queries.GetKillStatsRebuildState(ctx)
	if err != nil {
		return fmt.Errorf("read rebuild state: %w", err)
	}
	if st.DoneAt.Valid {
		return nil
	}
	app.killStatsRebuilding.Store(true)
	defer app.killStatsRebuilding.Store(false)
	// Wait out any fold that read the flag before it was raised:
	// folds hold the lock shared while they count.
	if _, err := app.pool.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", killStatsLockKey); err != nil {
		return fmt.Errorf("wait for folds: %w", err)
	}

	cursor := st.Cursor
	if !st.StartedAt.Valid {
		if _, err := app.pool.Exec(ctx, "TRUNCATE kill_entity_daily_rebuild, kill_entity_daily_ships_rebuild, kill_entity_daily_systems_rebuild"); err != nil {
			return fmt.Errorf("clear rebuild tables: %w", err)
		}
		if err := app.queries.StartKillStatsRebuild(ctx, time.Now().UTC()); err != nil {
			return fmt.Errorf("start rebuild: %w", err)
		}
		cursor = 0
		logging.Infof("killboard stats rebuild: started")
	} else {
		logging.Infof("killboard stats rebuild: resuming after kill %d", cursor)
	}

	started := time.Now()
	chunks := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		through, err := app.queries.KillStatsRebuildChunkEnd(ctx, db.KillStatsRebuildChunkEndParams{AfterID: cursor, MaxRows: killStatsRebuildChunk})
		if err != nil {
			return fmt.Errorf("find chunk after %d: %w", cursor, err)
		}
		if through == 0 {
			break
		}
		if err := app.rebuildKillStatsChunk(ctx, cursor, through); err != nil {
			return fmt.Errorf("count kills %d to %d: %w", cursor+1, through, err)
		}
		cursor = through
		chunks++
		if chunks%50 == 0 {
			logging.Infof("killboard stats rebuild: counted through kill %d (%d chunks, %s)", cursor, chunks, time.Since(started).Round(time.Second))
		}
	}
	if err := app.swapKillStats(ctx); err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	logging.Infof("killboard stats rebuild: done, %d chunks in %s; the queued kills fold from here", chunks, time.Since(started).Round(time.Second))
	return nil
}

// rebuildKillStatsChunk counts one chunk into the _rebuild tables
// and stores the cursor, in one transaction.
func (app *Application) rebuildKillStatsChunk(ctx context.Context, after, through int64) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+killStatsRebuildTimeout+"'"); err != nil {
		return err
	}
	q := db.New(tx)
	if err := q.RebuildKillEntityDailyChunk(ctx, db.RebuildKillEntityDailyChunkParams{AfterID: after, ThroughID: through}); err != nil {
		return fmt.Errorf("daily: %w", err)
	}
	if err := q.RebuildKillEntityDailyShipsChunk(ctx, db.RebuildKillEntityDailyShipsChunkParams{AfterID: after, ThroughID: through}); err != nil {
		return fmt.Errorf("ships: %w", err)
	}
	if err := q.RebuildKillEntityDailySystemsChunk(ctx, db.RebuildKillEntityDailySystemsChunkParams{AfterID: after, ThroughID: through}); err != nil {
		return fmt.Errorf("systems: %w", err)
	}
	if err := q.SetKillStatsRebuildCursor(ctx, through); err != nil {
		return fmt.Errorf("cursor: %w", err)
	}
	return tx.Commit(ctx)
}

// swapKillStats puts the rebuilt tables in place of the live ones
// and marks the rebuild done, in one transaction: readers see the
// old aggregates until the commit and the new ones after it.
func (app *Application) swapKillStats(ctx context.Context) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, stmt := range []string{
		"SET LOCAL statement_timeout = '" + killStatsRebuildTimeout + "'",
		"LOCK TABLE kill_entity_daily, kill_entity_daily_ships, kill_entity_daily_systems IN ACCESS EXCLUSIVE MODE",
		"DROP TABLE kill_entity_daily, kill_entity_daily_ships, kill_entity_daily_systems",
		"ALTER TABLE kill_entity_daily_rebuild RENAME TO kill_entity_daily",
		"ALTER TABLE kill_entity_daily_ships_rebuild RENAME TO kill_entity_daily_ships",
		"ALTER TABLE kill_entity_daily_systems_rebuild RENAME TO kill_entity_daily_systems",
		"ALTER TABLE kill_entity_daily RENAME CONSTRAINT kill_entity_daily_rebuild_pkey TO kill_entity_daily_pkey",
		"ALTER TABLE kill_entity_daily_ships RENAME CONSTRAINT kill_entity_daily_ships_rebuild_pkey TO kill_entity_daily_ships_pkey",
		"ALTER TABLE kill_entity_daily_systems RENAME CONSTRAINT kill_entity_daily_systems_rebuild_pkey TO kill_entity_daily_systems_pkey",
		"UPDATE kill_stats_rebuild_state SET done_at = now() WHERE id = 1",
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return tx.Commit(ctx)
}
