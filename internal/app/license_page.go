package app

import (
	_ "embed"
	"net/http"
)

// The license page serves the project's legal texts in the app's
// own shell: the BSD-3-Clause-Clear license and the additional
// patent grant. The embedded files are byte copies of the LICENSE
// and PATENTS files at the repository root; a test pins them equal
// so the page can never drift from the canonical texts.

//go:embed license.txt
var licenseText string

//go:embed patents.txt
var patentsText string

type legalView struct {
	License string
	Patents string
}

// handleLicense renders the public license page. Public for the
// same reason the About page is: the terms should be readable
// before anyone links a character.
func (app *Application) handleLicense(w http.ResponseWriter, r *http.Request) {
	data := app.publicPage(r.Context())
	data.Legal = &legalView{License: licenseText, Patents: patentsText}
	app.render(r.Context(), w, http.StatusOK, "license.html", data)
}
