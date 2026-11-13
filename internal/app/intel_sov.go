package app

// ---------------------------------------------------------------------------
// The sovereignty tracker's ingest. Two writers, both run by the place
// stats pass and both skipped when their ESI payload has not changed:
//
//   logSovChanges      before the holder set is replaced, one statement
//                      compares the stored holders with the incoming
//                      ones and logs every system whose holder changed
//                      (sov_changes, schema 154)
//   refreshSovCampaigns mirrors GET /sovereignty/campaigns/ into
//                      sov_campaigns, replaced whole
//
// The tracker page (intel_sov_page.go) only reads these tables and
// system_sovereignty, a bounded read per block.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// placeStatsKindSovCampaigns is the place_stats_meta row for the
// contests mirror.
const placeStatsKindSovCampaigns = "sov_campaigns"

// logSovChanges logs, inside the pass's transaction, every system
// whose holder in incoming differs from the stored set: a new holder,
// a lost claim, or a claim on a system nobody held. A system's change
// is dated by the new holder's claimed_since when ESI gives one, else
// by now. An empty stored set logs nothing. It returns how many
// changes it logged.
func (app *Application) logSovChanges(ctx context.Context, tx pgx.Tx, incoming esi.SovMap, now time.Time) (int64, error) {
	var stored int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM system_sovereignty`).Scan(&stored); err != nil {
		return 0, err
	}
	if stored == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(incoming))
	alliances := make([]int64, 0, len(incoming))
	corps := make([]int64, 0, len(incoming))
	factions := make([]int64, 0, len(incoming))
	since := make([]*time.Time, 0, len(incoming))
	seen := make(map[int64]bool, len(incoming))
	for _, s := range incoming {
		if s.SystemID <= 0 || seen[s.SystemID] {
			continue
		}
		seen[s.SystemID] = true
		ids = append(ids, s.SystemID)
		alliances = append(alliances, s.AllianceID)
		corps = append(corps, s.CorporationID)
		factions = append(factions, s.FactionID)
		if s.ClaimedSince.IsZero() {
			since = append(since, nil)
		} else {
			t := s.ClaimedSince.UTC()
			since = append(since, &t)
		}
	}
	// A system the payload no longer names has no holder now; one the
	// stored set lacks had none before. Holders compare by alliance,
	// then by faction: a corporation change inside one alliance is
	// not a change of sovereignty.
	tag, err := tx.Exec(ctx, `
		WITH n AS (
			SELECT * FROM unnest($1::bigint[], $2::bigint[], $3::bigint[], $4::bigint[], $5::timestamptz[])
				AS t(system_id, alliance_id, corporation_id, faction_id, claimed_since)
		)
		INSERT INTO sov_changes (system_id, changed_at, old_alliance_id, old_corporation_id, old_faction_id,
			new_alliance_id, new_corporation_id, new_faction_id)
		SELECT COALESCE(n.system_id, o.system_id),
		       CASE WHEN n.alliance_id > 0 AND n.claimed_since IS NOT NULL AND n.claimed_since <= $6 THEN n.claimed_since ELSE $6 END,
		       COALESCE(o.alliance_id, 0), COALESCE(o.corporation_id, 0), COALESCE(o.faction_id, 0),
		       COALESCE(n.alliance_id, 0), COALESCE(n.corporation_id, 0), COALESCE(n.faction_id, 0)
		FROM n FULL JOIN system_sovereignty o ON o.system_id = n.system_id
		WHERE COALESCE(o.alliance_id, 0) <> COALESCE(n.alliance_id, 0)
		   OR COALESCE(o.faction_id, 0) <> COALESCE(n.faction_id, 0)`,
		ids, alliances, corps, factions, since, now.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// refreshSovCampaigns mirrors the stored campaigns snapshot into
// sov_campaigns, only when the snapshot changed since the last write.
// Reports whether it stored.
func (app *Application) refreshSovCampaigns(ctx context.Context) bool {
	snap, err := app.queries.GetGlobalSnapshot(ctx, esi.GlobalSovCampaigns)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("worker: sov campaigns: read snapshot: %v", err)
		}
		return false
	}
	if meta, err := app.queries.GetPlaceStatsMeta(ctx, placeStatsKindSovCampaigns); err == nil {
		if snap.Etag != "" && meta.SourceEtag == snap.Etag {
			return false
		}
		if snap.Etag == "" && !snap.FetchedAt.After(meta.FetchedAt) {
			return false
		}
	}
	var campaigns esi.SovCampaigns
	if err := json.Unmarshal([]byte(snap.Payload), &campaigns); err != nil {
		logging.Errorf("worker: sov campaigns: decode: %v", err)
		return false
	}
	var (
		ids, systems, constellations, structures, defenders []int64
		types                                               []string
		defScores, attScores                                []float64
		starts                                              []time.Time
	)
	seen := make(map[int64]bool, len(campaigns))
	for _, c := range campaigns {
		if c.CampaignID <= 0 || c.SystemID <= 0 || seen[c.CampaignID] {
			continue
		}
		seen[c.CampaignID] = true
		ids = append(ids, c.CampaignID)
		systems = append(systems, c.SystemID)
		constellations = append(constellations, c.ConstellationID)
		structures = append(structures, c.StructureID)
		defenders = append(defenders, c.DefenderID)
		types = append(types, c.EventType)
		defScores = append(defScores, c.DefenderScore)
		attScores = append(attScores, c.AttackersScore)
		starts = append(starts, c.StartTime.UTC())
	}
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		logging.Errorf("worker: sov campaigns: begin tx: %v", err)
		return false
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM sov_campaigns`); err != nil {
		logging.Errorf("worker: sov campaigns: clear: %v", err)
		return false
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO sov_campaigns (campaign_id, system_id, constellation_id, structure_id, event_type,
				defender_id, defender_score, attackers_score, start_time, updated_at)
			SELECT unnest($1::bigint[]), unnest($2::bigint[]), unnest($3::bigint[]), unnest($4::bigint[]),
			       unnest($5::text[]), unnest($6::bigint[]), unnest($7::double precision[]),
			       unnest($8::double precision[]), unnest($9::timestamptz[]), $10::timestamptz`,
			ids, systems, constellations, structures, types, defenders, defScores, attScores, starts, time.Now().UTC()); err != nil {
			logging.Errorf("worker: sov campaigns: store: %v", err)
			return false
		}
	}
	if err := app.queries.WithTx(tx).UpsertPlaceStatsMeta(ctx, dbPlaceMeta(placeStatsKindSovCampaigns, snap)); err != nil {
		logging.Errorf("worker: sov campaigns: store meta: %v", err)
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		logging.Errorf("worker: sov campaigns: commit: %v", err)
		return false
	}
	return true
}

// dbPlaceMeta is the place_stats_meta row recording which snapshot a
// distilled table holds.
func dbPlaceMeta(kind string, snap db.GlobalSnapshot) db.UpsertPlaceStatsMetaParams {
	return db.UpsertPlaceStatsMetaParams{
		Kind: kind, FetchedAt: snap.FetchedAt.UTC(), CachedUntil: snap.CachedUntil.UTC(), SourceEtag: snap.Etag,
	}
}
