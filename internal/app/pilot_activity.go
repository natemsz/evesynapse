package app

// ---------------------------------------------------------------------------
// Pilot activity profiles (schema 088): what the public kill record
// says about a pilot, kept as stored aggregates (the ships they fly
// and lose, the systems they are active in, the hours, the usual
// hull, the gang size) and read by the pilot page's activity
// section (pilot_intel.go). Pages read these rows only, never the
// raw kills.
//
// A pilot first met in an intel reading (a channel report, a scan)
// is backfilled once from zKillboard's history API: up to ten
// pages of their most recent kills and losses, folded kill by kill
// into the aggregates. After that the live kill stream
// (killfeed.go) keeps the profile current, folding in each new
// kill the pilot loses or lands the final blow on. One marker per
// (kill, pilot) already folded in, written in the same transaction
// as the fold, keeps the two writers from ever counting the same
// kill twice.
//
// The backfill runs on one bounded worker: a fixed-size queue,
// drop-and-log when full (the offer is made again on the pilot's
// next reading), requests spaced out at the API's etiquette, and
// a descriptive user agent. A pilot whose backfill fails is left
// unmarked, so a later reading offers them again and the markers
// keep the retry honest.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
)

const (
	// The backfill queue's depth: readings offer pilots faster
	// than one worker at the API's pace can ever take them, and a
	// dropped offer costs nothing (the next reading repeats it).
	pilotBackfillQueueSize = 256
	// A backfill reads at most this many pages of a pilot's
	// history: 200 kills and losses a page, newest first, so the
	// profile covers the pilot's recent years and the pass stays
	// bounded however long the record runs.
	pilotBackfillMaxPages = 10
	pilotBackfillPageSize = 200 // the API's page size, per its docs
	// The pace between history requests: the API asks clients to
	// space repeated requests out; one a second is that, with
	// margin.
	pilotBackfillPace = 1100 * time.Millisecond
)

const pilotBackfillUserAgent = "EveSynapse pilot activity (contact: the site operator; self-hosted EVE Online companion)"

// pilotBackfillQueueChan returns the backfill queue, making it on
// first use.
func (app *Application) pilotBackfillQueueChan() chan int64 {
	app.pilotBackfillMu.Lock()
	defer app.pilotBackfillMu.Unlock()
	if app.pilotBackfillCh == nil {
		app.pilotBackfillCh = make(chan int64, pilotBackfillQueueSize)
		app.pilotBackfillQueued = map[int64]bool{}
	}
	return app.pilotBackfillCh
}

// notePilotsEncountered offers pilots met in a reading (a channel
// report, a scan) to the backfill worker. It never blocks the
// reading: a full queue drops the offer and logs it, and the
// pilot's next reading offers them again.
func (app *Application) notePilotsEncountered(ids []int64) {
	if !app.cfg.PilotBackfillEnabled() || len(ids) == 0 {
		return
	}
	ch := app.pilotBackfillQueueChan()
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		app.pilotBackfillMu.Lock()
		if !app.pilotBackfillQueued[id] {
			select {
			case ch <- id:
				app.pilotBackfillQueued[id] = true
			default:
				logging.Warnf("pilot activity: backfill queue full; pilot %d waits for a later reading", id)
			}
		}
		app.pilotBackfillMu.Unlock()
	}
}

// noteAnalysisPilots offers the pilots an intel analysis resolved
// to the backfill worker.
func (app *Application) noteAnalysisPilots(a intelanalyze.Analysis) {
	if len(a.Pilots) == 0 {
		return
	}
	ids := make([]int64, 0, len(a.Pilots))
	for _, p := range a.Pilots {
		ids = append(ids, p.ID)
	}
	app.notePilotsEncountered(ids)
}

// runPilotBackfill is the backfill worker: one pilot at a time,
// at the API's pace, until ctx ends.
func (app *Application) runPilotBackfill(ctx context.Context) {
	if !app.cfg.PilotBackfillEnabled() {
		logging.Infof("pilot activity: backfill off (PILOT_BACKFILL=off)")
		return
	}
	ch := app.pilotBackfillQueueChan()
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-ch:
			app.backfillPilot(ctx, client, id)
			app.pilotBackfillMu.Lock()
			delete(app.pilotBackfillQueued, id)
			app.pilotBackfillMu.Unlock()
		}
	}
}

