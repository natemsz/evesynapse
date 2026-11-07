package app

// ---------------------------------------------------------------------------
// Live zKillboard reads (killboard switch, schema 128): the killboards
// and My Killmails read zKillboard live instead of the local
// involvement table. The stats endpoint gives all-time counts and
// the top ship list; the history endpoint gives the recent kills
// and losses. Both are bounded (one stats call, a few history
// pages, 200 kills a page) and run under a 10s timeout; a failed
// fetch degrades the section to empty, never hangs the page.
//
// ISK is always our own appraisal: the fetched kills are priced
// with killUnitPrices, the same house price order the rest of the
// site uses. zKillboard's own ISK figures are never read or shown.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	goccyjson "github.com/goccy/go-json"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// zkillHistoryMaxPages bounds a board read: five pages of the
// history API, 200 killmails a page, newest first.
const zkillHistoryMaxPages = 5

// scopedHistoryKind maps a killboard scope kind to the zKillboard
// history endpoint's path segment for the entity.
func scopedHistoryKind(kind string) zkillHistoryKind {
	switch kind {
	case "corporation":
		return zkillHistoryCorporation
	case "alliance":
		return zkillHistoryAlliance
	default:
		return zkillHistoryCharacter
	}
}

// zkillEntryEntityPart says whether a fetched history entry is a
// kill or a loss for the entity: a kill when the entity attacked,
// a loss when the entity is the victim. Entries the entity took
// no part in are neither; the history endpoint should not return
// any, but the readers count only participations.
func zkillEntryEntityPart(e zkillHistoryEntry, kind string, id int64) (isKill, isLoss bool) {
	switch kind {
	case "corporation":
		if e.Victim.CorporationID == id {
			return false, true
		}
		for _, a := range e.Attackers {
			if a.CorporationID == id {
				return true, false
			}
		}
	case "alliance":
		if e.Victim.AllianceID == id {
			return false, true
		}
		for _, a := range e.Attackers {
			if a.AllianceID == id {
				return true, false
			}
		}
	default:
		if e.Victim.CharacterID == id {
			return false, true
		}
		for _, a := range e.Attackers {
			if a.CharacterID == id {
				return true, false
			}
		}
	}
	return false, false
}

// zkillShipCount is one ship's all-time kill count from the stats
// endpoint's topAllTime ship list.
type zkillShipCount struct {
	ShipTypeID int64
	Kills      int64
}

// zkillStats is a pilot, corporation or alliance's all-time board
// from the zKillboard stats endpoint.
type zkillStats struct {
	Kills         int64
	Losses        int64
	Solo          int64
	SoloLosses    int64
	Ships         []zkillShipCount
	TopIskKillIDs []int64
}

// fetchZKillStats reads one entity's all-time board from the
// zKillboard stats endpoint. A nil stats with a nil error means the
// endpoint answered with no board for the entity.
func (app *Application) fetchZKillStats(ctx context.Context, kind zkillHistoryKind, id int64) (*zkillStats, error) {
	client := &http.Client{Timeout: pilotLiveTimeout}
	url := fmt.Sprintf("%sstats/%s/%d/", app.cfg.ZKillAPIBaseURL(), kind, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zkill stats %s %d: %s", kind, id, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var wire struct {
		ShipsDestroyed int64 `json:"shipsDestroyed"`
		ShipsLost      int64 `json:"shipsLost"`
		SoloKills      int64 `json:"soloKills"`
		SoloLosses     int64 `json:"soloLosses"`
		TopAllTime     []struct {
			Type string `json:"type"`
			Data []struct {
				Kills      int64 `json:"kills"`
				ShipTypeID int64 `json:"shipTypeID"`
			} `json:"data"`
		} `json:"topAllTime"`
		TopIskKillIDs []int64 `json:"topIskKillIDs"`
	}
	if err := goccyjson.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	st := &zkillStats{
		Kills:         wire.ShipsDestroyed,
		Losses:        wire.ShipsLost,
		Solo:          wire.SoloKills,
		SoloLosses:    wire.SoloLosses,
		TopIskKillIDs: wire.TopIskKillIDs,
	}
	for _, list := range wire.TopAllTime {
		if list.Type != "ship" {
			continue
		}
		for _, d := range list.Data {
			if d.ShipTypeID == 0 {
				continue
			}
			st.Ships = append(st.Ships, zkillShipCount{ShipTypeID: d.ShipTypeID, Kills: d.Kills})
		}
	}
	sort.Slice(st.Ships, func(i, j int) bool { return st.Ships[i].Kills > st.Ships[j].Kills })
	return st, nil
}

