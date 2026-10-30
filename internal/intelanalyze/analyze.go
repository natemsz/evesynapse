// Package intelanalyze works out what an intel report says: the pilots
// and ships in it, the gate, what kind of report it is (a sighting, a
// gate camp, a fleet, capitals or a cyno, a bubble, someone in Local,
// a clear, a question, chatter), and whether the pilots are friendly.
// It is pure: names are looked up through the caller's dictionaries
// (Dict), and nothing is fetched. A name it cannot place is returned
// in Unknown, for the caller to look up and analyse again.
//
// Input is a report as internal/intelchat reads it: the system already
// found, the count, clear and "no visual" taken out, and the rest as
// said, with the two-space gaps that separate names dragged from Local.
package intelanalyze

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kinds of report, most pressing first (Rank). Stored: never rename.
const (
	KindCapital  = "capital"  // capitals, a cyno, a titan bridge
	KindCamp     = "camp"     // a gate camp
	KindBubble   = "bubble"   // a bubble, an interdictor or HIC
	KindFleet    = "fleet"    // a fleet, ten or more, or bombers
	KindSighting = "sighting" // hostiles seen
	KindLocal    = "local"    // in Local, no visual
	KindFriendly = "friendly" // only friendlies named
	KindClear    = "clear"
	KindQuestion = "question"
	KindChatter  = "chatter"
)

// Rank orders kinds by how pressing they are: lower is more.
func Rank(kind string) int {
	switch kind {
	case KindCapital:
		return 0
	case KindCamp:
		return 1
	case KindBubble:
		return 2
	case KindFleet:
		return 3
	case KindSighting:
		return 4
	case KindLocal:
		return 5
	case KindFriendly:
		return 6
	case KindClear:
		return 7
	case KindQuestion:
		return 8
	}
	return 9
}

// Ship classes the analysis acts on (from the SDE's group names).
const (
	ClassCapital   = "capital"
	ClassInterdict = "interdiction" // interdictors and HICs: bubbles
	ClassBomber    = "bomber"
	ClassHunter    = "hunter" // recons, T3Cs, black ops: they hunt
	ClassFast      = "fast"   // interceptors
	ClassOther     = ""
)

// ClassOfGroup is a ship group's class.
func ClassOfGroup(group string) string {
	switch group {
	case "Titan", "Supercarrier", "Carrier", "Dreadnought", "Force Auxiliary", "Lancer Dreadnought", "Capital Industrial Ship", "Jump Freighter":
		return ClassCapital
	case "Interdictor", "Heavy Interdiction Cruiser":
		return ClassInterdict
	case "Stealth Bomber":
		return ClassBomber
	case "Force Recon Ship", "Combat Recon Ship", "Strategic Cruiser", "Black Ops":
		return ClassHunter
	case "Interceptor":
		return ClassFast
	}
	return ClassOther
}

// Pilot is one pilot named in a report.
type Pilot struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Friendly bool   `json:"friendly,omitempty"`
	Red      bool   `json:"red,omitempty"` // a set standing says hostile
}

// Ship is a ship type or class named, how many if said.
type Ship struct {
	TypeID int64  `json:"type,omitempty"` // 0 for a class said by its nickname ("inty")
	Name   string `json:"name"`
	Class  string `json:"class,omitempty"`
	N      int    `json:"n,omitempty"`
}

// StandingCounts is a report's split by standing, in the order
// the site speaks of them: hostiles, friendlies, neutrals, and
// the unclassified no stored source speaks for.
type StandingCounts struct {
	Hostile      int `json:"hostile"`
	Friendly     int `json:"friendly"`
	Neutral      int `json:"neutral"`
	Unclassified int `json:"unclassified"`
}

// Total is everyone the split counts.
func (c StandingCounts) Total() int {
	return c.Hostile + c.Friendly + c.Neutral + c.Unclassified
}

// Bounds on the residue kept with an analysis: the spans of a
// report's text the analysis did not consume. Bookkeeping for
// later classification work, so it stays small: at most
// MaxResidueSpans spans and MaxResidueBytes of text in all.
const (
	MaxResidueSpans = 40
	MaxResidueBytes = 2048
)

