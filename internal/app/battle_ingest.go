package app

// ---------------------------------------------------------------------------
// Battle reports, ingest: game logs in, fights out.
//
//	POST /intel/battles/upload     a game log file, or part of one pasted
//	POST /api/intel/v1/gamelog     the game log lines Axon tails and sends
//
// The site reads the log (internal/gamelog): its combat lines, cut
// into fights, each one's frame (listener, window, session file),
// per-second series and one tally per other party. The text is read
// as it arrives and dropped; only the fights are kept. A fight is
// stored for the signed-in account, or the account whose Axon
// credential signed the post, and only for one of that account's
// own characters.
//
// Storing is idempotent on (character, start): a fight read again
// replaces its frame, series and tallies and keeps its share link.
// Correlation with killmails (battle_correlate.go) runs right after,
// and again from the worker while late mails can still arrive.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/gamelog"
	"evesynapse/internal/logging"
)

const (
	battleMaxParties   = 500
	battleMaxWeapons   = 40
	battleMaxTackles   = 300
	battleMaxNameLen   = 100
	battleMaxAge       = 90 * 24 * time.Hour
	battleDPSWindow    = 10 // seconds averaged for peak incoming DPS
	battleSliceSeconds = gamelog.SliceSeconds
)

// battleSlice is one battleSliceSeconds step of a fight: per party
// (its index in the stored parties), damage out and in, repairs out
// and in, and capacitor pressure in.
type battleSlice struct {
	At   int        `json:"at"`
	Rows [][6]int64 `json:"rows"`
}

// sdeCategoryShip is the SDE's ship category.
const sdeCategoryShip = 6

// battleSeries is the stored series: the five per-second lines.
type battleSeries struct {
	DamageOut []int64 `json:"damage_out"`
	DamageIn  []int64 `json:"damage_in"`
	RepsOut   []int64 `json:"reps_out"`
	RepsIn    []int64 `json:"reps_in"`
	CapIn     []int64 `json:"cap_in"`
}

// battleIngest is what one game log added to the battle reports.
type battleIngest struct {
	Stored, Updated int
	IDs             []int64  // the fights stored or updated, in log order
	Skipped         []string // why a fight was not stored, one sentence each
	Quiet           int      // fights with no player on the other side
}

func (b *battleIngest) add(o battleIngest) {
	b.Stored += o.Stored
	b.Updated += o.Updated
	b.IDs = append(b.IDs, o.IDs...)
	b.Skipped = append(b.Skipped, o.Skipped...)
	b.Quiet += o.Quiet
}

var errBattleNotOwn = errors.New("not a character on this account")

// ingestGameLog files a read game log's fights for one of the
// account's characters: the one its header or file name names, or
// pick when the text names none (a pasted part of a log). Each fight
// is stored and correlated with its killmails.
func (app *Application) ingestGameLog(ctx context.Context, userID int64, l gamelog.Log, pick int64) (battleIngest, error) {
	out := battleIngest{Quiet: l.Quiet}
	charID, listener := l.CharacterID, l.Listener
	if charID == 0 && listener == "" {
		charID = pick
	}
	charID, name, ok := app.accountCharacterFor(ctx, userID, charID, listener)
	if !ok {
		return out, errBattleNotOwn
	}
	now := time.Now().UTC()
	if over := len(l.Fights) - battleFightsPerLog; over > 0 {
		out.Skipped = append(out.Skipped, fmt.Sprintf("Only the first %d fights in one log are read; %d more were left out.", battleFightsPerLog, over))
		l.Fights = l.Fights[:battleFightsPerLog]
	}
	for i := range l.Fights {
		f := &l.Fights[i]
		f.Listener = name
		if why := fitFight(f, now); why != "" {
			out.Skipped = append(out.Skipped, fmt.Sprintf("The fight at %s EVE %s.", f.Start.Format("2006-01-02 15:04"), why))
			continue
		}
		id, inserted, err := app.storeFight(ctx, userID, charID, f, now)
		if err != nil {
			return out, err
		}
		if err := app.correlateFight(ctx, id); err != nil {
			logging.Errorf("battle reports: correlate fight %d: %v", id, err)
		}
		out.IDs = append(out.IDs, id)
		if inserted {
			out.Stored++
		} else {
			out.Updated++
		}
	}
	return out, nil
}

