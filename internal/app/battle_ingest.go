package app

// ---------------------------------------------------------------------------
// Battle reports, ingest: POST /api/intel/v1/fights.
//
// Axon reads the pilot's game logs, cuts them into fights and posts
// each one's aggregate, signed with its paired credential like every
// call on this API: the frame (listener, window, session file), the
// per-second series (damage out and in, repairs out and in,
// capacitor pressure in) and one tally per other party. The raw log
// never arrives. A fight is stored for the account whose credential
// signed it, and only for one of that account's own characters.
//
// Storing is idempotent on (character, start): a fight posted again
// replaces its frame, series and tallies and keeps its share link.
// Correlation with killmails (battle_correlate.go) runs right after,
// and again from the worker while late mails can still arrive.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	battleMaxBody     = 1 << 20
	battleMaxSeconds  = 2*60*60 + 1
	battleMaxParties  = 500
	battleMaxWeapons  = 40
	battleMaxNameLen  = 100
	battleMaxAge      = 30 * 24 * time.Hour
	battleDPSWindow   = 10 // seconds averaged for peak incoming DPS
	battleQualityKeys = "miss graze glance hit penetrate smash wreck"
)

// battleFightIn is a fight as Axon posts it (axon watch/fights.go).
type battleFightIn struct {
	Listener    string    `json:"listener"`
	CharacterID int64     `json:"character_id"`
	Session     string    `json:"session"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Series      struct {
		DamageOut []int64 `json:"damage_out"`
		DamageIn  []int64 `json:"damage_in"`
		RepsOut   []int64 `json:"reps_out"`
		RepsIn    []int64 `json:"reps_in"`
		CapIn     []int64 `json:"cap_in"`
	} `json:"series"`
	Parties []struct {
		Name      string    `json:"name"`
		Ticker    string    `json:"ticker"`
		Hull      string    `json:"hull"`
		Pilot     bool      `json:"pilot"`
		DamageOut int64     `json:"damage_out"`
		DamageIn  int64     `json:"damage_in"`
		RepsOut   int64     `json:"reps_out"`
		RepsIn    int64     `json:"reps_in"`
		CapOut    int64     `json:"cap_out"`
		CapIn     int64     `json:"cap_in"`
		First     time.Time `json:"first"`
		Last      time.Time `json:"last"`
		Weapons   []struct {
			Dir     string           `json:"dir"`
			Weapon  string           `json:"weapon"`
			Shots   int64            `json:"shots"`
			Damage  int64            `json:"damage"`
			Quality map[string]int64 `json:"quality"`
		} `json:"weapons"`
	} `json:"parties"`
	Tackles []struct {
		From  string    `json:"from"`
		To    string    `json:"to"`
		Kind  string    `json:"kind"`
		First time.Time `json:"first"`
		Count int64     `json:"count"`
	} `json:"tackles"`
	Slices   []battleSlice `json:"slices"`
	Lines    int           `json:"lines"`
	Unparsed int           `json:"unparsed"`
}

// battleSlice is one battleSliceSeconds step of a fight: per party
// (its index in the posted parties), damage out and in, repairs out
// and in, and capacitor pressure in.
type battleSlice struct {
	At   int        `json:"at"`
	Rows [][6]int64 `json:"rows"`
}

// sdeCategoryShip is the SDE's ship category.
const sdeCategoryShip = 6

const (
	battleSliceSeconds = 10
	battleMaxTackles   = 300
)

// battleSeries is the stored series: the five per-second lines.
type battleSeries struct {
	DamageOut []int64 `json:"damage_out"`
	DamageIn  []int64 `json:"damage_in"`
	RepsOut   []int64 `json:"reps_out"`
	RepsIn    []int64 `json:"reps_in"`
	CapIn     []int64 `json:"cap_in"`
}

// check says what is wrong with a posted fight, in words for the
// Axon log, or "".
func (f *battleFightIn) check(now time.Time) string {
	f.Listener = strings.TrimSpace(f.Listener)
	switch {
	case f.Listener == "" || len(f.Listener) > battleMaxNameLen:
		return "listener is missing"
	case f.Start.IsZero() || f.End.Before(f.Start):
		return "start and end are not a window"
	case f.Start.After(now.Add(10 * time.Minute)):
		return "the fight starts in the future: check this computer's clock"
	case now.Sub(f.End) > battleMaxAge:
		return "the fight is older than 30 days"
	case len(f.Parties) > battleMaxParties:
		return fmt.Sprintf("send up to %d parties", battleMaxParties)
	}
	n := int(f.End.Sub(f.Start)/time.Second) + 1
	if n > battleMaxSeconds {
		return "a fight covers at most two hours"
	}
	s := &f.Series
	for _, line := range [][]int64{s.DamageOut, s.DamageIn, s.RepsOut, s.RepsIn, s.CapIn} {
		if len(line) != n {
			return "every series holds one value per second of the window"
		}
		for _, v := range line {
			if v < 0 {
				return "series values are not negative"
			}
		}
	}
	if len(f.Tackles) > battleMaxTackles {
		return fmt.Sprintf("send up to %d tackle rows", battleMaxTackles)
	}
	for i := range f.Tackles {
		t := &f.Tackles[i]
		if t.From == "" || t.To == "" || len(t.From) > battleMaxNameLen || len(t.To) > battleMaxNameLen || (t.Kind != "scramble" && t.Kind != "disruption") || t.Count < 0 {
			return "a tackle row is malformed"
		}
	}
	if len(f.Slices) > battleMaxSeconds/battleSliceSeconds+1 {
		return "too many slices for the window"
	}
	for _, sl := range f.Slices {
		if sl.At < 0 || sl.At >= n || sl.At%battleSliceSeconds != 0 || len(sl.Rows) > len(f.Parties) {
			return "a slice lies outside the window"
		}
		for _, r := range sl.Rows {
			if r[0] < 0 || r[0] >= int64(len(f.Parties)) {
				return "a slice names a party that was not sent"
			}
			for _, v := range r[1:] {
				if v < 0 {
					return "slice values are not negative"
				}
			}
		}
	}
	for i := range f.Parties {
		p := &f.Parties[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" || len(p.Name) > battleMaxNameLen || len(p.Hull) > battleMaxNameLen || len(p.Ticker) > 16 {
			return "a party has no name, or a name too long"
		}
		if len(p.Weapons) > battleMaxWeapons {
			return fmt.Sprintf("send up to %d weapons a party", battleMaxWeapons)
		}
		if p.DamageOut < 0 || p.DamageIn < 0 || p.RepsOut < 0 || p.RepsIn < 0 || p.CapOut < 0 || p.CapIn < 0 {
			return "tallies are not negative"
		}
		for j := range p.Weapons {
			w := &p.Weapons[j]
			if (w.Dir != "out" && w.Dir != "in") || len(w.Weapon) > battleMaxNameLen || w.Shots < 0 || w.Damage < 0 {
				return "a weapon tally is malformed"
			}
			for k, v := range w.Quality {
				if !strings.Contains(battleQualityKeys, k) || v < 0 {
					return "a quality word is not one the log uses"
				}
			}
		}
	}
	return ""
}

// peakIncoming is the highest incoming DPS over battleDPSWindow
// seconds, and the second that window starts.
func peakIncoming(in []int64) (int64, int) {
	var best int64
	at := 0
	var sum int64
	for i, v := range in {
		sum += v
		if i >= battleDPSWindow {
			sum -= in[i-battleDPSWindow]
		}
		w := min(i+1, battleDPSWindow)
		if dps := sum / int64(w); dps > best {
			best, at = dps, i-w+1
		}
	}
	return best, at
}

func newShareToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleAPIFight is POST /api/intel/v1/fights.
func (app *Application) handleAPIFight(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := tokenUser(ctx)
	var f battleFightIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, battleMaxBody)).Decode(&f); err != nil {
		intelAPIError(w, http.StatusBadRequest, "send one fight as JSON, up to 1 MB")
		return
	}
	now := time.Now().UTC()
	if msg := f.check(now); msg != "" {
		intelAPIError(w, http.StatusBadRequest, msg)
		return
	}
	charID, ok := app.accountCharacterFor(ctx, userID, f.CharacterID, f.Listener)
	if !ok {
		intelAPIError(w, http.StatusForbidden, fmt.Sprintf("%s is not a character on this account", f.Listener))
		return
	}
	id, inserted, err := app.storeFight(ctx, userID, charID, &f, now)
	if err != nil {
		logging.Errorf("battle reports: store fight for user %d: %v", userID, err)
		intelAPIError(w, http.StatusInternalServerError, "could not store the fight")
		return
	}
	if err := app.correlateFight(ctx, id); err != nil {
		logging.Errorf("battle reports: correlate fight %d: %v", id, err)
	}
	status := "updated"
	if inserted {
		status = "stored"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}{id, status})
}

// accountCharacterFor is the account's character the fight's log
// belongs to: by the ID in the log's file name when that is one of
// the account's, else by the listener's name.
func (app *Application) accountCharacterFor(ctx context.Context, userID, charID int64, listener string) (int64, bool) {
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return 0, false
	}
	for _, c := range chars {
		if charID > 0 && c.CharacterID == charID {
			return c.CharacterID, true
		}
	}
	for _, c := range chars {
		if strings.EqualFold(c.Name, listener) {
			return c.CharacterID, true
		}
	}
	return 0, false
}

// storeFight writes the fight, its series and its tallies in one
// transaction.
func (app *Application) storeFight(ctx context.Context, userID, charID int64, f *battleFightIn, now time.Time) (int64, bool, error) {
	var names []string
	for _, p := range f.Parties {
		names = append(names, p.Name)
		if p.Hull != "" {
			names = append(names, p.Hull)
		}
		for _, w := range p.Weapons {
			if w.Weapon != "" {
				names = append(names, w.Weapon)
			}
		}
	}
	typeIDs := map[string]int64{}
	category := map[string]int64{}
	if len(names) > 0 {
		if rows, err := app.queries.ListBattleTypeCategories(ctx, names); err == nil {
			for _, t := range rows {
				typeIDs[t.Name] = t.TypeID
				category[t.Name] = t.CategoryID
			}
		}
	}
	// What the log's names are: a decorated party whose type is a
	// charge, a drone or a fighter is something a pilot launched,
	// not a pilot; a bare name that is a ship type is a ship the
	// log named by hull alone.
	for i := range f.Parties {
		p := &f.Parties[i]
		if p.Hull != "" {
			if c, ok := category[p.Hull]; ok && c != sdeCategoryShip {
				p.Pilot = false
			}
		} else if category[p.Name] == sdeCategoryShip {
			p.Hull = p.Name
		}
	}
	var dmgOut, dmgIn, repOut, repIn, capIn int64
	for i := range f.Series.DamageOut {
		dmgOut += f.Series.DamageOut[i]
		dmgIn += f.Series.DamageIn[i]
		repOut += f.Series.RepsOut[i]
		repIn += f.Series.RepsIn[i]
		capIn += f.Series.CapIn[i]
	}
	pilots := 0
	for _, p := range f.Parties {
		if p.Pilot {
			pilots++
		}
	}
	peak, peakAt := peakIncoming(f.Series.DamageIn)
	series, err := json.Marshal(battleSeries(f.Series))
	if err != nil {
		return 0, false, err
	}
	slices, err := json.Marshal(f.Slices)
	if err != nil {
		return 0, false, err
	}
	token, err := newShareToken()
	if err != nil {
		return 0, false, err
	}

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	q := app.queries.WithTx(tx)
	row, err := q.UpsertBattleFight(ctx, db.UpsertBattleFightParams{
		UserID: userID, CharacterID: charID, Listener: f.Listener, SessionFile: truncate(f.Session, 64),
		StartedAt: f.Start.UTC(), EndedAt: f.End.UTC(),
		DamageOut: dmgOut, DamageIn: dmgIn, RepsOut: repOut, RepsIn: repIn, CapIn: capIn,
		PeakInDps: peak, PeakInAt: int32(peakAt), Pilots: int32(pilots),
		Lines: int32(f.Lines), Unparsed: int32(f.Unparsed), ShareToken: token, ReceivedAt: now,
	})
	if err != nil {
		return 0, false, err
	}
	if err := q.UpsertBattleFightSeries(ctx, db.UpsertBattleFightSeriesParams{FightID: row.ID, Series: string(series), Slices: string(slices)}); err != nil {
		return 0, false, err
	}
	if err := q.DeleteBattleFightWeapons(ctx, row.ID); err != nil {
		return 0, false, err
	}
	if err := q.DeleteBattleFightParties(ctx, row.ID); err != nil {
		return 0, false, err
	}
	if err := q.DeleteBattleFightTackles(ctx, row.ID); err != nil {
		return 0, false, err
	}
	for i, t := range f.Tackles {
		if err := q.InsertBattleFightTackle(ctx, db.InsertBattleFightTackleParams{
			FightID: row.ID, Seq: int32(i), FromName: t.From, ToName: t.To, Kind: t.Kind,
			FirstAt: t.First.UTC(), Attempts: t.Count,
		}); err != nil {
			return 0, false, err
		}
	}
	for i, p := range f.Parties {
		first, last := p.First.UTC(), p.Last.UTC()
		if first.IsZero() {
			first = f.Start.UTC()
		}
		if last.Before(first) {
			last = first
		}
		params := db.InsertBattleFightPartyParams{
			FightID: row.ID, Seq: int32(i), Name: p.Name, Ticker: p.Ticker, Hull: p.Hull,
			HullTypeID: typeIDs[p.Hull], Pilot: p.Pilot,
			DamageOut: p.DamageOut, DamageIn: p.DamageIn, RepsOut: p.RepsOut, RepsIn: p.RepsIn,
			CapOut: p.CapOut, CapIn: p.CapIn, FirstAt: first, LastAt: last,
		}
		if err := q.InsertBattleFightParty(ctx, params); err != nil {
			return 0, false, err
		}
		for _, wt := range p.Weapons {
			qc := wt.Quality
			if err := q.InsertBattleFightWeapon(ctx, db.InsertBattleFightWeaponParams{
				FightID: row.ID, PartySeq: int32(i), Dir: wt.Dir, Weapon: wt.Weapon,
				WeaponTypeID: typeIDs[wt.Weapon], Shots: wt.Shots, Damage: wt.Damage,
				QMiss: qc["miss"], QGraze: qc["graze"], QGlance: qc["glance"], QHit: qc["hit"],
				QPenetrate: qc["penetrate"], QSmash: qc["smash"], QWreck: qc["wreck"],
			}); err != nil {
				return 0, false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return row.ID, row.Inserted, nil
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
