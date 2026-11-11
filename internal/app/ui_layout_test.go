package app

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// frameworkPageSheets are the page stylesheets whose pages are
// built on the layout framework (layout.css). A page joins the list
// when it migrates.
var frameworkPageSheets = []string{"assets.css", "overview.css", "skillplans.css", "jobs.css", "mining.css", "colonies.css", "pi.css", "blueprints.css", "contacts.css", "mail.css", "wallet.css", "kill.css", "killmails.css", "lp.css", "trends.css", "baskets.css", "markettools.css", "finances.css", "profit.css", "corp.css", "intel.css", "killboard.css", "channels.css", "scans.css", "workbench.css", "dashboard.css", "fitting.css", "map.css", "items.css", "itemsheet.css", "appraisal.css", "battles.css"}

// TestFrameworkPageSheetsMeasureTheirOwnSpace holds the layout
// framework's first rule on every migrated page: a page's own sheet
// never decides layout from the window. Window-width media queries
// break the page whenever the sidebar takes or gives back room, and
// they are how per-page layout patches crept in; a framework page
// adapts through the framework's parts and container queries on the
// space it actually has. Fixed positioning is out for the same
// reason the framework bans it inside .l-page: the page is a size
// container, so a fixed child would pin to the page, not the screen.
// TestInspectorNavBarsStayOutOfThePane pins the framework's
// inspector bar rule: a bar holding nothing but its back and
// close links stays out of the pane at wide widths, where both
// links hide -- otherwise every inspector hangs a blank sticky
// strip across its top. Bars carrying a title, search, or
// filters keep their room, and the pane keeps its top padding.
func TestInspectorNavBarsStayOutOfThePane(t *testing.T) {
	css := readStatic(t, "layout.css")
	for _, want := range []string{
		".l-pane--insp > .l-pane-bar:not(:has(> :not(.l-back, .l-close)))",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("layout.css: missing %q", want)
		}
	}
}

func TestFrameworkPageSheetsMeasureTheirOwnSpace(t *testing.T) {
	widthQuery := regexp.MustCompile(`@media[^{]*\((?:min-|max-)?(?:width|height)\b|@media[^{]*\(\s*\d[^)]*(?:width|height)`)
	for _, name := range frameworkPageSheets {
		css := readStatic(t, name)
		if m := widthQuery.FindString(css); m != "" {
			t.Errorf("%s decides layout from the window (%q); use the framework's parts or a container query", name, m)
		}
		if strings.Contains(css, "position: fixed") {
			t.Errorf("%s positions something fixed; inside .l-page use the console drawer or the top layer", name)
		}
	}
}

// TestNoHasInsideHas enforces the :has() rule every sheet must keep:
// a :has() never sits inside another :has(). Browsers reject the
// whole selector, so the rule is dropped without a word and the
// layout it was written for never applies.
func TestNoHasInsideHas(t *testing.T) {
	sheets, _ := fs.Glob(staticFS, "static/*.css")
	comment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	for _, name := range sheets {
		raw, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Comments are blanked to their newlines, so lines stay true.
		css := comment.ReplaceAllStringFunc(string(raw), func(c string) string {
			return strings.Repeat("\n", strings.Count(c, "\n"))
		})
		var open []bool // per open paren: does it belong to a :has(
		inHas := 0
		for i := 0; i < len(css); i++ {
			switch {
			case strings.HasPrefix(css[i:], ":has("):
				if inHas > 0 {
					line := strings.Count(css[:i], "\n") + 1
					t.Errorf("%s:%d: :has() nested inside :has(), which browsers drop", name, line)
				}
				open = append(open, true)
				inHas++
				i += len(":has(") - 1
			case css[i] == '(':
				open = append(open, false)
			case css[i] == ')' && len(open) > 0:
				if open[len(open)-1] {
					inHas--
				}
				open = open[:len(open)-1]
			case css[i] == '}':
				open, inHas = open[:0], 0
			}
		}
	}
}
