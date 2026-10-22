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
// written "+3", "3x" or "x3"; "clr"/"clear" marks the system clear;
// "nv" (no visual) and "status?" questions are understood. What is
// left once those are taken out is the report's text -- usually the
// pilots' names.
package intelchat

import (
	"regexp"
	"strconv"
	"strings"
	"time"
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
	Hostiles   int    // 0: no count given
	Clear      bool
	NoVisual   bool
}

// Outcome of Parse beyond the reports: lines that named no system,
// and questions ("status?"), neither of which are reports.
type Outcome struct {
	Reports   []Report
	NoSystem  int
	Questions int
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
		if r.Speaker == "EVE System" || msg == "" {
			continue
		}
		words := strings.Fields(msg)
		used := make([]bool, len(words))
		// The system: the longest name found, earliest first.
	find:
		for n := maxSystemWords; n >= 1; n-- {
			for i := 0; i+n <= len(words); i++ {
				name := strings.ToLower(strings.Join(trimAll(words[i:i+n]), " "))
				if name == "" {
					continue
				}
				if id, proper, ok := lookup(name); ok {
					r.SystemID, r.SystemName = id, proper
					for k := i; k < i+n; k++ {
						used[k] = true
					}
					break find
				}
			}
		}
		question := false
		for i, w := range words {
			if used[i] {
				continue
			}
			lw := strings.ToLower(trim(w))
			switch {
			case lw == "clr" || lw == "clear":
				r.Clear, used[i] = true, true
			case lw == "nv" || lw == "novis" || lw == "novisual":
				r.NoVisual, used[i] = true, true
			case lw == "status" || lw == "status?" || strings.HasSuffix(w, "?") && len(words) <= 3:
				question = true
			default:
				if m := countRe.FindStringSubmatch(lw); m != nil {
					for _, g := range m[1:] {
						if g != "" {
							n, _ := strconv.Atoi(g)
							r.Hostiles += n
						}
					}
					used[i] = true
				}
			}
		}
		if r.SystemID == 0 {
			out.NoSystem++
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

var wordRe = regexp.MustCompile(`\S+`)

func trim(w string) string { return strings.Trim(w, ",.;:*!()[]") }

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
