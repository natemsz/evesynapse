// Package intelchat reads lines copied out of an in-game intel chat
// channel into reports: who said it, when, which system, and what --
// a "clear", a count, the rest as said. It is pure: the system
// dictionary comes from the caller (Lookup), and nothing is fetched.
//
// A line as the client copies it:
//
//	[ 2026.10.21 11:20:05 ] Some Scout > 1DQ1-A  Hostile One  Hostile Two +3 nv
//
// The timestamp and speaker are optional (a bare "1DQ1-A clr" reads
// too). The system is found anywhere in the message, longest name
// first (names run up to three words: "Old Man Star"). Counts are
// written "+3", "3x" or "x3", or as a bare number before who they
// are ("5 reds", "12 neuts"); "clr"/"clear" marks the system clear;
// "nv" (no visual) and "status?" questions are understood. What is
// left once those are taken out is the report's text -- usually the
// pilots' names.
package intelchat

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Lookup finds a system by its lowercased name: its ID and proper
// name, or ok false.
type Lookup func(lower string) (id int64, name string, ok bool)

// Report is one line read.
type Report struct {
	At         time.Time // zero when the line carried no timestamp
	Speaker    string
	SystemID   int64
	SystemName string
	Text       string // what is left: usually pilots' names
	Raw        string // the message as written, before anything was taken out
	Hostiles   int    // 0: no count given
	Clear      bool
	NoVisual   bool
}

// Outcome of Parse beyond the reports: lines that named no system,
// and questions ("status?"), neither of which are reports. The
// no-system lines are kept as written, so whoever pasted them can
// be shown what was not read instead of it vanishing, and as read
// -- speaker, time and message -- for a reader that can still
// make something of one.
type Outcome struct {
	Reports       []Report
	NoSystem      int
	NoSystemLines []string
	NoSystemRead  []Report
	Questions     int
}

var (
	stamp   = regexp.MustCompile(`^\[\s*(?:(\d{4})\.(\d{2})\.(\d{2})\s+)?(\d{1,2}):(\d{2})(?::(\d{2}))?\s*\]\s*`)
	countRe = regexp.MustCompile(`^(?:\+(\d{1,3})|(\d{1,3})x|x(\d{1,3}))$`)
)

// maxSystemWords is the most words a system name runs to.
const maxSystemWords = 3

// Parse reads raw, one report a line. now dates a timestamp that
// gives only the time of day (today, or yesterday when that time is
// still ahead), and no timestamp at all means "now" (At stays zero).
func Parse(raw string, lookup Lookup, now time.Time) Outcome {
	var out Outcome
	now = now.UTC()
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		orig := line
		var r Report
		if m := stamp.FindStringSubmatch(line); m != nil {
			r.At = stampTime(m, now)
			line = line[len(m[0]):]
		}
		msg := line
		if i := strings.Index(line, " > "); i >= 0 {
			r.Speaker = strings.TrimSpace(line[:i])
			msg = strings.TrimSpace(line[i+3:])
		}
		r.Raw = msg
		if r.Speaker == "EVE System" || msg == "" {
			continue
		}
		words := strings.Fields(msg)
		used := make([]bool, len(words))
		// The system. Every name in the message is a candidate,
		// a longer name covering the shorter ones inside it, and
		// the scout's own system is the best placed of them:
		//   - not followed by "gate": that is the gate's far side
		//     ("on bosena gate in helid" is in Helid);
		//   - said to be "in" or "@" the system;
		//   - the message's first word, where scouts put it;
		//   - else the longest, then the earliest.
		// A name dragged in as a link counts like a typed one.
		type cand struct {
			i, n     int
			id       int64
			proper   string
			gate, in bool
			first    bool
		}
		var cands []cand
		covered := make([]bool, len(words))
		for n := maxSystemWords; n >= 1; n-- {
			for i := 0; i+n <= len(words); i++ {
				if slices.Contains(covered[i:i+n], true) {
					continue
				}
				name := strings.ToLower(strings.Join(trimAll(words[i:i+n]), " "))
				if name == "" {
					continue
				}
				if id, proper, ok := lookup(name); ok {
					c := cand{i: i, n: n, id: id, proper: proper, first: i == 0}
					c.gate = i+n < len(words) && strings.HasPrefix(strings.ToLower(trim(words[i+n])), "gate")
					// "in Y gate camp" is a camp in Y, not Y's gate
					// ("@ X gate camped" stays X's gate).
					if c.gate && i > 0 && strings.EqualFold(words[i-1], "in") && i+n+1 < len(words) && strings.HasPrefix(strings.ToLower(trim(words[i+n+1])), "camp") {
						c.gate = false
					}
					c.in = i > 0 && (strings.EqualFold(words[i-1], "in") || words[i-1] == "@")
					cands = append(cands, c)
					for k := i; k < i+n; k++ {
						covered[k] = true
					}
				}
			}
		}
		if len(cands) > 0 {
			rank := func(c cand) [4]int {
				return [4]int{boolRank(!c.gate), boolRank(c.in), boolRank(c.first), c.n}
			}
			best := cands[0]
			for _, c := range cands[1:] {
				rb, rc := rank(best), rank(c)
				if slices.Compare(rc[:], rb[:]) > 0 || (rc == rb && c.i < best.i) {
					best = c
				}
			}
			r.SystemID, r.SystemName = best.id, best.proper
			for k := best.i; k < best.i+best.n; k++ {
				used[k] = true
			}
		}
		// A short message ending in a question mark asks ("anyone
		// in BY-7PY?"), wherever the mark sits.
		question := strings.HasSuffix(msg, "?") && len(words) <= 4
		// "3x" and "x3" multiply a ship ("sabre x3") wherever a
		// word stands beside them; they count hostiles only on a
		// line with nothing else to count ("Jita 5x"). "+3" always
		// counts hostiles.
		var times []int
		for i, w := range words {
			// A word dragged in as a link ("Clear*", "Red*") is a
			// name, never the channel's shorthand.
			if used[i] || isLink(w) {
				continue
			}
			lw := strings.ToLower(trim(w))
			switch {
			case lw == "clr" || lw == "clear" || lw == "cleared":
				r.Clear, used[i] = true, true
			case lw == "nv" || lw == "novis" || lw == "novisual":
				r.NoVisual, used[i] = true, true
			case lw == "no" && i+1 < len(words) && !used[i+1] && (strings.EqualFold(trim(words[i+1]), "vis") || strings.EqualFold(trim(words[i+1]), "visual")):
				r.NoVisual, used[i], used[i+1] = true, true, true
			case i+1 < len(words) && !used[i+1] && !isLink(words[i+1]) && bareCount(lw) > 0 && hostileNoun(strings.ToLower(trim(words[i+1]))):
				// "5 reds": a bare number counts when who they are
				// follows it; the pair is the count, not text.
				r.Hostiles += bareCount(lw)
				used[i], used[i+1] = true, true
			case lw == "status" || lw == "stat" || strings.HasSuffix(w, "?") && len(words) <= 3:
				question = true
			default:
				if m := countRe.FindStringSubmatch(lw); m != nil {
					if m[1] == "" {
						times = append(times, i)
						continue
					}
					n, _ := strconv.Atoi(m[1])
					r.Hostiles += n
					used[i] = true
				}
			}
		}
		if len(times) > 0 && !hasWords(words, used, times) {
			for _, i := range times {
				m := countRe.FindStringSubmatch(strings.ToLower(trim(words[i])))
				n, _ := strconv.Atoi(m[2] + m[3])
				r.Hostiles += n
				used[i] = true
			}
		}
		if r.SystemID == 0 {
			// Read like a report's: the clear, the no visual and
			// the counts are taken out, so a reader of what is
			// left never takes "clr" for a name. Raw keeps it all.
			r.Text = restText(msg, used)
			out.NoSystem++
			out.NoSystemLines = append(out.NoSystemLines, orig)
			out.NoSystemRead = append(out.NoSystemRead, r)
			continue
		}
		if question && !r.Clear && r.Hostiles == 0 {
			out.Questions++
			continue
		}
		r.Text = restText(msg, used)
		out.Reports = append(out.Reports, r)
	}
	return out
}