// Analysis is what a report says.
type Analysis struct {
	Kind   string  `json:"kind"`
	Pilots []Pilot `json:"pilots,omitempty"`
	Ships  []Ship  `json:"ships,omitempty"`
	Gate   int64   `json:"gate,omitempty"` // the neighbour system the reading names: its gate leads there, or the action is placed there
	// GateModel says the gate was proposed by the residue
	// model pass and verified against the system's real gates
	// (app side), not read by the deterministic passes here.
	GateModel bool            `json:"gate_model,omitempty"`
	Flags     []string        `json:"flags,omitempty"`
	Count     int             `json:"count,omitempty"`
	Counts    *StandingCounts `json:"counts,omitempty"` // the standing split, when it counts anyone
	Unknown   []string        `json:"-"`                // names to look up, then analyse again
	// Residue is the text the analysis did not consume: word
	// runs that matched no ship, gate, keyword or pilot and are
	// not waiting on a lookup (those stand in Unknown). It is
	// recorded, never read back: no output above changes for it.
	Residue []string `json:"residue,omitempty"`
	// Systems are the other systems the text names by their
	// exact names that are not the gate: places spoken of
	// ("NOL-M9 -> 1DQ1-A" from a system one jump further), never
	// names. Location is a celestial said in shorthand ("moon 4",
	// "P3M2"), as the site names it ("Planet 3 Moon 2").
	Systems  []int64 `json:"systems,omitempty"`
	Location string  `json:"location,omitempty"`
	// Areas are the regions and constellations the text names:
	// places, never names.
	Areas []int64 `json:"areas,omitempty"`
	// Suspect is the text, as written, of pilots this reading
	// resolved on a name that is as likely a word as a
	// character (SuspectPilot): "Ok", "Ess", "Help". The pilot
	// stands in Pilots; the residue step is asked about the
	// span and may take it back.
	Suspect []string `json:"suspect,omitempty"`
}

// Input is one report to analyse.
type Input struct {
	System   int64
	Text     string
	Hostiles int
	Clear    bool
	NoVisual bool
}

// Dict is how the analysis finds names. Each takes a lower-cased name.
type Dict struct {
	// Ship finds a ship type by its exact name.
	Ship func(lower string) (id int64, name, class string, ok bool)
	// Type finds any inventory type by its exact name. A string
	// that is a type's name is the thing, not a pilot: EVE has
	// characters named after items ("Force Field", "Ibis"), and in
	// free-form text the item reading wins. Ships are asked first,
	// for their class.
	Type func(lower string) (id int64, name string, ok bool)
	// Pilot finds a pilot by exact name; known false is "no such
	// pilot" (asked before), and both false is "not known yet".
	Pilot func(lower string) (id int64, name string, ok, known bool)
	// Gate finds a system by name, when it is a neighbour of system.
	Gate func(system int64, lower string) (id int64, ok bool)
	// GateNear finds a neighbour system by a near-miss of its
	// name (a letter slipped, or a shortened form). It is asked
	// for the name slot of a gate phrase, after Gate itself
	// missed, and wherever a line speaks of a place (a location
	// preposition, a gate mention, a movement verb): a word
	// that lands here is the system, a location, never a
	// pilot, and is not offered to the pilot lookup.
	GateNear func(system int64, lower string) (id int64, ok bool)
	// System finds any system by its exact name. A system the
	// text names is a place wherever it stands: a neighbour is
	// the gate (through Gate), any other is one of the systems
	// spoken of, and neither is offered as a pilot.
	System func(lower string) (id int64, ok bool)
	// Area finds a region or a constellation by its exact name.
	Area func(lower string) (id int64, ok bool)
	// Names, when set, is the intel classifier's reading of the
	// text: the runs it takes for pilots' names, as written. The
	// pilot pass tries those runs first; nil keeps the plain
	// longest-first reading.
	Names func(text string) []string
	// Friend reports whether a pilot counts as friendly, and whether
	// a set standing says they are hostile.
	Friend func(id int64) (friendly, red bool)
}

