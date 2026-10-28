package app

// ---------------------------------------------------------------------------
// Public contracts, the warming passes (schema 055): what the
// list sweep leaves behind, filled in from the same allowance.
//
// Item lists are fetched once per contract, ever -- a
// contract's list is immutable once issued, so the fetch is
// pure history-keeping. The drain walks newest first, so a
// fresh contract is appraised within a cycle or two and the
// cold backlog backfills behind it. A list that keeps failing
// -- a decode error counts exactly like a fetch error -- is
// parked after a bounded run of failures (schema 060 keeps the
// count and the reason on the contract row) instead of being
// retried on every pass forever. Courier contracts refuse the
// route (their cargo is private): the refusal is recorded and
// never retried. Bids refresh for live auctions on the same
// 30-minute cadence as the list; a bid list is replace-on-
// read, small enough that nothing cleverer is warranted.
//
// Dogma probing is the abyssal corpus being born: every
// singleton item on a live contract is probed exactly once
// against ESI's dynamic-item route (a miss is an ordinary
// module, and costs error budget, so the outcome is stored
// either way and no instance is ever probed twice). A hit
// stores the instance's rolls forever and opens its sighting
// history (pub_contracts.go's completion step keeps the
// sightings of live listings current from there).
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	// pubContractItemsPerCycle bounds item-list fetches in
	// one pass: steady state is the new-contract rate, and
	// the first pass over a full corpus drains over a few
	// cycles instead of bursting.
	pubContractItemsPerCycle = 80
	// pubContractBidsPerCycle bounds auction bid refreshes in
	// one pass (auctions are ~1% of the corpus and ride the
	// list route's cadence through their own fetch-state
	// rows, so a pass only ever touches the due slice).
	pubContractBidsPerCycle = 60
	// pubContractProbesPerCycle bounds dogma probes in one
	// pass: paced on purpose, because every miss costs ESI
	// error budget.
	pubContractProbesPerCycle = 60
	// pubContractItemsMaxAttempts is the item drain's parking
	// threshold: a contract whose item list fails this many
	// times in a row (fetch, decode or store) is parked and
	// leaves the drain. Decode failures are deterministic --
	// the same payload fails the same way every pass -- so
	// the bound is small; three leaves room for a transient
	// fetch error or two to clear first.
	pubContractItemsMaxAttempts = 3
)

// pubContractBidsKind names an auction's bid-refresh row in
// market_fetch_state.
func pubContractBidsKind(contractID int64) string {
	return fmt.Sprintf("pubbids_%d", contractID)
}

// warmPublicContractItems fetches the item lists of contracts
// whose lists were never fetched, newest first. One landing
// (or one definitive refusal) per contract, ever; a list that
// keeps failing is parked at pubContractItemsMaxAttempts.
func (app *Application) warmPublicContractItems(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	pending, err := app.queries.ListPublicContractsPendingItems(ctx, pubContractItemsPerCycle)
	if err != nil {
		logging.Errorf("worker: public contracts: pending items: %v", err)
		return 0, false
	}
	paths := make([]string, len(pending))
	for i, pc := range pending {
		paths[i] = fmt.Sprintf("/contracts/public/items/%d/", pc.ContractID)
	}
	answers, spent := app.fetchPublicWindows(ctx, paths, allowance)
	for i, pc := range pending[:spent] {
		body, err := answers[i].body, answers[i].err
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return stored, true
			}
			if code, ok := esi.StatusCode(err); ok {
				switch code {
				case http.StatusBadRequest:
					// Courier cargo is private by design: the
					// refusal is the answer, recorded once.
					app.setPublicContractItemsState(ctx, pc.ContractID, "private")
					stored++
					continue
				case http.StatusNotFound:
					// The contract ended before its list was
					// ever fetched; it leaves with the sweep.
					app.setPublicContractItemsState(ctx, pc.ContractID, "gone")
					stored++
					continue
				}
			}
			// Any other refusal is a failed attempt on the
			// record: below the threshold the contract stays
			// pending and is retried next pass; at it, the
			// note parks it.
			app.notePublicContractItemsFailure(ctx, pc.ContractID, "fetch: "+err.Error())
			continue
		}
		var items []esi.PublicContractItem
		if err := json.Unmarshal(body, &items); err != nil {
			logging.Errorf("worker: public contracts: items %d: decode: %v", pc.ContractID, err)
			app.notePublicContractItemsFailure(ctx, pc.ContractID, "decode: "+err.Error())
			continue
		}
		if err := app.storePublicContractItems(ctx, pc.ContractID, items); err != nil {
			logging.Errorf("worker: public contracts: items %d: store: %v", pc.ContractID, err)
			app.notePublicContractItemsFailure(ctx, pc.ContractID, "store: "+err.Error())
			continue
		}
		stored++
	}
	return stored, false
}

