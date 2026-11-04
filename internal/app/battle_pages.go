package app

// ---------------------------------------------------------------------------
// Battle reports, pages (Intel section):
//
//	GET  /intel/battles/                         the account's fights
//	GET  /intel/battle/{id}/?m=<seq>             one report, for its owner
//	GET  /intel/battle/{id}/moment/{seq}/        one moment's panel (fragment)
//	POST /intel/battle/{id}/share                the share link on or off
//	GET  /intel/battle/s/{token}/?m=<seq>        a shared report, for anyone
//	GET  /intel/battle/s/{token}/moment/{seq}/   its moment panels
//
// Everything shown was stored at ingest and correlation
// (battle_ingest.go, battle_correlate.go, battle_moments.go); these
// handlers read a handful of bounded rows and format them. A moment
// link is a plain link to the report with ?m=; battle.js upgrades it
// to load the moment's panel in place.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// --- The list ---

type battlesView struct {
	Rows   []battleIndexRow
	Tiles  []battleTile
	Viewer map[int64]bool
}

type battleIndexRow struct {
	Href        string
	When        string
	At          time.Time
	SystemID    int64
	SystemName  string
	Listener    string
	CharacterID int64
	Length      string
	Outcome     string
	OutcomeText string
	Kills       int32
	Losses      int32
	Destroyed   string
	Lost        string
	SparkOut    string
	SparkIn     string
	Pilots      int32
}

type battleTile struct {
	Label, Value, Sub, Tone string
}

// --- One report ---

type battleView struct {
	ID          int64
	Base        string // the report's own path, owner or shared
	Owner       bool
	Shared      bool
	ShareURL    string
	ShareAction string
	Title       string
	When        string
	At          time.Time
	Length      string
	SystemID    int64
	SystemName  string
	SystemNote  string
	Listener    string
	CharacterID int64
	Hull        string
	HullTypeID  int64
	HullNote    string
	Outcome     string
	OutcomeText string
	Tiles       []battleTile

	Chart    battleChart
	HasChart bool
	Band     battleBand
	Lanes    []battleLane
	Moment   *battleMomentView
	Findings []battleFindingRow

	Kills      []battleKillRow
	ShotBy     battlePartyList
	Shot       battlePartyList
	Support    battlePartyList
	Neuts      battlePartyList
	WeaponsOut battleWeaponList
	WeaponsIn  battleWeaponList
	GridTackle []battleTackleRow
	GridMore   []battleTackleRow // the grid's tackle past the first rows, behind Show all
	GridTotal  int
	MyTackle   []battleTackleRow
	Hostile    battleSide
	Friendly   battleSide

	Lines    int32
	Unparsed int32
	Viewer   map[int64]bool
}

// battleBand is the selected moment's span over the chart, in the
// chart's own units.
type battleBand struct {
	X, W float64
	On   bool
}

type battleLane struct {
	Key   string
	Label string
	Marks []battleLaneMark
}

type battleLaneMark struct {
	Seq      int32
	Kind     string
	Left     float64 // percent of the fight
	Width    float64 // percent of the fight
	BandX    float64 // the chart band, chart units
	BandW    float64
	Title    string
	When     string
	Href     string
	Fragment string
	Current  bool
}

type battleMomentView struct {
	Seq       int32
	Kind      string
	KindLabel string
	Title     string
	Window    string
	Summary   string
	Figures   []battleTile
	Attackers battlePartyList
	Targets   battlePartyList
	Support   battlePartyList
	Neuts     battlePartyList
	Tackles   []battleTackleRow
	Kills     []battleKillRow
	SparkOut  string
	SparkIn   string
	SparkReps string
	MarkA     float64 // the window's start on the sparkline
	MarkB     float64 // and its width
	Prev      string
	Next      string
	PrevFrag  string
	NextFrag  string
	Position  string
	Viewer    map[int64]bool
}

type battleFindingRow struct {
	Rule  string
	Label string
	Text  string
	First bool // the first finding of its rule: it carries the label
}

// battleRowsShown is how many rows a long table shows before its
// Show all control.
const battleRowsShown = 8

