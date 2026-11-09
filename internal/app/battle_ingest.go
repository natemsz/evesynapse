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

// ingestGameLog files a read game log's fights for one of the
// account's characters, already resolved from the log's header, its
// file name or the pilot picked for a paste. Each fight is stored
// and correlated with its killmails.
func (app *Application) ingestGameLog(ctx context.Context, userID, charID int64, name string, l gamelog.Log) (battleIngest, error) {
	out := battleIngest{Quiet: l.Quiet}
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
// character it belongs to. It passes the queue's checks before its
// body is read (battle_queue.go), checks whose log it is, and sets
// it waiting; the page shows it until it is read.
func (app *Application) handleBattleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/intel/battles/")
	q := app.battleQueue()
	if why, wait := q.admit(userID, false, r.ContentLength, time.Now()); why != "" {
		back("Not read: " + why + ". Try again " + battleWaitText(wait) + ".")
		return
	}
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
	jobs, msg := app.uploadJobs(ctx, userID, multipart.NewReader(bytes.NewReader(body), params["boundary"]))
	if msg != "" {
		back(msg)
		return
	}
	var names []string
	ahead := 0
	for _, j := range jobs {
		n, why, wait := q.enqueue(j, time.Now())
		if why != "" {
			back("Not read: " + why + ". Try again " + battleWaitText(wait) + ".")
			return
		}
		names = append(names, j.name)
		ahead = n
	}
	msg = strings.Join(names, " and ") + " is waiting to be read; its fights show here when it is."
	if ahead == 0 {
		msg = strings.Join(names, " and ") + " is being read; its fights show here in a moment."
	}
	back(msg)
}

// uploadJobs reads the form's parts into jobs, each checked for
// whose log it is. A problem comes back as a sentence.
func (app *Application) uploadJobs(ctx context.Context, userID int64, mr *multipart.Reader) ([]*battleJob, string) {
	var parts []*battleJob
	var pick int64
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "Choose a game log file or paste one."
		}
		switch part.FormName() {
		case "character":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			pick, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		case "log", "paste":
			j := &battleJob{userID: userID, name: "Pasted log"}
			if part.FormName() == "log" {
				if part.FileName() == "" {
					continue
				}
				j.session = strings.TrimSuffix(filepath.Base(part.FileName()), ".gz")
				j.name = j.session
			}
			b, err := io.ReadAll(io.LimitReader(part, battleWireMax+1))
			if err != nil {
				return nil, "Could not read the upload; try again."
			}
			if part.FormName() == "paste" && len(bytes.TrimSpace(b)) == 0 {
				continue // the paste box left empty
			}
			j.body = b
			parts = append(parts, j)
		}
	}
	if len(parts) == 0 {
		return nil, "Choose a game log file or paste one."
	}
	for _, j := range parts {
		listener := gamelog.Listener(j.body)
		charID := gamelog.SessionCharacter(j.session)
		if listener == "" && charID == 0 {
			if pick == 0 {
				return nil, "Pick which of your characters the pasted log belongs to."
			}
			charID = pick
		}
		id, name, ok := app.accountCharacterFor(ctx, userID, charID, listener)
		if !ok {
			who := listener
			if who == "" {
				who = "That character"
			}
			return nil, who + " is not a character on this account."
		}
		j.charID, j.listener = id, name
	}
	return parts, ""
}

// readBattleJob reads one queued log and files its fights.
func (app *Application) readBattleJob(ctx context.Context, j *battleJob) battleResult {
	l, err := gamelog.Read(bytes.NewReader(j.body), j.session)
	res := battleResult{read: l.Text}
	if err != nil {
		res.Failed, res.Text = true, battleReadProblem(err)
		return res
	}
	got, err := app.ingestGameLog(ctx, j.userID, j.charID, j.listener, l)
	if err != nil {
		logging.Errorf("battle reports: read %s for user %d: %v", j.name, j.userID, err)
		res.Failed, res.Text = true, "Could not store the fights; send the log again."
		return res
	}
	res.Text, res.IDs = battleIngestText(got), got.IDs
	return res
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
	return "Could not read the log; send it again."
}

// battleWaitText is when to try again, in words.
func battleWaitText(d time.Duration) string {
	if m := int((d + time.Minute - 1) / time.Minute); m > 1 {
		return fmt.Sprintf("in %d minutes", m)
	}
	return "in a minute"
}

// battleIngestText is a read log's result in one or two sentences.
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
		parts = append(parts, "No fights with another player, only NPCs.")
	case len(t.Skipped) == 0:
		parts = append(parts, "No combat in it.")
	}
	return strings.Join(append(parts, t.Skipped...), " ")
}

// battleAPIGate runs the queue's checks on an Axon post before its
// body is read.
func (app *Application) battleAPIGate(w http.ResponseWriter, r *http.Request, userID int64) (func(), bool) {
	if why, wait := app.battleQueue().admit(userID, true, r.ContentLength, time.Now()); why != "" {
		retryAfter(w, wait)
		intelAPIError(w, http.StatusTooManyRequests, why)
		return nil, false
	}
	return func() {}, true
}

// handleAPIGameLog is POST /api/intel/v1/gamelog?session=&listener=:
// the game log text Axon read since its last post, gzipped, sent
// once the log has gone quiet so a fight never spans two posts.
// listener names the pilot when the text starts after the header.
// The text waits in the queue; the answer says where.
func (app *Application) handleAPIGameLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := tokenUser(ctx)
	q := r.URL.Query()
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		intelAPIError(w, http.StatusBadRequest, "send gzipped game log text")
		return
	}
	j := &battleJob{userID: userID, session: strings.TrimSuffix(filepath.Base(strings.TrimSpace(q.Get("session"))), ".gz"), body: body}
	j.name = j.session
	listener := gamelog.Listener(body)
	if listener == "" {
		listener = truncate(strings.TrimSpace(q.Get("listener")), battleMaxNameLen)
	}
	charID := gamelog.SessionCharacter(j.session)
	if listener == "" && charID == 0 {
		intelAPIError(w, http.StatusBadRequest, "listener is missing")
		return
	}
	id, name, ok := app.accountCharacterFor(ctx, userID, charID, listener)
	if !ok {
		intelAPIError(w, http.StatusForbidden, fmt.Sprintf("%s is not a character on this account", listener))
		return
	}
	j.charID, j.listener = id, name
	ahead, why, wait := app.battleQueue().enqueue(j, time.Now())
	if why != "" {
		retryAfter(w, wait)
		intelAPIError(w, http.StatusServiceUnavailable, why)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Queued bool `json:"queued"`
		Ahead  int  `json:"ahead"`
	}{true, ahead})
}

// retryAfter sets Retry-After in whole seconds, at least one.
func retryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(d.Seconds()+0.999))))
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

	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
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
	if err := tx.Commit(ctx); err != nil {
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
