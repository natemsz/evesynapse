package app

// ---------------------------------------------------------------------------
// Public contracts (schema 055): the worker mirrors ESI's
// public contract routes -- the list for every region, each
// contract's item list exactly once, live auctions' bids --
// into pub_contracts and its satellites on the routes' own
// 30-minute cache cadence. Pages read the stored rows; nothing
// here fetches at render.
//
// The list sweep follows the region book sweep's shape:
// staged and resumable per region (pub_contract_sweep_state
// carries the page cursor), each fetched page and the cursor
// advance in one transaction, and a restart resumes mid-list
// instead of at page 1. A region's sweep completes when its
// last page lands; completion is what retires contracts --
// expiry passed, or absent from two consecutive completed
// sweeps (one absence can be a torn read while ESI republishes
// its cache). Departed contracts take their items and bids
// with them; abyssal sightings keep the departure on record
// (pub_contracts_warm.go keeps the corpus itself).
//
// Heat decides the sweep's order, never its coverage: hub
// regions walk first, then every other region in id order,
// and a lone deployment still covers the whole universe each
// cadence -- a full pass is ~155 page fetches per window.
// Every fetch kind rides the market pass's bookkeeping
// (marketFetchDue / recordMarketFetch), and the pass spends
// from its own small fetch lane so a contract burst can
// neither starve nor be starved by the market sweeps.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	// pubContractSweepGate is the list route's own cache
	// window, measured on the live route: a region swept more
	// recently than this has nothing new to say.
	pubContractSweepGate = 30 * time.Minute
	// pubContractFetchesPerCycle is the pass's own fetch lane:
	// room for a full universe pass every cadence (~155 list
	// pages per 30 minutes is ~6 per minute-cycle) with the
	// item, bid and probe warming below sharing what is left.
	pubContractFetchesPerCycle = 240
	// pubContractMaxPagesTotal is a safety stop far beyond
	// any real region (The Forge runs ~35 pages): a region
	// reporting more is treated as complete at the stop
	// rather than paged forever.
	pubContractMaxPagesTotal = 200
)

// pubContractRegionKind names a region's sweep row in
// market_fetch_state.
func pubContractRegionKind(regionID int64) string {
	return fmt.Sprintf("pubcontracts_%d", regionID)
}

// refreshPublicContracts is the pass, called from
// refreshPublicData with its own allowance. It advances every
// due region's list sweep as far as the allowance reaches,
// then warms item lists, auction bids and dogma probes from
// what is left (pub_contracts_warm.go). It reports how many
// fetches landed data, and whether ESI's error limit cut the
// pass short.
func (app *Application) refreshPublicContracts(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	app.pubContractMu.Lock()
	defer app.pubContractMu.Unlock()

	regionIDs, err := app.queries.ListSDERegionIDs(ctx)
	if err != nil {
		logging.Errorf("worker: public contracts: list regions: %v", err)
		return 0, false
	}
	if len(regionIDs) == 0 {
		return 0, false // no static data yet; the SDE import comes first
	}
	structures, ltd := app.refreshPublicStructures(ctx, allowance)
	stored += structures
	if ltd {
		return stored, true
	}
	for _, regionID := range pubContractRegionOrder(regionIDs) {
		if ctx.Err() != nil {
			return stored, false // shutting down: no sense asking every region's state
		}
		pages, ltd := app.advancePublicContractRegion(ctx, regionID, allowance)
		stored += pages
		if ltd {
			return stored, true
		}
		if allowance.Exhausted() {
			return stored, false
		}
	}

	items, ltd := app.warmPublicContractItems(ctx, allowance)
	stored += items
	if ltd {
		return stored, true
	}
	bids, ltd := app.warmPublicContractBids(ctx, allowance)
	stored += bids
	if ltd {
		return stored, true
	}
	probes, ltd := app.warmAbyssalProbes(ctx, allowance)
	stored += probes
	if ltd {
		return stored, true
	}
	sales, ltd := app.probeAbyssalSales(ctx, allowance)
	stored += sales
	return stored, ltd
}

// publicStructuresKind names the public structures route
// in market_fetch_state.
const publicStructuresKind = "pubcontracts_public_structures"