// backfillPilot folds a pilot's recent kill history from
// zKillboard into their activity profile, once. A pilot already
// backfilled is left alone; a failed pass leaves the profile
// unmarked, so a later reading offers the pilot again.
func (app *Application) backfillPilot(ctx context.Context, client *http.Client, id int64) {
	prof, err := app.queries.GetPilotActivityProfile(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logging.Warnf("pilot activity: read profile %d: %v", id, err)
		return
	}
	if err == nil && prof.BackfilledAt.Valid {
		return
	}
	if err != nil {
		// No profile yet: claim it. The row's presence is what
		// lets the live stream maintain the profile from here on.
		if err := app.queries.ClaimPilotActivityProfile(ctx, db.ClaimPilotActivityProfileParams{
			CharacterID: id, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			logging.Warnf("pilot activity: claim profile %d: %v", id, err)
			return
		}
	}
	base := app.cfg.ZKillAPIBaseURL()
	seen := 0
	for page := 1; page <= pilotBackfillMaxPages; page++ {
		if !killfeedSleepCtx(ctx, pilotBackfillPace) {
			return
		}
		entries, err := zkillHistoryPage(ctx, client, base, id, page)
		if err != nil {
			logging.Warnf("pilot activity: backfill pilot %d, page %d: %v", id, page, err)
			return
		}
		for _, e := range entries {
			if e.KillmailID <= 0 || e.KillmailTime.IsZero() {
				continue
			}
			seen++
			if _, err := app.applyPilotKill(ctx, id, e.killmailFull, false); err != nil {
				logging.Warnf("pilot activity: fold kill %d for pilot %d: %v", e.KillmailID, id, err)
			}
		}
		if len(entries) < pilotBackfillPageSize {
			break
		}
	}
	if err := app.queries.MarkPilotActivityBackfilled(ctx, db.MarkPilotActivityBackfilledParams{
		CharacterID: id, BackfilledAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		logging.Warnf("pilot activity: mark pilot %d backfilled: %v", id, err)
		return
	}
	logging.Infof("pilot activity: backfilled pilot %d from %d kills and losses", id, seen)
}

// zkillHistoryEntry is one killmail from zKillboard's history
// API: the ESI killmail with zKillboard's metadata beside it.
type zkillHistoryEntry struct {
	killmailFull
	ZKB killZKB `json:"zkb"`
}

// zkillHistoryPage fetches one page of a pilot's kills and losses
// from zKillboard's history API, newest first, 200 to a page. The
// API may answer an error as a JSON object holding an error field
// without a failure status, so the body is checked too.
func zkillHistoryPage(ctx context.Context, client *http.Client, base string, characterID int64, page int) ([]zkillHistoryEntry, error) {
	url := base + "characterID/" + strconv.FormatInt(characterID, 10) + "/"
	if page > 1 {
		url += "page/" + strconv.Itoa(page) + "/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", pilotBackfillUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("history API answered %d", resp.StatusCode)
	}
	var entries []zkillHistoryEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			return nil, fmt.Errorf("history API error: %s", apiErr.Error)
		}
		return nil, fmt.Errorf("decode history page: %w", err)
	}
	return entries, nil
}