// fetchZKillHistory reads an entity's recent kills and losses from
// the zKillboard history endpoint, newest first, at most maxPages
// pages of 200. A zero cutoff reads without a time bound. The
// second return is true when every available page was read, so the
// caller knows whether more history exists past the bound.
func (app *Application) fetchZKillHistory(ctx context.Context, kind zkillHistoryKind, id int64, maxPages int, cutoff time.Time) ([]zkillHistoryEntry, bool) {
	client := &http.Client{Timeout: pilotLiveTimeout}
	base := app.cfg.ZKillAPIBaseURL()
	var out []zkillHistoryEntry
	complete := true
	for page := 1; page <= maxPages; page++ {
		entries, err := zkillHistoryPage(ctx, client, base, kind, id, page)
		if err != nil {
			logging.Warnf("zkill live %s %d page %d: %v", kind, id, page, err)
			return out, false
		}
		stop := false
		for _, e := range entries {
			if e.KillmailID <= 0 || e.KillmailTime.IsZero() {
				continue
			}
			if !cutoff.IsZero() && e.KillmailTime.Before(cutoff) {
				stop = true
				break
			}
			out = append(out, e)
		}
		if stop || len(entries) < pilotBackfillPageSize {
			return out, true
		}
		if page == maxPages {
			complete = false
		}
	}
	return out, complete
}

// priceZKillEntries prices fetched kills with our own appraisal:
// the hull and every fitted, cargo and nested container line,
// destroyed and dropped. The result maps killmail id to the full
// fitted value; a kill with no priced line is absent.
func (app *Application) priceZKillEntries(ctx context.Context, entries []zkillHistoryEntry) map[int64]float64 {
	out := map[int64]float64{}
	if len(entries) == 0 {
		return out
	}
	types := map[int64]bool{}
	linesByKill := make(map[int64][]killLine, len(entries))
	for _, e := range entries {
		lines := killLines(e.killmailFull)
		linesByKill[e.KillmailID] = lines
		for _, l := range lines {
			types[l.typeID] = true
		}
	}
	price, err := app.killUnitPrices(ctx, types)
	if err != nil {
		logging.Warnf("zkill live prices: %v", err)
		return out
	}
	for id, lines := range linesByKill {
		total, _, _ := killValue(lines, price)
		if total > 0 {
			out[id] = total
		}
	}
	return out
}

// zkillEntryKillmail decodes a fetched history entry back into an
// ESI killmail, so the shared row shaper can read it.
func zkillEntryKillmail(e zkillHistoryEntry) (esi.Killmail, bool) {
	var km esi.Killmail
	if len(e.Raw) == 0 {
		return km, false
	}
	if err := goccyjson.Unmarshal(e.Raw, &km); err != nil {
		return km, false
	}
	return km, true
}

// rowFromZKill shapes a feed row for a fetched history entry, the
// same row the local feed builds from a stored kill. The value is
// our own appraisal of the entry, 0 when unpriced.
func (n *killNames) rowFromZKill(e zkillHistoryEntry, value float64, now time.Time) killRow {
	km := e.killmailFull
	sys := n.system(km.SolarSystemID)
	sec, tier := secRound(sys.Security)
	r := killRow{
		ID:           km.KillmailID,
		Time:         km.KillmailTime,
		ShipTypeID:   km.Victim.ShipTypeID,
		ShipName:     n.typeName(km.Victim.ShipTypeID),
		VictimID:     km.Victim.CharacterID,
		VictimName:   n.character(km.Victim.CharacterID),
		CorpID:       km.Victim.CorporationID,
		CorpName:     n.corp(km.Victim.CorporationID),
		AllianceID:   km.Victim.AllianceID,
		AllianceName: n.alliance(km.Victim.AllianceID),
		SystemID:     km.SolarSystemID,
		SystemName:   sys.Name,
		RegionID:     sys.RegionID,
		RegionName:   n.region(sys.RegionID),
		SecText:      sec,
		SecTier:      tier,
		When:         km.KillmailTime.UTC().Format("15:04"),
		Ago:          ageText(now, km.KillmailTime),
		Attackers:    len(km.Attackers),
		NPC:          e.ZKB.NPC,
		Solo:         e.ZKB.Solo,
		Awox:         e.ZKB.Awox,
	}
	if r.VictimID == 0 {
		r.VictimIsStructure = true
		r.VictimName = r.ShipName
	}
	for _, a := range km.Attackers {
		if a.FinalBlow {
			r.FinalID = a.CharacterID
			r.FinalName = n.character(a.CharacterID)
			r.FinalCorpID = a.CorporationID
			r.FinalCorpName = n.corp(a.CorporationID)
			r.FinalAllianceID = a.AllianceID
			r.FinalAllianceName = n.alliance(a.AllianceID)
			break
		}
	}
	if r.FinalID == 0 && len(km.Attackers) > 0 {
		a := km.Attackers[0]
		r.FinalID = a.CharacterID
		r.FinalName = n.character(a.CharacterID)
		r.FinalCorpID = a.CorporationID
		r.FinalCorpName = n.corp(a.CorporationID)
		r.FinalAllianceID = a.AllianceID
		r.FinalAllianceName = n.alliance(a.AllianceID)
	}
	if value > 0 {
		r.Value = formatISKShort(value)
		r.Big = value >= killBigValue
	}
	return r
}
