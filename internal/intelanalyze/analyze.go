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

// Analysis is what a report says.
type Analysis struct {
	Kind    string   `json:"kind"`
	Pilots  []Pilot  `json:"pilots,omitempty"`
	Ships   []Ship   `json:"ships,omitempty"`
	Gate    int64    `json:"gate,omitempty"` // the system the gate named leads to
	Flags   []string `json:"flags,omitempty"`
	Count   int      `json:"count,omitempty"`
	Unknown []string `json:"-"` // names to look up, then analyse again
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
	"gate": "", "on": "", "at": "", "in": "", "and": "", "the": "", "x": "", "+": "", "-": "", "/": "", "with": "",
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
	// Segments: names dragged from Local come two spaces apart, and
	// commas or slashes part a list.
	text := strings.NewReplacer(",", "  ", " / ", "  ", ";", "  ").Replace(in.Text)
	var segs [][]string
	for _, s := range strings.Split(text, "  ") {
		if f := strings.Fields(s); len(f) > 0 {
			segs = append(segs, f)
		}
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
	for _, seg := range segs {
		whole := clean(strings.Join(seg, " "))
		if len(seg) <= 3 {
			// A whole segment that is a type's name is the thing
			// (asked before pilots: a character may share the
			// name), and one that is a pilot's name is one pilot.
			if s, ok := lookupType(d, whole); ok {
				addShip(s, 0)
				continue
			}
			if id, name, ok, _ := lookupPilot(d, whole); ok {
				if !seenPilot[id] {
					seenPilot[id] = true
					a.Pilots = append(a.Pilots, Pilot{ID: id, Name: name})
				}
				continue
			}
		}
		// Otherwise word by word: ships (up to three words, longest
		// first), gates ("X gate"), counts, keywords, then pilots.
		used := make([]bool, len(seg))
		markLocation(seg, used)
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
			for n := 3; n >= 1 && !matched; n-- {
				if i+n > len(seg) {
					continue
				}
				phrase := clean(strings.Join(seg[i:i+n], " "))
				// "X gate": the gate to a neighbour.
				if i+n < len(seg) && clean(seg[i+n]) == "gate" && d.Gate != nil {
					if g, ok := d.Gate(in.System, phrase); ok {
						a.Gate = g
						for k := i; k <= i+n; k++ {
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
				for _, cand := range []string{phrase, strings.TrimSuffix(phrase, "s")} {
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
			}
			if matched {
				continue
			}
			if f, ok := keywords[w]; ok {
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
		// first; what no lookup knows is asked about later.
		var left []string
		flush := func() {
			for len(left) > 0 {
				took := 0
				for n := min(3, len(left)); n >= 1; n-- {
					name := clean(strings.Join(left[:n], " "))
					id, proper, ok, known := lookupPilot(d, name)
					if ok {
						if !seenPilot[id] {
							seenPilot[id] = true
							a.Pilots = append(a.Pilots, Pilot{ID: id, Name: proper})
						}
						took = n
						break
					}
					if !known {
						a.Unknown = append(a.Unknown, name)
					}
				}
				if took == 0 {
					took = 1
				}
				left = left[took:]
			}
		}
		for i, w := range seg {
			if used[i] {
				flush()
				continue
			}
			left = append(left, w)
		}
		flush()
	}
	if d.Friend != nil {
		for i := range a.Pilots {
			a.Pilots[i].Friendly, a.Pilots[i].Red = d.Friend(a.Pilots[i].ID)
		}
	}
	for f := range flags {
		a.Flags = append(a.Flags, f)
	}
	sort.Strings(a.Flags)
	a.Unknown = dedupe(a.Unknown)
	a.Kind = kindOf(in, a, flags)
	return a
}

func lookupPilot(d Dict, lower string) (int64, string, bool, bool) {
	if d.Pilot == nil || lower == "" || len(lower) > 37 {
		return 0, "", false, true
	}
	if !pilotCandidate(lower) {
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
	for _, cand := range []string{phrase, strings.TrimSuffix(phrase, "s")} {
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

// romanRe is a roman numeral, strictly: only numeral letters, in
// numeral order ("civil", "vivid" and "mild" do not match).
var romanRe = regexp.MustCompile(`^m{0,3}(cm|cd|d?c{0,3})(xc|xl|l?x{0,3})(ix|iv|v?i{0,3})$`)

func isRomanNumeral(w string) bool { return w != "" && romanRe.MatchString(w) }

// pilotCandidate is whether a phrase from free-form text may be a
// pilot at all. Not candidates: a string with no letter (a number
// or distance off a paste), a standalone roman numeral (a planet's
// designator, "VIII"), a bare place noun, and a phrase ending in
// one ("Minmatar Military Station").
func pilotCandidate(name string) bool {
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
	if isRomanNumeral(name) || placeNouns[name] {
		return false
	}
	if i := strings.LastIndex(name, " "); i >= 0 && placeNouns[name[i+1:]] {
		return false
	}
	// A phrase of nothing but numbers, designators, place nouns
	// and units ("km 2498", "0 m") has no name in it either.
	for _, w := range strings.Fields(name) {
		if !isDigits(w) && !isRomanNumeral(w) && !placeNouns[w] && w != "-" {
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
