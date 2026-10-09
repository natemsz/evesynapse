package app

import (
	"context"
	"net/http"
)

// sessionFlash is the session key of a one-time message: an action that
// redirects ("3 skills were already trained and left out") leaves it,
// and the next page it lands on shows it once and clears it.
const sessionFlash = "flash"

// flash leaves msg for the next page this session renders. A second
// flash before that page loads replaces the first.
func (app *Application) flash(ctx context.Context, msg string) {
	app.sessions.Put(ctx, sessionFlash, msg)
}

// flashBack returns the way a form handler answers: leave a message
// and go back to path.
func (app *Application) flashBack(w http.ResponseWriter, r *http.Request, path string) func(string) {
	return func(message string) {
		app.flash(r.Context(), message)
		http.Redirect(w, r, path, http.StatusSeeOther)
	}
}
