package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The true-value pass: the worker side of true values
// (truevalue.go). One bounded chunk of manufacturable types
// per cycle, priced bottom-up from stored inputs -- SDE
// blueprints, The Forge's swept typicals, the stored guide --
// plus one bounded crawl of ESI's public contracts per cycle
// for the types the market cannot price. Pages never compute
// any of this; they read the stored rows.
//
// A stored row goes stale when the profile version moves,
// when the inputs it was computed from move (the guide's
// fetched_at or the Forge sweep's stamp, fingerprinted
// below), or when it ages past trueValueStaleAfter. The sweep
// walks products in type order behind a cursor, so a full
// pass over the ~4,800 manufacturable types takes a dozen
// cycles and then idles on fresh rows.
// ---------------------------------------------------------------------------

// tvPassChunk bounds one cycle's evaluations.
const tvPassChunk = 400

// tvPassState is the pass's progress between cycles. Only the
// worker touches it (refreshCycle is serial), so it needs no
// lock of its own.
type tvPassState struct {
	cursor      int    // next product index to evaluate
	fingerprint string // inputs the stored rows were computed under
	graph       *tvBlueprintGraph
	profile     trueValueProfile

	samples   map[int64][]tvContractSample // thin type -> contract samples
	lastCrawl map[int64]time.Time          // region -> last crawl
	crawlNext int                          // next region to try crawling

	// The certain tier (schema 052): sale samples harvested
	// from linked characters' own contracts that ESI reports
	// finished, kept apart from the crawl's listing samples.
	fulfilled     map[int64][]tvContractSample // thin type -> fulfilled-sale samples
	fulfilledSeen map[int64]bool               // contract IDs already harvested
	snapshotSeen  map[int64]time.Time          // character -> contracts snapshot vintage last harvested
}

// The evidence tiers behind an estimated sell, weakest last
// in this listing but strongest first where they are spent
// (estimatedSellTiered): a recorded sale outranks a watched
// listing that vanished before expiry (likely sold -- an
// issuer can also delete a listing, so it is never certain),
// and both outrank a live listing, which is only an ask.
const (
	tvSampleFulfilled = "fulfilled"
	tvSampleVanished  = "vanished"
	tvSampleListing   = "listing"
)

// tvContractSample is one clean contract observation whose
// implied unit price for a thin type sat inside the band
// around that type's build cost. source is its evidence tier
// (the tvSample* constants).
type tvContractSample struct {
	price  float64
	at     time.Time
	source string
}

// tvSampleWindow is how long a sample of any tier stays
// meaningful (contracts turn over fast; a fortnight covers
// the crawl's rotation several times over).
const tvSampleWindow = 14 * 24 * time.Hour

// tvBlueprintGraph is the whole SDE recipe book in memory,
// loaded once per sweep (the SDE changes only on reimport).
type tvBlueprintGraph struct {
	byProduct map[int64]*buildplan.Blueprint
	products  []int64 // product type IDs, ascending
	ships     map[int64]bool
}

func (g *tvBlueprintGraph) BlueprintForProduct(productTypeID int64) (*buildplan.Blueprint, bool) {
	bp, ok := g.byProduct[productTypeID]
	return bp, ok
}

// loadTVBlueprintGraph reads every manufacturing blueprint and
// its materials in three batched queries (the planner's
// prefetch shape), plus the ship set for the hull ME rule.
func (app *Application) loadTVBlueprintGraph(ctx context.Context) (*tvBlueprintGraph, error) {
	rows, err := app.queries.ListSDEBlueprintProducts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list blueprint products: %w", err)
	}
	graph := &tvBlueprintGraph{
		byProduct: make(map[int64]*buildplan.Blueprint, len(rows)),
		ships:     make(map[int64]bool),
	}
	productIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		productIDs = append(productIDs, row.ProductTypeID)
	}
	sort.Slice(productIDs, func(i, j int) bool { return productIDs[i] < productIDs[j] })
	graph.products = productIDs

	blueprints, err := app.queries.GetSDEBlueprintsForProducts(ctx, productIDs)
	if err != nil {
		return nil, fmt.Errorf("load blueprints: %w", err)
	}
	blueprintIDs := make([]int64, 0, len(blueprints))
	for _, bp := range blueprints {
		graph.byProduct[bp.ProductTypeID] = &buildplan.Blueprint{
			BlueprintTypeID:    bp.BlueprintTypeID,
			ProductTypeID:      bp.ProductTypeID,
			ProductQuantity:    bp.ProductQuantity,
			MaxProductionLimit: bp.MaxProductionLimit,
			TimeSeconds:        bp.ManufacturingTimeSeconds,
		}
		blueprintIDs = append(blueprintIDs, bp.BlueprintTypeID)
	}
	materials, err := app.queries.ListSDEBlueprintMaterialsForBlueprints(ctx, blueprintIDs)
	if err != nil {
		return nil, fmt.Errorf("load blueprint materials: %w", err)
	}
	byBlueprint := make(map[int64]*buildplan.Blueprint, len(blueprints))
	for _, bp := range graph.byProduct {
		byBlueprint[bp.BlueprintTypeID] = bp
	}
	for _, m := range materials {
		if bp, ok := byBlueprint[m.BlueprintTypeID]; ok {
			bp.Materials = append(bp.Materials, buildplan.Material{TypeID: m.MaterialTypeID, Quantity: m.Quantity})
		}
	}
	if shipIDs, err := app.queries.ListSDEShipTypeIDs(ctx, productIDs); err == nil {
		for _, id := range shipIDs {
			graph.ships[id] = true
		}
	} else {
		logging.Errorf("true values: ship set: %v", err)
	}
	return graph, nil
}

