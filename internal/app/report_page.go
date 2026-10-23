package app

import (
	"net/http"
)

// handleReport renders the public Report a bug page: how to reach
// the operator with a bug report or a feature request, on Discord
// or by email. Public on purpose, like the About page: the people
// who hit a bug may not have an account yet, and the footer on
// every page links here.
func (app *Application) handleReport(w http.ResponseWriter, r *http.Request) {
	data := app.publicPage(r.Context())
	app.render(r.Context(), w, http.StatusOK, "report.html", data)
}
