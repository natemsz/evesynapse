package app

// ---------------------------------------------------------------------------
// Fleet battle reports, the explorer page: one battle from the fleet's
// side, read from its one stored row (fleet_battle_detail, built by
// fleet_explorer.go). The strip is the centre, as on a pilot's own
// report: the fleet's damage, damage taken and repairs on one time axis,
// the kills and losses on a rail above it, the moments of the battle in
// lanes under it, and a lane per pilot. Picking a moment opens it in the
// panel below (battle.js loads the panel in place, the same contract as
// a pilot's report); the page's script zooms the strip into any window.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	fleetChartW = 600.0
	fleetChartH = 160.0
	fleetRows   = 12 // whole-battle tables show this many before Show all
)

// fleetMomentKinds names a fleet moment's kind and the lane it rides.
var fleetMomentKinds = map[string]struct{ label, lane string }{
	momentKill:     {"Kill", "mails"},
	momentLoss:     {"Loss", "mails"},
	momentTackle:   {"Tackle", "tackle"},
	momentPressure: {"Incoming peak", "incoming"},
	momentContact:  {"First shots", "outgoing"},
	momentTarget:   {"Primary", "outgoing"},
	momentEnergy:   {"Energy warfare", "energy"},
	momentRepairs:  {"Repairs", "repairs"},
}

var fleetLanes = []struct{ key, label string }{
	{"mails", "Kills"},
	{"outgoing", "Outgoing"},
	{"incoming", "Incoming"},
	{"repairs", "Repairs"},
	{"energy", "Energy"},
	{"tackle", "Tackle"},
}

// fleetExplorerView is the explorer of one fleet battle.
type fleetExplorerView struct {
	BattleSeq   int32
	Outcome     string // won, lost, traded, none
	OutcomeText string
	Efficiency  string // "85%", empty when nothing died
	ISKBar      [2]float64
	Tiles       []battleTile
	Chart       fleetChartView
	Rail        []fleetRailMark
	Lanes       []battleLane
	LaneIcons   map[string]int64 // a module's icon beside a lane's label
	PilotLanes  []fleetPilotLane
	Band        battleBand
	Moment      *fleetMomentView
	Findings    []battleFindingRow
	Kills       []fleetKillView
	Ours        fleetSideView
	Theirs      fleetSideView
	Pilots      fleetPartyTable
	Enemies     fleetPartyTable
	WeaponsOut  battleWeaponList
	WeaponsIn   battleWeaponList
	Tackles     []fleetTackleView
	WeaponLogs  int
	Zoom        template.JS // the strip's data for the zoom (#fl-zoom-data)
	Hover       template.JS // the chart's card per column (#bt-hover-data)
	Demo        bool
	ID          int64
	// MailsOnly: no log counts in this battle, so it is drawn from its
	// killmails: the chart shows the ISK destroyed and lost so far.
	MailsOnly        bool
	Groups           []fleetGroupView // the enemy, by alliance or corporation
	WeaponsFromMails bool
	AddMails         string // where a missing killmail is added
}

// fleetGroupView is one alliance or corporation on the other side.
type fleetGroupView struct {
	Logo  string
	Link  template.HTML
	Count int
	Lost  int
}

type fleetChartView struct {
	Out, In, Reps string // polyline points
	YTop          string
	ISK           bool // the lines are ISK destroyed and lost so far
	Ticks         []string
	Marks         []battleChartMark
}

// fleetRailMark is a kill or loss on the rail over the chart.
type fleetRailMark struct {
	Left     float64
	Ship     int64
	Loss     bool
	Label    string
	Href     string
	Fragment string
	Kill     *fleetKillView // its card
}

// fleetPilotLane is one pilot's row under the strip: when they dealt
// damage and took it, and when they died.
type fleetPilotLane struct {
	Who     fleetWho
	Cells   []fleetLaneCell
	DeathX  float64
	Died    bool
	Summary string
}

type fleetLaneCell struct {
	X, W    float64
	Out, In int // intensity 1 to 5, 0 for none
}

// fleetWho is a pilot or a ship as a row names it: a portrait for a
// character, the hull's picture, a link where there is a page.
type fleetWho struct {
	Group      template.HTML // alliance, else corporation, from a killmail
	GroupLogo  string
	FinalBlow  bool
	Weapon     int64
	WeaponName string
	Name       string
	Link       template.HTML
	Portrait   string
	ShipTypeID int64
	Hull       string
	Ticker     string
	Died       bool
	Mail       int64
	MailHref   string
	Card       *fleetCard // its card over the whole battle
	Bare       bool       // no placeholder where it has no picture
}

type fleetPartyRow struct {
	Who        fleetWho
	A, B, C    string // the row's figures, by table
	AW, BW, CW float64
	Note       string
	Muted      bool // figures from killmails beside figures from logs
}

// fleetTableWithHead is a party table and its three figure headings.
type fleetTableWithHead struct {
	Table   fleetPartyTable
	A, B, C string
}

type fleetPartyTable struct {
	ID    string
	Top   []fleetPartyRow
	More  []fleetPartyRow
	Total int
}

type fleetKillView struct {
	At         string
	Loss       bool
	Ship       string
	ShipTypeID int64
	Victim     fleetWho
	Value      string
	Top        string // our top damage on a kill, their top on a loss
	Hands      int
	Href       string // the killboard page, empty for the demo
	Moment     string // the moment that opens it
	Fragment   string
	// Its card: whose ship died, the most damage and the final blow on
	// its mail, and how many were on it.
	Title   string
	TopHand *fleetHandView
	Blow    *fleetHandView
	OnMail  int
}

type fleetSideView struct {
	Pilots  int
	Hulls   []battleHull
	Tickers []battleCount
}

type fleetTackleView struct {
	Kind, Icon string
	IconID     int64
	From, To   string
	At         string
	Count      int64
	Ours, OnUs bool
}