// refreshPublicStructures mirrors ESI's public structures
// route -- the player structures anyone may dock at --
// into public_structures (schema 068), one replace per
// cadence window, stamped. The contract search's
// un-dockable-citadel filter reads the stored set; nothing
// fetches at render. A failed or empty fetch leaves the
// stored set untouched: a torn read must never unpublish
// every public citadel at once.
func (app *Application) refreshPublicStructures(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	if !app.marketFetchDue(ctx, publicStructuresKind, pubContractSweepGate) {
		return 0, false
	}
	if !allowance.Take() {
		return 0, false
	}
	body, _, err := app.esi.FetchRaw(ctx, "", "/universe/structures/")
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return 0, true
		}
		app.recordMarketFetch(ctx, publicStructuresKind, marketFetchFailureState(err), "public structures fetch failed")
		return 0, false
	}
	var ids []int64
	if err := json.Unmarshal(body, &ids); err != nil || len(ids) == 0 {
		app.recordMarketFetch(ctx, publicStructuresKind, fetchStateError, "public structures decode failed")
		return 0, false
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("worker: public structures: begin: %v", err)
		return 0, false
	}
	defer tx.Rollback()
	if err := db.New(tx).ReplacePublicStructures(ctx, db.ReplacePublicStructuresParams{
		Ids: ids, RefreshedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("worker: public structures: store: %v", err)
		return 0, false
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("worker: public structures: commit: %v", err)
		return 0, false
	}
	app.recordMarketFetch(ctx, publicStructuresKind, fetchStateOK, "")
	return 1, false
}

// pubContractRegionOrder puts the five market hub regions
// first, in marketRegions order, and every other region after
// them in id order: where users trade is where contracts
// matter first, and the long tail still walks every pass.
func pubContractRegionOrder(regionIDs []int64) []int64 {
	hubRank := make(map[int64]int, len(marketRegions))
	for i, r := range marketRegions {
		hubRank[r.ID] = i
	}
	ordered := append([]int64(nil), regionIDs...)
	sort.Slice(ordered, func(i, j int) bool {
		ri, iHub := hubRank[ordered[i]]
		rj, jHub := hubRank[ordered[j]]
		switch {
		case iHub && jHub:
			return ri < rj
		case iHub:
			return true
		case jHub:
			return false
		default:
			return ordered[i] < ordered[j]
		}
	})
	return ordered
}

// advancePublicContractRegion moves one region's list sweep
// forward under its stored cursor. An idle region starts only
// when its last completed sweep is older than the route's
// cache window; a sweep between pages resumes regardless of
// that gate. Each page is stored with the cursor advance in
// one transaction; the final page's transaction also runs the
// completion step (retirements, sighting refreshes, the cycle
// stamp), so a region is never half-swept in a reader's view.
func (app *Application) advancePublicContractRegion(ctx context.Context, regionID int64, allowance *fetchBudget) (pages int, limited bool) {
	kind := pubContractRegionKind(regionID)
	state, err := app.queries.GetPublicContractSweepState(ctx, regionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("worker: public contracts: sweep state %d: %v", regionID, err)
		return 0, false
	}
	if state.NextPage == 0 {
		if !app.marketFetchDue(ctx, kind, pubContractSweepGate) {
			return 0, false
		}
		state = db.PubContractSweepState{
			RegionID: regionID, NextPage: 1, TotalPages: 0,
			Cycle: state.Cycle, UpdatedAt: time.Now().UTC(),
		}
		if err := app.queries.UpsertPublicContractSweepState(ctx, db.UpsertPublicContractSweepStateParams(state)); err != nil {
			logging.Errorf("worker: public contracts: start sweep %d: %v", regionID, err)
			return 0, false
		}
	}
	cycle := state.Cycle + 1

	for state.NextPage >= 1 {
		if state.TotalPages > 0 && state.NextPage > state.TotalPages {
			// A previous pass stored its last page but stopped
			// before the completion step; finish it now.
			if err := app.completePublicContractRegion(ctx, regionID, state, cycle); err != nil {
				logging.Errorf("worker: public contracts: complete %d: %v", regionID, err)
				return pages, false
			}
			app.recordMarketFetch(ctx, kind, fetchStateOK, "")
			return pages, false
		}
		if !allowance.Take() {
			return pages, false // budget spent; the cursor resumes next cycle
		}
		rows, totalPages, err := app.fetchPublicContractPage(ctx, regionID, state.NextPage)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return pages, true
			}
			app.recordMarketFetch(ctx, kind, marketFetchFailureState(err), "contract list page failed")
			return pages, false
		}
		if totalPages > pubContractMaxPagesTotal {
			totalPages = pubContractMaxPagesTotal
		}
		state.TotalPages = int64(totalPages)
		last := state.NextPage >= state.TotalPages
		if err := app.storePublicContractPage(ctx, regionID, state, cycle, rows, last); err != nil {
			logging.Errorf("worker: public contracts: store page %d/%d: %v", regionID, state.NextPage, err)
			return pages, false
		}
		pages++
		if last {
			app.recordMarketFetch(ctx, kind, fetchStateOK, "")
			return pages, false
		}
		state.NextPage++
	}
	return pages, false
}

