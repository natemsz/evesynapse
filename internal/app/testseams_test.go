package app

// Test seams: thin entry points the tests drive that the running
// app reaches by its own paths (the worker's budgets, the link
// page's server-aware resolvers, and so on).

import (
	"context"
	"time"

	"evesynapse/internal/db/sqlc"
)

// notifyPass runs after a refresh cycle: for every account with
// characters, turn what is newly true into notifications.
func (app *Application) notifyPass(ctx context.Context, characters []db.Character, now time.Time) (created int) {
	return app.notifyAccounts(ctx, characters, now, nil)
}

// refreshMarketData runs one market pass, reporting how many
// payloads it stored (history downloads + book reads) and whether
// ESI's error limit stopped it. The region sweep runs on its own
// lane (runMarketLane), not in this pass.
func (app *Application) refreshMarketData(ctx context.Context, characters []db.Character) (stored int, limited bool) {
	// The market pass spends from its own lane, not the
	// character pass's budget: the character pass can spend
	// its whole allowance in a busy cycle.
	allowance := &fetchBudget{left: maxMarketFetchesPerCycle}
	return app.refreshMarketWarm(ctx, characters, allowance)
}

// appraisePublicContracts is the worker pass: it prices the
// due slice of live contracts (item list stored; never priced,
// or priced before the refresh age) and stores the per-line
// and per-contract results in one transaction per contract.
// Stored reads and writes only -- no ESI -- so it runs after
// the public contracts pass in the same cycle.
func (app *Application) appraisePublicContracts(ctx context.Context) int {
	return app.appraisePublicContractsWithin(ctx, 0)
}

func (app *Application) structureName(id int64) (string, bool) {
	if e, ok := app.entityLookup(id); ok && e.Kind == "structure" && e.Name != "" {
		return e.Name, true
	}
	return "", false
}

// requiredScopes lists the scopes the module cannot run without.
func (m moduleDef) requiredScopes() []string {
	var out []string
	for _, s := range m.Scopes {
		if !s.Optional {
			out = append(out, s.Scope)
		}
	}
	return out
}

// notice builds the partial's view model for module m on character
// name, or nil when the module is fully enabled and there is nothing
// to say. A locked module lists its missing required scopes; a limited
// one lists the optional scopes that would light up more of it.
func (st moduleStatus) notice(m moduleDef, characterID int64, name string) *lockedNotice {
	n := &lockedNotice{CharacterName: name, RelinkURL: relinkURL(m.ID, characterID)}
	switch st.State {
	case moduleLocked:
		n.Locked = true
		n.Heading = m.Title + " is locked for this character."
		n.Scopes = st.MissingRequired
	case moduleLimited:
		n.Heading = m.Title + " is limited for this character."
		n.Scopes = st.MissingOptional
	default:
		return nil
	}
	return n
}

// characterModuleStates returns every manifest module's status for ch,
// keyed by module id. Corp modules reflect the token's scopes only;
// the in-corporation role they also need is not visible in scopes.
func characterModuleStates(ch db.Character) map[string]moduleStatus {
	granted := scopeSet(ch.Scopes)
	defs := moduleDefs()
	out := make(map[string]moduleStatus, len(defs))
	for _, m := range defs {
		out[m.ID] = m.statusFor(granted)
	}
	return out
}

// drain reads every waiting log in the calling goroutine.
func (q *battleQueue) drain(ctx context.Context, read func(context.Context, *battleJob) battleResult) {
	for j := q.next(); j != nil; j = q.next() {
		q.read(ctx, j, read)
	}
}

// orderByDue stably orders characters most-overdue first: a
// character's due key is the earliest cached_until among the core
// snapshot kinds (a kind with no snapshot yet is due immediately,
// key zero). The sort is stable, so callers can layer the
// priority-flag ordering on top. Freshness for every character
// comes from one batched query, not one per character.
func (app *Application) orderByDue(ctx context.Context, characters []db.Character) []db.Character {
	out, _ := app.orderByDueTimed(ctx, characters)
	return out
}

// fullLinkChoice is every feature with every enhancement: today's link.
func fullLinkChoice() linkChoice { return fullLinkChoiceWith(nil) }

// resolveLink works a choice out: the features it names (an activity names
// the features that declare it), the closure over what they require, the
// scopes those declare, and the opted-in enhancements. An unknown activity,
// feature or an enhancement that belongs to no chosen feature is an error.
func resolveLink(c linkChoice) (linkResolved, error) { return resolveLinkWith(c, nil) }

// requestableScopes is every scope this build can ask for: the full link.
// In a build with every module it is eveScopes; a build without a module
// leaves that module's scopes out.
func requestableScopes() []string { return requestableScopesWith(nil) }
