package app

// ---------------------------------------------------------------------------
// Battle reports, moments: the fight's timeline.
//
// A moment is a stretch of the fight worth stopping on: a kill or a
// loss, a tackle on the pilot, a burst of energy warfare, a peak of
// incoming damage, a surge of repairs, or the moment a new attacker
// takes over the pressure (or the pilot switches targets). Each one
// carries its window and the detail its panel shows: who hit whom in
// that window, the repairs and neutralizers in it, the tackle and the
// mails. All of it is derived here, when the fight is correlated, from
// the per-second series and the per-party 10-second slices; the page
// reads one stored row per moment it opens.
// ---------------------------------------------------------------------------

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// Moment kinds, each with its own color on the timeline.
const (
	momentContact  = "contact"
	momentKill     = "kill"
	momentLoss     = "loss"
	momentTackle   = "tackle"
	momentEnergy   = "energy"
	momentPressure = "pressure"
	momentRepairs  = "repairs"
	momentFocus    = "focus"
	momentTarget   = "target"
)

// The timeline's thresholds, in one place.
const (
	momentPeakApart     = 60  // seconds between two peaks of one kind
	momentPeaksMax      = 3   // peaks of one kind at most
	momentPeakFloorPct  = 35  // a later peak must reach this share of the highest
	momentFocusMinPct   = 40  // a new main attacker's share of its slice's incoming
	momentFocusFloorPct = 15  // that slice's incoming against the fight's busiest slice
	momentShiftsMax     = 5   // focus shifts (and target switches) at most
	momentKillLead      = 30  // seconds of run-up a kill's window shows
	momentTacklesMax    = 6   // tackle moments at most
	momentRows          = 6   // parties a moment's table lists
	momentTotalMax      = 40  // moments a fight keeps
	momentSparkW        = 120 // a moment's sparkline, in SVG units
	momentSparkH        = 28
)

// battleMomentDetail is what a moment's panel shows (stored as JSON).
type battleMomentDetail struct {
	Summary   string            `json:"summary"`
	From      int               `json:"from"`
	To        int               `json:"to"`
	DamageOut int64             `json:"damage_out"`
	DamageIn  int64             `json:"damage_in"`
	RepsIn    int64             `json:"reps_in"`
	RepsOut   int64             `json:"reps_out"`
	CapIn     int64             `json:"cap_in"`
	Attackers []momentParty     `json:"attackers,omitempty"`
	Targets   []momentParty     `json:"targets,omitempty"`
	Support   []momentParty     `json:"support,omitempty"`
	Neuts     []momentParty     `json:"neuts,omitempty"`
	Tackles   []momentTackleRow `json:"tackles,omitempty"`
	Kills     []momentKillRow   `json:"kills,omitempty"`
	SparkOut  string            `json:"spark_out"`
	SparkIn   string            `json:"spark_in"`
	SparkReps string            `json:"spark_reps"`
	Marks     []battleChartMark `json:"marks,omitempty"`
}

type momentParty struct {
	Name        string `json:"name"`
	Hull        string `json:"hull,omitempty"`
	HullTypeID  int64  `json:"hull_type_id,omitempty"`
	CharacterID int64  `json:"character_id,omitempty"`
	Ticker      string `json:"ticker,omitempty"`
	Pilot       bool   `json:"pilot"`
	Died        bool   `json:"died,omitempty"`
	Amount      int64  `json:"amount"`
	Pct         int64  `json:"pct"`
}

type momentTackleRow struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	At    int    `json:"at"`
	Count int64  `json:"count"`
	Mine  bool   `json:"mine"`
}

type momentKillRow struct {
	KillmailID int64  `json:"killmail_id"`
	Victim     string `json:"victim"`
	VictimID   int64  `json:"victim_id"`
	ShipTypeID int64  `json:"ship_type_id"`
	Loss       bool   `json:"loss"`
	At         int    `json:"at"`
}

type battleMoment struct {
	Kind   string
	At     int
	End    int
	Title  string
	Brief  battleMomentBrief
	Detail battleMomentDetail
}

// battleMomentBrief is what a moment's timeline tooltip shows: its
// figures and the party that led each way, read with the lanes.
type battleMomentBrief struct {
	DamageOut int64        `json:"out"`
	DamageIn  int64        `json:"in"`
	RepsIn    int64        `json:"rep"`
	CapIn     int64        `json:"cap"`
	TopIn     *momentParty `json:"top_in,omitempty"`
	TopOut    *momentParty `json:"top_out,omitempty"`
	ShipID    int64        `json:"ship,omitempty"` // a kill or loss: the hull that died
}

