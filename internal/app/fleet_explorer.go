package app

// ---------------------------------------------------------------------------
// Fleet battle reports, the explorer: one battle seen from the whole fleet.
//
// Built by the report worker with the report itself (fleet_reports.go),
// from the same member logs and killmails, by the same counting rules as
// mergeFleetLogs: a pilot's 10-second step counts from the one read that
// covers it, damage between fleet members is friendly fire and in no
// total, and repairs count from the giver's log. Each enemy is merged
// across every log that saw it, so the damage it did to the fleet is the
// sum of what each pilot's own log recorded taking from it, and the
// damage the fleet did to it is the sum of what each pilot's own log
// recorded dealing. Whole-fight tables (weapons, tackle) come only from
// reads counted in full, so an overlapping re-read never adds them twice.
//
// The page reads this one stored row (fleet_battle_detail): the steps
// for the zoomable chart, each pilot's lane, the enemies, the weapons,
// the tackle, the moments of the timeline with what happened in each
// window, and what the numbers say.
// ---------------------------------------------------------------------------

import (
	"fmt"
	json "github.com/goccy/go-json"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/gamelog"
	"evesynapse/internal/humanize"
)

// The explorer's bounds, in one place.
const (
	fleetDetailEnemies  = 60 // enemies kept, most involved first
	fleetDetailLanes    = 60 // pilots with a lane of their own
	fleetDetailColumns  = 180
	fleetDetailWeapons  = 40
	fleetDetailTackles  = 40
	fleetDetailMoments  = 40
	fleetMomentRows     = 6
	fleetPeakApartSteps = 6 // a minute between two peaks of one kind
	fleetPeaksMax       = 3
	fleetSwitchesMax    = 6
	fleetTacklesMax     = 6
)

// fleetLogParty is one party of a member's log, as its read names it.
type fleetLogParty struct {
	Name, Ticker, Hull string
	HullTypeID         int64
	CharacterID        int64
	Pilot              bool
}

// key is the party as one enemy across every log that saw it: by its
// character when a killmail named it, else by its pilot's name, else by
// what the log calls it (a drone or an unnamed ship).
func (p fleetLogParty) key() string {
	switch {
	case p.CharacterID > 0:
		return fmt.Sprintf("c:%d", p.CharacterID)
	case p.Pilot && p.Name != "":
		return "n:" + strings.ToLower(p.Name)
	}
	return "h:" + strings.ToLower(p.Name) + "|" + strings.ToLower(p.Hull)
}

// fleetDetailVersion is the explorer's build: a stored explorer from an
// older build lacks what a newer one adds (the demo seeds again for it).
// 2: killmail hands, the killmail-only battle, our own private kills.
// 3: each pilot's and enemy's heaviest weapons for their cards, a chart
// of each moment's window, and the chart's card on EVE time and for a
// battle drawn from its killmails.
const fleetDetailVersion = 3

// fleetDetail is the stored explorer of one fleet battle.
type fleetDetail struct {
	Version int     `json:"v"`
	Start   int64   `json:"start"` // the first step, a Unix second on a 10-second boundary
	Out     []int64 `json:"out"`   // per step: damage the fleet dealt
	In      []int64 `json:"in"`    // damage the fleet took
	Reps    []int64 `json:"reps"`  // repairs the fleet gave
	CapIn   []int64 `json:"cap_in"`
	CapOut  []int64 `json:"cap_out"`
	PeakIn  int64   `json:"peak_in"` // the busiest step's incoming, per second
	PeakAt  int     `json:"peak_at"` // its offset in seconds
	CapOnUs int64   `json:"cap_on_us"`
	OurCap  int64   `json:"our_cap"`

	Pilots      []fleetDetailPilot  `json:"pilots"`
	Enemies     []fleetDetailParty  `json:"enemies"`
	EnemyCount  int                 `json:"enemy_count"`
	Allies      []fleetDetailParty  `json:"allies,omitempty"`
	WeaponsOut  []fleetDetailWeapon `json:"weapons_out,omitempty"`
	WeaponsIn   []fleetDetailWeapon `json:"weapons_in,omitempty"`
	WeaponLogs  int                 `json:"weapon_logs"` // logs whose weapons and tackle count
	Tackles     []fleetDetailTackle `json:"tackles,omitempty"`
	Moments     []fleetDetailMoment `json:"moments,omitempty"`
	Findings    []fleetFinding      `json:"findings,omitempty"`
	Hover       []battleHoverPt     `json:"hover,omitempty"`
	LaneColumns int                 `json:"lane_columns"`
	// Logged is how many of the fleet's pilots have a log counted here.
	// With none, the battle is drawn from its killmails alone: IskKill
	// and IskLoss carry the ISK destroyed and lost so far, per step.
	Logged  int     `json:"logged"`
	IskKill []int64 `json:"isk_kill,omitempty"`
	IskLoss []int64 `json:"isk_loss,omitempty"`
	// WeaponsFromMails: no log was read in full, so the weapons are the
	// ones the killmails list, with no shots or hit quality.
	WeaponsFromMails bool `json:"weapons_from_mails,omitempty"`
}

// fleetDetailPilot is one of the fleet's pilots in the battle.
type fleetDetailPilot struct {
	ID       int64 `json:"id"`
	Ship     int64 `json:"ship"`
	Out      int64 `json:"out"`
	In       int64 `json:"in"`
	Reps     int64 `json:"reps"`
	CapIn    int64 `json:"cap_in"`
	HasLog   bool  `json:"has_log"`
	Died     bool  `json:"died,omitempty"`
	LossMail int64 `json:"loss_mail,omitempty"`
	DiedAt   int   `json:"died_at,omitempty"`
	OnMails  int   `json:"on_mails,omitempty"` // kills it is on
	// FromMails: no log of this pilot counts, so Out and In are the
	// damage the killmails credit: dealt on kills, taken on its loss.
	FromMails bool    `json:"from_mails,omitempty"`
	LaneOut   []int64 `json:"lane_out,omitempty"`
	LaneIn    []int64 `json:"lane_in,omitempty"`
	// Weapons: its heaviest weapons, for its card.
	Weapons []fleetCardWeapon `json:"weapons,omitempty"`
}

// fleetDetailParty is one enemy (or a third party that repaired the
// fleet), merged across the logs that saw it.
type fleetDetailParty struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	CharacterID int64  `json:"character_id,omitempty"`
	Ticker      string `json:"ticker,omitempty"`
	Hull        string `json:"hull,omitempty"`
	HullTypeID  int64  `json:"hull_type_id,omitempty"`
	Pilot       bool   `json:"pilot,omitempty"`
	ToUs        int64  `json:"to_us"`
	FromUs      int64  `json:"from_us"`
	CapOnUs     int64  `json:"cap_on_us,omitempty"`
	RepsToUs    int64  `json:"reps_to_us,omitempty"`
	Died        bool   `json:"died,omitempty"`
	KillMail    int64  `json:"kill_mail,omitempty"`
	First       int    `json:"first"` // seconds into the battle
	Last        int    `json:"last"`
	Shooters    int    `json:"shooters,omitempty"` // our pilots who hit it
	Hit         int    `json:"hit,omitempty"`      // our pilots it hit
	CorpID      int64  `json:"corp_id,omitempty"`  // from a killmail
	AllianceID  int64  `json:"alliance_id,omitempty"`
	// FromMails: no log saw it, so its figures are the killmails' own:
	// ToUs the damage it did on the fleet's losses, FromUs the fleet's
	// damage on its own killmail.
	FromMails bool `json:"from_mails,omitempty"`
	// Weapons: the heaviest it fired at the fleet, for its card.
	Weapons []fleetCardWeapon `json:"weapons,omitempty"`
}

// fleetCardWeapon is one of a pilot's or an enemy's heaviest weapons,
// with the damage it did over the battle.
type fleetCardWeapon struct {
	Name   string `json:"n"`
	TypeID int64  `json:"t,omitempty"`
	Damage int64  `json:"d"`
}