// fleetMomentView is one moment's panel.
type fleetMomentView struct {
	Seq       int
	Kind      string
	KindLabel string
	Window    string
	Title     string
	Summary   string
	Figures   []battleTile
	Kills     []fleetKillView
	Shooters  fleetPartyTable
	Targets   fleetPartyTable
	Attack    fleetPartyTable
	Hurt      fleetPartyTable
	Support   fleetPartyTable
	Neuts     fleetPartyTable
	Hands     []fleetHandView
	Tackles   []fleetTackleView
	Mail      string // the killboard page of a kill or loss
	From, To  int
	Position  string
	Prev      string
	PrevFrag  string
	Next      string
	NextFrag  string
	Demo      bool
	NeutIcon  int64
	FromMails bool
	// The window on a small chart (fleetMomentSpark).
	SparkOut, SparkIn, SparkReps string
	SparkISK                     bool
	MarkA, MarkB                 float64
}

type fleetHandView struct {
	Who    fleetWho
	Damage string
	Pct    int64
	BarW   float64
	Ours   bool
}

// characterPortrait is a character's portrait, empty for an invented one.
func characterPortrait(id int64) string {
	if id <= 0 {
		return ""
	}
	return portraitURL(id, 64)
}

// fleetWhoOf names one of the fleet's pilots.
func fleetWhoOf(names *killNames, viewer map[int64]bool, id, ship int64) fleetWho {
	name := names.character(id)
	w := fleetWho{Name: name, Link: charLink(viewer, id, name), Portrait: characterPortrait(id), ShipTypeID: ship}
	if ship != 0 {
		w.Hull = names.typeName(ship)
	}
	return w
}

// fleetWhoParty names one party of the battle from what the logs called it.
func fleetWhoParty(names *killNames, viewer map[int64]bool, p momentParty) fleetWho {
	w := fleetWho{Name: p.Name, ShipTypeID: p.HullTypeID, Hull: p.Hull, Ticker: p.Ticker, Died: p.Died}
	if p.CharacterID > 0 {
		if p.Name == "" {
			w.Name = names.character(p.CharacterID)
		}
		w.Link, w.Portrait = charLink(viewer, p.CharacterID, w.Name), characterPortrait(p.CharacterID)
	} else {
		w.Link = template.HTML(template.HTMLEscapeString(w.Name))
	}
	if w.Hull == w.Name {
		w.Hull = ""
	}
	return w
}

func fleetPct(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return round1(float64(a) * 100 / float64(b))
}

func fleetTable(id string, rows []fleetPartyRow) fleetPartyTable {
	t := fleetPartyTable{ID: id, Total: len(rows)}
	if len(rows) > fleetRows {
		t.Top, t.More = rows[:fleetRows], rows[fleetRows:]
	} else {
		t.Top = rows
	}
	return t
}

// fleetMomentTable is one of a moment's party lists: who, a share bar,
// the amount.
func fleetMomentTable(id string, names *killNames, viewer map[int64]bool, cards *fleetCards, list []momentParty) fleetPartyTable {
	var rows []fleetPartyRow
	for _, p := range list {
		who := fleetWhoParty(names, viewer, p)
		if p.Pilot && p.CharacterID > 0 && p.Name == "" {
			who.Name = names.character(p.CharacterID)
		}
		who.Card = cards.party(p)
		// One of the fleet's pilots: its ship, under its name.
		if ship := cards.ship(p.CharacterID); ship != 0 {
			if who.ShipTypeID == 0 {
				who.ShipTypeID = ship
			}
			if who.Hull == "" {
				who.Hull = names.typeName(who.ShipTypeID)
			}
		}
		rows = append(rows, fleetPartyRow{Who: who, A: battleNum(p.Amount), AW: float64(p.Pct), Note: fmt.Sprintf("%d%%", p.Pct)})
	}
	return fleetTable(id, rows)
}

// fleetHand is one attacker on a killmail, with its card.
func fleetHand(names *killNames, viewer map[int64]bool, cards *fleetCards, h fleetMailHand) fleetHandView {
	who := fleetWhoOf(names, viewer, h.Character, h.Ship)
	if h.Character == 0 && h.Ship != 0 {
		who.Name = names.typeName(h.Ship)
		who.Link = template.HTML(template.HTMLEscapeString(who.Name))
	}
	if h.Ours {
		who.Card = cards.pilot(h.Character)
	} else {
		who.Group, who.GroupLogo = fleetGroupOf(names, h.Corp, h.Alliance)
		who.Card = cards.enemy(h.Character, who.Name, who.Hull, h.Character > 0)
	}
	who.Weapon, who.FinalBlow = h.Weapon, h.FinalBlow
	if h.Weapon != 0 {
		who.WeaponName = names.typeName(h.Weapon)
	}
	return fleetHandView{Who: who, Damage: battleNum(h.Damage), Pct: h.Pct, BarW: float64(h.Pct), Ours: h.Ours}
}

// fleetVictim is a killmail's victim, with its card: a loss is one of
// the fleet's pilots, a kill an enemy.
func fleetVictim(names *killNames, viewer map[int64]bool, cards *fleetCards, loss bool, victim, ship int64) fleetWho {
	who := fleetWhoOf(names, viewer, victim, 0)
	who.Bare = true
	if loss {
		who.Card = cards.pilot(victim)
	} else {
		who.Card = cards.enemy(victim, who.Name, names.typeName(ship), victim > 0)
	}
	return who
}

// fleetKillHref is a killmail's killboard page, empty when it has none:
// a demo mail, or a kill only our members' own ESI brought in, which
// stays private until zKillboard posts it.
func fleetKillHref(linked bool, id int64) string {
	if !linked || id <= 0 {
		return ""
	}
	return "/intel/kill/" + strconv.FormatInt(id, 10) + "/"
}

// fleetCard is a pilot's or an enemy's card: the tooltip the shared
// hover layer (app.js) shows over its name, with its figures for the
// whole battle and its heaviest weapons.
type fleetCard struct {
	Rows        []fleetCardRow
	WeaponsHead string
	Weapons     []battleCardWeapon
	Foot        string
}

// fleetCardRow is one figure on a card. Tone colours its swatch and its
// value: out, in, rep, cap or kill; a plain count has none.
type fleetCardRow struct {
	Label, Value, Tone string
}

