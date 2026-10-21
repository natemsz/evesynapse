package app

// ---------------------------------------------------------------------------
// The killboard's ingest (schema 074): every killmail in New Eden.
//
// The public stream is zKillboard's R2Z2: one JSON file per killmail
// at <base>/<sequence>.json, the sequence global and strictly
// increasing, the latest at <base>/sequence.json, each file holding
// the full ESI killmail and zKillboard's metadata. One goroutine
// follows it (runKillfeed): a file found is ingested and the next is
// asked for after 100 ms; a file not there yet waits six seconds, as
// the stream asks; a 403 or 429 backs off a minute. The cursor is
// stored, so a restart resumes where it stopped; files last a day,
// so a gap longer than that (or a file that expired) is skipped
// rather than waited on. KILLFEED=off turns the stream off.
//
// Our characters' killmails arrive through ESI already
// (killmail_details); the maintenance pass merges any the board has
// not stored. Those are stored private: nobody posted them, so no
// page shows them, though they still count in aggregates. When the
// stream carries the same kill it becomes public.
//
// Every kill is priced by our own appraisal (priceKills): the hull,
// every fitted, cargo and nested container line, destroyed and
// dropped, through the same house price order the rest of the site
// uses; blueprint copies count as nothing. zKillboard's figure is
// kept beside ours. Attackers and the full killmail are kept a year;
// the summary row forever.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	killfeedStepWait    = 100 * time.Millisecond
	killfeedIdleWait    = 6 * time.Second
	killfeedBlockedWait = time.Minute
	// killfeedJumpGap: a cursor this far behind the stream's latest
	// is past the files' day-long retention; it jumps to the latest.
	killfeedJumpGap = 20000

	killDetailRetention = 365 * 24 * time.Hour
	killPriceBatch      = 300
	killMergeBatch      = 500
)

const killfeedUserAgent = "EveSynapse killboard (contact: the site operator; self-hosted EVE Online companion)"

// killPackage is one R2Z2 file.
type killPackage struct {
	KillmailID int64           `json:"killmail_id"`
	Hash       string          `json:"hash"`
	ESI        json.RawMessage `json:"esi"`
	ZKB        killZKB         `json:"zkb"`
	SequenceID int64           `json:"sequence_id"`
}

// killZKB is zKillboard's metadata on a kill (the fields kept).
type killZKB struct {
	LocationID int64   `json:"locationID"`
	TotalValue float64 `json:"totalValue"`
	Points     int32   `json:"points"`
	NPC        bool    `json:"npc"`
	Solo       bool    `json:"solo"`
	Awox       bool    `json:"awox"`
}

// killmailFull is the ESI killmail, every field the board keeps.
type killmailFull struct {
	KillmailID    int64     `json:"killmail_id"`
	KillmailTime  time.Time `json:"killmail_time"`
	SolarSystemID int64     `json:"solar_system_id"`
	Victim        struct {
		CharacterID   int64      `json:"character_id"`
		CorporationID int64      `json:"corporation_id"`
		AllianceID    int64      `json:"alliance_id"`
		FactionID     int64      `json:"faction_id"`
		ShipTypeID    int64      `json:"ship_type_id"`
		DamageTaken   int64      `json:"damage_taken"`
		Items         []killItem `json:"items"`
	} `json:"victim"`
	Attackers []struct {
		CharacterID   int64 `json:"character_id"`
		CorporationID int64 `json:"corporation_id"`
		AllianceID    int64 `json:"alliance_id"`
		FactionID     int64 `json:"faction_id"`
		ShipTypeID    int64 `json:"ship_type_id"`
		WeaponTypeID  int64 `json:"weapon_type_id"`
		DamageDone    int64 `json:"damage_done"`
		FinalBlow     bool  `json:"final_blow"`
	} `json:"attackers"`
}

// killItem is one victim item line; containers nest their contents.
type killItem struct {
	ItemTypeID        int64      `json:"item_type_id"`
	Flag              int64      `json:"flag"`
	QuantityDestroyed int64      `json:"quantity_destroyed"`
	QuantityDropped   int64      `json:"quantity_dropped"`
	Singleton         int64      `json:"singleton"`
	Items             []killItem `json:"items"`
}

