package app

import (
	"net/http"
)

// handleAbout renders the public About page: what EveSynapse is,
// how to reach the operator, and the privacy policy, as three
// anchored sections (about, contact, privacy) that the footer on
// every page links into. Public on purpose: someone deciding
// whether to link a character should be able to read the privacy
// policy first, without an account.
func (app *Application) handleAbout(w http.ResponseWriter, r *http.Request) {
	data := app.publicPage(r.Context())
	app.render(r.Context(), w, http.StatusOK, "about.html", data)
}
