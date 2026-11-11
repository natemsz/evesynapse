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
// uses; blueprint copies count as nothing. zKillboard's own figure
// is never read or kept: the price on a kill is ours or nothing.
// Attackers and the full killmail are kept a year;
// the summary row forever. A kill that arrives already past that
// year, from a history walk or the ESI merge rather than the
// stream, is stored in its post-prune shape from the start:
// summary row and priced lines, no attackers, no killmail.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"io"
	"net/http"
	"strconv"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"

	// goccyjson decodes the killmail payloads at ingest and in
	// the pricing pass: faster than encoding/json with identical
	// results (TestAppDecoderParity in decode_parity_test.go).
	// The standard library stays for the stream envelope and the
	// RawMessage plumbing.
	goccyjson "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
)

const (
	killfeedStepWait    = 100 * time.Millisecond
	killfeedIdleWait    = 6 * time.Second
	killfeedBlockedWait = time.Minute
	// killfeedCursorBatch coalesces the cursor write: one
	// SetKillfeedState per batch of sequences instead of one
	// per kill at stream pace. Ingest is idempotent, so
	// replaying an unsaved tail after a crash is safe.
	killfeedCursorBatch = 25
	// killfeedJumpGap: a cursor this far behind the stream's latest
	// is past the files' day-long retention; it jumps to the latest.
	killfeedJumpGap = 20000

	killDetailRetention = 2 * 365 * 24 * time.Hour
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
// Its totalValue is never read: a kill's price is EveSynapse's own
// appraisal or nothing. The hash rides along for the history
// backfill (pilot_activity.go), which hands it to the kill store
// with the killmail it came with.
type killZKB struct {
	LocationID int64  `json:"locationID"`
	Hash       string `json:"hash"`
	Points     int32  `json:"points"`
	NPC        bool   `json:"npc"`
	Solo       bool   `json:"solo"`
	Awox       bool   `json:"awox"`
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

// killAttackersParams is one kill's attacker rows, in killmail
// order.
func killAttackersParams(km killmailFull) db.InsertKillAttackersParams {
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
	return ap
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
// Kill details are stored zstd-compressed: at killmail sizes
// EncodeAll halves the bytes of gzip at the same speed, and
// DecodeAll reads them back faster than a fresh gzip reader.
// Rows stored before the switch are gzip; killDetailRaw tells
// the two apart by magic bytes, so nothing already stored
// needs rewriting. The shared encoder and decoder serve the
// single-shot calls only, which is the concurrency shape
// klauspost documents for EncodeAll and DecodeAll.
var (
	killZstdEnc, killZstdEncErr = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	killZstdDec, killZstdDecErr = zstd.NewReader(nil)
)

// compressKillDetail encodes one killmail for kill_details.
func compressKillDetail(raw []byte) []byte {
	if killZstdEncErr == nil {
		return killZstdEnc.EncodeAll(raw, nil)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return buf.Bytes()
}

// killDetailRaw decodes a stored kill detail, zstd or gzip.
func killDetailRaw(payload []byte) ([]byte, error) {
	if len(payload) >= 4 && payload[0] == 0x28 && payload[1] == 0xb5 && payload[2] == 0x2f && payload[3] == 0xfd {
		if killZstdDecErr != nil {
			return nil, killZstdDecErr
		}
		return killZstdDec.DecodeAll(payload, nil)
	}
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func (app *Application) runKillfeed(ctx context.Context) {
	app.primeRecentKills(ctx)
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
	lastLatestCheck := time.Time{}
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
				// The cursor does not advance past a kill
				// that did not land: advancing here would
				// skip it silently and permanently. The same
				// sequence is retried after the idle wait;
				// ingest is idempotent, and a package that
				// can never land blocks loudly here instead
				// of leaving a silent hole.
				logging.Errorf("killfeed: sequence %d: %v (will retry)", seq, err)
				killfeedSleep(ctx, killfeedIdleWait)
				continue
			}
			ingested++
			seq++
			if seq%killfeedCursorBatch == 0 {
				app.saveKillfeedCursor(ctx, seq)
			}
			killfeedSleep(ctx, killfeedStepWait)
		case status == http.StatusNotFound:
			// Not written yet, or expired: the latest says
			// which, but asking on every 404 doubles the
			// idle request rate forever; the latest only
			// matters when a gap may have expired, so it is
			// re-asked at most every half minute.
			if time.Since(lastLatestCheck) < 30*time.Second {
				app.saveKillfeedCursor(ctx, seq)
				killfeedSleep(ctx, killfeedIdleWait)
				continue
			}
			lastLatestCheck = time.Now()
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
			app.saveKillfeedCursor(ctx, seq)
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
	_, err := app.ingestKill(ctx, pkg.ESI, pkg.Hash, &z, "zkill", true, false)
	if err == nil {
		// A stored kill also feeds the activity profiles of the
		// pilots it headlines (pilot_activity.go).
		app.maintainPilotActivity(ctx, pkg.ESI)
	}
	return err
}

// ingestKill stores one kill from either source: the summary row
// (or, for a kill already held, what this copy adds), and on the
// first copy its permanent involvement record and, unless the
// kill is stored in post-prune shape, its attackers and the full
// killmail, gzipped. It reports whether this was the first copy.
// With pruneAged set, a
// first copy already past the retention cutoff is stored in its
// post-prune shape instead: the summary row, its involvement and
// its attackers (for the stats fold, pruned once counted),
// without the killmail or per-line prices. The stream passes pruneAged
// false: its kills are always young.
func (app *Application) ingestKill(ctx context.Context, raw json.RawMessage, hash string, z *killZKB, source string, public bool, pruneAged bool) (bool, error) {
	var km killmailFull
	if err := goccyjson.Unmarshal(raw, &km); err != nil {
		return false, fmt.Errorf("decode killmail: %w", err)
	}
	if km.KillmailID <= 0 || km.KillmailTime.IsZero() {
		return false, errors.New("killmail without an id or a time")
	}
	prunedShape := pruneAged && km.KillmailTime.Before(time.Now().UTC().Add(-killDetailRetention))
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

	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	// A public copy queues the kill for the stats fold when it is
	// new or was stored private until now; a kill already public
	// was queued (or counted) before and is not queued twice.
	wasPublic := false
	if public {
		if was, err := q.GetKillPublicForUpdate(ctx, km.KillmailID); err == nil {
			wasPublic = was
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("read kill %d: %w", km.KillmailID, err)
		}
	}
	inserted, err := q.UpsertKill(ctx, p)
	if err != nil {
		return false, fmt.Errorf("store kill %d: %w", km.KillmailID, err)
	}
	if public && !wasPublic {
		if err := q.InsertKillStatsPending(ctx, db.InsertKillStatsPendingParams{
			KillmailID: km.KillmailID, KillmailTime: km.KillmailTime.UTC(),
		}); err != nil {
			return false, fmt.Errorf("queue kill %d for stats: %w", km.KillmailID, err)
		}
	}
	if inserted {
		// The permanent involvement record lands on every first
		// copy, including one stored in post-prune shape: the
		// kill's priced value keeps crediting the attackers after
		// their detail ages out.
		iv := db.InsertKillInvolvementParams{KillmailID: km.KillmailID}
		for _, a := range km.Attackers {
			iv.CharacterIds = append(iv.CharacterIds, a.CharacterID)
			iv.CorporationIds = append(iv.CorporationIds, a.CorporationID)
			iv.AllianceIds = append(iv.AllianceIds, a.AllianceID)
		}
		if len(iv.CharacterIds) > 0 {
			if err := q.InsertKillInvolvement(ctx, iv); err != nil {
				return false, fmt.Errorf("store involvement %d: %w", km.KillmailID, err)
			}
		}
	}
	if inserted {
		// Attacker rows land on every first copy, a kill past
		// retention included: the stats fold reads them for ships
		// flown, and the prune removes them only once the kill is
		// counted.
		ap := killAttackersParams(km)
		if len(ap.Seqs) > 0 {
			if err := q.InsertKillAttackers(ctx, ap); err != nil {
				return false, fmt.Errorf("store attackers %d: %w", km.KillmailID, err)
			}
		}
	}
	if inserted && !prunedShape {
		if err := q.InsertKillDetail(ctx, db.InsertKillDetailParams{
			KillmailID: km.KillmailID, Payload: compressKillDetail(raw), StoredAt: time.Now().UTC(),
		}); err != nil {
			return false, fmt.Errorf("store detail %d: %w", km.KillmailID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return inserted, err
	}
	if inserted {
		// The ring entry goes in before pricing: priceKillLines
		// finishes by calling noteRecentKillPrice, which can only
		// fill the price into an entry that already exists.
		if public {
			app.noteRecentKill(ctx, db.Kill{
				KillmailID:          km.KillmailID,
				Hash:                hash,
				KillmailTime:        km.KillmailTime.UTC(),
				SystemID:            km.SolarSystemID,
				RegionID:            p.RegionID,
				LocationID:          p.LocationID,
				VictimCharacterID:   km.Victim.CharacterID,
				VictimCorporationID: km.Victim.CorporationID,
				VictimAllianceID:    km.Victim.AllianceID,
				VictimFactionID:     km.Victim.FactionID,
				ShipTypeID:          km.Victim.ShipTypeID,
				DamageTaken:         km.Victim.DamageTaken,
				AttackerCount:       int32(len(km.Attackers)),
				FbCharacterID:       p.FbCharacterID,
				FbCorporationID:     p.FbCorporationID,
				FbAllianceID:        p.FbAllianceID,
				FbShipTypeID:        p.FbShipTypeID,
				Npc:                 p.Npc,
				Solo:                p.Solo,
				Awox:                p.Awox,
				Points:              p.Points,
				Source:              source,
				Public:              public,
				IngestedAt:          p.IngestedAt,
			})
		}
		// Priced once, here at ingest: the kill's lines are
		// appraised and stored, and renders read the stored
		// prices instead of repricing per request. A pricing
		// failure leaves the kill for the pricing pass, which
		// covers kills held from before line prices existed.
		if err := app.priceKillLines(ctx, km); err != nil {
			logging.Warnf("killboard: price kill %d at ingest: %v", km.KillmailID, err)
		}
	} else if public && !wasPublic {
		// A kill already stored in private shape is now public:
		// fold it from its stored row. Its queue row keeps this to
		// once.
		if err := app.foldKillStats(ctx, km.KillmailID); err != nil {
			logging.Warnf("killboard stats: fold public kill %d: %v", km.KillmailID, err)
		}
	}
	return inserted, nil
}

// priceKillLines appraises one kill's lines once and stores
// the per-line prices beside the kill's totals.
func (app *Application) priceKillLines(ctx context.Context, km killmailFull) error {
	lines := killLines(km)
	types := map[int64]bool{}
	for _, l := range lines {
		types[l.typeID] = true
	}
	price, err := app.killUnitPrices(ctx, types)
	if err != nil {
		return err
	}
	return app.storeKillPrices(ctx, km.KillmailID, km.KillmailTime, lines, price, time.Now().UTC())
}

// storeKillPrices persists one kill's totals from an appraisal's
// unit prices, and its per-line prices only while the kill is
// within retention. Past two years a kill is metadata for intel
// only, so its lines are never stored; the total still lands on
// the kills row, which is what per-player value stats read.
func (app *Application) storeKillPrices(ctx context.Context, killmailID int64, killTime time.Time, lines []killLine, price map[int64]float64, now time.Time) error {
	if !killTime.Before(now.Add(-killDetailRetention)) {
		var typeIDs []int64
		var unitPrices []float64
		for _, l := range lines {
			if p, ok := price[l.typeID]; ok {
				typeIDs = append(typeIDs, l.typeID)
				unitPrices = append(unitPrices, p)
			}
		}
		if len(typeIDs) > 0 {
			if err := app.queries.InsertKillLinePrices(ctx, db.InsertKillLinePricesParams{
				KillmailID: killmailID, TypeIds: typeIDs, UnitPrices: unitPrices,
			}); err != nil {
				return err
			}
		}
	}
	total, dropped, cov := killValue(lines, price)
	if err := app.queries.SetKillValue(ctx, db.SetKillValueParams{
		KillmailID: killmailID, ValueOurs: total, ValueDroppedOurs: dropped,
		ValueCoverage: float32(cov), PricedAt: now,
	}); err != nil {
		return err
	}
	app.noteRecentKillPrice(ctx, killmailID, total, dropped, float64(cov), now)
	// The priced value is final for this kill: fold it into the
	// stored entity statistics the pages read. A fold failure is
	// left for the maintenance sweep, never failed pricing.
	if err := app.foldKillStats(ctx, killmailID); err != nil {
		logging.Warnf("killboard stats: fold kill %d at pricing: %v", killmailID, err)
	}
	return nil
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
// characters' ESI killmails, offer any pre-permanence pilot
// profiles their restoration walk, price what is unpriced, and
// once a day prune details past retention.
func (app *Application) killboardMaintenance(ctx context.Context) {
	if rows, err := app.queries.ListESIKillsToMerge(ctx, killMergeBatch); err == nil {
		merged := 0
		for _, r := range rows {
			if ok, err := app.ingestKill(ctx, json.RawMessage(r.Payload), r.Hash, nil, "esi", false, true); err != nil {
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
	app.backfillKillStats(ctx)
	app.backfillMissingKillFeeds(ctx)
	app.refreshKillboardHeadline(ctx)
	app.offerPilotRestorations(ctx)
	app.warmKillNames(ctx)
	app.resolveNotedNames(ctx)
	if n, err := app.priceKills(ctx); err != nil {
		logging.Errorf("killboard: price: %v", err)
	} else if n > 0 {
		logging.Infof("killboard: priced %d kills", n)
	}
	app.publishKillboardStats(ctx)
	// The retention prune waits for the stats rebuild: the attacker
	// rows the restorer reads back (killboard_restore.go) must
	// stay until the rebuild has counted their ships.
	if time.Since(app.killLastPruned) >= 24*time.Hour && app.killStatsRebuilt(ctx) {
		app.killLastPruned = time.Now()
		before := time.Now().UTC().Add(-killDetailRetention)
		d, err1 := app.queries.PruneKillDetails(ctx, before)
		a, err2 := app.queries.PruneKillAttackers(ctx, before)
		l, err4 := app.queries.PruneKillLinePrices(ctx, before)
		_, err3 := app.queries.PrunePilotActivityApplied(ctx, before)
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			logging.Errorf("killboard: prune: %v", err)
		} else if d+a+l > 0 {
			logging.Infof("killboard: pruned %d killmails, %d attacker rows and %d priced lines past two years", d, a, l)
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

// KillmailLossValue values one loss the way the killboard values
// kills: its stored value when the killboard priced it, else the
// same appraisal over the payload's lines.
func (app *Application) KillmailLossValue(ctx context.Context, killmailID int64, payload string) float64 {
	if k, err := app.queries.GetKill(ctx, killmailID); err == nil && k.ValueOurs.Valid {
		return k.ValueOurs.Float64
	}
	var km killmailFull
	if json.Unmarshal([]byte(payload), &km) != nil {
		return 0
	}
	lines := killLines(km)
	types := make(map[int64]bool, len(lines))
	for _, l := range lines {
		types[l.typeID] = true
	}
	price, err := app.killUnitPrices(ctx, types)
	if err != nil {
		logging.Errorf("killboard: price loss %d: %v", killmailID, err)
		return 0
	}
	total, _, _ := killValue(lines, price)
	return total
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
		at    time.Time
		lines []killLine
	}
	var work []pending
	types := map[int64]bool{}
	for _, r := range rows {
		raw, err := killDetailRaw(r.Payload)
		if err != nil {
			continue
		}
		var km killmailFull
		if goccyjson.Unmarshal(raw, &km) != nil {
			continue
		}
		ls := killLines(km)
		for _, l := range ls {
			types[l.typeID] = true
		}
		work = append(work, pending{id: r.KillmailID, at: km.KillmailTime, lines: ls})
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
		if err := app.storeKillPrices(ctx, w.id, w.at, w.lines, price, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
