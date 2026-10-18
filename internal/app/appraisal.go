package app

// ---------------------------------------------------------------------------
// The appraisal core: one pricing service for anything made of
// item lines, built for the public contract search and consumed
// unchanged by whatever prices lines next (the standalone
// appraisal tool is the named second consumer). It lives in
// core on purpose: core owns every pricing input -- the swept
// Forge typicals, the stored true values, the labeled Fuzzwork
// fallback -- and the service must outlive any one feature, so
// it carries no contract fields: callers pass lines in, totals
// come out (module.AppraisalLine / module.Appraisal).
//
// Every line prices in the house order, from stored data only:
//  1. our own swept Forge typical sell, while it is fresh;
//  2. the stored true-value estimated sell;
//  3. the Fuzzwork fallback's median sell, inside its hard
//     expiry, provenance and as-of attached;
//  4. our own swept typical again however old (the floor rule:
//     a stale own row still speaks, with its age, when the
//     fallback is expired or absent);
//  5. unpriced -- an honest gap. CCP's guide average is banned
//     as a price source and is never consulted here.
//
// Abyssal (mutaplasmid) lines are the one step ahead of the
// order: a line carrying a singleton item id with a stored
// abyssal estimate (schema 057, computed from its rolls by
// the scoring pass in abyssal.go) prices at that estimate,
// tier "abyssal". A mutated line without a stored estimate
// falls through the order on its mutated type -- no market
// exists for it, so it lands unpriced, an honest gap.
//
// Blueprint copy lines stand outside the order entirely: a
// copy is not its original, and every type-level figure for
// a blueprint type -- the swept typical, the fallback
// median -- is the ORIGINAL's price, because originals are
// what the market trades. A copy prices only from the
// stored copy medians (schema 061: the median ask of
// single-copy contracts for its type and runs, distilled
// from our own corpus by refreshBlueprintCopyMedians below),
// tier "copy_median", or it is unpriced. An original
// blueprint line is an ordinary line: its typical is the
// right price class and the order applies unchanged.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// appraisalRefreshAge is how old a stored appraisal may get
// before the pass reprices it: the pricing inputs move on an
// hourly-ish cadence (typicals sweep, true values recompute,
// fallback poll), so an hour keeps stored numbers within one
// input move of current without repricing the corpus flat.
const appraisalRefreshAge = time.Hour

// appraisalBatchPerCycle caps one appraisal pass: the first
// pass over a full corpus drains over a few cycles in
// contract_id order instead of holding the worker.
const appraisalBatchPerCycle = 400

// copyMedianMinSample is the thinnest corpus slice a stored
// copy median may stand on -- the abyssal corpus's bucket
// minimum, kept here too: below it a median is an anecdote,
// and the copy stays unpriced instead.
const copyMedianMinSample = 5

// dealFlagMaxRatio is the deal flag's line: an asking price at
// or under this share of the appraised value flags the
// contract as priced under our numbers.
const dealFlagMaxRatio = 0.85

// appraisalPrice is one type's resolved unit price: the
// figure, the tier that spoke, and when that tier's figure was
// current. ok is false when no tier could speak.
type appraisalPrice struct {
	unit float64
	tier string
	asOf time.Time
	ok   bool
}

// copyMedianKey keys the stored copy medians: a blueprint
// type and a copy's remaining runs. The medians stratify by
// runs alone -- the corpus's copy listings are overwhelmingly
// one efficiency grade, so per-efficiency strata would stand
// on samples too thin to name a price.
type copyMedianKey struct {
	typeID int64
	runs   int64
}

