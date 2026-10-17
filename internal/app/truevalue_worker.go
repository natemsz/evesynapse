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
}

// tvContractSample is one clean public-contract observation:
// a single-type item_exchange contract whose unit price sat
// inside the band around that type's build cost.
type tvContractSample struct {
	price float64
	at    time.Time
}

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

	// Leaf prices: Forge typical sell first (the price a
	// builder actually pays), then the guide's average, then
	// its adjusted. The EIV base for job fees follows the
	// planner: adjusted where present, average behind it.
	leafPrice := func(typeID int64) (float64, bool) {
		if v := typicalSell[typeID]; v > 0 {
			return v, true
		}
		if p, ok := guide[typeID]; ok {
			if p.AveragePrice > 0 {
				return p.AveragePrice, true
			}
			if p.AdjustedPrice > 0 {
				return p.AdjustedPrice, true
			}
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
			samples := freshContractSamples(st.samples[typeID], now)
			row.SellSamples = int64(len(samples))
			row.EstSell = sql.NullFloat64{Float64: estimatedSell(buildCost, samples), Valid: true}
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
	app.crawlPublicContracts(ctx, thinCosts, graph.ships, now)
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
// they stay meaningful for (contracts turn over fast; a
// fortnight covers the crawl's rotation several times over).
func freshContractSamples(samples []tvContractSample, now time.Time) []tvContractSample {
	out := samples[:0]
	for _, s := range samples {
		if now.Sub(s.at) < 14*24*time.Hour {
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

// ---------------------------------------------------------------------------
// Public contracts: the sell-side signal for thin types. A
// contract is evidence only in context -- public contracts are
// full of scams, mislabels and bundled fits -- so a sample has
// to earn it: the header price must sit within a band around
// some thin type's build cost before its item list is even
// fetched, and the item list must be exactly one type (one
// unit, for a ship) whose implied unit price sits in the same
// band around that type's own build cost. What survives is a
// cluster around the build-cost anchor, never a raw feed.
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
)

// tvPublicContract is one header row of GET
// /contracts/public/{region}/ (only the fields used).
type tvPublicContract struct {
	ContractID int64   `json:"contract_id"`
	Price      float64 `json:"price"`
	Status     string  `json:"status"`
	Type       string  `json:"type"`
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
func (app *Application) crawlPublicContracts(ctx context.Context, thinCosts map[int64]float64, ships map[int64]bool, now time.Time) {
	st := &app.trueValuePass
	for typeID, samples := range st.samples {
		if samples = freshContractSamples(samples, now); len(samples) == 0 {
			delete(st.samples, typeID)
		} else {
			st.samples[typeID] = samples
		}
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
		app.crawlContractRegion(ctx, regionID, costs, thinCosts, ships, now)
		return
	}
}

// crawlContractRegion reads one region's public contracts:
// headers first (paginated, capped), the band filter over
// them, then item lists for the survivors (capped).
func (app *Application) crawlContractRegion(ctx context.Context, regionID int64, sortedCosts []float64, thinCosts map[int64]float64, ships map[int64]bool, now time.Time) {
	st := &app.trueValuePass
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
	fetched, sampled := 0, 0
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
		if typeID, unit, ok := tvContractSampleFor(c, items, thinCosts, ships); ok {
			st.samples[typeID] = append(st.samples[typeID], tvContractSample{price: unit, at: now})
			sampled++
		}
	}
	if sampled > 0 {
		logging.Infof("true values: contracts region %d: %d candidates, %d samples", regionID, len(candidates), sampled)
	}
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

// tvContractSampleFor is the strict second filter, run on a
// fetched item list: the contract must be exactly one thin
// type (a single unit of it, for a ship hull -- fitted and
// bundled sales say nothing clean about the hull), and its
// implied unit price must sit in the band around that type's
// own build cost.
func tvContractSampleFor(c tvPublicContract, items []tvPublicContractItem, thinCosts map[int64]float64, ships map[int64]bool) (typeID int64, unitPrice float64, ok bool) {
	var qty int64
	for _, it := range items {
		if !it.IsIncluded || it.Quantity <= 0 {
			continue
		}
		if typeID == 0 {
			typeID = it.TypeID
		} else if it.TypeID != typeID {
			return 0, 0, false
		}
		qty += it.Quantity
	}
	if typeID == 0 || qty <= 0 {
		return 0, 0, false
	}
	buildCost, thin := thinCosts[typeID]
	if !thin {
		return 0, 0, false
	}
	if ships[typeID] && qty != 1 {
		return 0, 0, false
	}
	unit := c.Price / float64(qty)
	if unit < tvContractBandLow*buildCost || unit > tvContractBandHigh*buildCost {
		return 0, 0, false
	}
	return typeID, unit, true
}
