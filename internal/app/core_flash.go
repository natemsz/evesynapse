package app

import "context"

// sessionFlash is the session key of a one-time message: an action that
// redirects ("3 skills were already trained and left out") leaves it,
// and the next page it lands on shows it once and clears it.
const sessionFlash = "flash"

// flash leaves msg for the next page this session renders. A second
// flash before that page loads replaces the first.
func (app *Application) flash(ctx context.Context, msg string) {
	app.sessions.Put(ctx, sessionFlash, msg)
}
