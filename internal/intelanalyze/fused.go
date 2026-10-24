package intelanalyze

// ---------------------------------------------------------------------------
// Reading a roster whose separators died on the way in.
//
// A local list pasted into the game's chat is sent by the client as
// one message with the lines fused: "AltorrAstria Starbourne" --
// each line's text run straight into the next, the spaces inside a
// name kept, the ones between names gone (and, in a long paste,
// present only sometimes). Read as prose that text is one long
// unknown word run and the analysis gives up on it, though every
// name in it is a pilot the site may already know.
//
// NameIndex segments such a text against the known pilot names:
// a deterministic word-break (dynamic programming over a trie of
// the names) that prefers the reading covering the most of the
// text with real names, longest names first at a tie. It is the
// same longest-match rule the spaced reading follows -- a consumed
// span yields no fragment pilots inside it ("Citizen" is not read
// out of "Caldari Citizen 92702902" once the full name matches).
//
// FusedCandidates proposes the names a fused text might hold, for
// the caller to verify through its usual bulk lookup: the text is
// split at its case boundaries (a fusion joins where one name's
// last letter meets the next name's first, and the join shows in
// the casing) and the pieces are windowed back together the way
// names run -- up to four pieces, spaces where the paste kept
// them.
// ---------------------------------------------------------------------------

import (
	"strings"
	"unicode"
)

// The bar a segmentation must clear to re-read a report as a
// roster: at least FusedMinPilots pilots, covering at least
// FusedMinCoverPct of the text's letters and digits. Anything
// less is left exactly as the plain analysis read it.
const (
	FusedMinPilots   = 3
	FusedMinCoverPct = 70
)

// PassesRosterBar is the bar above, for a Segment result.
func PassesRosterBar(pilots []Pilot, covered, total int) bool {
	return len(pilots) >= FusedMinPilots && total > 0 && covered*100 >= total*FusedMinCoverPct
}

// NameIndex is a trie of known pilot names, lower-cased, for
// segmenting fused text. Build it with NewNameIndex.
type NameIndex struct {
	root *nameTrieNode
}

type nameTrieNode struct {
	next  map[rune]*nameTrieNode
	pilot *Pilot // set where a name ends
}

// NewNameIndex builds the index from pilots (their proper names).
// A name of fewer than two letters or digits cannot anchor a
// segmentation and is left out.
func NewNameIndex(pilots []Pilot) *NameIndex {
	ix := &NameIndex{root: &nameTrieNode{}}
	for _, p := range pilots {
		name := []rune(strings.ToLower(p.Name))
		alnum := 0
		for _, r := range name {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				alnum++
			}
		}
		if alnum < 2 {
			continue
		}
		node := ix.root
		for _, r := range name {
			if node.next == nil {
				node.next = map[rune]*nameTrieNode{}
			}
			n := node.next[r]
			if n == nil {
				n = &nameTrieNode{}
				node.next[r] = n
			}
			node = n
		}
		p := p
		node.pilot = &p
	}
	return ix
}

// maxNameRunes bounds a trie walk from one position: a character
// name runs to 37 characters, and a match is never longer.
const maxNameRunes = 40

// Segment word-breaks text against the index: the reading that
// covers the most of the text's letters and digits with pilot
// names (fewest pilots on a tie -- the longest names). It returns
// the pilots in text order (each once, however often the text
// names them), and the coverage: covered of total letters and
// digits. Spaces in the text may fall inside a name (its own
// spaces) or between names; a rune no name covers is skipped as
// residue.
func (ix *NameIndex) Segment(text string) (pilots []Pilot, covered, total int) {
	runes := []rune(text)
	n := len(runes)
	if n == 0 || ix == nil {
		return nil, 0, 0
	}
	lower := make([]rune, n)
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			total++
		}
	}
	// Best reading of the prefix ending at each position: the
	// coverage, the pilot count, and how it got there.
	cov := make([]int, n+1)
	np := make([]int, n+1)
	from := make([]int, n+1)
	via := make([]*Pilot, n+1)
	for i := range cov {
		cov[i] = -1
	}
	cov[0] = 0
	relax := func(j, c, p, f int, v *Pilot) {
		if c > cov[j] || (c == cov[j] && p < np[j]) {
			cov[j], np[j], from[j], via[j] = c, p, f, v
		}
	}
	for i := 0; i < n; i++ {
		if cov[i] < 0 {
			continue
		}
		// A pilot name starting here.
		node := ix.root
		alnum := 0
		for j := i; j < n && j-i < maxNameRunes; j++ {
			node = node.next[lower[j]]
			if node == nil {
				break
			}
			if unicode.IsLetter(lower[j]) || unicode.IsDigit(lower[j]) {
				alnum++
			}
			if node.pilot != nil {
				relax(j+1, cov[i]+alnum, np[i]+1, i, node.pilot)
			}
		}
		// Or one rune of residue (a space between names, or text
		// no name covers).
		relax(i+1, cov[i], np[i], i, nil)
	}
	if cov[n] < 0 {
		return nil, 0, total
	}
	covered = cov[n]
	seen := map[int64]bool{}
	var rev []Pilot
	for at := n; at > 0; at = from[at] {
		if via[at] != nil && !seen[via[at].ID] {
			seen[via[at].ID] = true
			rev = append(rev, *via[at])
		}
	}
	pilots = make([]Pilot, len(rev))
	for i, p := range rev {
		pilots[len(rev)-1-i] = p
	}
	return pilots, covered, total
}

// FusedCandidates proposes pilot names a fused text might hold,
// lower-cased as the analysis's Unknown list carries them: the
// text split at its case boundaries, windowed one to four pieces
// at a time, pieces from one pasted word joined tight and words
// joined with a space -- the shapes the roster's names take.
// Whole words are candidates too. The caller verifies them; a
// candidate that is no pilot costs one bulk lookup and settles.
func FusedCandidates(text string) []string {
	type piece struct {
		s     string
		field int
	}
	var pieces []piece
	for fi, f := range strings.Fields(text) {
		r := []rune(f)
		start := 0
		for k := 1; k < len(r); k++ {
			prev, cur := r[k-1], r[k]
			boundary := unicode.IsUpper(cur) &&
				(unicode.IsLower(prev) || unicode.IsDigit(prev) ||
					(unicode.IsUpper(prev) && k+1 < len(r) && unicode.IsLower(r[k+1])))
			if boundary {
				pieces = append(pieces, piece{string(r[start:k]), fi})
				start = k
			}
		}
		pieces = append(pieces, piece{string(r[start:]), fi})
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = clean(s)
		if s == "" || len([]rune(s)) > 37 || !pilotCandidate(s) || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for i := range pieces {
		var b strings.Builder
		for j := i; j < len(pieces) && j < i+4; j++ {
			if j > i {
				if pieces[j].field == pieces[j-1].field {
					// One pasted word: its pieces join tight.
				} else {
					b.WriteByte(' ')
				}
			}
			b.WriteString(pieces[j].s)
			add(b.String())
		}
	}
	if len(out) > 400 {
		out = out[:400]
	}
	return out
}
