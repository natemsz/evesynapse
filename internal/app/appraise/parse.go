package appraise

// The paste parsers. A paste arrives as one of the shapes
// players actually copy: an EFT fit (handled by the fitting
// tool's own parser, see appraise.go), or a line list — plain
// lists, multibuy lines, cargo-scan and asset-window copies.
// The list parser is deliberately liberal: it does not decide
// what a line means, it produces name candidates in precedence
// order (the full name as typed first, then the name with a
// trailing quantity read off it, then the name with a mutation
// prefix stripped), and resolution against the item database
// picks the first candidate that names a real type. That
// ordering is what keeps "Cap Booster 3200" (a type whose name
// ends in a number) safe while "Tritanium 1000000" still reads
// as a quantity. Nothing is silently dropped: a line with no
// usable name lands in Bad, a line whose candidates all miss
// lands in the result's unresolved list with its text intact.
// A line carrying the game's showinfo item link is the one
// exception to name-first reading: the link's ids identify a
// unique item instance (see resolveList), and its candidates
// exist only as the base-item fallback for when that item's
// rolls are not on record.

import (
	"regexp"
	"strconv"
	"strings"
)

// maxPasteLines caps one appraisal's input, in non-empty
// lines; the page says the cap out loud.
const maxPasteLines = 1000

// nameCandidate is one reading of a pasted line: a type name
// and the quantity that reading carries.
type nameCandidate struct {
	Name        string
	Qty         int64
	AbyssalBase bool // reached by stripping an "Abyssal "/"Mutated " prefix
}

// pasteLine is one parsed input line: its candidate readings
// plus any blueprint facts the line carried.
type pasteLine struct {
	Candidates []nameCandidate
	Copy       bool // blueprint annotation said copy (BPC)
	HasFacts   bool // a blueprint annotation was present at all
	ME, TE     int64
	HasME      bool
	HasTE      bool
	Runs       int64
	HasRuns    bool
	// A showinfo item link (<url=showinfo:TYPE//ITEM>) names
	// one unique item instance: the type and item ids ride
	// here, and LinkName is the link's display text.
	LinkTypeID int64
	LinkItemID int64
	LinkName   string
	Raw        string
}

// pasteResult is a whole parsed paste.
type pasteResult struct {
	Lines     []pasteLine
	Bad       []string // raw lines with no usable name in them
	Truncated bool     // input ran past maxPasteLines
}

var (
	reQtyPrefix = regexp.MustCompile(`^([\d,]+(?:\.\d+)?)\s*[x×]\s*(.+)$`)
	reQtySuffix = regexp.MustCompile(`^(.+?)\s*[x×]\s*([\d,]+(?:\.\d+)?)$`)
	reCommaQty  = regexp.MustCompile(`^(.+?),\s*([\d,]+(?:\.\d+)?)$`)
	// A trailing volume ("1,200 m3", "12.5 m³") is a note
	// about size, never a quantity: the number in front of
	// the unit measures the stack's volume, not its count.
	reVolumeSuffix = regexp.MustCompile(`(?i)[\s,]+[\d,]+(?:\.\d+)?\s*(?:m3|m³|m\^3)\s*$`)
	reVolumeField  = regexp.MustCompile(`(?i)^[\d,]+(?:\.\d+)?\s*(?:m3|m³|m\^3)$`)
	// The game's item-link markup: an item link names one
	// unique item instance (type id // item id); a type link
	// names only the type.
	reShowinfoItem = regexp.MustCompile(`<url=showinfo:(\d+)//(\d+)>([^<]*)</url>`)
	reShowinfoType = regexp.MustCompile(`<url=showinfo:(\d+)>([^<]*)</url>`)
	reME           = regexp.MustCompile(`^me[:=]?(\d+)$`)
	reTE           = regexp.MustCompile(`^te[:=]?(\d+)$`)
	reRuns         = regexp.MustCompile(`^(\d+)runs?$`)
	reRunsLabel    = regexp.MustCompile(`^runs?[:=]?(\d+)$`)
)

