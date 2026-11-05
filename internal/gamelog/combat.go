package gamelog

// The game log's combat grammar. The client writes one game log per
// session into Gamelogs, beside Chatlogs, named
//
//	20261104_120000_90000001.txt
//
// (when the session began, and the listening character's ID), with a
// header naming the listener, then one line an event:
//
//	[ 2026.11.04 12:00:05 ] (combat) <color=0xff00ffff><b>523</b> ... to ...
//
// The markup carries colors and fonts; stripped, the combat lines
// read as plain sentences, which is what ParseCombat reads. A line of
// the combat kind that matches no rule here is counted, never
// guessed at, so a client that changes its wording shows up as a
// rising unparsed count rather than as invented numbers.

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event kinds.
const (
	EvDamage = "damage" // Amount is hit points; Quality may be set
	EvMiss   = "miss"   // a shot that did nothing
	EvRep    = "rep"    // Amount is hit points repaired; Layer says which
	EvCap    = "cap"    // Amount is GJ neutralized or drained
	EvTackle = "tackle" // Weapon is "scramble" or "disruption"; Party tackles To
)

// Directions, from the listening pilot's side.
const (
	DirOut = "out" // the pilot did it to Party
	DirIn  = "in"  // Party did it to the pilot
)

// Hit quality words, normalized: the only place weapon application
// is ever recorded.
const (
	QMiss      = "miss"
	QGraze     = "graze"
	QGlance    = "glance"
	QHit       = "hit"
	QPenetrate = "penetrate"
	QSmash     = "smash"
	QWreck     = "wreck"
)

// Qualities lists the quality words worst to best.
var Qualities = []string{QMiss, QGraze, QGlance, QHit, QPenetrate, QSmash, QWreck}

var qualityWords = map[string]string{
	"misses":           QMiss,
	"grazes":           QGraze,
	"glances off":      QGlance,
	"hits":             QHit,
	"penetrates":       QPenetrate,
	"smashes":          QSmash,
	"wrecks":           QWreck,
	"barely scratches": QGraze,
	"lightly hits":     QGlance,
	"heavily hits":     QPenetrate,
	"barely misses":    QMiss,
}

// Party is the other side of an event as the log names it. A player
// ship reads "Name[TICK](Hull)". A drone or a missile in flight wears
// the same decoration with its own type in the brackets (a drone's
// name and type agree; a missile's type is a charge), and an NPC, a
// structure, or a ship the client names by hull alone (repairs,
// energy warfare and tackle lines do) reads as a bare name. Pilot is
// true only for the first.
type Party struct {
	Name   string
	Ticker string
	Hull   string
	Pilot  bool
}

// CombatEvent is one recognized line.
type CombatEvent struct {
	At      time.Time
	Kind    string
	Dir     string
	Party   Party
	Amount  int64
	Weapon  string // the module, drone or charge the line names
	Quality string // damage and misses only
	Layer   string // reps only: shield, armor, hull
	To      Party  // tackle only: the ship Party tried to hold
}

var (
	gameStamp  = regexp.MustCompile(`^\[ (\d{4}\.\d{2}\.\d{2} \d{2}:\d{2}:\d{2}) \] \((\w+)\) (.*)$`)
	markup     = regexp.MustCompile(`<[^>]*>`)
	firstColor = regexp.MustCompile(`(?i)<color=0x([0-9a-f]{8})>`)
	partyDeco  = regexp.MustCompile(`^(.*?)\s*\[([^\]]*)\]\s*\(([^)]+)\)$`)

	reDamage  = regexp.MustCompile(`^(\d+) (to|from) (.+)$`)
	reMissIn  = regexp.MustCompile(`^(.+?) misses you completely(?: - (.+))?$`)
	reMissOut = regexp.MustCompile(`^Your (?:group of )?(.+?) misses (.+?) completely(?: - (.+))?$`)
	reRep     = regexp.MustCompile(`^(\d+) remote (armor|shield|hull|capacitor) (?:repaired|boosted|transmitted) (by|to) (.+?)(?: - (.+))?$`)
	reCap     = regexp.MustCompile(`^([+-]?\d+) GJ energy (neutralized|drained) (?:(from|to) )?(.+?)(?: - (.+))?$`)
	reTackle  = regexp.MustCompile(`^Warp (scramble|disruption) attempt from (.+?) to (.+?)!?$`)
)

// incomingColor is the red the client paints effects on the pilot
// with; an energy neutralized line carries no other direction word.
const incomingColor = "ffe57f7f"

