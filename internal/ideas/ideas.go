// Package ideas is the stored trade-ideas answer: the model's
// markdown-shaped text parsed into strategy groups (with the model's
// text escaped, never markup), and the readers that load it from the
// database. The Trade Ideas module (internal/app/tradeideas) writes the
// answer and renders its page; core's home widget reads the same stored
// answer through this package, so neither depends on the other.
package ideas

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
)

// Row is one scanner row of the CSV a trade-ideas answer was written
// from, exactly as it was sent to the model: the 1-based row number
// the model's "(#n)" references name, and the pre-formatted figure
// strings (never re-derived numbers). The answer stores its rows
// beside its body (trade_ideas.rows), and Parse composes each idea's
// figure segment from them, so the model never transcribes — and
// never slips on — the figures it used to copy out of the CSV.
type Row struct {
	N       int    `json:"n"`
	Item    string `json:"item"`
	Station string `json:"station"`
	Buy     string `json:"buy"`
	Sell    string `json:"sell"`
	Spread  string `json:"spread"` // as sent, with its "%" (e.g. "12.3%")
	Sold    string `json:"sold"`   // sold per day, as sent
	Profit  string `json:"profit"` // estimated daily profit, as sent
}

// DecodeRows reads a stored row set. Anything unusable (an answer
// written before rows were stored, a damaged value) reads as no
// rows, and the body renders verbatim, as it always has.
func DecodeRows(raw string) []Row {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var rows []Row
	if err := json.Unmarshal([]byte(raw), &rows); err != nil || len(rows) == 0 {
		return nil
	}
	return rows
}

// EncodeRows renders a row set for storage beside an answer's body.
func EncodeRows(rows []Row) (string, error) {
	b, err := json.Marshal(rows)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

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
	w.Groups = Parse(row.Body, DecodeRows(row.Rows.String))
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
	w.Groups = Parse(row.Body, DecodeRows(row.Rows.String))
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
// strategy. An idea in the current answer shape —
// "* **Item** (#n) — reason" — is composed at parse time: the
// "(#n)" reference (never rendered) names a row of the answer's
// stored row set, and the displayed line takes its item name and
// figure segment from that row, in the exact format the model used
// to transcribe. A body with no row set (every answer written
// before rows were stored) renders verbatim, as it always has.
// Text is HTML-escaped before any marker becomes an
// element, so model text can never inject markup; leftover
// asterisks are marker noise (EVE item names contain none) and are
// dropped. Lines before the first header, or an answer with no
// headers at all, land in a group titled "Ideas"; a group with
// nothing in it is dropped.
func Parse(body string, rows []Row) []Group {
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
			content = composeIdea(content, rows)
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

// composeIdea resolves one idea bullet against the answer's stored
// row set. The current shape is "**Item** (#n) — reason": the
// reference is extracted (never rendered), and a reference that
// lands composes the display line from the ROW — its item name, not
// the model's, and its figure strings in the format the model used
// to transcribe. No row set, no reference, an unparseable reference,
// or one out of range leaves the model's own item and reason with
// no figure segment; nothing here can break a line.
func composeIdea(content string, rows []Row) string {
	if len(rows) == 0 {
		return content
	}
	n, valid, cleaned, present := splitRowRef(content)
	if !present {
		return content
	}
	head, reason := splitItemReason(cleaned)
	if valid {
		for _, r := range rows {
			if r.N == n {
				line := "**" + r.Item + "** · " + r.Station +
					" · Highest buy " + r.Buy +
					" · Lowest sell " + r.Sell +
					" · Spread " + r.Spread +
					" · " + r.Sold + " sold per day" +
					" · Estimated daily profit " + r.Profit
				if reason != "" {
					line += " — " + reason
				}
				return line
			}
		}
	}
	// The reference named nothing: the model's item and reason,
	// with no figure segment and no reference.
	if reason == "" {
		return head
	}
	return head + " — " + reason
}

// splitRowRef removes a "(#n)" reference from a bullet and reports
// what it held. present is true when a "(#...)" token was there at
// all; valid only when it was a plain row number.
func splitRowRef(content string) (n int, valid bool, cleaned string, present bool) {
	open := strings.Index(content, "(#")
	if open < 0 {
		return 0, false, content, false
	}
	end := strings.Index(content[open:], ")")
	if end < 0 {
		return 0, false, content, false
	}
	end += open
	inner := content[open+2 : end]
	cleaned = strings.TrimSpace(strings.ReplaceAll(content[:open]+" "+content[end+1:], "  ", " "))
	if v, err := strconv.Atoi(inner); err == nil && inner != "" {
		return v, true, cleaned, true
	}
	return 0, false, cleaned, true
}

// splitItemReason splits a bullet's head (the bold item, usually)
// from its reason at the first dash separator.
func splitItemReason(s string) (head, reason string) {
	for _, sep := range []string{" — ", " – ", " - "} {
		if i := strings.Index(s, sep); i >= 0 {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(sep):])
		}
	}
	return strings.TrimSpace(s), ""
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