// parseQtyToken reads one quantity token ("1,000,000", "500").
// A decimal token counts only when it is whole: "20.0" is 20,
// never 200 (the point is not a group separator), and "20.5"
// is not a quantity at all. The ceiling keeps a pasted price
// column or an item id from becoming an absurd quantity.
func parseQtyToken(s string) (int64, bool) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, ",", ""), " ", "")
	if s == "" || len(s) > 15 {
		return 0, false
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac := s[:i], s[i+1:]
		if whole == "" || frac == "" {
			return 0, false
		}
		for _, c := range frac {
			if c != '0' {
				return 0, false
			}
		}
		s = whole
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// isNumberToken reports whether a token is all digits and
// commas — including "000", a thousand group that is not a
// quantity by itself. A whole decimal ("20.0") counts too.
func isNumberToken(s string) bool {
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return false
	}
	_, ok := parseQtyToken(s)
	if ok {
		return true
	}
	// "000" and friends are number-shaped without being a
	// usable quantity; they still mark a trailing number run.
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// bpAnnotation parses the inside of a trailing "(...)"/"[...]"
// group for blueprint facts: BPO/BPC, ME n, TE n, n runs, in
// any order and most spellings. ok is false when the group
// holds none of that vocabulary (some type names carry
// parentheses of their own; those are not annotations).
func bpAnnotation(inner string) (pl *pasteLine, ok bool) {
	pl = &pasteLine{}
	toks := strings.FieldsFunc(strings.ToLower(inner), func(r rune) bool {
		return r == ' ' || r == ',' || r == ';' || r == '\t'
	})
	for i, tok := range toks {
		switch {
		case tok == "bpo":
			pl.HasFacts, pl.Copy = true, false
		case tok == "bpc":
			pl.HasFacts, pl.Copy = true, true
		case reME.MatchString(tok):
			pl.ME, _ = strconv.ParseInt(reME.FindStringSubmatch(tok)[1], 10, 64)
			pl.HasFacts, pl.HasME = true, true
		case reTE.MatchString(tok):
			pl.TE, _ = strconv.ParseInt(reTE.FindStringSubmatch(tok)[1], 10, 64)
			pl.HasFacts, pl.HasTE = true, true
		case reRuns.MatchString(tok):
			pl.Runs, _ = strconv.ParseInt(reRuns.FindStringSubmatch(tok)[1], 10, 64)
			pl.HasFacts, pl.HasRuns = true, true
		case reRunsLabel.MatchString(tok):
			pl.Runs, _ = strconv.ParseInt(reRunsLabel.FindStringSubmatch(tok)[1], 10, 64)
			pl.HasFacts, pl.HasRuns = true, true
		case i+1 < len(toks) && (toks[i+1] == "runs" || toks[i+1] == "run"):
			if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
				pl.Runs, pl.HasFacts, pl.HasRuns = n, true, true
			}
		case (tok == "me" || tok == "te" || tok == "runs" || tok == "run") && i+1 < len(toks):
			if n, err := strconv.ParseInt(toks[i+1], 10, 64); err == nil {
				pl.HasFacts = true
				switch tok {
				case "me":
					pl.ME, pl.HasME = n, true
				case "te":
					pl.TE, pl.HasTE = n, true
				default:
					pl.Runs, pl.HasRuns = n, true
				}
			}
		}
	}
	return pl, pl.HasFacts
}

// stripAnnotation removes a trailing blueprint annotation
// group from a line, returning the line's facts (nil when the
// trailing group is not an annotation).
func stripAnnotation(work string) (string, *pasteLine) {
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}} {
		if !strings.HasSuffix(work, pair[1]) {
			continue
		}
		open := strings.LastIndex(work, pair[0])
		if open < 0 {
			continue
		}
		if pl, ok := bpAnnotation(work[open+1 : len(work)-1]); ok {
			return strings.TrimSpace(work[:open]), pl
		}
	}
	return work, nil
}

// trailingReadings reads quantities off the end of a name:
// the trailing run of number tokens, in two readings — the
// whole run as one thousand-grouped number ("Tritanium
// 1 000 000"), and just the last token ("Widget 5 250": the
// type is "Widget 5", the quantity 250). Both are candidates;
// resolution order decides. Empty when the name does not end
// in number tokens or nothing would be left of the name.
func trailingReadings(name string) []nameCandidate {
	fields := strings.Fields(name)
	if len(fields) < 2 {
		return nil
	}
	end := len(fields)
	start := end
	for start > 0 {
		if !isNumberToken(fields[start-1]) {
			break
		}
		start--
	}
	if start == end || start == 0 {
		return nil
	}
	var out []nameCandidate
	if end-start > 1 {
		// Thousand-grouped shape: every group after the
		// first is exactly 3 digits.
		grouped := true
		for i := start + 1; i < end; i++ {
			if len(strings.ReplaceAll(fields[i], ",", "")) != 3 {
				grouped = false
				break
			}
		}
		if first := strings.ReplaceAll(fields[start], ",", ""); len(first) > 3 {
			grouped = false
		}
		if grouped {
			if qty, ok := parseQtyToken(strings.Join(fields[start:end], "")); ok {
				out = append(out, nameCandidate{Name: strings.Join(fields[:start], " "), Qty: qty})
			}
		}
	}
	if qty, ok := parseQtyToken(fields[end-1]); ok {
		out = append(out, nameCandidate{Name: strings.Join(fields[:end-1], " "), Qty: qty})
	}
	return out
}