// ParseCombat reads one game log line. combat is whether the line
// is of the (combat) kind at all (so an unrecognized one can be
// counted); ok whether a rule read it.
func ParseCombat(line string) (ev CombatEvent, combat, ok bool) {
	m := gameStamp.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return ev, false, false
	}
	at, err := time.ParseInLocation("2006.01.02 15:04:05", m[1], time.UTC)
	if err != nil {
		return ev, false, false
	}
	kind, raw := m[2], m[3]
	if kind != "combat" && kind != "notify" {
		return ev, false, false
	}
	color := ""
	if c := firstColor.FindStringSubmatch(raw); c != nil {
		color = strings.ToLower(c[1])
	}
	text := strings.Join(strings.Fields(markup.ReplaceAllString(raw, " ")), " ")
	text = strings.ReplaceAll(text, " [ ", " [")
	text = strings.ReplaceAll(text, " ] ", "] ")
	ev.At = at
	if t := reTackle.FindStringSubmatch(text); t != nil {
		// The client logs every scramble and disruption attempt
		// it sees on grid, not only those on the pilot, and names
		// both ships by hull. Dir is set only when a side reads
		// "you"; otherwise the line is an observation, and the site
		// decides what it says about the pilot.
		ev.Kind, ev.Weapon = EvTackle, t[1]
		ev.Party, ev.To = parseParty(t[2]), parseParty(t[3])
		switch {
		case strings.EqualFold(ev.To.Name, "you"):
			ev.Dir = DirIn
		case strings.EqualFold(ev.Party.Name, "you"):
			ev.Dir = DirOut
		}
		return ev, true, ev.Party.Name != "" && ev.To.Name != ""
	}
	if kind == "notify" {
		return ev, false, false
	}
	return parseCombatText(ev, text, color)
}

func parseCombatText(ev CombatEvent, text, color string) (CombatEvent, bool, bool) {
	if t := reMissIn.FindStringSubmatch(text); t != nil && !strings.HasPrefix(text, "Your ") {
		ev.Kind, ev.Dir, ev.Quality = EvMiss, DirIn, QMiss
		ev.Party = parseParty(t[1])
		ev.Weapon = strings.TrimSpace(t[2])
		return ev, true, ev.Party.Name != ""
	}
	if t := reMissOut.FindStringSubmatch(text); t != nil {
		ev.Kind, ev.Dir, ev.Quality = EvMiss, DirOut, QMiss
		ev.Weapon = strings.TrimSpace(t[1])
		ev.Party = parseParty(t[2])
		return ev, true, ev.Party.Name != ""
	}
	if t := reRep.FindStringSubmatch(text); t != nil {
		if t[2] == "capacitor" {
			// Capacitor transfers are read (not unparsed) but are
			// not repairs and not pressure: nothing to count.
			return ev, true, false
		}
		ev.Kind, ev.Layer = EvRep, t[2]
		ev.Amount, _ = strconv.ParseInt(t[1], 10, 64)
		if t[3] == "by" {
			ev.Dir = DirIn
		} else {
			ev.Dir = DirOut
		}
		ev.Party = parseParty(t[4])
		ev.Weapon = strings.TrimSpace(t[5])
		return ev, true, ev.Party.Name != ""
	}
	if t := reCap.FindStringSubmatch(text); t != nil {
		ev.Kind = EvCap
		n, _ := strconv.ParseInt(t[1], 10, 64)
		ev.Amount = n
		if n < 0 {
			ev.Amount = -n
		}
		switch {
		case t[2] == "drained" && t[3] == "from":
			ev.Dir = DirOut
		case t[2] == "drained":
			ev.Dir = DirIn
		case n < 0 || color == incomingColor:
			ev.Dir = DirIn
		default:
			ev.Dir = DirOut
		}
		ev.Party = parseParty(t[4])
		ev.Weapon = strings.TrimSpace(t[5])
		// A zero cycle (a nosferatu against an empty capacitor)
		// is read and counts nothing.
		return ev, true, ev.Party.Name != ""
	}
	if t := reDamage.FindStringSubmatch(text); t != nil {
		ev.Kind = EvDamage
		ev.Amount, _ = strconv.ParseInt(t[1], 10, 64)
		ev.Dir = DirOut
		if t[2] == "from" {
			ev.Dir = DirIn
		}
		parts := strings.Split(t[3], " - ")
		if q, ok := qualityWords[strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))]; ok && len(parts) > 1 {
			ev.Quality = q
			parts = parts[:len(parts)-1]
		}
		if len(parts) > 1 {
			ev.Weapon = strings.TrimSpace(strings.Join(parts[1:], " - "))
		}
		ev.Party = parseParty(parts[0])
		return ev, true, ev.Party.Name != ""
	}
	return ev, true, false
}

// parseParty reads "Name[TICK](Hull)" or a bare name.
func parseParty(s string) Party {
	s = strings.TrimSpace(s)
	if m := partyDeco.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		p := Party{Name: strings.TrimSpace(m[1]), Ticker: strings.TrimSpace(m[2]), Hull: strings.TrimSpace(m[3]), Pilot: true}
		p.Pilot = !strings.EqualFold(p.Name, p.Hull) && !isCharge(p.Hull)
		return p
	}
	return Party{Name: s}
}

// chargeWords mark a missile in flight's type.
var chargeWords = []string{"Missile", "Rocket", "Torpedo", "Bomb"}

func isCharge(t string) bool {
	for _, w := range chargeWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}