// fleetDetailWeapon is one weapon across the fleet (or across the
// enemy), with how well it landed.
type fleetDetailWeapon struct {
	Weapon  string   `json:"weapon"`
	TypeID  int64    `json:"type_id,omitempty"`
	Users   int      `json:"users"`
	Shots   int64    `json:"shots"`
	Damage  int64    `json:"damage"`
	Quality [7]int64 `json:"q"`
}

// fleetDetailTackle is one hold seen on grid, merged across logs.
type fleetDetailTackle struct {
	From   string `json:"from"`
	FromID int64  `json:"from_id,omitempty"`
	To     string `json:"to"`
	ToID   int64  `json:"to_id,omitempty"`
	Kind   string `json:"kind"`
	At     int    `json:"at"`
	Count  int64  `json:"count"`
	Ours   bool   `json:"ours,omitempty"` // a fleet pilot held an enemy
	OnUs   bool   `json:"on_us,omitempty"`
}

// fleetDetailMoment is a stretch of the battle worth stopping on.
type fleetDetailMoment struct {
	Seq    int               `json:"seq"`
	Kind   string            `json:"kind"`
	At     int               `json:"at"`
	End    int               `json:"end"`
	Title  string            `json:"title"`
	Ship   int64             `json:"ship,omitempty"` // a kill or loss: the hull that died
	Mail   int64             `json:"mail,omitempty"`
	Detail fleetMomentDetail `json:"detail"`
	Brief  fleetMomentBrief  `json:"brief"`
}

// fleetMomentBrief is a moment's timeline card.
type fleetMomentBrief struct {
	Out    int64        `json:"out"`
	In     int64        `json:"in"`
	Reps   int64        `json:"rep"`
	Cap    int64        `json:"cap"`
	TopIn  *momentParty `json:"top_in,omitempty"`
	TopOut *momentParty `json:"top_out,omitempty"`
}

// fleetMomentDetail is what a moment's panel shows.
type fleetMomentDetail struct {
	Summary   string              `json:"summary"`
	From      int                 `json:"from"`
	To        int                 `json:"to"`
	Out       int64               `json:"out"`
	In        int64               `json:"in"`
	Reps      int64               `json:"reps"`
	CapIn     int64               `json:"cap_in"`
	Shooters  []momentParty       `json:"shooters,omitempty"` // our pilots dealing damage
	Targets   []momentParty       `json:"targets,omitempty"`  // enemies taking it
	Attack    []momentParty       `json:"attack,omitempty"`   // enemies dealing damage to us
	Hurt      []momentParty       `json:"hurt,omitempty"`     // our pilots taking it
	Support   []momentParty       `json:"support,omitempty"`  // our pilots repairing
	Neuts     []momentParty       `json:"neuts,omitempty"`    // enemies draining us
	Kills     []momentKillRow     `json:"kills,omitempty"`
	Tackles   []fleetDetailTackle `json:"tackles,omitempty"`
	Attackers []fleetMailHand     `json:"attackers,omitempty"` // a kill or loss: who was on the mail
	// FromMails: no log covers this window, so its lists come from the
	// killmails in it. MailOut is the fleet's damage on those kills,
	// MailIn the damage on its losses.
	FromMails bool  `json:"from_mails,omitempty"`
	MailOut   int64 `json:"mail_out,omitempty"`
	MailIn    int64 `json:"mail_in,omitempty"`
	OnMail    int   `json:"on_mail,omitempty"`   // a kill: the fleet's pilots on its mail
	HandsAll  int   `json:"hands_all,omitempty"` // a kill or loss: everyone on its mail
	// The window on a small chart with some lead either side, in
	// momentSparkW by momentSparkH: the fleet's damage dealt, taken and
	// repaired, or with no log the ISK destroyed and lost so far, and
	// where the window starts on it and how wide it is.
	SparkOut  string  `json:"spark_out,omitempty"`
	SparkIn   string  `json:"spark_in,omitempty"`
	SparkReps string  `json:"spark_reps,omitempty"`
	SparkISK  bool    `json:"spark_isk,omitempty"`
	MarkA     float64 `json:"mark_a,omitempty"`
	MarkB     float64 `json:"mark_b,omitempty"`
}

// fleetMailHand is one character on a killmail, from the mail itself.
type fleetMailHand struct {
	Character int64 `json:"character"`
	Ship      int64 `json:"ship,omitempty"`
	Weapon    int64 `json:"weapon,omitempty"`
	Corp      int64 `json:"corp,omitempty"`
	Alliance  int64 `json:"alliance,omitempty"`
	Damage    int64 `json:"damage"`
	Pct       int64 `json:"pct"`
	Ours      bool  `json:"ours,omitempty"`
	FinalBlow bool  `json:"fb,omitempty"`
}

// fleetFinding is one plain sentence the numbers support.
type fleetFinding struct {
	Rule string `json:"rule"`
	Text string `json:"text"`
}

// fleetEnemyAcc is an enemy being merged across logs.
type fleetEnemyAcc struct {
	fleetDetailParty
	shooters, hit map[int64]bool
	seen          bool
	// From the killmails, for an enemy no log saw.
	mailed                bool
	mailTo, mailFrom      int64
	mailFirst, mailLast   int
	mailShooter, mailHits map[int64]bool
}

// fleetEnemyKey finds the enemy a killmail names among those the logs
// merged: by character, else by the character's name (a log that knew
// only the name), else a ship with no pilot by its type. New is the key
// to file it under when nothing matched.
func fleetEnemyKey(x fleetDetailInput, enemies map[string]*fleetEnemyAcc, char, ship int64) (key string, found bool) {
	if char > 0 {
		key = fmt.Sprintf("c:%d", char)
		if enemies[key] != nil {
			return key, true
		}
		if x.Name != nil {
			if n := x.Name(char); n != "" && enemies["n:"+strings.ToLower(n)] != nil {
				return "n:" + strings.ToLower(n), true
			}
		}
		return key, false
	}
	name := "An unnamed ship"
	if x.Type != nil && ship != 0 {
		name = x.Type(ship)
	}
	key = "h:" + name + "|" + name
	return key, enemies[key] != nil
}

// fleetMailHands is everyone on a mail, the most damage first. A mail
// read without its hands (an older read, the demo's) has our own only.
func fleetMailHands(m *fleetMail) []fleetMailHandRow {
	if len(m.Hands) > 0 {
		return m.Hands
	}
	out := make([]fleetMailHandRow, 0, len(m.Attackers))
	for _, a := range m.Attackers {
		out = append(out, fleetMailHandRow{Character: a.Character, Ship: a.Ship, Damage: a.Damage})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Damage > out[j].Damage })
	return out
}

// fleetMailOurs says a hand on a mail is one of the fleet's pilots,
// flying with it when the ship died.
func fleetMailOurs(x fleetDetailInput, m *fleetMail, char int64) bool {
	p, ok := x.On[char]
	return ok && char > 0 && p.holds(m.At)
}

// fleetStepTop is who did the most each way in one step.
type fleetStepTop struct {
	in, out map[string]int64
	rep     map[int64]int64
}

// fleetDetailInput is what a battle's explorer is built from.
type fleetDetailInput struct {
	Cluster *fleetCluster
	Counted map[int64][]gamelog.StepRange
	On      map[int64]fleetPresence
	Ships   map[int64]int64
	Merge   fleetMerge
	Name    func(int64) string // a character's name
	Type    func(int64) string // a type's name
}