// refreshTrueValues runs one bounded chunk of the pass. It is
// called from refreshCycle behind the guide refresh and the
// cost-index refresh, whose outputs are its inputs.
func (app *Application) refreshTrueValues(ctx context.Context) {
	guide := app.storedGuidePrices(ctx)
	if guide == nil {
		return // no guide yet: nothing honest to price from
	}
	st := &app.trueValuePass
	if st.samples == nil {
		st.samples = make(map[int64][]tvContractSample)
		st.lastCrawl = make(map[int64]time.Time)
	}
	if st.fulfilled == nil {
		st.fulfilled = make(map[int64][]tvContractSample)
		st.fulfilledSeen = make(map[int64]bool)
		st.snapshotSeen = make(map[int64]time.Time)
	}

	// The Forge's swept stats are the input price book: a
	// type's leaf price is its typical sell there.
	statsRows, err := app.queries.ListMarketRegionStatsByRegion(ctx, defaultMarketRegion)
	if err != nil {
		logging.Errorf("true values: forge stats: %v", err)
		return
	}
	typicalSell := make(map[int64]float64, len(statsRows))
	dailyVolume := make(map[int64]float64, len(statsRows))
	var statsStamp time.Time
	for _, row := range statsRows {
		if row.TypicalSell > 0 {
			typicalSell[row.TypeID] = row.TypicalSell
		}
		dailyVolume[row.TypeID] = row.AvgDailyVolume
		if row.UpdatedAt.After(statsStamp) {
			statsStamp = row.UpdatedAt
		}
	}

	// The fingerprint of everything a stored row was computed
	// from: profile version, guide vintage, sweep vintage.
	meta, _ := app.guideMeta(ctx)
	fingerprint := fmt.Sprintf("%d|%s|%d", trueValueProfileVersion,
		meta.FetchedAt.UTC().Format(time.RFC3339Nano), statsStamp.Unix())
	inputsChanged := fingerprint != st.fingerprint

	if st.graph == nil || st.cursor == 0 {
		graph, err := app.loadTVBlueprintGraph(ctx)
		if err != nil {
			logging.Errorf("true values: %v", err)
			return
		}
		if len(graph.products) == 0 {
			return // SDE not imported yet
		}
		st.graph = graph
		st.profile = app.resolveTrueValueProfile(ctx)
	}
	graph := st.graph

	existing := make(map[int64]db.TrueValue)
	if rows, err := app.queries.ListTrueValues(ctx); err == nil {
		for _, row := range rows {
			existing[row.TypeID] = row
		}
	} else {
		logging.Errorf("true values: load stored: %v", err)
	}

	// The stored-price lookup the contract sampler nets a
	// fitted contract's other items out at, and the probable
	// tier: listings the crawl watched vanish before expiry,
	// read back from the observation table (they outlive the
	// process, unlike the listing samples in memory).
	priceOf := tvStoredPricerFrom(typicalSell, existing, guide)
	vanished := make(map[int64][]tvContractSample)
	if rows, err := app.queries.ListTVVanishedSamplesSince(ctx, sql.NullTime{Time: time.Now().Add(-tvSampleWindow), Valid: true}); err == nil {
		for _, row := range rows {
			if row.VanishedAt.Valid {
				// A listing ESI confirmed accepted is a recorded
				// sale (pub_contracts_sales.go), not a likely one.
				source := tvSampleVanished
				if row.Sold {
					source = tvSampleFulfilled
				}
				vanished[row.TypeID] = append(vanished[row.TypeID], tvContractSample{
					price: row.SamplePrice, at: row.VanishedAt.Time, source: source,
				})
			}
		}
	} else {
		logging.Errorf("true values: vanished samples: %v", err)
	}

	// Leaf prices: Forge typical sell first (the price a
	// builder actually pays), then the stored estimated sell
	// from the previous pass. The guide's average is banned
	// as a price source (owner doctrine, 2026-10-17) and the
	// adjusted price is not one either: a leaf neither house
	// source knows is unpriced, and whatever builds from it
	// stays unpriced with it — an honest gap, never a fossil.
	// The EIV base for job fees follows the planner: adjusted
	// where present, average behind it (fees only).
	leafPrice := func(typeID int64) (float64, bool) {
		if v := typicalSell[typeID]; v > 0 {
			return v, true
		}
		if tv, ok := existing[typeID]; ok && tv.EstSell.Valid && tv.EstSell.Float64 > 0 {
			return tv.EstSell.Float64, true
		}
		return 0, false
	}
	adjusted := make(map[int64]float64, len(guide))
	for id, p := range guide {
		switch {
		case p.AdjustedPrice > 0:
			adjusted[id] = p.AdjustedPrice
		case p.AveragePrice > 0:
			adjusted[id] = p.AveragePrice
		}
	}
	// One coster for the chunk: hulls price through RootCost
	// (their own blueprint at hull ME), everything else
	// through UnitCost (component ME throughout).
	coster := buildplan.NewUnitCoster(graph, leafPrice, adjusted, buildplan.CostProfile{
		RootME:      trueValueShipME,
		ComponentME: trueValueComponentME,
		MatFactor:   st.profile.matFactor,
		CostFactor:  st.profile.costFactor,
		CostIndex:   st.profile.costIndex,
		FacilityTax: buildplan.NPCFacilityTax,
	})

	now := time.Now()
	start := st.cursor
	if start >= len(graph.products) {
		start = 0
	}
	end := start + tvPassChunk
	if end > len(graph.products) {
		end = len(graph.products)
	}
	computed, unpriced := 0, 0
	thinCosts := make(map[int64]float64)
	for _, typeID := range graph.products[start:end] {
		if row, ok := existing[typeID]; ok && !inputsChanged &&
			row.ProfileVersion == trueValueProfileVersion &&
			now.Sub(row.ComputedAt) < trueValueStaleAfter {
			if isThinType(guide, dailyVolume, typeID) {
				thinCosts[typeID] = row.BuildCost
			}
			continue
		}
		var buildCost float64
		var ok bool
		if graph.ships[typeID] {
			buildCost, ok = coster.RootCost(typeID)
		} else {
			buildCost, ok = coster.UnitCost(typeID)
		}
		if !ok || buildCost <= 0 {
			unpriced++
			continue
		}
		row := db.UpsertTrueValueParams{
			TypeID:         typeID,
			BuildCost:      buildCost,
			ComputedAt:     now,
			ProfileVersion: trueValueProfileVersion,
		}
		if isThinType(guide, dailyVolume, typeID) {
			thinCosts[typeID] = buildCost
			st.fulfilled[typeID] = freshContractSamples(st.fulfilled[typeID], now)
			samples := freshContractSamples(st.samples[typeID], now)
			samples = append(samples, freshContractSamples(vanished[typeID], now)...)
			samples = append(samples, st.fulfilled[typeID]...)
			est, source, count := estimatedSellTiered(buildCost, samples)
			row.SellSamples = int64(count)
			row.SellSource = source
			row.EstSell = sql.NullFloat64{Float64: est, Valid: true}
		}
		if err := app.queries.UpsertTrueValue(ctx, row); err != nil {
			logging.Errorf("true values: store %d: %v", typeID, err)
			continue
		}
		computed++
	}
	st.cursor = end
	if end >= len(graph.products) {
		st.cursor = 0
		st.fingerprint = fingerprint
	}
	if computed > 0 || unpriced > 0 {
		logging.Infof("true values: priced %d types (%d unpriced) at chunk %d-%d of %d",
			computed, unpriced, start, end, len(graph.products))
	}

	// Thin types whose stored rows predate this chunk still
	// feed the contracts crawl's band check.
	for typeID, row := range existing {
		if _, done := thinCosts[typeID]; !done && row.BuildCost > 0 && isThinType(guide, dailyVolume, typeID) {
			thinCosts[typeID] = row.BuildCost
		}
	}
	app.harvestFulfilledSamples(ctx, thinCosts, graph.ships, priceOf, now)
	app.crawlPublicContracts(ctx, thinCosts, graph.ships, priceOf, now)
}

