package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Planetary-industry worker pass (Phase 2): keeps the colonies
// list and one layout snapshot per planet warm for one character,
// and harvests the pin/schematic/planet names the pages resolve.
//
// The colonies endpoint is gated by esi-planets.manage_planets.v1,
// which the app only started requesting in Phase 2 (CCP publishes
// no read scope for it — see auth.go). A character linked before
// that gets a refusal: ESI answers 403 for some scope failures and
// 401 for others (a token lacking the scope is "unauthorized" for
// the endpoint), so both statuses classify as the scope missing.
// The refusal is recorded in snapshot_fetch_state like the
// economy stale-scope refusals, the kind backs off for
// roleMissingBackoff instead of retrying every minute, and the
// pages say "PI not enabled — re-link" from the record. It is
// never a dead token: the cycle's validAccessToken gate runs
// before this pass, so a 401 here means the (valid) token simply
// lacks the planetary scope, and link_state is never touched.
// When the character's granted scopes show the scope has landed
// (a re-link), the backoff is bypassed so PI starts within a
// cycle.
// ---------------------------------------------------------------------------

// planetScope is the only SSO scope gating the colony GETs.
const planetScope = "esi-planets.manage_planets.v1"

// piScopeDetail is the user-safe fetch-state detail recorded when
// ESI refuses the colonies endpoint for want of the scope. Pages
// show it (plus the consent-screen wording) as the "not enabled"
// state; piNotEnabled matches on the prefix.
const piScopeDetail = "Planetary industry is not enabled for this character"

// maxPlanetLayoutsPerCycle bounds layout fetches per character
// per worker cycle. A character can hold at most six colonies, so
// the cap is headroom, not a schedule.
const maxPlanetLayoutsPerCycle = 8

// characterHasScope reports whether the character's granted scope
// set (characters.scopes, space-joined at sign-in) includes scope.
func characterHasScope(ch db.Character, scope string) bool {
	for _, s := range strings.Fields(ch.Scopes) {
		if s == scope {
			return true
		}
	}
	return false
}

// piScopeRefusal reports whether an ESI failure on a planetary
// endpoint means "this login never granted the planetary scope"
// rather than a broken token or a transient error. CCP answers
// scope-gated endpoints inconsistently — 403 for some, 401 for
// others (verified live: the colonies endpoint answers 401 to a
// token without esi-planets.manage_planets.v1) — so both statuses
// classify the same way here. This is deliberately NOT the
// token-dead path: it only ever records the scope-missing state.
func piScopeRefusal(err error) bool {
	if esi.IsForbidden(err) {
		return true
	}
	code, ok := esi.StatusCode(err)
	return ok && code == 401
}

// piNotEnabled reports whether PI is dark for this character
// because its login predates the planetary scope: a recorded
// colonies refusal stands and the granted scopes still lack the
// scope. A character that re-linked (scope present) is never
// flagged — the worker retries within the cycle.
func (app *Application) piNotEnabled(ctx context.Context, ch db.Character) bool {
	if characterHasScope(ch, planetScope) {
		return false
	}
	state, detail, found := app.corpKindState(ctx, ch.CharacterID, esi.SnapPlanets)
	return found && state == fetchStateError && strings.HasPrefix(detail, piScopeDetail)
}

// refreshPlanetarySnapshots runs the PI pass for one character and
// reports how many snapshots it stored and whether ESI's error
// limit stopped it.
func (app *Application) refreshPlanetarySnapshots(ctx context.Context, ch db.Character, allowance *fetchBudget) (refreshed int, limited bool) {
	switch app.fetchPlanetsKind(ctx, ch, allowance) {
	case corpFetchOK:
		refreshed++
	case corpFetchLimited:
		return refreshed, true
	}
	warmed, ltd := app.warmPlanetLayouts(ctx, ch, allowance)
	return refreshed + warmed, ltd
}

