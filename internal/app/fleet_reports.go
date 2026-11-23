package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/gamelog"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The worker's fleet report pass: which fleets changed, what each one's
// logs and mails add up to (fleet_merge.go), and the stored rows the
// pages read.
// ---------------------------------------------------------------------------

const (
	fleetReportBatch   = 10
	fleetFightBatch    = 500
	fleetClosedBatch   = 20
	fleetReportRefresh = time.Hour
	fleetLeadersEvery  = 15 * time.Minute
	// fleetSightingGrace is how far outside a pilot's fleet sightings
	// they still count as on it. The fleet is read about once a minute,
	// and a pilot can drop out right after their ship dies.
	fleetSightingGrace = 10 * time.Minute
)

var errFleetFightGone = errors.New("a fight was removed during the build")

// fleetReportWindow is the time a fleet's logs and mails can fall in.
func fleetReportWindow(op db.Op) (time.Time, time.Time) {
	return op.StartsAt.UTC().Add(-opCaptureLead), opEnd(op).UTC().Add(opCaptureTail)
}

// refreshFleetReports is the worker's pass. Local reads only; no ESI
// call. A fleet that cannot be built yet goes to the back of the queue,
// so it never holds up the others.
func (app *Application) refreshFleetReports(ctx context.Context) {
	now := time.Now().UTC()
	margin := int32(max(opCaptureLead, opCaptureTail) / time.Second)
	if _, err := app.queries.QueueFleetsForChangedFights(ctx, db.QueueFleetsForChangedFightsParams{RowLimit: fleetFightBatch, MarginSeconds: margin}); err != nil {
		logging.Errorf("worker: fleet reports: read changed fights: %v", err)
	}
	closedBefore := now.Add(-opCaptureTail)
	if err := app.queries.QueueClosedFleets(ctx, db.QueueClosedFleetsParams{
		ClosedBefore: closedBefore, LateSince: now.Add(-battleLateMails),
		RefreshBefore: now.Add(-fleetReportRefresh), RowLimit: fleetClosedBatch,
	}); err != nil {
		logging.Errorf("worker: fleet reports: queue closed fleets: %v", err)
	}
	rows, err := app.queries.ListFleetReportsToBuild(ctx, db.ListFleetReportsToBuildParams{ClosedBefore: closedBefore, RowLimit: fleetReportBatch})
	if err != nil {
		logging.Errorf("worker: fleet reports: list: %v", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		done, err := app.buildFleetReport(ctx, row.OpID, row.DirtySeq, now)
		if err != nil {
			logging.Errorf("worker: fleet reports: build fleet %d: %v", row.OpID, err)
		}
		if done && err == nil {
			continue
		}
		if err := app.queries.MarkFleetReportDirty(ctx, row.OpID); err != nil {
			logging.Errorf("worker: fleet reports: requeue fleet %d: %v", row.OpID, err)
		}
	}
	if now.Unix()-app.fleetLeadersAt.Load() >= int64(fleetLeadersEvery/time.Second) {
		if err := app.refreshFleetLeaders(ctx, now); err != nil {
			logging.Errorf("worker: fleet reports: leaderboards: %v", err)
			return
		}
		app.fleetLeadersAt.Store(now.Unix())
	}
}

// fleetBattleBuild is one battle ready to store.
type fleetBattleBuild struct {
	Battle   db.InsertFleetBattleParams
	FightIDs []int64
	Pilots   []db.InsertFleetBattlePilotParams
	Kills    []db.InsertFleetBattleKillParams
	Series   string
}

// fleetReportBuild is everything one fleet's build stores.
type fleetReportBuild struct {
	Battles []fleetBattleBuild
	Pilots  []db.InsertFleetScorePilotParams
	Mails   []db.InsertFleetScoreMailParams
}

// buildFleetReport rebuilds one fleet. done is false when it has to wait,
// for a pilot's name that is still being looked up. A cancelled fleet
// keeps no report.
func (app *Application) buildFleetReport(ctx context.Context, opID, seq int64, now time.Time) (done bool, err error) {
	op, err := app.queries.GetOp(ctx, opID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// The demo's reports are built by its seed from logs that are not
	// stored, so a rebuild here would empty them.
	if isDemoFleet(op) {
		return true, app.queries.ClearFleetReportDirty(ctx, db.ClearFleetReportDirtyParams{DirtySeq: seq, ComputedAt: timeSet(now), OpID: op.ID})
	}
	build := &fleetReportBuild{}
	if !op.CancelledAt.Valid {
		var ready bool
		if build, ready, err = app.readFleetReport(ctx, op, now); err != nil || !ready {
			return false, err
		}
	}
	return true, app.storeFleetReport(ctx, op.ID, seq, build, now)
}

// markFleetReportChanged queues a fleet's report, and those of later
// fleets whose attendance credit depends on this one.
func markFleetReportChanged(ctx context.Context, q *db.Queries, opID int64) error {
	if err := q.MarkFleetReportDirty(ctx, opID); err != nil {
		return err
	}
	return q.MarkLaterOverlappingFleets(ctx, opID)
}

// fleetFirstSighting is when the fleet read first saw the pilot. A row
// marked by hand before that keeps its earlier time for other uses.
func fleetFirstSighting(a db.OpAttendance) time.Time {
	if a.FirstFleetAt.Valid {
		return a.FirstFleetAt.Time.UTC()
	}
	return a.FirstSeenAt.UTC()
}

// fleetPresences is when each attendance row puts its pilot on the fleet.
// A row read from the fleet says when they were seen. A hand mark is the
// manager's word for the whole fleet.
func fleetPresences(rows []db.OpAttendance, from, to time.Time) map[int64]fleetPresence {
	out := make(map[int64]fleetPresence, len(rows))
	for _, a := range rows {
		p := fleetPresence{From: from, To: to}
		if a.Source == attendanceFleet {
			if seen := fleetFirstSighting(a).Add(-fleetSightingGrace); seen.After(p.From) {
				p.From = seen
			}
			if seen := a.LastSeenAt.UTC().Add(fleetSightingGrace); seen.Before(p.To) {
				p.To = seen
			}
		}
		out[a.CharacterID] = p
	}
	return out
}

// readFleetReport works out a fleet's battles and scores from stored
// rows. ready is false while a roster name is unknown: without it, a
// fleetmate's hits would read as an enemy's and inflate the totals.
func (app *Application) readFleetReport(ctx context.Context, op db.Op, now time.Time) (*fleetReportBuild, bool, error) {
	attendance, err := app.queries.ListOpAttendance(ctx, op.ID)
	if err != nil {
		return nil, false, err
	}
	build := &fleetReportBuild{}
	if len(attendance) == 0 {
		return build, true, nil
	}
	from, to := fleetReportWindow(op)
	on := fleetPresences(attendance, from, to)
	roster := make([]int64, 0, len(attendance))
	ships := make(map[int64]int64, len(attendance))
	for _, a := range attendance {
		roster = append(roster, a.CharacterID)
		ships[a.CharacterID] = a.ShipTypeID
	}
	reads, counted, ready, err := app.readFleetLogs(ctx, op, roster, on, from, to)
	if err != nil || !ready {
		return nil, false, err
	}
	mails, err := app.readFleetMails(ctx, roster, on, from, to)
	if err != nil {
		return nil, false, err
	}
	for i, c := range clusterFleetBattles(mails, reads, on) {
		if b, ok := composeFleetBattle(op.ID, int32(i), c, counted, on, ships, now); ok {
			build.Battles = append(build.Battles, b)
		}
	}
	for i := range build.Battles {
		build.Battles[i].Battle.Seq = int32(i)
	}
	earlier, err := app.queries.ListPilotsOnEarlierOverlappingFleet(ctx, op.ID)
	if err != nil {
		return nil, false, err
	}
	build.Pilots, build.Mails = fleetScoreRows(op, attendance, earlier, mails)
	return build, true, nil
}

// fleetScoreRows is what each pilot earned on a fleet.
func fleetScoreRows(op db.Op, attendance []db.OpAttendance, earlier []db.ListPilotsOnEarlierOverlappingFleetRow, mails []*fleetMail) ([]db.InsertFleetScorePilotParams, []db.InsertFleetScoreMailParams) {
	sameOwner := make(map[int64]bool, len(earlier))
	publicly := make(map[int64]bool, len(earlier))
	for _, e := range earlier {
		sameOwner[e.CharacterID] = e.SameOwner
		publicly[e.CharacterID] = e.EarlierPublic
	}
	sighted := 0
	for _, a := range attendance {
		if a.Source == attendanceFleet {
			sighted++
		}
	}
	pilots := make([]db.InsertFleetScorePilotParams, 0, len(attendance))
	for _, a := range attendance {
		seen := a.Source == attendanceFleet
		long := a.LastSeenAt.UTC().Sub(fleetFirstSighting(a)) >= fleetCreditMinPresence
		pilots = append(pilots, db.InsertFleetScorePilotParams{
			OpID: op.ID, CharacterID: a.CharacterID, FlownAt: op.StartsAt.UTC(), SeenInFleet: seen,
			MemberCredit: !sameOwner[a.CharacterID],
			PublicCredit: seen && long && sighted >= fleetCreditMinPilots && !publicly[a.CharacterID],
		})
	}
	var scored []db.InsertFleetScoreMailParams
	for _, m := range mails {
		for _, s := range fleetMailScores(m) {
			scored = append(scored, db.InsertFleetScoreMailParams{
				OpID: op.ID, CharacterID: s.Character, KillmailID: s.Mail, At: s.At,
				Loss: s.Loss, Counts: s.Counts, ValueIsk: s.Value, ValuePoints: s.ValuePoints,
			})
		}
	}
	return pilots, scored
}

// readFleetLogs loads the logs pilots chose to add. Every read touching
// the window is needed to settle which one counts for each clock step,
// but only reads that count for a step on the fleet are opened.
func (app *Application) readFleetLogs(ctx context.Context, op db.Op, roster []int64, on map[int64]fleetPresence, from, to time.Time) ([]*fleetLogRead, map[int64][]gamelog.StepRange, bool, error) {
	edge := time.Duration(gamelog.SliceSeconds) * time.Second
	rows, err := app.queries.ListFleetLogReads(ctx, db.ListFleetLogReadsParams{
		OpID: op.ID, EarliestStart: from.Add(-gamelog.MaxFightLength - edge), ToAt: to.Add(edge), FromAt: from.Add(-edge),
	})
	if err != nil {
		return nil, nil, false, err
	}
	all := make([]*fleetLogRead, 0, len(rows))
	for _, r := range rows {
		all = append(all, &fleetLogRead{
			ID: r.ID, Character: r.CharacterID, Start: r.StartedAt.UTC(), End: r.EndedAt.UTC(),
			Received: r.ReceivedAt, SystemID: r.SystemID, Usable: r.Usable,
		})
	}
	counted := countedByRead(all)
	var used, toOpen []*fleetLogRead
	for _, r := range all {
		p, ok := on[r.Character]
		if !ok || !p.overlaps(r.Start, r.End) {
			continue
		}
		used = append(used, r)
		for _, sr := range counted[r.ID] {
			if sr.To > p.From.Unix() && sr.From <= p.To.Unix() {
				toOpen = append(toOpen, r)
				break
			}
		}
	}
	if len(toOpen) == 0 {
		return used, counted, true, nil
	}
	names, ready := app.fleetRosterNames(ctx, roster)
	if !ready {
		return nil, nil, false, nil
	}
	onRoster := make(map[int64]bool, len(roster))
	for _, id := range roster {
		onRoster[id] = true
	}
	for _, r := range toOpen {
		raw, err := app.queries.GetBattleFightAligned(ctx, r.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			r.Usable = false
			continue
		}
		if err != nil {
			return nil, nil, false, err
		}
		if err := json.Unmarshal([]byte(raw), &r.Steps); err != nil {
			return nil, nil, false, fmt.Errorf("decode steps of fight %d: %w", r.ID, err)
		}
		parties, err := app.queries.ListBattleFightPilotParties(ctx, r.ID)
		if err != nil {
			return nil, nil, false, err
		}
		r.Fleetmates = make(map[int64]int64, len(parties))
		for _, p := range parties {
			switch {
			case onRoster[p.CharacterID]:
				r.Fleetmates[int64(p.Seq)] = p.CharacterID
			case names[strings.ToLower(p.Name)] != 0:
				r.Fleetmates[int64(p.Seq)] = names[strings.ToLower(p.Name)]
			}
		}
	}
	return used, counted, true, nil
}

// fleetRosterNames is the roster by lower-cased name, for telling
// fleetmates in a log. ready is false while a name is still being
// looked up, and the lookup is asked for.
func (app *Application) fleetRosterNames(ctx context.Context, roster []int64) (map[string]int64, bool) {
	out := make(map[string]int64, len(roster))
	ready := true
	for _, id := range roster {
		name, settled := app.resolvedCharacterName(ctx, id)
		if !settled {
			ready = false
			app.notePageWant(ctx, pageWantCharacter, id, 0)
			continue
		}
		if name != "" {
			out[strings.ToLower(name)] = id
		}
	}
	return out, ready
}

// readFleetMails is the public mails with a pilot who was on the fleet
// when the ship died, on either side.
func (app *Application) readFleetMails(ctx context.Context, roster []int64, on map[int64]fleetPresence, from, to time.Time) ([]*fleetMail, error) {
	rows, err := app.queries.ListFleetMails(ctx, db.ListFleetMailsParams{PaddingValue: killPaddingValue, Roster: roster, FromAt: from, ToAt: to})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, k := range rows {
		ids = append(ids, k.KillmailID)
	}
	attackers, err := app.queries.ListFleetMailAttackers(ctx, db.ListFleetMailAttackersParams{KillmailIds: ids, Roster: roster})
	if err != nil {
		return nil, err
	}
	byMail := make(map[int64]map[int64]*fleetMailAttacker, len(rows))
	for _, a := range attackers {
		m := byMail[a.KillmailID]
		if m == nil {
			m = map[int64]*fleetMailAttacker{}
			byMail[a.KillmailID] = m
		}
		if seen := m[a.CharacterID]; seen != nil {
			seen.Damage += a.DamageDone
			continue
		}
		m[a.CharacterID] = &fleetMailAttacker{Character: a.CharacterID, Ship: a.ShipTypeID, Damage: a.DamageDone}
	}
	out := make([]*fleetMail, 0, len(rows))
	for _, k := range rows {
		at := k.KillmailTime.UTC()
		victim, listed := on[k.VictimCharacterID]
		m := &fleetMail{
			ID: k.KillmailID, At: at, System: k.SystemID, Victim: k.VictimCharacterID, Ship: k.ShipTypeID,
			Damage: k.DamageTaken, Value: k.ValueOurs, Counts: k.Counts, Loss: listed && victim.holds(at),
		}
		for id, a := range byMail[k.KillmailID] {
			if p, ok := on[id]; ok && p.holds(at) {
				m.Attackers = append(m.Attackers, *a)
			}
		}
		if m.Loss || len(m.Attackers) > 0 {
			out = append(out, m)
		}
	}
	return out, nil
}

// composeFleetBattle turns one cluster into stored rows. A cluster with
// no mail and nothing counted from a log is not a battle.
func composeFleetBattle(opID int64, seq int32, c *fleetCluster, counted map[int64][]gamelog.StepRange, on map[int64]fleetPresence, ships map[int64]int64, now time.Time) (fleetBattleBuild, bool) {
	merge := mergeFleetLogs(c.Reads, counted, on)
	if len(c.Mails) == 0 && merge.Out+merge.In+merge.Reps+merge.Cap+merge.Friendly == 0 {
		return fleetBattleBuild{}, false
	}
	pilots := map[int64]*db.InsertFleetBattlePilotParams{}
	touch := func(id int64) *db.InsertFleetBattlePilotParams {
		p := pilots[id]
		if p == nil {
			p = &db.InsertFleetBattlePilotParams{CharacterID: id, ShipTypeID: ships[id]}
			pilots[id] = p
		}
		return p
	}
	for id, p := range on {
		if p.overlaps(c.From, c.To) {
			touch(id)
		}
	}
	b := fleetBattleBuild{Battle: db.InsertFleetBattleParams{
		OpID: opID, Seq: seq, SystemID: c.System, StartedAt: c.From, EndedAt: c.To,
		DamageOut: merge.Out, DamageIn: merge.In, RepsOut: merge.Reps, CapOut: merge.Cap, FriendlyFire: merge.Friendly,
		ComputedAt: now,
	}}
	for _, m := range c.Mails {
		value := 0.0
		if m.Value.Valid {
			value = m.Value.Float64
		} else {
			b.Battle.UnpricedMails++
		}
		if m.Loss {
			b.Battle.Losses++
			b.Battle.IskLost += value
			p := touch(m.Victim)
			p.Died = true
			p.ShipTypeID = m.Ship
		} else {
			b.Battle.Kills++
			b.Battle.IskDestroyed += value
			for _, a := range m.Attackers {
				p := touch(a.Character)
				p.KillsOn++
				if p.ShipTypeID == 0 {
					p.ShipTypeID = a.Ship
				}
			}
		}
		b.Kills = append(b.Kills, db.InsertFleetBattleKillParams{
			KillmailID: m.ID, KillmailTime: m.At, Loss: m.Loss, VictimCharacterID: m.Victim, ShipTypeID: m.Ship, ValueIsk: sql.NullFloat64{Float64: m.Value.Float64, Valid: m.Value.Valid},
		})
	}
	for id, d := range merge.Pilots {
		p := touch(id)
		p.DamageOut, p.DamageIn, p.RepsOut, p.HasLog = d.Out, d.In, d.Reps, true
	}
	leftOut := map[int64]bool{}
	for _, r := range c.Reads {
		b.FightIDs = append(b.FightIDs, r.ID)
		if !r.Usable {
			leftOut[r.Character] = true
		}
	}
	for id := range leftOut {
		if p := pilots[id]; p == nil || !p.HasLog {
			b.Battle.LogsLeftOut++
		}
	}
	for _, p := range pilots {
		b.Battle.PilotsTotal++
		if p.HasLog {
			b.Battle.PilotsWithLogs++
		}
		b.Pilots = append(b.Pilots, *p)
	}
	if curve := buildFleetCurve(merge.Steps); curve.Seconds > 0 {
		if raw, err := json.Marshal(curve); err == nil {
			b.Series = string(raw)
		}
	}
	return b, true
}

// storeFleetReport replaces the fleet's rows in one transaction and
// clears the mark it was built from, unless a newer one has landed.
func (app *Application) storeFleetReport(ctx context.Context, opID, seq int64, build *fleetReportBuild, now time.Time) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if err := q.DeleteFleetBattles(ctx, opID); err != nil {
		return err
	}
	if err := q.DeleteFleetScorePilots(ctx, opID); err != nil {
		return err
	}
	if err := q.DeleteFleetScoreMails(ctx, opID); err != nil {
		return err
	}
	for _, b := range build.Battles {
		id, err := q.InsertFleetBattle(ctx, b.Battle)
		if err != nil {
			return err
		}
		if len(b.FightIDs) > 0 {
			n, err := q.InsertFleetBattleFights(ctx, db.InsertFleetBattleFightsParams{FleetBattleID: id, FightIds: b.FightIDs})
			if err != nil {
				return err
			}
			if n != int64(len(b.FightIDs)) {
				return errFleetFightGone
			}
		}
		for _, p := range b.Pilots {
			p.FleetBattleID = id
			if err := q.InsertFleetBattlePilot(ctx, p); err != nil {
				return err
			}
		}
		for _, k := range b.Kills {
			k.FleetBattleID = id
			if err := q.InsertFleetBattleKill(ctx, k); err != nil {
				return err
			}
		}
		if b.Series != "" {
			if err := q.InsertFleetBattleSeries(ctx, db.InsertFleetBattleSeriesParams{FleetBattleID: id, Payload: b.Series}); err != nil {
				return err
			}
		}
	}
	for _, p := range build.Pilots {
		if err := q.InsertFleetScorePilot(ctx, p); err != nil {
			return err
		}
	}
	for _, m := range build.Mails {
		if err := q.InsertFleetScoreMail(ctx, m); err != nil {
			return err
		}
	}
	if err := q.ClearFleetReportDirty(ctx, db.ClearFleetReportDirtyParams{DirtySeq: seq, ComputedAt: timeSet(now), OpID: opID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// fleetLeaderPeriods are the boards' time spans. Days 0 is all time.
var fleetLeaderPeriods = []struct {
	Key, Label string
	Days       int
}{
	{"30d", "Last 30 days", 30},
	{"all", "All time", 0},
}

// refreshFleetLeaders ranks every board in one transaction, so a reader
// never sees a half-written board.
func (app *Application) refreshFleetLeaders(ctx context.Context, now time.Time) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if err := q.DeleteFleetLeaders(ctx); err != nil {
		return err
	}
	for _, p := range fleetLeaderPeriods {
		since := time.Unix(0, 0).UTC()
		if p.Days > 0 {
			since = now.AddDate(0, 0, -p.Days)
		}
		for _, run := range []struct{ public, byOwner bool }{{true, false}, {true, true}, {false, true}} {
			if err := q.RankFleetPilots(ctx, db.RankFleetPilotsParams{
				Period: p.Key, PublicOnly: run.public, ByOwner: run.byOwner, Since: since,
				PointsFleet: fleetPointsAttend, PointsKill: fleetPointsKill, PointsDeath: fleetPointsDeath,
			}); err != nil {
				return err
			}
		}
		if err := q.RankFleetGroups(ctx, db.RankFleetGroupsParams{
			Since: since, PointsFleet: fleetPointsAttend, PointsKill: fleetPointsKill, PointsDeath: fleetPointsDeath, Period: p.Key,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
