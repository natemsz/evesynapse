package app

// The shared demo account: one account, flagged on its user row
// (schema 076), holding a seeded fictional pilot that anyone can
// test-drive from the landing page's Try it now button. It exists
// to let people see what the app does with a lived-in account
// before they link their own characters.
//
// The account is deliberately unlike a real one in three ways,
// all enforced here rather than by hiding UI:
//
//   - It has no EVE tokens. Its character rows carry empty token
//     fields, and characterMaySync keeps the worker from ever
//     queueing it for a refresh, so no code path tries to spend a
//     token that does not exist.
//   - Its writes are turned away server-side (demoWriteGate): the
//     account is shared by every visitor, so nothing one visitor
//     does may stick for the next, and nothing may leak into
//     shared state like the public fit corpus.
//   - It heals itself: seedDemoData (demo_seed.go) can rebuild the
//     whole seed state at any time, and the worker restores it
//     daily, so visitor drift and stale dates never accumulate.
//
// The pilot and everyone she interacts with are invented; the IDs
// sit in a band EVE has not issued, so names and portraits resolve
// through the ordinary stored machinery (seeded rows) and the
// image server's generic portrait.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// The demo cast. Character and corporation IDs are fictional and
// chosen far outside the ranges CCP has issued.
const (
	demoCharacterID  = int64(2900000001) // Astra Vale, the all-V pilot
	demoCharacter2ID = int64(2900000002) // Juno Park, her industry alt
	demoCorpID       = int64(2900001001) // Test Pilot Collective

	demoCharName  = "Astra Vale"
	demoChar2Name = "Juno Park"
	demoCorpName  = "Test Pilot Collective"
)

// demoPilots is the demo account's roster, in switcher order.
var demoPilots = []struct {
	ID   int64
	Name string
}{
	{demoCharacterID, demoCharName},
	{demoCharacter2ID, demoChar2Name},
}

// demoWriteNotice is the one answer every blocked write gives.
const demoWriteNotice = "That's switched off in the demo: this account is shared by every visitor, so changes are not saved. Link your own characters to use it for real."

// demoUserID resolves the demo account's user id, or 0 when this
// install has no demo account yet. The answer is cached on the
// application; seeding sets the cache, so the steady-state cost
// is one mutex read per call.
func (app *Application) demoUserID(ctx context.Context) int64 {
	app.demoMu.Lock()
	defer app.demoMu.Unlock()
	if app.demoUserKnown {
		return app.demoUserIDCached
	}
	u, err := app.queries.GetDemoUser(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No demo account yet; cache that too, so a render
			// never pays for the lookup twice. ensureDemoAccount
			// replaces the cached value when it creates one.
			app.demoUserIDCached = 0
			app.demoUserKnown = true
		} else {
			logging.Errorf("demo: look up demo user: %v", err)
		}
		return 0
	}
	app.demoUserIDCached = u.ID
	app.demoUserKnown = true
	return u.ID
}

// setDemoUserID records the demo account's id in the cache.
func (app *Application) setDemoUserID(id int64) {
	app.demoMu.Lock()
	defer app.demoMu.Unlock()
	app.demoUserIDCached = id
	app.demoUserKnown = true
}

// isDemoUser reports whether a user id is the demo account.
func (app *Application) isDemoUser(ctx context.Context, userID int64) bool {
	return userID != 0 && userID == app.demoUserID(ctx)
}

// sessionIsDemo reports whether the request's session belongs to
// the demo account.
func (app *Application) sessionIsDemo(ctx context.Context) bool {
	return app.isDemoUser(ctx, app.userID(ctx))
}

// characterMaySync is the refresh-eligibility check the worker
// and every token-spending pass use: the plain link-state check,
// plus the demo account, whose pilots have no tokens to spend and
// whose data is the seed's, never ESI's.
func (app *Application) characterMaySync(ctx context.Context, ch db.Character) bool {
	if !characterSyncs(ch) {
		return false
	}
	return !app.isDemoUser(ctx, ch.UserID)
}

// ensureDemoAccount makes sure the demo account, its pilots and
// its seed data exist, creating whatever is missing. It runs at
// boot and again from the demo sign-in route, so a first visit
// works even on an install whose boot seed was interrupted. The
// account and character rows are upserts with fixed values; the
// data seed runs only when it is absent, and re-running it is
// the reset's job, not this one's.
func (app *Application) ensureDemoAccount(ctx context.Context) error {
	userID := app.demoUserID(ctx)
	if userID == 0 {
		u, err := app.queries.CreateUser(ctx)
		if err != nil {
			return err
		}
		if err := app.queries.MarkUserDemo(ctx, u.ID); err != nil {
			return err
		}
		userID = u.ID
		app.setDemoUserID(userID)
		logging.Infof("demo: created the demo account (user %d)", userID)
	}
	scopes := strings.Join(eveScopes, " ")
	for _, pilot := range demoPilots {
		if _, err := app.queries.UpsertCharacter(ctx, db.UpsertCharacterParams{
			CharacterID: pilot.ID,
			UserID:      userID,
			Name:        pilot.Name,
			Scopes:      scopes,
			LinkState:   linkStateOK,
		}); err != nil {
			return err
		}
		if err := app.queries.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
			CharacterID:   pilot.ID,
			CorporationID: demoCorpID,
		}); err != nil {
			return err
		}
	}
	if !app.demoSeedPresent(ctx) {
		if err := app.seedDemoData(ctx, time.Now()); err != nil {
			return err
		}
		if err := app.queries.StampDemoReset(ctx, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

// demoSeedPresent reports whether the seed data is in place; the
// skills snapshot is the canary, seeded first and never removed
// by anything but the reset's own wipe-and-reseed.
func (app *Application) demoSeedPresent(ctx context.Context) bool {
	_, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: demoCharacterID, Kind: esi.SnapSkills,
	})
	return err == nil
}