// isThinType reports whether a type lacks a real market: no
// guide average, or a Forge presence below the healthy line.
// Thin types are the ones contracts have to speak for.
func isThinType(guide map[int64]esi.MarketPrice, dailyVolume map[int64]float64, typeID int64) bool {
	p, ok := guide[typeID]
	if !ok || p.AveragePrice <= 0 {
		return true
	}
	return dailyVolume[typeID] < trueValueHealthyDailyVolume
}

// freshContractSamples drops samples older than the window
// they stay meaningful for (tvSampleWindow).
func freshContractSamples(samples []tvContractSample, now time.Time) []tvContractSample {
	out := samples[:0]
	for _, s := range samples {
		if now.Sub(s.at) < tvSampleWindow {
			out = append(out, s)
		}
	}
	return out
}

// estimatedSell turns a build cost and the clean contract
// samples around it into the estimated sell price: the
// samples' median once at least two agree a market exists,
// the build cost itself (the anchored fallback) before that.
func estimatedSell(buildCost float64, samples []tvContractSample) float64 {
	if len(samples) < 2 {
		return buildCost
	}
	prices := make([]float64, 0, len(samples))
	for _, s := range samples {
		prices = append(prices, s.price)
	}
	sort.Float64s(prices)
	mid := len(prices) / 2
	if len(prices)%2 == 1 {
		return prices[mid]
	}
	return (prices[mid-1] + prices[mid]) / 2
}