// Keywords and the flags they raise. Some raise a class too (a
// nickname for a ship class).
var keywords = map[string]string{
	"camp": "camp", "camping": "camp", "camped": "camp", "gatecamp": "camp", "gc": "camp",
	"bubble": "bubble", "bubbles": "bubble", "bubbled": "bubble", "drag": "bubble", "dragbubble": "bubble",
	"cyno": "cyno", "cynos": "cyno", "cyno'd": "cyno",
	"bridge": "bridge", "bridged": "bridge", "titan": "capital", "titans": "capital",
	"cap": "capital", "caps": "capital", "capital": "capital", "capitals": "capital", "super": "capital", "supers": "capital",
	"dread": "capital", "dreads": "capital", "carrier": "capital", "carriers": "capital", "fax": "capital", "blops": "hunter",
	"fleet": "fleet", "gang": "fleet", "blob": "fleet",
	"bomber": "bomber", "bombers": "bomber", "bomb": "bomber",
	"inty": "fast", "intys": "fast", "ceptor": "fast", "ceptors": "fast", "interceptor": "fast",
	"dic": "dictor", "dics": "dictor", "dictor": "dictor", "dictors": "dictor", "hic": "dictor", "hics": "dictor",
	"recon": "hunter", "recons": "hunter", "t3": "hunter", "t3c": "hunter", "cloaky": "cloaky", "cloakies": "cloaky", "cloaked": "cloaky",
	"neut": "local", "neuts": "local", "red": "local", "reds": "local", "local": "local", "spike": "local", "docked": "docked", "dock": "docked",
	"npc": "npc", "npcs": "npc", "guristas": "npc", "serpentis": "npc", "sansha": "npc", "sanshas": "npc", "angels": "npc",
	"trig": "npc", "trigs": "npc", "triglavian": "npc", "triglavians": "npc", "drifter": "npc", "drifters": "npc", "edencom": "npc",
	"gate": "", "on": "", "at": "", "in": "", "and": "", "the": "", "x": "", "+": "", "-": "", "—": "", "–": "", "/": "", "with": "",
	"->": "", "=>": "", "-->": "", "<-": "", "→": "", "@": "",
}

// Nicknamed classes, as ships the report names.
var classNick = map[string]Ship{
	"fast":   {Name: "Interceptor", Class: ClassFast},
	"dictor": {Name: "Interdictor", Class: ClassInterdict},
	"hunter": {Name: "Recon or T3", Class: ClassHunter},
	"bomber": {Name: "Stealth Bomber", Class: ClassBomber},
}

func clean(w string) string { return strings.ToLower(strings.Trim(w, ",.;:*!()[]?\"'")) }

// count reads "3", "3x", "x3", "+3".
func count(w string) (int, bool) {
	w = strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(w, "x"), "x"), "+")
	n, err := strconv.Atoi(w)
	return n, err == nil && n > 0 && n < 1000
}