// mutationBase strips one mutation prefix ("Abyssal ",
// "Mutated ") from a name.
func mutationBase(name string) (string, bool) {
	for _, p := range []string{"Abyssal ", "Mutated "} {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return strings.TrimSpace(name[len(p):]), true
		}
	}
	return "", false
}

// candidates builds a line's readings in precedence order:
// as typed, with a trailing quantity read off, and each of
// those with a mutation prefix stripped.
func candidates(name string, qty int64, qtyExplicit bool) []nameCandidate {
	out := []nameCandidate{{Name: name, Qty: qty}}
	if !qtyExplicit {
		out = append(out, trailingReadings(name)...)
	}
	for _, c := range append([]nameCandidate{}, out...) {
		if base, ok := mutationBase(c.Name); ok {
			out = append(out, nameCandidate{Name: base, Qty: c.Qty, AbyssalBase: true})
		}
	}
	return out
}

// linkCandidates builds a linked line's name readings: a
// link names one unique (usually mutated) item, so when its
// rolls cannot price it the honest fallback is its base
// item — the mutation-stripped reading goes first, the name
// as linked second. Quantity is 1: the link is one item,
// whatever quantity text surrounds it.
func linkCandidates(name string) []nameCandidate {
	if base, ok := mutationBase(name); ok {
		return []nameCandidate{
			{Name: base, Qty: 1, AbyssalBase: true},
			{Name: name, Qty: 1},
		}
	}
	return []nameCandidate{{Name: name, Qty: 1}}
}

// parseListLine parses one non-EFT line into a pasteLine.
// ok is false when the line carries no usable name at all.
func parseListLine(raw string) (pasteLine, bool) {
	work := strings.TrimSpace(raw)
	// An item link anywhere in the line makes the line that
	// one item: quantity text around it does not multiply a
	// unique instance.
	if m := reShowinfoItem.FindStringSubmatch(work); m != nil {
		pl := pasteLine{Raw: raw, LinkName: strings.TrimSpace(m[3])}
		pl.LinkTypeID, _ = strconv.ParseInt(m[1], 10, 64)
		pl.LinkItemID, _ = strconv.ParseInt(m[2], 10, 64)
		if pl.LinkItemID > 0 {
			pl.Candidates = linkCandidates(pl.LinkName)
			return pl, true
		}
	}
	// A type link names only a type: unwrap it to its
	// display text and let the ordinary readings (quantities
	// around it included) apply.
	work = reShowinfoType.ReplaceAllString(work, "$2")
	// A trailing volume measures size, not count: drop it
	// before any quantity reading, so "Tritanium 1,200 m3"
	// is one Tritanium with a volume note, not 1,200 of them.
	work = reVolumeSuffix.ReplaceAllString(work, "")
	work, facts := stripAnnotation(work)
	work = reVolumeSuffix.ReplaceAllString(work, "")
	var pl pasteLine
	if facts != nil {
		pl = *facts
	}
	pl.Raw = raw

	name, qty, explicit := "", int64(1), false
	switch {
	case strings.Contains(work, "\t"):
		// Tab-separated copies (asset window, spreadsheet):
		// name first, then the first field that reads as a
		// quantity; the rest (group, category, size) is noise.
		fields := strings.Split(work, "\t")
		name = strings.TrimSpace(fields[0])
		for _, f := range fields[1:] {
			if reVolumeField.MatchString(strings.TrimSpace(f)) {
				continue // a size column, not a quantity
			}
			if n, ok := parseQtyToken(f); ok {
				qty, explicit = n, true
				break
			}
		}
	case reQtyPrefix.MatchString(work):
		m := reQtyPrefix.FindStringSubmatch(work)
		if n, ok := parseQtyToken(m[1]); ok {
			qty, explicit, name = n, true, strings.TrimSpace(m[2])
		}
	case reQtySuffix.MatchString(work):
		m := reQtySuffix.FindStringSubmatch(work)
		if n, ok := parseQtyToken(m[2]); ok {
			qty, explicit, name = n, true, strings.TrimSpace(m[1])
		}
	case reCommaQty.MatchString(work):
		m := reCommaQty.FindStringSubmatch(work)
		if n, ok := parseQtyToken(m[2]); ok {
			qty, explicit, name = n, true, strings.TrimSpace(m[1])
		}
	default:
		name = work
	}
	if name == "" {
		return pl, false
	}
	pl.Candidates = candidates(name, qty, explicit)
	return pl, true
}

// parsePaste parses a whole line-list paste. EFT pastes are
// detected and handled by the caller before this runs.
func parsePaste(text string) pasteResult {
	var res pasteResult
	seen := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		seen++
		if seen > maxPasteLines {
			res.Truncated = true
			break
		}
		if pl, ok := parseListLine(line); ok {
			res.Lines = append(res.Lines, pl)
		} else {
			res.Bad = append(res.Bad, line)
		}
	}
	return res
}
