// Package inteltag is the intel classifier: a small sequence tagger
// that labels each word of an intel report as the system, a gate, a
// pilot, a ship, a count, a celestial, an activity, a state, or
// nothing. It is an averaged structured perceptron over hand-made
// features (the word, its shape, its link star, its neighbours, and
// which of EVE's name lists a run of words starting or covering it
// is in), decoded with Viterbi over begin/inside/outside labels. It
// is pure Go and stdlib-only, so the site and Axon can both run it,
// and it trains in minutes on the synthetic reports
// internal/intelsynth makes.
package inteltag

import (
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"evesynapse/internal/intelanalyze"
)

// Lexicon says which kinds of EVE name a lower-cased phrase is:
// "system", "ship", "area" (a region or constellation). The site
// answers from its SDE dictionaries and its verified pilot names
// ("pilot"), training from the universe the
// examples were made from.
type Lexicon interface {
	Kinds(lower string) []string
}

// Token is one word of a report, in bytes.
type Token struct {
	Start, End int
	Text       string
}

// Tokenize splits a report into words at spaces, and peels
// brackets and trailing punctuation off as their own tokens; a
// link's star stays on its word, as a feature.
func Tokenize(text string) []Token {
	var out []Token
	i := 0
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		j := i
		for j < len(text) {
			r, size := utf8.DecodeRuneInString(text[j:])
			if unicode.IsSpace(r) {
				break
			}
			j += size
		}
		s, e := i, j
		for s < e && strings.ContainsRune("([", rune(text[s])) {
			out = append(out, Token{s, s + 1, text[s : s+1]})
			s++
		}
		var tail []Token
		for e > s && strings.ContainsRune(",;:!?)]", rune(text[e-1])) {
			tail = append([]Token{{e - 1, e, text[e-1 : e]}}, tail...)
			e--
		}
		if e > s {
			out = append(out, Token{s, e, text[s:e]})
		}
		out = append(out, tail...)
		i = j
	}
	return out
}

// celestRe is a celestial in shorthand: "p3m2", "p3", "m2", "4-4".
var celestRe = regexp.MustCompile(`^(p\d{1,2}(m\d{1,2})?|m\d{1,2}|\d-\d{1,2})$`)

func cleanTok(s string) string {
	return strings.ToLower(strings.Trim(s, ",.;:*!()[]?\"'"))
}

// shape is a word's look: "Xx", "XXd-X", "x", collapsed runs.
func shape(s string) string {
	var b strings.Builder
	var last rune
	for _, r := range s {
		c := r
		switch {
		case unicode.IsUpper(r):
			c = 'X'
		case unicode.IsLower(r):
			c = 'x'
		case unicode.IsDigit(r):
			c = 'd'
		}
		if c != last {
			b.WriteRune(c)
			last = c
		}
	}
	return b.String()
}

// Features are each token's feature names.
func Features(toks []Token, lex Lexicon) [][]string {
	n := len(toks)
	words := make([]string, n)
	for i, t := range toks {
		words[i] = cleanTok(t.Text)
	}
	out := make([][]string, n)
	at := func(i int) string {
		if i < 0 {
			return "<s>"
		}
		if i >= n {
			return "</s>"
		}
		return words[i]
	}
	for i, t := range toks {
		w := words[i]
		f := []string{"b", "w=" + w, "sh=" + shape(strings.TrimRight(t.Text, "*")),
			"p-1=" + at(i-1), "p+1=" + at(i+1), "p-2=" + at(i-2), "p+2=" + at(i+2),
			"p-1w=" + at(i-1) + "|" + w, "w+1=" + w + "|" + at(i+1)}
		if len(w) >= 3 {
			f = append(f, "pre="+w[:3], "suf="+w[len(w)-3:])
		}
		if strings.HasSuffix(t.Text, "*") {
			f = append(f, "star")
		}
		if celestRe.MatchString(w) {
			f = append(f, "celest")
		}
		if !strings.ContainsFunc(w, unicode.IsLetter) {
			f = append(f, "noletter")
		}
		if i > 0 && strings.HasSuffix(toks[i-1].Text, "*") {
			f = append(f, "prevstar")
		}
		if i == 0 {
			f = append(f, "first")
		}
		if i == n-1 {
			f = append(f, "last")
		}
		if intelanalyze.NameLike(t.Text) {
			f = append(f, "namelike")
		} else {
			f = append(f, "plainword")
		}
		// The gap before: two spaces part names dragged from Local.
		if i > 0 && t.Start-toks[i-1].End >= 2 {
			f = append(f, "gap2")
		}
		if i+1 < n && toks[i+1].Start-t.End >= 2 {
			f = append(f, "gap2after")
		}
		out[i] = f
	}
	// Lexicon runs: a run of up to four words that is a known
	// name marks its first word B and the rest I, with its kind.
	if lex != nil {
		for i := 0; i < n; i++ {
			for m := 1; m <= 4 && i+m <= n; m++ {
				parts := make([]string, m)
				for k := 0; k < m; k++ {
					parts[k] = words[i+k]
				}
				phrase := strings.Join(parts, " ")
				if phrase == "" {
					continue
				}
				for _, kind := range lex.Kinds(phrase) {
					out[i] = append(out[i], "lex="+kind+":B", "lex="+kind+":B"+itoa(m))
					for k := 1; k < m; k++ {
						out[i+k] = append(out[i+k], "lex="+kind+":I")
					}
				}
			}
		}
	}
	return out
}