// fetchPublicContractPage reads one page of a region's public
// contract list and reports the total page count from the
// X-Pages header. Auth-free public data, like the region book
// fetches.
func (app *Application) fetchPublicContractPage(ctx context.Context, regionID, page int64) ([]esi.PublicContract, int, error) {
	path := fmt.Sprintf("/contracts/public/%d/?page=%d", regionID, page)
	body, header, err := app.esi.FetchRaw(ctx, "", path)
	if err != nil {
		return nil, 0, err
	}
	totalPages := 1
	if raw := header.Get("X-Pages"); raw != "" {
		var n int
		if _, serr := fmt.Sscanf(raw, "%d", &n); serr == nil && n > 0 {
			totalPages = n
		}
	}
	var rows []esi.PublicContract
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, 0, fmt.Errorf("public contract page: decode: %w", err)
	}
	return rows, totalPages, nil
}

// storePublicContractPage stores one fetched page and the
// cursor advance in one transaction. On the sweep's final
// page the same transaction runs the completion step, so the
// new cycle and its retirements land atomically.
func (app *Application) storePublicContractPage(ctx context.Context, regionID int64, state db.PubContractSweepState, cycle int64, rows []esi.PublicContract, last bool) error {
	now := time.Now().UTC()
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin page store: %w", err)
	}
	defer tx.Rollback()
	q := db.New(tx)

	if page := publicContractPageParams(regionID, cycle, now, rows); len(page.ContractIds) > 0 {
		if err := q.UpsertPublicContracts(ctx, page); err != nil {
			return fmt.Errorf("upsert %d contracts: %w", len(page.ContractIds), err)
		}
	}

	nextPage := state.NextPage + 1
	newCycle := state.Cycle
	if last {
		nextPage = 0
		newCycle = cycle
		if err := completePublicContractRegionTx(ctx, q, regionID, cycle, now); err != nil {
			return err
		}
	}
	if err := q.UpsertPublicContractSweepState(ctx, db.UpsertPublicContractSweepStateParams{
		RegionID: regionID, NextPage: nextPage, TotalPages: state.TotalPages,
		Cycle: newCycle, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("advance sweep cursor %d: %w", regionID, err)
	}
	return tx.Commit()
}

// publicContractPageParams is one list page as the columns of one
// upsert. A row without its dates cannot be placed in time and is
// skipped; a contract the page lists twice is kept once, its last
// listing, since one statement may not write the same row twice.
func publicContractPageParams(regionID, cycle int64, now time.Time, rows []esi.PublicContract) db.UpsertPublicContractsParams {
	p := db.UpsertPublicContractsParams{RegionID: regionID, LastCycle: cycle, Seen: now}
	type contractRow struct {
		esi.PublicContract
		issued, expired time.Time
	}
	at := make(map[int64]int, len(rows))
	var kept []contractRow
	for _, row := range rows {
		issued, err := time.Parse(time.RFC3339, row.DateIssued)
		if err != nil {
			continue
		}
		expired, err := time.Parse(time.RFC3339, row.DateExpired)
		if err != nil {
			continue
		}
		r := contractRow{row, issued, expired}
		if i, seen := at[row.ContractID]; seen {
			kept[i] = r
			continue
		}
		at[row.ContractID] = len(kept)
		kept = append(kept, r)
	}
	for _, r := range kept {
		p.ContractIds = append(p.ContractIds, r.ContractID)
		p.Types = append(p.Types, r.Type)
		p.IssuerIds = append(p.IssuerIds, r.IssuerID)
		p.IssuerCorporationIds = append(p.IssuerCorporationIds, r.IssuerCorporationID)
		p.ForCorporations = append(p.ForCorporations, r.ForCorporation)
		p.Titles = append(p.Titles, r.Title)
		p.Prices = append(p.Prices, r.Price)
		p.Buyouts = append(p.Buyouts, r.Buyout)
		p.Rewards = append(p.Rewards, r.Reward)
		p.Collaterals = append(p.Collaterals, r.Collateral)
		p.Volumes = append(p.Volumes, r.Volume)
		p.StartLocationIds = append(p.StartLocationIds, r.StartLocationID)
		p.EndLocationIds = append(p.EndLocationIds, r.EndLocationID)
		p.DatesIssued = append(p.DatesIssued, r.issued)
		p.DatesExpired = append(p.DatesExpired, r.expired)
		p.DaysToComplete = append(p.DaysToComplete, r.DaysToComplete)
	}
	return p
}