// pubContractFetchWindow is how many of the warming passes' reads
// are in flight at once. ESI's rate budgets are still enforced in
// the client (esi/ratelimit.go); the window only stops the reads
// waiting on one another.
const pubContractFetchWindow = 6

// publicAnswer is one read of a window: its body, or why not.
type publicAnswer struct {
	body []byte
	err  error
}

// errPublicReadLost stands for a read whose goroutine panicked.
var errPublicReadLost = errors.New("read did not finish")

// fetchPublicWindows reads auth-free paths pubContractFetchWindow at
// a time, each spending one fetch from allowance, and answers them in
// path order. It stops at the first window that ran out of allowance
// or met ESI's error limit; spent is how many paths were read (the
// first spent answers), which the caller walks in order as it would
// have walked one read after another.
func (app *Application) fetchPublicWindows(ctx context.Context, paths []string, allowance *fetchBudget) (answers []publicAnswer, spent int) {
	for start := 0; start < len(paths) && ctx.Err() == nil; start += pubContractFetchWindow {
		end := min(start+pubContractFetchWindow, len(paths))
		n := 0
		for start+n < end && allowance.Take() {
			n++
		}
		window := make([]publicAnswer, n)
		var wg sync.WaitGroup
		for i := range window {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer recoverWorkerPanic("public contract read")
				window[i].err = errPublicReadLost
				window[i].body, _, window[i].err = app.esi.FetchRaw(ctx, "", paths[start+i])
			}()
		}
		wg.Wait()
		answers = append(answers, window...)
		if n < end-start {
			break // allowance spent
		}
		for _, a := range window {
			if errors.Is(a.err, esi.ErrErrorLimit) {
				return answers, len(answers)
			}
		}
	}
	return answers, len(answers)
}

// storePublicContractItems stores one contract's item list
// and marks it fetched, in one transaction.
func (app *Application) storePublicContractItems(ctx context.Context, contractID int64, items []esi.PublicContractItem) error {
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin items store: %w", err)
	}
	defer tx.Rollback()
	q := db.New(tx)
	for _, item := range items {
		if err := q.InsertPublicContractItem(ctx, db.InsertPublicContractItemParams{
			RecordID: item.RecordID, ContractID: contractID, TypeID: item.TypeID,
			ItemID:             nullInt64(item.ItemID),
			Quantity:           item.Quantity,
			IsIncluded:         item.IsIncluded,
			IsBlueprintCopy:    item.IsBlueprintCopy,
			Runs:               nullInt64Ptr(item.Runs),
			MaterialEfficiency: nullInt64Ptr(item.MaterialEfficiency),
			TimeEfficiency:     nullInt64Ptr(item.TimeEfficiency),
		}); err != nil {
			return fmt.Errorf("insert item %d: %w", item.RecordID, err)
		}
	}
	if err := q.SetPublicContractItemsState(ctx, db.SetPublicContractItemsStateParams{
		ContractID: contractID, ItemsState: "stored",
	}); err != nil {
		return fmt.Errorf("mark items stored %d: %w", contractID, err)
	}
	// A known abyssal instance on this list opens its sighting
	// for the listing now: a relist never re-probes, so this
	// is the moment its new listing enters the history.
	if err := q.OpenAbyssalSightingsForContract(ctx, db.OpenAbyssalSightingsForContractParams{
		ContractID: contractID, LastSeenAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("open sightings %d: %w", contractID, err)
	}
	if err := q.StampAbyssalSightingAttribution(ctx, db.StampAbyssalSightingAttributionParams{
		ContractID: sql.NullInt64{Int64: contractID, Valid: true},
	}); err != nil {
		return fmt.Errorf("stamp sightings %d: %w", contractID, err)
	}
	return tx.Commit()
}

func (app *Application) setPublicContractItemsState(ctx context.Context, contractID int64, state string) {
	if err := app.queries.SetPublicContractItemsState(ctx, db.SetPublicContractItemsStateParams{
		ContractID: contractID, ItemsState: state,
	}); err != nil {
		logging.Errorf("worker: public contracts: items state %d: %v", contractID, err)
	}
}