// battlePartyList is a party table: the rows shown, and the rest
// behind the table's Show all control (open without script).
type battlePartyList struct {
	ID    string
	Top   []battlePartyRow
	More  []battlePartyRow
	Total int
}

type battleWeaponList struct {
	ID    string
	Top   []battleWeaponRow
	More  []battleWeaponRow
	Total int
}

func splitParties(id string, rows []battlePartyRow) battlePartyList {
	l := battlePartyList{ID: id, Top: rows, Total: len(rows)}
	if len(rows) > battleRowsShown+2 {
		l.Top, l.More = rows[:battleRowsShown], rows[battleRowsShown:]
	}
	return l
}

func splitWeapons(id string, rows []battleWeaponRow) battleWeaponList {
	l := battleWeaponList{ID: id, Top: rows, Total: len(rows)}
	if len(rows) > battleRowsShown+2 {
		l.Top, l.More = rows[:battleRowsShown], rows[battleRowsShown:]
	}
	return l
}

type battleKillRow struct {
	KillmailID int64
	At         string
	Loss       bool
	Victim     string
	VictimID   int64
	Ship       string
	ShipTypeID int64
	Value      string
	Mine       string
	Share      string
	TTK        string
}

type battlePartyRow struct {
	Name        string
	CharacterID int64
	Ticker      string
	Hull        string
	HullTypeID  int64
	Pilot       bool
	Died        bool
	Amount      string
	Share       string
	BarW        int64
	Window      string
}

type battleWeaponRow struct {
	Weapon       string
	WeaponTypeID int64
	Party        string
	Shots        string
	Damage       string
	Segs         []battleQualitySeg
	Good         string
	Judged       bool
}

type battleQualitySeg struct {
	Class string
	X, W  float64
	Label string
}

type battleTackleRow struct {
	From     string
	To       string
	Kind     string
	At       string
	Attempts string
	Mine     bool
}

// momentKinds names each moment kind for its chip, and the lane it
// rides in.
var momentKinds = map[string]struct{ label, lane string }{
	momentKill:     {"Kill", "mails"},
	momentLoss:     {"Loss", "mails"},
	momentTackle:   {"Tackle", "tackle"},
	momentPressure: {"Incoming peak", "incoming"},
	momentFocus:    {"Focus shift", "incoming"},
	momentContact:  {"First shots", "outgoing"},
	momentTarget:   {"Target switch", "outgoing"},
	momentEnergy:   {"Energy warfare", "energy"},
	momentRepairs:  {"Repairs", "repairs"},
}

// battleLanes is the timeline's lane order.
var battleLanes = []struct{ key, label string }{
	{"mails", "Mails"},
	{"tackle", "Tackle"},
	{"incoming", "Incoming"},
	{"outgoing", "Your fire"},
	{"energy", "Energy"},
	{"repairs", "Repairs"},
}

var findingLabels = map[string]string{
	"focus":        "Focus",
	"application":  "Application",
	"wasted":       "Wasted damage",
	"tank":         "Tank",
	"cap":          "Capacitor",
	"contribution": "Contribution",
	"tackle":       "Tackle",
}

func battleOutcomeText(o string) string {
	switch o {
	case "loss":
		return "Loss"
	case "kill":
		return "Kill"
	}
	return "No kill"
}

func battleLength(f db.BattleFight) string {
	return clockOffset(int(f.EndedAt.Sub(f.StartedAt) / time.Second))
}

func (app *Application) battleSystemName(ctx context.Context, id int64) string {
	if id <= 0 {
		return ""
	}
	return app.locationTitle(ctx, id, "solar_system")
}

func (app *Application) viewerCharacters(ctx context.Context, userID int64) map[int64]bool {
	out := map[int64]bool{}
	if userID <= 0 {
		return out
	}
	if chars, err := app.queries.ListCharactersByUser(ctx, userID); err == nil {
		for _, c := range chars {
			out[c.CharacterID] = true
		}
	}
	return out
}

