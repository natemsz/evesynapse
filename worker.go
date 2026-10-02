package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// runWorker is the background ESI refresh scheduler. Every minute it
// walks every linked character, makes sure their access token is
// usable (refreshing near-expiry tokens), and re-fetches any cached
// snapshot whose cached_until has passed. If ESI answers with its
// error-limit status (420/429) the cycle stops and the next tick
// retries — CCP asks clients to back off instead of hammering.
//
// Logging is deliberately quiet: one summary line per cycle only when
// something was refreshed or failed, plus a 10-minute heartbeat so
// liveness is visible without spamming the console.
func (app *application) runWorker(ctx context.Context) {
	log.Printf("worker: started")

	cycle := time.NewTicker(time.Minute)
	heartbeat := time.NewTicker(10 * time.Minute)
	defer cycle.Stop()
	defer heartbeat.Stop()

	// First pass right away so a cold start doesn't wait a minute for
	// fresh data.
	app.refreshCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker: stopped")
			return
		case <-heartbeat.C:
			log.Printf("worker: alive")
		case <-cycle.C:
			app.refreshCycle(ctx)
		}
	}
}

// refreshCycle performs one pass over all characters. See runWorker.
func (app *application) refreshCycle(ctx context.Context) {
	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		log.Printf("worker: list characters: %v", err)
		return
	}

	var refreshed, failed int
	limited := false

	for _, ch := range characters {
		if ctx.Err() != nil {
			return
		}

		// Ensure the token is usable before touching snapshots; a
		// revoked refresh token means this character needs a fresh
		// login, and fetching would only fail three more times.
		if _, err := app.validAccessToken(ctx, ch); err != nil {
			log.Printf("worker: token for character %d unusable: %v", ch.CharacterID, err)
			failed++
			continue
		}

		for _, kind := range []string{snapSkills, snapSkillqueue, snapWallet} {
			snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
			switch {
			case serr == nil && snapshotFresh(snap):
				continue // still inside ESI's cache window
			case serr != nil && !errors.Is(serr, sql.ErrNoRows):
				log.Printf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
			}

			if _, err := app.fetchAndStoreSnapshot(ctx, ch, kind); err != nil {
				failed++
				if errors.Is(err, errESIErrorLimit) {
					log.Printf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
					limited = true
				} else {
					log.Printf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
				}
				break // don't keep pushing this character this cycle
			}
			refreshed++
		}
		if limited {
			break
		}
	}

	if refreshed > 0 || failed > 0 {
		log.Printf("worker: cycle done: %d snapshot(s) refreshed, %d failure(s)", refreshed, failed)
	}
}
