package app

// ---------------------------------------------------------------------------
// On-demand pilot intel: when a pilot's page holds no stored activity
// at all, the page asks zKillboard for their recent history live and
// reads the stats off the answer, without storing anything. A pilot no
// registered user has linked never gets a backfill (the gate in
// pilot_activity.go and corp_backfill.go), so this is the only way the
// page learns anything about a stranger. It is a read, not a write:
// no kill is ingested, no profile is claimed, no coverage is marked.
// The fetch is bounded (a few pages, the last year only) and runs
// under a short timeout, so a slow zKillboard degrades the section
// to its empty state instead of hanging the page.
// ---------------------------------------------------------------------------

import (
	"context"
	"net/http"
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

// liveKill is one fetched killmail reduced to the pilot's part in
// it: a loss when they are the victim, a kill when they attacked.
// flew is the hull they flew on a kill, 0 on a loss.
type liveKill struct {
	km   zkillHistoryEntry
	lost bool
	flew int64
}

// fillPilotIntelLive fetches the pilot's recent kill history from
// zKillboard and fills the view's activity from it: kills and
// losses, ISK destroyed, the ships flown, and the latest kills
// and losses. The values are EveSynapse's own appraisal of the
// destroyed hulls (killUnitPrices), never zKillboard's totalValue.
// It returns false when nothing was found or the fetch failed;
// the view is left untouched then, and the page keeps its empty
// state.
func (app *Application) fillPilotIntelLive(ctx context.Context, v *pilotIntelView, id int64, names *killNames, now time.Time) bool {
	ctx, cancel := context.WithTimeout(ctx, pilotLiveTimeout)
	defer cancel()
	client := &http.Client{Timeout: pilotLiveTimeout}
	base := app.cfg.ZKillAPIBaseURL()
	cutoff := now.Add(-killDetailRetention)

	var found []liveKill
	shipTypes := map[int64]bool{}
	for page := 1; page <= pilotLiveMaxPages; page++ {
		entries, err := zkillHistoryPage(ctx, client, base, zkillHistoryCharacter, id, page)
		if err != nil {
			logging.Warnf("pilot intel %d: live zkill: %v", id, err)
			return false
		}
		for _, e := range entries {
			if e.KillmailID <= 0 || e.KillmailTime.IsZero() {
				continue
			}
			if e.KillmailTime.Before(cutoff) {
				// Past the retention year: the history is
				// ordered newest first, so older pages
				// only go further back.
				return app.finishPilotIntelLive(ctx, v, found, shipTypes, names, now)
			}
			lk := liveKill{km: e}
			if e.Victim.CharacterID == id {
				lk.lost = true
			} else {
				for _, a := range e.Attackers {
					if a.CharacterID == id {
						lk.flew = a.ShipTypeID
						break
					}
				}
				if lk.flew == 0 {
					continue
				}
			}
			found = append(found, lk)
			shipTypes[e.Victim.ShipTypeID] = true
		}
		if len(entries) < pilotBackfillPageSize {
			break
		}
	}
	return app.finishPilotIntelLive(ctx, v, found, shipTypes, names, now)
}

// finishPilotIntelLive prices the fetched killmails' hulls with
// EveSynapse's own appraisal and fills the view from them.
func (app *Application) finishPilotIntelLive(ctx context.Context, v *pilotIntelView, found []liveKill, shipTypes map[int64]bool, names *killNames, now time.Time) bool {
	if len(found) == 0 {
		return false
	}
	price, err := app.killUnitPrices(ctx, shipTypes)
	if err != nil {
		logging.Warnf("pilot intel %d: live zkill prices: %v", v.CharacterID, err)
		price = map[int64]float64{}
	}
	var kills, losses, solo int64
	var iskKilled float64
	flown := map[int64]int64{}
	for _, lk := range found {
		hull := price[lk.km.Victim.ShipTypeID]
		if lk.lost {
			losses++
			continue
		}
		kills++
		if len(lk.km.Attackers) == 1 {
			solo++
		}
		iskKilled += hull
		if lk.flew > 0 {
			flown[lk.flew]++
		}
	}
	if kills+losses == 0 {
		return false
	}
	v.Kills, v.Losses, v.Solo = kills, losses, solo
	if iskKilled > 0 {
		v.ISK = iskShortGo(iskKilled)
	}
	// The ships flown, most flown first.
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
	for _, r := range rows {
		v.Ships = append(v.Ships, pilotCount{ID: r.id, Name: names.typeName(r.id), N: r.n, Pct: pctOf(r.n, top)})
	}
	// The latest kills and losses, newest first.
	for i, lk := range found {
		if i >= 10 {
			break
		}
		sys := names.system(lk.km.SolarSystemID)
		k := pilotKill{
			KillmailID: lk.km.KillmailID,
			Lost:       lk.lost,
			ShipTypeID: lk.km.Victim.ShipTypeID,
			Ship:       names.typeName(lk.km.Victim.ShipTypeID),
			System:     sys.Name,
			Ago:        ageText(now, lk.km.KillmailTime),
		}
		k.SecText, k.SecTier = secRound(sys.Security)
		if !lk.lost && lk.flew > 0 {
			k.Flew = names.typeName(lk.flew)
		}
		if hull := price[lk.km.Victim.ShipTypeID]; hull > 0 {
			k.Value = iskShortGo(hull)
		}
		v.Recent = append(v.Recent, k)
	}
	if len(v.Recent) > 0 {
		last := v.Recent[0]
		v.LastSeen = &last
	}
	v.LiveFromZKill = true
	return true
}
