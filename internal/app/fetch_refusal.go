package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// One refusal policy for per-character ESI fetches.
//
// When ESI definitively refuses a (character, kind) — a 401 or 403
// for a scope the login never granted, a token ESI rejects, or an
// in-game role the character lacks — the refusal is recorded once
// in snapshot_fetch_state and every fetch path consults that record
// before spending fetch allowance, so the want is not retried every
// cycle. The record clears when the character re-links (a fresh
// sign-in stores a new token and grant set; see auth_links.go) and,
// as a safety valve, may be rechecked once refusalRecheck has
// passed. Transient failures never settle: ESI's error limit keeps
// its own backoff, and 5xx/network errors record nothing here.
//
// Refusals live in the same store the corporation pass built for
// its role-missing state: a role refusal is the role_missing state
// (corp.go, with its own shorter recheck, since an in-game role can
// be granted at any time); scope and token refusals are error rows
// whose detail carries one of the refusal prefixes below, which is
// also what the pages render as the explanation.
// ---------------------------------------------------------------------------

// refusalRecheck bounds how long a recorded scope or token refusal
// keeps a (character, kind) out of the fetch rotation before one
// retry is allowed. A re-link clears the record immediately; this
// only covers grants that change without one.
const refusalRecheck = 24 * time.Hour

// unauthorizedDetailPrefix marks a recorded 401 in a fetch-state
// detail (the text after it says whether the scope or the token
// was refused, and what signing in again does).
const unauthorizedDetailPrefix = "ESI refused (401)"

// Pseudo-kinds under which per-character ESI wants that store no
// snapshot record their refusals in snapshot_fetch_state.
const (
	// structureNamesKind: the structure-name lookups one character
	// is asked to make (structures_worker.go).
	structureNamesKind = "structure_names"
	// opFleetKind: the fleet read behind op attendance for the
	// op's FC character (ops_attendance.go).
	opFleetKind = "op_fleet"
)

// fetchRefusal classifies an ESI failure as a definitive refusal
// of this (character, kind): ESI answered 401 or 403. The reason
// separates a scope the login never granted (ESI names the scope
// in its answer) from a token ESI rejected outright; a 403 that
// names no scope is reported as a plain refusal and paths that
// know the in-game meaning (the corporation role gate) classify
// it themselves before deferring to this layer.
func fetchRefusal(err error) (code int, scopeNamed bool, ok bool) {
	var se *esi.StatusError
	if !errors.As(err, &se) {
		return 0, false, false
	}
	if se.Code != http.StatusUnauthorized && se.Code != http.StatusForbidden {
		return 0, false, false
	}
	return se.Code, strings.Contains(strings.ToLower(se.Detail), "scope"), true
}

// scopeNamedRefusal reports whether err is a refusal the shared
// layer owns ahead of a path's own classification: any 401, or a
// 403 that names the missing scope in ESI's answer.
func scopeNamedRefusal(err error) bool {
	code, scopeNamed, ok := fetchRefusal(err)
	return ok && (code == http.StatusUnauthorized || scopeNamed)
}

// refusalDetail reports whether a fetch-state detail records a
// refusal (any path's wording) rather than a transient error.
func refusalDetail(detail string) bool {
	return strings.HasPrefix(detail, forbiddenDetailPrefix) ||
		strings.HasPrefix(detail, unauthorizedDetailPrefix) ||
		strings.HasPrefix(detail, piScopeDetail)
}

// fetchStateSettled reports whether a recorded fetch outcome keeps
// its (character, kind) out of the fetch rotation right now: a role
// refusal inside its backoff, or a scope/token refusal inside the
// recheck window.
func fetchStateSettled(state db.SnapshotFetchState) bool {
	switch {
	case state.State == fetchStateRoleMissing:
		return time.Since(state.AttemptedAt) < roleMissingBackoff
	case state.State == fetchStateError && refusalDetail(state.Detail):
		return time.Since(state.AttemptedAt) < refusalRecheck
	}
	return false
}

// refusalSettled consults the recorded fetch state for one
// (character, kind): true when a refusal is on record and still in
// its window, so the caller skips the fetch entirely.
func (app *Application) refusalSettled(ctx context.Context, characterID int64, kind string) bool {
	state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		return false
	}
	return fetchStateSettled(state)
}

// recordFetchRefusal stores one refusal for a (character, kind)
// and writes the refusal's single log line. It reports whether the
// error was a refusal at all, so callers can fold it into their
// error switch: a true result replaces the caller's per-cycle
// error line.
func (app *Application) recordFetchRefusal(ctx context.Context, characterID int64, kind string, err error) bool {
	code, scopeNamed, ok := fetchRefusal(err)
	if !ok {
		return false
	}
	var detail, reason string
	switch {
	case code == http.StatusUnauthorized && scopeNamed:
		detail = unauthorizedDetailPrefix + " — this character's login does not grant the scope this data needs; sign in again to re-grant scopes."
		reason = "the scope is not granted on this login"
	case code == http.StatusUnauthorized:
		detail = unauthorizedDetailPrefix + " — the character's token was rejected; sign in again to re-link the character."
		reason = "the token was rejected"
	case scopeNamed:
		detail = forbiddenDetailPrefix + " — this character's login predates the current scope list; sign in again to re-grant scopes."
		reason = "the scope is not granted on this login"
	default:
		detail = forbiddenDetailPrefix + " — ESI refused this data for the character's login; sign in again to re-grant scopes."
		reason = "the login was refused"
	}
	app.recordCorpFetchState(ctx, characterID, kind, fetchStateError, detail)
	logging.Infof("worker: %s for character %d refused by ESI (%d): %s; recorded, and not fetched again until the character signs in again", kind, characterID, code, reason)
	return true
}

// clearCharacterFetchStates forgets every recorded fetch outcome
// for a character. It runs on a re-link: the fresh token and grant
// set make every refusal recorded against the old ones stale, so
// the character's kinds are fetchable again on the next cycle.
func (app *Application) clearCharacterFetchStates(ctx context.Context, characterID int64) {
	if err := app.queries.DeleteSnapshotFetchStatesForCharacter(ctx, characterID); err != nil {
		logging.Errorf("sso: clear fetch states for character %d: %v", characterID, err)
	}
}