// buildFleetDetail builds one battle's explorer. Pure: the worker and the
// demo seed hand it what they read.
func buildFleetDetail(x fleetDetailInput) fleetDetail {
	c := x.Cluster
	first := gamelog.ClockStep(c.From.Unix())
	last := gamelog.ClockStep(c.To.Unix())
	for at := range x.Merge.Steps {
		first, last = min(first, at), max(last, at)
	}
	step := int64(gamelog.SliceSeconds)
	n := int((last-first)/step) + 1
	d := fleetDetail{
		Version: fleetDetailVersion, Start: first, Out: make([]int64, n), In: make([]int64, n), Reps: make([]int64, n),
		CapIn: make([]int64, n), CapOut: make([]int64, n),
	}
	idx := func(at int64) int { return int((at - first) / step) }
	sec := func(t time.Time) int { return int(t.Unix() - first) }

	type pilotAcc struct {
		fleetDetailPilot
		out, in []int64
	}
	pilots := map[int64]*pilotAcc{}
	touch := func(id int64) *pilotAcc {
		p := pilots[id]
		if p == nil {
			p = &pilotAcc{fleetDetailPilot: fleetDetailPilot{ID: id, Ship: x.Ships[id]}}
			pilots[id] = p
		}
		return p
	}
	for id, p := range x.On {
		if p.overlaps(c.From, c.To) {
			touch(id)
		}
	}
	enemies := map[string]*fleetEnemyAcc{}
	// Per step, who did the most each way, for the chart's card.
	tops := make([]fleetStepTop, n)

	for _, r := range c.Reads {
		presence, present := x.On[r.Character]
		if !r.Usable || !present {
			continue
		}
		for _, sl := range r.Steps {
			if !stepCounted(x.Counted[r.ID], sl.At) || !presence.holdsStep(sl.At) {
				continue
			}
			i := idx(sl.At)
			if i < 0 || i >= n {
				continue
			}
			pilot := touch(r.Character)
			pilot.HasLog = true
			if pilot.out == nil {
				pilot.out, pilot.in = make([]int64, n), make([]int64, n)
			}
			for _, row := range sl.Rows {
				mate := r.Fleetmates[row[0]]
				if mate == r.Character {
					continue
				}
				pilot.Reps += row[3]
				d.Reps[i] += row[3]
				if row[3] > 0 {
					if tops[i].rep == nil {
						tops[i].rep = map[int64]int64{}
					}
					tops[i].rep[r.Character] += row[3]
				}
				if mate != 0 {
					continue // friendly fire, in no total
				}
				party := r.Parties[row[0]]
				if party.Name == "" && party.Hull == "" {
					party.Name = "An unnamed ship"
				}
				key := party.key()
				e := enemies[key]
				if e == nil {
					e = &fleetEnemyAcc{fleetDetailParty: fleetDetailParty{
						Key: key, Name: party.Name, CharacterID: party.CharacterID, Ticker: party.Ticker,
						Hull: party.Hull, HullTypeID: party.HullTypeID, Pilot: party.Pilot, First: -1,
					}, shooters: map[int64]bool{}, hit: map[int64]bool{}}
					enemies[key] = e
				}
				if e.HullTypeID == 0 && party.HullTypeID != 0 {
					e.Hull, e.HullTypeID = party.Hull, party.HullTypeID
				}
				if e.Ticker == "" {
					e.Ticker = party.Ticker
				}
				out, in, repIn, capOut, capIn := row[1], row[2], row[4], row[5], row[6]
				pilot.Out += out
				pilot.In += in
				pilot.CapIn += capIn
				pilot.out[i] += out
				pilot.in[i] += in
				d.Out[i] += out
				d.In[i] += in
				d.CapIn[i] += capIn
				d.CapOut[i] += capOut
				e.FromUs += out
				e.ToUs += in
				e.CapOnUs += capIn
				e.RepsToUs += repIn
				if out > 0 {
					e.shooters[r.Character] = true
				}
				if in > 0 {
					e.hit[r.Character] = true
				}
				if out+in+capIn+repIn > 0 {
					at := int(sl.At - first)
					if e.First < 0 || at < e.First {
						e.First = at
					}
					e.Last = max(e.Last, at+int(step))
					e.seen = true
				}
				if in > 0 || out > 0 {
					if tops[i].in == nil {
						tops[i].in, tops[i].out = map[string]int64{}, map[string]int64{}
					}
					tops[i].in[key] += in
					tops[i].out[key] += out
				}
			}
		}
	}

	// The mails: who died, who was on what. Logs come first: a pilot or
	// an enemy a log saw keeps the log's figures, and the mails fill in
	// only what no log covers, and who died.
	enemyOf := func(char, ship int64) *fleetEnemyAcc {
		key, found := fleetEnemyKey(x, enemies, char, ship)
		if found {
			e := enemies[key]
			if char > 0 && e.CharacterID == 0 {
				e.CharacterID = char
			}
			return e
		}
		e := &fleetEnemyAcc{fleetDetailParty: fleetDetailParty{Key: key, CharacterID: char, Pilot: char > 0, First: -1},
			shooters: map[int64]bool{}, hit: map[int64]bool{}}
		switch {
		case char > 0 && x.Name != nil:
			e.Name = x.Name(char)
		case x.Type != nil && ship != 0:
			e.Name = x.Type(ship)
		default:
			e.Name = "An unnamed ship"
		}
		enemies[key] = e
		return e
	}
	mailSaw := func(e *fleetEnemyAcc, at int, ship, corp, alliance int64) {
		if !e.mailed {
			e.mailed, e.mailFirst, e.mailLast = true, at, at
			e.mailShooter, e.mailHits = map[int64]bool{}, map[int64]bool{}
		}
		e.mailFirst, e.mailLast = min(e.mailFirst, at), max(e.mailLast, at)
		if e.HullTypeID == 0 && ship != 0 {
			e.HullTypeID = ship
			if x.Type != nil {
				e.Hull = x.Type(ship)
			}
		}
		if e.CorpID == 0 {
			e.CorpID, e.AllianceID = corp, alliance
		}
	}
	mailOut, mailIn := map[int64][]int64{}, map[int64][]int64{}
	lane := func(m map[int64][]int64, id int64, i int, v int64) {
		if i < 0 || i >= n || v <= 0 {
			return
		}
		if m[id] == nil {
			m[id] = make([]int64, n)
		}
		m[id][i] += v
	}
	for _, m := range c.Mails {
		at, i := sec(m.At), idx(gamelog.ClockStep(m.At.Unix()))
		hands := fleetMailHands(m)
		if m.Loss {
			p := touch(m.Victim)
			p.Died, p.LossMail, p.DiedAt = true, m.ID, at
			if m.Ship != 0 {
				p.Ship = m.Ship
			}
			lane(mailIn, m.Victim, i, m.Damage)
			for _, a := range hands {
				if fleetMailOurs(x, m, a.Character) {
					continue // a fleetmate on its own loss: in no total
				}
				e := enemyOf(a.Character, a.Ship)
				mailSaw(e, at, a.Ship, a.Corp, a.Alliance)
				e.mailTo += a.Damage
				e.mailHits[m.Victim] = true
			}
			continue
		}
		var ours int64
		var shooters []int64
		for _, a := range hands {
			if !fleetMailOurs(x, m, a.Character) {
				continue
			}
			p := touch(a.Character)
			p.OnMails++
			if p.Ship == 0 {
				p.Ship = a.Ship
			}
			lane(mailOut, a.Character, i, a.Damage)
			ours += a.Damage
			shooters = append(shooters, a.Character)
		}
		if m.Victim == 0 && m.Ship == 0 {
			continue
		}
		e := enemyOf(m.Victim, m.Ship)
		e.Died, e.KillMail = true, m.ID
		if m.Ship != 0 && (e.HullTypeID == 0 || !e.seen) {
			e.HullTypeID = m.Ship
			if x.Type != nil {
				e.Hull = x.Type(m.Ship)
			}
		}
		mailSaw(e, at, m.Ship, m.VictimCorp, m.VictimAlliance)
		e.mailFrom += ours
		for _, id := range shooters {
			e.mailShooter[id] = true
		}
	}
	for id, p := range pilots {
		if p.HasLog {
			d.Logged++
			continue
		}
		for _, v := range mailOut[id] {
			p.Out += v
		}
		for _, v := range mailIn[id] {
			p.In += v
		}
		p.FromMails = p.Out+p.In > 0
		if mailOut[id] != nil || mailIn[id] != nil {
			p.out, p.in = mailOut[id], mailIn[id]
			if p.out == nil {
				p.out = make([]int64, n)
			}
			if p.in == nil {
				p.in = make([]int64, n)
			}
		}
	}
	for _, e := range enemies {
		if !e.mailed {
			continue
		}
		if !e.seen {
			e.ToUs, e.FromUs, e.FromMails = e.mailTo, e.mailFrom, true
			e.First, e.Last = e.mailFirst, e.mailLast
			for id := range e.mailShooter {
				e.shooters[id] = true
			}
			for id := range e.mailHits {
				e.hit[id] = true
			}
		}
	}
	if d.Logged == 0 && len(c.Mails) > 0 {
		d.IskKill, d.IskLoss = make([]int64, n), make([]int64, n)
		for _, m := range c.Mails {
			i := idx(gamelog.ClockStep(m.At.Unix()))
			if i < 0 || i >= n || !m.Value.Valid {
				continue
			}
			if m.Loss {
				d.IskLoss[i] += int64(m.Value.Float64)
			} else {
				d.IskKill[i] += int64(m.Value.Float64)
			}
		}
		for i := 1; i < n; i++ {
			d.IskKill[i] += d.IskKill[i-1]
			d.IskLoss[i] += d.IskLoss[i-1]
		}
	}

	// Totals and the busiest step.
	for i := range d.In {
		d.CapOnUs += d.CapIn[i]
		d.OurCap += d.CapOut[i]
		if per := d.In[i] / step; per > d.PeakIn {
			d.PeakIn, d.PeakAt = per, i*int(step)
		}
	}

	// Pilots: most damage first, lanes for those with logs.
	cols := min(n, fleetDetailColumns)
	d.LaneColumns = cols
	bucket := func(src []int64) []int64 {
		out := make([]int64, cols)
		for i, v := range src {
			out[i*cols/n] += v
		}
		return out
	}
	for _, p := range pilots {
		dp := p.fleetDetailPilot
		d.Pilots = append(d.Pilots, dp)
	}
	sort.Slice(d.Pilots, func(i, j int) bool {
		a, b := d.Pilots[i], d.Pilots[j]
		if a.Out+a.Reps != b.Out+b.Reps {
			return a.Out+a.Reps > b.Out+b.Reps
		}
		return a.ID < b.ID
	})
	laned := 0
	for i := range d.Pilots {
		p := pilots[d.Pilots[i].ID]
		if p.out != nil && laned < fleetDetailLanes {
			d.Pilots[i].LaneOut, d.Pilots[i].LaneIn = bucket(p.out), bucket(p.in)
			laned++
		}
	}

	// Enemies: the most involved first; third parties that only
	// repaired the fleet are allies.
	var hostile []fleetDetailParty
	for _, e := range enemies {
		if !e.seen && !e.Died && !e.mailed {
			continue
		}
		e.Shooters, e.Hit = len(e.shooters), len(e.hit)
		if e.First < 0 {
			e.First = 0
		}
		if e.ToUs == 0 && e.FromUs == 0 && e.CapOnUs == 0 && e.RepsToUs > 0 {
			d.Allies = append(d.Allies, e.fleetDetailParty)
			continue
		}
		hostile = append(hostile, e.fleetDetailParty)
	}
	sort.Slice(hostile, func(i, j int) bool {
		a, b := hostile[i], hostile[j]
		if a.ToUs+a.FromUs != b.ToUs+b.FromUs {
			return a.ToUs+a.FromUs > b.ToUs+b.FromUs
		}
		return a.Key < b.Key
	})
	d.EnemyCount = len(hostile)
	d.Enemies = hostile[:min(len(hostile), fleetDetailEnemies)]

	d.WeaponsOut, d.WeaponsIn, d.Tackles, d.WeaponLogs = fleetWholeTables(x, first)
	if d.Logged > 0 {
		d.Hover = fleetHover(d, tops, enemies, x, first)
	} else if len(d.IskKill) == n {
		d.Hover = fleetIskHover(d, x, first)
	}
	if d.WeaponLogs == 0 {
		d.WeaponsOut, d.WeaponsIn = fleetMailWeapons(x)
		d.WeaponsFromMails = len(d.WeaponsOut)+len(d.WeaponsIn) > 0
	}
	fleetCardWeapons(x, enemies, &d)
	d.Moments = fleetMoments(x, d, enemies, first)
	d.Findings = fleetFindings(x, d)
	return d
}