// fitFight bounds a fight read from a log to what a report stores,
// or says why it is not stored.
func fitFight(f *gamelog.Fight, now time.Time) string {
	switch {
	case f.Start.After(now.Add(10 * time.Minute)):
		return "starts in the future"
	case now.Sub(f.End) > battleMaxAge:
		return "is older than 90 days"
	}
	if len(f.Parties) > battleMaxParties {
		f.Parties = f.Parties[:battleMaxParties]
		for i := range f.Slices {
			rows := f.Slices[i].Rows[:0]
			for _, r := range f.Slices[i].Rows {
				if r[0] < battleMaxParties {
					rows = append(rows, r)
				}
			}
			f.Slices[i].Rows = rows
		}
	}
	if len(f.Tackles) > battleMaxTackles {
		f.Tackles = f.Tackles[:battleMaxTackles]
	}
	for i := range f.Tackles {
		t := &f.Tackles[i]
		t.From, t.To = truncate(t.From, battleMaxNameLen), truncate(t.To, battleMaxNameLen)
	}
	for i := range f.Parties {
		p := &f.Parties[i]
		p.Name, p.Hull, p.Ticker = truncate(p.Name, battleMaxNameLen), truncate(p.Hull, battleMaxNameLen), truncate(p.Ticker, 16)
		if len(p.Weapons) > battleMaxWeapons {
			p.Weapons = p.Weapons[:battleMaxWeapons]
		}
		for j := range p.Weapons {
			p.Weapons[j].Weapon = truncate(p.Weapons[j].Weapon, battleMaxNameLen)
		}
	}
	return ""
}

// handleBattleUpload is POST /intel/battles/upload: one game log
// file (the page gzips it first), or a part of one pasted with the
// character it belongs to. It passes the battle log gate before its
// body is read (battle_limits.go); the text is read in memory and
// dropped.
func (app *Application) handleBattleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/intel/battles/")
	release, why, wait := battleLogGate.admit(userID, false, r.ContentLength, time.Now())
	if release == nil {
		back("Not read: " + why + ". Try again " + battleWaitText(wait) + ".")
		return
	}
	defer release()
	// A few MB on a slow uplink can outlast the server's 30-second
	// read limit; a signed-in upload gets two minutes.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(2 * time.Minute))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, battleWireMax+64<<10))
	if err != nil {
		back(battleReadProblem(err))
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		back("Choose a game log file or paste one.")
		return
	}
	var total battleIngest
	msg := ""
	err = battleLogGate.read(ctx, func() error {
		var err error
		total, msg, err = app.readUpload(ctx, userID, multipart.NewReader(bytes.NewReader(body), params["boundary"]))
		return err
	})
	switch {
	case errors.Is(err, errBattleBusy):
		back("The site is reading other logs right now. Try again in a minute.")
		return
	case err != nil:
		logging.Errorf("battle reports: upload for user %d: %v", userID, err)
		back("Could not store the fights; try again.")
		return
	case msg != "":
		back(msg)
		return
	}
	msg = battleIngestText(total)
	if len(total.IDs) == 1 {
		app.flash(ctx, msg)
		http.Redirect(w, r, fmt.Sprintf("/intel/battle/%d/", total.IDs[0]), http.StatusSeeOther)
		return
	}
	back(msg)
}

// readUpload reads the form's parts and files their fights. A
// problem with what was sent comes back as a sentence; err is the
// site's own failure.
func (app *Application) readUpload(ctx context.Context, userID int64, mr *multipart.Reader) (battleIngest, string, error) {
	var total battleIngest
	var logs []gamelog.Log
	var pick int64
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, "Choose a game log file or paste one.", nil
		}
		switch part.FormName() {
		case "character":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			pick, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		case "log", "paste":
			session := ""
			if part.FormName() == "log" {
				if part.FileName() == "" {
					continue
				}
				session = filepath.Base(part.FileName())
			}
			seen := &blankReader{r: part}
			l, err := gamelog.Read(seen, session)
			if err != nil && !seen.text && part.FormName() == "paste" {
				continue // the paste box left empty
			}
			if err != nil {
				return total, battleReadProblem(err), nil
			}
			logs = append(logs, l)
		}
	}
	if len(logs) == 0 {
		return total, "Choose a game log file or paste one.", nil
	}
	for _, l := range logs {
		if l.Listener == "" && l.CharacterID == 0 && pick == 0 {
			return total, "Pick which of your characters the pasted log belongs to.", nil
		}
		res, err := app.ingestGameLog(ctx, userID, l, pick)
		if errors.Is(err, errBattleNotOwn) {
			who := l.Listener
			if who == "" {
				who = "That character"
			}
			return total, who + " is not a character on this account.", nil
		}
		if err != nil {
			return total, "", err
		}
		total.add(res)
	}
	return total, "", nil
}

// blankReader notes whether anything but white space went through.
type blankReader struct {
	r    io.Reader
	text bool
}

func (b *blankReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if !b.text && len(bytes.TrimSpace(p[:n])) > 0 {
		b.text = true
	}
	return n, err
}

// battleReadProblem says why a log could not be read.
func battleReadProblem(err error) string {
	var big *http.MaxBytesError
	switch {
	case errors.As(err, &big):
		return fmt.Sprintf("That is over %d MB to send. Choose the file rather than pasting it, so the page can compress it first.", battleWireMax>>20)
	case errors.Is(err, gamelog.ErrTooLarge):
		return "That log is over 32 MB."
	case errors.Is(err, gamelog.ErrNotGameLog):
		return "That is not a game log: its lines start with a time stamp like [ 2026.11.04 12:00:05 ] (combat)."
	}
	return "Could not read the log; try again."
}

