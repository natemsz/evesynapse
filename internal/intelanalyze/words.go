package intelanalyze

// ---------------------------------------------------------------------------
// What a name looks like in prose.
//
// EVE has a character for nearly any word: "Help", "Tackled",
// "Local", "Up". A reading that offers every leftover word to the
// pilot lookup finds one, and a scout's sentence comes back as a
// list of characters. So free-form text only reads a pilot out of
// words that look like a name as written (NameLike): a word with
// a capital, a digit or a link mark, or a word that is not plain
// English. A lowercase everyday word never starts, ends or sits
// inside a pilot's name in prose; it is the scout's own words.
//
// A link the scout dragged into chat is copied out with a star
// after it ("Calebyr*", "1DQ1-A*"): the star ends a linked name,
// so a pilot reading never runs past it, and a starred name is
// one the scout meant as a name.
//
// Celestials said in shorthand ("moon 4", "P3M2", "4-4", "the
// sun") are places, read into the analysis's location and never
// offered as names; so are the objects a scan shows that are not
// ships (wrecks, containers, billboards).
// ---------------------------------------------------------------------------

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// commonWords are everyday English and intel chat words: the
// scaffolding of a scout's sentence. Lowercase as written, they
// are never part of a pilot's name in prose.
var commonWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
a about above across after again against ago all almost alone along already also although always am help
among an and another any anyone anything anyway are around as ask asked at away back bad be became because
become been before behind being below beside best better between big both but by call called came can
cant can't come comes coming could did didnt didn't do does doesnt doesn't doing done dont don't down during
each either else enough even ever every everyone everything far few first for from full get gets
getting give go goes going gone good got great had has have having he her here hers him his how however
i i'm im if in inside instead into is isnt isn't it its it's itself just keep kept know last late later least left
less let lets let's like likely little long look looked looking looks lot lots made make many may maybe me
might mine more most much must my near need never new next nice no none nor not nothing now of off
often old on once one only onto or other others our out outside over own past per perhaps please
possible possibly prob probably put quite rather really right said same saw say says see seen seems
she should since so some someone something soon still such sure take taken than that thats that's the their
them then there theres there's these they theyre they're thing things think this those though through thru to
too took toward towards try trying two under until up upon us very via was wasnt wasn't watch way we well
went were what whats what's when where which while who whole whos who's why will with within without wont won't
would yet you your youre you're yours pls plz lit up vis fyi camper campers inc ty thx
also anyone away
jump jumped jumping jumps warp warped warping warps land landed landing lands align aligned aligning
dock docked docking undock undocked undocking log logged logging logoff logon login
move moved moving moves head heading headed heads come came coming leave leaving left arrive arrived
sit sitting sat hold holding held stay staying stayed wait waiting camp camping camped
roam roaming hunt hunting hunter hunters rat ratting ratter mine mining miner miners mined
kill killed killing kills die died dead dying lose lost loss tackle tackled tackling point pointed
scram scrammed web webbed bump bumped bumping shoot shooting shot pop popped popping
anchor anchored anchoring reinforce reinforced reffed timer timers online offline
probe probing probed scan scanning scanned scans dscan d-scan onscan overview grid ongrid offgrid
spike spiked spiking red reds neut neuts blue blues friendly hostile hostiles
fleet fleets gang gangs roam roams blob squad group crew solo
ship ships hull hulls ships pod pods podded
cloak cloaked cloaky cloakies cloaking decloak decloaked
chill careful heads-up headsup incoming inbound outbound rage
system systems gate gates stargate jump-bridge bridge bridges route
station stations structure structures citadel keep ihub tcu skyhook poco
belt belts anom anoms site sites sig sigs signature wormhole hole holes
north south east west side
`) {
		m[w] = true
	}
	return m
}()

// objectNouns are the things a scan shows that are neither ships
// nor pilots: a span that is, or ends in, one of them names the
// thing. EVE has characters called "Wreck" and "Container", and in
// free-form text the object wins.
var objectNouns = map[string]bool{
	"wreck": true, "wrecks": true, "container": true, "containers": true,
	"can": true, "cans": true, "jetcan": true, "jetcans": true, "mtu": true,
	"depot": true, "billboard": true, "beacon": true, "drone": true, "drones": true,
	"probe": true, "probes": true, "bubble": true, "bubbles": true,
	"ihub": true, "tcu": true, "skyhook": true, "poco": true, "ess": true,
	"ansiblex": true, "ansi": true, "jb": true,
}

// NameLike is whether a word, as the scout wrote it, may be part
// of a pilot's name in free-form text: it carries a capital, a
// digit or a link star, or it is not an everyday word.
func NameLike(word string) bool {
	if strings.HasSuffix(word, "*") {
		return true
	}
	w := clean(word)
	if w == "" || objectNouns[w] || chatWords[w] {
		return false
	}
	for _, r := range strings.Trim(word, ",.;:!()[]?\"'") {
		if unicode.IsUpper(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return !commonWords[w]
}

// nameShaped is whether a whole segment of up to three words reads
// as one name: every word NameLike, none a keyword, a count or a
// place cue. A name dragged from Local stands alone between its
// gaps; a short phrase ("still there", "on moon 4") does not.
func nameShaped(seg []string) bool {
	for _, w := range seg {
		c := clean(w)
		if !NameLike(w) || placeCues[c] || isArrow(c) {
			return false
		}
		if _, ok := count(c); ok {
			return false
		}
		if f, ok := keywords[c]; ok && f != "" {
			return false
		}
	}
	return true
}

// linkEnd is whether a word closes a linked name: the star the
// client writes after a dragged link.
func linkEnd(word string) bool {
	return strings.HasSuffix(strings.TrimRight(word, ",.;:!()[]?\"'"), "*")
}

// isArrow is a movement arrow: "->", "=>", ">", "<-".
func isArrow(w string) bool {
	switch w {
	case "->", "=>", ">", "-->", "<-", "<=", "<", "→":
		return true
	}
	return false
}

var (
	pmRe     = regexp.MustCompile(`^p(\d{1,2})\s?m(\d{1,2})$`) // p3m2
	pRe      = regexp.MustCompile(`^p(\d{1,2})$`)              // p3
	mRe      = regexp.MustCompile(`^m(\d{1,2})$`)              // the m2 after a p3
	dashPMRe = regexp.MustCompile(`^(\d{1,2})-(\d{1,2})$`)     // 4-4
)

// markCelestial reads celestials said in shorthand in seg into a
// location, marking their words read: "moon 4", "planet 3 moon 2",
// "P3M2", "p3 m2", "4-4", "belt 2", "the sun". It returns the
// location as the site names it ("Planet 3 Moon 2"), or "".
func markCelestial(seg []string, used []bool) string {
	loc := ""
	set := func(s string, from, to int) {
		if loc == "" {
			loc = s
		}
		for k := from; k < to; k++ {
			used[k] = true
		}
	}
	num := func(i int) (int, bool) {
		if i >= len(seg) || used[i] {
			return 0, false
		}
		n, err := strconv.Atoi(clean(seg[i]))
		return n, err == nil && n > 0 && n < 100
	}
	for i := 0; i < len(seg); i++ {
		if used[i] || linkEnd(seg[i]) {
			continue
		}
		w := clean(seg[i])
		switch {
		case w == "planet" || w == "moon" || w == "belt":
			n, ok := num(i + 1)
			if !ok {
				continue
			}
			s := strings.ToUpper(w[:1]) + w[1:] + " " + strconv.Itoa(n)
			end := i + 2
			if w == "planet" && end+1 < len(seg) && clean(seg[end]) == "moon" {
				if m, ok := num(end + 1); ok {
					s += " Moon " + strconv.Itoa(m)
					end += 2
				}
			}
			set(s, i, end)
			i = end - 1
		case w == "sun" || w == "star":
			set("Sun", i, i+1)
		case pmRe.MatchString(w):
			m := pmRe.FindStringSubmatch(w)
			set("Planet "+m[1]+" Moon "+m[2], i, i+1)
		case pRe.MatchString(w):
			m := pRe.FindStringSubmatch(w)
			s, end := "Planet "+m[1], i+1
			if i+1 < len(seg) && !used[i+1] {
				if mm := mRe.FindStringSubmatch(clean(seg[i+1])); mm != nil {
					s += " Moon " + mm[1]
					end++
				}
			}
			set(s, i, end)
			i = end - 1
		case dashPMRe.MatchString(w) && !countsNext(seg, i):
			m := dashPMRe.FindStringSubmatch(w)
			set("Planet "+m[1]+" Moon "+m[2], i, i+1)
		}
	}
	return loc
}

// countsNext is whether the word after seg[i] says who a number
// counts ("5-6 reds"): then "5-6" is a count, not a planet and moon.
func countsNext(seg []string, i int) bool {
	if i+1 >= len(seg) {
		return false
	}
	w := clean(seg[i+1])
	return reportWords[w] || keywords[w] == "local"
}

// isSystem is whether a lower-cased phrase is a system's exact name.
func isSystem(d Dict, lower string) bool {
	if d.System == nil {
		return false
	}
	_, ok := d.System(lower)
	return ok
}

// gateTo is whether the system id, named as lower, is a neighbour
// of system: its gate leads there.
func gateTo(d Dict, system int64, lower string, id int64) bool {
	g, ok := d.Gate(system, lower)
	return ok && g == id
}

// appendID adds id to ids once.
func appendID(ids []int64, id int64) []int64 {
	for _, v := range ids {
		if v == id {
			return ids
		}
	}
	return append(ids, id)
}

// shipNicks are the short names scouts give hulls, to the hull's
// own name: "sacs" are Sacrileges, a "cane" a Hurricane. Read as
// the hull wherever the ship dictionary knows the full name.
var shipNicks = map[string]string{
	"sac": "sacrilege", "cerb": "cerberus", "vaga": "vagabond", "muni": "muninn",
	"cane": "hurricane", "phoon": "typhoon", "geddon": "armageddon", "domi": "dominix",
	"mega": "megathron", "pest": "tempest", "rattle": "rattlesnake", "mach": "machariel",
	"vindi": "vindicator", "bhaal": "bhaalgorn", "harb": "harbinger",
	"nado": "tornado", "corm": "cormorant", "jag": "jaguar",
	"kiki": "kikimora", "drek": "drekavac", "ikki": "ikitursa", "iki": "ikitursa",
	"rev": "revelation", "nag": "naglfar", "thanny": "thanatos",
	"nid": "nidhoggur", "appo": "apostle", "levi": "leviathan", "rag": "ragnarok",
	"rorq": "rorqual", "barg": "barghest", "cyna": "cynabal",
	"vni": "vexor navy issue", "rni": "raven navy issue", "sni": "scorpion navy issue",
	"cni": "caracal navy issue", "dni": "dominix navy issue", "bni": "brutix navy issue",
	"fni": "ferox navy issue", "ani": "apocalypse navy issue", "mni": "megathron navy issue",
	"hfi": "hurricane fleet issue", "tfi": "typhoon fleet issue", "sfi": "stabber fleet issue",
	"oni": "omen navy issue", "eni": "exequror navy issue",
	"geddon navy": "armageddon navy issue",
}

// shipCands are the names a phrase may be a ship by: as written,
// without a plural "s", and by nickname.
func shipCands(phrase string) []string {
	out := []string{phrase}
	if t := strings.TrimSuffix(phrase, "s"); t != phrase {
		out = append(out, t)
	}
	for _, c := range out {
		if full, ok := shipNicks[c]; ok && full != c {
			out = append(out, full)
		}
	}
	return out
}

// ShipNicknames is the nickname table, short name to the hull's
// full name (lower-cased), for tools that write reports the way
// scouts do.
func ShipNicknames() map[string]string {
	out := make(map[string]string, len(shipNicks))
	for k, v := range shipNicks {
		out[k] = v
	}
	return out
}

// areaCues are the words before a region that make it where pilots
// came from or are going, read ahead of a ship of the same name
// ("came from Curse"). "in" is not one: "in Curse" is the ship.
var areaCues = map[string]bool{"from": true, "to": true, "into": true, "toward": true, "towards": true}

// holdsThing is whether any run of seg's words is a ship, an item,
// a system or an area, or any word a celestial in shorthand: then
// the segment is read word by word, never taken whole as one
// pilot ("Mirx88 Kryos" is a pilot and a Kryos).
func holdsThing(d Dict, seg []string) bool {
	probe := make([]bool, len(seg))
	if markCelestial(seg, probe) != "" {
		return true
	}
	for i := range seg {
		for n := 1; n <= 3 && i+n <= len(seg); n++ {
			phrase := clean(strings.Join(seg[i:i+n], " "))
			if _, ok := lookupType(d, phrase); ok {
				return true
			}
			if isSystem(d, phrase) {
				return true
			}
			if d.Area != nil {
				if _, ok := d.Area(phrase); ok {
					return true
				}
			}
		}
	}
	return false
}

// nameOrder is the order the pilot pass tries run lengths in at
// the head of left: longest first, but a run the classifier read as
// one name goes ahead of the rest.
func nameOrder(seg []string, left []int, names map[string]bool) []int {
	top := min(3, len(left))
	order := make([]int, 0, top)
	if len(names) > 0 {
		for n := top; n >= 1; n-- {
			words := make([]string, n)
			for k := 0; k < n; k++ {
				words[k] = seg[left[k]]
			}
			if names[clean(strings.Join(words, " "))] {
				order = append(order, n)
				break
			}
		}
	}
	for n := top; n >= 1; n-- {
		if len(order) == 0 || order[0] != n {
			order = append(order, n)
		}
	}
	return order
}

// splitByNames is whether the classifier reads a short segment as
// something other than one name: then it is read word by word, not
// taken whole ("Sort Dragon Kanea" is two pilots).
func splitByNames(seg []string, names map[string]bool) bool {
	if len(names) == 0 || len(seg) < 2 {
		return false
	}
	if names[clean(strings.Join(seg, " "))] {
		return false
	}
	for i := range seg {
		for n := 1; n <= len(seg)-i; n++ {
			if names[clean(strings.Join(seg[i:i+n], " "))] {
				return true
			}
		}
	}
	return false
}