// fleetCards are a battle's cards, found by whom they belong to: the
// fleet's pilots by character, the enemy by its key and, for a pilot,
// by character too.
type fleetCards struct {
	pilots  map[int64]*fleetCard
	ships   map[int64]int64 // each pilot's ship
	enemies map[string]*fleetCard
	byChar  map[int64]*fleetCard
}

func newFleetCards(d fleetDetail, clock func(int) string) *fleetCards {
	c := &fleetCards{pilots: map[int64]*fleetCard{}, ships: map[int64]int64{}, enemies: map[string]*fleetCard{}, byChar: map[int64]*fleetCard{}}
	for _, p := range d.Pilots {
		c.pilots[p.ID], c.ships[p.ID] = fleetPilotCard(p, clock), p.Ship
	}
	for _, e := range d.Enemies {
		card := fleetEnemyCard(e, clock)
		c.enemies[e.Key] = card
		if e.CharacterID > 0 {
			c.byChar[e.CharacterID] = card
		}
	}
	return c
}

// pilot is a fleet pilot's card.
func (c *fleetCards) pilot(id int64) *fleetCard {
	if c == nil || id <= 0 {
		return nil
	}
	return c.pilots[id]
}

// enemy is an enemy's card, by its character or, for a ship nobody is
// named in, by its hull.
func (c *fleetCards) enemy(char int64, name, hull string, pilot bool) *fleetCard {
	if c == nil {
		return nil
	}
	if card := c.byChar[char]; char > 0 && card != nil {
		return card
	}
	keys := []string{fleetLogParty{Name: name, Hull: hull, CharacterID: char, Pilot: pilot}.key()}
	if name != "" {
		keys = append(keys, "n:"+strings.ToLower(name), "h:"+name+"|"+name)
	}
	for _, k := range keys {
		if card := c.enemies[k]; card != nil {
			return card
		}
	}
	return nil
}

// ship is a fleet pilot's ship.
func (c *fleetCards) ship(id int64) int64 {
	if c == nil || id <= 0 {
		return 0
	}
	return c.ships[id]
}

// party is the card of anyone a moment lists: one of the fleet's pilots
// or an enemy.
func (c *fleetCards) party(p momentParty) *fleetCard {
	if card := c.pilot(p.CharacterID); card != nil {
		return card
	}
	return c.enemy(p.CharacterID, p.Name, p.Hull, p.Pilot)
}

func fleetCardRows(c *fleetCard, rows ...fleetCardRow) {
	for _, r := range rows {
		if r.Value != "" && r.Value != "0" && r.Value != "0 GJ" {
			c.Rows = append(c.Rows, r)
		}
	}
}

func fleetCardWeaponsOf(list []fleetCardWeapon) []battleCardWeapon {
	var out []battleCardWeapon
	for _, w := range list {
		out = append(out, battleCardWeapon{Name: w.Name, TypeID: w.TypeID, Damage: battleNum(w.Damage)})
	}
	return out
}

// fleetPilotCard is a fleet pilot's card: what its log counted over the
// battle or, with no log, what the killmails credit it, and its
// heaviest weapons.
func fleetPilotCard(p fleetDetailPilot, clock func(int) string) *fleetCard {
	c := &fleetCard{Weapons: fleetCardWeaponsOf(p.Weapons)}
	switch {
	case p.HasLog:
		fleetCardRows(c,
			fleetCardRow{"Dealt", battleNum(p.Out), "out"},
			fleetCardRow{"Taken", battleNum(p.In), "in"},
			fleetCardRow{"Repaired", battleNum(p.Reps), "rep"},
			fleetCardRow{"Drained from them", battleNum(p.CapIn) + " GJ", "cap"})
	case p.FromMails:
		fleetCardRows(c,
			fleetCardRow{"Damage on kills", battleNum(p.Out), "out"},
			fleetCardRow{"Damage on their loss", battleNum(p.In), "in"})
	}
	if p.OnMails > 0 {
		fleetCardRows(c, fleetCardRow{"Kills they are on", strconv.Itoa(p.OnMails), "kill"})
	}
	if len(c.Weapons) > 0 {
		c.WeaponsHead = "Their heaviest weapons"
	}
	var foot []string
	if p.Died {
		foot = append(foot, "Lost their ship at "+clock(p.DiedAt)+".")
	}
	switch {
	case p.HasLog:
	case p.FromMails:
		foot = append(foot, "No log added, so the figures come from killmails.")
	default:
		foot = append(foot, "No log added for this battle.")
	}
	c.Foot = strings.Join(foot, " ")
	return c
}

// fleetEnemyCard is an enemy's card: what it did to the fleet and took
// from it, how many of the fleet's pilots it fought, its heaviest
// weapons on the fleet, and when it was on the field.
func fleetEnemyCard(e fleetDetailParty, clock func(int) string) *fleetCard {
	c := &fleetCard{Weapons: fleetCardWeaponsOf(e.Weapons)}
	if e.FromMails {
		fleetCardRows(c,
			fleetCardRow{"Damage on the fleet's losses", battleNum(e.ToUs), "in"},
			fleetCardRow{"The fleet's damage on its killmail", battleNum(e.FromUs), "out"})
	} else {
		fleetCardRows(c,
			fleetCardRow{"Dealt to the fleet", battleNum(e.ToUs), "in"},
			fleetCardRow{"Taken from the fleet", battleNum(e.FromUs), "out"},
			fleetCardRow{"Drained from the fleet", battleNum(e.CapOnUs) + " GJ", "cap"})
	}
	fleetCardRows(c,
		fleetCardRow{"Fleet pilots it hit", strconv.Itoa(e.Hit), ""},
		fleetCardRow{"Fleet pilots shooting it", strconv.Itoa(e.Shooters), ""})
	if len(c.Weapons) > 0 {
		c.WeaponsHead = "Its heaviest weapons on the fleet"
	}
	when := "On the field from " + clock(e.First) + " to " + clock(e.Last) + "."
	if e.FromMails {
		when = "Seen only on killmails, from " + clock(e.First) + " to " + clock(e.Last) + "."
	}
	if e.Died {
		when = "Destroyed. " + when
	}
	c.Foot = when
	return c
}