// runKillfeed follows the public stream until ctx ends.
func (app *Application) runKillfeed(ctx context.Context) {
	if !app.cfg.KillfeedEnabled() {
		logging.Infof("killfeed: off (KILLFEED=off)")
		return
	}
	base := app.cfg.KillfeedBaseURL()
	client := &http.Client{Timeout: 30 * time.Second}
	seq := int64(0)
	if st, err := app.queries.GetKillfeedState(ctx); err == nil {
		seq = st.Sequence
	}
	ingested, skipped := 0, 0
	report := time.Now()
	for ctx.Err() == nil {
		if seq == 0 {
			latest, err := killfeedLatest(ctx, client, base)
			if err != nil {
				logging.Warnf("killfeed: latest sequence: %v", err)
				killfeedSleep(ctx, killfeedIdleWait)
				continue
			}
			seq = latest
			logging.Infof("killfeed: following the stream from sequence %d", seq)
		}
		status, body, err := killfeedGet(ctx, client, base+strconv.FormatInt(seq, 10)+".json")
		switch {
		case err != nil:
			if ctx.Err() == nil {
				logging.Warnf("killfeed: %v", err)
			}
			killfeedSleep(ctx, killfeedIdleWait)
		case status == http.StatusOK:
			if err := app.ingestKillPackage(ctx, body); err != nil {
				logging.Errorf("killfeed: sequence %d: %v", seq, err)
			} else {
				ingested++
			}
			seq++
			app.saveKillfeedCursor(ctx, seq)
			killfeedSleep(ctx, killfeedStepWait)
		case status == http.StatusNotFound:
			// Not written yet, or expired: the latest says which.
			latest, lerr := killfeedLatest(ctx, client, base)
			switch {
			case lerr == nil && latest-seq > killfeedJumpGap:
				logging.Warnf("killfeed: %d files behind, past the stream's retention; jumping to %d", latest-seq, latest)
				skipped += int(latest - seq)
				seq = latest
				app.saveKillfeedCursor(ctx, seq)
			case lerr == nil && latest > seq:
				skipped++
				seq++
				app.saveKillfeedCursor(ctx, seq)
				killfeedSleep(ctx, killfeedStepWait)
			default:
				killfeedSleep(ctx, killfeedIdleWait)
			}
		case status == http.StatusForbidden || status == http.StatusTooManyRequests:
			logging.Warnf("killfeed: stream answered %d; backing off", status)
			killfeedSleep(ctx, killfeedBlockedWait)
		default:
			logging.Warnf("killfeed: stream answered %d for sequence %d", status, seq)
			killfeedSleep(ctx, killfeedIdleWait)
		}
		if time.Since(report) >= 10*time.Minute {
			logging.Infof("killfeed: %d kills ingested, %d sequence gaps skipped in the last 10 minutes", ingested, skipped)
			ingested, skipped, report = 0, 0, time.Now()
		}
	}
}

func (app *Application) saveKillfeedCursor(ctx context.Context, seq int64) {
	if err := app.queries.SetKillfeedState(ctx, db.SetKillfeedStateParams{Sequence: seq, UpdatedAt: time.Now().UTC()}); err != nil && ctx.Err() == nil {
		logging.Errorf("killfeed: save cursor: %v", err)
	}
}

func killfeedSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func killfeedGet(ctx context.Context, client *http.Client, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", killfeedUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

func killfeedLatest(ctx context.Context, client *http.Client, base string) (int64, error) {
	status, body, err := killfeedGet(ctx, client, base+"sequence.json")
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("sequence.json answered %d", status)
	}
	var v struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Sequence <= 0 {
		return 0, fmt.Errorf("sequence.json unreadable")
	}
	return v.Sequence, nil
}

// ingestKillPackage stores one stream file's kill, public.
func (app *Application) ingestKillPackage(ctx context.Context, body []byte) error {
	var pkg killPackage
	if err := json.Unmarshal(body, &pkg); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if pkg.KillmailID <= 0 || len(pkg.ESI) == 0 {
		return errors.New("package without a killmail")
	}
	z := pkg.ZKB
	_, err := app.ingestKill(ctx, pkg.ESI, pkg.Hash, &z, "zkill", true)
	return err
}