// fleetMailWeapons is the weapons table when no log was read in full:
// each weapon the killmails list, with the damage they credit it, the
// fleet's on its kills and the enemy's on its losses.
func fleetMailWeapons(x fleetDetailInput) (out, in []fleetDetailWeapon) {
	type acc struct {
		damage int64
		users  map[int64]bool
	}
	ours, theirs := map[int64]*acc{}, map[int64]*acc{}
	for _, m := range x.Cluster.Mails {
		for _, a := range fleetMailHands(m) {
			if a.Weapon == 0 {
				continue
			}
			mine := fleetMailOurs(x, m, a.Character)
			var into map[int64]*acc
			switch {
			case m.Loss && !mine:
				into = theirs
			case !m.Loss && mine:
				into = ours
			default:
				continue
			}
			w := into[a.Weapon]
			if w == nil {
				w = &acc{users: map[int64]bool{}}
				into[a.Weapon] = w
			}
			w.damage += a.Damage
			who := a.Character
			if who == 0 {
				who = -a.Ship // a ship with no pilot counts once per type
			}
			w.users[who] = true
		}
	}
	list := func(m map[int64]*acc) []fleetDetailWeapon {
		var rows []fleetDetailWeapon
		for id, w := range m {
			name := ""
			if x.Type != nil {
				name = x.Type(id)
			}
			rows = append(rows, fleetDetailWeapon{Weapon: name, TypeID: id, Users: len(w.users), Damage: w.damage})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Damage != rows[j].Damage {
				return rows[i].Damage > rows[j].Damage
			}
			return rows[i].TypeID < rows[j].TypeID
		})
		return rows[:min(len(rows), fleetDetailWeapons)]
	}
	return list(ours), list(theirs)
}

// fleetReadWhole says a read counts in full: every step it holds is the
// one counted for its pilot, so its whole-fight tables add nothing twice.
func fleetReadWhole(r *fleetLogRead, counted []gamelog.StepRange) bool {
	from, to := gamelog.ClockStep(r.Start.Unix()), gamelog.ClockStep(r.End.Unix())
	for at := from; at <= to; at += int64(gamelog.SliceSeconds) {
		if !stepCounted(counted, at) {
			return false
		}
	}
	return true
}