func itoa(n int) string { return string(rune('0' + n)) }

// Model is a trained tagger.
type Model struct {
	Labels []string             // "O", then "B-x", "I-x" for each kind
	W      map[string][]float32 // feature -> score per label
	Trans  [][]float32          // [previous label, or len(Labels) for the start][label]
	Lex    Lexicon              // set by the caller before use; not saved
}

// Kinds are the span labels the tagger reads, as intelsynth labels them.
var Kinds = []string{"system", "gate", "dest", "area", "location", "pilot", "ship", "count", "activity", "state"}

// NewModel makes an empty model over Kinds.
func NewModel() *Model {
	m := &Model{Labels: []string{"O"}, W: map[string][]float32{}}
	for _, k := range Kinds {
		m.Labels = append(m.Labels, "B-"+k, "I-"+k)
	}
	L := len(m.Labels)
	m.Trans = make([][]float32, L+1)
	for i := range m.Trans {
		m.Trans[i] = make([]float32, L)
	}
	return m
}

// allowed is whether label to may follow label from (len(Labels)
// for the start): an inside label only continues its own kind.
func (m *Model) allowed(from, to int) bool {
	lt := m.Labels[to]
	if !strings.HasPrefix(lt, "I-") {
		return true
	}
	if from == len(m.Labels) {
		return false
	}
	lf := m.Labels[from]
	return lf != "O" && lf[2:] == lt[2:]
}

// Decode is the best label sequence for the tokens' features.
func (m *Model) Decode(feats [][]string) []int {
	n, L := len(feats), len(m.Labels)
	if n == 0 {
		return nil
	}
	emit := make([][]float64, n)
	for t, fs := range feats {
		e := make([]float64, L)
		for _, f := range fs {
			if w, ok := m.W[f]; ok {
				for l := range e {
					e[l] += float64(w[l])
				}
			}
		}
		emit[t] = e
	}
	return m.DecodeScores(emit)
}

// DecodeScores is the best label sequence for per-token label
// scores, under the model's transitions.
func (m *Model) DecodeScores(emit [][]float64) []int {
	n, L := len(emit), len(m.Labels)
	if n == 0 {
		return nil
	}
	score := make([][]float64, n)
	back := make([][]int, n)
	for t := range score {
		score[t] = make([]float64, L)
		back[t] = make([]int, L)
	}
	for l := 0; l < L; l++ {
		score[0][l] = math.Inf(-1)
		if m.allowed(L, l) {
			score[0][l] = emit[0][l] + float64(m.Trans[L][l])
		}
	}
	for t := 1; t < n; t++ {
		for l := 0; l < L; l++ {
			best, arg := math.Inf(-1), 0
			for p := 0; p < L; p++ {
				if !m.allowed(p, l) {
					continue
				}
				if s := score[t-1][p] + float64(m.Trans[p][l]); s > best {
					best, arg = s, p
				}
			}
			score[t][l], back[t][l] = best+emit[t][l], arg
		}
	}
	path := make([]int, n)
	best := math.Inf(-1)
	for l := 0; l < L; l++ {
		if score[n-1][l] > best {
			best, path[n-1] = score[n-1][l], l
		}
	}
	for t := n - 1; t > 0; t-- {
		path[t-1] = back[t][path[t]]
	}
	return path
}

// Span is one tagged stretch of a report, in bytes.
type Span struct {
	Start, End int
	Kind       string
}

// Tag labels a report's text.
func (m *Model) Tag(text string) []Span {
	toks := Tokenize(text)
	return m.Spans(toks, m.Decode(Features(toks, m.Lex)))
}

// Spans turns a label sequence back into spans. A name dragged in
// as a link ends before the client's star.
func (m *Model) Spans(toks []Token, path []int) []Span {
	var out []Span
	for t, l := range path {
		lab := m.Labels[l]
		end := toks[t].End
		if strings.HasSuffix(toks[t].Text, "*") {
			end--
		}
		switch {
		case strings.HasPrefix(lab, "B-"):
			out = append(out, Span{toks[t].Start, end, lab[2:]})
		case strings.HasPrefix(lab, "I-") && len(out) > 0:
			out[len(out)-1].End = end
		}
	}
	return out
}