// fleetFindingLabels names each finding's rule for its heading.
var fleetFindingLabels = map[string]string{
	"outcome":   "Result",
	"focus":     "Focus",
	"logistics": "Repairs",
	"pressure":  "Incoming",
	"primary":   "Enemy fire",
	"damage":    "Top damage",
	"energy":    "Energy warfare",
}

// fleetFindingRows gives each finding its rule's heading, on the first
// of a run of the same rule.
func fleetFindingRows(list []fleetFinding) []battleFindingRow {
	var out []battleFindingRow
	for _, f := range list {
		first := len(out) == 0 || out[len(out)-1].Rule != f.Rule
		out = append(out, battleFindingRow{Rule: f.Rule, Label: fleetFindingLabels[f.Rule], Text: f.Text, First: first})
	}
	return out
}

// fleetExplorerFor builds the explorer of the shown battle.
func (app *Application) fleetExplorerFor(ctx context.Context, view *fleetReportView, shown db.FleetBattle, d fleetDetail, pick string) *fleetExplorerView {
	names := app.newKillNames(ctx)
	x := &fleetExplorerView{BattleSeq: shown.Seq, Demo: view.Demo, ID: view.ID, Findings: fleetFindingRows(d.Findings), WeaponLogs: d.WeaponLogs,
		MailsOnly: d.Logged == 0, WeaponsFromMails: d.WeaponsFromMails, AddMails: view.Path + "/killmails"}
	viewer := view.ViewerChars
	href := func(id int64) string { return fleetKillHref(view.Linked[id], id) }
	n := len(d.Out)
	length := max(n*10, 10)
	base := view.Path + "?battle=" + strconv.Itoa(int(shown.Seq))
	fragBase := fmt.Sprintf("/fleets/%d/report/battle/%d/moment/", view.ID, shown.Seq)

	// Outcome and the ISK balance.
	destroyed, lost := shown.IskDestroyed, shown.IskLost
	switch {
	case shown.Kills > 0 && shown.Losses == 0:
		x.Outcome, x.OutcomeText = "kill", "Won"
	case shown.Kills == 0 && shown.Losses > 0:
		x.Outcome, x.OutcomeText = "loss", "Lost"
	case shown.Kills > 0:
		if destroyed >= lost {
			x.Outcome, x.OutcomeText = "kill", "Won on ISK"
		} else {
			x.Outcome, x.OutcomeText = "loss", "Lost on ISK"
		}
	default:
		x.Outcome, x.OutcomeText = "nokill", "No kills"
	}
	if destroyed+lost > 0 {
		x.Efficiency = fmt.Sprintf("%.0f%%", destroyed*100/(destroyed+lost))
		x.ISKBar = [2]float64{round1(destroyed * 100 / (destroyed + lost)), round1(lost * 100 / (destroyed + lost))}
	}

	// Tiles.
	var reps int64
	for _, v := range d.Reps {
		reps += v
	}
	x.Tiles = []battleTile{
		{Label: "Kills", Value: strconv.Itoa(int(shown.Kills)), Sub: formatISKShort(destroyed) + " ISK destroyed", Tone: "pos"},
		{Label: "Losses", Value: strconv.Itoa(int(shown.Losses)), Sub: formatISKShort(lost) + " ISK lost", Tone: "neg"},
	}
	if x.MailsOnly {
		var onKills, onLosses int64
		for _, p := range d.Pilots {
			onKills += p.Out
			onLosses += p.In
		}
		x.Tiles = append(x.Tiles,
			battleTile{Label: "Damage on kills", Value: battleNum(onKills), Sub: "what the killmails credit the fleet", Tone: "out"},
			battleTile{Label: "Damage on losses", Value: battleNum(onLosses), Sub: "to the ships the fleet lost", Tone: "in"})
	} else {
		x.Tiles = append(x.Tiles,
			battleTile{Label: "Damage dealt", Value: battleNum(shown.DamageOut), Sub: "to enemies", Tone: "out"},
			battleTile{Label: "Damage taken", Value: battleNum(shown.DamageIn), Sub: fmt.Sprintf("peak %s a second", battleNum(d.PeakIn)), Tone: "in"},
			battleTile{Label: "Repairs", Value: battleNum(reps), Sub: repsCoverage(reps, shown.DamageIn), Tone: "rep"})
	}
	x.Tiles = append(x.Tiles, battleTile{Label: "Enemies", Value: strconv.Itoa(d.EnemyCount), Sub: fmt.Sprintf("%d of our pilots", len(d.Pilots)), Tone: ""})

	// The chart: rates a second, on one axis; with no log, the ISK
	// destroyed and lost so far.
	series := [3][]int64{d.Out, d.In, d.Reps}
	per := int64(10)
	if x.MailsOnly && len(d.IskKill) == n {
		series, per = [3][]int64{d.IskKill, d.IskLoss, make([]int64, n)}, 1
	}
	var top int64
	for i := 0; i < n; i++ {
		top = max(top, series[0][i]/per, series[1][i]/per, series[2][i]/per)
	}
	ceil := niceCeil(float64(max(top, 1)))
	line := func(v []int64) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			xp := 0.0
			if n > 1 {
				xp = float64(i) * fleetChartW / float64(n-1)
			}
			yp := fleetChartH - float64(v[i])/float64(per)/ceil*fleetChartH
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%.1f,%.1f", xp, yp)
		}
		return b.String()
	}
	x.Chart = fleetChartView{Out: line(series[0]), In: line(series[1]), Reps: line(series[2]), YTop: battleNum(int64(ceil))}
	if per == 1 {
		x.Chart.ISK, x.Chart.YTop = true, formatISKShort(ceil)
	}
	clock := func(sec int) string { return time.Unix(d.Start+int64(sec), 0).UTC().Format("15:04:05") }
	cards := newFleetCards(d, clock)
	for i := 0; i <= 4; i++ {
		x.Chart.Ticks = append(x.Chart.Ticks, clock(length * i / 4)[:5])
	}
	xOf := func(sec int) float64 { return round1(float64(sec) * fleetChartW / float64(length)) }
	pctOf := func(sec int) float64 { return round1(float64(sec) * 100 / float64(length)) }

	// Moments: lanes, rail, and the open one.
	momentBySeq := map[int]fleetDetailMoment{}
	mailMoment := map[int64]int{}
	for _, m := range d.Moments {
		momentBySeq[m.Seq] = m
		if m.Mail != 0 {
			mailMoment[m.Mail] = m.Seq
		}
	}
	current := 0
	if v, err := strconv.Atoi(pick); err == nil {
		if _, ok := momentBySeq[v]; ok {
			current = v
		}
	}
	if current == 0 {
		current = defaultFleetMoment(d.Moments)
	}
	lanes := map[string]*battleLane{}
	for _, l := range fleetLanes {
		lanes[l.key] = &battleLane{Key: l.key, Label: l.label}
	}
	for _, m := range d.Moments {
		kind := fleetMomentKinds[m.Kind]
		lane := lanes[kind.lane]
		if lane == nil {
			continue
		}
		mark := battleLaneMark{
			Label: kind.label, Seq: int32(m.Seq), Kind: m.Kind,
			Left: pctOf(m.Detail.From), Width: max(pctOf(m.Detail.To-m.Detail.From), 0.8),
			BandX: xOf(m.Detail.From), BandW: max(xOf(m.Detail.To-m.Detail.From), 2),
			Title: m.Title, When: clock(m.At),
			Href: base + "&m=" + strconv.Itoa(m.Seq) + "#bt-moment", Fragment: fragBase + strconv.Itoa(m.Seq) + "/",
			Current: m.Seq == current,
			Brief:   battleMomentBrief{DamageOut: m.Brief.Out, DamageIn: m.Brief.In, RepsIn: m.Brief.Reps, CapIn: m.Brief.Cap, TopIn: m.Brief.TopIn, TopOut: m.Brief.TopOut, ShipID: m.Ship},
		}
		lane.Marks = append(lane.Marks, mark)
		if m.Seq == current {
			x.Band = battleBand{X: mark.BandX, W: mark.BandW, On: true}
		}
		if m.Kind == momentKill || m.Kind == momentLoss {
			x.Rail = append(x.Rail, fleetRailMark{Left: pctOf(m.At), Ship: m.Ship, Loss: m.Kind == momentLoss, Label: clock(m.At) + " " + m.Title, Href: mark.Href, Fragment: mark.Fragment})
			x.Chart.Marks = append(x.Chart.Marks, battleChartMark{X: xOf(m.At), Loss: m.Kind == momentLoss, KillmailID: m.Mail, Label: clock(m.At) + " " + m.Title})
		}
	}
	for _, l := range fleetLanes {
		if len(lanes[l.key].Marks) > 0 {
			x.Lanes = append(x.Lanes, *lanes[l.key])
		}
	}
	icons := app.fleetTackleIcons(ctx)
	x.LaneIcons = map[string]int64{"energy": icons["energy"], "tackle": icons["scramble"]}
	if len(d.WeaponsOut) > 0 {
		x.LaneIcons["outgoing"] = d.WeaponsOut[0].TypeID
	}
	if len(d.WeaponsIn) > 0 {
		x.LaneIcons["incoming"] = d.WeaponsIn[0].TypeID
	}
	if current != 0 {
		x.Moment = app.fleetMomentViewFor(names, viewer, cards, view, d, current, base, fragBase, clock, icons)
	}

	// A lane per pilot with a log.
	var cellTop int64
	for _, p := range d.Pilots {
		for i := range p.LaneOut {
			cellTop = max(cellTop, p.LaneOut[i], p.LaneIn[i])
		}
	}
	level := func(v int64) int {
		if v <= 0 || cellTop <= 0 {
			return 0
		}
		return int(min(5, 1+v*5/(cellTop+1)))
	}
	cols := max(d.LaneColumns, 1)
	for _, p := range d.Pilots {
		if len(p.LaneOut) == 0 && !p.Died {
			continue
		}
		lane := fleetPilotLane{Who: fleetWhoOf(names, viewer, p.ID, p.Ship), Died: p.Died}
		lane.Who.Died, lane.Who.Card = p.Died, cards.pilot(p.ID)
		if p.Died {
			lane.DeathX = xOf(p.DiedAt)
		}
		for c := range p.LaneOut {
			if o, in := level(p.LaneOut[c]), level(p.LaneIn[c]); o+in > 0 {
				lane.Cells = append(lane.Cells, fleetLaneCell{X: round1(float64(c) * fleetChartW / float64(cols)), W: round1(fleetChartW / float64(cols)), Out: o, In: in})
			}
		}
		lane.Summary = fmt.Sprintf("%s dealt %s and took %s", lane.Who.Name, battleNum(p.Out), battleNum(p.In))
		if p.FromMails {
			lane.Summary = fmt.Sprintf("%s: %s damage on kills and %s on their loss, from killmails", lane.Who.Name, battleNum(p.Out), battleNum(p.In))
		}
		x.PilotLanes = append(x.PilotLanes, lane)
	}

	// The zoom's data: rates a second per step, the battle's length.
	type zoomData struct {
		Start  int64   `json:"start"`
		Length int     `json:"length"`
		Out    []int64 `json:"out"`
		In     []int64 `json:"in"`
		Reps   []int64 `json:"reps"`
		ISK    bool    `json:"isk,omitempty"`
	}
	z := zoomData{Start: d.Start, Length: length, Out: make([]int64, n), In: make([]int64, n), Reps: make([]int64, n), ISK: x.Chart.ISK}
	for i := 0; i < n; i++ {
		z.Out[i], z.In[i], z.Reps[i] = series[0][i]/per, series[1][i]/per, series[2][i]/per
	}
	if raw, err := json.Marshal(z); err == nil {
		x.Zoom = template.JS(raw)
	}
	if raw, err := json.Marshal(d.Hover); err == nil && len(d.Hover) > 0 {
		x.Hover = template.JS(raw)
	}

	// Killmails.
	for _, m := range d.Moments {
		if m.Kind != momentKill && m.Kind != momentLoss {
			continue
		}
		k := fleetKillView{
			At: clock(m.At), Loss: m.Kind == momentLoss, ShipTypeID: m.Ship, Ship: names.typeName(m.Ship),
			Href: href(m.Mail), Moment: base + "&m=" + strconv.Itoa(m.Seq) + "#bt-moment", Fragment: fragBase + strconv.Itoa(m.Seq) + "/",
			Hands: max(m.Detail.HandsAll, len(m.Detail.Attackers)), Title: m.Title, OnMail: m.Detail.OnMail,
		}
		for _, row := range view.Mails {
			if row.KillmailID == m.Mail {
				k.Value = row.Value
				k.Victim = fleetVictim(names, viewer, cards, k.Loss, row.VictimID, m.Ship)
			}
		}
		for _, h := range m.Detail.Attackers {
			if h.Ours != k.Loss {
				k.Top = fmt.Sprintf("%s, %d%%", names.character(h.Character), h.Pct)
				break
			}
		}
		// The most damage, and the final blow when someone else landed it.
		for i, h := range m.Detail.Attackers {
			hand := fleetHand(names, viewer, cards, h)
			switch {
			case i == 0:
				k.TopHand = &hand
			case h.FinalBlow && k.Blow == nil:
				k.Blow = &hand
			}
		}
		x.Kills = append(x.Kills, k)
	}
	for i := range x.Rail {
		for j := range x.Kills {
			if x.Rail[i].Fragment == x.Kills[j].Fragment {
				x.Rail[i].Kill = &x.Kills[j]
			}
		}
	}

	// The two sides.
	ours, theirs := map[int64]*battleHull{}, map[int64]*battleHull{}
	tickers := map[string]int{}
	for _, p := range d.Pilots {
		if p.Ship == 0 {
			continue
		}
		h := ours[p.Ship]
		if h == nil {
			h = &battleHull{TypeID: p.Ship, Name: names.typeName(p.Ship)}
			ours[p.Ship] = h
		}
		h.Count++
		if p.Died {
			h.Lost++
		}
	}
	for _, e := range d.Enemies {
		if e.HullTypeID != 0 {
			h := theirs[e.HullTypeID]
			if h == nil {
				h = &battleHull{TypeID: e.HullTypeID, Name: names.typeName(e.HullTypeID)}
				theirs[e.HullTypeID] = h
			}
			h.Count++
			if e.Died {
				h.Lost++
			}
		}
		if e.Ticker != "" {
			tickers[e.Ticker]++
		}
	}
	groups := map[int64]*fleetGroupView{}
	for _, e := range d.Enemies {
		id := e.AllianceID
		if id == 0 {
			id = e.CorpID
		}
		if id == 0 {
			continue
		}
		g := groups[id]
		if g == nil {
			g = &fleetGroupView{}
			if e.AllianceID != 0 {
				g.Link, g.Logo = fleetGroupOf(names, 0, id)
			} else {
				g.Link, g.Logo = fleetGroupOf(names, id, 0)
			}
			groups[id] = g
		}
		g.Count++
		if e.Died {
			g.Lost++
		}
	}
	for _, g := range groups {
		x.Groups = append(x.Groups, *g)
	}
	sort.Slice(x.Groups, func(i, j int) bool {
		if x.Groups[i].Count != x.Groups[j].Count {
			return x.Groups[i].Count > x.Groups[j].Count
		}
		return string(x.Groups[i].Link) < string(x.Groups[j].Link)
	})
	if len(x.Groups) > 8 {
		x.Groups = x.Groups[:8]
	}
	x.Ours = fleetSideView{Pilots: len(d.Pilots), Hulls: sortedHulls(ours)}
	x.Theirs = fleetSideView{Pilots: d.EnemyCount, Hulls: sortedHulls(theirs)}
	for t, c := range tickers {
		x.Theirs.Tickers = append(x.Theirs.Tickers, battleCount{Name: t, Count: c})
	}
	sort.Slice(x.Theirs.Tickers, func(i, j int) bool { return x.Theirs.Tickers[i].Count > x.Theirs.Tickers[j].Count })

	// Our pilots and the enemy, whole battle.
	var maxOut, maxIn, maxRep int64
	for _, p := range d.Pilots {
		maxOut, maxIn, maxRep = max(maxOut, p.Out), max(maxIn, p.In), max(maxRep, p.Reps)
	}
	var pilotRows []fleetPartyRow
	for _, p := range d.Pilots {
		who := fleetWhoOf(names, viewer, p.ID, p.Ship)
		who.Died, who.Mail, who.MailHref, who.Card = p.Died, p.LossMail, href(p.LossMail), cards.pilot(p.ID)
		row := fleetPartyRow{Who: who, AW: fleetPct(p.Out, maxOut), BW: fleetPct(p.In, maxIn), CW: fleetPct(p.Reps, maxRep)}
		switch {
		case p.HasLog:
			row.A, row.B, row.C = battleNum(p.Out), battleNum(p.In), battleNum(p.Reps)
		case x.MailsOnly:
			row.A, row.B, row.C = battleNum(p.Out), battleNum(p.In), strconv.Itoa(p.OnMails)
			row.CW = 0
		case p.FromMails:
			row.A, row.B, row.C, row.Note = battleNum(p.Out), battleNum(p.In), "-", "No log: damage from killmails"
			row.Muted = true
		default:
			row.A, row.B, row.C, row.Note = "-", "-", "-", "No log added"
		}
		if p.OnMails > 0 && !x.MailsOnly {
			row.Note = strings.TrimPrefix(row.Note+fmt.Sprintf(", on %d %s", p.OnMails, map[bool]string{true: "kill", false: "kills"}[p.OnMails == 1]), ", ")
		}
		pilotRows = append(pilotRows, row)
	}
	x.Pilots = fleetTable("fl-x-pilots-more", pilotRows)
	firing := 0
	for _, p := range d.Pilots {
		if p.HasLog && p.Out > 0 {
			firing++
		}
	}
	var eTo, eFrom int64
	for _, e := range d.Enemies {
		eTo, eFrom = max(eTo, e.ToUs), max(eFrom, e.FromUs)
	}
	var enemyRows []fleetPartyRow
	for _, e := range d.Enemies {
		who := fleetWhoParty(names, viewer, momentParty{Name: e.Name, Hull: e.Hull, HullTypeID: e.HullTypeID, CharacterID: e.CharacterID, Ticker: e.Ticker, Pilot: e.Pilot, Died: e.Died})
		who.Mail, who.MailHref = e.KillMail, href(e.KillMail)
		who.Group, who.GroupLogo = fleetGroupOf(names, e.CorpID, e.AllianceID)
		who.Card = cards.enemies[e.Key]
		row := fleetPartyRow{Who: who, A: battleNum(e.ToUs), B: battleNum(e.FromUs), AW: fleetPct(e.ToUs, eTo), BW: fleetPct(e.FromUs, eFrom)}
		if e.FromMails && !x.MailsOnly {
			row.Muted = true
		}
		row.C = clock(e.First)[:5] + " to " + clock(e.Last)[:5]
		// Who fired on it, only when that was not everyone who fired:
		// a fleet shooting its primaries together is the rule, not news.
		switch {
		case e.FromMails && !x.MailsOnly:
			row.Note = "Seen only on killmails"
		case e.Shooters > 0 && e.Shooters < firing:
			row.Note = fmt.Sprintf("Shot by %d of the %d pilots who fired", e.Shooters, firing)
		}
		enemyRows = append(enemyRows, row)
	}
	x.Enemies = fleetTable("fl-x-enemies-more", enemyRows)

	// Weapons and tackle.
	toRows := func(list []fleetDetailWeapon, id string, who string) battleWeaponList {
		var rows []battleWeaponRow
		for _, w := range list {
			wp := db.BattleFightWeapon{Weapon: w.Weapon, WeaponTypeID: w.TypeID, Shots: w.Shots, Damage: w.Damage,
				QMiss: w.Quality[0], QGraze: w.Quality[1], QGlance: w.Quality[2], QHit: w.Quality[3], QPenetrate: w.Quality[4], QSmash: w.Quality[5], QWreck: w.Quality[6]}
			rows = append(rows, weaponRow(wp, fmt.Sprintf("%d %s", w.Users, who)))
		}
		return splitWeapons(id, rows)
	}
	x.WeaponsOut = toRows(d.WeaponsOut, "fl-x-wout-more", "of our pilots")
	x.WeaponsIn = toRows(d.WeaponsIn, "fl-x-win-more", "enemy pilots")
	for _, t := range d.Tackles {
		x.Tackles = append(x.Tackles, fleetTackleFor(t, icons, clock))
	}
	return x
}