// fleetWholeTables merges the weapons and tackle of the reads counted in
// full: the fleet's weapons on enemies, the enemy's on the fleet, and
// every hold seen on grid.
func fleetWholeTables(x fleetDetailInput, first int64) (out, in []fleetDetailWeapon, tackles []fleetDetailTackle, logs int) {
	type wkey struct {
		name string
		id   int64
	}
	type wacc struct {
		fleetDetailWeapon
		users map[string]bool
	}
	outs, ins := map[wkey]*wacc{}, map[wkey]*wacc{}
	holds := map[string]*fleetDetailTackle{}
	partyByName := func(r *fleetLogRead, name string) (fleetLogParty, bool) {
		for _, p := range r.Parties {
			if strings.EqualFold(p.Name, name) {
				return p, true
			}
		}
		return fleetLogParty{}, false
	}
	for _, r := range x.Cluster.Reads {
		if _, present := x.On[r.Character]; !present || !r.Usable || !fleetReadWhole(r, x.Counted[r.ID]) {
			continue
		}
		if len(r.Weapons) > 0 || len(r.Tackles) > 0 {
			logs++
		}
		for _, w := range r.Weapons {
			if x.Cluster.Reads != nil && r.Fleetmates[int64(w.PartySeq)] != 0 {
				continue // a fleetmate: friendly fire
			}
			k := wkey{strings.ToLower(w.Weapon), w.WeaponTypeID}
			list, user := outs, fmt.Sprintf("c:%d", r.Character)
			if w.Dir == "in" {
				list, user = ins, r.Parties[int64(w.PartySeq)].key()
			}
			a := list[k]
			if a == nil {
				a = &wacc{fleetDetailWeapon: fleetDetailWeapon{Weapon: w.Weapon, TypeID: w.WeaponTypeID}, users: map[string]bool{}}
				list[k] = a
			}
			a.users[user] = true
			a.Shots += w.Shots
			a.Damage += w.Damage
			for i, q := range [7]int64{w.QMiss, w.QGraze, w.QGlance, w.QHit, w.QPenetrate, w.QSmash, w.QWreck} {
				a.Quality[i] += q
			}
		}
		for _, t := range r.Tackles {
			h := fleetDetailTackle{From: t.FromName, To: t.ToName, Kind: t.Kind, At: int(t.FirstAt.Unix() - first), Count: t.Attempts}
			if strings.EqualFold(t.FromName, "you") {
				h.FromID, h.Ours = r.Character, true
			} else if p, ok := partyByName(r, t.FromName); ok {
				if mate := fleetMateOf(r, p); mate != 0 {
					h.FromID, h.Ours = mate, true
				} else {
					h.From = fleetPartyLabel(p)
				}
			}
			if strings.EqualFold(t.ToName, "you") {
				h.ToID, h.OnUs = r.Character, true
			} else if p, ok := partyByName(r, t.ToName); ok {
				if mate := fleetMateOf(r, p); mate != 0 {
					h.ToID, h.OnUs = mate, true
				} else {
					h.To = fleetPartyLabel(p)
				}
			}
			if h.FromID != 0 && x.Name != nil {
				h.From = x.Name(h.FromID)
			}
			if h.ToID != 0 && x.Name != nil {
				h.To = x.Name(h.ToID)
			}
			k := strings.ToLower(h.From + "|" + h.To + "|" + h.Kind)
			if had := holds[k]; had != nil {
				had.At = min(had.At, h.At)
				had.Count = max(had.Count, h.Count)
				continue
			}
			holds[k] = &h
		}
	}
	collect := func(m map[wkey]*wacc) []fleetDetailWeapon {
		var list []fleetDetailWeapon
		for _, a := range m {
			a.Users = len(a.users)
			list = append(list, a.fleetDetailWeapon)
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Damage != list[j].Damage {
				return list[i].Damage > list[j].Damage
			}
			return list[i].Weapon < list[j].Weapon
		})
		return list[:min(len(list), fleetDetailWeapons)]
	}
	for _, h := range holds {
		tackles = append(tackles, *h)
	}
	sort.Slice(tackles, func(i, j int) bool {
		if tackles[i].At != tackles[j].At {
			return tackles[i].At < tackles[j].At
		}
		return tackles[i].From+tackles[i].To < tackles[j].From+tackles[j].To
	})
	return collect(outs), collect(ins), tackles[:min(len(tackles), fleetDetailTackles)], logs
}

// fleetMateOf is the fleet pilot a party of a log is, if it is one.
func fleetMateOf(r *fleetLogRead, p fleetLogParty) int64 {
	for seq, q := range r.Parties {
		if q == p {
			return r.Fleetmates[seq]
		}
	}
	return 0
}

// fleetPartyLabel names a party as a list line does.
func fleetPartyLabel(p fleetLogParty) string {
	if p.Pilot && p.Hull != "" && p.Hull != p.Name {
		return p.Name + " (" + p.Hull + ")"
	}
	return p.Name
}

// fleetHover is the chart's card for each column: the rates, who hit
// the fleet hardest, whom it hit hardest, its top repairer, and the
// mails that landed there.
func fleetHover(d fleetDetail, tops []fleetStepTop, enemies map[string]*fleetEnemyAcc, x fleetDetailInput, first int64) []battleHoverPt {
	n := len(d.Out)
	step := int64(gamelog.SliceSeconds)
	party := func(m map[string]int64) *hoverParty {
		best, bestKey := int64(0), ""
		for k, v := range m {
			if v > best || (v == best && v > 0 && k < bestKey) {
				best, bestKey = v, k
			}
		}
		if best == 0 {
			return nil
		}
		e := enemies[bestKey]
		return &hoverParty{Name: e.Name, Hull: e.Hull, Ship: e.HullTypeID, Amt: best}
	}
	out := make([]battleHoverPt, 0, n)
	for i := 0; i < n; i++ {
		pt := battleHoverPt{
			T: fleetClock(first + int64(i)*step), Out: d.Out[i] / step, In: d.In[i] / step, RepIn: d.Reps[i] / step, Cap: d.CapIn[i] / step,
		}
		pt.TopIn, pt.TopOt = party(tops[i].in), party(tops[i].out)
		best, who := int64(0), int64(0)
		for id, v := range tops[i].rep {
			if v > best || (v == best && id < who) {
				best, who = v, id
			}
		}
		if best > 0 && x.Name != nil {
			pt.TopRp = &hoverParty{Name: x.Name(who), Ship: x.Ships[who], Amt: best}
		}
		lo, hi := first+int64(i)*step, first+int64(i+1)*step
		for _, m := range x.Cluster.Mails {
			if at := m.At.Unix(); at >= lo && at < hi {
				pt.Mails = append(pt.Mails, hoverMail{Ship: m.Ship, Loss: m.Loss, Text: fleetMailLine(x, m)})
			}
		}
		out = append(out, pt)
	}
	return out
}

// fleetMailLine says what a mail was: "Bram Tollan lost a Tengu".
func fleetMailLine(x fleetDetailInput, m *fleetMail) string {
	who := "A pilot"
	if m.Victim != 0 && x.Name != nil {
		if n := x.Name(m.Victim); n != "" {
			who = n
		}
	}
	ship := "ship"
	if x.Type != nil && m.Ship != 0 {
		ship = x.Type(m.Ship)
	}
	return fmt.Sprintf("%s lost a %s", who, ship)
}

// fleetWindow sums every counted row of the battle's logs in one window
// of seconds: who on each side did what, the way a moment's panel lists
// it.
func fleetWindow(x fleetDetailInput, d fleetDetail, enemies map[string]*fleetEnemyAcc, from, to int) fleetMomentDetail {
	first := d.Start
	lo, hi := first+int64(from), first+int64(to)
	w := fleetMomentDetail{From: from, To: to}
	shooters, hurt, support := map[int64]int64{}, map[int64]int64{}, map[int64]int64{}
	targets, attack, neuts := map[string]int64{}, map[string]int64{}, map[string]int64{}
	for _, r := range x.Cluster.Reads {
		presence, present := x.On[r.Character]
		if !r.Usable || !present {
			continue
		}
		for _, sl := range r.Steps {
			if sl.At < lo-int64(gamelog.SliceSeconds)+1 || sl.At >= hi || !stepCounted(x.Counted[r.ID], sl.At) || !presence.holdsStep(sl.At) {
				continue
			}
			for _, row := range sl.Rows {
				mate := r.Fleetmates[row[0]]
				if mate == r.Character {
					continue
				}
				if row[3] > 0 {
					support[r.Character] += row[3]
					w.Reps += row[3]
				}
				if mate != 0 {
					continue
				}
				key := r.Parties[row[0]].key()
				w.Out += row[1]
				w.In += row[2]
				w.CapIn += row[6]
				shooters[r.Character] += row[1]
				hurt[r.Character] += row[2]
				targets[key] += row[1]
				attack[key] += row[2]
				neuts[key] += row[6]
			}
		}
	}
	// No log covers the window: its lists come from the killmails in it.
	if w.Out+w.In+w.Reps == 0 {
		for _, m := range x.Cluster.Mails {
			if at := int(m.At.Unix() - first); at < from || at > to {
				continue
			}
			w.FromMails = true
			hands := fleetMailHands(m)
			if m.Loss {
				hurt[m.Victim] += m.Damage
				w.MailIn += m.Damage
				for _, a := range hands {
					if fleetMailOurs(x, m, a.Character) {
						continue
					}
					if key, ok := fleetEnemyKey(x, enemies, a.Character, a.Ship); ok {
						attack[key] += a.Damage
					}
				}
				continue
			}
			var dealt int64
			for _, a := range hands {
				if fleetMailOurs(x, m, a.Character) {
					shooters[a.Character] += a.Damage
					dealt += a.Damage
				}
			}
			w.MailOut += dealt
			if key, ok := fleetEnemyKey(x, enemies, m.Victim, m.Ship); ok {
				targets[key] += dealt
			}
		}
	}
	ours := func(m map[int64]int64) []momentParty {
		var total int64
		for _, v := range m {
			total += v
		}
		var list []momentParty
		for id, v := range m {
			if v <= 0 {
				continue
			}
			name := ""
			if x.Name != nil {
				name = x.Name(id)
			}
			list = append(list, momentParty{Name: name, CharacterID: id, HullTypeID: x.Ships[id], Pilot: true, Amount: v, Pct: v * 100 / max(total, 1)})
		}
		return topMomentParties(list)
	}
	theirs := func(m map[string]int64) []momentParty {
		var total int64
		for _, v := range m {
			total += v
		}
		var list []momentParty
		for k, v := range m {
			e := enemies[k]
			if v <= 0 || e == nil {
				continue
			}
			list = append(list, momentParty{Name: e.Name, Hull: e.Hull, HullTypeID: e.HullTypeID, CharacterID: e.CharacterID, Ticker: e.Ticker, Pilot: e.Pilot, Died: e.Died, Amount: v, Pct: v * 100 / max(total, 1)})
		}
		return topMomentParties(list)
	}
	w.Shooters, w.Hurt, w.Support = ours(shooters), ours(hurt), ours(support)
	w.Targets, w.Attack, w.Neuts = theirs(targets), theirs(attack), theirs(neuts)
	for _, m := range x.Cluster.Mails {
		if at := int(m.At.Unix() - first); at >= from && at <= to {
			victim := ""
			if x.Name != nil && m.Victim != 0 {
				victim = x.Name(m.Victim)
			}
			w.Kills = append(w.Kills, momentKillRow{KillmailID: m.ID, Victim: victim, VictimID: m.Victim, ShipTypeID: m.Ship, Loss: m.Loss, At: at})
		}
	}
	for _, t := range d.Tackles {
		if t.At >= from && t.At <= to {
			w.Tackles = append(w.Tackles, t)
		}
	}
	return w
}

