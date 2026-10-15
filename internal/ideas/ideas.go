// Package ideas is the stored trade-ideas answer: the model's
// markdown-shaped text parsed into strategy groups (with the model's
// text escaped, never markup), and the readers that load it from the
// database. The Trade Ideas module (internal/app/tradeideas) writes the
// answer and renders its page; core's home widget reads the same stored
// answer through this package, so neither depends on the other.
package ideas

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	db "evesynapse/internal/db/sqlc"
)

// The two models that can answer, as stored with each answer.
const (
	// GemmaModel is Cloudflare Workers AI's Gemma.
	GemmaModel = "@cf/google/gemma-4-26b-a4b-it"
	// MuseModel is Meta's Muse Wonder.
	MuseModel = "muse-wonder-1.0"
)

// ModelLabel names the model behind a stored answer for the page and
// module footers.
func ModelLabel(model string) string {
	if strings.HasPrefix(model, "muse-") {
		return "Muse Wonder"
	}
	return "Gemma"
}

// Stored reads the module's own short answer (the home module's).
// Rendered reads only, one bounded row, no fetches. configured says
// whether a model is set up, for the empty state's wording.
func Stored(ctx context.Context, q *db.Queries, configured bool) *Widget {
	w := &Widget{Configured: configured}
	row, err := q.GetTradeIdeas(ctx)
	if err != nil || strings.TrimSpace(row.Body) == "" {
		return w
	}
	w.Groups = Parse(row.Body)
	w.HasIdeas = len(w.Groups) > 0
	w.AsOf = row.AsOf.UTC().Format("Jan 2, 3:04 PM")
	w.Model = ModelLabel(row.Model)
	return w
}

// StoredPage reads the page's own, longer answer, falling back to the
// module's until the first one lands.
func StoredPage(ctx context.Context, q *db.Queries, configured bool) *Widget {
	row, err := q.GetTradeIdeasPage(ctx)
	if err != nil || strings.TrimSpace(row.Body) == "" {
		return Stored(ctx, q, configured)
	}
	w := &Widget{Configured: configured, Detailed: true}
	w.Groups = Parse(row.Body)
	w.HasIdeas = len(w.Groups) > 0
	w.AsOf = row.AsOf.UTC().Format("Jan 2, 3:04 PM")
	w.Model = ModelLabel(row.Model)
	return w
}

// Widget is the stored answer for the page and the home
// module: the ideas grouped by strategy, with their age. The model's
// text is escaped first and only strong survives as markup
// (Inline): the model's text is data, never markup.
type Widget struct {
	Groups     []Group
	AsOf       string // "Jan 2, 3:04 PM", empty until an answer lands
	HasIdeas   bool
	Model      string // the model that answered, for the footer ("Muse Wonder", "Gemma")
	Configured bool   // a model is configured (Meta key or Cloudflare credentials)
	Detailed   bool   // the page's own answer: each idea carries its figures
}

// Group is one strategy: a tab on the page and the module.
type Group struct {
	ID     string // "ti-1", ...: the tab's panel id, unique on a page
	Accent int    // 1-6, rotating by place: the group's color (tradeideas.css)
	Title  template.HTML
	Desc   template.HTML   // the one-line description, if any
	Ideas  []template.HTML // one bullet each, best first
	// Briefs parallel Ideas: the item and its first short detail, for
	// the home module at its resting size (a verbose model's full
	// line is too much there; More and the page show Ideas).
	Briefs []template.HTML
	Notes  []template.HTML // plain lines under the heading
}

// More is how many ideas a list capped at n leaves out.
func (g Group) More(n int) int {
	if len(g.Ideas) <= n {
		return 0
	}
	return len(g.Ideas) - n
}

// maxOutLines caps one render's source lines: the
// stored answer is already capped, this is the belt beside it. The
// page's answer runs to some 130 lines (six strategies of twenty).
const maxOutLines = 400