// notePublicContractItemsFailure records one failed item-list
// attempt (schema 060): the counter and the reason land on the
// contract row, and at pubContractItemsMaxAttempts the row
// parks itself out of the drain. The parking is visible in
// the log exactly once per contract: the pass that crosses
// the threshold.
func (app *Application) notePublicContractItemsFailure(ctx context.Context, contractID int64, reason string) {
	if err := app.queries.NotePublicContractItemsFailure(ctx, db.NotePublicContractItemsFailureParams{
		ContractID: contractID, Reason: reason, MaxAttempts: pubContractItemsMaxAttempts,
	}); err != nil {
		logging.Errorf("worker: public contracts: items failure note %d: %v", contractID, err)
		return
	}
	ledger, err := app.queries.GetPublicContractItemsLedger(ctx, contractID)
	if err == nil && ledger.ItemsState == "parked" && ledger.ItemsAttempts == pubContractItemsMaxAttempts {
		logging.Errorf("worker: public contracts: items %d: parked after %d failed attempts (last: %s)",
			contractID, ledger.ItemsAttempts, ledger.ItemsError)
	}
}

func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}

func nullInt64Ptr(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// warmPublicContractBids refreshes the bid lists of live
// auctions whose refresh is due, soonest-ending first. Bids
// are anonymous in ESI and stored as they arrive: an amount
// and a time, nothing else exists to store.
func (app *Application) warmPublicContractBids(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	now := time.Now().UTC()
	auctions, err := app.queries.ListPublicContractLiveAuctions(ctx, db.ListPublicContractLiveAuctionsParams{
		DateExpired: now, Limit: pubContractBidsPerCycle * 4,
	})
	if err != nil {
		logging.Errorf("worker: public contracts: live auctions: %v", err)
		return 0, false
	}
	var due []int64
	var paths []string
	for _, contractID := range auctions {
		if len(due) >= pubContractBidsPerCycle {
			break
		}
		if !app.marketFetchDue(ctx, pubContractBidsKind(contractID), pubContractSweepGate) {
			continue
		}
		due = append(due, contractID)
		paths = append(paths, fmt.Sprintf("/contracts/public/bids/%d/", contractID))
	}
	answers, spent := app.fetchPublicWindows(ctx, paths, allowance)
	for i, contractID := range due[:spent] {
		kind := pubContractBidsKind(contractID)
		body, err := answers[i].body, answers[i].err
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return stored, true
			}
			// A finished auction answers 403 "Contract not public", which
			// settles dead like a 404 (marketFetchFailureState).
			app.recordMarketFetch(ctx, kind, marketFetchFailureState(err), "contract bids failed")
			continue
		}
		var bids []esi.PublicContractBid
		if err := json.Unmarshal(body, &bids); err != nil {
			logging.Errorf("worker: public contracts: bids %d: decode: %v", contractID, err)
			// An empty or torn answer waits out the error gate like a
			// failed fetch, instead of being asked again every cycle.
			app.recordMarketFetch(ctx, kind, fetchStateError, "contract bids decode failed")
			continue
		}
		if err := app.storePublicContractBids(ctx, contractID, bids); err != nil {
			logging.Errorf("worker: public contracts: bids %d: store: %v", contractID, err)
			continue
		}
		app.recordMarketFetch(ctx, kind, fetchStateOK, "")
		stored++
	}
	return stored, false
}

// storePublicContractBids replaces one auction's bid list:
// ESI returns the whole list each time, and a stored list
// that lags the route's by a bid is worse than one replaced.
func (app *Application) storePublicContractBids(ctx context.Context, contractID int64, bids []esi.PublicContractBid) error {
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bids store: %w", err)
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err := q.DeletePublicContractBids(ctx, contractID); err != nil {
		return fmt.Errorf("clear bids %d: %w", contractID, err)
	}
	for _, bid := range bids {
		at, err := time.Parse(time.RFC3339, bid.DateBid)
		if err != nil {
			continue
		}
		if err := q.InsertPublicContractBid(ctx, db.InsertPublicContractBidParams{
			BidID: bid.BidID, ContractID: contractID, Amount: bid.Amount, DateBid: at,
		}); err != nil {
			return fmt.Errorf("insert bid %d: %w", bid.BidID, err)
		}
	}
	return tx.Commit()
}