// handleDemoSignIn is GET /auth/demo: the landing page's Try it
// now button. It opens an ordinary session on the shared demo
// account, one per visitor, with no EVE SSO involved.
func (app *Application) handleDemoSignIn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := app.ensureDemoAccount(ctx); err != nil {
		logging.Errorf("demo: sign-in could not prepare the demo account: %v", err)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	userID := app.demoUserID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = app.sessions.RenewToken(ctx)
	app.sessions.Put(ctx, sessionAuthenticated, true)
	app.sessions.Put(ctx, sessionUserID, int(userID))
	putSessionCharID(app.sessions, ctx, demoCharacterID)
	app.sessions.Put(ctx, sessionCharacterName, demoCharName)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// demoPostAllowed lists the POST routes a demo session may still
// use: signing out, and the read-only tools whose answers are
// computed from what the visitor pastes or picks, never stored
// against an account (the appraiser, the fitting workbench's
// simulate/solve/export, and the anonymous-fit share flow, which
// is token-addressed exactly as it is for signed-out visitors).
func demoPostAllowed(path string) bool {
	if path == "/auth/logout" {
		return true
	}
	for _, prefix := range []string{
		"/fittings/simulate/",
		"/fittings/solve-implants/",
		"/fittings/export/",
		"/fittings/save-anonymous/",
		"/market/appraise/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return strings.HasPrefix(path, "/fittings/share/") && strings.HasSuffix(path, "/delete")
}

// demoWriteGate is the server-side safeguard for the shared demo
// account. Every mutating request from a demo session that is
// not on the read-only allowlist is turned away with the demo
// notice before it reaches a handler, whichever module owns the
// route: mail organization, fit saves and public publishing,
// route sends to the game client, calendar responses, PI
// commands, contract creation, character unlinking, account and
// settings changes, Discord linking, home layout and widget
// persistence, group/SRP/doctrine writes, and everything else
// that would change stored or shared state. GET routes that
// start a link flow (EVE character linking, Discord connect) are
// turned away the same way; a real sign-in through EVE SSO is
// unaffected, since it replaces the session's account outright.
func (app *Application) demoWriteGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.sessionIsDemo(r.Context()) {
			path := r.URL.Path
			switch {
			case r.Method == http.MethodGet &&
				(path == "/auth/eve" || path == "/auth/link" ||
					path == "/discord/connect" || path == "/discord/callback"):
				app.flash(r.Context(), demoWriteNotice)
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			case r.Method != http.MethodGet && r.Method != http.MethodHead &&
				!demoPostAllowed(path):
				if path == "/map/route" {
					// The map's route send speaks JSON.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"message":"Route sending is switched off in the demo."}`))
					return
				}
				app.flash(r.Context(), demoWriteNotice)
				http.Redirect(w, r, demoBackTarget(r), http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// demoBackTarget is where a turned-away write lands: back on the
// page the visitor was using, when the referer names one on this
// host, and home otherwise.
func demoBackTarget(r *http.Request) string {
	referer := r.Header.Get("Referer")
	if referer == "" {
		return "/"
	}
	if u, err := url.Parse(referer); err == nil && u.Host == r.Host && u.Path != "" {
		return u.Path
	}
	return "/"
}

// maybeResetDemo is the worker cycle's demo housekeeping: the
// daily restore of the seed state. A demo whose seed never
// landed (an install seeded before its SDE import finished) is
// retried on a short fuse instead, so the account completes
// itself once the data it resolves names from exists.
func (app *Application) maybeResetDemo(ctx context.Context, now time.Time) {
	if app.demoUserID(ctx) == 0 {
		return
	}
	last, err := app.queries.GetDemoLastReset(ctx)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("demo: read reset clock: %v", err)
			return
		}
		last = time.Time{}
	}
	seeded := app.demoSeedPresent(ctx)
	switch {
	case seeded && now.Sub(last) >= 24*time.Hour:
		// The daily restore.
	case !seeded && now.Sub(last) >= 10*time.Minute:
		// The seed is missing or incomplete; try again.
	default:
		return
	}
	if err := app.seedDemoData(ctx, now); err != nil {
		logging.Errorf("demo: restore seed state: %v", err)
		// Stamp the attempt anyway: a seed that cannot land
		// retries on the short fuse above, not every cycle.
		_ = app.queries.StampDemoReset(ctx, now.UTC())
		return
	}
	if err := app.queries.StampDemoReset(ctx, now.UTC()); err != nil {
		logging.Errorf("demo: stamp reset clock: %v", err)
		return
	}
	logging.Infof("demo: seed state restored")
}
