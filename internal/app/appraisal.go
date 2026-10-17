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
// Abyssal (mutaplasmid) lines price through this same order on
// their mutated type in this build: no market exists for them,
// so they land unpriced and the contract pages render their
// rolls with the source module's price as context only -- the
// abyssal estimate itself is a later step and slots in behind
// the same line contract.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
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

	now := time.Now().UTC()
	for _, ln := range lines {
		al := module.AppraisedLine{
			TypeID: ln.TypeID, Quantity: ln.Quantity, ItemID: ln.ItemID,
			Included: ln.Included, Tier: module.AppraisalTierUnpriced,
		}
		if p := resolveAppraisalPrice(own[ln.TypeID], tvs[ln.TypeID], fbs[ln.TypeID], now); p.ok {
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
	for _, tier := range []string{module.AppraisalTierTypical, module.AppraisalTierEstimate, module.AppraisalTierFallback} {
		if n := counts[tier]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", tier, n))
		}
	}
	return strings.Join(parts, ",")
}

// appraisalDealRatio is a contract's asking price over its
// appraised offered value, and it exists only under full
// offered-side coverage: a ratio over a partial appraisal
// would be a lie of precision, so partial coverage stores no
// ratio and never flags. Exchanges ask their price; auctions
// ask their buyout (an auction without one has no ask to
// judge); couriers carry no contents value to judge against.
func appraisalDealRatio(contract db.PubContract, res module.Appraisal) (float64, bool) {
	if res.OfferedLines == 0 || res.PricedOffered < res.OfferedLines || res.OfferedValue <= 0 {
		return 0, false
	}
	ask := contract.Price
	if contract.Type == "auction" {
		ask = contract.Buyout
	}
	if ask <= 0 {
		return 0, false
	}
	return ask / res.OfferedValue, true
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
// offered side's coverage (priced_lines/total_lines), which is
// the side the deal ratio judges; the wanted side's coverage
// lives on its lines, where the detail page reads it.
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