// Analyze works out what one report says.
func Analyze(in Input, d Dict) Analysis {
	a := Analysis{Count: in.Hostiles}
	flags := map[string]bool{}
	if in.NoVisual {
		flags["nv"] = true
	}
	shipAt := map[string]int{}
	addShip := func(s Ship, n int) {
		key := s.Name
		if i, ok := shipAt[key]; ok {
			if n > a.Ships[i].N {
				a.Ships[i].N = n
			}
			return
		}
		s.N = n
		shipAt[key] = len(a.Ships)
		a.Ships = append(a.Ships, s)
	}
	seenPilot := map[int64]bool{}
	// The classifier's name runs, when one is wired: where one
	// name ends and the next begins among words typed with single
	// spaces. They only order the pilot pass; every name is still
	// looked up and every rule below still holds.
	var names map[string]bool
	if d.Names != nil {
		names = map[string]bool{}
		for _, n := range d.Names(in.Text) {
			names[clean(strings.Join(strings.Fields(n), " "))] = true
		}
	}
	// Scan-shaped text is read as rows first (scanrows.go):
	// what it consumes is blanked, so the prose reading below
	// never sees a name column.
	text := consumeScanText(in.Text, d, addShip)
	// Segments: names dragged from Local come two spaces apart, and
	// commas or slashes part a list.
	text = strings.NewReplacer(",", "  ", " / ", "  ", ";", "  ").Replace(text)
	var segs [][]string
	for _, s := range strings.Split(text, "  ") {
		if f := strings.Fields(s); len(f) > 0 {
			segs = append(segs, f)
		}
	}
	for _, seg := range segs {
		whole := clean(strings.Join(seg, " "))
		if len(seg) <= 3 {
			// A whole segment that is a type's name is the thing
			// (asked before pilots: a character may share the
			// name), and one that is a pilot's name is one pilot
			// when it is shaped like one (nameShaped): a short
			// phrase between commas ("still there") is not.
			if s, ok := lookupType(d, whole); ok {
				addShip(s, 0)
				continue
			}
			if !nameShaped(seg) || holdsThing(d, seg) || splitByNames(seg, names) {
				// read word by word below
			} else if id, name, ok, _ := lookupPilot(d, whole); ok {
				if !seenPilot[id] {
					seenPilot[id] = true
					a.Pilots = append(a.Pilots, Pilot{ID: id, Name: name})
					if SuspectPilot(whole) && !linkEnd(seg[len(seg)-1]) {
						a.Suspect = append(a.Suspect, asWritten(seg))
					}
				}
				continue
			}
		}
		// Otherwise word by word: ships (up to three words, longest
		// first), gates ("X gate"), counts, keywords, then pilots.
		// claimed and owed walk with used for the residue: a
		// word the pilot pass takes is claimed, one a lookup is
		// still owed on is owed, and a word in neither is text
		// this reading did not consume.
		used := make([]bool, len(seg))
		claimed := make([]bool, len(seg))
		owed := make([]bool, len(seg))
		markLocation(seg, used)
		if loc := markCelestial(seg, used); loc != "" && a.Location == "" {
			a.Location = loc
		}
		placey := speaksOfPlace(seg)
		pending := 0 // a count said before its ship ("2 sabres")
		for i := 0; i < len(seg); i++ {
			if used[i] {
				continue
			}
			w := clean(seg[i])
			if n, ok := count(w); ok {
				used[i] = true
				pending = n
				continue
			}
			matched := false
			// A four-word region ("Vale of the Silent") runs past
			// the three-word phrases below.
			if d.Area != nil && i+4 <= len(seg) {
				if id, ok := d.Area(clean(strings.Join(seg[i:i+4], " "))); ok {
					a.Areas = appendID(a.Areas, id)
					for k := i; k < i+4; k++ {
						used[k] = true
					}
					matched = true
				}
			}
			for n := 3; n >= 1 && !matched; n-- {
				if i+n > len(seg) {
					continue
				}
				phrase := clean(strings.Join(seg[i:i+n], " "))
				// "X gate": the gate to a neighbour. An exact
				// name first; a near-miss of a neighbour's name
				// (GateNear) after it, so a slipped or shortened
				// gate name reads as the gate and the word never
				// falls through to the pilot pass.
				if i+n < len(seg) && clean(seg[i+n]) == "gate" {
					g, ok := int64(0), false
					if d.Gate != nil {
						g, ok = d.Gate(in.System, phrase)
					}
					if !ok && d.GateNear != nil {
						g, ok = d.GateNear(in.System, phrase)
					}
					if ok {
						a.Gate = g
						for k := i; k <= i+n; k++ {
							used[k] = true
						}
						matched = true
						break
					}
				}
				// A region or constellation said as where pilots came
				// from or are going ("came from Curse") is the place,
				// ahead of a ship that shares its name.
				if d.Area != nil && i > 0 && areaCues[clean(seg[i-1])] {
					if id, ok := d.Area(phrase); ok {
						a.Areas = appendID(a.Areas, id)
						for k := i; k < i+n; k++ {
							used[k] = true
						}
						matched = true
						break
					}
				}
				// A ship by name, or any other type by name: the
				// thing, claimed before the words can fall through
				// to pilots.
				var ship Ship
				var isShip bool
				for _, cand := range shipCands(phrase) {
					if id, name, class, ok := lookupShip(d, cand); ok {
						ship, isShip = Ship{TypeID: id, Name: name, Class: class}, true
						break
					}
					if id, name, ok := lookupInvType(d, cand); ok {
						ship, isShip = Ship{TypeID: id, Name: name}, true
						break
					}
				}
				if isShip {
					nn := pending
					// "sabre x2" / "sabre 2x"
					if i+n < len(seg) {
						if c, ok := count(clean(seg[i+n])); ok {
							nn = c
							used[i+n] = true
						}
					}
					addShip(ship, nn)
					pending = 0
					for k := i; k < i+n; k++ {
						used[k] = true
					}
					matched = true
				}
				// A system by its exact name is a place wherever
				// it stands: a neighbour is the gate (unless one
				// was read already), the report's own system said
				// again is read, and any other is a system spoken
				// of. None of them is a pilot.
				if !matched && d.System != nil {
					if id, ok := d.System(phrase); ok {
						switch {
						case id == in.System:
						case d.Gate != nil && gateTo(d, in.System, phrase, id):
							if a.Gate == 0 {
								a.Gate = id
							} else if a.Gate != id {
								a.Systems = appendID(a.Systems, id)
							}
						default:
							a.Systems = appendID(a.Systems, id)
						}
						for k := i; k < i+n; k++ {
							used[k] = true
						}
						matched = true
					}
				}
				// A region or a constellation by its exact name: a
				// place spoken of, never a pilot.
				if !matched && d.Area != nil {
					if id, ok := d.Area(phrase); ok {
						a.Areas = appendID(a.Areas, id)
						for k := i; k < i+n; k++ {
							used[k] = true
						}
						matched = true
					}
				}
				// The line speaks of a place and this phrase is
				// a near-miss of a neighbour's name: the phrase
				// is that system, a location, and is not offered
				// to the pilot pass, even when a character
				// carries the slipped spelling exactly. The
				// hook is the gate slot's own: one neighbour in
				// reach reads, two in reach is no reading. An
				// explicit gate already read stands.
				if !matched && placey && d.GateNear != nil {
					if g, ok := d.GateNear(in.System, phrase); ok {
						if a.Gate == 0 {
							a.Gate = g
						}
						for k := i; k < i+n; k++ {
							used[k] = true
						}
						matched = true
					}
				}
			}
			if matched {
				continue
			}
			// A word dragged in as a link is a name, not the
			// report's vocabulary: "Gate*" is a pilot.
			if f, ok := keywords[w]; ok && !linkEnd(seg[i]) {
				used[i] = true
				if s, isClass := classNick[f]; isClass {
					nn := pending
					if i+1 < len(seg) {
						if c, ok := count(clean(seg[i+1])); ok {
							nn = c
							used[i+1] = true
						}
					}
					addShip(s, nn)
					pending = 0
				} else if f != "" {
					flags[f] = true
				}
			}
		}
		// What is left: pilots, one to three words each, longest
		// first; what no lookup knows is asked about later. Only
		// words shaped like a name are offered (NameLike), and a
		// name never runs past a link's star: the scout's own
		// words stay unread, never become characters.
		var left []int
		flush := func() {
			for len(left) > 0 {
				took := 0
				for _, n := range nameOrder(seg, left, names) {
					words := make([]string, n)
					shaped := true
					for k := 0; k < n; k++ {
						words[k] = seg[left[k]]
						if !NameLike(words[k]) || (k < n-1 && linkEnd(words[k])) {
							shaped = false
						}
					}
					if !shaped {
						continue
					}
					name := clean(strings.Join(words, " "))
					id, proper, ok, known := lookupPilot(d, name)
					if !ok && known && linkEnd(words[n-1]) && d.Pilot != nil {
						// A linked name is a name even when it is
						// also a word free text refuses ("Sun*").
						id, proper, ok, known = d.Pilot(name)
					}
					if ok {
						if !seenPilot[id] {
							seenPilot[id] = true
							a.Pilots = append(a.Pilots, Pilot{ID: id, Name: proper})
							if SuspectPilot(name) && !linkEnd(words[n-1]) {
								a.Suspect = append(a.Suspect, asWritten(words))
							}
						}
						for k := 0; k < n; k++ {
							claimed[left[k]] = true
						}
						took = n
						break
					}
					if !known {
						a.Unknown = append(a.Unknown, name)
						for k := 0; k < n; k++ {
							owed[left[k]] = true
						}
					}
				}
				if took == 0 {
					took = 1
				}
				left = left[took:]
			}
		}
		for i := range seg {
			if used[i] {
				flush()
				continue
			}
			left = append(left, i)
		}
		flush()
		// What no pass consumed and no lookup is still owed is
		// the segment's residue: a run of such words, kept when
		// at least one of them could have been a name. A run of
		// nothing but place nouns, numerals and the report's
		// own vocabulary was set aside knowingly, not left
		// unread, and is not residue.
		for i := 0; i < len(seg); {
			if used[i] || claimed[i] || owed[i] {
				i++
				continue
			}
			j := i
			var words []string
			candidate := false
			for j < len(seg) && !used[j] && !claimed[j] && !owed[j] {
				words = append(words, seg[j])
				if PilotCandidate(clean(seg[j])) {
					candidate = true
				}
				j++
			}
			if candidate {
				a.Residue = append(a.Residue, strings.Join(words, " "))
			}
			i = j
		}
	}
	if d.Friend != nil {
		for i := range a.Pilots {
			a.Pilots[i].Friendly, a.Pilots[i].Red = d.Friend(a.Pilots[i].ID)
		}
	}
	// The standing split: the named pilots by their read, with
	// the headline count standing for the hostiles it names no
	// pilots for. A report made from a scan reading carries its
	// reading's own counts instead (the caller sets them).
	var counts StandingCounts
	for _, p := range a.Pilots {
		switch {
		case p.Red:
			counts.Hostile++
		case p.Friendly:
			counts.Friendly++
		default:
			counts.Neutral++
		}
	}
	if in.Hostiles > counts.Hostile {
		counts.Hostile = in.Hostiles
	}
	if counts.Total() > 0 {
		a.Counts = &counts
	}
	for f := range flags {
		a.Flags = append(a.Flags, f)
	}
	sort.Strings(a.Flags)
	a.Unknown = dedupe(a.Unknown)
	a.Residue = capResidue(dedupe(a.Residue))
	a.Suspect = dedupe(a.Suspect)
	a.Kind = kindOf(in, a, flags)
	return a
}