// handleBattles is GET /intel/battles/.
func (app *Application) handleBattles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	v := &battlesView{Viewer: app.viewerCharacters(ctx, userID)}
	data.Battles = v
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: "Battle Reports", CanonicalPath: "/intel/battles/"})
	fights, err := app.queries.ListBattleFightsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("battle reports: list for user %d: %v", userID, err)
		data.Error = "Could not load battle reports; check the server log."
		app.render(ctx, w, http.StatusOK, "battles.html", data)
		return
	}
	var kills, losses int32
	var destroyed, lost float64
	names := map[int64]string{}
	for _, f := range fights {
		kills += f.Kills
		losses += f.Losses
		destroyed += f.IskDestroyed
		lost += f.IskLost
		if _, ok := names[f.SystemID]; !ok {
			names[f.SystemID] = app.battleSystemName(ctx, f.SystemID)
		}
		row := battleIndexRow{
			Href: fmt.Sprintf("/intel/battle/%d/", f.ID), When: f.StartedAt.UTC().Format("2006-01-02 15:04"), At: f.StartedAt,
			SystemID: f.SystemID, SystemName: names[f.SystemID], Listener: f.Listener, CharacterID: f.CharacterID,
			Length: battleLength(f), Outcome: f.Outcome, OutcomeText: battleOutcomeText(f.Outcome),
			Kills: f.Kills, Losses: f.Losses, SparkOut: f.SparkOut, SparkIn: f.SparkIn, Pilots: f.Pilots,
		}
		if f.IskDestroyed > 0 {
			row.Destroyed = formatISKShort(f.IskDestroyed)
		}
		if f.IskLost > 0 {
			row.Lost = formatISKShort(f.IskLost)
		}
		v.Rows = append(v.Rows, row)
	}
	if len(fights) > 0 {
		v.Tiles = []battleTile{
			{Label: "Fights", Value: strconv.Itoa(len(fights)), Sub: "read from your game logs"},
			{Label: "Kills", Value: strconv.Itoa(int(kills)), Sub: formatISKShort(destroyed) + " destroyed", Tone: "pos"},
			{Label: "Losses", Value: strconv.Itoa(int(losses)), Sub: formatISKShort(lost) + " lost", Tone: "neg"},
			{Label: "Efficiency", Value: iskEfficiency(destroyed, lost), Sub: "of the ISK on these mails"},
		}
	}
	app.render(ctx, w, http.StatusOK, "battles.html", data)
}

// iskEfficiency is the destroyed share of all ISK on the mails.
func iskEfficiency(destroyed, lost float64) string {
	if destroyed+lost <= 0 {
		return "none"
	}
	return fmt.Sprintf("%.0f%%", destroyed/(destroyed+lost)*100)
}

// ownedFight is the report at {id} when the signed-in account owns it.
func (app *Application) ownedFight(w http.ResponseWriter, r *http.Request) (db.BattleFight, bool) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	f, err := app.queries.GetBattleFight(ctx, id)
	if err != nil || f.UserID != app.userID(ctx) {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("battle reports: fight %d: %v", id, err)
		}
		http.NotFound(w, r)
		return f, false
	}
	return f, true
}

// sharedFight is the report at {token} when its link is on.
func (app *Application) sharedFight(w http.ResponseWriter, r *http.Request) (db.BattleFight, bool) {
	f, err := app.queries.GetBattleFightByToken(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		http.NotFound(w, r)
		return f, false
	}
	return f, true
}

// handleBattle is GET /intel/battle/{id}/: the owner's view.
func (app *Application) handleBattle(w http.ResponseWriter, r *http.Request) {
	if f, ok := app.ownedFight(w, r); ok {
		app.renderBattle(w, r, app.page(r.Context()), f, true, fmt.Sprintf("/intel/battle/%d/", f.ID))
	}
}

// handleBattleShared is GET /intel/battle/s/{token}/.
func (app *Application) handleBattleShared(w http.ResponseWriter, r *http.Request) {
	if f, ok := app.sharedFight(w, r); ok {
		app.renderBattle(w, r, app.publicPage(r.Context()), f, f.UserID == app.userID(r.Context()), fmt.Sprintf("/intel/battle/s/%s/", f.ShareToken))
	}
}

// handleBattleMoment is GET /intel/battle/{id}/moment/{seq}/.
func (app *Application) handleBattleMoment(w http.ResponseWriter, r *http.Request) {
	if f, ok := app.ownedFight(w, r); ok {
		app.renderMoment(w, r, f, true, fmt.Sprintf("/intel/battle/%d/", f.ID))
	}
}