// applyPilotKill folds one killmail into one pilot's activity
// profile: their part in it (a loss, a kill, a final blow, the
// gang they flew with on it) and where and when it happened. It
// reports whether the kill was newly folded in. The (kill,
// pilot) marker is written in the same transaction, so neither
// writer can count a kill twice. requireProfile limits the fold
// to pilots who already have a profile: the live stream's rule,
// since it maintains profiles rather than starting them.
func (app *Application) applyPilotKill(ctx context.Context, characterID int64, km killmailFull, requireProfile bool) (bool, error) {
	if characterID <= 0 || km.KillmailID <= 0 || km.KillmailTime.IsZero() {
		return false, nil
	}
	var isVictim, isAttacker, finalBlow bool
	var victimShip, attackerShip int64
	if km.Victim.CharacterID == characterID {
		isVictim, victimShip = true, km.Victim.ShipTypeID
	}
	for _, a := range km.Attackers {
		if a.CharacterID == characterID {
			isAttacker, attackerShip = true, a.ShipTypeID
			finalBlow = a.FinalBlow
			break
		}
	}
	if !isVictim && !isAttacker {
		return false, nil
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if requireProfile {
		if _, err := q.GetPilotActivityProfile(ctx, characterID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
	}
	if _, err := q.MarkPilotKillApplied(ctx, db.MarkPilotKillAppliedParams{
		KillmailID: km.KillmailID, CharacterID: characterID, KillmailTime: km.KillmailTime.UTC(),
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // already folded in
		}
		return false, err
	}
	pp := db.ApplyPilotActivityKillParams{
		CharacterID: characterID, AppliedAt: time.Now().UTC(),
		KillmailTime: sql.NullTime{Time: km.KillmailTime.UTC(), Valid: true},
	}
	if isAttacker {
		pp.Kills = 1
		pp.GangTotal = int64(len(km.Attackers))
		pp.GangSamples = 1
	}
	if isVictim {
		pp.Losses = 1
	}
	if finalBlow {
		pp.FinalBlows = 1
	}
	if err := q.ApplyPilotActivityKill(ctx, pp); err != nil {
		return false, err
	}
	if isVictim && victimShip > 0 {
		if err := q.ApplyPilotActivityShip(ctx, db.ApplyPilotActivityShipParams{
			CharacterID: characterID, ShipTypeID: victimShip, Lost: 1,
		}); err != nil {
			return false, err
		}
	}
	if isAttacker && attackerShip > 0 {
		sp := db.ApplyPilotActivityShipParams{CharacterID: characterID, ShipTypeID: attackerShip, Flown: 1}
		if finalBlow {
			sp.FinalBlows = 1
		}
		if err := q.ApplyPilotActivityShip(ctx, sp); err != nil {
			return false, err
		}
	}
	if km.SolarSystemID > 0 {
		if err := q.ApplyPilotActivitySystem(ctx, db.ApplyPilotActivitySystemParams{
			CharacterID: characterID, SystemID: km.SolarSystemID, KillmailTime: km.KillmailTime.UTC(),
		}); err != nil {
			return false, err
		}
	}
	if err := q.ApplyPilotActivityHour(ctx, db.ApplyPilotActivityHourParams{
		CharacterID: characterID, Hour: int16(km.KillmailTime.UTC().Hour()),
	}); err != nil {
		return false, err
	}
	if err := q.RefreshPilotUsualShip(ctx, characterID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// maintainPilotActivity folds a kill from the live stream into
// the profiles of the two pilots it headlines, the victim and the
// final blow, when they have profiles. The rest of the attackers
// wait for their own backfill, which reads their whole part in
// it: folding every attacker on every kill through the stream
// would multiply the ingest writes by the gang size.
func (app *Application) maintainPilotActivity(ctx context.Context, raw json.RawMessage) {
	if !app.cfg.PilotBackfillEnabled() {
		return
	}
	var km killmailFull
	if err := json.Unmarshal(raw, &km); err != nil {
		return
	}
	pilots := map[int64]bool{}
	if km.Victim.CharacterID > 0 {
		pilots[km.Victim.CharacterID] = true
	}
	for _, a := range km.Attackers {
		if a.FinalBlow && a.CharacterID > 0 {
			pilots[a.CharacterID] = true
			break
		}
	}
	for id := range pilots {
		if _, err := app.applyPilotKill(ctx, id, km, true); err != nil {
			logging.Warnf("pilot activity: fold stream kill %d for pilot %d: %v", km.KillmailID, id, err)
		}
	}
}

// killfeedSleepCtx waits d, or until ctx ends; it reports whether
// the wait ran its course.
func killfeedSleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pilotActivityShip is one row of the activity section's ships
// panel, in a profile's terms.
type pilotActivityShip struct {
	ID   int64
	Name string
	Pct  int
	Text string // the counts that are not zero, e.g. "12 flown · 3 final blows · 2 lost"
}

// pilotActivityView is a pilot's stored kill record, read for
// their page by pilot_intel.go: the aggregates of every recorded
// kill and loss, which take over the activity section's numbers
// when the pilot has a profile with a record.
type pilotActivityView struct {
	Any         bool
	Kills       int64
	Losses      int64
	UsualShip   string
	UsualShipID int64
	Gang        string
	LastActive  string
	UpdatedAgo  string
	Ships       []pilotActivityShip
	Systems     []pilotCount
	Hours       []pilotHour
	Peak        string
}

// loadPilotActivity reads a pilot's stored kill record: the
// profile row and its top ships, systems and hours, all stored
// aggregates with bounded reads. No profile (the pilot has not
// been met in a reading yet) reads as nil, and the pilot's page
// falls back to the killboard read.
func (app *Application) loadPilotActivity(ctx context.Context, id int64, names *killNames, now time.Time) *pilotActivityView {
	prof, err := app.queries.GetPilotActivityProfile(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Warnf("pilot activity %d: read profile: %v", id, err)
		}
		return nil
	}
	v := &pilotActivityView{Kills: prof.Kills, Losses: prof.Losses, UpdatedAgo: ageText(now, prof.UpdatedAt)}
	v.Any = prof.Kills+prof.Losses > 0
	if !v.Any {
		return v
	}
	if prof.UsualShipTypeID > 0 {
		v.UsualShipID = prof.UsualShipTypeID
		v.UsualShip = names.typeName(prof.UsualShipTypeID)
	}
	if prof.GangSamples > 0 {
		avg := float64(prof.GangTotal) / float64(prof.GangSamples)
		if avg == float64(int64(avg)) {
			v.Gang = strconv.FormatInt(int64(avg), 10)
		} else {
			v.Gang = strconv.FormatFloat(avg, 'f', 1, 64)
		}
	}
	if prof.LastActiveAt.Valid {
		v.LastActive = ageText(now, prof.LastActiveAt.Time)
	}
	if rows, err := app.queries.ListPilotActivityShips(ctx, db.ListPilotActivityShipsParams{CharacterID: id, MaxRows: 8}); err == nil && len(rows) > 0 {
		for _, r := range rows {
			var parts []string
			if r.Flown > 0 {
				parts = append(parts, fmt.Sprintf("%d flown", r.Flown))
			}
			if r.FinalBlows > 0 {
				parts = append(parts, fmt.Sprintf("%d final blows", r.FinalBlows))
			}
			if r.Lost > 0 {
				parts = append(parts, fmt.Sprintf("%d lost", r.Lost))
			}
			v.Ships = append(v.Ships, pilotActivityShip{
				ID: r.ShipTypeID, Name: names.typeName(r.ShipTypeID),
				Pct: pctOf(r.Flown+r.Lost, rows[0].Flown+rows[0].Lost), Text: strings.Join(parts, " · "),
			})
		}
	} else if err != nil {
		logging.Warnf("pilot activity %d: ships: %v", id, err)
	}
	if rows, err := app.queries.ListPilotActivitySystems(ctx, db.ListPilotActivitySystemsParams{CharacterID: id, MaxRows: 8}); err == nil && len(rows) > 0 {
		for _, r := range rows {
			sys := names.system(r.SystemID)
			c := pilotCount{ID: r.SystemID, Name: sys.Name, N: r.Events, Pct: pctOf(r.Events, rows[0].Events), Ago: ageText(now, r.LastAt)}
			c.SecText, c.SecTier = secRound(sys.Security)
			v.Systems = append(v.Systems, c)
		}
	} else if err != nil {
		logging.Warnf("pilot activity %d: systems: %v", id, err)
	}
	if rows, err := app.queries.ListPilotActivityHours(ctx, id); err == nil && len(rows) > 0 {
		v.Hours = make([]pilotHour, 24)
		var top int64
		for h := range v.Hours {
			v.Hours[h].Hour = h
		}
		for _, r := range rows {
			if r.Hour >= 0 && r.Hour < 24 {
				v.Hours[r.Hour].N = r.Events
				top = max(top, r.Events)
			}
		}
		for h := range v.Hours {
			if v.Hours[h].N > 0 {
				v.Hours[h].Pct = pctOf(v.Hours[h].N, top)
			}
		}
		v.Peak = peakHours(v.Hours)
	} else if err != nil {
		logging.Warnf("pilot activity %d: hours: %v", id, err)
	}
	return v
}
