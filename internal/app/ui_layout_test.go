package app

import (
	"regexp"
	"strings"
	"testing"
)

// frameworkPageSheets are the page stylesheets whose pages are
// built on the layout framework (layout.css). A page joins the list
// when it migrates.
var frameworkPageSheets = []string{"assets.css", "overview.css", "skillplans.css", "jobs.css", "mining.css", "colonies.css", "blueprints.css", "contacts.css", "mail.css", "lp.css", "trends.css", "baskets.css"}

// TestFrameworkPageSheetsMeasureTheirOwnSpace holds the layout
// framework's first rule on every migrated page: a page's own sheet
// never decides layout from the window. Window-width media queries
// break the page whenever the sidebar takes or gives back room, and
// they are how per-page layout patches crept in; a framework page
// adapts through the framework's parts and container queries on the
// space it actually has. Fixed positioning is out for the same
// reason the framework bans it inside .l-page: the page is a size
// container, so a fixed child would pin to the page, not the screen.
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