// handleBattleSharedMoment is GET /intel/battle/s/{token}/moment/{seq}/.
func (app *Application) handleBattleSharedMoment(w http.ResponseWriter, r *http.Request) {
	if f, ok := app.sharedFight(w, r); ok {
		app.renderMoment(w, r, f, false, fmt.Sprintf("/intel/battle/s/%s/", f.ShareToken))
	}
}

// handleBattleShare is POST /intel/battle/{id}/share.
func (app *Application) handleBattleShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	back := app.flashBack(w, r, fmt.Sprintf("/intel/battle/%d/", id))
	on := r.FormValue("shared") == "on"
	if err := app.queries.SetBattleFightShared(ctx, db.SetBattleFightSharedParams{Shared: on, ID: id, UserID: app.userID(ctx)}); err != nil {
		logging.Errorf("battle reports: share fight %d: %v", id, err)
		back("Could not change sharing; try again.")
		return
	}
	if on {
		back("Share link on: anyone with the link can read this report.")
		return
	}
	back("Share link off: only you can read this report.")
}

func (app *Application) renderBattle(w http.ResponseWriter, r *http.Request, data pageData, f db.BattleFight, owner bool, base string) {
	ctx := r.Context()
	seq := int32(-1)
	if m, err := strconv.Atoi(r.URL.Query().Get("m")); err == nil && m >= 0 {
		seq = int32(m)
	}
	v := app.battleViewFor(ctx, f, owner, base, seq)
	data.Battle = v
	desc := fmt.Sprintf("%s, %s long: %d kills and %d losses.", v.Title, v.Length, f.Kills, f.Losses)
	meta := module.PageMeta{Title: v.Title, Description: desc, CanonicalPath: base}
	data.HeadMeta = app.HeadMeta(meta)
	app.render(ctx, w, http.StatusOK, "battle.html", data)
}

func (app *Application) renderMoment(w http.ResponseWriter, r *http.Request, f db.BattleFight, owner bool, base string) {
	ctx := r.Context()
	seq, err := strconv.Atoi(chi.URLParam(r, "seq"))
	if err != nil || seq < 0 {
		http.NotFound(w, r)
		return
	}
	moments, _ := app.queries.ListBattleFightMoments(ctx, f.ID)
	var viewer map[int64]bool
	if owner {
		viewer = app.viewerCharacters(ctx, f.UserID)
	}
	mv := app.momentViewFor(ctx, f, moments, int32(seq), base, viewer)
	if mv == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	app.renderFragment(w, "battle.html", "battle-moment", mv)
}