// battleWaitText is when to try again, in words.
func battleWaitText(d time.Duration) string {
	if m := int((d + time.Minute - 1) / time.Minute); m > 1 {
		return fmt.Sprintf("in %d minutes", m)
	}
	return "in a minute"
}

// battleIngestText is the upload's result in one or two sentences.
func battleIngestText(t battleIngest) string {
	var parts []string
	switch {
	case t.Stored > 0 && t.Updated > 0:
		parts = append(parts, fmt.Sprintf("Stored %s, updated %s.", pluralCount(int64(t.Stored), "new fight", "new fights"), pluralCount(int64(t.Updated), "fight", "fights")))
	case t.Stored > 0:
		parts = append(parts, fmt.Sprintf("Stored %s.", pluralCount(int64(t.Stored), "new fight", "new fights")))
	case t.Updated > 0:
		parts = append(parts, fmt.Sprintf("Updated %s already here.", pluralCount(int64(t.Updated), "fight", "fights")))
	case len(t.Skipped) == 0 && t.Quiet > 0:
		parts = append(parts, "No fights with another player in that log, only NPCs.")
	case len(t.Skipped) == 0:
		parts = append(parts, "No combat in that log.")
	}
	return strings.Join(append(parts, t.Skipped...), " ")
}

// battleAPIGate is the battle log gate in front of Axon's posts.
func battleAPIGate(w http.ResponseWriter, r *http.Request, userID int64) (func(), bool) {
	release, why, wait := battleLogGate.admit(userID, true, r.ContentLength, time.Now())
	if release == nil {
		retryAfter(w, wait)
		intelAPIError(w, http.StatusTooManyRequests, why)
		return nil, false
	}
	return release, true
}

// handleAPIGameLog is POST /api/intel/v1/gamelog?session=&listener=:
// the game log text Axon read since its last post, gzipped, sent
// once the log has gone quiet so a fight never spans two posts.
// listener names the pilot when the text starts after the header.
func (app *Application) handleAPIGameLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := tokenUser(ctx)
	q := r.URL.Query()
	var res battleIngest
	var l gamelog.Log
	var readErr error
	err := battleLogGate.read(ctx, func() error {
		l, readErr = gamelog.Read(r.Body, filepath.Base(strings.TrimSpace(q.Get("session"))))
		if readErr != nil {
			return nil
		}
		if l.Listener == "" {
			l.Listener = truncate(strings.TrimSpace(q.Get("listener")), battleMaxNameLen)
		}
		if l.Listener == "" && l.CharacterID == 0 {
			readErr = errBattleNotOwn
			return nil
		}
		var err error
		res, err = app.ingestGameLog(ctx, userID, l, 0)
		return err
	})
	switch {
	case errors.Is(err, errBattleBusy):
		retryAfter(w, time.Minute)
		intelAPIError(w, http.StatusServiceUnavailable, "the site is reading other logs: try again in a minute")
		return
	case errors.Is(readErr, gamelog.ErrTooLarge):
		intelAPIError(w, http.StatusRequestEntityTooLarge, "the text unpacks to over 32 MB")
		return
	case errors.Is(readErr, errBattleNotOwn):
		intelAPIError(w, http.StatusBadRequest, "listener is missing")
		return
	case readErr != nil:
		intelAPIError(w, http.StatusBadRequest, "the text holds no game log lines")
		return
	case errors.Is(err, errBattleNotOwn):
		intelAPIError(w, http.StatusForbidden, fmt.Sprintf("%s is not a character on this account", l.Listener))
		return
	case err != nil:
		logging.Errorf("battle reports: game log for user %d: %v", userID, err)
		intelAPIError(w, http.StatusInternalServerError, "could not store the fights")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Fights  []int64  `json:"fights"`
		Stored  int      `json:"stored"`
		Updated int      `json:"updated"`
		Quiet   int      `json:"quiet"`
		Skipped []string `json:"skipped,omitempty"`
	}{res.IDs, res.Stored, res.Updated, res.Quiet, res.Skipped})
}

// accountCharacterFor resolves a log's listener to one of the
// account's characters: by the session file's character ID when it
// has one, else by name. It returns the character's ID and name.
func (app *Application) accountCharacterFor(ctx context.Context, userID, charID int64, listener string) (int64, string, bool) {
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return 0, "", false
	}
	for _, c := range chars {
		if charID > 0 && c.CharacterID == charID {
			return c.CharacterID, c.Name, true
		}
	}
	for _, c := range chars {
		if listener != "" && strings.EqualFold(c.Name, listener) {
			return c.CharacterID, c.Name, true
		}
	}
	return 0, "", false
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

// storeFight writes the fight, its series and its tallies in one
// transaction.
func (app *Application) storeFight(ctx context.Context, userID, charID int64, f *gamelog.Fight, now time.Time) (int64, bool, error) {
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