type momentInput struct {
	Fight   db.BattleFight
	Series  battleSeries
	Slices  []battleSlice
	Parties []db.BattleFightParty // indexed by Seq
	Tackles []db.BattleFightTackle
	Kills   []db.BattleFightKill
	Ships   map[int64]string // ship type names for the kills
}

// partyName is a party as a moment title names it.
func momentPartyName(p db.BattleFightParty) string {
	switch {
	case p.Pilot && p.Hull != "":
		return fmt.Sprintf("%s (%s)", p.Name, p.Hull)
	case p.Hull != "" && p.Hull == p.Name:
		return "a " + p.Name
	}
	return p.Name
}

// peaks finds up to momentPeaksMax local highs of a rolling series,
// momentPeakApart apart, each at least momentPeakFloorPct of the top.
func peaks(r []float64) []int {
	type pk struct {
		at int
		v  float64
	}
	var all []pk
	for i, v := range r {
		if v > 0 && (i == 0 || v >= r[i-1]) && (i == len(r)-1 || v > r[i+1]) {
			all = append(all, pk{i, v})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].v > all[j].v })
	var out []int
	for _, p := range all {
		if len(out) == momentPeaksMax {
			break
		}
		if len(out) > 0 && p.v*100 < all[0].v*momentPeakFloorPct {
			break
		}
		far := true
		for _, o := range out {
			if abs(o-p.at) < momentPeakApart {
				far = false
			}
		}
		if far {
			out = append(out, p.at)
		}
	}
	return out
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// buildMoments lays out the fight's timeline.
func buildMoments(x momentInput) []battleMoment {
	n := len(x.Series.DamageOut)
	if n == 0 {
		return nil
	}
	clamp := func(v int) int { return min(max(v, 0), n-1) }
	bySeq := map[int64]db.BattleFightParty{}
	for _, p := range x.Parties {
		bySeq[int64(p.Seq)] = p
	}
	var out []battleMoment
	add := func(kind string, at, end int, title string) {
		at, end = clamp(at), clamp(end)
		if end < at {
			end = at
		}
		out = append(out, battleMoment{Kind: kind, At: at, End: end, Title: title})
	}

	// First contact.
	for i := 0; i < n; i++ {
		if x.Series.DamageOut[i] > 0 || x.Series.DamageIn[i] > 0 || x.Series.CapIn[i] > 0 {
			add(momentContact, i, i+battleDPSWindow, "First shots")
			break
		}
	}

	// Kills and losses.
	for _, k := range x.Kills {
		ship := x.Ships[k.ShipTypeID]
		if k.Loss {
			title := "You lose your " + ship
			if ship == "" {
				title = "You lose your ship"
			}
			add(momentLoss, int(k.AtSecond)-momentKillLead, int(k.AtSecond), title)
			continue
		}
		who := k.VictimName
		if who == "" {
			who = "A pilot"
		}
		title := who + " is destroyed"
		if ship != "" {
			title = fmt.Sprintf("%s loses a %s", who, ship)
		}
		add(momentKill, int(k.AtSecond)-momentKillLead, int(k.AtSecond), title)
	}

	// Tackle on the pilot, or by the pilot: the log says "you".
	nt := 0
	for _, t := range x.Tackles {
		if nt == momentTacklesMax {
			break
		}
		at := int(t.FirstAt.Sub(x.Fight.StartedAt) / time.Second)
		verb := "warp scrambles"
		if t.Kind == "disruption" {
			verb = "warp disrupts"
		}
		switch {
		case strings.EqualFold(t.ToName, "you"):
			add(momentTackle, at, at+battleDPSWindow, fmt.Sprintf("%s %s you", tackleName(t.FromName), verb))
		case strings.EqualFold(t.FromName, "you"):
			add(momentTackle, at, at+battleDPSWindow, fmt.Sprintf("You %s %s", strings.TrimSuffix(verb, "s"), tackleName(t.ToName)))
		default:
			continue
		}
		nt++
	}

	// Peaks: incoming damage, energy warfare, repairs.
	for _, at := range peaks(rolling(x.Series.DamageIn)) {
		v := rolling(x.Series.DamageIn)[at]
		add(momentPressure, at-battleDPSWindow+1, at, fmt.Sprintf("Incoming peaks at %s a second", battleNum(int64(v))))
	}
	for _, at := range peaks(rolling(x.Series.CapIn)) {
		var sum int64
		for i := max(at-battleDPSWindow+1, 0); i <= at; i++ {
			sum += x.Series.CapIn[i]
		}
		add(momentEnergy, at-battleDPSWindow+1, at, fmt.Sprintf("%s GJ neutralized in %d seconds", battleNum(sum), battleDPSWindow))
	}
	for _, at := range peaks(rolling(x.Series.RepsIn)) {
		var sum int64
		for i := max(at-battleDPSWindow+1, 0); i <= at; i++ {
			sum += x.Series.RepsIn[i]
		}
		add(momentRepairs, at-battleDPSWindow+1, at, fmt.Sprintf("%s repaired on you in %d seconds", battleNum(sum), battleDPSWindow))
	}

	// Focus: a new main attacker; target: a new main target.
	var busiestIn, busiestOut int64
	for _, sl := range x.Slices {
		var in, o int64
		for _, r := range sl.Rows {
			o += r[1]
			in += r[2]
		}
		busiestIn, busiestOut = max(busiestIn, in), max(busiestOut, o)
	}
	var lastIn, lastOut int64 = -1, -1
	shifts, switches := 0, 0
	for _, sl := range x.Slices {
		var in, o, topIn, topOut int64
		whoIn, whoOut := int64(-1), int64(-1)
		for _, r := range sl.Rows {
			o += r[1]
			in += r[2]
			if !bySeq[r[0]].Pilot {
				// A drone or a missile in flight is its pilot's
				// weapon, not a new focus.
				continue
			}
			if r[2] > topIn {
				topIn, whoIn = r[2], r[0]
			}
			if r[1] > topOut {
				topOut, whoOut = r[1], r[0]
			}
		}
		if whoIn >= 0 && whoIn != lastIn && in*100 >= busiestIn*momentFocusFloorPct && topIn*100 >= in*momentFocusMinPct {
			if lastIn >= 0 && shifts < momentShiftsMax {
				add(momentFocus, sl.At, sl.At+battleSliceSeconds-1, momentPartyName(bySeq[whoIn])+" takes over the pressure on you")
				shifts++
			}
			lastIn = whoIn
		}
		if whoOut >= 0 && whoOut != lastOut && o*100 >= busiestOut*momentFocusFloorPct && topOut*100 >= o*momentFocusMinPct {
			if lastOut >= 0 && switches < momentShiftsMax {
				add(momentTarget, sl.At, sl.At+battleSliceSeconds-1, "You switch to "+momentPartyName(bySeq[whoOut]))
				switches++
			}
			lastOut = whoOut
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	if len(out) > momentTotalMax {
		out = out[:momentTotalMax]
	}
	for i := range out {
		d := momentDetail(x, bySeq, out[i])
		out[i].Detail = d
		b := battleMomentBrief{DamageOut: d.DamageOut, DamageIn: d.DamageIn, RepsIn: d.RepsIn, CapIn: d.CapIn}
		if len(d.Attackers) > 0 {
			b.TopIn = &d.Attackers[0]
		}
		if len(d.Targets) > 0 {
			b.TopOut = &d.Targets[0]
		}
		for _, k := range d.Kills {
			if k.At == out[i].End {
				b.ShipID = k.ShipTypeID
			}
		}
		out[i].Brief = b
	}
	return out
}

func tackleName(s string) string {
	if strings.EqualFold(s, "you") {
		return "you"
	}
	return s
}

// momentDetail fills one moment's panel from its window.
func momentDetail(x momentInput, bySeq map[int64]db.BattleFightParty, m battleMoment) battleMomentDetail {
	d := battleMomentDetail{From: m.At, To: m.End}
	s := x.Series
	for i := m.At; i <= m.End && i < len(s.DamageOut); i++ {
		d.DamageOut += s.DamageOut[i]
		d.DamageIn += s.DamageIn[i]
		d.RepsIn += s.RepsIn[i]
		d.RepsOut += s.RepsOut[i]
		d.CapIn += s.CapIn[i]
	}
	// The slices that overlap the window, summed per party.
	sum := map[int64]*[5]int64{}
	for _, sl := range x.Slices {
		if sl.At+battleSliceSeconds-1 < m.At || sl.At > m.End {
			continue
		}
		for _, r := range sl.Rows {
			v := sum[r[0]]
			if v == nil {
				v = &[5]int64{}
				sum[r[0]] = v
			}
			for i := 0; i < 5; i++ {
				v[i] += r[i+1]
			}
		}
	}
	list := func(col int) []momentParty {
		var whole int64
		var rows []momentParty
		for seq, v := range sum {
			if v[col] <= 0 {
				continue
			}
			whole += v[col]
			p := bySeq[seq]
			rows = append(rows, momentParty{Name: p.Name, Hull: p.Hull, HullTypeID: p.HullTypeID, CharacterID: p.CharacterID, Ticker: p.Ticker, Pilot: p.Pilot, Died: p.Died, Amount: v[col]})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Amount != rows[j].Amount {
				return rows[i].Amount > rows[j].Amount
			}
			return rows[i].Name < rows[j].Name
		})
		for i := range rows {
			rows[i].Pct = pct(rows[i].Amount, whole)
		}
		if len(rows) > momentRows {
			rows = rows[:momentRows]
		}
		return rows
	}
	d.Targets, d.Attackers = list(0), list(1)
	d.Support, d.Neuts = list(3), list(4)

	for _, t := range x.Tackles {
		at := int(t.FirstAt.Sub(x.Fight.StartedAt) / time.Second)
		mine := strings.EqualFold(t.ToName, "you") || strings.EqualFold(t.FromName, "you")
		if at >= m.At && at <= m.End || mine && m.Kind == momentTackle && at == m.At {
			d.Tackles = append(d.Tackles, momentTackleRow{From: t.FromName, To: t.ToName, Kind: t.Kind, At: at, Count: t.Attempts, Mine: mine})
		}
	}
	sort.SliceStable(d.Tackles, func(i, j int) bool { return d.Tackles[i].Mine && !d.Tackles[j].Mine })
	if len(d.Tackles) > momentRows {
		d.Tackles = d.Tackles[:momentRows]
	}
	for _, k := range x.Kills {
		if int(k.AtSecond) >= m.At && int(k.AtSecond) <= m.End+momentKillLead/3 {
			d.Kills = append(d.Kills, momentKillRow{KillmailID: k.KillmailID, Victim: k.VictimName, VictimID: k.VictimCharacterID, ShipTypeID: k.ShipTypeID, Loss: k.Loss, At: int(k.AtSecond)})
		}
	}

	// A sparkline of the window with some lead either side.
	lo, hi := max(m.At-battleDPSWindow*2, 0), min(m.End+battleDPSWindow*2, len(s.DamageOut)-1)
	rOut, rIn, rReps := rolling(s.DamageOut), rolling(s.DamageIn), rolling(s.RepsIn)
	top := 0.0
	for i := lo; i <= hi; i++ {
		top = max(top, rOut[i], rIn[i], rReps[i])
	}
	spark := func(r []float64) string {
		var b strings.Builder
		for i := lo; i <= hi; i++ {
			x := 0.0
			if hi > lo {
				x = float64(i-lo) / float64(hi-lo) * momentSparkW
			}
			y := float64(momentSparkH)
			if top > 0 {
				y = momentSparkH - r[i]/top*momentSparkH
			}
			if i > lo {
				b.WriteByte(' ')
			}
			b.WriteString(strconv.FormatFloat(round1(x), 'f', -1, 64))
			b.WriteByte(',')
			b.WriteString(strconv.FormatFloat(round1(y), 'f', -1, 64))
		}
		return b.String()
	}
	d.SparkOut, d.SparkIn, d.SparkReps = spark(rOut), spark(rIn), spark(rReps)
	if hi > lo {
		d.Marks = append(d.Marks,
			battleChartMark{X: round1(float64(m.At-lo) / float64(hi-lo) * momentSparkW)},
			battleChartMark{X: round1(float64(m.End-lo) / float64(hi-lo) * momentSparkW)})
	}
	d.Summary = momentSummary(m, d)
	return d
}

// momentSummary says what the window holds in one sentence.
func momentSummary(m battleMoment, d battleMomentDetail) string {
	var parts []string
	span := d.To - d.From + 1
	if d.DamageIn > 0 {
		s := fmt.Sprintf("you took %s", battleNum(d.DamageIn))
		if len(d.Attackers) > 0 {
			s += fmt.Sprintf(", %d%% of it from %s", d.Attackers[0].Pct, d.Attackers[0].Name)
		}
		parts = append(parts, s)
	}
	if d.DamageOut > 0 {
		s := fmt.Sprintf("you dealt %s", battleNum(d.DamageOut))
		if len(d.Targets) > 0 {
			s += fmt.Sprintf(", mostly into %s", d.Targets[0].Name)
		}
		parts = append(parts, s)
	}
	if d.RepsIn > 0 {
		parts = append(parts, fmt.Sprintf("you were repaired for %s", battleNum(d.RepsIn)))
	}
	if d.CapIn > 0 {
		parts = append(parts, fmt.Sprintf("%s GJ was neutralized", battleNum(d.CapIn)))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Nothing passed between you and anyone else in these %d seconds.", span)
	}
	s := strings.Join(parts, "; ")
	return fmt.Sprintf("Over %d seconds from %s, %s.", span, clockOffset(d.From), s)
}
