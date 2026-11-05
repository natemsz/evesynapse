package gamelog

// Fights: a game log's combat events, cut into fights and aggregated
// into what a battle report stores: its frame (listener, window,
// session file), a per-second series and one tally per other party.
//
// A fight is a run of combat events with no quiet gap longer than
// FightGap. Only a fight with a player on the other side of the
// pilot's damage or capacitor warfare is kept: shooting rats is not
// a battle, and a session of it yields nothing; nor is a fleet's own
// smartbombs landing on each other while it does.

import (
	"sort"
	"strings"
	"time"
)

// FightGap is the quiet gap that ends a fight.
const FightGap = 60 * time.Second

// MaxFightLength is the longest window one fight covers; a fight
// still going at that length is closed and the next one starts.
const MaxFightLength = 2 * time.Hour

// SliceSeconds is the width of the per-party slices a fight carries.
const SliceSeconds = 10

// Fight is one fight as a battle report stores it.
type Fight struct {
	Listener    string    `json:"listener"`
	CharacterID int64     `json:"character_id,omitempty"`
	Session     string    `json:"session"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Series      Series    `json:"series"`
	Parties     []Tally   `json:"parties"`
	Tackles     []Tackle  `json:"tackles,omitempty"`
	// Slices are the fight in SliceSeconds steps, per party: who
	// did what to whom when, at a resolution a report can tell the
	// fight's moments apart by.
	Slices   []Slice `json:"slices,omitempty"`
	Lines    int     `json:"lines"`
	Unparsed int     `json:"unparsed"`
}

// Series holds one value per second from Start: index i is the
// second Start+i.
type Series struct {
	DamageOut []int64 `json:"damage_out"`
	DamageIn  []int64 `json:"damage_in"`
	RepsOut   []int64 `json:"reps_out"`
	RepsIn    []int64 `json:"reps_in"`
	CapIn     []int64 `json:"cap_in"`
}

// Tally is everything between the pilot and one other party.
type Tally struct {
	Name      string        `json:"name"`
	Ticker    string        `json:"ticker,omitempty"`
	Hull      string        `json:"hull,omitempty"`
	Pilot     bool          `json:"pilot"`
	DamageOut int64         `json:"damage_out"`
	DamageIn  int64         `json:"damage_in"`
	RepsOut   int64         `json:"reps_out"`
	RepsIn    int64         `json:"reps_in"`
	CapOut    int64         `json:"cap_out"`
	CapIn     int64         `json:"cap_in"`
	First     time.Time     `json:"first"`
	Last      time.Time     `json:"last"`
	Weapons   []WeaponTally `json:"weapons,omitempty"`
}

// Slice is one step of a fight: At is its first second from Start,
// and each row is a party (its index in Parties) with the damage out
// and in, repairs out and in, and capacitor pressure in between it
// and the pilot during the step.
type Slice struct {
	At   int        `json:"at"`
	Rows [][6]int64 `json:"rows"`
}

// Tackle is a warp scramble or disruption seen on grid: who tried to
// hold whom, as the log names them ("you" for the pilot, else by
// hull), when first, and how many attempts.
type Tackle struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	Kind  string    `json:"kind"`
	First time.Time `json:"first"`
	Count int64     `json:"count"`
}

// WeaponTally is one weapon's shots one way between the pilot and a
// party, with the quality words they drew.
type WeaponTally struct {
	Dir     string           `json:"dir"`
	Weapon  string           `json:"weapon"`
	Shots   int64            `json:"shots"`
	Damage  int64            `json:"damage"`
	Quality map[string]int64 `json:"quality,omitempty"`
}

// fightBuilder gathers one fight's events.
type fightBuilder struct {
	start, last time.Time
	hostile     bool // a player was on the other side of something hostile
	secs        map[int64]*[5]int64
	parties     map[string]*Tally
	weapons     map[string]*WeaponTally
	tackles     map[string]*Tackle
	slices      map[int64]map[string]*[5]int64
	torder      []string
	order       []string
	lines       int
	unparsed    int
}

func newFightBuilder(at time.Time) *fightBuilder {
	return &fightBuilder{start: at, last: at, secs: map[int64]*[5]int64{}, parties: map[string]*Tally{}, weapons: map[string]*WeaponTally{}, tackles: map[string]*Tackle{}, slices: map[int64]map[string]*[5]int64{}}
}

func (b *fightBuilder) add(ev CombatEvent) {
	b.lines++
	if ev.At.After(b.last) {
		b.last = ev.At
	}
	if ev.Kind == EvTackle {
		k := ev.Weapon + "\x00" + ev.Party.Name + "\x00" + ev.To.Name
		t := b.tackles[k]
		if t == nil {
			t = &Tackle{From: ev.Party.Name, To: ev.To.Name, Kind: ev.Weapon, First: ev.At}
			b.tackles[k] = t
			b.torder = append(b.torder, k)
		}
		t.Count++
		return
	}
	key := strings.ToLower(ev.Party.Name)
	t := b.parties[key]
	if t == nil {
		t = &Tally{Name: ev.Party.Name, First: ev.At}
		b.parties[key] = t
		b.order = append(b.order, key)
	}
	if ev.Party.Pilot {
		t.Pilot, t.Ticker, t.Hull = true, ev.Party.Ticker, ev.Party.Hull
	}
	if ev.At.After(t.Last) {
		t.Last = ev.At
	}
	sec := int64(ev.At.Sub(b.start) / time.Second)
	slot := b.secs[sec]
	if slot == nil {
		slot = &[5]int64{}
		b.secs[sec] = slot
	}
	hostile := false
	before := *slot
	defer func() {
		// Whatever this event added to the second, the party's
		// slice gets too.
		var d [5]int64
		moved := false
		for i := range d {
			d[i] = slot[i] - before[i]
			moved = moved || d[i] != 0
		}
		if !moved {
			return
		}
		step := sec / SliceSeconds
		m := b.slices[step]
		if m == nil {
			m = map[string]*[5]int64{}
			b.slices[step] = m
		}
		v := m[key]
		if v == nil {
			v = &[5]int64{}
			m[key] = v
		}
		for i := range d {
			v[i] += d[i]
		}
	}()
	switch ev.Kind {
	case EvDamage, EvMiss:
		hostile = true
		if ev.Dir == DirOut {
			t.DamageOut += ev.Amount
			slot[0] += ev.Amount
		} else {
			t.DamageIn += ev.Amount
			slot[1] += ev.Amount
		}
		if ev.Weapon != "" || ev.Quality != "" {
			wk := key + "\x00" + ev.Dir + "\x00" + strings.ToLower(ev.Weapon)
			w := b.weapons[wk]
			if w == nil {
				w = &WeaponTally{Dir: ev.Dir, Weapon: ev.Weapon}
				b.weapons[wk] = w
			}
			w.Shots++
			w.Damage += ev.Amount
			if ev.Quality != "" {
				if w.Quality == nil {
					w.Quality = map[string]int64{}
				}
				w.Quality[ev.Quality]++
			}
		}
	case EvRep:
		if ev.Dir == DirOut {
			t.RepsOut += ev.Amount
			slot[2] += ev.Amount
		} else {
			t.RepsIn += ev.Amount
			slot[3] += ev.Amount
		}
	case EvCap:
		hostile = true
		if ev.Dir == DirOut {
			t.CapOut += ev.Amount
		} else {
			t.CapIn += ev.Amount
			slot[4] += ev.Amount
		}
	}
	// A smartbomb hits everything in range, fleetmates included,
	// so its damage between pilots does not make a fight a battle.
	if hostile && ev.Party.Pilot && !strings.Contains(ev.Weapon, "Smartbomb") {
		b.hostile = true
	}
}

// fight finishes the builder into a Fight.
func (b *fightBuilder) fight(listener string, charID int64, session string) Fight {
	n := int(b.last.Sub(b.start)/time.Second) + 1
	s := Series{DamageOut: make([]int64, n), DamageIn: make([]int64, n), RepsOut: make([]int64, n), RepsIn: make([]int64, n), CapIn: make([]int64, n)}
	for sec, v := range b.secs {
		if sec < 0 || int(sec) >= n {
			continue
		}
		s.DamageOut[sec], s.DamageIn[sec], s.RepsOut[sec], s.RepsIn[sec], s.CapIn[sec] = v[0], v[1], v[2], v[3], v[4]
	}
	f := Fight{Listener: listener, CharacterID: charID, Session: session, Start: b.start, End: b.last, Series: s, Lines: b.lines, Unparsed: b.unparsed}
	for _, key := range b.order {
		t := *b.parties[key]
		for wk, w := range b.weapons {
			if strings.HasPrefix(wk, key+"\x00") {
				t.Weapons = append(t.Weapons, *w)
			}
		}
		sort.Slice(t.Weapons, func(i, j int) bool {
			if t.Weapons[i].Dir != t.Weapons[j].Dir {
				return t.Weapons[i].Dir > t.Weapons[j].Dir // out first
			}
			return t.Weapons[i].Damage > t.Weapons[j].Damage
		})
		f.Parties = append(f.Parties, t)
	}
	for _, k := range b.torder {
		f.Tackles = append(f.Tackles, *b.tackles[k])
	}
	index := map[string]int64{}
	for i, key := range b.order {
		index[key] = int64(i)
	}
	steps := make([]int64, 0, len(b.slices))
	for st := range b.slices {
		steps = append(steps, st)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i] < steps[j] })
	for _, st := range steps {
		sl := Slice{At: int(st) * SliceSeconds}
		for key, v := range b.slices[st] {
			sl.Rows = append(sl.Rows, [6]int64{index[key], v[0], v[1], v[2], v[3], v[4]})
		}
		sort.Slice(sl.Rows, func(i, j int) bool { return sl.Rows[i][0] < sl.Rows[j][0] })
		f.Slices = append(f.Slices, sl)
	}
	return f
}

// Segmenter cuts one listener's events into fights.
type Segmenter struct {
	Listener    string
	CharacterID int64
	Session     string
	// Quiet counts the fights closed with no player on the other
	// side, which are not kept.
	Quiet    int
	cur      *fightBuilder
	unparsed int
}

// Add takes one event in log order and returns a fight it closed,
// if any, and whether that fight is a battle worth keeping.
func (s *Segmenter) Add(ev CombatEvent) (Fight, bool) {
	var out Fight
	closed := false
	if s.cur != nil && (ev.At.Sub(s.cur.last) > FightGap || ev.At.Sub(s.cur.start) >= MaxFightLength) {
		out, closed = s.close()
	}
	if s.cur == nil {
		s.cur = newFightBuilder(ev.At)
		s.cur.unparsed, s.unparsed = s.unparsed, 0
	}
	s.cur.add(ev)
	return out, closed
}

// Unparsed counts a combat line no rule read, against the fight in
// progress (or the next one).
func (s *Segmenter) Unparsed() {
	if s.cur != nil {
		s.cur.unparsed++
		s.cur.lines++
		return
	}
	s.unparsed++
}

// Flush closes the fight in progress whatever the time.
func (s *Segmenter) Flush() (Fight, bool) {
	if s.cur == nil {
		return Fight{}, false
	}
	return s.close()
}

func (s *Segmenter) close() (Fight, bool) {
	b := s.cur
	s.cur = nil
	if !b.hostile {
		s.Quiet++
		return Fight{}, false
	}
	return b.fight(s.Listener, s.CharacterID, s.Session), true
}