// resolveAppraisalPrice applies the house order to one type's
// three stored rows (any may be nil). Pure, so the order and
// the floor rule are testable without a database.
func resolveAppraisalPrice(own *db.MarketRegionStat, tv *db.TrueValue, fb *db.MarketStatsFallback, now time.Time) appraisalPrice {
	if own != nil && own.TypicalSell > 0 && now.Sub(own.UpdatedAt) < fallbackOwnFresh {
		return appraisalPrice{unit: own.TypicalSell, tier: module.AppraisalTierTypical, asOf: own.UpdatedAt, ok: true}
	}
	if tv != nil && tv.EstSell.Valid && tv.EstSell.Float64 > 0 {
		return appraisalPrice{unit: tv.EstSell.Float64, tier: module.AppraisalTierEstimate, asOf: tv.ComputedAt, ok: true}
	}
	if fb != nil && fb.MedianSell > 0 && now.Sub(fb.SourceAsof) < fallbackHardExpiry {
		return appraisalPrice{unit: fb.MedianSell, tier: module.AppraisalTierFallback, asOf: fb.SourceAsof, ok: true}
	}
	if own != nil && own.TypicalSell > 0 {
		// The floor rule: our own row, however old, still
		// speaks when the fallback cannot -- and its as-of
		// travels with it, so no reader mistakes it for fresh.
		return appraisalPrice{unit: own.TypicalSell, tier: module.AppraisalTierTypical, asOf: own.UpdatedAt, ok: true}
	}
	return appraisalPrice{tier: module.AppraisalTierUnpriced}
}

// AppraiseLines prices a line list from stored data only: the
// three pricing tables are read once per distinct type set,
// and each line resolves through the house order. It is the
// module.Deps seam the contracts module (and, later, the
// appraisal tool) consumes; the worker's contract pass calls
// it too.
func (app *Application) AppraiseLines(ctx context.Context, lines []module.AppraisalLine) (module.Appraisal, error) {
	out := module.Appraisal{Lines: make([]module.AppraisedLine, 0, len(lines))}
	if len(lines) == 0 {
		return out, nil
	}
	typeIDs := make([]int64, 0, len(lines))
	seen := make(map[int64]bool, len(lines))
	for _, ln := range lines {
		if !seen[ln.TypeID] {
			seen[ln.TypeID] = true
			typeIDs = append(typeIDs, ln.TypeID)
		}
	}
	ownRows, err := app.queries.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{
		RegionID: defaultMarketRegion, Column2: typeIDs,
	})
	if err != nil {
		return out, fmt.Errorf("appraisal: forge typicals: %w", err)
	}
	tvRows, err := app.queries.ListTrueValuesForTypes(ctx, typeIDs)
	if err != nil {
		return out, fmt.Errorf("appraisal: true values: %w", err)
	}
	fbRows, err := app.queries.ListMarketStatsFallbackForTypes(ctx, db.ListMarketStatsFallbackForTypesParams{
		RegionID: defaultMarketRegion, Column2: typeIDs,
	})
	if err != nil {
		return out, fmt.Errorf("appraisal: fallback: %w", err)
	}
	own := make(map[int64]*db.MarketRegionStat, len(ownRows))
	for i := range ownRows {
		own[ownRows[i].TypeID] = &ownRows[i]
	}
	tvs := make(map[int64]*db.TrueValue, len(tvRows))
	for i := range tvRows {
		tvs[tvRows[i].TypeID] = &tvRows[i]
	}
	fbs := make(map[int64]*db.MarketStatsFallback, len(fbRows))
	for i := range fbRows {
		fbs[fbRows[i].TypeID] = &fbRows[i]
	}

	// Stored abyssal estimates for the singleton lines: a
	// mutated item prices from its rolls, not its type.
	itemIDs := make([]int64, 0, len(lines))
	seenItem := make(map[int64]bool, len(lines))
	for _, ln := range lines {
		if ln.ItemID > 0 && !seenItem[ln.ItemID] {
			seenItem[ln.ItemID] = true
			itemIDs = append(itemIDs, ln.ItemID)
		}
	}
	abyssalByItem := map[int64]*db.AbyssalItem{}
	if len(itemIDs) > 0 {
		abRows, err := app.queries.ListAbyssalItemsByIDs(ctx, itemIDs)
		if err != nil {
			return out, fmt.Errorf("appraisal: abyssal estimates: %w", err)
		}
		for i := range abRows {
			if abRows[i].EstValue.Valid {
				abyssalByItem[abRows[i].ItemID] = &abRows[i]
			}
		}
	}

	// Stored copy medians for the blueprint copy lines: a
	// copy prices from its type-and-runs median or not at
	// all, so the rows are read once for the copy lines'
	// distinct types and keyed by (type, runs). A copy row
	// can never carry runs 0, so a line with unknown runs
	// simply keys no median and stays unpriced.
	copyMedians := map[copyMedianKey]*db.BlueprintCopyMedian{}
	{
		copyTypes := make([]int64, 0, len(lines))
		seenCopy := make(map[int64]bool, len(lines))
		for _, ln := range lines {
			if ln.BlueprintCopy && !seenCopy[ln.TypeID] {
				seenCopy[ln.TypeID] = true
				copyTypes = append(copyTypes, ln.TypeID)
			}
		}
		if len(copyTypes) > 0 {
			cmRows, err := app.queries.ListBlueprintCopyMediansForTypes(ctx, copyTypes)
			if err != nil {
				return out, fmt.Errorf("appraisal: copy medians: %w", err)
			}
			for i := range cmRows {
				copyMedians[copyMedianKey{cmRows[i].TypeID, cmRows[i].Runs}] = &cmRows[i]
			}
		}
	}

	now := time.Now().UTC()
	for _, ln := range lines {
		al := module.AppraisedLine{
			TypeID: ln.TypeID, Quantity: ln.Quantity, ItemID: ln.ItemID,
			Included: ln.Included, Tier: module.AppraisalTierUnpriced,
		}
		if ln.BlueprintCopy {
			if cm, ok := copyMedians[copyMedianKey{ln.TypeID, ln.Runs}]; ok {
				al.UnitPrice = cm.MedianPrice
				al.LineValue = al.UnitPrice * float64(ln.Quantity)
				al.Tier = module.AppraisalTierCopyMedian
				al.AsOf = cm.ComputedAt
				al.Priced = true
			}
		} else if ab, ok := abyssalByItem[ln.ItemID]; ok && ln.ItemID > 0 {
			al.UnitPrice = ab.EstValue.Float64
			al.LineValue = al.UnitPrice * float64(ln.Quantity)
			al.Tier = module.AppraisalTierAbyssal
			al.AsOf = ab.ScoredAt.Time
			al.Priced = true
		} else if p := resolveAppraisalPrice(own[ln.TypeID], tvs[ln.TypeID], fbs[ln.TypeID], now); p.ok {
			al.UnitPrice, al.LineValue = p.unit, p.unit*float64(ln.Quantity)
			al.Tier, al.AsOf, al.Priced = p.tier, p.asOf, true
		}
		if ln.Included {
			out.OfferedLines++
			out.OfferedValue += al.LineValue
			if al.Priced {
				out.PricedOffered++
			}
		} else {
			out.WantedLines++
			out.WantValue += al.LineValue
			if al.Priced {
				out.PricedWanted++
			}
		}
		out.Lines = append(out.Lines, al)
	}
	return out, nil
}