// asWritten is a span's words as the scout wrote them, trimmed of
// the punctuation around them: what a suspect pilot is asked
// about under.
func asWritten(words []string) string {
	return strings.Trim(strings.Join(words, " "), ",.;:*!()[]?\"'")
}

// KindOf decides a stored reading's kind again, after a pilot was
// taken out of it: the flags are the reading's own, and in is the
// report's headline facts.
func KindOf(in Input, a Analysis) string {
	flags := map[string]bool{}
	for _, f := range a.Flags {
		flags[f] = true
	}
	return kindOf(in, a, flags)
}

// capResidue bounds residue spans to the package limits: whole
// spans in order, first come first kept; one span that alone
// runs past the byte bound is cut at it, on a rune boundary.
func capResidue(spans []string) []string {
	var out []string
	n := 0
	for _, s := range spans {
		if len(out) >= MaxResidueSpans {
			break
		}
		if n+len(s) > MaxResidueBytes {
			s = s[:MaxResidueBytes-n]
			for len(s) > 0 {
				if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size == 3 {
					break
				}
				s = s[:len(s)-1]
			}
			if s == "" {
				break
			}
			out = append(out, s)
			break
		}
		n += len(s) + 1
		out = append(out, s)
	}
	return out
}

func lookupPilot(d Dict, lower string) (int64, string, bool, bool) {
	if d.Pilot == nil || lower == "" || len(lower) > 37 {
		return 0, "", false, true
	}
	if !PilotCandidate(lower) {
		// Settled, not unknown: never asked about, never a pilot.
		return 0, "", false, true
	}
	return d.Pilot(lower)
}

