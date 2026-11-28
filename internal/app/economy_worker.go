package app

import (
	"context"
	"errors"
	json "github.com/goccy/go-json"
	"sync"
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
// login predates the current scope list is refused (ESI answers
// 403 for some scope failures, 401 for others); the refusal is
// recorded in snapshot_fetch_state under the shared refusal
// policy (fetch_refusal.go) and backed off, so the pages can say
// "sign in again" instead of warming forever. Other errors are
// recorded too and the pass moves on; only CCP's error limit
// (420/429) stops it.
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
		esi.SnapLoyalty,
	})
}

// refreshEconomySnapshots runs the economy pass for one character
// and reports how many snapshots it stored.
func (app *Application) refreshEconomySnapshots(ctx context.Context, ch db.Character) int {
	refreshed := 0
	granted := scopeSet(ch.Scopes)
	for _, kind := range economySnapshotKinds() {
		if ctx.Err() != nil {
			return refreshed
		}
		if kindExcludedByGrants(granted, kind) {
			continue // the login never granted this kind's scope: not planned
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
// due, with no cycle allowance.
func (app *Application) fetchCharKind(ctx context.Context, ch db.Character, kind string) corpFetchOutcome {
	return app.fetchSnapshotKind(ctx, ch, kind, nil)
}

// ---------------------------------------------------------------------------
// Contract item warming. The contracts list is a snapshot; the
// item list behind each contract is immutable once posted, so the
// worker fills the contract_details store once per contract
// (bounded per cycle) and pages render from the store only, the
// killmail-detail pattern exactly.
// ---------------------------------------------------------------------------

// maxContractItemsPerCycle bounds item-list fetches per character
// per worker cycle.
const maxContractItemsPerCycle = 100

// corpContractWarmEvery is how often one corporation's contract item
// lists are warmed: one character's pass, however many of the
// corporation's characters are linked. ESI throttles the corporation
// items endpoint (520 ConStopSpamming) when several characters each
// spend a full per-character allowance on it in the same minute.
const corpContractWarmEvery = time.Minute

// corpContractWarmHold is how long a corporation's item warming rests
// after its items endpoint answers with a server error such as 520
// ConStopSpamming, a throttle that lasts hours: asking again sooner
// only spends ESI error budget.
const corpContractWarmHold = time.Hour

// corpWarmGate records, per corporation, when its contract item
// lists may next be warmed. The zero value is ready to use.
type corpWarmGate struct {
	mu   sync.Mutex
	next map[int64]time.Time
}

// claim reports whether a character may warm corpID's item lists
// now, and if so keeps every other character off it for
// corpContractWarmEvery.
func (g *corpWarmGate) claim(corpID int64, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.next[corpID]) {
		return false
	}
	if g.next == nil {
		g.next = map[int64]time.Time{}
	}
	g.next[corpID] = now.Add(corpContractWarmEvery)
	return true
}

// hold keeps every character off corpID's item lists until until.
func (g *corpWarmGate) hold(corpID int64, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.next == nil {
		g.next = map[int64]time.Time{}
	}
	g.next[corpID] = until
}

// contractItemsErrorBudgetReserve is the remaining ESI error budget
// under which contract item warming stops for the cycle: the item
// lists are a warm-up and wait, core sync keeps the budget.
const contractItemsErrorBudgetReserve = 40

// warmContractItems fetches item lists for the contracts in the
// character's contracts snapshot that are not stored yet, at most
// maxContractItemsPerCycle of them. It reports how many were
// stored and whether ESI's error limit stopped the pass. Deleted
// contracts are skipped (their item endpoint is gone for good).
func (app *Application) warmContractItems(ctx context.Context, ch db.Character) (fetched int, limited bool) {
	return app.warmContractItemsOf(ctx, ch, esi.SnapContracts, 0, func(contractID int64) ([]byte, error) {
		return app.esi.FetchContractItems(ctx, ch, contractID)
	})
}

// warmCorpContractItems is warmContractItems for the contracts of the
// character's corporation (the corp_contracts snapshot), read through
// the corporation's items endpoint into the same store. One character
// at a time warms a corporation (corpWarmGate).
func (app *Application) warmCorpContractItems(ctx context.Context, ch db.Character) (fetched int, limited bool) {
	corpID, ok := app.corpIDForCharacter(ctx, ch)
	if !ok {
		return 0, false
	}
	return app.warmContractItemsOf(ctx, ch, esi.SnapCorpContracts, corpID, func(contractID int64) ([]byte, error) {
		return app.esi.FetchCorpContractItems(ctx, ch, corpID, contractID)
	})
}

// warmContractItemsOf warms the item lists behind one stored
// contracts snapshot kind, fetching each through fetch. With a
// corpID the lists are the corporation's: the pass first claims the
// corporation on app.corpContractWarm, and a server error holds it.
func (app *Application) warmContractItemsOf(ctx context.Context, ch db.Character, kind string, corpID int64, fetch func(contractID int64) ([]byte, error)) (fetched int, limited bool) {
	if kindExcludedByGrants(scopeSet(ch.Scopes), kind) {
		return 0, false // the login never granted the contracts scope: not planned
	}
	if app.refusalSettled(ctx, ch.CharacterID, kind) {
		return 0, false // the contracts login is refused; item lists would be too
	}
	payload, ok := app.snapshotPayload(ctx, ch.CharacterID, kind)
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

	// attempts counts every request, failed ones included: each
	// failure spends ESI error budget, so the cap bounds those too.
	attempts := 0
	for _, c := range contracts {
		if attempts >= maxContractItemsPerCycle {
			break
		}
		if c.ContractID <= 0 || c.Status == "deleted" {
			continue
		}
		if stored[c.ContractID] {
			continue // already stored (by any character's list)
		}
		if app.esiErrorBudgetBelow(contractItemsErrorBudgetReserve) {
			return fetched, false // item lists wait; core sync keeps the budget
		}
		// Claimed only once there is something to fetch, so a
		// character with nothing to do never keeps another off it.
		if corpID != 0 && attempts == 0 && !app.corpContractWarm.claim(corpID, time.Now()) {
			return fetched, false // another character warms this corporation this cycle
		}

		attempts++
		body, err := fetch(c.ContractID)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return fetched, true
			}
			if app.recordFetchRefusal(ctx, ch.CharacterID, kind, err) {
				return fetched, false // the login is refused; every remaining item would be too
			}
			if ctx.Err() == nil {
				logging.Errorf("worker: contract items %d for character %d: %v", c.ContractID, ch.CharacterID, err)
			}
			if code, has := esi.StatusCode(err); has && code >= 500 {
				// A server-side answer (520 ConStopSpamming when
				// one character asks too fast, or ESI trouble):
				// the next contract would get the same one, and
				// each costs error budget. Try again next cycle, or
				// for a corporation, once its hold is over: every
				// character in it would get the same answer.
				if corpID != 0 {
					app.corpContractWarm.hold(corpID, time.Now().Add(corpContractWarmHold))
				}
				return fetched, false
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
