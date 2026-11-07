package app

// ---------------------------------------------------------------------------
// Live pilot intel: the pilot page reads a pilot's board live from
// zKillboard, the stats endpoint for the all-time counts and ships
// flown, the history endpoint for the recent kills and losses. A
// pilot no registered user has linked never gets a backfill (the
// gate in pilot_activity.go and corp_backfill.go), so this is the
// only way the page learns anything about a stranger, and the
// stored profile (when one exists) overrides only the counts and
// the ships, systems and hours it carries. It is a read, not a
// write: no kill is ingested, no profile is claimed, no coverage
// is marked. The fetch is bounded (one stats call, a few history
// pages, the last year only) and runs under a short timeout, so a
// slow zKillboard degrades the section to its empty state instead
// of hanging the page.
// ---------------------------------------------------------------------------

import (
	"context"
	"sort"
	"time"

	"evesynapse/internal/logging"
)

const (
	// pilotLiveMaxPages bounds the live read: three pages of the
	// history API, 200 killmails a page, newest first.
	pilotLiveMaxPages = 3
	// pilotLiveTimeout bounds the whole live read, so the page
	// never waits on zKillboard longer than this.
	pilotLiveTimeout = 10 * time.Second
)

// pilotLiveResult is a pilot's board read live from zKillboard:
// all-time counts from the stats endpoint, the recent kills and
// losses from the history endpoint priced with EveSynapse's own
// appraisal (killUnitPrices), never zKillboard's totalValue. A nil
// result means both reads failed and the section stays empty.
type pilotLiveResult struct {
	Kills, Losses, Solo int64
	ISK                 string
	Ships               []pilotCount
	Recent              []pilotKill
}

func (app *Application) fetchPilotZKill(ctx context.Context, id int64, names *killNames, now time.Time) *pilotLiveResult {
	ctx, cancel := context.WithTimeout(ctx, pilotLiveTimeout)
	defer cancel()
	stats, err := app.fetchZKillStats(ctx, zkillHistoryCharacter, id)
	if err != nil {
		logging.Warnf("pilot intel %d: live zkill stats: %v", id, err)
	}
	entries, _ := app.fetchZKillHistory(ctx, zkillHistoryCharacter, id, pilotLiveMaxPages, now.Add(-killDetailRetention))
	if stats == nil && len(entries) == 0 {
		return nil
	}
	values := app.priceZKillEntries(ctx, entries)
	r := &pilotLiveResult{}
	// The recent kills and losses, newest first, priced with our
	// own appraisal of the full fitting. Entries the pilot took
	// no part in are skipped; the history endpoint should not
	// return any, but the page reads only participations. The
	// window counts are a fallback for when the stats call fails.
	var iskKilled float64
	var kills, losses, solo int64
	flown := map[int64]int64{}
	shown := 0
	for _, e := range entries {
		lost := e.Victim.CharacterID == id
		var flew int64
		if !lost {
			for _, a := range e.Attackers {
				if a.CharacterID == id {
					flew = a.ShipTypeID
					break
				}
			}
			if flew == 0 {
				continue
			}
			iskKilled += values[e.KillmailID]
			kills++
			if e.ZKB.Solo {
				solo++
			}
			flown[flew]++
		} else {
			losses++
		}
		if shown >= 10 {
			continue
		}
		shown++
		sys := names.system(e.SolarSystemID)
		k := pilotKill{
			KillmailID: e.KillmailID,
			Lost:       lost,
			ShipTypeID: e.Victim.ShipTypeID,
			Ship:       names.typeName(e.Victim.ShipTypeID),
			System:     sys.Name,
			Ago:        ageText(now, e.KillmailTime),
		}
		k.SecText, k.SecTier = secRound(sys.Security)
		if flew > 0 {
			k.Flew = names.typeName(flew)
		}
		if v := values[e.KillmailID]; v > 0 {
			k.Value = iskShortGo(v)
		}
		r.Recent = append(r.Recent, k)
	}
	if iskKilled > 0 {
		r.ISK = iskShortGo(iskKilled)
	}
	if stats != nil {
		// The stats endpoint is the source for the all-time
		// counts and the ships flown; the window counts above
		// are only a fallback when it fails.
		r.Kills, r.Losses, r.Solo = stats.Kills, stats.Losses, stats.Solo
		var top int64
		if len(stats.Ships) > 0 {
			top = stats.Ships[0].Kills
		}
		for _, s := range stats.Ships {
			r.Ships = append(r.Ships, pilotCount{ID: s.ShipTypeID, Name: names.typeName(s.ShipTypeID), N: s.Kills, Pct: pctOf(s.Kills, top)})
		}
	} else {
		// The stats call failed: count the fetched window
		// instead of showing nothing, and read the ships flown
		// from it.
		r.Kills, r.Losses, r.Solo = kills, losses, solo
		type shipRow struct {
			id int64
			n  int64
		}
		rows := make([]shipRow, 0, len(flown))
		for id, n := range flown {
			rows = append(rows, shipRow{id, n})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].n != rows[j].n {
				return rows[i].n > rows[j].n
			}
			return rows[i].id < rows[j].id
		})
		var top int64
		if len(rows) > 0 {
			top = rows[0].n
		}
		for _, row := range rows {
			r.Ships = append(r.Ships, pilotCount{ID: row.id, Name: names.typeName(row.id), N: row.n, Pct: pctOf(row.n, top)})
		}
	}
	return r
}
