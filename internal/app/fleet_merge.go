package app

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"evesynapse/internal/gamelog"
)

// ---------------------------------------------------------------------------
// Merging pilots' logs into one fleet's battles. Two pilots' logs see the
// same shots from opposite ends, so nothing here adds up a stored fight's
// totals:
//
//   - kills, losses and ISK come from distinct killmails
//   - damage out and in are each pilot's own, against parties outside the fleet
//   - a hit between two fleet pilots counts once, as friendly fire, in
//     neither damage total
//   - repairs and capacitor warfare count from the giver's log only
//   - each clock step of a pilot's log counts from one stored read
//
// Figures cover the logs pilots chose to add. They are never scaled up.
// ---------------------------------------------------------------------------

const (
	// fleetBattleGap is the quiet time in a system that separates two
	// battles of one fleet.
	fleetBattleGap   = 5 * time.Minute
	fleetCurvePoints = 300
	fleetCurveW      = 600.0
	fleetCurveH      = 160.0

	// Points. A pilot earns them for flying a fleet, for each kill they
	// are on, and for their share of the damage on a kill by its value.
	// A death takes some back. A pilot's total never goes below zero.
	fleetPointsAttend    = 10
	fleetPointsKill      = 2
	fleetPointsDeath     = 2
	fleetPointsValueStep = 10_000_000.0 // ISK for one point of a whole kill
	fleetPointsValueCap  = 50.0         // the most one kill's value can earn

	// A fleet earns public attendance points only when the fleet read saw
	// the pilot for this long, in a fleet with at least this many pilots
	// seen, so a hand mark or a short empty fleet cannot farm the board.
	fleetCreditMinPresence = 20 * time.Minute
	fleetCreditMinPilots   = 5
)

// fleetPresence is when a pilot was on the fleet.
type fleetPresence struct {
	From, To time.Time
}

func (p fleetPresence) holds(at time.Time) bool {
	return !at.Before(p.From) && !at.After(p.To)
}

// holdsStep: the pilot was on the fleet for any of this clock step.
func (p fleetPresence) holdsStep(step int64) bool {
	return step+gamelog.SliceSeconds > p.From.Unix() && step <= p.To.Unix()
}

func (p fleetPresence) overlaps(from, to time.Time) bool {
	return !to.Before(p.From) && !from.After(p.To)
}

// fleetLogRead is one stored read of a pilot's log. Usable: its clock
// steps match the read now stored. Fleetmates maps a party's seq to the
// roster pilot that party is.
type fleetLogRead struct {
	ID         int64
	Character  int64
	Start, End time.Time
	Received   time.Time
	SystemID   int64
	Usable     bool
	Steps      []gamelog.AlignedSlice
	Fleetmates map[int64]int64
}

type fleetPilotDamage struct {
	Out, In, Reps int64
}

// fleetMerge is what one battle's logs add up to. Steps holds damage out,
// damage in and repairs out per clock step.
type fleetMerge struct {
	Out, In, Reps, Cap, Friendly int64
	Pilots                       map[int64]*fleetPilotDamage
	Steps                        map[int64]*[3]int64
}

// countedByRead says which clock steps each read counts for. It takes
// every usable read of the pilots, so a step is settled once even when
// the reads covering it fall in different battles.
func countedByRead(reads []*fleetLogRead) map[int64][]gamelog.StepRange {
	byPilot := map[int64][]gamelog.FightRead{}
	for _, r := range reads {
		if r.Usable {
			byPilot[r.Character] = append(byPilot[r.Character], gamelog.FightRead{ID: r.ID, Start: r.Start, End: r.End, Received: r.Received})
		}
	}
	out := map[int64][]gamelog.StepRange{}
	for _, list := range byPilot {
		for _, sr := range gamelog.CountedSteps(list) {
			out[sr.ReadID] = append(out[sr.ReadID], sr)
		}
	}
	return out
}

func stepCounted(ranges []gamelog.StepRange, step int64) bool {
	for _, r := range ranges {
		if step >= r.From && step < r.To {
			return true
		}
	}
	return false
}