// Parse reads the stored markdown-shaped answer (###
// headers, * bullets, **bold**, *italic* lines) into one group per
// strategy. Text is HTML-escaped before any marker becomes an
// element, so model text can never inject markup; leftover
// asterisks are marker noise (EVE item names contain none) and are
// dropped. Lines before the first header, or an answer with no
// headers at all, land in a group titled "Ideas"; a group with
// nothing in it is dropped.
func Parse(body string) []Group {
	var groups []Group
	cur := -1
	ensure := func() {
		if cur < 0 {
			groups = append(groups, Group{Title: "Ideas"})
			cur = len(groups) - 1
		}
	}
	used := 0
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if used >= maxOutLines {
			break
		}
		used++
		switch content, isList := stripMarker(line); {
		case strings.HasPrefix(line, "#"):
			title := Inline(strings.TrimSpace(strings.TrimLeft(line, "#")))
			if title == "" {
				continue
			}
			groups = append(groups, Group{Title: stripStrong(title)})
			cur = len(groups) - 1
		case isList:
			if item := Inline(content); item != "" {
				ensure()
				groups[cur].Ideas = append(groups[cur].Ideas, item)
				groups[cur].Briefs = append(groups[cur].Briefs, Inline(brief(content)))
			}
		case len(line) > 2 && strings.HasPrefix(line, "*") && strings.HasSuffix(line, "*"):
			if desc := Inline(strings.Trim(line, "*")); desc != "" {
				ensure()
				if groups[cur].Desc == "" {
					groups[cur].Desc = desc
				} else {
					groups[cur].Notes = append(groups[cur].Notes, desc)
				}
			}
		default:
			if para := Inline(line); para != "" {
				ensure()
				groups[cur].Notes = append(groups[cur].Notes, para)
			}
		}
	}
	out := groups[:0]
	for _, g := range groups {
		if len(g.Ideas) == 0 && len(g.Notes) == 0 {
			continue
		}
		g.ID = fmt.Sprintf("ti-%d", len(out)+1)
		g.Accent = len(out)%6 + 1
		out = append(out, g)
	}
	return out
}

// briefMax caps a brief's detail, in runes.
const briefMax = 44

// brief shortens one idea line (markdown) for the compact
// module: the bolded item, then the first clause of what follows it
// (up to the first " · ", " — ", "; " or ". "), cut at a word near
// briefMax with an ellipsis. A line with no bold item keeps
// its own first clause.
func brief(line string) string {
	head, rest := "", line
	if strings.HasPrefix(line, "**") {
		if end := strings.Index(line[2:], "**"); end >= 0 {
			head = line[:end+4]
			rest = line[end+4:]
		}
	}
	rest = strings.TrimLeft(rest, " :·—–-,")
	for _, sep := range []string{" · ", " — ", " – ", "; ", ". "} {
		if i := strings.Index(rest, sep); i >= 0 {
			rest = rest[:i]
		}
	}
	rest = strings.TrimSpace(rest)
	if r := []rune(rest); len(r) > briefMax {
		cut := string(r[:briefMax])
		if sp := strings.LastIndex(cut, " "); sp > briefMax/2 {
			cut = cut[:sp]
		}
		rest = strings.TrimRight(cut, " ,;:") + "…"
	}
	switch {
	case head == "":
		return rest
	case rest == "":
		return head
	}
	return head + " — " + rest
}

// stripStrong drops strong tags from a heading the model bolded:
// a tab label is plain text.
func stripStrong(h template.HTML) template.HTML {
	return template.HTML(strings.NewReplacer("<strong>", "", "</strong>", "").Replace(string(h)))
}

// stripMarker removes one leading bullet marker ("* ",
// "- ", "N. ") and reports whether the line is a list item.
func stripMarker(line string) (string, bool) {
	if strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "- ") {
		return strings.TrimSpace(line[2:]), true
	}
	if dot := strings.Index(line, ". "); dot > 0 && dot < 5 {
		numbered := true
		for i := 0; i < dot; i++ {
			if line[i] < '0' || line[i] > '9' {
				numbered = false
				break
			}
		}
		if numbered {
			return strings.TrimSpace(line[dot+2:]), true
		}
	}
	return line, false
}

// Inline escapes text, turns **pairs** into strong,
// and drops leftover asterisks.
func Inline(text string) template.HTML {
	esc := template.HTMLEscapeString(text)
	var out strings.Builder
	for {
		a := strings.Index(esc, "**")
		if a < 0 {
			break
		}
		b := strings.Index(esc[a+2:], "**")
		if b < 0 {
			break
		}
		b += a + 2
		out.WriteString(esc[:a])
		out.WriteString("<strong>" + esc[a+2:b] + "</strong>")
		esc = esc[b+2:]
	}
	out.WriteString(esc)
	return template.HTML(strings.ReplaceAll(out.String(), "*", ""))
}