func (app *Application) battleViewFor(ctx context.Context, f db.BattleFight, owner bool, base string, seq int32) *battleView {
	v := &battleView{
		ID: f.ID, Base: base, Owner: owner, Shared: f.Shared,
		ShareAction: fmt.Sprintf("/intel/battle/%d/share", f.ID),
		When:        f.StartedAt.UTC().Format("2006-01-02 15:04"), At: f.StartedAt,
		Length: battleLength(f), SystemID: f.SystemID, SystemName: app.battleSystemName(ctx, f.SystemID),
		Listener: f.Listener, CharacterID: f.CharacterID,
		Outcome: f.Outcome, OutcomeText: battleOutcomeText(f.Outcome),
		Lines: f.Lines, Unparsed: f.Unparsed,
	}
	if owner {
		v.Viewer = app.viewerCharacters(ctx, f.UserID)
	}
	if f.Shared {
		path := fmt.Sprintf("/intel/battle/s/%s/", f.ShareToken)
		if v.ShareURL = app.publicPageURL(path); v.ShareURL == "" {
			v.ShareURL = path
		}
	}
	v.Title = "Battle report"
	if v.SystemName != "" {
		v.Title = "Battle in " + v.SystemName
	}
	switch f.SystemFrom {
	case "location":
		v.SystemNote = "placed by the pilot's location"
	case "":
		v.SystemNote = "no mail or location placed it"
	}
	if f.HullTypeID > 0 {
		v.Hull, v.HullTypeID = app.typeNameOrID(ctx, f.HullTypeID), f.HullTypeID
		if f.HullFrom == "ship" {
			v.HullNote = "the ship the pilot was last seen in"
		}
	}

	secs := max(f.EndedAt.Sub(f.StartedAt).Seconds(), 1)
	v.Tiles = []battleTile{
		{Label: "Damage dealt", Value: esi.FormatInt(f.DamageOut), Sub: fmt.Sprintf("%s a second on average", esi.FormatInt(int64(float64(f.DamageOut)/secs))), Tone: "out"},
		{Label: "Damage taken", Value: esi.FormatInt(f.DamageIn), Sub: fmt.Sprintf("peak %s a second at %s", esi.FormatInt(f.PeakInDps), clockOffset(int(f.PeakInAt))), Tone: "in"},
		{Label: "Repairs received", Value: esi.FormatInt(f.RepsIn), Sub: esi.FormatInt(f.RepsOut) + " given", Tone: "rep"},
		{Label: "Capacitor drained", Value: esi.FormatInt(f.CapIn) + " GJ", Sub: "neutralized or drained from you", Tone: "cap"},
	}

	if s, err := app.queries.GetBattleFightSeries(ctx, f.ID); err == nil && s.Chart != "" {
		if json.Unmarshal([]byte(s.Chart), &v.Chart) == nil && len(v.Chart.Lines) > 0 {
			v.HasChart = true
			v.Hostile, v.Friendly = v.Chart.Hostile, v.Chart.Friendly
		}
	}

	// The timeline and the moment it opens on.
	moments, _ := app.queries.ListBattleFightMoments(ctx, f.ID)
	if seq < 0 {
		seq = defaultMoment(moments)
	}
	span := float64(max(int(secs), 1))
	lanes := map[string]*battleLane{}
	for _, l := range battleLanes {
		lane := &battleLane{Key: l.key, Label: l.label}
		lanes[l.key] = lane
	}
	for _, m := range moments {
		k := momentKinds[m.Kind]
		lane := lanes[k.lane]
		if lane == nil {
			continue
		}
		left, width := float64(m.AtSecond), float64(m.EndSecond-m.AtSecond)
		if m.Kind == momentKill || m.Kind == momentLoss {
			// A mail is a point: the second the ship died. Its
			// panel still covers the run-up.
			left, width = float64(m.EndSecond), 0
		}
		mark := battleLaneMark{
			Seq: m.Seq, Kind: m.Kind, Title: m.Title,
			Left:  round1(left / span * 100),
			Width: round1(width / span * 100),
			BandX: round1(float64(m.AtSecond) / span * battleChartW),
			BandW: round1(max(float64(m.EndSecond-m.AtSecond), 1) / span * battleChartW),
			When:  clockOffset(int(m.AtSecond)),
			Href:  fmt.Sprintf("%s?m=%d#bt-moment", base, m.Seq), Fragment: fmt.Sprintf("%smoment/%d/", base, m.Seq),
			Current: m.Seq == seq,
		}
		if mark.Current {
			v.Band = battleBand{X: mark.BandX, W: mark.BandW, On: true}
		}
		lane.Marks = append(lane.Marks, mark)
	}
	for _, l := range battleLanes {
		if len(lanes[l.key].Marks) > 0 {
			v.Lanes = append(v.Lanes, *lanes[l.key])
		}
	}
	v.Moment = app.momentViewFor(ctx, f, moments, seq, base, v.Viewer)

	if rows, err := app.queries.ListBattleFightFindings(ctx, f.ID); err == nil {
		for _, fd := range rows {
			first := len(v.Findings) == 0 || v.Findings[len(v.Findings)-1].Rule != fd.Rule
			v.Findings = append(v.Findings, battleFindingRow{Rule: fd.Rule, Label: findingLabels[fd.Rule], Text: fd.Text, First: first})
		}
	}
	if rows, err := app.queries.ListBattleFightKills(ctx, f.ID); err == nil {
		for _, k := range rows {
			v.Kills = append(v.Kills, app.battleKillRow(ctx, k))
		}
	}

	parties, _ := app.queries.ListBattleFightParties(ctx, f.ID)
	bySeq := map[int32]db.BattleFightParty{}
	for _, p := range parties {
		bySeq[p.Seq] = p
	}
	row := func(p db.BattleFightParty, amount, whole int64) battlePartyRow {
		r := partyRow(p, amount, whole)
		r.Window = clockOffset(int(p.FirstAt.Sub(f.StartedAt)/time.Second)) + " to " + clockOffset(int(p.LastAt.Sub(f.StartedAt)/time.Second))
		return r
	}
	var capOut int64
	for _, p := range parties {
		capOut += p.CapIn
	}
	var shotBy, shot, support, neuts []battlePartyRow
	for _, p := range parties {
		if p.DamageIn > 0 {
			shotBy = append(shotBy, row(p, p.DamageIn, f.DamageIn))
		}
		if p.DamageOut > 0 {
			shot = append(shot, row(p, p.DamageOut, f.DamageOut))
		}
		if p.RepsIn > 0 {
			support = append(support, row(p, p.RepsIn, f.RepsIn))
		}
		if p.CapIn > 0 {
			neuts = append(neuts, row(p, p.CapIn, capOut))
		}
	}
	byShare := func(rows []battlePartyRow) []battlePartyRow {
		for i := 1; i < len(rows); i++ {
			for j := i; j > 0 && rows[j].BarW > rows[j-1].BarW; j-- {
				rows[j], rows[j-1] = rows[j-1], rows[j]
			}
		}
		return rows
	}
	v.ShotBy = splitParties("bt-in-more", byShare(shotBy))
	v.Shot = splitParties("bt-out-more", byShare(shot))
	v.Support = splitParties("bt-rep-more", byShare(support))
	v.Neuts = splitParties("bt-neut-more", byShare(neuts))

	var wout, win []battleWeaponRow
	for _, wa := range v.Chart.Weapons {
		r := weaponRow(db.BattleFightWeapon{
			Weapon: wa.Weapon, WeaponTypeID: wa.TypeID, Shots: wa.Shots, Damage: wa.Damage,
			QMiss: wa.Quality[0], QGraze: wa.Quality[1], QGlance: wa.Quality[2], QHit: wa.Quality[3],
			QPenetrate: wa.Quality[4], QSmash: wa.Quality[5], QWreck: wa.Quality[6],
		}, "")
		r.Party = fmt.Sprintf("%d %s", wa.Parties, map[bool]string{true: "target", false: "targets"}[wa.Parties == 1])
		if wa.Dir == "out" {
			wout = append(wout, r)
		} else {
			r.Party = fmt.Sprintf("%d %s", wa.Parties, map[bool]string{true: "ship", false: "ships"}[wa.Parties == 1])
			win = append(win, r)
		}
	}
	v.WeaponsOut, v.WeaponsIn = splitWeapons("bt-wout-more", wout), splitWeapons("bt-win-more", win)
	if rows, err := app.queries.ListBattleFightTackles(ctx, f.ID); err == nil {
		for _, t := range rows {
			tr := tackleRow(t, f.StartedAt)
			if tr.Mine {
				v.MyTackle = append(v.MyTackle, tr)
			} else {
				v.GridTackle = append(v.GridTackle, tr)
			}
		}
		v.GridTotal = len(v.GridTackle)
		if len(v.GridTackle) > battleRowsShown+2 {
			v.GridTackle, v.GridMore = v.GridTackle[:battleRowsShown], v.GridTackle[battleRowsShown:]
		}
	}
	return v
}