// mergeFleetLogs adds up one battle's reads. A hit between two fleet
// pilots can appear in both their logs, so each direction in each step
// keeps the larger of the two sightings and counts once.
func mergeFleetLogs(reads []*fleetLogRead, counted map[int64][]gamelog.StepRange, on map[int64]fleetPresence) fleetMerge {
	out := fleetMerge{Pilots: map[int64]*fleetPilotDamage{}, Steps: map[int64]*[3]int64{}}
	type shot struct{ step, from, to int64 }
	friendly := map[shot]int64{}
	for _, r := range reads {
		presence, present := on[r.Character]
		if !r.Usable || !present {
			continue
		}
		for _, sl := range r.Steps {
			if !stepCounted(counted[r.ID], sl.At) || !presence.holdsStep(sl.At) {
				continue
			}
			pilot := out.Pilots[r.Character]
			if pilot == nil {
				pilot = &fleetPilotDamage{}
				out.Pilots[r.Character] = pilot
			}
			step := out.Steps[sl.At]
			if step == nil {
				step = &[3]int64{}
				out.Steps[sl.At] = step
			}
			for _, row := range sl.Rows {
				mate := r.Fleetmates[row[0]]
				if mate == r.Character {
					continue
				}
				if mate != 0 {
					if row[1] > 0 {
						k := shot{sl.At, r.Character, mate}
						friendly[k] = max(friendly[k], row[1])
					}
					if row[2] > 0 {
						k := shot{sl.At, mate, r.Character}
						friendly[k] = max(friendly[k], row[2])
					}
				} else {
					out.Out += row[1]
					out.In += row[2]
					pilot.Out += row[1]
					pilot.In += row[2]
					step[0] += row[1]
					step[1] += row[2]
				}
				out.Reps += row[3]
				out.Cap += row[5]
				pilot.Reps += row[3]
				step[2] += row[3]
			}
		}
	}
	for _, v := range friendly {
		out.Friendly += v
	}
	return out
}

// fleetMailAttacker is a fleet pilot on a mail, on the fleet when it fell.
type fleetMailAttacker struct {
	Character, Ship, Damage int64
}

// fleetMail is a killmail that belongs to the fleet. Loss: the victim was
// on the fleet when the ship died.
type fleetMail struct {
	ID        int64
	At        time.Time
	System    int64
	Victim    int64
	Ship      int64
	Damage    int64
	Value     sql.NullFloat64
	Counts    bool
	Loss      bool
	Attackers []fleetMailAttacker
}

// fleetScoreMail is what one mail is worth to one pilot.
type fleetScoreMail struct {
	Character   int64
	Mail        int64
	At          time.Time
	Loss        bool
	Counts      bool
	Value       float64
	ValuePoints float64
}

// fleetMailScores is the mail's worth to each fleet pilot on it. On a
// loss the victim takes the death and fleet pilots who shot them earn
// nothing. Value points follow the pilot's share of the damage.
func fleetMailScores(m *fleetMail) []fleetScoreMail {
	value := 0.0
	if m.Value.Valid {
		value = m.Value.Float64
	}
	if m.Loss {
		return []fleetScoreMail{{Character: m.Victim, Mail: m.ID, At: m.At, Loss: true, Counts: m.Counts, Value: value}}
	}
	whole := min(value/fleetPointsValueStep, fleetPointsValueCap)
	out := make([]fleetScoreMail, 0, len(m.Attackers))
	for _, a := range m.Attackers {
		share := 0.0
		if m.Damage > 0 {
			share = min(float64(a.Damage)/float64(m.Damage), 1)
		}
		out = append(out, fleetScoreMail{Character: a.Character, Mail: m.ID, At: m.At, Counts: m.Counts, Value: value, ValuePoints: whole * share})
	}
	return out
}

// fleetCluster is one battle: a system and a stretch of time.
type fleetCluster struct {
	System   int64
	From, To time.Time
	Mails    []*fleetMail
	Reads    []*fleetLogRead
}

func (c *fleetCluster) grow(from, to time.Time) {
	if from.Before(c.From) {
		c.From = from
	}
	if to.After(c.To) {
		c.To = to
	}
}

