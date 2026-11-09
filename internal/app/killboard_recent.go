package app

// The recent kills in-memory ring: holds the 100 most recent public
// kills streamed in by the killfeed (killfeed.go) in RAM and serves
// the /intel/kills/ front page directly without querying Postgres.
// Scoped feeds (?character=, ?corporation=, ?system=) and older
// pages past the ring window fall back to SQL. Gated by LOW_RAM_MODE=1.

import (
	"context"
	"database/sql"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// recentKillsCapacity is the maximum number of public kills kept resident.
const recentKillsCapacity = 100

type recentKillsState struct {
	mu     sync.RWMutex
	kills  []db.Kill
	loaded bool
}

// primeRecentKills seeds the in-memory recent kills ring from Postgres.
func (app *Application) primeRecentKills(ctx context.Context) {
	if app.cfg.lowRAMMode {
		return
	}
	kills, err := app.queries.ListKillsRecent(ctx, db.ListKillsRecentParams{
		BeforeTime: time.Now().Add(time.Hour).UTC(),
		BeforeID:   1 << 62,
		MinValue:   0,
		SoloOnly:   false,
		MaxRows:    recentKillsCapacity,
	})
	if err != nil {
		logging.Warnf("killfeed: prime recent kills: %v", err)
		return
	}
	r := &app.recentKills
	r.mu.Lock()
	r.kills = kills
	r.loaded = true
	r.mu.Unlock()
}

// noteRecentKill inserts or updates one public kill in the resident
// ring buffer in reverse-chronological order.
func (app *Application) noteRecentKill(k db.Kill) {
	if app.cfg.lowRAMMode || !k.Public {
		return
	}
	r := &app.recentKills
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loaded = true
	for i := range r.kills {
		if r.kills[i].KillmailID == k.KillmailID {
			r.kills[i] = k
			return
		}
	}
	inserted := false
	for i := range r.kills {
		if k.KillmailTime.After(r.kills[i].KillmailTime) ||
			(k.KillmailTime.Equal(r.kills[i].KillmailTime) && k.KillmailID > r.kills[i].KillmailID) {
			r.kills = append(r.kills[:i], append([]db.Kill{k}, r.kills[i:]...)...)
			inserted = true
			break
		}
	}
	if !inserted && len(r.kills) < recentKillsCapacity {
		r.kills = append(r.kills, k)
	}
	if len(r.kills) > recentKillsCapacity {
		r.kills = r.kills[:recentKillsCapacity]
	}
}

// noteRecentKillPrice updates the appraisal price fields of a kill
// currently held in the recent kills ring.
func (app *Application) noteRecentKillPrice(killmailID int64, valueOurs, valueDropped float64, cov float64, pricedAt time.Time) {
	if app.cfg.lowRAMMode {
		return
	}
	r := &app.recentKills
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.kills {
		if r.kills[i].KillmailID == killmailID {
			r.kills[i].ValueOurs = sql.NullFloat64{Float64: valueOurs, Valid: true}
			r.kills[i].ValueDroppedOurs = sql.NullFloat64{Float64: valueDropped, Valid: true}
			r.kills[i].ValueCoverage = sql.NullFloat64{Float64: cov, Valid: true}
			r.kills[i].PricedAt = sql.NullTime{Time: pricedAt, Valid: true}
			return
		}
	}
}

// getRecentKills attempts to serve recent kills from the resident ring buffer.
func (app *Application) getRecentKills(ctx context.Context, bt time.Time, bid int64, minValue float64, solo bool, maxRows int) ([]db.Kill, bool) {
	if app.cfg.lowRAMMode {
		return nil, false
	}
	r := &app.recentKills
	r.mu.RLock()
	loaded := r.loaded
	r.mu.RUnlock()
	if !loaded {
		app.primeRecentKills(ctx)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.loaded || len(r.kills) == 0 {
		return nil, false
	}

	var out []db.Kill
	for _, k := range r.kills {
		if !k.KillmailTime.Before(bt) && !(k.KillmailTime.Equal(bt) && k.KillmailID < bid) {
			continue
		}
		if minValue > 0 && (!k.ValueOurs.Valid || k.ValueOurs.Float64 < minValue) {
			continue
		}
		if solo && !k.Solo {
			continue
		}
		out = append(out, k)
		if len(out) >= maxRows {
			break
		}
	}

	if len(out) >= maxRows {
		return out, true
	}
	// A short page is only complete when nothing narrows it: with
	// a value floor or solo-only, matching kills beyond the ring
	// still exist, so the short page must fall through to SQL or
	// the Big kills and Solo filters read empty on a fresh page.
	if minValue <= 0 && !solo && bt.After(time.Now().Add(30*time.Minute)) && bid >= (1<<62-1) {
		return out, true
	}
	return nil, false
}
