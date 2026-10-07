package app

import (
	"context"
	"fmt"
	"net/http"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Corporation cluster: shared scaffolding for the corporation
// subpages and the worker's corporation pass.
//
// Corporation payloads ride the per-character snapshot table under
// corp_* kinds: each linked character's worker pass fetches its own
// corporation's data with its own token, so pages follow the
// selected character's corporation exactly like the per-character
// pages do, and character/corporation ID collisions in EVE's
// numeric space can never cross-contaminate (everything is keyed
// by the viewing character).
//
// Role-gated endpoints (member tracking, wallets, orders, assets,
// structures, corp killmails) answer 403 when the character lacks
// the in-game role. The worker records that in snapshot_fetch_state
// (schema 005) instead of retrying every cycle, and the pages
// render a plain "needs the role" state from it. Handlers read
// snapshots and local tables only — never ESI.
// ---------------------------------------------------------------------------

// walletDivisionCount is EVE's fixed corporation wallet division
// count (divisions 1..7; 1 is the master wallet).
const walletDivisionCount = 7

// Fetch-state values stored in snapshot_fetch_state.state.
const (
	fetchStateOK          = "ok"
	fetchStateRoleMissing = "role_missing"
	fetchStateError       = "error"
	// fetchStateDead marks a definitive market failure (the
	// request itself is wrong -- a bad type id, or a type with
	// no such data in that region): marketFetchDue gates it
	// like a success, since retrying sooner fails the same way.
	fetchStateDead = "dead"
)

// corpAssetNamesKind is the worker pseudo-kind under which the
// corp-assets name-warming pass records its attempts in
// snapshot_fetch_state (it throttles the names POST; no snapshot
// is ever stored under it).
const corpAssetNamesKind = "corp_asset_names"

// corpSnapshotKinds lists every corporation snapshot kind the
// worker maintains per character, in fetch order: identity and
// membership first (the member-tracking 403 retry logic keys off
// the members fetch), then the role-gated datasets.
func corpSnapshotKinds() []string {
	kinds := []string{
		esi.SnapCorpInfo,
		esi.SnapCorpMembers,
		esi.SnapCorpMemberTracking,
		esi.SnapCorpWallets,
	}
	for d := int64(1); d <= walletDivisionCount; d++ {
		kinds = append(kinds, esi.CorpJournalKind(d))
	}
	for d := int64(1); d <= walletDivisionCount; d++ {
		kinds = append(kinds, esi.CorpTxnsKind(d))
	}
	return append(kinds,
		esi.SnapCorpOrders,
		esi.SnapCorpAssets,
		esi.SnapCorpStructures,
		esi.SnapCorpKillmails,
	)
}

// corpKindRole returns the human label of the in-game role an ESI
// 403 on kind means the viewing character lacks (from each
// endpoint's x-required-roles in CCP's OpenAPI document). "" for
// kinds any corp member can read.
func corpKindRole(kind string) string {
	switch kind {
	case esi.SnapCorpMemberTracking, esi.SnapCorpAssets, esi.SnapCorpKillmails:
		return "Director"
	case esi.SnapCorpOrders:
		return "Accountant or Trader"
	case esi.SnapCorpStructures:
		return "Station Manager"
	case esi.SnapCorpWallets:
		return "Accountant or Junior Accountant"
	}
	if _, ok := divisionOfKind(kind); ok {
		return "Accountant or Junior Accountant"
	}
	return ""
}

// divisionOfKind extracts the wallet division from a per-division
// snapshot kind (corp_journal_3 / corp_txns_5).
func divisionOfKind(kind string) (int64, bool) {
	for _, prefix := range []string{esi.SnapCorpJournalPrefix, esi.SnapCorpTxnsPrefix} {
		if len(kind) > len(prefix) && kind[:len(prefix)] == prefix {
			var d int64
			if _, err := fmt.Sscanf(kind[len(prefix):], "%d", &d); err == nil && d >= 1 && d <= walletDivisionCount {
				return d, true
			}
		}
	}
	return 0, false
}

// walletDivisionLabel names a wallet division the way the client
// does: division 1 is the master wallet.
func walletDivisionLabel(division int64) string {
	if division == 1 {
		return "Master Wallet"
	}
	return fmt.Sprintf("Division %d", division)
}

// ---------------------------------------------------------------------------
// Handler-side loaders: snapshot rows and fetch state, DB only.
// ---------------------------------------------------------------------------

// corpKindState returns the recorded fetch outcome for
// (character, kind): state ("ok"/"role_missing"/"error") plus its
// detail (role label or error text). found=false when the worker
// has never attempted the kind.
func (app *Application) corpKindState(ctx context.Context, characterID int64, kind string) (state, detail string, found bool) {
	row, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		return "", "", false
	}
	return row.State, row.Detail, true
}