func lookupShip(d Dict, lower string) (int64, string, string, bool) {
	if d.Ship == nil || lower == "" {
		return 0, "", "", false
	}
	return d.Ship(lower)
}

func lookupInvType(d Dict, lower string) (int64, string, bool) {
	if d.Type == nil || lower == "" {
		return 0, "", false
	}
	return d.Type(lower)
}

// lookupType finds a whole phrase as a thing: a ship (for its
// class), else any inventory type. A phrase that misses is tried
// once more without a trailing "s", the plural.
func lookupType(d Dict, phrase string) (Ship, bool) {
	for _, cand := range shipCands(phrase) {
		if id, name, class, ok := lookupShip(d, cand); ok {
			return Ship{TypeID: id, Name: name, Class: class}, true
		}
		if id, name, ok := lookupInvType(d, cand); ok {
			return Ship{TypeID: id, Name: name}, true
		}
	}
	return Ship{}, false
}

// placeNouns are words for places and things in space, and the
// distance units a mangled paste leaves behind. Bare, in free-form
// text, they are never a pilot's name -- EVE has characters called
// "Station" and "Moon", and a scout who writes "station" means the
// station. (Typed lists -- a pasted Local, a killmail -- resolve
// names elsewhere and are not bound by this.)
var placeNouns = map[string]bool{
	"station": true, "moon": true, "planet": true, "sun": true,
	"star": true, "belt": true, "stargate": true, "outpost": true,
	"citadel": true, "tower": true, "structure": true,
	"km": true, "au": true,
}

