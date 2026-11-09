package app

// ---------------------------------------------------------------------------
// Killboard statistics, precomputed at ingest (schema 131): every
// public kill folds once into per entity, per day aggregates
// (kills, solo kills, losses, ISK, biggest kill, ships flown and
// lost, systems) for its victim and attacker characters,
// corporations and alliances. The killboard and pilot pages read
// those stored rows only; the global headline is refreshed by the
// worker pass into its own stored row and top lists. No render
// aggregates the kills table, and no render fetches kills: new
// kills arrive on the stream follower and the backfill queues,
// both in the background.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

const killStatsSweepBatch = 200

type killEntityRef struct {
	kind string
	id   int64
}

func killValueOf(k db.Kill) float64 {
	if k.ValueOurs.Valid {
		return k.ValueOurs.Float64
	}
	if k.ValueZkill.Valid {
		return k.ValueZkill.Float64
	}
	return 0
}

// foldKillStats folds one stored public kill into the daily
// entity aggregates, exactly once per entity: the applied marker
// is written in the same transaction as the aggregates, so the
// ingest fold, the pricing pass and the maintenance sweep can
// never count a kill twice.
func (app *Application) foldKillStats(ctx context.Context, killmailID int64) error {
	k, err := app.queries.GetKill(ctx, killmailID)
	if err != nil {
		return err
	}
	inv, err := app.queries.ListKillInvolvement(ctx, killmailID)
	if err != nil {
		return err
	}
	ships, err := app.queries.ListKillAttackerShips(ctx, killmailID)
	if err != nil {
		return err
	}
	value := killValueOf(k)
	day := time.Date(k.KillmailTime.Year(), k.KillmailTime.Month(), k.KillmailTime.Day(), 0, 0, 0, 0, time.UTC)
	victim := map[string]int64{"character": k.VictimCharacterID, "corporation": k.VictimCorporationID, "alliance": k.VictimAllianceID}

	// One combined delta per entity: an entity can sit on both
	// sides of one kill (a corporation that loses a ship to its
	// own members), and both sides count.
	type delta struct {
		p       db.ApplyKillEntityDailyParams
		systems bool
	}
	deltas := map[killEntityRef]*delta{}
	get := func(e killEntityRef) *delta {
		d, ok := deltas[e]
		if !ok {
			d = &delta{p: db.ApplyKillEntityDailyParams{Day: day, Kind: e.kind, EntityID: e.id}}
			deltas[e] = d
		}
		return d
	}
	if !k.Npc {
		seen := map[killEntityRef]bool{}
		for _, a := range inv {
			for _, e := range []killEntityRef{{"character", a.CharacterID}, {"corporation", a.CorporationID}, {"alliance", a.AllianceID}} {
				if e.id <= 0 || e.id == victim[e.kind] || seen[e] {
					continue
				}
				seen[e] = true
				d := get(e)
				d.p.Kills = 1
				d.p.IskKilled = value
				d.p.BiggestKillmailID = killmailID
				d.p.BiggestValue = value
				if k.Solo {
					d.p.Solo = 1
				}
				d.systems = k.SystemID > 0
			}
		}
	}
	for kind, id := range victim {
		if id <= 0 {
			continue
		}
		d := get(killEntityRef{kind, id})
		d.p.Losses = 1
		d.p.IskLost = value
	}
	if len(deltas) == 0 {
		return nil
	}

	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	folded := map[killEntityRef]bool{}
	feedValue := 0.0
	if k.ValueOurs.Valid {
		feedValue = k.ValueOurs.Float64
	}
	for e, d := range deltas {
		// The feed row lands whatever the aggregates' marker
		// says: kills folded before the feed existed gain their
		// rows here without counting twice in the aggregates.
		if err := q.InsertKillEntityFeed(ctx, db.InsertKillEntityFeedParams{
			Kind: e.kind, EntityID: e.id, KillmailTime: k.KillmailTime, KillmailID: killmailID, Solo: k.Solo, Value: feedValue,
		}); err != nil {
			return err
		}
		// The daily aggregates count public kills only: a
		// private kill gets its feed rows (its owner's pages
		// read them) and nothing else until it turns public.
		if !k.Public {
			continue
		}
		if _, err := q.MarkKillEntityApplied(ctx, db.MarkKillEntityAppliedParams{
			KillmailID: killmailID, Kind: e.kind, EntityID: e.id, KillmailTime: k.KillmailTime,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return err
		}
		folded[e] = true
		if err := q.ApplyKillEntityDaily(ctx, d.p); err != nil {
			return err
		}
		if d.systems {
			if err := q.ApplyKillEntityDailySystem(ctx, db.ApplyKillEntityDailySystemParams{Day: day, Kind: e.kind, EntityID: e.id, SystemID: k.SystemID}); err != nil {
				return err
			}
		}
	}
	if !k.Npc {
		type shipKey struct {
			e    killEntityRef
			ship int64
		}
		seenShip := map[shipKey]bool{}
		for _, a := range ships {
			if a.ShipTypeID <= 0 {
				continue
			}
			for _, e := range []killEntityRef{{"character", a.CharacterID}, {"corporation", a.CorporationID}, {"alliance", a.AllianceID}} {
				key := shipKey{e, a.ShipTypeID}
				if !folded[e] || e.id == victim[e.kind] || seenShip[key] {
					continue
				}
				seenShip[key] = true
				if err := q.ApplyKillEntityDailyShip(ctx, db.ApplyKillEntityDailyShipParams{Day: day, Kind: e.kind, EntityID: e.id, ShipTypeID: a.ShipTypeID, Flown: 1}); err != nil {
					return err
				}
			}
		}
	}
	if k.ShipTypeID > 0 {
		for kind, id := range victim {
			e := killEntityRef{kind, id}
			if !folded[e] {
				continue
			}
			if err := q.ApplyKillEntityDailyShip(ctx, db.ApplyKillEntityDailyShipParams{Day: day, Kind: kind, EntityID: id, ShipTypeID: k.ShipTypeID, Lost: 1}); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// backfillKillStats is the maintenance sweep: public kills stored
// before the daily aggregates existed (or whose fold failed) are
// folded here, in the background, never at render.
func (app *Application) backfillKillStats(ctx context.Context) {
	ids, err := app.queries.ListUnfoldedKills(ctx, killStatsSweepBatch)
	if err != nil {
		logging.Errorf("killboard stats: list unfolded kills: %v", err)
		return
	}
	for _, id := range ids {
		if err := app.foldKillStats(ctx, id); err != nil {
			logging.Warnf("killboard stats: fold kill %d: %v", id, err)
		}
	}
	if len(ids) > 0 {
		logging.Infof("killboard stats: folded %d stored kills", len(ids))
	}
}

// backfillMissingKillFeeds is the maintenance sweep for kills
// stored before the entity feed existed: the fold writes their
// feed rows (the aggregates' markers keep the stats counted
// once), in the background, never at render.
func (app *Application) backfillMissingKillFeeds(ctx context.Context) {
	ids, err := app.queries.ListKillsMissingFeed(ctx, killStatsSweepBatch)
	if err != nil {
		logging.Errorf("killboard stats: list kills missing feed: %v", err)
		return
	}
	for _, id := range ids {
		if err := app.foldKillStats(ctx, id); err != nil {
			logging.Warnf("killboard stats: fold kill %d for feed: %v", id, err)
		}
	}
	if len(ids) > 0 {
		logging.Infof("killboard stats: fed %d stored kills", len(ids))
	}
}

// refreshKillboardHeadline recomputes the board headline and its
// top lists from the last day of kills, in the worker pass. The
// render path reads only the stored rows this writes.
func (app *Application) refreshKillboardHeadline(ctx context.Context) {
	now := time.Now().UTC()
	day := now.Add(-24 * time.Hour)
	st, err := app.queries.GetKillStats(ctx, db.GetKillStatsParams{HourAgo: now.Add(-time.Hour), DayAgo: day})
	if err != nil {
		logging.Errorf("killboard stats: headline stats: %v", err)
		return
	}
	var biggest int64
	if k, err := app.queries.GetBiggestKill(ctx, day); err == nil {
		biggest = k.KillmailID
	}
	if err := app.queries.UpsertKillboardHeadline(ctx, db.UpsertKillboardHeadlineParams{
		KillsHour: st.KillsHour, KillsDay: st.KillsDay, IskDay: st.IskDay, BiggestKillmailID: biggest, ComputedAt: now,
	}); err != nil {
		logging.Errorf("killboard stats: store headline: %v", err)
		return
	}
	store := func(kind string, ids []int64, kills []int64, isk []float64) {
		if _, err := app.queries.ReplaceKillboardTop(ctx, db.ReplaceKillboardTopParams{Kind: kind, EntityIds: ids, KillsList: kills, IskList: isk}); err != nil {
			logging.Errorf("killboard stats: store top %s: %v", kind, err)
		}
	}
	if rows, err := app.queries.TopKillerAlliances(ctx, db.TopKillerAlliancesParams{Since: day, MaxRows: 8}); err == nil {
		ids, ks, vs := make([]int64, 0, len(rows)), make([]int64, 0, len(rows)), make([]float64, 0, len(rows))
		for _, r := range rows {
			ids, ks, vs = append(ids, r.AllianceID), append(ks, r.Kills), append(vs, r.Isk)
		}
		store("alliance", ids, ks, vs)
	}
	if rows, err := app.queries.TopShipsLost(ctx, db.TopShipsLostParams{Since: day, MaxRows: 8}); err == nil {
		ids, ks, vs := make([]int64, 0, len(rows)), make([]int64, 0, len(rows)), make([]float64, 0, len(rows))
		for _, r := range rows {
			ids, ks, vs = append(ids, r.ShipTypeID), append(ks, r.Losses), append(vs, 0)
		}
		store("ship", ids, ks, vs)
	}
	if rows, err := app.queries.TopKillSystems(ctx, db.TopKillSystemsParams{Since: day, MaxRows: 8}); err == nil {
		ids, ks, vs := make([]int64, 0, len(rows)), make([]int64, 0, len(rows)), make([]float64, 0, len(rows))
		for _, r := range rows {
			ids, ks, vs = append(ids, r.SystemID), append(ks, r.Kills), append(vs, 0)
		}
		store("system", ids, ks, vs)
	}
}

// killEntityBiggest reads an entity's most valuable kill in a
// window from the stored daily aggregates.
func (app *Application) killEntityBiggest(ctx context.Context, names *killNames, kind string, id int64, since time.Time) *killRow {
	big, err := app.queries.GetKillEntityBiggest(ctx, db.GetKillEntityBiggestParams{Kind: kind, EntityID: id, Since: since})
	if err != nil || big <= 0 {
		return nil
	}
	k, err := app.queries.GetPublicKill(ctx, big)
	if err != nil {
		return nil
	}
	return scopedBiggestRow(names, k)
}

// killEntityTop reads an entity's top ships flown, ships lost and
// systems from the stored daily aggregates.
func (app *Application) killEntityTop(ctx context.Context, names *killNames, kind string, id int64, since time.Time) killboardTop {
	var top killboardTop
	if rows, err := app.queries.ListKillEntityTopShips(ctx, db.ListKillEntityTopShipsParams{Kind: kind, EntityID: id, Since: since, MaxRows: 8}); err == nil {
		for _, r := range rows {
			if r.Flown > 0 {
				top.TopShips = append(top.TopShips, killTopRow{ID: r.ShipTypeID, Name: names.typeName(r.ShipTypeID), Count: r.Flown})
			}
		}
	}
	if rows, err := app.queries.ListKillEntityShipsLost(ctx, db.ListKillEntityShipsLostParams{Kind: kind, EntityID: id, Since: since, MaxRows: 8}); err == nil {
		for _, r := range rows {
			top.Ships = append(top.Ships, killTopRow{ID: r.ShipTypeID, Name: names.typeName(r.ShipTypeID), Count: r.Lost})
		}
	}
	if rows, err := app.queries.ListKillEntityTopSystems(ctx, db.ListKillEntityTopSystemsParams{Kind: kind, EntityID: id, Since: since, MaxRows: 8}); err == nil {
		for _, r := range rows {
			s := names.system(r.SystemID)
			sec, tier := secRound(s.Security)
			top.Systems = append(top.Systems, killTopRow{ID: r.SystemID, Name: s.Name, Count: r.N, Note: sec, Tier: tier})
		}
	}
	return top
}