// appraisalTierMix stamps what a contract's appraisal is made
// of: the count of priced lines per tier, in house-order
// sequence ("typical:4,fallback:1"), empty when nothing
// priced. The detail page words it for readers.
func appraisalTierMix(lines []module.AppraisedLine) string {
	counts := map[string]int{}
	for _, ln := range lines {
		if ln.Priced {
			counts[ln.Tier]++
		}
	}
	var parts []string
	for _, tier := range []string{module.AppraisalTierTypical, module.AppraisalTierEstimate, module.AppraisalTierAbyssal, module.AppraisalTierCopyMedian, module.AppraisalTierFallback} {
		if n := counts[tier]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", tier, n))
		}
	}
	return strings.Join(parts, ",")
}

// appraisalDealRatio is a contract's effective cost over
// its appraised offered value, and it exists only under full
// coverage on both sides: a ratio over a partial appraisal
// would be a lie of precision, so partial coverage stores no
// ratio and never flags. The cost is what the buyer hands
// over. Exchanges ask their price plus the wanted side --
// the items the buyer must give up, valued through the same
// pricing order as the offered side -- because the ISK alone
// is not the price of a barter: a 1-ISK ask wanting 499 PLEX
// is not a giveaway, and a wanted side that did not fully
// price leaves the cost unknowable, so it gates exactly like
// the offered side. Auctions ask their buyout (an auction
// without one has no ask to judge) and have no wanted side;
// couriers carry no contents value to judge against.
func appraisalDealRatio(contract db.PubContract, res module.Appraisal) (float64, bool) {
	if res.OfferedLines == 0 || res.PricedOffered < res.OfferedLines || res.OfferedValue <= 0 {
		return 0, false
	}
	switch contract.Type {
	case "item_exchange":
		if res.PricedWanted < res.WantedLines {
			return 0, false
		}
		cost := contract.Price + res.WantValue
		if cost <= 0 {
			return 0, false
		}
		return cost / res.OfferedValue, true
	case "auction":
		if contract.Buyout <= 0 {
			return 0, false
		}
		return contract.Buyout / res.OfferedValue, true
	}
	return 0, false
}