// defaultMoment is the moment a report opens on: the first loss,
// else the first kill, else the first incoming peak, else the first.
func defaultMoment(ms []db.ListBattleFightMomentsRow) int32 {
	for _, kind := range []string{momentLoss, momentKill, momentPressure} {
		for _, m := range ms {
			if m.Kind == kind {
				return m.Seq
			}
		}
	}
	if len(ms) > 0 {
		return ms[0].Seq
	}
	return -1
}

func partyRow(p db.BattleFightParty, amount, whole int64) battlePartyRow {
	r := battlePartyRow{
		Name: p.Name, CharacterID: p.CharacterID, Ticker: p.Ticker, Hull: p.Hull, HullTypeID: p.HullTypeID,
		Pilot: p.Pilot, Died: p.Died, Amount: esi.FormatInt(amount),
	}
	if whole > 0 {
		r.BarW = pct(amount, whole)
		r.Share = fmt.Sprintf("%d%%", r.BarW)
	}
	return r
}

var qualityLabels = []struct{ class, label string }{
	{"q-miss", "missed"}, {"q-graze", "grazed"}, {"q-glance", "glanced off"}, {"q-hit", "hit"},
	{"q-pen", "penetrated"}, {"q-smash", "smashed"}, {"q-wreck", "wrecked"},
}