// corpSectionState is the empty-state triage every corporation
// section renders from: data loaded, still warming, or refused for
// want of an in-game role.
type corpSectionState struct {
	Loaded      bool
	Warming     bool   // no snapshot yet and no refusal recorded
	Forbidden   bool   // ESI answered 403 on the last attempt
	RoleMissing string // role label when Forbidden named one ("" = label-less)
}

// corpSection resolves the display state for one snapshot kind:
// a decoded snapshot wins; otherwise the recorded fetch state
// decides between the role note and the warming note.
func (app *Application) corpSection(ctx context.Context, characterID int64, kind string, out any) corpSectionState {
	if app.loadCorpSnapshot(ctx, characterID, kind, out) {
		return corpSectionState{Loaded: true}
	}
	state, detail, found := app.corpKindState(ctx, characterID, kind)
	if found && state == fetchStateRoleMissing {
		return corpSectionState{Forbidden: true, RoleMissing: detail}
	}
	return corpSectionState{Warming: true}
}

// ---------------------------------------------------------------------------
// Page scaffolding: the selected character, its corporation, and
// the display identity of that corporation.
// ---------------------------------------------------------------------------

// corpViewBase carries what every corporation subpage header
// renders: the viewing character, the corporation's display title,
// and the IDs the switcher/sub-nav links need. Embedded in each
// subpage's view.
type corpViewBase struct {
	CharacterName string
	CharacterID   int64 // active character (switcher + sub-nav links)
	CorpID        int64
	CorpTitle     string // "Name [TICKER]", or "Corporation #<id>" until corp_info lands
	MemberCount   string // member count from corp_info, "" when unknown
}

// corpPageSelection is pickCorpPage's result: the switcher state
// plus the selected character's corporation (from local tables
// only).
type corpPageSelection struct {
	Active db.Character
	Links  []assetCharLink
	Base   corpViewBase
}

// pickCorpPage resolves the corporation subpage context: the
// signed-in user's characters and active one (pickCharacter
// semantics), the active character's corporation ID from the
// worker-maintained map, and the corporation's display identity
// from its stored public payload. ok=false means the session has
// no user or no characters yet.
func (app *Application) pickCorpPage(ctx context.Context, r *http.Request) (corpPageSelection, bool, error) {
	_, active, links, err := app.pickCharacter(ctx, r, "/corporations/")
	if err != nil {
		return corpPageSelection{}, false, err
	}
	if links == nil {
		return corpPageSelection{}, false, nil
	}

	sel := corpPageSelection{Active: active, Links: links}
	base := corpViewBase{
		CharacterName: active.Name,
		CharacterID:   active.CharacterID,
		CorpTitle:     "Corporation",
	}
	if row, err := app.queries.GetCharacterCorporation(ctx, active.CharacterID); err == nil {
		base.CorpID = row.CorporationID
		base.CorpTitle = fmt.Sprintf("Corporation #%d", row.CorporationID)
	}
	var info esi.Corporation
	if app.loadCorpSnapshot(ctx, active.CharacterID, esi.SnapCorpInfo, &info) && info.Name != "" {
		base.CorpTitle = info.Name
		if info.Ticker != "" {
			base.CorpTitle = fmt.Sprintf("%s [%s]", info.Name, info.Ticker)
		}
		base.MemberCount = esi.FormatInt(info.MemberCount)
	}
	sel.Base = base
	return sel, true, nil
}

// corpStructureNames decodes the stored structures snapshot into a
// structure ID to name map (payload names, no ESI), used to title
// structure locations on the orders/assets pages.
func (app *Application) corpStructureNames(ctx context.Context, characterID int64) map[int64]string {
	var structures esi.CorpStructures
	if !app.loadCorpSnapshot(ctx, characterID, esi.SnapCorpStructures, &structures) {
		return nil
	}
	names := make(map[int64]string, len(structures))
	for _, s := range structures {
		if s.Name != "" {
			names[s.StructureID] = s.Name
		}
	}
	return names
}

// corpLocationTitle renders an order/tracking location: NPC
// station and system names from the local caches, player
// structures from the structures snapshot when it named them,
// then the resolved structure-name cache (structures.go), and
// honest "#<id>" fallbacks otherwise.
func (app *Application) corpLocationTitle(ctx context.Context, locationID int64, structureNames map[int64]string) string {
	if name, ok := app.esi.CachedPlaceName(ctx, locationID); ok {
		return name
	}
	if name, ok := structureNames[locationID]; ok {
		return name
	}
	if name := app.resolvedStructureTitle(ctx, locationID); name != "" {
		return name
	}
	// Upwell structure IDs live up around 1e12, far above the
	// station (6e7) and system (3e7) ranges.
	if locationID > 1_000_000_000 {
		return fmt.Sprintf("Structure #%d", locationID)
	}
	return fmt.Sprintf("Location #%d", locationID)
}