// topMomentParties keeps a window's biggest parties, largest first.
func topMomentParties(list []momentParty) []momentParty {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Amount != list[j].Amount {
			return list[i].Amount > list[j].Amount
		}
		return list[i].Name < list[j].Name
	})
	return list[:min(len(list), fleetMomentRows)]
}

// fleetPeaks finds up to fleetPeaksMax local highs of a per-step series,
// a minute apart, each at least momentPeakFloorPct of the highest.
func fleetPeaks(v []int64) []int {
	f := make([]float64, len(v))
	for i, x := range v {
		f[i] = float64(x)
	}
	type pk struct {
		at int
		v  float64
	}
	var all []pk
	for i, x := range f {
		if x > 0 && (i == 0 || x >= f[i-1]) && (i == len(f)-1 || x > f[i+1]) {
			all = append(all, pk{i, x})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].v > all[j].v })
	var out []int
	for _, p := range all {
		if len(out) == fleetPeaksMax || (len(out) > 0 && p.v*100 < all[0].v*momentPeakFloorPct) {
			break
		}
		far := true
		for _, o := range out {
			if abs(o-p.at) < fleetPeakApartSteps {
				far = false
			}
		}
		if far {
			out = append(out, p.at)
		}
	}
	return out
}

// fleetMoments lays out the battle's timeline from the fleet's side:
// first contact, every kill and loss, the peaks of incoming damage,
// repairs and energy warfare, the fleet's switches of primary, and the
// first holds on its pilots. Each carries what happened in its window.
func fleetMoments(x fleetDetailInput, d fleetDetail, enemies map[string]*fleetEnemyAcc, first int64) []fleetDetailMoment {
	step := int(gamelog.SliceSeconds)
	n := len(d.Out)
	length := n * step
	clamp := func(v int) int { return min(max(v, 0), length) }
	var ms []fleetDetailMoment
	add := func(kind string, at, from, to int, title string) *fleetDetailMoment {
		ms = append(ms, fleetDetailMoment{Kind: kind, At: at, End: clamp(to), Title: title, Detail: fleetMomentDetail{From: clamp(from), To: clamp(to)}})
		return &ms[len(ms)-1]
	}
	partyName := func(k string) string {
		if e := enemies[k]; e != nil {
			if e.Pilot && e.Hull != "" && e.Hull != e.Name {
				return e.Name + " (" + e.Hull + ")"
			}
			return e.Name
		}
		return "an enemy"
	}
	for i := 0; i < n; i++ {
		if d.Out[i]+d.In[i] > 0 {
			add(momentContact, i*step, i*step, i*step+3*step, "First shots")
			break
		}
	}
	for _, m := range x.Cluster.Mails {
		at := int(m.At.Unix() - first)
		ship := "ship"
		if x.Type != nil && m.Ship != 0 {
			ship = x.Type(m.Ship)
		}
		who := ""
		if x.Name != nil && m.Victim != 0 {
			who = x.Name(m.Victim)
		}
		kind, title := momentKill, ship+" destroyed"
		if who != "" {
			title = who + "'s " + ship + " destroyed"
		}
		if m.Loss {
			kind = momentLoss
			if who != "" {
				title = who + " lost a " + ship
			} else {
				title = "A " + ship + " lost"
			}
		}
		mo := add(kind, at, at-momentKillLead, at+step, title)
		mo.Ship, mo.Mail = m.Ship, m.ID
		// Everyone on the mail, either side, the most damage first; the
		// share is of all the damage the mail lists.
		hands := fleetMailHands(m)
		mo.Detail.HandsAll = len(hands)
		total := m.Damage
		if total <= 0 {
			for _, a := range hands {
				total += a.Damage
			}
		}
		for _, a := range hands {
			if !m.Loss && fleetMailOurs(x, m, a.Character) {
				mo.Detail.OnMail++
			}
		}
		for _, a := range hands {
			mo.Detail.Attackers = append(mo.Detail.Attackers, fleetMailHand{
				Character: a.Character, Ship: a.Ship, Weapon: a.Weapon, Corp: a.Corp, Alliance: a.Alliance,
				Damage: a.Damage, Pct: a.Damage * 100 / max(total, 1), Ours: fleetMailOurs(x, m, a.Character), FinalBlow: a.FinalBlow,
			})
			if len(mo.Detail.Attackers) == 12 {
				break
			}
		}
	}
	for _, i := range fleetPeaks(d.In) {
		add(momentPressure, i*step, i*step-2*step, i*step+3*step, fmt.Sprintf("Incoming damage peaks at %s a second", battleNum(d.In[i]/int64(step))))
	}
	for _, i := range fleetPeaks(d.Reps) {
		add(momentRepairs, i*step, i*step-2*step, i*step+3*step, fmt.Sprintf("Repairs peak at %s a second", battleNum(d.Reps[i]/int64(step))))
	}
	if d.CapOnUs > 0 {
		for _, i := range fleetPeaks(d.CapIn) {
			add(momentEnergy, i*step, i*step-2*step, i*step+3*step, fmt.Sprintf("Energy warfare drains %s GJ", battleNum(d.CapIn[i])))
		}
	}
	// The fleet's primary: the enemy taking most of its damage, step by
	// step; a new one that holds two steps is a switch.
	primary, held, switches := "", 0, 0
	for i := 0; i < n && switches < fleetSwitchesMax; i++ {
		best, key := int64(0), ""
		// The step's top target, from the chart's per-step tops.
		if i < len(d.Hover) && d.Hover[i].TopOt != nil {
			for k, e := range enemies {
				if e.Name == d.Hover[i].TopOt.Name && e.Hull == d.Hover[i].TopOt.Hull {
					best, key = d.Hover[i].TopOt.Amt, k
					break
				}
			}
		}
		if best*100 < d.Out[i]*momentFocusMinPct || key == "" {
			held = 0
			continue
		}
		if key == primary {
			continue
		}
		held++
		if held < 2 {
			continue
		}
		primary, held = key, 0
		at := (i - 1) * step
		add(momentTarget, at, at, at+4*step, "The fleet's fire moves to "+partyName(key))
		switches++
	}
	tackled := 0
	for _, t := range d.Tackles {
		if !t.OnUs || tackled == fleetTacklesMax {
			continue
		}
		add(momentTackle, t.At, t.At-step, t.At+3*step, fmt.Sprintf("%s held by %s", t.To, t.From))
		tackled++
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At < ms[j].At })
	if len(ms) > fleetDetailMoments {
		ms = ms[:fleetDetailMoments]
	}
	for i := range ms {
		ms[i].Seq = i + 1
		w := fleetWindow(x, d, enemies, ms[i].Detail.From, ms[i].Detail.To)
		w.Attackers, w.OnMail, w.HandsAll = ms[i].Detail.Attackers, ms[i].Detail.OnMail, ms[i].Detail.HandsAll
		ms[i].Detail = w
		ms[i].Brief = fleetMomentBrief{Out: w.Out, In: w.In, Reps: w.Reps, Cap: w.CapIn}
		if len(w.Attack) > 0 {
			top := w.Attack[0]
			ms[i].Brief.TopIn = &top
		}
		if len(w.Targets) > 0 {
			top := w.Targets[0]
			ms[i].Brief.TopOut = &top
		}
		ms[i].Detail.Summary = fleetMomentSummary(x, ms[i], w)
		fleetMomentSpark(d, &ms[i].Detail)
	}
	return ms
}