func weaponRow(wp db.BattleFightWeapon, party string) battleWeaponRow {
	r := battleWeaponRow{
		Weapon: wp.Weapon, WeaponTypeID: wp.WeaponTypeID, Party: party,
		Shots: esi.FormatInt(wp.Shots), Damage: esi.FormatInt(wp.Damage),
	}
	if r.Weapon == "" {
		r.Weapon = "Drones and missiles"
	}
	counts := []int64{wp.QMiss, wp.QGraze, wp.QGlance, wp.QHit, wp.QPenetrate, wp.QSmash, wp.QWreck}
	var judged int64
	for _, c := range counts {
		judged += c
	}
	if judged == 0 {
		return r
	}
	r.Judged = true
	x := 0.0
	for i, c := range counts {
		if c == 0 {
			continue
		}
		wd := float64(c) / float64(judged) * 100
		r.Segs = append(r.Segs, battleQualitySeg{Class: qualityLabels[i].class, X: round1(x), W: round1(wd), Label: fmt.Sprintf("%d %s", c, qualityLabels[i].label)})
		x += wd
	}
	r.Good = fmt.Sprintf("%d%%", pct(judged-wp.QMiss-wp.QGraze, judged))
	return r
}

func tackleRow(t db.BattleFightTackle, start time.Time) battleTackleRow {
	kind := "Scramble"
	if t.Kind == "disruption" {
		kind = "Disruption"
	}
	return battleTackleRow{
		From: t.FromName, To: t.ToName, Kind: kind,
		At:       clockOffset(int(t.FirstAt.Sub(start) / time.Second)),
		Attempts: esi.FormatInt(t.Attempts),
		Mine:     strings.EqualFold(t.FromName, "you") || strings.EqualFold(t.ToName, "you"),
	}
}

func (app *Application) battleKillRow(ctx context.Context, k db.BattleFightKill) battleKillRow {
	row := battleKillRow{
		KillmailID: k.KillmailID, At: clockOffset(int(k.AtSecond)), Loss: k.Loss,
		Victim: k.VictimName, VictimID: k.VictimCharacterID,
		Ship: app.typeNameOrID(ctx, k.ShipTypeID), ShipTypeID: k.ShipTypeID,
	}
	if row.Victim == "" && k.VictimCharacterID > 0 {
		row.Victim = app.displayCharacter(ctx, k.VictimCharacterID)
	}
	if k.ValueIsk.Valid {
		row.Value = formatISKShort(k.ValueIsk.Float64)
	}
	if !k.Loss && k.MyDamage > 0 {
		row.Mine = esi.FormatInt(k.MyDamage)
		if k.DamageTaken > 0 {
			row.Share = fmt.Sprintf("%d%%", pct(k.MyDamage, k.DamageTaken))
		}
	}
	if k.TtkSeconds >= 0 {
		row.TTK = clockOffset(int(k.TtkSeconds))
	}
	return row
}