// fleetGroupOf names an alliance, else a corporation, with its logo.
func fleetGroupOf(names *killNames, corp, alliance int64) (template.HTML, string) {
	switch {
	case alliance == 0 && demoInventedGroup(corp):
		return template.HTML(template.HTMLEscapeString(names.corp(corp))), ""
	case alliance > 0:
		return allianceLink(alliance, names.alliance(alliance)), fmt.Sprintf("https://images.evetech.net/alliances/%d/logo?size=32", alliance)
	case corp > 0:
		return corpLink(corp, names.corp(corp)), fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=32", corp)
	}
	return "", ""
}

func repsCoverage(reps, in int64) string {
	if in <= 0 {
		return "no damage taken"
	}
	return fmt.Sprintf("%d%% of damage taken", reps*100/in)
}

// defaultFleetMoment is the moment the page opens on: the first loss,
// else the first kill, else the first moment.
func defaultFleetMoment(ms []fleetDetailMoment) int {
	for _, kind := range []string{momentLoss, momentKill} {
		for _, m := range ms {
			if m.Kind == kind {
				return m.Seq
			}
		}
	}
	if len(ms) > 0 {
		return ms[0].Seq
	}
	return 0
}

func sortedHulls(m map[int64]*battleHull) []battleHull {
	var out []battleHull
	for _, h := range m {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// fleetTackleIcons is the module whose picture stands for each kind of
// hold and for energy warfare.
func (app *Application) fleetTackleIcons(ctx context.Context) map[string]int64 {
	names := map[string]string{"scramble": "Warp Scrambler II", "disruption": "Warp Disruptor II", "web": "Stasis Webifier II", "energy": "Heavy Energy Neutralizer II"}
	list := make([]string, 0, len(names))
	for _, n := range names {
		list = append(list, n)
	}
	out := map[string]int64{}
	rows, err := app.queries.ListSDETypesByNames(ctx, list)
	if err != nil {
		logging.Errorf("fleets: tackle icons: %v", err)
		return out
	}
	for kind, name := range names {
		for _, r := range rows {
			if r.Name == name && (out[kind] == 0 || r.TypeID < out[kind]) {
				out[kind] = r.TypeID
			}
		}
	}
	return out
}

func fleetTackleFor(t fleetDetailTackle, icons map[string]int64, clock func(int) string) fleetTackleView {
	kind := map[string]string{"scramble": "Scrambled", "disruption": "Pointed", "web": "Webbed"}[t.Kind]
	if kind == "" {
		kind = t.Kind
	}
	return fleetTackleView{Kind: kind, IconID: icons[t.Kind], From: t.From, To: t.To, At: clock(t.At), Count: t.Count, Ours: t.Ours, OnUs: t.OnUs}
}

// fleetMomentViewFor builds one moment's panel.
func (app *Application) fleetMomentViewFor(names *killNames, viewer map[int64]bool, cards *fleetCards, view *fleetReportView, d fleetDetail, seq int, base, fragBase string, clock func(int) string, icons map[string]int64) *fleetMomentView {
	var m fleetDetailMoment
	pos := 0
	for i, mo := range d.Moments {
		if mo.Seq == seq {
			m, pos = mo, i
		}
	}
	if m.Seq == 0 {
		return nil
	}
	kind := fleetMomentKinds[m.Kind]
	w := m.Detail
	v := &fleetMomentView{
		Seq: m.Seq, Kind: m.Kind, KindLabel: kind.label, Title: m.Title, Summary: w.Summary,
		Window: clock(w.From) + " to " + clock(w.To) + " EVE", From: w.From, To: w.To,
		Position: fmt.Sprintf("%d of %d", pos+1, len(d.Moments)), Demo: view.Demo,
		Mail: fleetKillHref(view.Linked[m.Mail], m.Mail), NeutIcon: icons["energy"], FromMails: w.FromMails,
		SparkOut: w.SparkOut, SparkIn: w.SparkIn, SparkReps: w.SparkReps, SparkISK: w.SparkISK, MarkA: w.MarkA, MarkB: w.MarkB,
	}
	if w.FromMails {
		v.Figures = []battleTile{
			{Label: "Damage on kills", Value: battleNum(w.MailOut), Tone: "out"},
			{Label: "Damage on losses", Value: battleNum(w.MailIn), Tone: "in"},
		}
	} else {
		v.Figures = []battleTile{
			{Label: "Dealt", Value: battleNum(w.Out), Tone: "out"},
			{Label: "Taken", Value: battleNum(w.In), Tone: "in"},
			{Label: "Repaired", Value: battleNum(w.Reps), Tone: "rep"},
		}
	}
	if w.CapIn > 0 {
		v.Figures = append(v.Figures, battleTile{Label: "Drained", Value: battleNum(w.CapIn) + " GJ", Tone: "cap"})
	}
	for _, k := range w.Kills {
		kv := fleetKillView{At: clock(k.At), Loss: k.Loss, ShipTypeID: k.ShipTypeID, Ship: names.typeName(k.ShipTypeID), Href: fleetKillHref(view.Linked[k.KillmailID], k.KillmailID)}
		kv.Victim = fleetVictim(names, viewer, cards, k.Loss, k.VictimID, k.ShipTypeID)
		v.Kills = append(v.Kills, kv)
	}
	id := strconv.Itoa(m.Seq)
	v.Shooters = fleetMomentTable("fl-m-sh-"+id, names, viewer, cards, w.Shooters)
	v.Targets = fleetMomentTable("fl-m-tg-"+id, names, viewer, cards, w.Targets)
	v.Attack = fleetMomentTable("fl-m-at-"+id, names, viewer, cards, w.Attack)
	v.Hurt = fleetMomentTable("fl-m-hu-"+id, names, viewer, cards, w.Hurt)
	v.Support = fleetMomentTable("fl-m-su-"+id, names, viewer, cards, w.Support)
	v.Neuts = fleetMomentTable("fl-m-ne-"+id, names, viewer, cards, w.Neuts)
	for _, h := range w.Attackers {
		v.Hands = append(v.Hands, fleetHand(names, viewer, cards, h))
	}
	for _, t := range w.Tackles {
		v.Tackles = append(v.Tackles, fleetTackleFor(t, icons, clock))
	}
	if pos > 0 {
		p := d.Moments[pos-1].Seq
		v.Prev, v.PrevFrag = base+"&m="+strconv.Itoa(p)+"#bt-moment", fragBase+strconv.Itoa(p)+"/"
	}
	if pos < len(d.Moments)-1 {
		nx := d.Moments[pos+1].Seq
		v.Next, v.NextFrag = base+"&m="+strconv.Itoa(nx)+"#bt-moment", fragBase+strconv.Itoa(nx)+"/"
	}
	return v
}

// fleetDetailOf reads a battle's stored explorer; false when the battle
// was built before explorers were.
func (app *Application) fleetDetailOf(ctx context.Context, battleID int64) (fleetDetail, bool) {
	raw, err := app.queries.GetFleetBattleDetail(ctx, battleID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: explorer of battle %d: %v", battleID, err)
		}
		return fleetDetail{}, false
	}
	var d fleetDetail
	if err := json.Unmarshal([]byte(raw), &d); err != nil || len(d.Out) == 0 {
		return fleetDetail{}, false
	}
	return d, true
}

// handleFleetReportMoment is GET /fleets/{opID}/report/battle/{seq}/
// moment/{m}/: one moment's panel alone, for battle.js to load in place.
// It answers to exactly who the Report tab answers to.
func (app *Application) handleFleetReportMoment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	seq, _ := strconv.Atoi(chi.URLParam(r, "seq"))
	m, _ := strconv.Atoi(chi.URLParam(r, "m"))
	op, err := app.queries.GetOp(ctx, id)
	if err != nil || !app.fleetReportTab(ctx, op, v, time.Now()) {
		http.NotFound(w, r)
		return
	}
	battles, err := app.queries.ListFleetBattles(ctx, op.ID)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, b := range battles {
		if int(b.Seq) != seq {
			continue
		}
		d, ok := app.fleetDetailOf(ctx, b.ID)
		if !ok {
			break
		}
		view := &fleetReportView{ID: op.ID, Path: fleetReportPath(op.ID), Demo: isDemoFleet(op), ViewerChars: map[int64]bool{}, Linked: map[int64]bool{}}
		if kills, err := app.queries.ListFleetBattleKills(ctx, b.ID); err == nil {
			for _, k := range kills {
				view.Linked[k.KillmailID] = k.Public && !view.Demo
			}
		}
		for _, c := range v.Characters {
			view.ViewerChars[c.CharacterID] = true
		}
		clock := func(sec int) string { return time.Unix(d.Start+int64(sec), 0).UTC().Format("15:04:05") }
		base := view.Path + "?battle=" + strconv.Itoa(seq)
		frag := fmt.Sprintf("/fleets/%d/report/battle/%d/moment/", op.ID, seq)
		mv := app.fleetMomentViewFor(app.newKillNames(ctx), view.ViewerChars, newFleetCards(d, clock), view, d, m, base, frag, clock, app.fleetTackleIcons(ctx))
		if mv == nil {
			break
		}
		w.Header().Set("X-Robots-Tag", "noindex")
		app.renderFragment(w, "fleet_report.html", "fleet-moment", mv)
		return
	}
	http.NotFound(w, r)
}

// fillFleetExplorer adds the shown battle's explorer to the report.
func (app *Application) fillFleetExplorer(ctx context.Context, view *fleetReportView, pick, moment string) {
	if view.Battle == nil {
		return
	}
	battles, err := app.queries.ListFleetBattles(ctx, view.ID)
	if err != nil || len(battles) == 0 {
		return
	}
	shown := battles[0]
	if want, err := strconv.Atoi(pick); err == nil {
		for _, b := range battles {
			if int(b.Seq) == want {
				shown = b
			}
		}
	}
	if d, ok := app.fleetDetailOf(ctx, shown.ID); ok {
		view.X = app.fleetExplorerFor(ctx, view, shown, d, moment)
	}
}
