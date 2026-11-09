package app

// ---------------------------------------------------------------------------
// Battle reports, correlation: a fight joined to its killmails.
//
// A mail belongs to a fight when the fight's character is on it, as
// victim or attacker, its time falls inside the fight's window with
// battleMailMargin either side, and its system agrees with the
// fight's (the matched mails settle the system first; the
// character's stored location stands in when no mail does and the
// location was read near the fight's end).
//
// Correlation runs when a fight is posted and again from the worker
// for every fight still inside battleLateMails, so a mail that lands
// after its fight still joins it. Each run rewrites the fight's
// matched kills, its parties' matches, its chart and its findings,
// all at once: the report page only reads them.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	battleMailMargin     = 120 * time.Second
	battleLateMails      = 72 * time.Hour
	battleCorrelateBatch = 100
	battleLocationFresh  = 15 * time.Minute
)

// correlateFight joins one fight to its mails and stores what the
// report shows.
func (app *Application) correlateFight(ctx context.Context, id int64) error {
	f, err := app.queries.GetBattleFight(ctx, id)
	if err != nil {
		return err
	}
	srow, err := app.queries.GetBattleFightSeries(ctx, id)
	if err != nil {
		return err
	}
	var series battleSeries
	if err := json.Unmarshal([]byte(srow.Series), &series); err != nil {
		return fmt.Errorf("decode series: %w", err)
	}
	var slices []battleSlice
	if srow.Slices != "" {
		if err := json.Unmarshal([]byte(srow.Slices), &slices); err != nil {
			return fmt.Errorf("decode slices: %w", err)
		}
	}
	tackles, err := app.queries.ListBattleFightTackles(ctx, id)
	if err != nil {
		return err
	}
	parties, err := app.queries.ListBattleFightParties(ctx, id)
	if err != nil {
		return err
	}
	weapons, err := app.queries.ListBattleFightWeapons(ctx, id)
	if err != nil {
		return err
	}
	mails, err := app.queries.ListKillsForFight(ctx, db.ListKillsForFightParams{
		CharacterID: f.CharacterID,
		FromAt:      f.StartedAt.Add(-battleMailMargin),
		ToAt:        f.EndedAt.Add(battleMailMargin),
	})
	if err != nil {
		return err
	}

	// The system: the matched mails' own, else the stored location.
	systemID, systemFrom := f.SystemID, f.SystemFrom
	if systemFrom != "mail" {
		systemID, systemFrom = 0, ""
	}
	counts := map[int64]int{}
	for _, m := range mails {
		counts[m.SystemID]++
	}
	for sys, n := range counts {
		if n > counts[systemID] || (n == counts[systemID] && sys < systemID) {
			systemID, systemFrom = sys, "mail"
		}
	}
	if systemID == 0 {
		if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: f.CharacterID, Kind: esi.SnapLocation}); err == nil {
			d := snap.FetchedAt.Sub(f.EndedAt)
			if d < 0 {
				d = -d
			}
			var loc esi.Location
			if d <= battleLocationFresh && json.Unmarshal([]byte(snap.Payload), &loc) == nil && loc.SolarSystemID > 0 {
				systemID, systemFrom = loc.SolarSystemID, "location"
			}
		}
	}
	kept := mails[:0]
	for _, m := range mails {
		if systemID == 0 || m.SystemID == systemID {
			kept = append(kept, m)
		}
	}
	mails = kept

	// Who is who on the mails: names to characters and hulls.
	ids := make([]int64, 0, len(mails))
	for _, m := range mails {
		ids = append(ids, m.KillmailID)
	}
	type onMail struct {
		charID, shipID int64
	}
	byName := map[string]onMail{}
	if len(ids) > 0 {
		atk, err := app.queries.ListKillAttackerCharacters(ctx, ids)
		if err != nil {
			return err
		}
		for _, a := range atk {
			if name, ok := app.resolvedCharacterName(ctx, a.CharacterID); ok && name != "" {
				byName[strings.ToLower(name)] = onMail{a.CharacterID, a.ShipTypeID}
			}
		}
	}
	victimName := map[int64]string{}
	for _, m := range mails {
		if m.VictimCharacterID == 0 {
			continue
		}
		if name, ok := app.resolvedCharacterName(ctx, m.VictimCharacterID); ok && name != "" {
			victimName[m.KillmailID] = name
			byName[strings.ToLower(name)] = onMail{m.VictimCharacterID, m.ShipTypeID}
		}
	}
	died := map[int64]bool{}
	for _, m := range mails {
		if m.VictimCharacterID > 0 && m.VictimCharacterID != f.CharacterID {
			died[m.VictimCharacterID] = true
		}
	}
	for i := range parties {
		p := &parties[i]
		if hit, ok := byName[strings.ToLower(p.Name)]; ok {
			p.CharacterID = hit.charID
			p.Died = died[hit.charID]
			if p.HullTypeID == 0 {
				p.HullTypeID = hit.shipID
			}
		} else {
			p.CharacterID, p.Died = 0, false
		}
	}

	// The kills, placed in the fight's timeline.
	firstIn := -1
	for i, v := range series.DamageIn {
		if v > 0 {
			firstIn = i
			break
		}
	}
	var kills []db.BattleFightKill
	var nKills, nLosses int32
	var iskDestroyed, iskLost float64
	for _, m := range mails {
		k := db.BattleFightKill{
			FightID: id, KillmailID: m.KillmailID, KillmailTime: m.KillmailTime.UTC(),
			AtSecond:          int32(m.KillmailTime.Sub(f.StartedAt) / time.Second),
			Loss:              m.VictimCharacterID == f.CharacterID,
			VictimCharacterID: m.VictimCharacterID, VictimName: victimName[m.KillmailID],
			ShipTypeID: m.ShipTypeID, DamageTaken: m.DamageTaken, MyDamage: m.MyDamage,
			TtkSeconds: -1, ValueIsk: m.ValueOurs,
		}
		if k.Loss {
			nLosses++
			if firstIn >= 0 {
				k.TtkSeconds = k.AtSecond - int32(firstIn)
			}
			if m.ValueOurs.Valid {
				iskLost += m.ValueOurs.Float64
			}
		} else {
			nKills++
			for _, p := range parties {
				if p.CharacterID > 0 && p.CharacterID == m.VictimCharacterID {
					if p.DamageOut > 0 {
						k.MyDamage = p.DamageOut // the log's own measure
					}
					k.TtkSeconds = int32(m.KillmailTime.Sub(p.FirstAt) / time.Second)
					break
				}
			}
			if m.ValueOurs.Valid {
				iskDestroyed += m.ValueOurs.Float64
			}
		}
		if k.TtkSeconds < -1 {
			k.TtkSeconds = -1
		}
		kills = append(kills, k)
	}
	outcome := "nokill"
	switch {
	case nLosses > 0:
		outcome = "loss"
	case nKills > 0:
		outcome = "kill"
	}

	ch := buildBattleChart(series, kills)
	ch.Hostile, ch.Friendly = buildSides(parties, weapons)
	ch.Weapons = aggregateWeapons(weapons)
	sparkTop := 0.0
	for _, r := range [][]float64{rolling(series.DamageOut), rolling(series.DamageIn)} {
		for _, v := range r {
			sparkTop = max(sparkTop, v)
		}
	}
	sparkOut, sparkIn := indexSpark(series.DamageOut, sparkTop), indexSpark(series.DamageIn, sparkTop)

	// The pilot's hull: the loss mail's, else the ship the
	// character was last read in, when that read was near the fight.
	var hullID int64
	hullFrom := ""
	for _, k := range kills {
		if k.Loss {
			hullID, hullFrom = k.ShipTypeID, "loss"
			break
		}
	}
	if hullID == 0 {
		if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: f.CharacterID, Kind: esi.SnapShip}); err == nil {
			d := snap.FetchedAt.Sub(f.EndedAt)
			if d < 0 {
				d = -d
			}
			var ship esi.Ship
			if d <= battleLocationFresh && json.Unmarshal([]byte(snap.Payload), &ship) == nil && ship.ShipTypeID > 0 {
				hullID, hullFrom = ship.ShipTypeID, "ship"
			}
		}
	}
	ships := map[int64]string{}
	for _, k := range kills {
		if _, ok := ships[k.ShipTypeID]; !ok && k.ShipTypeID > 0 {
			ships[k.ShipTypeID] = app.typeNameOrID(ctx, k.ShipTypeID)
		}
	}
	moments := buildMoments(momentInput{Fight: f, Series: series, Slices: slices, Parties: parties, Tackles: tackles, Kills: kills, Ships: ships})
	ch.Hover = buildChartHover(series, slices, parties, kills, ships)
	chart, err := json.Marshal(ch)
	if err != nil {
		return err
	}
	capacity := 0.0
	if nLosses > 0 {
		for _, k := range kills {
			if k.Loss {
				if attrs, err := app.queries.ListSDETypeAttributes(ctx, k.ShipTypeID); err == nil {
					for _, a := range attrs {
						if a.AttributeID == 482 { // capacitor capacity
							capacity = a.Value
						}
					}
				}
				break
			}
		}
	}
	findings := battleFindings(battleFacts{
		Fight: f, Series: series, Parties: parties, Weapons: weapons, Kills: kills, Tackles: tackles, ShipCapacitor: capacity,
	})

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := app.queries.WithTx(tx)
	if err := q.DeleteBattleFightKills(ctx, id); err != nil {
		return err
	}
	for _, k := range kills {
		if err := q.InsertBattleFightKill(ctx, db.InsertBattleFightKillParams(k)); err != nil {
			return err
		}
	}
	for _, p := range parties {
		if err := q.SetBattleFightPartyMatch(ctx, db.SetBattleFightPartyMatchParams{
			CharacterID: p.CharacterID, Died: p.Died, HullTypeID: p.HullTypeID, FightID: id, Seq: p.Seq,
		}); err != nil {
			return err
		}
	}
	if err := q.SetBattleFightChart(ctx, db.SetBattleFightChartParams{FightID: id, Chart: string(chart)}); err != nil {
		return err
	}
	if err := q.DeleteBattleFightMoments(ctx, id); err != nil {
		return err
	}
	for i, m := range moments {
		detail, err := json.Marshal(m.Detail)
		if err != nil {
			return err
		}
		brief, err := json.Marshal(m.Brief)
		if err != nil {
			return err
		}
		if err := q.InsertBattleFightMoment(ctx, db.InsertBattleFightMomentParams{
			FightID: id, Seq: int32(i), Kind: m.Kind, AtSecond: int32(m.At), EndSecond: int32(m.End),
			Title: m.Title, Brief: string(brief), Detail: string(detail),
		}); err != nil {
			return err
		}
	}
	if err := q.DeleteBattleFightFindings(ctx, id); err != nil {
		return err
	}
	for i, fd := range findings {
		if err := q.InsertBattleFightFinding(ctx, db.InsertBattleFightFindingParams{FightID: id, Seq: int32(i), Rule: fd.Rule, Text: fd.Text}); err != nil {
			return err
		}
	}
	if err := q.SetBattleFightCorrelation(ctx, db.SetBattleFightCorrelationParams{
		SystemID: systemID, SystemFrom: systemFrom, HullTypeID: hullID, HullFrom: hullFrom, Kills: nKills, Losses: nLosses,
		IskDestroyed: iskDestroyed, IskLost: iskLost, Outcome: outcome, SparkOut: sparkOut, SparkIn: sparkIn,
		CorrelatedAt: timeSet(time.Now().UTC()), ID: id,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// correlateRecentFights is the worker's pass: every fight a late
// mail could still join is matched again, oldest match first.
func (app *Application) correlateRecentFights(ctx context.Context) {
	fights, err := app.queries.ListBattleFightsToCorrelate(ctx, db.ListBattleFightsToCorrelateParams{
		Since: time.Now().UTC().Add(-battleLateMails), RowLimit: battleCorrelateBatch,
	})
	if err != nil {
		logging.Errorf("worker: battle reports: list fights: %v", err)
		return
	}
	for _, f := range fights {
		if ctx.Err() != nil {
			return
		}
		if err := app.correlateFight(ctx, f.ID); err != nil {
			logging.Errorf("worker: battle reports: correlate fight %d: %v", f.ID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The chart: the report's DPS curve, laid out once here.

const (
	battleChartW      = 600.0
	battleChartH      = 160.0
	battleChartPoints = 300
)

type battleChart struct {
	Hover    []battleHoverPt   `json:"hover,omitempty"`
	Weapons  []battleWeaponAgg `json:"weapons,omitempty"`
	Hostile  battleSide        `json:"hostile"`
	Friendly battleSide        `json:"friendly"`
	Max      int64             `json:"max"`
	Lines    []battleChartLine `json:"lines"`
	Ticks    []battleChartTick `json:"ticks"`
	YTicks   []battleChartTick `json:"yticks"`
	Marks    []battleChartMark `json:"marks"`
}

// battleHoverPt is what the chart's crosshair shows at one of its
// columns: the time, the rates drawn there (10-second averages), and
// who led each way in the 10-second slice under it. Short keys: the
// page carries one per column.
type battleHoverPt struct {
	T     string      `json:"t"`
	Out   int64       `json:"o"`
	In    int64       `json:"i"`
	RepIn int64       `json:"r"`
	RepOt int64       `json:"g"`
	Cap   int64       `json:"c"`
	TopIn *hoverParty `json:"ti,omitempty"`
	TopOt *hoverParty `json:"to,omitempty"`
	TopRp *hoverParty `json:"tr,omitempty"`
	Mails []hoverMail `json:"m,omitempty"`
}

type hoverParty struct {
	Name string `json:"n"`
	Hull string `json:"h,omitempty"`
	Ship int64  `json:"s,omitempty"`
	Amt  int64  `json:"a"`
}

type hoverMail struct {
	Ship int64  `json:"s"`
	Loss bool   `json:"l,omitempty"`
	Text string `json:"t"`
}

// buildChartHover lays out one hover point per chart column, in the
// same buckets the chart's lines are drawn from.
func buildChartHover(s battleSeries, slices []battleSlice, parties []db.BattleFightParty, kills []db.BattleFightKill, ships map[int64]string) []battleHoverPt {
	n := len(s.DamageOut)
	if n == 0 {
		return nil
	}
	rOut, rIn, rRep, rRpo, rCap := rolling(s.DamageOut), rolling(s.DamageIn), rolling(s.RepsIn), rolling(s.RepsOut), rolling(s.CapIn)
	bySeq := map[int64]db.BattleFightParty{}
	for _, p := range parties {
		bySeq[int64(p.Seq)] = p
	}
	at := map[int]battleSlice{}
	for _, sl := range slices {
		at[sl.At] = sl
	}
	top := func(sl battleSlice, col int) *hoverParty {
		var best [6]int64
		found := false
		for _, r := range sl.Rows {
			if r[col] > best[col] {
				best, found = r, true
			}
		}
		if !found {
			return nil
		}
		p := bySeq[best[0]]
		return &hoverParty{Name: p.Name, Hull: p.Hull, Ship: p.HullTypeID, Amt: best[col]}
	}
	buckets := min(n, battleChartPoints)
	out := make([]battleHoverPt, 0, buckets)
	for i := 0; i < buckets; i++ {
		lo, hi := i*n/buckets, max((i+1)*n/buckets, i*n/buckets+1)
		var pt battleHoverPt
		for j := lo; j < hi && j < n; j++ {
			pt.Out, pt.In = max(pt.Out, int64(rOut[j])), max(pt.In, int64(rIn[j]))
			pt.RepIn, pt.RepOt, pt.Cap = max(pt.RepIn, int64(rRep[j])), max(pt.RepOt, int64(rRpo[j])), max(pt.Cap, int64(rCap[j]))
		}
		mid := (lo + hi) / 2
		pt.T = clockOffset(mid)
		if sl, ok := at[mid/battleSliceSeconds*battleSliceSeconds]; ok {
			pt.TopIn, pt.TopOt, pt.TopRp = top(sl, 2), top(sl, 1), top(sl, 4)
		}
		for _, k := range kills {
			if int(k.AtSecond) >= lo && int(k.AtSecond) < hi {
				who := k.VictimName
				if k.Loss {
					who = "You"
				} else if who == "" {
					who = "A pilot"
				}
				pt.Mails = append(pt.Mails, hoverMail{Ship: k.ShipTypeID, Loss: k.Loss, Text: fmt.Sprintf("%s lost a %s", who, ships[k.ShipTypeID])})
			}
		}
		out = append(out, pt)
	}
	return out
}

// battleWeaponAgg is one weapon's shots one way across every party
// it was used on, with its hit quality counts worst to best.
type battleWeaponAgg struct {
	Dir     string   `json:"dir"`
	Weapon  string   `json:"weapon"`
	TypeID  int64    `json:"type_id"`
	Parties int      `json:"parties"`
	Shots   int64    `json:"shots"`
	Damage  int64    `json:"damage"`
	Quality [7]int64 `json:"quality"`
}

// aggregateWeapons folds the per-party weapon rows into one row a
// weapon each way, most damage first.
func aggregateWeapons(ws []db.BattleFightWeapon) []battleWeaponAgg {
	idx := map[string]int{}
	var out []battleWeaponAgg
	for _, w := range ws {
		k := w.Dir + "\x00" + w.Weapon
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, battleWeaponAgg{Dir: w.Dir, Weapon: w.Weapon, TypeID: w.WeaponTypeID})
		}
		a := &out[i]
		a.Parties++
		a.Shots += w.Shots
		a.Damage += w.Damage
		for j, q := range []int64{w.QMiss, w.QGraze, w.QGlance, w.QHit, w.QPenetrate, w.QSmash, w.QWreck} {
			a.Quality[j] += q
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir > out[j].Dir
		}
		return out[i].Damage > out[j].Damage
	})
	return out
}

// battleSide is one side's composition: its hulls with how many of
// each were seen and lost, and its tickers.
type battleSide struct {
	Pilots  int           `json:"pilots"`
	Hulls   []battleHull  `json:"hulls,omitempty"`
	Tickers []battleCount `json:"tickers,omitempty"`
}

type battleHull struct {
	TypeID int64  `json:"type_id"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
	Lost   int    `json:"lost"`
}

type battleCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// buildSides sorts the parties into the two sides the log can tell
// apart: those who repaired the pilot or were repaired by them, and
// the pilots who shot the pilot, neutralized them, or took their
// fire from anything but a smartbomb (which hits fleetmates too).
func buildSides(parties []db.BattleFightParty, weapons []db.BattleFightWeapon) (hostile, friendly battleSide) {
	aimed := map[int32]bool{}
	for _, w := range weapons {
		if w.Dir == "out" && w.Damage > 0 && !strings.Contains(w.Weapon, "Smartbomb") {
			aimed[w.PartySeq] = true
		}
	}
	shotBy := map[int32]bool{}
	for _, w := range weapons {
		if w.Dir == "in" && w.Damage > 0 && !strings.Contains(w.Weapon, "Smartbomb") {
			shotBy[w.PartySeq] = true
		}
	}
	type acc struct {
		hulls   map[string]*battleHull
		tickers map[string]int
		side    *battleSide
	}
	h := acc{map[string]*battleHull{}, map[string]int{}, &hostile}
	f := acc{map[string]*battleHull{}, map[string]int{}, &friendly}
	for _, p := range parties {
		var a *acc
		switch {
		case p.RepsIn > 0 || p.RepsOut > 0:
			a = &f
		case p.Pilot && (shotBy[p.Seq] || aimed[p.Seq] || p.CapIn > 0):
			a = &h
		default:
			continue
		}
		if p.Hull == "" {
			continue
		}
		if p.Pilot {
			a.side.Pilots++
		}
		hl := a.hulls[p.Hull]
		if hl == nil {
			hl = &battleHull{TypeID: p.HullTypeID, Name: p.Hull}
			a.hulls[p.Hull] = hl
		}
		hl.Count++
		if p.Died {
			hl.Lost++
		}
		if p.Ticker != "" {
			a.tickers[p.Ticker]++
		}
	}
	for _, a := range []acc{h, f} {
		for _, hl := range a.hulls {
			a.side.Hulls = append(a.side.Hulls, *hl)
		}
		sort.Slice(a.side.Hulls, func(i, j int) bool {
			if a.side.Hulls[i].Count != a.side.Hulls[j].Count {
				return a.side.Hulls[i].Count > a.side.Hulls[j].Count
			}
			return a.side.Hulls[i].Name < a.side.Hulls[j].Name
		})
		for t, n := range a.tickers {
			a.side.Tickers = append(a.side.Tickers, battleCount{t, n})
		}
		sort.Slice(a.side.Tickers, func(i, j int) bool {
			if a.side.Tickers[i].Count != a.side.Tickers[j].Count {
				return a.side.Tickers[i].Count > a.side.Tickers[j].Count
			}
			return a.side.Tickers[i].Name < a.side.Tickers[j].Name
		})
	}
	return hostile, friendly
}

// indexSpark is a fight's activity for the list's trend column: one
// line of a series over the whole fight, 80 by 20.
func indexSpark(v []int64, top float64) string {
	r := rolling(v)
	n := len(r)
	if n == 0 || top <= 0 {
		return ""
	}
	const w, hgt, pts = 80.0, 20.0, 60
	buckets := min(n, pts)
	var b strings.Builder
	for i := 0; i < buckets; i++ {
		lo, hi := i*n/buckets, max((i+1)*n/buckets, i*n/buckets+1)
		val := 0.0
		for j := lo; j < hi && j < n; j++ {
			val = max(val, r[j])
		}
		x := 0.0
		if buckets > 1 {
			x = float64(i) / float64(buckets-1) * w
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.FormatFloat(round1(x), 'f', -1, 64))
		b.WriteByte(',')
		b.WriteString(strconv.FormatFloat(round1(hgt-val/top*hgt), 'f', -1, 64))
	}
	return b.String()
}

type battleChartLine struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Points string `json:"points"`
	Peak   int64  `json:"peak"`
}

type battleChartTick struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Label string  `json:"label"`
}

type battleChartMark struct {
	X          float64 `json:"x"`
	Loss       bool    `json:"loss"`
	KillmailID int64   `json:"killmail_id"`
	Label      string  `json:"label"`
}

// rolling is a series averaged over battleDPSWindow seconds.
func rolling(v []int64) []float64 {
	out := make([]float64, len(v))
	var sum int64
	for i := range v {
		sum += v[i]
		if i >= battleDPSWindow {
			sum -= v[i-battleDPSWindow]
		}
		out[i] = float64(sum) / float64(min(i+1, battleDPSWindow))
	}
	return out
}

func buildBattleChart(s battleSeries, kills []db.BattleFightKill) battleChart {
	n := len(s.DamageOut)
	var c battleChart
	if n == 0 {
		return c
	}
	defs := []struct {
		key, label string
		v          []int64
	}{
		{"out", "Damage out", s.DamageOut},
		{"in", "Damage in", s.DamageIn},
		{"repin", "Repairs in", s.RepsIn},
		{"repout", "Repairs out", s.RepsOut},
	}
	type line struct {
		key, label string
		r          []float64
	}
	var lines []line
	maxV := 0.0
	for _, d := range defs {
		var total int64
		for _, x := range d.v {
			total += x
		}
		if total == 0 && d.key != "out" && d.key != "in" {
			continue
		}
		r := rolling(d.v)
		for _, x := range r {
			maxV = max(maxV, x)
		}
		lines = append(lines, line{d.key, d.label, r})
	}
	top := niceCeil(maxV)
	c.Max = int64(top)
	xOf := func(sec float64) float64 {
		if n <= 1 {
			return 0
		}
		return round1(sec / float64(n-1) * battleChartW)
	}
	yOf := func(v float64) float64 {
		if top <= 0 {
			return battleChartH
		}
		return round1(battleChartH - v/top*battleChartH)
	}
	buckets := min(n, battleChartPoints)
	for _, l := range lines {
		var b strings.Builder
		peak := 0.0
		for i := 0; i < buckets; i++ {
			lo, hi := i*n/buckets, (i+1)*n/buckets
			v := 0.0
			for j := lo; j < max(hi, lo+1) && j < n; j++ {
				v = max(v, l.r[j])
			}
			peak = max(peak, v)
			sec := float64(lo)
			if buckets > 1 {
				sec = float64(i) * float64(n-1) / float64(buckets-1)
			}
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strconv.FormatFloat(xOf(sec), 'f', -1, 64))
			b.WriteByte(',')
			b.WriteString(strconv.FormatFloat(yOf(v), 'f', -1, 64))
		}
		c.Lines = append(c.Lines, battleChartLine{Key: l.key, Label: l.label, Points: b.String(), Peak: int64(peak)})
	}
	// The axes' labels sit in HTML beside and under the chart, at
	// even fractions of it, so they keep their size at any width:
	// five along the time axis, three up the rate axis (top first).
	for i := 0; i <= 4; i++ {
		sec := (n - 1) * i / 4
		c.Ticks = append(c.Ticks, battleChartTick{X: xOf(float64(sec)), Label: clockOffset(sec)})
	}
	for i := 2; i >= 0; i-- {
		v := top * float64(i) / 2
		c.YTicks = append(c.YTicks, battleChartTick{Y: yOf(v), Label: battleNum(int64(v))})
	}
	for _, k := range kills {
		sec := min(max(float64(k.AtSecond), 0), float64(n-1))
		label := "Kill at " + clockOffset(int(k.AtSecond))
		if k.Loss {
			label = "Your loss at " + clockOffset(int(k.AtSecond))
		}
		c.Marks = append(c.Marks, battleChartMark{X: xOf(sec), Loss: k.Loss, KillmailID: k.KillmailID, Label: label})
	}
	return c
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// niceCeil rounds a chart's top up to 1, 2 or 5 times a power of ten.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 10
	}
	p := 1.0
	for p*10 <= v {
		p *= 10
	}
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

// clockOffset is seconds into a fight as m:ss (negative before it).
func clockOffset(sec int) string {
	sign := ""
	if sec < 0 {
		sign, sec = "-", -sec
	}
	if sec >= 3600 {
		return fmt.Sprintf("%s%d:%02d:%02d", sign, sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%s%d:%02d", sign, sec/60, sec%60)
}

// battleNum is a whole number with thousands separators.
func battleNum(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