// estimatedSellTiered spends the samples by evidence tier:
// the strongest tier holding at least two fresh samples sets
// the estimate -- its median, its name, and its count. A tier
// with a single sample cannot speak, however strong: one
// observation is an anecdote at every tier. When no tier
// speaks, the estimate stays the build-cost anchor, the
// source is empty, and the count is every fresh sample (the
// informational count the stored row has always carried).
func estimatedSellTiered(buildCost float64, samples []tvContractSample) (est float64, source string, count int) {
	for _, tier := range []string{tvSampleFulfilled, tvSampleVanished, tvSampleListing} {
		var tierSamples []tvContractSample
		for _, s := range samples {
			if s.source == tier {
				tierSamples = append(tierSamples, s)
			}
		}
		if len(tierSamples) >= 2 {
			return estimatedSell(buildCost, tierSamples), tier, len(tierSamples)
		}
	}
	return buildCost, "", len(samples)
}

// tvStoredPricer prices a type from stored data alone, in
// the house order: The Forge's swept typical sell, then the
// stored true-value estimated sell, then CCP's adjusted
// price. The guide's average is not a price source here (or
// anywhere in the sampler): it is too stale to net real
// fittings against. The contract sampler nets a fitted
// contract's other items out at these prices; ok=false when
// none of the three knows the type, which rejects the sample
// rather than guessing.
type tvStoredPricer func(typeID int64) (float64, bool)