// fleetClock is a Unix second as the EVE clock reads it.
func fleetClock(at int64) string { return time.Unix(at, 0).UTC().Format("15:04:05") }

// fleetIskHover is the chart's card per column for a battle drawn from
// its killmails: the ISK destroyed and lost so far, and the mails that
// landed in the column.
func fleetIskHover(d fleetDetail, x fleetDetailInput, first int64) []battleHoverPt {
	step := int64(gamelog.SliceSeconds)
	out := make([]battleHoverPt, 0, len(d.IskKill))
	for i := range d.IskKill {
		pt := battleHoverPt{T: fleetClock(first + int64(i)*step), IskK: d.IskKill[i], IskL: d.IskLoss[i]}
		lo, hi := first+int64(i)*step, first+int64(i+1)*step
		for _, m := range x.Cluster.Mails {
			if at := m.At.Unix(); at >= lo && at < hi {
				pt.Mails = append(pt.Mails, hoverMail{Ship: m.Ship, Loss: m.Loss, Text: fleetMailLine(x, m)})
			}
		}
		out = append(out, pt)
	}
	return out
}

// fleetCardWeaponsShown is how many weapons a pilot's or an enemy's card
// lists.
const fleetCardWeaponsShown = 3

// fleetCardWeapons gives every pilot and enemy its heaviest weapons, for
// its card. A log read in full names the weapons its pilot fired and the
// ones each enemy fired at that pilot; for anyone no such log saw, the
// killmails name the weapon each attacker is listed with and the damage
// it did there.
func fleetCardWeapons(x fleetDetailInput, enemies map[string]*fleetEnemyAcc, d *fleetDetail) {
	type wkey struct {
		name string
		id   int64
	}
	type tally map[wkey]int64
	logOurs, logTheirs := map[int64]tally{}, map[string]tally{}
	mailOurs, mailTheirs := map[int64]tally{}, map[string]tally{}
	addTo := func(m map[int64]tally, id int64, k wkey, v int64) {
		if m[id] == nil {
			m[id] = tally{}
		}
		m[id][k] += v
	}
	addKey := func(m map[string]tally, key string, k wkey, v int64) {
		if m[key] == nil {
			m[key] = tally{}
		}
		m[key][k] += v
	}
	for _, r := range x.Cluster.Reads {
		if _, present := x.On[r.Character]; !present || !r.Usable || !fleetReadWhole(r, x.Counted[r.ID]) {
			continue
		}
		for _, w := range r.Weapons {
			if w.Damage <= 0 || r.Fleetmates[int64(w.PartySeq)] != 0 {
				continue // nothing landed, or a fleetmate: friendly fire
			}
			k := wkey{w.Weapon, w.WeaponTypeID}
			if w.Dir != "in" {
				addTo(logOurs, r.Character, k, w.Damage)
				continue
			}
			party := r.Parties[int64(w.PartySeq)]
			if party.Name == "" && party.Hull == "" {
				party.Name = "An unnamed ship"
			}
			addKey(logTheirs, party.key(), k, w.Damage)
		}
	}
	for _, m := range x.Cluster.Mails {
		for _, a := range fleetMailHands(m) {
			if a.Weapon == 0 || a.Damage <= 0 {
				continue
			}
			mine := fleetMailOurs(x, m, a.Character)
			switch {
			case !m.Loss && mine:
				addTo(mailOurs, a.Character, wkey{id: a.Weapon}, a.Damage)
			case m.Loss && !mine:
				if key, found := fleetEnemyKey(x, enemies, a.Character, a.Ship); found {
					addKey(mailTheirs, key, wkey{id: a.Weapon}, a.Damage)
				}
			}
		}
	}
	heaviest := func(t tally) []fleetCardWeapon {
		var list []fleetCardWeapon
		for k, v := range t {
			name := k.name
			if name == "" && k.id != 0 && x.Type != nil {
				name = x.Type(k.id)
			}
			if name == "" {
				name = "Drones and missiles"
			}
			list = append(list, fleetCardWeapon{Name: name, TypeID: k.id, Damage: v})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Damage != list[j].Damage {
				return list[i].Damage > list[j].Damage
			}
			return list[i].Name < list[j].Name
		})
		return list[:min(len(list), fleetCardWeaponsShown)]
	}
	for i := range d.Pilots {
		t := logOurs[d.Pilots[i].ID]
		if len(t) == 0 {
			t = mailOurs[d.Pilots[i].ID]
		}
		d.Pilots[i].Weapons = heaviest(t)
	}
	for i := range d.Enemies {
		t := logTheirs[d.Enemies[i].Key]
		if len(t) == 0 {
			t = mailTheirs[d.Enemies[i].Key]
		}
		d.Enemies[i].Weapons = heaviest(t)
	}
}

// fleetMomentSpark draws a moment's window on its small chart, with
// three steps of lead either side: the fleet's damage dealt, taken and
// repaired, or with no log the ISK destroyed and lost so far. A window
// with nothing to draw has no chart.
func fleetMomentSpark(d fleetDetail, w *fleetMomentDetail) {
	step := int(gamelog.SliceSeconds)
	n := len(d.Out)
	a, b, c := d.Out, d.In, d.Reps
	isk := d.Logged == 0 && len(d.IskKill) == n
	if isk {
		a, b, c = d.IskKill, d.IskLoss, nil
	}
	lo := max(w.From/step-3, 0)
	hi := min((w.To+step-1)/step+3, n-1)
	if hi <= lo || len(a) < n || len(b) < n {
		return
	}
	var top int64
	for i := lo; i <= hi; i++ {
		top = max(top, a[i], b[i])
		if c != nil {
			top = max(top, c[i])
		}
	}
	if top <= 0 {
		return
	}
	span := float64(hi - lo)
	line := func(v []int64) string {
		var sb strings.Builder
		for i := lo; i <= hi; i++ {
			if i > lo {
				sb.WriteByte(' ')
			}
			fmt.Fprintf(&sb, "%.1f,%.1f", float64(i-lo)/span*momentSparkW, momentSparkH-float64(v[i])/float64(top)*momentSparkH)
		}
		return sb.String()
	}
	w.SparkOut, w.SparkIn, w.SparkISK = line(a), line(b), isk
	if c != nil {
		w.SparkReps = line(c)
	}
	w.MarkA = round1(max(float64(w.From)/float64(step)-float64(lo), 0) / span * momentSparkW)
	w.MarkB = round1(max(float64(w.To-w.From)/float64(step)/span*momentSparkW, 1))
}