// warmAbyssalProbes probes singleton items no probe has
// ever touched whose type a mutaplasmid can produce, newest
// listing first. Every outcome is stored: a dynamic hit
// opens the instance's corpus row and its sighting for the
// listing that surfaced it; a 404 means the instance is
// gone or was never dynamic, and is never asked about again.
func (app *Application) warmAbyssalProbes(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	candidates, err := app.queries.ListAbyssalProbeCandidates(ctx, pubContractProbesPerCycle)
	if err != nil {
		logging.Errorf("worker: public contracts: probe candidates: %v", err)
		return 0, false
	}
	now := time.Now().UTC()
	for _, cand := range candidates {
		if !cand.ItemID.Valid {
			continue
		}
		if !allowance.Take() {
			return stored, false
		}
		itemID := cand.ItemID.Int64
		path := fmt.Sprintf("/dogma/dynamic/items/%d/%d/", cand.TypeID, itemID)
		body, _, err := app.esi.FetchRaw(ctx, "", path)
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return stored, true
			}
			if code, ok := esi.StatusCode(err); ok && code == http.StatusNotFound {
				if err := app.queries.InsertAbyssalProbe(ctx, db.InsertAbyssalProbeParams{
					ItemID: itemID, TypeID: cand.TypeID, IsDynamic: false, ProbedAt: now,
				}); err != nil {
					logging.Errorf("worker: public contracts: store probe %d: %v", itemID, err)
					continue
				}
				stored++
			}
			continue // anything else is transient: no probe row, retried later
		}
		var dyn esi.DynamicItem
		if err := json.Unmarshal(body, &dyn); err != nil {
			logging.Errorf("worker: public contracts: probe %d: decode: %v", itemID, err)
			continue
		}
		attrs, err := json.Marshal(dyn.DogmaAttributes)
		if err != nil {
			logging.Errorf("worker: public contracts: probe %d: encode rolls: %v", itemID, err)
			continue
		}
		if err := app.storeAbyssalProbeHit(ctx, cand, dyn, attrs, now); err != nil {
			logging.Errorf("worker: public contracts: probe %d: store: %v", itemID, err)
			continue
		}
		stored++
	}
	return stored, false
}

// storeAbyssalProbeHit stores one dynamic item: the probe
// outcome, the instance's rolls (first fetch wins; the rolls
// never change), and the opening sighting for the listing
// that surfaced it -- its asking price, region and location,
// first seen when the listing was.
func (app *Application) storeAbyssalProbeHit(ctx context.Context, cand db.ListAbyssalProbeCandidatesRow, dyn esi.DynamicItem, attrs []byte, now time.Time) error {
	itemID := cand.ItemID.Int64
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin probe store: %w", err)
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err := q.InsertAbyssalProbe(ctx, db.InsertAbyssalProbeParams{
		ItemID: itemID, TypeID: cand.TypeID, IsDynamic: true, ProbedAt: now,
	}); err != nil {
		return fmt.Errorf("insert probe: %w", err)
	}
	if err := q.UpsertAbyssalItem(ctx, db.UpsertAbyssalItemParams{
		ItemID: itemID, TypeID: cand.TypeID,
		SourceTypeID: dyn.SourceTypeID, MutatorTypeID: dyn.MutatorTypeID,
		CreatedBy: dyn.CreatedBy, Attributes: attrs,
		FetchedAt: now, FirstSeenAt: cand.FirstSeen,
	}); err != nil {
		return fmt.Errorf("upsert abyssal item: %w", err)
	}
	if err := q.UpsertAbyssalSighting(ctx, db.UpsertAbyssalSightingParams{
		ItemID: itemID, ContractID: cand.ContractID, ContractType: cand.Type,
		Price: cand.Price, RegionID: cand.RegionID,
		LocationID:  cand.StartLocationID,
		DateExpired: sql.NullTime{Time: cand.DateExpired, Valid: true},
		FirstSeenAt: cand.FirstSeen, LastSeenAt: now,
	}); err != nil {
		return fmt.Errorf("open sighting: %w", err)
	}
	// A newly known abyssal also changes its contract-mates'
	// attribution: restamp every sighting on the contract.
	if err := q.StampAbyssalSightingAttribution(ctx, db.StampAbyssalSightingAttributionParams{
		ContractID: sql.NullInt64{Int64: cand.ContractID, Valid: true},
	}); err != nil {
		return fmt.Errorf("stamp sightings: %w", err)
	}
	return tx.Commit()
}