func tvStoredPricerFrom(typicalSell map[int64]float64, stored map[int64]db.TrueValue, guide map[int64]esi.MarketPrice) tvStoredPricer {
	return func(typeID int64) (float64, bool) {
		if v := typicalSell[typeID]; v > 0 {
			return v, true
		}
		if tv, ok := stored[typeID]; ok && tv.EstSell.Valid && tv.EstSell.Float64 > 0 {
			return tv.EstSell.Float64, true
		}
		if p, ok := guide[typeID]; ok && p.AdjustedPrice > 0 {
			return p.AdjustedPrice, true
		}
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// Contracts: the sell-side signal for thin types, in three
// evidence tiers (schema 052). A contract is evidence only in
// context -- public contracts are full of scams, mislabels
// and bundled fits -- so every sample has to earn it: the
// header price must sit within a band around some thin type's
// build cost before its item list is even fetched, and the
// item list must name the thin type cleanly (see
// tvContractSampleFor). What survives is a cluster around the
// build-cost anchor, never a raw feed.
//
//   fulfilled -- contracts of linked characters that ESI
//     reports finished: certain sales, harvested from the
//     sync's own stores (harvestFulfilledSamples).
//   vanished -- public listings the crawl accepted and later
//     watched disappear before expiry: likely sold. An
//     issuer can delete a listing instead, so this tier is
//     probable, never certain, and is labelled that way.
//   listing -- live public listings: asks, as before.
// ---------------------------------------------------------------------------

// tvContractRegions: the five hub regions, then the regions
// contract-traded capital ships actually change hands in.
var tvContractRegions = []int64{
	10000002, // The Forge
	10000043, // Domain
	10000032, // Sinq Laison
	10000030, // Heimatar
	10000042, // Metropolis
	10000060, // Delve
	10000058, // Fountain
	10000050, // Querious
}

const (
	// One region per cycle at most, each at most twice a day:
	// the crawl is a background signal, not a feed.
	tvContractCrawlInterval  = 12 * time.Hour
	tvContractMaxPages       = 25
	tvContractMaxItemFetches = 150
	// Below this a contract is parts and noise, not a thin
	// capital or collectible changing hands.
	tvContractMinPrice = 1_000_000
	// The band around build cost a price must sit inside to
	// count -- wide enough for real negotiation, far inside
	// the 10x-and-pray scam range.
	tvContractBandLow  = 0.2
	tvContractBandHigh = 5.0
	// A listing that disappears further than this before its
	// expiry did not merely expire: it sold or was withdrawn,
	// either way an exit the expiry cannot explain. Listings
	// vanishing inside the margin are inconclusive and are
	// forgotten instead of counted.
	tvContractVanishMargin = 24 * time.Hour
	// Per-crawl cap on re-checking watched listings, inside
	// the same item-fetch budget the candidates spend from.
	tvContractMaxProbes = 50
)

// tvPublicContract is one header row of GET
// /contracts/public/{region}/ (only the fields used).
type tvPublicContract struct {
	ContractID  int64   `json:"contract_id"`
	Price       float64 `json:"price"`
	Status      string  `json:"status"`
	Type        string  `json:"type"`
	DateExpired string  `json:"date_expired"`
}

// tvPublicContractItem is one row of GET
// /contracts/public/items/{contract}/ (only the fields used).
type tvPublicContractItem struct {
	TypeID     int64 `json:"type_id"`
	Quantity   int64 `json:"quantity"`
	IsIncluded bool  `json:"is_included"`
}

// crawlPublicContracts advances the crawl by at most one due
// region. Samples accumulate in the pass state; evaluation
// (above) consumes them.
func (app *Application) crawlPublicContracts(ctx context.Context, thinCosts map[int64]float64, ships map[int64]bool, priceOf tvStoredPricer, now time.Time) {
	st := &app.trueValuePass
	for typeID, samples := range st.samples {
		if samples = freshContractSamples(samples, now); len(samples) == 0 {
			delete(st.samples, typeID)
		} else {
			st.samples[typeID] = samples
		}
	}
	// Observation housekeeping: a watched listing that
	// expired without ever vanishing can never become
	// evidence now, and a vanished row outlives its samples
	// by nothing once they age out.
	if err := app.queries.PurgeTVContractObservations(ctx, db.PurgeTVContractObservationsParams{
		ExpiresAt:  now.Add(-tvContractVanishMargin),
		VanishedAt: sql.NullTime{Time: now.Add(-tvSampleWindow), Valid: true},
	}); err != nil {
		logging.Errorf("true values: purge observations: %v", err)
	}
	if len(thinCosts) == 0 {
		return
	}
	costs := make([]float64, 0, len(thinCosts))
	for _, bc := range thinCosts {
		if bc > 0 {
			costs = append(costs, bc)
		}
	}
	sort.Float64s(costs)

	for i := 0; i < len(tvContractRegions); i++ {
		idx := (st.crawlNext + i) % len(tvContractRegions)
		regionID := tvContractRegions[idx]
		if now.Sub(st.lastCrawl[regionID]) < tvContractCrawlInterval {
			continue
		}
		st.crawlNext = idx + 1
		st.lastCrawl[regionID] = now
		app.crawlContractRegion(ctx, regionID, costs, thinCosts, ships, priceOf, now)
		return
	}
}

// crawlContractRegion reads one region's public contracts:
// first the watched listings from earlier crawls are
// re-checked (a listing that has vanished from the public
// endpoint is the probable tier's raw material), then headers
// (paginated, capped), the band filter over them, and item
// lists for the survivors (capped). Probes and candidate item
// fetches spend from one budget.
func (app *Application) crawlContractRegion(ctx context.Context, regionID int64, sortedCosts []float64, thinCosts map[int64]float64, ships map[int64]bool, priceOf tvStoredPricer, now time.Time) {
	st := &app.trueValuePass
	fetched := app.reconcileContractObservations(ctx, regionID, now)
	var candidates []tvPublicContract
	for page := 1; page <= tvContractMaxPages; page++ {
		body, header, err := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/contracts/public/%d/?page=%d", regionID, page))
		if err != nil {
			logging.Warnf("true values: contracts region %d page %d: %v", regionID, page, err)
			return
		}
		var rows []tvPublicContract
		if err := json.Unmarshal(body, &rows); err != nil {
			logging.Warnf("true values: contracts region %d decode: %v", regionID, err)
			return
		}
		for _, c := range rows {
			if tvContractHeaderCandidate(c, sortedCosts) {
				candidates = append(candidates, c)
			}
		}
		pages := 1
		if xp := header.Get("X-Pages"); xp != "" {
			if n, aerr := strconv.Atoi(xp); aerr == nil && n > 1 {
				pages = n
			}
		}
		if page >= pages {
			break
		}
	}
	sampled := 0
	for _, c := range candidates {
		if fetched >= tvContractMaxItemFetches {
			break
		}
		fetched++
		body, _, err := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/contracts/public/items/%d/", c.ContractID))
		if err != nil {
			continue
		}
		var items []tvPublicContractItem
		if err := json.Unmarshal(body, &items); err != nil {
			continue
		}
		if typeID, unit, shape, ok := tvContractSampleFor(c.Price, items, thinCosts, ships, priceOf); ok {
			st.samples[typeID] = append(st.samples[typeID], tvContractSample{price: unit, at: now, source: tvSampleListing})
			sampled++
			app.observeContract(ctx, c, regionID, typeID, unit, shape, now)
		}
	}
	if sampled > 0 {
		logging.Infof("true values: contracts region %d: %d candidates, %d samples", regionID, len(candidates), sampled)
	}
}

// observeContract records (or refreshes) the crawl's memory
// of an accepted listing, so a later crawl can tell whether
// it is still there. Without a parseable expiry the listing
// cannot take part in vanish detection and is not watched.
func (app *Application) observeContract(ctx context.Context, c tvPublicContract, regionID, typeID int64, unit float64, shape string, now time.Time) {
	expires, err := time.Parse(time.RFC3339, c.DateExpired)
	if err != nil {
		return
	}
	if err := app.queries.UpsertTVContractObservation(ctx, db.UpsertTVContractObservationParams{
		ContractID: c.ContractID, RegionID: regionID, TypeID: typeID, Shape: shape,
		ListedPrice: c.Price, SamplePrice: unit, FirstSeenAt: now, ExpiresAt: expires.UTC(),
	}); err != nil {
		logging.Errorf("true values: observe contract %d: %v", c.ContractID, err)
	}
}

// reconcileContractObservations re-checks this region's
// watched listings against the public items endpoint (the
// listing's own endpoint: gone from it means gone from the
// public market). A listing still there is touched; one that
// answers 204 before its expiry was accepted -- a recorded
// sale (pub_contracts_sales.go); one that answers 404 vanished
// -- marked vanished when that happened well before its
// expiry (the probable tier: deleted, or accepted too long ago
// for ESI to say), deleted when expiry itself is explanation
// enough. Any other answer is inconclusive and leaves the row
// for the next crawl. It returns the item fetches spent,
// against the crawl budget.
func (app *Application) reconcileContractObservations(ctx context.Context, regionID int64, now time.Time) (spent int) {
	st := &app.trueValuePass
	rows, err := app.queries.ListTVContractObservationsByRegion(ctx, regionID)
	if err != nil {
		logging.Errorf("true values: list observations region %d: %v", regionID, err)
		return 0
	}
	for _, obs := range rows {
		if spent >= tvContractMaxProbes || spent >= tvContractMaxItemFetches {
			break
		}
		spent++
		exit, err := app.probeContractExit(ctx, obs.ContractID, obs.ExpiresAt, now)
		if err != nil {
			break // ESI's error limit: the rest wait for the next crawl
		}
		switch exit {
		case exitLive:
			if err := app.queries.TouchTVContractObservation(ctx, db.TouchTVContractObservationParams{
				ContractID: obs.ContractID, LastSeenAt: now,
			}); err != nil {
				logging.Errorf("true values: touch observation %d: %v", obs.ContractID, err)
			}
			continue
		case exitSold:
			if err := app.queries.MarkTVContractObservationSold(ctx, db.MarkTVContractObservationSoldParams{
				ContractID: obs.ContractID, VanishedAt: sql.NullTime{Time: now, Valid: true},
			}); err != nil {
				logging.Errorf("true values: mark observation %d sold: %v", obs.ContractID, err)
				continue
			}
			st.samples[obs.TypeID] = dropContractSample(st.samples[obs.TypeID], obs)
			logging.Infof("true values: contract %d (type %d) accepted before expiry; counted as a recorded sale", obs.ContractID, obs.TypeID)
			continue
		case exitUnknown:
			continue // a transient answer says nothing either way
		}
		if tvVanishedBeforeExpiry(now, obs.ExpiresAt) {
			if err := app.queries.MarkTVContractObservationVanished(ctx, db.MarkTVContractObservationVanishedParams{
				ContractID: obs.ContractID, VanishedAt: sql.NullTime{Time: now, Valid: true},
			}); err != nil {
				logging.Errorf("true values: mark observation %d vanished: %v", obs.ContractID, err)
				continue
			}
			// The listing copy of this sample leaves the ask
			// tier: the same contract now speaks as probable.
			st.samples[obs.TypeID] = dropContractSample(st.samples[obs.TypeID], obs)
			logging.Infof("true values: contract %d (type %d) vanished before expiry; counted as likely sold", obs.ContractID, obs.TypeID)
		} else if err := app.queries.DeleteTVContractObservation(ctx, obs.ContractID); err != nil {
			logging.Errorf("true values: forget observation %d: %v", obs.ContractID, err)
		}
	}
	return spent
}

// tvVanishedBeforeExpiry reports whether a listing seen gone
// at `now` disappeared too long before its expiry for the
// expiry itself to explain the exit.
func tvVanishedBeforeExpiry(now, expiresAt time.Time) bool {
	return now.Before(expiresAt.Add(-tvContractVanishMargin))
}

// dropContractSample removes the listing-tier sample one
// observation originally produced (same price, recorded at
// the moment the listing was first seen) from a sample list.
func dropContractSample(samples []tvContractSample, obs db.TrueValueContractObservation) []tvContractSample {
	out := samples[:0]
	for _, s := range samples {
		if s.source == tvSampleListing && s.price == obs.SamplePrice &&
			s.at.Sub(obs.FirstSeenAt) >= 0 && s.at.Sub(obs.FirstSeenAt) < time.Minute {
			continue
		}
		out = append(out, s)
	}
	return out
}

// tvContractHeaderCandidate is the cheap first filter, run on
// header fields alone: an outstanding item exchange, priced
// like something real, within the band of at least one thin
// type's build cost.
func tvContractHeaderCandidate(c tvPublicContract, sortedCosts []float64) bool {
	if c.Type != "item_exchange" {
		return false
	}
	if c.Status != "" && c.Status != "outstanding" {
		return false
	}
	if c.Price < tvContractMinPrice {
		return false
	}
	return tvPriceInBand(sortedCosts, c.Price)
}

// tvPriceInBand reports whether any build cost in the sorted
// slice could anchor this price: some bc with
// price in [bandLow*bc, bandHigh*bc].
func tvPriceInBand(sortedCosts []float64, price float64) bool {
	lo := price / tvContractBandHigh
	hi := price / tvContractBandLow
	i := sort.Search(len(sortedCosts), func(i int) bool { return sortedCosts[i] >= lo })
	return i < len(sortedCosts) && sortedCosts[i] <= hi
}

// The shapes tvContractSampleFor accepts.
const (
	tvContractShapeSingle = "single"
	tvContractShapeFitted = "fitted"
)

// tvContractSampleFor is the strict second filter, run on a
// fetched item list. Two shapes earn a sample:
//
//   - single: the contract is exactly one thin type (a single
//     unit of it, for a ship hull), and its implied unit
//     price sits in the band around that type's build cost.
//   - fitted: the contract carries exactly one unit of one
//     thin ship hull plus other items -- the way titans
//     actually trade, never bare. The other items are netted
//     out at their stored prices (priceOf) and the residual
//     is the hull's implied price, admitted under the same
//     band. A second thin type aboard makes the subject
//     ambiguous and rejects the contract, as does any item
//     stored prices cannot value: the residual must be
//     arithmetic, never a guess.
func tvContractSampleFor(price float64, items []tvPublicContractItem, thinCosts map[int64]float64, ships map[int64]bool, priceOf tvStoredPricer) (typeID int64, unitPrice float64, shape string, ok bool) {
	byType := make(map[int64]int64)
	for _, it := range items {
		if !it.IsIncluded || it.Quantity <= 0 {
			continue
		}
		byType[it.TypeID] += it.Quantity
	}
	if len(byType) == 0 {
		return 0, 0, "", false
	}
	if len(byType) == 1 {
		for id, qty := range byType {
			buildCost, thin := thinCosts[id]
			if !thin {
				return 0, 0, "", false
			}
			if ships[id] && qty != 1 {
				return 0, 0, "", false
			}
			unit := price / float64(qty)
			if unit < tvContractBandLow*buildCost || unit > tvContractBandHigh*buildCost {
				return 0, 0, "", false
			}
			return id, unit, tvContractShapeSingle, true
		}
	}
	// Fitted shape: one candidate hull, everything else
	// netted away.
	var hull int64
	for id, qty := range byType {
		if _, thin := thinCosts[id]; thin {
			if hull != 0 || !ships[id] || qty != 1 {
				return 0, 0, "", false
			}
			hull = id
		}
	}
	if hull == 0 {
		return 0, 0, "", false
	}
	net := 0.0
	for id, qty := range byType {
		if id == hull {
			continue
		}
		unit, priced := priceOf(id)
		if !priced {
			return 0, 0, "", false
		}
		net += unit * float64(qty)
	}
	residual := price - net
	buildCost := thinCosts[hull]
	if residual < tvContractBandLow*buildCost || residual > tvContractBandHigh*buildCost {
		return 0, 0, "", false
	}
	return hull, residual, tvContractShapeFitted, true
}

// ---------------------------------------------------------------------------
// Fulfilled contracts: the certain tier. The public crawl can
// only ever see asks; a sale is visible exactly once -- in
// the contract lists of the characters involved, which the
// sync already stores. A finished item_exchange contract
// whose item list the sampler accepts (single, or fitted with
// the fit netted out at stored prices) is a recorded sale of
// its thin subject at the sampled price. The samples are
// aggregate evidence only: they feed est-sell medians and are
// never displayed per user.
// ---------------------------------------------------------------------------

// tvFulfilledStatuses are ESI's completed-sale statuses: the
// contract ran to completion, as opposed to deleted,
// cancelled, rejected or expired outcomes, which say nothing
// about a price anyone paid.
var tvFulfilledStatuses = map[string]bool{
	"finished":            true,
	"finished_issuer":     true,
	"finished_contractor": true,
}

// harvestFulfilledSamples scans the contracts snapshots that
// changed since the last pass for finished sales of thin
// types and records them as certain-tier samples. A contract
// is harvested once; its sample then ages like any other.
func (app *Application) harvestFulfilledSamples(ctx context.Context, thinCosts map[int64]float64, ships map[int64]bool, priceOf tvStoredPricer, now time.Time) {
	st := &app.trueValuePass
	snaps, err := app.queries.ListSnapshotsByKind(ctx, esi.SnapContracts)
	if err != nil {
		logging.Errorf("true values: fulfilled harvest: list snapshots: %v", err)
		return
	}
	for _, snap := range snaps {
		if seen, ok := st.snapshotSeen[snap.CharacterID]; ok && seen.Equal(snap.FetchedAt) {
			continue
		}
		st.snapshotSeen[snap.CharacterID] = snap.FetchedAt
		var contracts esi.Contracts
		if err := json.Unmarshal([]byte(snap.Payload), &contracts); err != nil {
			continue
		}
		for _, c := range contracts {
			if st.fulfilledSeen[c.ContractID] {
				continue
			}
			if !tvFulfilledSale(c, snap.CharacterID) {
				continue
			}
			completed := tvContractCompletedAt(c)
			if completed.IsZero() || now.Sub(completed) >= tvSampleWindow || completed.After(now) {
				continue
			}
			detail, err := app.queries.GetContractDetail(ctx, c.ContractID)
			if err != nil {
				continue // items not warmed yet; a later snapshot vintage retries
			}
			var stored esi.ContractItems
			if err := json.Unmarshal([]byte(detail.Payload), &stored); err != nil {
				continue
			}
			items := make([]tvPublicContractItem, 0, len(stored))
			for _, it := range stored {
				items = append(items, tvPublicContractItem{TypeID: it.TypeID, Quantity: it.Quantity, IsIncluded: it.IsIncluded})
			}
			if typeID, unit, _, ok := tvContractSampleFor(c.Price, items, thinCosts, ships, priceOf); ok {
				st.fulfilled[typeID] = append(st.fulfilled[typeID], tvContractSample{
					price: unit, at: completed, source: tvSampleFulfilled,
				})
				st.fulfilledSeen[c.ContractID] = true
			}
		}
	}
}

// tvFulfilledSale reports whether a synced contract is a
// completed sale our own user took part in: an item exchange
// ESI reports finished, with the snapshot's character as its
// issuer or its acceptor. Assignee-only visibility (a
// contract addressed to the character but sold between
// others) is not participation and does not count.
func tvFulfilledSale(c esi.Contract, characterID int64) bool {
	if c.Type != "item_exchange" || !tvFulfilledStatuses[c.Status] {
		return false
	}
	if c.Price < tvContractMinPrice {
		return false
	}
	return c.IssuerID == characterID || c.AcceptorID == characterID
}

// tvContractCompletedAt reads a finished contract's
// completion time, falling back to its acceptance time.
func tvContractCompletedAt(c esi.Contract) time.Time {
	for _, s := range []string{c.DateCompleted, c.DateAccepted} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
