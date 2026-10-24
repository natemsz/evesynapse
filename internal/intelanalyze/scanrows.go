package intelanalyze

// ---------------------------------------------------------------------------
// Reading scan-shaped text whose separators died on the way in.
//
// A d-scan pasted into the game's chat, or relayed by a tool,
// arrives with its tabs gone: the rows fuse into one run of prose,
// "17715 fb Gila -35825 AI's Logistics Agency HQ Raitaru 1,048,258
// km35825 ..." -- each row's type ID fused onto the tail of the
// row before it. The prose reading has no column model, so the
// name column -- player-set ship and structure names, which say
// nothing about who flies what -- falls through to the pilot pass
// and mints fragment pilots out of it ("AI's", "Perch", "free").
//
// This pass reads the rows first, with the same dictionaries the
// prose reading uses, and blanks what it consumes, so the prose
// reading only ever sees what was never a row. A d-scan row is
// strict: its leading number must be the very type ID of the ship
// or type that follows its name column, and a distance (or the
// end of the text, where a paste was cut) must close it. The name
// column of such a row is set aside whole: no pilot is ever read
// out of it, and none of its words are left for the residue.
//
// An overview paste fused the same way has no type IDs; its rows
// are told by their tails (a distance, then the velocity, radial
// and transversal figures, bracketed tickers riding along). An
// overview's name column names pilots, so only the tail is
// consumed and the name is left for the pilot pass.
//
// A structure name with no type column left ("Teonusude -
// Eagle's Perch", running to a distance) is consumed as one
// location, the way markLocation consumes a station's name, with
// the same guard: a span a known pilot stands in is not a place.
// ---------------------------------------------------------------------------

import (
	"strconv"
	"strings"
)

// scanTok is one whitespace-delimited piece of the text, with
// the byte range it stands in, so consumed pieces can be
// blanked where they stand.
type scanTok struct {
	s      string
	lo, hi int
}

// scanTokens splits text into its pieces. A fused row tail --
// a distance unit or the "-" of an empty distance with the next
// row's type ID run onto it ("km35825", "-22544") -- is split
// into its two pieces, which share the chunk's range.
func scanTokens(text string) []scanTok {
	var toks []scanTok
	for i := 0; i < len(text); {
		if text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] != ' ' && text[j] != '\t' && text[j] != '\n' && text[j] != '\r' {
			j++
		}
		chunk := text[i:j]
		if head, digits, ok := fusedTail(chunk); ok {
			mid := i + len(head)
			toks = append(toks, scanTok{head, i, mid}, scanTok{digits, mid, j})
		} else {
			toks = append(toks, scanTok{chunk, i, j})
		}
		i = j
	}
	return toks
}

// fusedTail splits a chunk that is a distance tail with a type
// ID run onto it: "km35825" into "km" and "35825", "-22544"
// into "-" and "22544".
func fusedTail(chunk string) (head, digits string, ok bool) {
	i := len(chunk)
	for i > 0 && chunk[i-1] >= '0' && chunk[i-1] <= '9' {
		i--
	}
	if i == 0 || i == len(chunk) {
		return "", "", false
	}
	head, digits = chunk[:i], chunk[i:]
	if len(digits) < 4 || len(digits) > 7 {
		return "", "", false
	}
	switch strings.ToLower(head) {
	case "km", "m", "au", "-":
		return head, digits, true
	}
	return "", "", false
}