// clusterFleetBattles cuts a fleet's mails and logs into battles by
// system and quiet gaps, so a roam gets one report per fight. A log with
// no known system joins the battle it overlaps most, or stands alone.
// A log counts only for the time its pilot was on the fleet, so a long
// log cannot stretch a battle or join two apart.
func clusterFleetBattles(mails []*fleetMail, reads []*fleetLogRead, on map[int64]fleetPresence) []*fleetCluster {
	type span struct {
		from, to time.Time
		mail     *fleetMail
		read     *fleetLogRead
	}
	onFleet := func(r *fleetLogRead) (time.Time, time.Time) {
		from, to := r.Start, r.End
		if p, ok := on[r.Character]; ok {
			if p.From.After(from) {
				from = p.From
			}
			if p.To.Before(to) {
				to = p.To
			}
		}
		return from, to
	}
	bySystem := map[int64][]span{}
	var unplaced []*fleetLogRead
	for _, m := range mails {
		bySystem[m.System] = append(bySystem[m.System], span{from: m.At, to: m.At, mail: m})
	}
	for _, r := range reads {
		if r.SystemID == 0 {
			unplaced = append(unplaced, r)
			continue
		}
		from, to := onFleet(r)
		bySystem[r.SystemID] = append(bySystem[r.SystemID], span{from: from, to: to, read: r})
	}
	var out []*fleetCluster
	cut := func(system int64, list []span) {
		slices.SortStableFunc(list, func(a, b span) int { return a.from.Compare(b.from) })
		var cur *fleetCluster
		for _, s := range list {
			if cur == nil || s.from.Sub(cur.To) > fleetBattleGap {
				cur = &fleetCluster{System: system, From: s.from, To: s.to}
				out = append(out, cur)
			}
			cur.grow(s.from, s.to)
			if s.mail != nil {
				cur.Mails = append(cur.Mails, s.mail)
			}
			if s.read != nil {
				cur.Reads = append(cur.Reads, s.read)
			}
		}
	}
	systems := make([]int64, 0, len(bySystem))
	for sys := range bySystem {
		systems = append(systems, sys)
	}
	slices.Sort(systems)
	for _, sys := range systems {
		cut(sys, bySystem[sys])
	}
	var alone []span
	for _, r := range unplaced {
		start, end := onFleet(r)
		var best *fleetCluster
		var bestCover time.Duration = -1
		for _, c := range out {
			if end.Add(fleetBattleGap).Before(c.From) || start.Add(-fleetBattleGap).After(c.To) {
				continue
			}
			from, to := start, end
			if c.From.After(from) {
				from = c.From
			}
			if c.To.Before(to) {
				to = c.To
			}
			if cover := max(to.Sub(from), 0); cover > bestCover {
				best, bestCover = c, cover
			}
		}
		if best == nil {
			alone = append(alone, span{from: start, to: end, read: r})
			continue
		}
		best.grow(start, end)
		best.Reads = append(best.Reads, r)
	}
	cut(0, alone)
	slices.SortStableFunc(out, func(a, b *fleetCluster) int {
		if c := a.From.Compare(b.From); c != 0 {
			return c
		}
		return cmp.Compare(a.System, b.System)
	})
	return out
}

// fleetCurve is a battle's damage curve, laid out once at build time.
type fleetCurve struct {
	Out     string          `json:"out"`
	In      string          `json:"in"`
	Reps    string          `json:"reps"`
	TopDPS  int64           `json:"top_dps"`
	Seconds int64           `json:"seconds"`
	Bars    []fleetCurveBar `json:"bars"`
}

// fleetCurveBar is one point's hover target and what it reads out.
type fleetCurveBar struct {
	X     float64 `json:"x"`
	W     float64 `json:"w"`
	Label string  `json:"label"`
}

// buildFleetCurve draws damage per second across the battle's clock
// steps. A long battle is averaged into at most fleetCurvePoints points.
func buildFleetCurve(steps map[int64]*[3]int64) fleetCurve {
	if len(steps) == 0 {
		return fleetCurve{}
	}
	first, last := int64(0), int64(0)
	for at := range steps {
		if first == 0 || at < first {
			first = at
		}
		last = max(last, at)
	}
	n := int((last-first)/gamelog.SliceSeconds) + 1
	bucket := max((n+fleetCurvePoints-1)/fleetCurvePoints, 1)
	points := (n + bucket - 1) / bucket
	vals := make([][3]float64, points)
	for at, v := range steps {
		i := int((at-first)/gamelog.SliceSeconds) / bucket
		for k := range v {
			vals[i][k] += float64(v[k])
		}
	}
	span := float64(bucket * gamelog.SliceSeconds)
	top := 0.0
	for i := range vals {
		for k := range vals[i] {
			vals[i][k] /= span
			top = max(top, vals[i][k])
		}
	}
	ceil := niceCeil(top)
	if ceil <= 0 {
		ceil = 1
	}
	line := func(k int) string {
		var b strings.Builder
		for i := range vals {
			x := 0.0
			if len(vals) > 1 {
				x = float64(i) / float64(len(vals)-1) * fleetCurveW
			}
			fmt.Fprintf(&b, "%g,%g ", round1(x), round1(fleetCurveH-vals[i][k]/ceil*(fleetCurveH-6)))
			if len(vals) == 1 {
				fmt.Fprintf(&b, "%g,%g ", fleetCurveW, round1(fleetCurveH-vals[i][k]/ceil*(fleetCurveH-6)))
			}
		}
		return strings.TrimSpace(b.String())
	}
	bars := make([]fleetCurveBar, len(vals))
	w := fleetCurveW / float64(len(vals))
	for i, v := range vals {
		at := time.Unix(first+int64(i*bucket*gamelog.SliceSeconds), 0).UTC()
		bars[i] = fleetCurveBar{
			X: round1(float64(i) * w), W: round1(w),
			Label: fmt.Sprintf("%s: %s damage out, %s damage in, %s repairs per second", at.Format("15:04:05"),
				battleNum(int64(v[0]+0.5)), battleNum(int64(v[1]+0.5)), battleNum(int64(v[2]+0.5))),
		}
	}
	return fleetCurve{Out: line(0), In: line(1), Reps: line(2), TopDPS: int64(ceil), Seconds: last - first + gamelog.SliceSeconds, Bars: bars}
}