// reportWords are the vocabulary of a report itself: standings
// and the scaffolding of the interpreted sentence a scan report
// carries ("hostiles", "neutral", "on grid", "ships unknown").
// They are read as words, never as names -- a pilot named after
// one resolves in typed lists, not in free-form text, exactly as
// with the place nouns above.
var reportWords = map[string]bool{
	"hostile": true, "hostiles": true, "neutral": true, "neutrals": true,
	"friendly": true, "friendlies": true, "unclassified": true,
	"unresolved": true, "grid": true, "ship": true, "ships": true,
	"scan": true, "attached": true, "unknown": true, "no": true,
	"incl": true, "you": true, "more": true, "killed": true,
	"here": true, "this": true, "week": true, "system": true,
	"hot": true, "kills": true, "npc": true, "likely": true,
	"ratting": true, "heavy": true, "traffic": true, "clusters": true,
	"partial": true, "roster": true, "names": true, "last": true,
	"hours": true, "local": true,
}

// romanRe is a roman numeral, strictly: only numeral letters, in
// numeral order ("civil", "vivid" and "mild" do not match).
var romanRe = regexp.MustCompile(`^m{0,3}(cm|cd|d?c{0,3})(xc|xl|l?x{0,3})(ix|iv|v?i{0,3})$`)

func isRomanNumeral(w string) bool { return w != "" && romanRe.MatchString(w) }

// chatWords are intel chat's own shorthand: the clear, the no
// visual, the question and the sign-offs a scout types alone on a
// line. EVE has characters called "Clr" and "Nv", and a line of
// nothing but "clr" means the system is clear. Like the place
// nouns, they are words in free-form text, never names.
var chatWords = map[string]bool{
	"clr": true, "clear": true, "cleared": true, "nv": true, "novis": true,
	"novisual": true, "status": true, "stat": true, "upd": true, "update": true,
	"inc": true, "incoming": true, "gg": true, "o7": true, "o/": true, "ty": true,
	"thx": true, "np": true, "lol": true, "ok": true, "kk": true, "yes": true,
	"gone": true, "left": true, "still": true, "same": true, "safe": true,
}

// suspectWords are everyday words that are also some pilot's name.
// Read out of free-form text they are as likely the word as the
// pilot, so the reading keeps the pilot and asks.
var suspectWords = map[string]bool{
	"help": true, "test": true, "wtf": true, "bro": true, "dude": true,
	"sure": true, "fine": true, "nope": true, "now": true, "soon": true,
	"again": true, "just": true, "only": true, "there": true, "back": true,
	"out": true, "up": true, "down": true, "off": true, "over": true,
	"come": true, "go": true, "get": true, "look": true, "see": true,
	"seen": true, "watch": true, "careful": true, "hunter": true,
	"hunting": true, "roam": true, "roaming": true, "ess": true,
	"anom": true, "site": true, "rat": true, "rats": true, "ping": true,
	"probe": true, "probes": true, "warp": true, "align": true,
	"aligned": true, "tackle": true, "tackled": true, "dead": true,
	"died": true, "kill": true, "loss": true, "pod": true, "podded": true,
}

// SuspectPilot is whether a pilot read out of free-form text on
// this lower-cased phrase is doubtful: one short word (three
// letters or fewer) or an everyday word. Such a pilot stands, and
// its span goes to the residue step to confirm.
func SuspectPilot(name string) bool {
	if strings.ContainsRune(name, ' ') {
		return false
	}
	return utf8.RuneCountInString(name) <= 3 || suspectWords[name]
}

// PilotCandidate is whether a lower-cased phrase from free-form
// text may be a pilot at all. Not candidates: a string with no
// letter (a number or distance off a paste), a standalone roman
// numeral (a planet's designator, "VIII"), a bare place noun or
// chat shorthand ("clr"), and a phrase ending in a place noun
// ("Minmatar Military Station"). Every reading of free-form text
// asks it, the roster readings of fused text included; only a
// typed list (a Local paste, one name a line) does not.
func PilotCandidate(name string) bool {
	letter := false
	for _, r := range name {
		if unicode.IsLetter(r) {
			letter = true
			break
		}
	}
	if !letter {
		return false
	}
	if isRomanNumeral(name) || placeNouns[name] || reportWords[name] || chatWords[name] {
		return false
	}
	if i := strings.LastIndex(name, " "); i >= 0 && placeNouns[clean(name[i+1:])] {
		return false
	}
	// A phrase of nothing but numbers, designators, place nouns
	// and units ("km 2498", "0 m") has no name in it either. The
	// words are cleaned first: a phrase carries its punctuation
	// inside ("(incl." is the word "incl").
	for _, w := range strings.Fields(name) {
		w = clean(w)
		if !isDigits(w) && !isRomanNumeral(w) && !placeNouns[w] && !reportWords[w] && !chatWords[w] && w != "-" && w != "—" && w != "–" {
			return true
		}
	}
	return false
}