// typeIDTok reads a token as a d-scan row's leading type ID:
// plain digits, four to seven of them (a distance's thousands
// separators keep it out: "1,048" is no row's head).
func typeIDTok(s string) (int64, bool) {
	s = clean(s)
	if len(s) < 4 || len(s) > 7 {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func isNumTok(s string) bool {
	s = strings.ReplaceAll(clean(s), ",", "")
	if s == "" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func isUnitTok(s string) bool {
	switch clean(s) {
	case "km", "m", "au":
		return true
	}
	return false
}

// distEnd reads a distance at toks[q]: "-" alone, or a number
// and its unit. It returns where the distance ends.
func distEnd(toks []scanTok, q int) (int, bool) {
	if q >= len(toks) {
		return q, false
	}
	if clean(toks[q].s) == "-" {
		return q + 1, true
	}
	if isNumTok(toks[q].s) && q+1 < len(toks) && isUnitTok(toks[q+1].s) {
		return q + 2, true
	}
	return q, false
}

// rowType tries a token span as a row's type column: a ship by
// name, else any inventory type, longest phrases first, as the
// prose reading tries them -- plus the phrase as it stands,
// since a type's own name may end in a parenthesis the prose
// cleaning trims away ("Stargate (Minmatar System)").
func rowType(d Dict, toks []scanTok, i, n int) (Ship, bool) {
	joined := ""
	for k := 0; k < n; k++ {
		if k > 0 {
			joined += " "
		}
		joined += toks[i+k].s
	}
	seen := map[string]bool{}
	for _, cand := range []string{clean(joined), strings.ToLower(joined)} {
		for _, c := range []string{cand, strings.TrimSuffix(cand, "s")} {
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			if id, name, class, ok := lookupShip(d, c); ok {
				return Ship{TypeID: id, Name: name, Class: class}, true
			}
			if id, name, ok := lookupInvType(d, c); ok {
				return Ship{TypeID: id, Name: name}, true
			}
		}
	}
	return Ship{}, false
}

// maxNameToks bounds a d-scan row's name column, in tokens: a
// player-set name runs to 37 characters, a structure's
// "<system> - <name>" a few words past that.
const maxNameToks = 12

// dscanRowAt reads the d-scan row whose type ID stands at
// toks[i]. The name column runs from there to the type column:
// the first span that resolves to the row's own type, with a
// distance (or the text's end, where the paste was cut) after
// it. A span crossed by an em dash is two fragments the paste
// joined, not one name, and ends the search.
func dscanRowAt(toks []scanTok, i int, d Dict) (Ship, int, bool) {
	id, ok := typeIDTok(toks[i].s)
	if !ok {
		return Ship{}, 0, false
	}
	last := min(len(toks), i+1+maxNameToks)
	for p := i + 1; p <= last; p++ {
		if p > i+1 {
			if c := clean(toks[p-1].s); c == "—" || c == "–" {
				break
			}
		}
		if p == len(toks) {
			break
		}
		for n := 3; n >= 1; n-- {
			if p+n > len(toks) {
				continue
			}
			ship, ok := rowType(d, toks, p, n)
			if !ok || ship.TypeID != id {
				continue
			}
			q := p + n
			if q == len(toks) {
				return ship, q, true // the paste was cut mid-row
			}
			if end, ok := distEnd(toks, q); ok {
				return ship, end, true
			}
		}
	}
	return Ship{}, 0, false
}

// overviewRowAt reads the overview row whose type stands at
// toks[i]: a distance after it, then at least two of the
// velocity figures an overview row carries (a number, with its
// unit when the paste kept it -- a bare figure counts only
// when another figure follows it). It returns where the row's
// tail ends; the name column before toks[i] is not the row's
// to consume.
func overviewRowAt(toks []scanTok, i int, d Dict) (Ship, int, bool) {
	for n := 3; n >= 1; n-- {
		if i+n > len(toks) {
			continue
		}
		ship, ok := rowType(d, toks, i, n)
		if !ok {
			continue
		}
		end, ok := distEnd(toks, i+n)
		if !ok {
			continue
		}
		groups := 0
		for end < len(toks) {
			if !isNumTok(toks[end].s) {
				break
			}
			next := end + 1
			if next < len(toks) {
				if u := clean(toks[next].s); u == "m/s" || u == "rad/s" {
					next++
				} else if !isNumTok(toks[next].s) {
					break // a lone figure with no unit and no figure after it
				}
			}
			groups++
			end = next
		}
		if groups < 2 {
			continue
		}
		// Bracketed tickers riding on the row's tail.
		for end < len(toks) && isTickerTok(toks[end].s) {
			end++
		}
		return ship, end, true
	}
	return Ship{}, 0, false
}

func isTickerTok(s string) bool {
	return len(s) >= 2 && strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")
}

// locationSpanAt reads a structure name running to a distance
// with no type column left in it: "<system> - <name>", the words
// from toks[i] to a distance, consumed as one location. The
// span must open on a capitalised word, hold its dash, run to
// at most maxNameToks tokens, and hold no known pilot: a span
// a pilot stands in is a report about them, not a place. A
// type at the span's tail is the structure's own, and is read
// as one.
func locationSpanAt(toks []scanTok, used []bool, i int, d Dict) (Ship, int, bool) {
	if used[i] || toks[i].s == "" || toks[i].s[0] < 'A' || toks[i].s[0] > 'Z' {
		return Ship{}, 0, false
	}
	dash := false
	last := min(len(toks), i+maxNameToks)
	for j := i; j < last; j++ {
		if used[j] {
			return Ship{}, 0, false
		}
		if c := clean(toks[j].s); c == "—" || c == "–" {
			return Ship{}, 0, false
		}
		if clean(toks[j].s) == "-" {
			dash = true
			continue
		}
		end, isDist := distEnd(toks, j)
		if !isDist {
			continue
		}
		// A distance found: the span is toks[i:j], closed by it.
		if !dash || j-i < 3 {
			return Ship{}, 0, false
		}
		for k := i; k < j; k++ {
			for n := 1; n <= 3 && k+n <= j; n++ {
				words := make([]string, n)
				for m := 0; m < n; m++ {
					words[m] = toks[k+m].s
				}
				if _, _, ok, _ := lookupPilot(d, clean(strings.Join(words, " "))); ok {
					return Ship{}, 0, false
				}
			}
		}
		// A type standing at the span's tail, just before the
		// distance, is the structure's type column: read it.
		for n := 3; n >= 1; n-- {
			if j-n < i {
				continue
			}
			if ship, ok := rowType(d, toks, j-n, n); ok {
				return ship, end, true
			}
		}
		return Ship{}, end, true
	}
	return Ship{}, 0, false
}

// consumeScanText reads the scan-shaped rows out of text with
// d's dictionaries, adds their ships through addShip, and
// returns the text with every consumed span blanked, so the
// prose reading never sees a name column.
func consumeScanText(text string, d Dict, addShip func(Ship, int)) string {
	toks := scanTokens(text)
	if len(toks) == 0 {
		return text
	}
	used := make([]bool, len(toks))
	blank := func(from, to int) {
		for k := from; k < to; k++ {
			used[k] = true
		}
	}
	// D-scan rows: type ID, name column, type, distance.
	for i := 0; i < len(toks); {
		if used[i] {
			i++
			continue
		}
		if ship, end, ok := dscanRowAt(toks, i, d); ok {
			addShip(ship, 0)
			blank(i, end)
			i = end
			continue
		}
		i++
	}
	// Overview rows: the tail only; the name column names
	// pilots there and is left standing. One overview row
	// makes the text overview-shaped, and its bracketed
	// tickers are consumed wherever they ride.
	overview := false
	for i := 0; i < len(toks); {
		if used[i] {
			i++
			continue
		}
		if ship, end, ok := overviewRowAt(toks, i, d); ok {
			addShip(ship, 0)
			blank(i, end)
			overview = true
			i = end
			continue
		}
		i++
	}
	if overview {
		for i := range toks {
			if !used[i] && isTickerTok(toks[i].s) {
				used[i] = true
			}
		}
	}
	// Structure names with no type column left.
	for i := 0; i < len(toks); {
		if used[i] {
			i++
			continue
		}
		if ship, end, ok := locationSpanAt(toks, used, i, d); ok {
			if ship.TypeID != 0 {
				addShip(ship, 0)
			}
			blank(i, end)
			i = end
			continue
		}
		i++
	}
	out := []byte(text)
	for i, tk := range toks {
		if used[i] {
			for k := tk.lo; k < tk.hi; k++ {
				out[k] = ' '
			}
		}
	}
	return string(out)
}