// fetchPlanetsKind refreshes the colonies list when it is due.
// It is fetchCharKind (economy_worker.go) specialized for the
// planetary scope: the refusal records the PI detail, and the
// recorded refusal stops applying the moment the scope shows up
// in the character's granted scopes.
func (app *Application) fetchPlanetsKind(ctx context.Context, ch db.Character, allowance *fetchBudget) corpFetchOutcome {
	snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapPlanets})
	switch {
	case serr == nil && esi.SnapshotFresh(snap):
		return corpFetchSkipped
	case serr != nil && !errors.Is(serr, sql.ErrNoRows):
		logging.Errorf("worker: read %s snapshot for character %d: %v", esi.SnapPlanets, ch.CharacterID, serr)
	}

	// A recorded scope refusal backs the kind off — unless the
	// character has since re-linked with the scope, in which
	// case retrying now is exactly what the user is waiting for.
	if !characterHasScope(ch, planetScope) {
		if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: esi.SnapPlanets}); err == nil &&
			state.State == fetchStateError && strings.HasPrefix(state.Detail, piScopeDetail) {
			if time.Since(state.AttemptedAt) < roleMissingBackoff {
				return corpFetchSkipped
			}
		}
	}

	if !allowance.take() {
		return corpFetchSkipped
	}
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapPlanets); err != nil {
		switch {
		case errors.Is(err, esi.ErrErrorLimit):
			logging.Warnf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", esi.SnapPlanets, ch.CharacterID)
			return corpFetchLimited
		case piScopeRefusal(err):
			code, _ := esi.StatusCode(err)
			detail := piScopeDetail + " — sign in again to re-link and grant the planetary scope."
			app.recordCorpFetchState(ctx, ch.CharacterID, esi.SnapPlanets, fetchStateError, detail)
			logging.Infof("worker: %s for character %d refused by ESI (%d); planetary scope not granted on this login", esi.SnapPlanets, ch.CharacterID, code)
			return corpFetchFailed
		default:
			logging.Errorf("worker: refresh %s for character %d: %v", esi.SnapPlanets, ch.CharacterID, err)
			app.recordCorpFetchState(ctx, ch.CharacterID, esi.SnapPlanets, fetchStateError, err.Error())
			return corpFetchFailed
		}
	}
	app.recordCorpFetchState(ctx, ch.CharacterID, esi.SnapPlanets, fetchStateOK, "")
	return corpFetchOK
}

// warmPlanetLayouts fetches the colony layout behind each colony
// of the character's colonies snapshot whose stored layout is
// missing or past its ESI cache window, at most
// maxPlanetLayoutsPerCycle of them. Layouts change when a colony
// is edited in-game, so unlike killmail details they refresh on
// the snapshot cadence rather than living forever. It reports how
// many were stored and whether ESI's error limit stopped the pass.
func (app *Application) warmPlanetLayouts(ctx context.Context, ch db.Character, allowance *fetchBudget) (fetched int, limited bool) {
	var colonies esi.Colonies
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapPlanets, &colonies) {
		return 0, false // no colonies list yet; nothing to detail
	}
	hasScope := characterHasScope(ch, planetScope)

	for _, colony := range colonies {
		if fetched >= maxPlanetLayoutsPerCycle {
			break
		}
		if colony.PlanetID <= 0 {
			continue
		}
		kind := esi.PlanetLayoutKind(colony.PlanetID)
		snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
		switch {
		case serr == nil && esi.SnapshotFresh(snap):
			continue
		case serr != nil && !errors.Is(serr, sql.ErrNoRows):
			logging.Errorf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
			continue
		}

		// The same scope refusal the list records applies to
		// every layout (same endpoint family); honor its backoff
		// per layout instead of 403ing the whole set each cycle.
		if !hasScope {
			if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: kind}); err == nil &&
				state.State == fetchStateError && strings.HasPrefix(state.Detail, piScopeDetail) {
				if time.Since(state.AttemptedAt) < roleMissingBackoff {
					continue
				}
			}
		}

		if !allowance.take() {
			break
		}
		if err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
			switch {
			case errors.Is(err, esi.ErrErrorLimit):
				return fetched, true
			case piScopeRefusal(err):
				code, _ := esi.StatusCode(err)
				detail := piScopeDetail + " — sign in again to re-link and grant the planetary scope."
				app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, detail)
				logging.Infof("worker: %s for character %d refused by ESI (%d); planetary scope not granted on this login", kind, ch.CharacterID, code)
			default:
				if ctx.Err() == nil {
					logging.Errorf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
				}
				app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, err.Error())
			}
			continue
		}
		fetched++
	}
	return fetched, false
}