// completePublicContractRegion finishes a sweep whose pages
// all landed in an earlier pass (its completion step was
// interrupted): the retirement step and the cycle stamp, in
// one transaction.
func (app *Application) completePublicContractRegion(ctx context.Context, regionID int64, state db.PubContractSweepState, cycle int64) error {
	now := time.Now().UTC()
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin completion: %w", err)
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err := completePublicContractRegionTx(ctx, q, regionID, cycle, now); err != nil {
		return err
	}
	if err := q.UpsertPublicContractSweepState(ctx, db.UpsertPublicContractSweepStateParams{
		RegionID: regionID, NextPage: 0, TotalPages: state.TotalPages,
		Cycle: cycle, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("stamp sweep cycle %d: %w", regionID, err)
	}
	return tx.Commit()
}

// completePublicContractRegionTx is a completed sweep's
// retirement step, inside the caller's transaction: contracts
// whose expiry passed, and contracts unseen in this cycle and
// the one before it, leave the live tables -- their bids and
// items go with them, and any abyssal sightings on them are
// stamped with how they left (expired, or vanished early: the
// record carries the fact, never a guess at accepted versus
// cancelled). The sightings of everything this cycle saw are
// then refreshed from the stored rows.
func completePublicContractRegionTx(ctx context.Context, q *db.Queries, regionID, cycle int64, now time.Time) error {
	departed, err := q.ListDepartedPublicContracts(ctx, db.ListDepartedPublicContractsParams{
		RegionID: regionID, DateExpired: now, LastCycle: cycle - 2,
	})
	if err != nil {
		return fmt.Errorf("list departed %d: %w", regionID, err)
	}
	if len(departed) > 0 {
		byKind := map[string][]int64{}
		all := make([]int64, 0, len(departed))
		for _, d := range departed {
			byKind[d.DepartKind] = append(byKind[d.DepartKind], d.ContractID)
			all = append(all, d.ContractID)
		}
		for kind, ids := range byKind {
			if err := q.StampAbyssalSightingsDeparted(ctx, db.StampAbyssalSightingsDepartedParams{
				DepartedAt: now, DepartKind: kind, ContractIds: ids,
			}); err != nil {
				return fmt.Errorf("stamp sightings departed %d: %w", regionID, err)
			}
		}
		if err := q.DeletePublicContractBidsByContracts(ctx, all); err != nil {
			return fmt.Errorf("delete departed bids %d: %w", regionID, err)
		}
		if err := q.DeletePublicContractItemsByContracts(ctx, all); err != nil {
			return fmt.Errorf("delete departed items %d: %w", regionID, err)
		}
		if err := q.DeletePublicContractsByIDs(ctx, all); err != nil {
			return fmt.Errorf("delete departed contracts %d: %w", regionID, err)
		}
	}
	if err := q.RefreshAbyssalSightingsForRegionCycle(ctx, db.RefreshAbyssalSightingsForRegionCycleParams{
		RegionID: regionID, LastCycle: cycle, LastSeenAt: now,
	}); err != nil {
		return fmt.Errorf("refresh sightings %d: %w", regionID, err)
	}
	// Attribution rides every sighting write (schema 069): what
	// else shares each listing's contract, while it is stored.
	if err := q.StampAbyssalSightingAttribution(ctx, db.StampAbyssalSightingAttributionParams{
		RegionID: sql.NullInt64{Int64: regionID, Valid: true}, Cycle: sql.NullInt64{Int64: cycle, Valid: true},
	}); err != nil {
		return fmt.Errorf("stamp sightings %d: %w", regionID, err)
	}
	return nil
}
