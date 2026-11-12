package app

import (
	"math/bits"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Typo tolerance for the static data searches (items, systems,
// regions): every search box that looks for those. A query that
// finds nothing is read again with each misspelled word replaced by
// the closest word the game's names use ("damge contrl" reads as
// "damage control", "hurican" as "hurricane"). Pilot, corporation
// and alliance names are never corrected: they are not words.
//
// The vocabulary is built with the in-memory search snapshot, so a
// correction reads memory only, and it runs only when the exact
// search came back empty: a search that finds something costs
// nothing extra. Without a snapshot (low-RAM mode, or before the
// first build lands) queries are not corrected.
// ---------------------------------------------------------------------------

// searchVocab is every word of the published type, system and
// region names (three letters or longer), with how many names use
// it, bucketed by length for the distance scan.
type searchVocab struct {
	uses  map[string]int
	byLen map[int][]string
	// masks[n][i] is which letters and digits byLen[n][i] uses, a
	// bit each: one edit changes at most two bits, so a word whose
	// mask differs in more than twice the allowed edits is skipped
	// without measuring the distance.
	masks map[int][]uint64
	// joined is every word between NUL separators: one substring
	// search over it says whether a text is part of a known word.
	joined string
}

// minFixLen is the shortest word a correction is tried for: shorter
// words have too many near neighbours to guess well.
const minFixLen = 4

func buildSearchVocab(snap *searchSnapshot) *searchVocab {
	v := &searchVocab{uses: map[string]int{}, byLen: map[int][]string{}, masks: map[int][]uint64{}}
	add := func(lower string) {
		for _, w := range searchWordsOf(lower) {
			if len(w) >= 3 {
				v.uses[w]++
			}
		}
	}
	for _, e := range snap.types {
		add(e.lower)
	}
	for _, e := range snap.systems {
		add(e.lower)
	}
	for _, e := range snap.regions {
		add(e.lower)
	}
	all := make([]string, 0, len(v.uses))
	for w := range v.uses {
		v.byLen[len(w)] = append(v.byLen[len(w)], w)
		all = append(all, w)
	}
	for n, ws := range v.byLen {
		sort.Strings(ws)
		ms := make([]uint64, len(ws))
		for i, w := range ws {
			ms[i] = letterMask(w)
		}
		v.masks[n] = ms
	}
	sort.Strings(all)
	v.joined = "\x00" + strings.Join(all, "\x00") + "\x00"
	return v
}

// searchWordsOf splits lowered text into its runs of letters and
// digits.
func searchWordsOf(lower string) []string {
	return strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// correct is q, lowered, with each misspelled word replaced by its
// closest vocabulary word; ok is false when no word changed.
func (v *searchVocab) correct(q string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(q))
	var b strings.Builder
	changed := false
	start := -1
	flush := func(end int) {
		w := lower[start:end]
		if fix := v.fix(w); fix != "" {
			b.WriteString(fix)
			changed = true
		} else {
			b.WriteString(w)
		}
		start = -1
	}
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		word := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		switch {
		case word && start < 0:
			start = i
		case !word && start >= 0:
			flush(i)
			b.WriteByte(c)
		case !word:
			b.WriteByte(c)
		}
	}
	if start >= 0 {
		flush(len(lower))
	}
	if !changed {
		return q, false
	}
	return b.String(), true
}

// fix is the vocabulary word w most likely means, "" when w needs no
// fix or nothing is close enough. A word that is part of a known
// word is left alone: the substring search already matches it, and
// it is often a word still being typed.
func (v *searchVocab) fix(w string) string {
	if len(w) < minFixLen || allDigits(w) {
		return ""
	}
	if _, ok := v.uses[w]; ok {
		return ""
	}
	limit := 1
	switch {
	case len(w) >= 10:
		limit = 3
	case len(w) >= 6:
		limit = 2
	}
	if strings.Contains(v.joined, w) {
		return ""
	}
	best, bestD, bestUses := "", limit+1, 0
	var rows editRows
	wm := letterMask(w)
	for n := len(w) - limit; n <= len(w)+limit; n++ {
		ms := v.masks[n]
		for i, x := range v.byLen[n] {
			if bits.OnesCount64(ms[i]^wm) > 2*limit {
				continue
			}
			d := rows.distance(w, x, limit)
			if d < bestD || (d == bestD && v.uses[x] > bestUses) {
				best, bestD, bestUses = x, d, v.uses[x]
			}
		}
	}
	return best
}

// letterMask sets a bit for each letter and digit s uses.
func letterMask(s string) uint64 {
	var m uint64
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z':
			m |= 1 << (c - 'a')
		case c >= '0' && c <= '9':
			m |= 1 << (26 + c - '0')
		}
	}
	return m
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// editRows holds the three rows of the distance table, reused
// across one correction's comparisons so the scan allocates once.
type editRows struct{ prev2, prev, cur []int }

// distance counts the single-letter inserts, deletes, changes and
// swaps of neighbours that turn a into b, stopping early at limit+1
// once every path is past the limit.
func (r *editRows) distance(a, b string, limit int) int {
	if a == b {
		return 0
	}
	n := len(b) + 1
	if cap(r.cur) < n {
		r.prev2, r.prev, r.cur = make([]int, n), make([]int, n), make([]int, n)
	}
	prev2, prev, cur := r.prev2[:n], r.prev[:n], r.cur[:n]
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j < n; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d = min(d, prev2[j-2]+1)
			}
			cur[j] = d
			rowMin = min(rowMin, d)
		}
		if rowMin > limit {
			return limit + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(b)]
}

// correctSearch is q read with its misspellings fixed against the
// static data names, for a search that found nothing as typed; ok
// is false when there is no snapshot or nothing to fix.
func (app *Application) correctSearch(q string) (string, bool) {
	snap := app.searchSnapshot()
	if snap == nil || snap.vocab == nil {
		return q, false
	}
	return snap.vocab.correct(q)
}

// CorrectSearch is correctSearch for the feature modules.
func (app *Application) CorrectSearch(q string) (string, bool) {
	return app.correctSearch(q)
}
