package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Economy worker pass: keeps every
// economy snapshot warm for one character and warms contract item
// lists into contract_details.
//
// These endpoints take no in-game role, but a character whose SSO
// login predates the current scope list gets a 403; that is
// recorded in snapshot_fetch_state (state "error", detail naming
// the refusal) and backed off like the corporation role states,
// so the pages can say "sign in again" instead of warming
// forever. Other errors are recorded too and the pass moves on;
// only CCP's error limit (420/429) stops it.
// ---------------------------------------------------------------------------

// forbiddenDetailPrefix marks a recorded 403 in a fetch-state
// detail (the full text tells the user to re-link the character).
const forbiddenDetailPrefix = "ESI refused (403)"

// economySnapshotKinds lists the economy snapshot kinds in fetch
// order: wallet windows first, then orders, contracts, industry.
func economySnapshotKinds() []string {
	return builtInKinds([]string{
		esi.SnapWalletJournal, esi.SnapWalletTxns,
		esi.SnapOrders, esi.SnapOrdersHistory,
		esi.SnapContracts,
		esi.SnapIndustryJobs, esi.SnapBlueprints, esi.SnapMining,
	})
}

// refreshEconomySnapshots runs the economy pass for one character
// and reports how many snapshots it stored.
func (app *Application) refreshEconomySnapshots(ctx context.Context, ch db.Character) int {
	refreshed := 0
	for _, kind := range economySnapshotKinds() {
		if ctx.Err() != nil {
			return refreshed
		}
		switch app.fetchCharKind(ctx, ch, kind) {
		case corpFetchOK:
			refreshed++
		case corpFetchLimited:
			return refreshed
		}
	}
	return refreshed
}

// fetchCharKind refreshes one economy snapshot kind when it is
// due, recording the outcome like fetchCorpKind does.
func (app *Application) fetchCharKind(ctx context.Context, ch db.Character, kind string) corpFetchOutcome {
	snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	switch {
	case serr == nil && esi.SnapshotFresh(snap):
		return corpFetchSkipped
	case serr != nil && !errors.Is(serr, sql.ErrNoRows):
		logging.Errorf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
	}

	// A recorded 403 (stale-scope login) backs this kind off
	// instead of retrying every minute; the pages explain from
	// the record in the meantime.
	if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: kind}); err == nil &&
		state.State == fetchStateError && strings.HasPrefix(state.Detail, forbiddenDetailPrefix) {
		if time.Since(state.AttemptedAt) < roleMissingBackoff {
			return corpFetchSkipped
		}
	}

	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
		switch {
		case errors.Is(err, esi.ErrErrorLimit):
			logging.Warnf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
			return corpFetchLimited
		case esi.IsForbidden(err):
			detail := forbiddenDetailPrefix + " — this character's login predates the current scope list; sign in again to re-grant scopes."
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, detail)
			logging.Infof("worker: %s for character %d refused by ESI (403); recorded as a stale-scope login", kind, ch.CharacterID)
			return corpFetchFailed
		default:
			logging.Errorf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, err.Error())
			return corpFetchFailed
		}
	}
	app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateOK, "")
	return corpFetchOK
}

// ---------------------------------------------------------------------------
// Contract item warming. The contracts list is a snapshot; the
// item list behind each contract is immutable once posted, so the
// worker fills the contract_details store once per contract
// (bounded per cycle) and pages render from the store only — the
// killmail-detail pattern exactly.
// ---------------------------------------------------------------------------

// maxContractItemsPerCycle bounds item-list fetches per character
// per worker cycle.
const maxContractItemsPerCycle = 100

// warmContractItems fetches item lists for the contracts in the
// character's contracts snapshot that are not stored yet, at most
// maxContractItemsPerCycle of them. It reports how many were
// stored and whether ESI's error limit stopped the pass. Deleted
// contracts are skipped (their item endpoint is gone for good).
func (app *Application) warmContractItems(ctx context.Context, ch db.Character) (fetched int, limited bool) {
	payload, ok := app.snapshotPayload(ctx, ch.CharacterID, esi.SnapContracts)
	if !ok {
		return 0, false // no contracts list yet; nothing to warm
	}
	var contracts esi.Contracts
	if err := json.Unmarshal([]byte(payload), &contracts); err != nil {
		logging.Errorf("worker: warm contract items for character %d: decode contracts: %v", ch.CharacterID, err)
		return 0, false
	}

	// Which of these contracts already have their item list
	// stored, read in one query for the whole list instead of a
	// probe per contract. (Details are keyed by contract, so one
	// stored by any character's list counts for all of them.)
	ids := make([]int64, 0, len(contracts))
	for _, c := range contracts {
		if c.ContractID > 0 {
			ids = append(ids, c.ContractID)
		}
	}
	stored := map[int64]bool{}
	if len(ids) > 0 {
		storedIDs, err := app.queries.ListContractDetailIDsForContracts(ctx, ids)
		if err != nil {
			logging.Errorf("worker: warm contract items for character %d: list stored contract details: %v", ch.CharacterID, err)
			return 0, false
		}
		for _, id := range storedIDs {
			stored[id] = true
		}
	}

	for _, c := range contracts {
		if fetched >= maxContractItemsPerCycle {
			break
		}
		if c.ContractID <= 0 || c.Status == "deleted" {
			continue
		}
		if stored[c.ContractID] {
			continue // already stored (by any character's list)
		}

		body, err := app.esi.FetchContractItems(ctx, ch, c.ContractID)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return fetched, true
			}
			if ctx.Err() == nil {
				logging.Errorf("worker: contract items %d for character %d: %v", c.ContractID, ch.CharacterID, err)
			}
			continue
		}
		if err := app.queries.UpsertContractDetail(ctx, db.UpsertContractDetailParams{
			ContractID:  c.ContractID,
			CharacterID: ch.CharacterID,
			Payload:     string(body),
			FetchedAt:   time.Now().UTC(),
		}); err != nil {
			logging.Errorf("worker: store contract items %d for character %d: %v", c.ContractID, ch.CharacterID, err)
			continue
		}
		stored[c.ContractID] = true
		fetched++
	}
	return fetched, false
}