// appraisalDealFlag says whether a stored deal ratio flags:
// at or under the deal line. The stored ratio already implies
// full coverage (see appraisalDealRatio), so the flag is the
// threshold alone.
func appraisalDealFlag(ratio sql.NullFloat64) bool {
	return ratio.Valid && ratio.Float64 <= dealFlagMaxRatio
}

// appraisePublicContracts is the worker pass: it prices the
// due slice of live contracts (item list stored; never priced,
// or priced before the refresh age) and stores the per-line
// and per-contract results in one transaction per contract.
// Stored reads and writes only -- no ESI -- so it runs after
// the public contracts pass in the same cycle.
func (app *Application) appraisePublicContracts(ctx context.Context) int {
	now := time.Now().UTC()
	ids, err := app.queries.ListPublicContractsDueAppraisal(ctx, db.ListPublicContractsDueAppraisalParams{
		Cutoff: now.Add(-appraisalRefreshAge), MaxRows: appraisalBatchPerCycle,
	})
	if err != nil {
		logging.Errorf("worker: appraisal: list due: %v", err)
		return 0
	}
	done := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := app.appraiseOnePublicContract(ctx, id, now); err != nil {
			logging.Errorf("worker: appraisal: contract %d: %v", id, err)
			continue
		}
		done++
	}
	return done
}

// appraiseOnePublicContract prices one contract's stored lines
// and stamps the results. The contract row's totals cover the
// offered side's coverage (priced_lines/total_lines); the
// wanted side's coverage lives on its lines, where the detail
// page reads it, and in the Appraisal result the ratio gates
// on before it will speak.
func (app *Application) appraiseOnePublicContract(ctx context.Context, contractID int64, now time.Time) error {
	contract, err := app.queries.GetPublicContract(ctx, contractID)
	if err != nil {
		return fmt.Errorf("read contract: %w", err)
	}
	items, err := app.queries.ListPublicContractItems(ctx, contractID)
	if err != nil {
		return fmt.Errorf("read items: %w", err)
	}
	lines := make([]module.AppraisalLine, 0, len(items))
	for _, it := range items {
		ln := module.AppraisalLine{
			TypeID: it.TypeID, Quantity: it.Quantity, Included: it.IsIncluded,
		}
		if it.ItemID.Valid {
			ln.ItemID = it.ItemID.Int64
		}
		if it.IsBlueprintCopy {
			// The copy facts ride to the core: without them
			// the line would price at its original's figure.
			ln.BlueprintCopy = true
			if it.Runs.Valid {
				ln.Runs = it.Runs.Int64
			}
			if it.MaterialEfficiency.Valid {
				ln.MaterialEfficiency = it.MaterialEfficiency.Int64
			}
			if it.TimeEfficiency.Valid {
				ln.TimeEfficiency = it.TimeEfficiency.Int64
			}
		}
		lines = append(lines, ln)
	}
	res, err := app.AppraiseLines(ctx, lines)
	if err != nil {
		return err
	}

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin appraisal store: %w", err)
	}
	defer tx.Rollback()
	q := db.New(tx)
	for i, it := range items {
		al := res.Lines[i]
		params := db.UpdatePublicContractItemAppraisalParams{
			RecordID: it.RecordID, PriceTier: al.Tier,
		}
		if al.Priced {
			params.UnitPrice = sql.NullFloat64{Float64: al.UnitPrice, Valid: true}
			params.LineValue = sql.NullFloat64{Float64: al.LineValue, Valid: true}
			params.PriceAsof = sql.NullTime{Time: al.AsOf, Valid: true}
		}
		if err := q.UpdatePublicContractItemAppraisal(ctx, params); err != nil {
			return fmt.Errorf("store line %d: %w", it.RecordID, err)
		}
	}
	ratio, hasRatio := appraisalDealRatio(contract, res)
	params := db.UpdatePublicContractAppraisalParams{
		ContractID: contractID, OfferedValue: res.OfferedValue, WantValue: res.WantValue,
		PricedLines: int64(res.PricedOffered), TotalLines: int64(res.OfferedLines),
		TierMix: appraisalTierMix(res.Lines), PricedAt: now,
	}
	if hasRatio {
		params.DealRatio = sql.NullFloat64{Float64: ratio, Valid: true}
	}
	if err := q.UpdatePublicContractAppraisal(ctx, params); err != nil {
		return fmt.Errorf("store contract appraisal: %w", err)
	}
	return tx.Commit()
}