// fleetMomentSummary says in one sentence what a moment's window held.
func fleetMomentSummary(x fleetDetailInput, m fleetDetailMoment, w fleetMomentDetail) string {
	secs := max(w.To-w.From, 1)
	var parts []string
	if w.Out > 0 {
		parts = append(parts, fmt.Sprintf("the fleet dealt %s damage", battleNum(w.Out)))
	}
	if w.In > 0 {
		parts = append(parts, fmt.Sprintf("took %s", battleNum(w.In)))
	}
	if w.Reps > 0 {
		parts = append(parts, fmt.Sprintf("repaired %s", battleNum(w.Reps)))
	}
	if len(parts) == 0 {
		if w.FromMails {
			return fleetMailSummary(x, m, w)
		}
		return "No logged damage or repairs in this window."
	}
	s := fmt.Sprintf("In these %d seconds %s.", secs, strings.Join(parts, ", "))
	if len(w.Attack) > 0 && w.Attack[0].Pct >= 40 {
		s += fmt.Sprintf(" %s did %d%% of the damage the fleet took.", fleetPartyShort(w.Attack[0]), w.Attack[0].Pct)
	}
	if len(w.Targets) > 0 && w.Targets[0].Pct >= 50 {
		s += fmt.Sprintf(" %d%% of the fleet's damage went into %s.", w.Targets[0].Pct, fleetPartyShort(w.Targets[0]))
	}
	return s
}

// fleetMailSummary says what a window's killmails show, for a window no
// log covers.
func fleetMailSummary(x fleetDetailInput, m fleetDetailMoment, w fleetMomentDetail) string {
	name := func(h fleetMailHand) string {
		who := ""
		if x.Name != nil && h.Character > 0 {
			who = x.Name(h.Character)
		}
		ship := ""
		if x.Type != nil && h.Ship != 0 {
			ship = x.Type(h.Ship)
		}
		switch {
		case who != "" && ship != "":
			return who + " (" + ship + ")"
		case who != "":
			return who
		case ship != "":
			return "a " + ship
		}
		return "an unnamed ship"
	}
	var ours, theirs []fleetMailHand
	var fb *fleetMailHand
	for i, h := range w.Attackers {
		if h.Ours {
			ours = append(ours, h)
		} else {
			theirs = append(theirs, h)
		}
		if h.FinalBlow {
			fb = &w.Attackers[i]
		}
	}
	switch m.Kind {
	case momentLoss:
		if len(theirs) == 0 {
			break
		}
		s := fmt.Sprintf("From the killmail: %s did the most damage, %d%%.", name(theirs[0]), theirs[0].Pct)
		if fb != nil && fb.Character != theirs[0].Character {
			s += " The final blow was " + name(*fb) + "'s."
		}
		return s
	case momentKill:
		if len(ours) == 0 {
			break
		}
		pilots := "pilot is"
		if w.OnMail != 1 {
			pilots = "pilots are"
		}
		return fmt.Sprintf("From the killmail: %d of the fleet's %s on it, and %s did the most damage, %d%%.", w.OnMail, pilots, name(ours[0]), ours[0].Pct)
	}
	var kills, losses int
	for _, k := range w.Kills {
		if k.Loss {
			losses++
		} else {
			kills++
		}
	}
	return fmt.Sprintf("From the killmails in this window: %s and %s.", humanize.Plural(kills, "kill"), fleetLossWord(losses))
}

func fleetPartyShort(p momentParty) string {
	if p.Pilot && p.Hull != "" && p.Hull != p.Name {
		return p.Name + "'s " + p.Hull
	}
	return p.Name
}

// fleetFindings says what the numbers support, in plain sentences.
func fleetFindings(x fleetDetailInput, d fleetDetail) []fleetFinding {
	var out []fleetFinding
	say := func(rule, text string) { out = append(out, fleetFinding{Rule: rule, Text: text}) }
	var kills, losses int
	var destroyed, lost float64
	for _, m := range x.Cluster.Mails {
		v := 0.0
		if m.Value.Valid {
			v = m.Value.Float64
		}
		if m.Loss {
			losses++
			lost += v
		} else {
			kills++
			destroyed += v
		}
	}
	if kills+losses > 0 {
		eff := ""
		if destroyed+lost > 0 {
			eff = fmt.Sprintf(" (%.0f%% of the ISK destroyed was theirs)", destroyed*100/(destroyed+lost))
		}
		say("outcome", fmt.Sprintf("%s for %s, %s ISK destroyed against %s lost%s.",
			humanize.Plural(kills, "kill"), fleetLossWord(losses), formatISKShort(destroyed), formatISKShort(lost), eff))
	}
	// Focus: how much of the fleet's damage went into what it killed.
	var onKilled, total int64
	for _, e := range d.Enemies {
		if e.FromMails {
			continue // a killmail credits only damage on the kill
		}
		total += e.FromUs
		if e.Died {
			onKilled += e.FromUs
		}
	}
	if total > 0 && kills > 0 {
		say("focus", fmt.Sprintf("%d%% of the fleet's damage went into ships it went on to kill.", onKilled*100/total))
	}
	var in, reps int64
	for i := range d.In {
		in += d.In[i]
		reps += d.Reps[i]
	}
	if in > 0 && reps > 0 {
		say("logistics", fmt.Sprintf("Repairs came to %d%% of the damage the fleet took: %s repaired against %s taken.", reps*100/in, battleNum(reps), battleNum(in)))
	}
	if d.PeakIn > 0 {
		say("pressure", fmt.Sprintf("Incoming damage peaked at %s a second, %s into the battle.", battleNum(d.PeakIn), clockOffset(d.PeakAt)))
	}
	// Whom the enemy went for.
	var topHurt fleetDetailPilot
	for _, p := range d.Pilots {
		if p.HasLog && p.In > topHurt.In {
			topHurt = p
		}
	}
	if in > 0 && topHurt.In*100 >= in*20 && x.Name != nil {
		end := "."
		if topHurt.Died {
			end = ", before going down."
		}
		say("primary", fmt.Sprintf("The enemy's fire fell hardest on %s: %d%% of everything the fleet took%s", x.Name(topHurt.ID), topHurt.In*100/in, end))
	}
	// Who did the most: from the logs when there are any, else from what
	// the killmails credit.
	var topOut fleetDetailPilot
	var out64 int64
	for _, p := range d.Pilots {
		if p.HasLog != (d.Logged > 0) {
			continue
		}
		out64 += p.Out
		if p.Out > topOut.Out {
			topOut = p
		}
	}
	switch {
	case out64 <= 0 || topOut.Out <= 0 || x.Name == nil:
	case d.Logged > 0:
		say("damage", fmt.Sprintf("%s dealt the most damage, %s, %d%% of the fleet's.", x.Name(topOut.ID), battleNum(topOut.Out), topOut.Out*100/out64))
	default:
		say("damage", fmt.Sprintf("%s did the most damage on the fleet's kills, %d%% of what their killmails credit, and is on %d of %d.",
			x.Name(topOut.ID), topOut.Out*100/out64, topOut.OnMails, kills))
	}
	// Who killed the fleet: the enemy with the most damage on its losses.
	if d.Logged == 0 && losses > 0 {
		var top fleetDetailParty
		var onLosses int64
		for _, e := range d.Enemies {
			onLosses += e.ToUs
			if e.ToUs > top.ToUs {
				top = e
			}
		}
		if top.ToUs > 0 {
			who := top.Name
			if top.Pilot && top.Hull != "" && top.Hull != top.Name {
				who += " (" + top.Hull + ")"
			}
			say("primary", fmt.Sprintf("%s did %d%% of the damage on the fleet's losses.", who, top.ToUs*100/max(onLosses, 1)))
		}
	}
	if d.CapOnUs > 0 {
		say("energy", fmt.Sprintf("Enemy energy warfare drained %s GJ from the fleet.", battleNum(d.CapOnUs)))
	}
	if len(out) > 7 {
		out = out[:7]
	}
	return out
}

func fleetLossWord(n int) string {
	if n == 0 {
		return "no losses"
	}
	if n == 1 {
		return "1 loss"
	}
	return fmt.Sprintf("%d losses", n)
}

// fleetDetailJSON builds a battle's explorer and stores it as JSON;
// empty when it cannot be written.
func fleetDetailJSON(c *fleetCluster, counted map[int64][]gamelog.StepRange, on map[int64]fleetPresence, ships map[int64]int64, name, typeName func(int64) string) string {
	d := buildFleetDetail(fleetDetailInput{
		Cluster: c, Counted: counted, On: on, Ships: ships, Name: name, Type: typeName,
		Merge: mergeFleetLogs(c.Reads, counted, on),
	})
	raw, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(raw)
}