func isDigits(w string) bool {
	w = strings.ReplaceAll(w, ",", "")
	if w == "" {
		return false
	}
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// placeCues are the words that make a line speak of a place:
// the location prepositions, the gate itself, and the movement
// verbs of getting somewhere. A near-miss of a neighbour's name
// only reads as that system where the line speaks of a place;
// a bare name in a list of names stays a name.
var placeCues = map[string]bool{
	"in": true, "at": true, "to": true, "from": true, "into": true, "onto": true,
	"gate": true, "gates": true,
	"jump": true, "jumped": true, "jumps": true, "jumping": true,
	"went": true, "gone": true, "going": true,
	"moved": true, "moving": true, "heading": true, "headed": true,
	"warped": true, "entered": true, "came": true, "through": true,
	"->": true, "=>": true, "-->": true, "<-": true, "→": true, "@": true,
}

// speaksOfPlace is whether any word of seg is a place cue.
func speaksOfPlace(seg []string) bool {
	for _, w := range seg {
		if placeCues[clean(w)] {
			return true
		}
	}
	return false
}

// markLocation marks a celestial's name in seg as read, so none of
// it is offered as pilots or ships: "VII - Moon 1 - Vherokior
// Tribe Treasury" -- a planet designator, its moon, and the
// station's name, which runs to the segment's end or to the
// distance a paste put after it. The system itself was already
// taken out (or never in the text); what follows its designator
// is one location, however many words the station's name runs to.
func markLocation(seg []string, used []bool) {
	for i := 0; i+2 < len(seg); i++ {
		if used[i] || !isRomanNumeral(clean(seg[i])) || clean(seg[i+1]) != "-" {
			continue
		}
		j := i + 2
		moon := false
		if j+1 < len(seg) && clean(seg[j]) == "moon" && isDigits(clean(seg[j+1])) {
			moon, j = true, j+2
		}
		if j < len(seg) && clean(seg[j]) == "-" {
			j++
			// The station's name runs until a distance or the
			// segment's end.
			for j < len(seg) {
				if isDigits(clean(seg[j])) && j+1 < len(seg) {
					if u := clean(seg[j+1]); u == "km" || u == "m" || u == "au" {
						break
					}
				}
				j++
			}
		} else if !moon {
			continue // a designator and a dash, but no location after it
		}
		for k := i; k < j; k++ {
			used[k] = true
		}
		i = j - 1
	}
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// kindOf decides what kind of report it is.
func kindOf(in Input, a Analysis, flags map[string]bool) string {
	hasClass := func(c string) bool {
		for _, s := range a.Ships {
			if s.Class == c {
				return true
			}
		}
		return false
	}
	hostiles := 0
	friends := 0
	for _, p := range a.Pilots {
		if p.Friendly && !p.Red {
			friends++
		} else {
			hostiles++
		}
	}
	something := in.Hostiles > 0 || hostiles > 0 || len(a.Ships) > 0 || len(flags) > 0
	switch {
	case in.Clear && !something:
		return KindClear
	case flags["capital"] || flags["cyno"] || flags["bridge"] || hasClass(ClassCapital):
		return KindCapital
	case flags["camp"]:
		return KindCamp
	case flags["bubble"] || hasClass(ClassInterdict):
		return KindBubble
	case flags["fleet"] || in.Hostiles >= 10 || hostiles >= 10 || hasClass(ClassBomber):
		return KindFleet
	case friends > 0 && hostiles == 0 && in.Hostiles <= friends && len(a.Ships) == 0:
		return KindFriendly
	case in.NoVisual || (flags["local"] && len(a.Ships) == 0):
		if hostiles > 0 || in.Hostiles > 0 || flags["local"] {
			return KindLocal
		}
	case hostiles > 0 || in.Hostiles > 0 || len(a.Ships) > 0 || flags["cloaky"] || flags["docked"]:
		return KindSighting
	}
	if in.Clear {
		return KindClear
	}
	if len(a.Unknown) > 0 {
		// Words that may yet be pilots: a sighting until known.
		return KindSighting
	}
	return KindChatter
}
