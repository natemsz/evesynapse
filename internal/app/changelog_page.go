package app

import (
	_ "embed"
	"html/template"
	"net/http"
	"strings"

	"evesynapse/internal/htmlsanitize"
)

// The changelog page renders the project's changelog in the
// app's own shell, one section per release, newest first. The
// embedded file is a byte copy of the CHANGELOG.md at the
// repository root; a test pins them equal so the page can never
// drift from the canonical text.

//go:embed changelog.md
var changelogText string

type changelogView struct {
	Body template.HTML
}

// changelogHTML is the changelog rendered once, at startup: the
// text is fixed for the life of the build, so no request ever
// pays to render it again. The file's own title and intro stay
// in the repository copy, and the page lists released versions,
// so the render starts at the first version section.
var changelogHTML = renderChangelog(changelogText)

func renderChangelog(src string) template.HTML {
	if i := strings.Index(src, "## [v"); i >= 0 {
		src = src[i:]
	}
	return htmlsanitize.Markdown(escapeUnderscores(src))
}

// escapeUnderscores keeps snake_case names (LOW_RAM_MODE and
// its kin) literal in the render: in the notice renderer a _
// marks italics, and a changelog entry is a title, not prose,
// so its underscores are text. A word carrying an address
// keeps its underscores: an escape inside one would break it.
func escapeUnderscores(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		words := strings.Split(line, " ")
		for j, w := range words {
			if !strings.Contains(w, "://") {
				words[j] = strings.ReplaceAll(w, "_", `\_`)
			}
		}
		lines[i] = strings.Join(words, " ")
	}
	return strings.Join(lines, "\n")
}

// handleChangelog renders the public changelog page. Public for
// the same reason the About page is: what each release changed
// should be readable without an account.
func (app *Application) handleChangelog(w http.ResponseWriter, r *http.Request) {
	data := app.publicPage(r.Context())
	data.Changelog = &changelogView{Body: changelogHTML}
	app.render(r.Context(), w, http.StatusOK, "changelog.html", data)
}