// refreshBlueprintCopyMedians is the worker pass behind the
// copy-median tier (schema 061): it recomputes the stored
// medians from the live corpus at most once per the
// appraisal refresh age -- the cadence the stored appraisals
// reprice on, so a fresh median and the values it feeds move
// together. Returns the number of (type, runs) rows stored,
// and 0 when the stored rows are still fresh.
func (app *Application) refreshBlueprintCopyMedians(ctx context.Context) int {
	stamp, err := app.queries.GetBlueprintCopyMediansStamp(ctx)
	switch {
	case err == nil:
		if time.Since(stamp) < appraisalRefreshAge {
			return 0
		}
	case !errors.Is(err, sql.ErrNoRows):
		logging.Errorf("worker: copy medians: stamp: %v", err)
		return 0
	}
	n, err := app.recomputeBlueprintCopyMedians(ctx, time.Now().UTC())
	if err != nil {
		logging.Errorf("worker: copy medians: %v", err)
		return 0
	}
	return n
}

// recomputeBlueprintCopyMedians distills the live corpus
// into the stored medians in one replace-all transaction:
// single-line item exchanges whose one stored line is an
// included copy with known runs, asked above zero, grouped
// by type and runs, at or over the minimum sample (the
// candidate query carries the shape and the gate). A
// contract's own ask is one listing among its group's; at
// the minimum sample the median stands on the group, not on
// any one listing, so no anti-anchoring machinery rides
// along. The corpus holds only live contracts (a row leaves
// when its contract does), so a group that falls under the
// sample simply stops being stored, and its copies fall back
// to honestly unpriced on the next reprice.
func (app *Application) recomputeBlueprintCopyMedians(ctx context.Context, now time.Time) (int, error) {
	cands, err := app.queries.ListBlueprintCopyMedianCandidates(ctx, copyMedianMinSample)
	if err != nil {
		return 0, fmt.Errorf("candidates: %w", err)
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin copy median store: %w", err)
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err := q.DeleteBlueprintCopyMedians(ctx); err != nil {
		return 0, fmt.Errorf("clear stored medians: %w", err)
	}
	stored := 0
	for _, c := range cands {
		if !c.Runs.Valid {
			continue // the candidate query filters runs in; belt and braces
		}
		if err := q.InsertBlueprintCopyMedian(ctx, db.InsertBlueprintCopyMedianParams{
			TypeID: c.TypeID, Runs: c.Runs.Int64, MedianPrice: c.MedianPrice,
			SampleCount: c.SampleCount, ComputedAt: now,
		}); err != nil {
			return 0, fmt.Errorf("store median %d/%d: %w", c.TypeID, c.Runs.Int64, err)
		}
		stored++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit copy medians: %w", err)
	}
	return stored, nil
}