// momentViewFor reads one moment's stored panel.
func (app *Application) momentViewFor(ctx context.Context, f db.BattleFight, ms []db.ListBattleFightMomentsRow, seq int32, base string, viewer map[int64]bool) *battleMomentView {
	if seq < 0 {
		return nil
	}
	m, err := app.queries.GetBattleFightMoment(ctx, db.GetBattleFightMomentParams{FightID: f.ID, Seq: seq})
	if err != nil {
		return nil
	}
	var d battleMomentDetail
	if json.Unmarshal([]byte(m.Detail), &d) != nil {
		return nil
	}
	mv := &battleMomentView{
		Seq: m.Seq, Kind: m.Kind, KindLabel: momentKinds[m.Kind].label, Title: m.Title,
		Window:  clockOffset(int(m.AtSecond)) + " to " + clockOffset(int(m.EndSecond)),
		Summary: d.Summary, SparkOut: d.SparkOut, SparkIn: d.SparkIn, SparkReps: d.SparkReps,
		Viewer: viewer,
	}
	if len(d.Marks) == 2 {
		mv.MarkA, mv.MarkB = d.Marks[0].X, max(d.Marks[1].X-d.Marks[0].X, 1)
	}
	mv.Figures = []battleTile{
		{Label: "Dealt", Value: esi.FormatInt(d.DamageOut), Tone: "out"},
		{Label: "Taken", Value: esi.FormatInt(d.DamageIn), Tone: "in"},
		{Label: "Repaired", Value: esi.FormatInt(d.RepsIn), Tone: "rep"},
		{Label: "Neutralized", Value: esi.FormatInt(d.CapIn) + " GJ", Tone: "cap"},
	}
	conv := func(ps []momentParty) []battlePartyRow {
		var out []battlePartyRow
		for _, p := range ps {
			out = append(out, battlePartyRow{
				Name: p.Name, CharacterID: p.CharacterID, Ticker: p.Ticker, Hull: p.Hull, HullTypeID: p.HullTypeID,
				Pilot: p.Pilot, Died: p.Died, Amount: esi.FormatInt(p.Amount), BarW: p.Pct, Share: fmt.Sprintf("%d%%", p.Pct),
			})
		}
		return out
	}
	mv.Attackers, mv.Targets = splitParties("", conv(d.Attackers)), splitParties("", conv(d.Targets))
	mv.Support, mv.Neuts = splitParties("", conv(d.Support)), splitParties("", conv(d.Neuts))
	for _, t := range d.Tackles {
		kind := "Scramble"
		if t.Kind == "disruption" {
			kind = "Disruption"
		}
		mv.Tackles = append(mv.Tackles, battleTackleRow{From: t.From, To: t.To, Kind: kind, At: clockOffset(t.At), Attempts: esi.FormatInt(t.Count), Mine: t.Mine})
	}
	for _, k := range d.Kills {
		row := battleKillRow{KillmailID: k.KillmailID, At: clockOffset(k.At), Loss: k.Loss, Victim: k.Victim, VictimID: k.VictimID, ShipTypeID: k.ShipTypeID, Ship: app.typeNameOrID(ctx, k.ShipTypeID)}
		mv.Kills = append(mv.Kills, row)
	}
	for i, x := range ms {
		if x.Seq != seq {
			continue
		}
		mv.Position = fmt.Sprintf("%d of %d", i+1, len(ms))
		if i > 0 {
			mv.Prev = fmt.Sprintf("%s?m=%d#bt-moment", base, ms[i-1].Seq)
			mv.PrevFrag = fmt.Sprintf("%smoment/%d/", base, ms[i-1].Seq)
		}
		if i+1 < len(ms) {
			mv.Next = fmt.Sprintf("%s?m=%d#bt-moment", base, ms[i+1].Seq)
			mv.NextFrag = fmt.Sprintf("%smoment/%d/", base, ms[i+1].Seq)
		}
	}
	return mv
}

// battleLinks maps the given mails to the viewer's own fights that
// matched them, for the Killmails page's Battle report links.
func (app *Application) battleLinks(ctx context.Context, userID int64, ids []int64) map[int64]string {
	out := map[int64]string{}
	if userID <= 0 || len(ids) == 0 {
		return out
	}
	rows, err := app.queries.ListBattleFightIDsForKills(ctx, db.ListBattleFightIDsForKillsParams{UserID: userID, KillmailIds: ids})
	if err != nil {
		logging.Errorf("killmails: battle links: %v", err)
		return out
	}
	for _, r := range rows {
		out[r.KillmailID] = fmt.Sprintf("/intel/battle/%d/", r.FightID)
	}
	return out
}

// Link is the party's name, linked to its pilot page once a mail has
// named the character.
func (r battlePartyRow) Link() template.HTML { return charLink(nil, r.CharacterID, r.Name) }