// restText is what is left of msg once the used words are taken out.
// Names dragged from the game's Local are pasted two spaces apart,
// and the analyser splits pilots there (internal/intelanalyze), so
// such a gap is kept, and a word taken out leaves one too; any other
// run of space is one space.
func restText(msg string, used []bool) string {
	var b strings.Builder
	gap := ""
	for i, span := range wordRe.FindAllStringIndex(msg, -1) {
		if i < len(used) && used[i] {
			gap = "  "
			continue
		}
		if b.Len() > 0 {
			if gap == "" {
				gap = " "
				if prev := msg[:span[0]]; strings.HasSuffix(prev, "  ") || strings.HasSuffix(prev, "\t") {
					gap = "  "
				}
			}
			b.WriteString(gap)
		}
		b.WriteString(msg[span[0]:span[1]])
		gap = ""
	}
	return b.String()
}

// bareCount reads a plain number ("5", "12") as a count. Zero and
// anything longer than three digits are not counts.
func bareCount(w string) int {
	if len(w) == 0 || len(w) > 3 {
		return 0
	}
	n, err := strconv.Atoi(w)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// hostileNoun is who a bare count counts: the words intel uses for
// pilots who are not friendly.
func hostileNoun(w string) bool {
	switch w {
	case "red", "reds", "neut", "neuts", "hostile", "hostiles":
		return true
	}
	return false
}

var wordRe = regexp.MustCompile(`\S+`)

func trim(w string) string { return strings.Trim(w, ",.;:*!()[]?") }

func trimAll(ws []string) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = trim(w)
	}
	return out
}

// stampTime reads a chat timestamp (EVE time is UTC). A time of day
// alone is the latest such moment not after now.
func stampTime(m []string, now time.Time) time.Time {
	h, _ := strconv.Atoi(m[4])
	mi, _ := strconv.Atoi(m[5])
	s := 0
	if m[6] != "" {
		s, _ = strconv.Atoi(m[6])
	}
	if m[1] != "" {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return time.Date(y, time.Month(mo), d, h, mi, s, 0, time.UTC)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, s, 0, time.UTC)
	if t.After(now.Add(time.Minute)) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// hasWords is whether any word not yet read, other than those at
// skip, has a letter in it: something a count could belong to.
func hasWords(words []string, used []bool, skip []int) bool {
	for i, w := range words {
		if used[i] || slices.Contains(skip, i) {
			continue
		}
		for _, r := range w {
			if unicode.IsLetter(r) {
				return true
			}
		}
	}
	return false
}

// isLink is whether a word closes a name dragged in as a link: the
// client writes a star after it.
func isLink(w string) bool {
	return strings.HasSuffix(strings.TrimRight(w, ",.;:!()[]?"), "*")
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