// ingestKill stores one kill from either source: the summary row
// (or, for a kill already held, what this copy adds), and on the
// first copy its attackers and the full killmail, gzipped. It
// reports whether this was the first copy.
func (app *Application) ingestKill(ctx context.Context, raw json.RawMessage, hash string, z *killZKB, source string, public bool) (bool, error) {
	var km killmailFull
	if err := json.Unmarshal(raw, &km); err != nil {
		return false, fmt.Errorf("decode killmail: %w", err)
	}
	if km.KillmailID <= 0 || km.KillmailTime.IsZero() {
		return false, errors.New("killmail without an id or a time")
	}
	p := db.UpsertKillParams{
		KillmailID: km.KillmailID, Hash: hash, KillmailTime: km.KillmailTime.UTC(),
		SystemID: km.SolarSystemID, RegionID: app.killRegion(ctx, km.SolarSystemID),
		VictimCharacterID: km.Victim.CharacterID, VictimCorporationID: km.Victim.CorporationID,
		VictimAllianceID: km.Victim.AllianceID, VictimFactionID: km.Victim.FactionID,
		ShipTypeID: km.Victim.ShipTypeID, DamageTaken: km.Victim.DamageTaken,
		AttackerCount: int32(len(km.Attackers)),
		Source:        source, Public: public, IngestedAt: time.Now().UTC(),
	}
	for _, a := range km.Attackers {
		if a.FinalBlow {
			p.FbCharacterID, p.FbCorporationID, p.FbAllianceID, p.FbShipTypeID = a.CharacterID, a.CorporationID, a.AllianceID, a.ShipTypeID
			break
		}
	}
	if z != nil {
		p.LocationID, p.Points = z.LocationID, z.Points
		p.Npc, p.Solo, p.Awox = z.NPC, z.Solo, z.Awox
		if z.TotalValue > 0 {
			p.ValueZkill = sql.NullFloat64{Float64: z.TotalValue, Valid: true}
		}
	} else {
		// Our own reading of what zKillboard flags: every attacker
		// an NPC, or a lone attacker.
		npc := len(km.Attackers) > 0
		for _, a := range km.Attackers {
			if a.CharacterID > 0 {
				npc = false
			}
		}
		p.Npc, p.Solo = npc, !npc && len(km.Attackers) == 1
	}

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	inserted, err := q.UpsertKill(ctx, p)
	if err != nil {
		return false, fmt.Errorf("store kill %d: %w", km.KillmailID, err)
	}
	if inserted {
		ap := db.InsertKillAttackersParams{KillmailID: km.KillmailID}
		for i, a := range km.Attackers {
			ap.Seqs = append(ap.Seqs, int32(i))
			ap.CharacterIds = append(ap.CharacterIds, a.CharacterID)
			ap.CorporationIds = append(ap.CorporationIds, a.CorporationID)
			ap.AllianceIds = append(ap.AllianceIds, a.AllianceID)
			ap.FactionIds = append(ap.FactionIds, a.FactionID)
			ap.ShipTypeIds = append(ap.ShipTypeIds, a.ShipTypeID)
			ap.WeaponTypeIds = append(ap.WeaponTypeIds, a.WeaponTypeID)
			ap.DamageDones = append(ap.DamageDones, a.DamageDone)
			ap.FinalBlows = append(ap.FinalBlows, a.FinalBlow)
		}
		if len(ap.Seqs) > 0 {
			if err := q.InsertKillAttackers(ctx, ap); err != nil {
				return false, fmt.Errorf("store attackers %d: %w", km.KillmailID, err)
			}
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		if err := q.InsertKillDetail(ctx, db.InsertKillDetailParams{
			KillmailID: km.KillmailID, Payload: buf.Bytes(), StoredAt: time.Now().UTC(),
		}); err != nil {
			return false, fmt.Errorf("store detail %d: %w", km.KillmailID, err)
		}
	}
	return inserted, tx.Commit()
}

// killRegion is a system's region, from the SDE (cached once).
func (app *Application) killRegion(ctx context.Context, systemID int64) int64 {
	app.killRegionsMu.Lock()
	defer app.killRegionsMu.Unlock()
	if len(app.killRegions) == 0 {
		rows, err := app.queries.ListSystemRegions(ctx)
		if err != nil {
			return 0
		}
		m := make(map[int64]int64, len(rows))
		for _, r := range rows {
			m[r.SystemID] = r.RegionID
		}
		app.killRegions = m
	}
	return app.killRegions[systemID]
}

// killboardMaintenance is the worker's killboard pass: merge our
// characters' ESI killmails, price what is unpriced, and once a day
// prune details past retention.
func (app *Application) killboardMaintenance(ctx context.Context) {
	if rows, err := app.queries.ListESIKillsToMerge(ctx, killMergeBatch); err == nil {
		merged := 0
		for _, r := range rows {
			if ok, err := app.ingestKill(ctx, json.RawMessage(r.Payload), r.Hash, nil, "esi", false); err != nil {
				logging.Errorf("killboard: merge %d: %v", r.KillmailID, err)
			} else if ok {
				merged++
			}
		}
		if merged > 0 {
			logging.Infof("killboard: merged %d of our characters' killmails", merged)
		}
	} else {
		logging.Errorf("killboard: list ESI killmails: %v", err)
	}
	if n, err := app.priceKills(ctx); err != nil {
		logging.Errorf("killboard: price: %v", err)
	} else if n > 0 {
		logging.Infof("killboard: priced %d kills", n)
	}
	if time.Since(app.killLastPruned) >= 24*time.Hour {
		app.killLastPruned = time.Now()
		before := time.Now().UTC().Add(-killDetailRetention)
		d, err1 := app.queries.PruneKillDetails(ctx, before)
		a, err2 := app.queries.PruneKillAttackers(ctx, before)
		if err := errors.Join(err1, err2); err != nil {
			logging.Errorf("killboard: prune: %v", err)
		} else if d+a > 0 {
			logging.Infof("killboard: pruned %d killmails and %d attacker rows past a year", d, a)
		}
	}
}

// killLine is one priced line of a kill: a type, how many were
// destroyed and dropped.
type killLine struct {
	typeID             int64
	destroyed, dropped int64
}

// killLines flattens a kill into its priced lines: the hull, then
// every item, containers' contents included. Blueprint copies
// (singleton 2) are worth nothing on a kill and are left out.
func killLines(km killmailFull) []killLine {
	lines := []killLine{{typeID: km.Victim.ShipTypeID, destroyed: 1}}
	var walk func(items []killItem)
	walk = func(items []killItem) {
		for _, it := range items {
			if it.Singleton != 2 && it.ItemTypeID > 0 && it.QuantityDestroyed+it.QuantityDropped > 0 {
				lines = append(lines, killLine{typeID: it.ItemTypeID, destroyed: it.QuantityDestroyed, dropped: it.QuantityDropped})
			}
			walk(it.Items)
		}
	}
	walk(km.Victim.Items)
	return lines
}

// killValue totals a kill's lines at unit prices: the whole value,
// the dropped share, and the share of lines that had a price.
func killValue(lines []killLine, price map[int64]float64) (total, dropped float64, coverage float64) {
	priced := 0
	for _, l := range lines {
		p, ok := price[l.typeID]
		if !ok {
			continue
		}
		priced++
		total += p * float64(l.destroyed+l.dropped)
		dropped += p * float64(l.dropped)
	}
	if len(lines) > 0 {
		coverage = float64(priced) / float64(len(lines))
	}
	return total, dropped, coverage
}

// priceKills prices one batch of unpriced kills with our own
// appraisal, one AppraiseLines call for every type in the batch.
func (app *Application) priceKills(ctx context.Context) (int, error) {
	rows, err := app.queries.ListUnpricedKills(ctx, killPriceBatch)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	type pending struct {
		id    int64
		lines []killLine
	}
	var work []pending
	types := map[int64]bool{}
	for _, r := range rows {
		zr, err := gzip.NewReader(bytes.NewReader(r.Payload))
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			continue
		}
		var km killmailFull
		if json.Unmarshal(raw, &km) != nil {
			continue
		}
		ls := killLines(km)
		for _, l := range ls {
			types[l.typeID] = true
		}
		work = append(work, pending{id: r.KillmailID, lines: ls})
	}
	lines := make([]module.AppraisalLine, 0, len(types))
	for id := range types {
		lines = append(lines, module.AppraisalLine{TypeID: id, Quantity: 1, Included: true})
	}
	price := map[int64]float64{}
	if len(lines) > 0 {
		res, err := app.AppraiseLines(ctx, lines)
		if err != nil {
			return 0, err
		}
		for _, l := range res.Lines {
			if l.Priced {
				price[l.TypeID] = l.UnitPrice
			}
		}
	}
	now := time.Now().UTC()
	n := 0
	for _, w := range work {
		total, dropped, cov := killValue(w.lines, price)
		if err := app.queries.SetKillValue(ctx, db.SetKillValueParams{
			KillmailID: w.id, ValueOurs: total, ValueDroppedOurs: dropped,
			ValueCoverage: float32(cov), PricedAt: now,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
