package app

import (
	"context"
	"evesynapse/internal/app/module"
	"fmt"
)

// ---------------------------------------------------------------------------
// Economy cluster: Wallet, Orders, Contracts, Industry, rendered from
// worker-warmed snapshots and local tables only. Each dataset degrades
// on its own: a section whose snapshot has not landed says so, and a
// recorded fetch failure (a login that predates today's scope list
// answering 403, say) says that instead.
// ---------------------------------------------------------------------------

// econSectionState is the empty-state triage for one economy
// dataset: loaded, still warming, or unavailable with a note
// explaining why (mirrors the corporation sections' states).
type econSectionState = module.EconSectionState

// econSection resolves one economy section's display state: a
// decoded snapshot wins; otherwise the recorded fetch state
// decides between the warming note and the failure note. (The
// loader is loadCorpSnapshot — a plain snapshot-row decode
// despite the name; it never fetches.)
func (app *Application) econSection(ctx context.Context, characterID int64, kind string, out any) econSectionState {
	if app.loadCorpSnapshot(ctx, characterID, kind, out) {
		return econSectionState{Loaded: true}
	}
	state, detail, found := app.corpKindState(ctx, characterID, kind)
	if found && (state == fetchStateError || state == fetchStateRoleMissing) && detail != "" {
		return econSectionState{Note: detail}
	}
	return econSectionState{Warming: true}
}

// orderRangeLabel renders an order's range readably: the ESI
// enum is "station"|"solarsystem"|"region" or a jump count.

// econLocationTitle renders an order/transaction/contract
// location: NPC station and system names from the local caches,
// then the worker-resolved structure name, then a
// "#<id>".
func (app *Application) econLocationTitle(ctx context.Context, locationID int64) string {
	if name, ok := app.esi.CachedPlaceName(ctx, locationID); ok {
		return name
	}
	if locationID > 1_000_000_000 {
		if name := app.resolvedStructureTitle(ctx, locationID); name != "" {
			return name
		}
		app.notePageWantFromContext(ctx, pageWantStructure, locationID)
		return fmt.Sprintf("Structure #%d", locationID)
	}
	app.notePageWantFromContext(ctx, pageWantPlace, locationID)
	return fmt.Sprintf("Station #%d", locationID)
}

// ---------------------------------------------------------------------------
// Wallet page: balance header, journal window, transaction window.
// ---------------------------------------------------------------------------

// txnCounterparty resolves a market-transaction counterparty (no
// kind travels with transactions): a resolved character name
// means character, a resolved corporation name means
// corporation, anything else falls back to the character label.
func (app *Application) txnCounterparty(ctx context.Context, id int64) (name string, isChar, isCorp bool) {
	if name, settled := app.resolvedCharacterName(ctx, id); settled && name != "" {
		return name, true, false
	}
	if name, ok := app.esi.CachedCorpName(id); ok && name != "" {
		return name, false, true
	}
	return app.displayCharacter(ctx, id), false, false
}

// ---------------------------------------------------------------------------
// Orders page: open orders plus recent closed history.
// ---------------------------------------------------------------------------
