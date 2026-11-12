package app

import (
	"context"
	"database/sql"
	"fmt"
	json "github.com/goccy/go-json"
	"slices"
	"sort"
	"time"

	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The true-value pass: the worker side of true values
// (truevalue.go). Every manufacturable type is priced
// bottom-up from stored inputs -- SDE blueprints, The Forge's
// swept typicals, the stored guide -- plus contract evidence
// for the types the market cannot price: the stored
// public-contract corpus, exit probes over the corpus's
// departed listings (truevalue_sales.go), and ship types with
// no blueprint at all. Thin non-ship types price from their
// samples by evidence tier; thin ships price the abyssal way,
// by a learned correction over their build cost
// (truevalue_learn.go). An estimated sell is stored only when
// the evidence produces one; a build cost is never stored as
// one. Pages never compute any of this; they read the stored
// rows.
//
// A pass runs when its inputs move (the profile, the guide's
// fetched_at, the Forge sweep's stamp, the SDE import) or when
// contract evidence moves (a listing arrives or leaves, an exit
// probe settles, a sale is harvested). The arithmetic is cheap
// and runs whole; the corpus reads are not, so each type's
// mined contracts are cached and read again only when its
// evidence moves, and only changed rows are written.
// ---------------------------------------------------------------------------

// tvMinePerCycle bounds one cycle's corpus reads: the types
// whose cached contracts are missing. A warm pass reads only
// the types that gained a contract since the last one; a cold
// start (process start, or the cache's renewal) fills over a
// handful of cycles, ship hulls first, since the ship fit
// waits for every hull's evidence.
const tvMinePerCycle = 600

// tvArrivalOverlap is how far each check for newly stored
// contracts reaches back past the newest one already handled,
// so a stamp written by a transaction that committed late is
// still seen. Contracts already handled are skipped by ID.
const tvArrivalOverlap = 10 * time.Minute

// tvPassState is the pass's memory between cycles. Only the
// worker touches it (refreshCycle is serial), so it needs no
// lock of its own.
type tvPassState struct {
	fingerprint   string // inputs the stored rows were last computed under
	evidenceMoved bool   // contract evidence changed since the last pass
	graph         *tvBlueprintGraph
	graphStamp    string // SDE import stamp the graph was loaded under

	// The corpus cache: each thin type's mined live contracts.
	// A type absent from corpus is mined on the next pass; a
	// type whose corpus gained a contract is dropped from it.
	// The cache is emptied at start and again after
	// trueValueStaleAfter, so no missed signal outlives that.
	corpus        map[int64][]*tvCorpusContract
	cacheStarted  time.Time
	arrivalsSince time.Time                                 // newest stored-items stamp handled
	arrivalsSeen  map[int64]time.Time                       // contract -> stamp, inside the overlap
	observed      map[int64]db.TrueValueContractObservation // contract -> observation last written

	// The certain tier (schema 052): sale samples harvested
	// from linked characters' own contracts that ESI reports
	// finished, kept apart from the corpus's listing samples.
	fulfilled     map[int64][]tvContractSample // thin type -> fulfilled-sale samples
	fulfilledSeen map[int64]bool               // contract IDs already harvested
	snapshotSeen  map[int64]time.Time          // character -> contracts snapshot vintage last harvested
}

// tvCorpusContract is one live item exchange the corpus miner
// read for a thin type, kept so later passes can judge it
// again against current prices without reading it again.
// lastSeen follows the contract sweep's sightings
// (refreshTVCorpusLive).
type tvCorpusContract struct {
	id        int64
	regionID  int64
	price     float64
	firstSeen time.Time
	lastSeen  time.Time
	expiresAt time.Time
	items     []tvPublicContractItem
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

// tvContractSample is one clean contract observation of a
// thin type: a contract whose shape the sampler accepts,
// reduced to its implied unit price for that type. Whether
// the price agrees with the type's build-cost anchor is the
// estimate step's question, not the sample's. source is its
// evidence tier (the tvSample* constants); contractID is the
// contract it came from (0 when the source does not carry
// one), so the same contract seen by two stores speaks once.
type tvContractSample struct {
	price      float64
	at         time.Time
	source     string
	contractID int64
}

// tvSampleWindow is how long a sample stays meaningful
// for the TIERED estimate (thin non-ship types): contracts
// turn over fast, and a fortnight holds several turnovers
// of the corpus. Ship evidence is not cut
// off at this window -- the learned fit weights age
// instead (tvSampleWeight) -- and is kept to
// tvEvidenceRetention.
const tvSampleWindow = 14 * 24 * time.Hour

// tvEvidenceRetention bounds how long evidence is KEPT --
// exited rows in the observation store, samples in the
// pass's memory -- never how long it counts. A confirmed
// sale from weeks ago still counts in a ship fit,
// discounted by its age curve; the horizon sits well past
// the point where the slowest curve (a sale's) reaches its
// floor (~119 days), so evidence lives at its floored
// voice for a long plateau before storage lets it go. It
// is storage hygiene, not an estimate gate.
const tvEvidenceRetention = 240 * 24 * time.Hour

// tvBlueprintGraph is the whole SDE recipe book in memory,
// loaded once per sweep (the SDE changes only on reimport).
type tvBlueprintGraph struct {
	byProduct map[int64]*buildplan.Blueprint
	products  []int64 // product type IDs, ascending
	ships     map[int64]bool
	allShips  []int64 // every SDE ship type ID, ascending
	shipsAll  map[int64]bool
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
	// A product several blueprints make is listed once.
	slices.Sort(productIDs)
	productIDs = slices.Compact(productIDs)
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
	if allShips, err := app.queries.ListAllSDEShipTypeIDs(ctx); err == nil {
		graph.allShips = allShips
		graph.shipsAll = make(map[int64]bool, len(allShips))
		for _, id := range allShips {
			graph.shipsAll[id] = true
		}
	} else {
		logging.Errorf("true values: all-ships set: %v", err)
	}
	return graph, nil
}

// refreshTrueValues runs the pass when its inputs moved. It is
// called from refreshCycle behind the guide refresh and the
// cost-index refresh, whose outputs are its inputs.
//
// Every pass computes every manufacturable type's build cost
// (all in memory: the recipe book, the swept typicals and the
// guide's adjusted prices) and judges every thin type's
// contract evidence from the corpus cache, then writes only
// the rows whose values differ from the stored ones, in one
// statement. The corpus is read again only for the types whose
// evidence moved; that read was the pass's cost, not the math.
func (app *Application) refreshTrueValues(ctx context.Context) {
	meta, ok := app.guideMeta(ctx)
	if !ok {
		return // no guide yet: nothing honest to price from
	}
	st := &app.trueValuePass
	now := time.Now()
	if st.corpus == nil || now.Sub(st.cacheStarted) >= trueValueStaleAfter {
		st.corpus = make(map[int64][]*tvCorpusContract)
		st.observed = make(map[int64]db.TrueValueContractObservation)
		st.arrivalsSeen = make(map[int64]time.Time)
		st.cacheStarted = now
		st.arrivalsSince = now
		st.fingerprint = ""
	}
	if st.fulfilled == nil {
		st.fulfilled = make(map[int64][]tvContractSample)
		st.fulfilledSeen = make(map[int64]bool)
		st.snapshotSeen = make(map[int64]time.Time)
	}

	// Evidence first, every cycle, ahead of the gate: departed
	// listings get their exit probes (ESI remembers an accepted
	// contract only for a while), contracts that left the live
	// set leave the cache, and types whose corpus gained a
	// contract are queued to be mined again.
	app.probeContractExits(ctx, now)
	app.refreshTVCorpusLive(ctx)
	app.noteTVCorpusArrivals(ctx)

	// The pass runs when its inputs moved: the profile, the
	// guide's vintage, the sweep's stamp, the SDE import, or
	// the contract evidence. Otherwise the cycle idles.
	profile := app.resolveTrueValueProfile(ctx)
	sdeStamp, _ := app.sdeMeta(ctx, "imported_at")
	stamp, err := app.queries.GetMarketRegionStatsStamp(ctx, defaultMarketRegion)
	if err != nil {
		logging.Errorf("true values: forge stats stamp: %v", err)
		return
	}
	var statsStamp time.Time
	if t, ok := stamp.Stamp.(time.Time); ok {
		statsStamp = t
	}
	fingerprint := fmt.Sprintf("%d|%s|%d|%s|%v", trueValueProfileVersion,
		meta.FetchedAt.UTC().Format(time.RFC3339Nano), statsStamp.UnixNano(), sdeStamp, profile)
	if fingerprint == st.fingerprint && !st.evidenceMoved {
		return
	}
	st.evidenceMoved = false

	guide := app.storedGuidePrices(ctx)
	if guide == nil {
		return // no guide yet: nothing honest to price from
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
	for _, row := range statsRows {
		if row.TypicalSell > 0 {
			typicalSell[row.TypeID] = row.TypicalSell
		}
		dailyVolume[row.TypeID] = row.AvgDailyVolume
	}

	if st.graph == nil || st.graphStamp != sdeStamp {
		graph, err := app.loadTVBlueprintGraph(ctx)
		if err != nil {
			logging.Errorf("true values: %v", err)
			return
		}
		if len(graph.products) == 0 {
			return // SDE not imported yet
		}
		st.graph, st.graphStamp = graph, sdeStamp
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
	// tier: watched listings that left before expiry, read
	// back from the observation table.
	priceOf := tvStoredPricerFrom(typicalSell, existing)
	vanished := make(map[int64][]tvContractSample)
	if rows, err := app.queries.ListTVVanishedSamplesSince(ctx, sql.NullTime{Time: now.Add(-tvEvidenceRetention), Valid: true}); err == nil {
		for _, row := range rows {
			if row.VanishedAt.Valid {
				// A listing ESI confirmed accepted is a recorded
				// sale (pub_contracts_sales.go), not a likely one.
				source := tvSampleVanished
				if row.Sold {
					source = tvSampleFulfilled
				}
				vanished[row.TypeID] = append(vanished[row.TypeID], tvContractSample{
					price: row.SamplePrice, at: row.VanishedAt.Time, source: source, contractID: row.ContractID,
				})
			}
		}
	} else {
		logging.Errorf("true values: vanished samples: %v", err)
	}

	// Leaf prices: Forge typical sell first (the price a
	// builder actually pays), then the stored estimated sell
	// from the previous pass, then the stored build cost --
	// a COST input only: what a builder pays to make the leaf
	// is the honest basis for a parent's build cost, and it
	// is quoted as nobody's sell price anywhere. The guide's
	// average is banned as a price source (my ruling,
	// 2026-10-17) and the adjusted price is not one either: a
	// leaf no house source knows is unpriced, and whatever
	// builds from it stays unpriced with it, an honest gap,
	// never a fossil. The EIV base for job fees follows the
	// planner: adjusted where present, average behind it
	// (fees only).
	leafPrice := func(typeID int64) (float64, bool) {
		if v := typicalSell[typeID]; v > 0 {
			return v, true
		}
		if tv, ok := existing[typeID]; ok {
			if est, ok := storedEstimate(tv); ok {
				return est, true
			}
			if tv.BuildCost > 0 {
				return tv.BuildCost, true
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
	// One coster for the pass: hulls price through RootCost
	// (their own blueprint at hull ME), everything else
	// through UnitCost (component ME throughout).
	coster := buildplan.NewUnitCoster(graph, leafPrice, adjusted, buildplan.CostProfile{
		RootME:      trueValueShipME,
		ComponentME: trueValueComponentME,
		MatFactor:   profile.matFactor,
		CostFactor:  profile.costFactor,
		CostIndex:   profile.costIndex,
		FacilityTax: buildplan.NPCFacilityTax,
	})

	// The thin set, whole: every type contracts may have to
	// speak for this pass. It is the subject set the corpus
	// miner judges contract shapes against (a second thin
	// type aboard a fitted contract makes its subject
	// ambiguous there too).
	thinSubjects := make(map[int64]bool, len(graph.products))
	for _, id := range graph.products {
		if isThinType(dailyVolume, id) {
			thinSubjects[id] = true
		}
	}
	// Evidence-only types: ships with no manufacturing
	// blueprint (prize hulls, limited-run rewards). The BOM
	// walk can never reach them -- no blueprint, no build
	// cost, no row -- yet contracts price them every day.
	// They join the thin set as sampler subjects and are
	// priced from contract evidence alone below.
	var evidenceTypes []int64
	for _, id := range graph.allShips {
		if _, manufacturable := graph.byProduct[id]; manufacturable {
			continue
		}
		if isThinType(dailyVolume, id) {
			thinSubjects[id] = true
			evidenceTypes = append(evidenceTypes, id)
		}
	}

	mined, waiting := app.mineTVCorpus(ctx, thinSubjects, graph.shipsAll)
	observed := make(map[int64]db.TrueValueContractObservation)
	listingsOf := func(typeID int64) ([]tvContractSample, bool) {
		contracts, ok := st.corpus[typeID]
		if !ok {
			return nil, false
		}
		return corpusListingSamples(typeID, contracts, thinSubjects, graph.shipsAll, priceOf, observed), true
	}

	var rows []db.TrueValue
	unpriced := 0
	thinCosts := make(map[int64]float64)
	var pendingShips []int64 // thin hulls owed a row: priced below, by the learned fit
	for _, typeID := range graph.products {
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
		row := db.TrueValue{
			TypeID:         typeID,
			BuildCost:      buildCost,
			ComputedAt:     now,
			ProfileVersion: trueValueProfileVersion,
		}
		if isThinType(dailyVolume, typeID) {
			thinCosts[typeID] = buildCost
			st.fulfilled[typeID] = retainedContractSamples(st.fulfilled[typeID], now)
			if graph.ships[typeID] {
				// Ships price from the learned fit, not the
				// tiered median: the ship step below writes
				// this hull's row from the family model.
				pendingShips = append(pendingShips, typeID)
				continue
			}
			listings, ok := listingsOf(typeID)
			if !ok {
				continue // its row waits for its corpus to be mined
			}
			// The corpus speaks first; the vanished tier and
			// the fulfilled tier follow. The tiered rules judge the samples
			// that corroborate the build-cost anchor. An
			// estimate is stored only when the evidence
			// produces one -- never the build cost standing
			// in for a price.
			samples := freshContractSamples(listings, now)
			samples = append(samples, freshContractSamples(vanished[typeID], now)...)
			samples = append(samples, freshContractSamples(st.fulfilled[typeID], now)...)
			samples = dedupeSamplesByContract(samples)
			var inBand []tvContractSample
			for _, s := range samples {
				if s.price >= tvContractBandLow*buildCost && s.price <= tvContractBandHigh*buildCost {
					inBand = append(inBand, s)
				}
			}
			est, source, count, ok := estimatedSellTiered(inBand)
			if !ok {
				count, source = len(samples), ""
			}
			row.SellSamples = int64(count)
			row.SellSource = source
			if ok {
				row.EstSell = sql.NullFloat64{Float64: est, Valid: true}
			}
		}
		rows = append(rows, row)
	}

	// Thin types the coster cannot price this pass still feed
	// the fit's ladders and the fulfilled harvest's band check
	// at their stored build cost.
	for typeID, row := range existing {
		if _, done := thinCosts[typeID]; !done && row.BuildCost > 0 && isThinType(dailyVolume, typeID) {
			thinCosts[typeID] = row.BuildCost
		}
	}
	rows = append(rows, app.priceThinShips(ctx, pendingShips, evidenceTypes, thinSubjects, thinCosts, vanished, graph, listingsOf, now)...)
	written := app.storeTrueValues(ctx, rows, existing, now)
	app.storeTVObservations(ctx, observed)
	if waiting == 0 {
		st.fingerprint = fingerprint
	} else {
		st.evidenceMoved = true // the rest of the corpus is mined next cycle
	}
	if written > 0 || mined > 0 {
		logging.Infof("true values: %d rows changed (%d types unpriced), corpus read for %d types (%d waiting)",
			written, unpriced, mined, waiting)
	}

	app.harvestFulfilledSamples(ctx, thinCosts, graph.ships, priceOf, now)
	app.purgeContractObservations(ctx, now)
	// The live asks behind unpriced types, from the observations
	// the corpus pass just updated: shown beside the gap, never
	// a price.
	if err := app.queries.RefreshTrueValueAsks(ctx); err != nil {
		logging.Errorf("true values: asks: %v", err)
	}
}

// refreshTVCorpusLive brings the corpus cache in line with the
// contract sweep: each cached contract takes the sweep's latest
// sighting as its stamp, the way a fresh read would, and a
// contract that left the live set leaves the cache. A type
// whose cached list was full is mined again, so an older
// contract the cap held back can take the free place.
func (app *Application) refreshTVCorpusLive(ctx context.Context) {
	st := &app.trueValuePass
	byID := make(map[int64][]*tvCorpusContract)
	for _, contracts := range st.corpus {
		for _, c := range contracts {
			byID[c.id] = append(byID[c.id], c)
		}
	}
	if len(byID) == 0 {
		return
	}
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	live, err := app.queries.ListTVCorpusContractsLive(ctx, ids)
	if err != nil {
		logging.Errorf("true values: corpus sightings: %v", err)
		return
	}
	alive := make(map[int64]bool, len(live))
	for _, row := range live {
		alive[row.ContractID] = true
		for _, c := range byID[row.ContractID] {
			c.lastSeen = row.LastSeen
		}
	}
	if len(alive) == len(byID) {
		return
	}
	for typeID, contracts := range st.corpus {
		kept := contracts[:0]
		for _, c := range contracts {
			if alive[c.id] {
				kept = append(kept, c)
			}
		}
		switch {
		case len(kept) == len(contracts):
			continue
		case len(contracts) >= tvCorpusMaxContracts:
			delete(st.corpus, typeID)
		default:
			st.corpus[typeID] = kept
		}
		st.evidenceMoved = true
	}
	for id := range byID {
		if !alive[id] {
			delete(st.observed, id)
		}
	}
}

// noteTVCorpusArrivals drops from the corpus cache every type
// a newly stored contract carries, so the next pass mines it
// with the new listing among its asks.
func (app *Application) noteTVCorpusArrivals(ctx context.Context) {
	st := &app.trueValuePass
	rows, err := app.queries.ListTVCorpusArrivals(ctx, st.arrivalsSince.Add(-tvArrivalOverlap))
	if err != nil {
		logging.Errorf("true values: corpus arrivals: %v", err)
		return
	}
	for _, row := range rows {
		if _, seen := st.arrivalsSeen[row.ContractID]; seen {
			continue
		}
		st.arrivalsSeen[row.ContractID] = row.StoredAt
		if row.StoredAt.After(st.arrivalsSince) {
			st.arrivalsSince = row.StoredAt
		}
		for _, typeID := range row.TypeIds {
			if _, cached := st.corpus[typeID]; cached {
				delete(st.corpus, typeID)
				st.evidenceMoved = true
			}
		}
	}
	for id, at := range st.arrivalsSeen {
		if at.Before(st.arrivalsSince.Add(-tvArrivalOverlap)) {
			delete(st.arrivalsSeen, id)
		}
	}
}

// mineTVCorpus reads the corpus for the thin types the cache
// lacks, at most tvMinePerCycle of them, hulls first. It
// returns how many it read and how many still wait.
func (app *Application) mineTVCorpus(ctx context.Context, subjects map[int64]bool, ships map[int64]bool) (mined, waiting int) {
	st := &app.trueValuePass
	for typeID := range st.corpus {
		if !subjects[typeID] {
			delete(st.corpus, typeID) // no longer thin: contracts need not speak for it
		}
	}
	var due []int64
	for typeID := range subjects {
		if _, cached := st.corpus[typeID]; !cached {
			due = append(due, typeID)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if ships[due[i]] != ships[due[j]] {
			return ships[due[i]]
		}
		return due[i] < due[j]
	})
	for _, typeID := range due {
		if mined == tvMinePerCycle {
			break
		}
		contracts, err := app.readTVCorpus(ctx, typeID)
		if err != nil {
			logging.Errorf("true values: corpus %d: %v", typeID, err)
			break
		}
		st.corpus[typeID] = contracts
		mined++
	}
	return mined, len(due) - mined
}

// readTVCorpus reads one type's live corpus contracts with
// every stored item line of each (ListTrueValueCorpusContracts).
func (app *Application) readTVCorpus(ctx context.Context, typeID int64) ([]*tvCorpusContract, error) {
	rows, err := app.queries.ListTrueValueCorpusContracts(ctx, db.ListTrueValueCorpusContractsParams{
		TypeID: typeID, Price: tvContractMinPrice, Limit: tvCorpusMaxContracts,
	})
	if err != nil {
		return nil, err
	}
	out := []*tvCorpusContract{}
	var cur *tvCorpusContract
	for _, row := range rows {
		if cur == nil || cur.id != row.ContractID {
			cur = &tvCorpusContract{
				id: row.ContractID, regionID: row.RegionID, price: row.Price,
				firstSeen: row.FirstSeen, lastSeen: row.LastSeen, expiresAt: row.DateExpired,
			}
			out = append(out, cur)
		}
		cur.items = append(cur.items, tvPublicContractItem{TypeID: row.TypeID, Quantity: row.Quantity, IsIncluded: row.IsIncluded})
	}
	return out, nil
}

// storeTrueValues writes the rows whose values differ from the
// stored ones, in one statement per batch, and returns how
// many it wrote. An unchanged row keeps its stored stamp.
func (app *Application) storeTrueValues(ctx context.Context, rows []db.TrueValue, existing map[int64]db.TrueValue, now time.Time) int {
	const batch = 1000
	written := 0
	var p db.UpsertTrueValuesParams
	flush := func() {
		if len(p.TypeIds) == 0 {
			return
		}
		p.ComputedAt, p.ProfileVersion = now, trueValueProfileVersion
		if err := app.queries.UpsertTrueValues(ctx, p); err != nil {
			logging.Errorf("true values: store %d rows: %v", len(p.TypeIds), err)
		} else {
			written += len(p.TypeIds)
		}
		p = db.UpsertTrueValuesParams{}
	}
	for _, row := range rows {
		if old, ok := existing[row.TypeID]; ok && !tvRowChanged(old, row) {
			continue
		}
		p.TypeIds = append(p.TypeIds, row.TypeID)
		p.BuildCosts = append(p.BuildCosts, row.BuildCost)
		p.EstSells = append(p.EstSells, row.EstSell.Float64)
		p.HasEsts = append(p.HasEsts, row.EstSell.Valid)
		p.SellSamples = append(p.SellSamples, row.SellSamples)
		p.SellSources = append(p.SellSources, row.SellSource)
		app.noteStatPackTrueValue(row)
		if len(p.TypeIds) == batch {
			flush()
		}
	}
	flush()
	return written
}

// tvRowChanged reports whether a computed row says something
// the stored one does not.
func tvRowChanged(old, row db.TrueValue) bool {
	return old.BuildCost != row.BuildCost || old.EstSell != row.EstSell ||
		old.SellSamples != row.SellSamples || old.SellSource != row.SellSource ||
		old.ProfileVersion != row.ProfileVersion
}

// tvObservationSame reports whether two observations of a
// contract record the same thing.
func tvObservationSame(a, b db.TrueValueContractObservation) bool {
	return a.ContractID == b.ContractID && a.RegionID == b.RegionID && a.TypeID == b.TypeID &&
		a.Shape == b.Shape && a.ListedPrice == b.ListedPrice && a.SamplePrice == b.SamplePrice &&
		a.FirstSeenAt.Equal(b.FirstSeenAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}

// storeTVObservations records the corpus listings the pass
// accepted, writing only those whose shape or sample moved
// since it last wrote them.
func (app *Application) storeTVObservations(ctx context.Context, observed map[int64]db.TrueValueContractObservation) {
	st := &app.trueValuePass
	var p db.UpsertTVContractObservationsParams
	for id, o := range observed {
		if old, ok := st.observed[id]; ok && tvObservationSame(old, o) {
			continue
		}
		p.ContractIds = append(p.ContractIds, o.ContractID)
		p.RegionIds = append(p.RegionIds, o.RegionID)
		p.TypeIds = append(p.TypeIds, o.TypeID)
		p.Shapes = append(p.Shapes, o.Shape)
		p.ListedPrices = append(p.ListedPrices, o.ListedPrice)
		p.SamplePrices = append(p.SamplePrices, o.SamplePrice)
		p.FirstSeenAts = append(p.FirstSeenAts, o.FirstSeenAt)
		p.ExpiresAts = append(p.ExpiresAts, o.ExpiresAt)
	}
	if len(p.ContractIds) == 0 {
		return
	}
	if err := app.queries.UpsertTVContractObservations(ctx, p); err != nil {
		logging.Errorf("true values: observe %d corpus contracts: %v", len(p.ContractIds), err)
		return
	}
	if st.observed == nil {
		st.observed = make(map[int64]db.TrueValueContractObservation)
	}
	for id, o := range observed {
		st.observed[id] = o
	}
}

// isThinType reports whether a type lacks a real market: The
// Forge trades fewer than trueValueHealthyDailyVolume units of it
// a day (none at all when the sweep holds no row for it). Thin
// types are the ones contracts have to speak for. Only our own
// swept and recorded data decides it.
func isThinType(dailyVolume map[int64]float64, typeID int64) bool {
	return dailyVolume[typeID] < trueValueHealthyDailyVolume
}

// freshContractSamples drops samples older than the window
// they stay meaningful for (tvSampleWindow): the tiered
// estimate's freshness rule, applied where that estimate
// consumes samples.
func freshContractSamples(samples []tvContractSample, now time.Time) []tvContractSample {
	out := samples[:0]
	for _, s := range samples {
		if now.Sub(s.at) < tvSampleWindow {
			out = append(out, s)
		}
	}
	return out
}

// retainedContractSamples drops samples past the evidence
// retention horizon (tvEvidenceRetention): storage hygiene
// for the pass's in-memory sets. Ship fits consume samples
// WITHOUT the freshness filter -- age reaches them as a
// weight (tvSampleWeight), never as this or any cutoff.
func retainedContractSamples(samples []tvContractSample, now time.Time) []tvContractSample {
	out := samples[:0]
	for _, s := range samples {
		if now.Sub(s.at) < tvEvidenceRetention {
			out = append(out, s)
		}
	}
	return out
}

// medianSamplePrice is the median of a sample set's prices.
func medianSamplePrice(samples []tvContractSample) float64 {
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

// estimatedSellTiered spends samples by evidence tier: the
// strongest tier holding at least two samples sets the
// estimate -- its median, its name, and its count. A tier
// with a single sample cannot speak, however strong: one
// observation is an anecdote at every tier. When no tier
// speaks, ok is false and the count is every sample (the
// informational count the stored row carries).
func estimatedSellTiered(samples []tvContractSample) (est float64, source string, count int, ok bool) {
	for _, tier := range []string{tvSampleFulfilled, tvSampleVanished, tvSampleListing} {
		var tierSamples []tvContractSample
		for _, s := range samples {
			if s.source == tier {
				tierSamples = append(tierSamples, s)
			}
		}
		if len(tierSamples) >= 2 {
			return medianSamplePrice(tierSamples), tier, len(tierSamples), true
		}
	}
	return 0, "", len(samples), false
}

// tvCorpusMaxContracts bounds the corpus contracts mined for
// one type at one evaluation: its newest-seen live asks.
const tvCorpusMaxContracts = 40

// corpusListingSamples judges one thin type's cached corpus
// contracts into its listing-tier samples. The corpus is the
// contract sweep's own store of live contracts (every region,
// continuously), so this reads the evidence the whole market
// is showing. A sample's stamp is its contract's last-seen
// time: the ask stands now. Contracts are judged by the shape
// rules (tvContractSampleShape) at current prices but NOT
// banded: banding is the estimate step's business, against
// the type's current anchor.
//
// An accepted contract is also noted as an observation in
// observed, the pass's longitudinal memory, so when the
// listing leaves the corpus its exit can be probed and the
// sample promoted to a likely or confirmed sale
// (truevalue_sales.go): the corpus connection is what lets
// sale evidence accumulate at corpus scale.
func corpusListingSamples(typeID int64, contracts []*tvCorpusContract, subjects map[int64]bool, ships map[int64]bool, priceOf tvStoredPricer, observed map[int64]db.TrueValueContractObservation) []tvContractSample {
	var out []tvContractSample
	for _, c := range contracts {
		subject, unit, shape, ok := tvContractSampleShape(c.price, c.items, subjects, ships, priceOf)
		if !ok || subject != typeID {
			continue
		}
		out = append(out, tvContractSample{price: unit, at: c.lastSeen, source: tvSampleListing, contractID: c.id})
		if !c.expiresAt.IsZero() {
			observed[c.id] = db.TrueValueContractObservation{
				ContractID: c.id, RegionID: c.regionID, TypeID: typeID, Shape: shape,
				ListedPrice: c.price, SamplePrice: unit, FirstSeenAt: c.firstSeen, ExpiresAt: c.expiresAt,
			}
		}
	}
	return out
}

// dedupeSamplesByContract makes one contract speak once, at
// its strongest tier: the same contract can surface as a
// live ask in the corpus and -- once its exit is probed --
// a likely or confirmed sale in the
// observation store, and counting it at every tier would let
// one contract outvote the market.
func dedupeSamplesByContract(samples []tvContractSample) []tvContractSample {
	strength := func(source string) int {
		switch source {
		case tvSampleFulfilled:
			return 3
		case tvSampleVanished:
			return 2
		}
		return 1
	}
	best := make(map[int64]int) // contract ID -> index into out
	out := samples[:0]
	for _, s := range samples {
		if s.contractID == 0 {
			out = append(out, s)
			continue
		}
		if i, seen := best[s.contractID]; seen {
			if strength(s.source) > strength(out[i].source) {
				out[i] = s
			}
			continue
		}
		best[s.contractID] = len(out)
		out = append(out, s)
	}
	return out
}

// priceThinShips is the ship side of the pass: it fits the
// learned pricing model (truevalue_learn.go) over every thin
// ship type's contract evidence and returns the rows for the
// pass's thin hulls (pending) and the evidence-only types (no
// blueprint, hence no build cost and no ladder: priced by
// their learned level inside the same fit). The fit waits
// until every hull's corpus is in the cache: a family fitted
// on part of its evidence would price its hulls wrong.
//
// Evidence assembly per type mirrors the tiered path's: the
// corpus speaks first, then the vanished/confirmed tier
// read back from the
// observation store, and the fulfilled harvest -- deduped
// by contract at the strongest tier.
func (app *Application) priceThinShips(ctx context.Context, pending []int64, evidenceTypes []int64, subjects map[int64]bool, ladders map[int64]float64, vanished map[int64][]tvContractSample, graph *tvBlueprintGraph, listingsOf func(int64) ([]tvContractSample, bool), now time.Time) []db.TrueValue {
	st := &app.trueValuePass
	shipSubjects := make(map[int64]bool, len(subjects))
	subjectIDs := make([]int64, 0, len(subjects))
	for typeID := range subjects {
		if graph.shipsAll[typeID] {
			shipSubjects[typeID] = true
			subjectIDs = append(subjectIDs, typeID)
		}
	}
	if len(shipSubjects) == 0 {
		return nil
	}
	sort.Slice(subjectIDs, func(i, j int) bool { return subjectIDs[i] < subjectIDs[j] })

	samplesByType := make(map[int64][]tvContractSample, len(subjectIDs))
	for _, typeID := range subjectIDs {
		// No freshness filter here, deliberately: hulls
		// move slowly, and the fit ages every sample by
		// weight (tvSampleWeight) -- a confirmed sale from
		// weeks ago still counts, discounted. The 14-day
		// hard window the tiered path keeps is what let
		// real ship estimates lapse.
		samples, ok := listingsOf(typeID)
		if !ok {
			return nil // a hull's corpus is still to be mined
		}
		samples = append(samples, vanished[typeID]...)
		samples = append(samples, st.fulfilled[typeID]...)
		samplesByType[typeID] = dedupeSamplesByContract(samples)
	}

	// The family of a hull is its class: the SDE group name.
	// A type the SDE does not group stands alone as its own
	// family (its fit then rests on its own samples only).
	families := make(map[int64]string, len(subjectIDs))
	for _, typeID := range subjectIDs {
		families[typeID] = fmt.Sprintf("type:%d", typeID)
	}
	if rows, err := app.queries.ListTypeGroupNames(ctx, subjectIDs); err == nil {
		for _, row := range rows {
			if row.Name != "" {
				families[row.TypeID] = row.Name
			}
		}
	} else {
		logging.Errorf("true values: ship families: %v", err)
	}
	pricing := buildTVShipPricing(shipSubjects, ladders, samplesByType, families, now)

	rows := make([]db.TrueValue, 0, len(pending)+len(evidenceTypes))
	for _, list := range [][]int64{pending, evidenceTypes} {
		for _, typeID := range list {
			est, source, count, ok := pricing.price(typeID)
			row := db.TrueValue{
				TypeID:         typeID,
				BuildCost:      ladders[typeID],
				ComputedAt:     now,
				ProfileVersion: trueValueProfileVersion,
				SellSamples:    int64(count),
				SellSource:     source,
			}
			if ok {
				row.EstSell = sql.NullFloat64{Float64: est, Valid: true}
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// tvStoredPricer prices a type from stored data alone, in
// the house order: The Forge's swept typical sell, then the
// stored true-value estimated sell, then the stored build
// cost (a cost input to the netting arithmetic, quoted as
// nobody's price). CCP's published prices are never a price
// here. The contract sampler nets a fitted contract's other
// items out at these prices; ok=false when none of them knows
// the type, which rejects the sample rather than guessing.
type tvStoredPricer func(typeID int64) (float64, bool)

func tvStoredPricerFrom(typicalSell map[int64]float64, stored map[int64]db.TrueValue) tvStoredPricer {
	return func(typeID int64) (float64, bool) {
		if v := typicalSell[typeID]; v > 0 {
			return v, true
		}
		if tv, ok := stored[typeID]; ok {
			if est, ok := storedEstimate(tv); ok {
				return est, true
			}
			if tv.BuildCost > 0 {
				return tv.BuildCost, true
			}
		}
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// Contracts: the sell-side signal for thin types, in three
// evidence tiers (schema 052). A contract is evidence only in
// context -- public contracts are full of scams, mislabels
// and bundled fits -- so every sample has to earn it: the
// item list must name the thin type cleanly (see
// tvContractSampleShape), and where a build-cost anchor
// exists the price must sit within its band. The listing
// tier's main source is the stored public-contract corpus,
// mined at evaluation time (corpusListingSamples), which
// also records the listings it accepts as observations. The
// public contract sweep owns every fetch of the public
// routes; the pass reads what it stored. A departed listing
// gets one exit probe (probeContractExits), which is how
// listings graduate to the vanished and fulfilled tiers the
// corpus, holding only live contracts, can never see leave. The tiers are the evidence's
// provenance and its weight: thin non-ship types price from
// the strongest tier holding two samples; thin ships feed
// every tier into the learned correction
// (truevalue_learn.go), confirmed sales weighted highest.
//
//   fulfilled -- contracts of linked characters that ESI
//     reports finished: certain sales, harvested from the
//     sync's own stores (harvestFulfilledSamples).
//   vanished -- public listings the corpus accepted and that
//     left before expiry: likely sold. An
//     issuer can delete a listing instead, so this tier is
//     probable, never certain, and is labelled that way.
//   listing -- live public listings: asks, as before.
// ---------------------------------------------------------------------------

const (
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
)

// tvPublicContractItem is one item row of a public contract,
// as the corpus stores it and ESI's items route answers it.
type tvPublicContractItem struct {
	TypeID     int64 `json:"type_id"`
	Quantity   int64 `json:"quantity"`
	IsIncluded bool  `json:"is_included"`
}

// purgeContractObservations is the observation store's
// housekeeping: a watched listing that expired without ever
// vanishing can never become evidence now, and an exited row
// is kept to the evidence retention horizon -- ship fits
// still count its sale, age-weighted, long after it leaves.
func (app *Application) purgeContractObservations(ctx context.Context, now time.Time) {
	if err := app.queries.PurgeTVContractObservations(ctx, db.PurgeTVContractObservationsParams{
		ExpiresAt:  now.Add(-tvContractVanishMargin),
		VanishedAt: sql.NullTime{Time: now.Add(-tvEvidenceRetention), Valid: true},
	}); err != nil {
		logging.Errorf("true values: purge observations: %v", err)
	}
}

// tvVanishedBeforeExpiry reports whether a listing seen gone
// at `now` disappeared too long before its expiry for the
// expiry itself to explain the exit.
func tvVanishedBeforeExpiry(now, expiresAt time.Time) bool {
	return now.Before(expiresAt.Add(-tvContractVanishMargin))
}

// The shapes tvContractSampleFor accepts.
const (
	tvContractShapeSingle = "single"
	tvContractShapeFitted = "fitted"
)

// tvContractSampleFor is the strict filter, run on a contract's
// item list: the shape test (tvContractSampleShape), plus the
// band around the subject's build cost. The fulfilled harvest
// admits samples through it; the stored-corpus miner runs the
// shape test alone and leaves
// banding to the estimate step, which is what lets contract
// evidence that flatly contradicts an anchor (or a type with
// no anchor at all) still be heard there.
func tvContractSampleFor(price float64, items []tvPublicContractItem, thinCosts map[int64]float64, ships map[int64]bool, priceOf tvStoredPricer) (typeID int64, unitPrice float64, shape string, ok bool) {
	subjects := make(map[int64]bool, len(thinCosts))
	for id := range thinCosts {
		subjects[id] = true
	}
	typeID, unitPrice, shape, ok = tvContractSampleShape(price, items, subjects, ships, priceOf)
	if !ok {
		return 0, 0, "", false
	}
	anchor := thinCosts[typeID]
	if unitPrice < tvContractBandLow*anchor || unitPrice > tvContractBandHigh*anchor {
		return 0, 0, "", false
	}
	return typeID, unitPrice, shape, true
}

// tvContractSampleShape judges a contract's structure alone.
// Two shapes earn a sample:
//
//   - single: the contract is exactly one thin type (a single
//     unit of it, for a ship hull).
//   - fitted: the contract carries exactly one unit of one
//     thin ship hull plus other items -- the way titans
//     actually trade, never bare. The other items are netted
//     out at their stored prices (priceOf) and the residual
//     is the hull's implied price. A second thin type aboard
//     makes the subject ambiguous and rejects the contract,
//     as does any item stored prices cannot value: the
//     residual must be arithmetic, never a guess.
//
// subjects is the thin set the subject must belong to.
func tvContractSampleShape(price float64, items []tvPublicContractItem, subjects map[int64]bool, ships map[int64]bool, priceOf tvStoredPricer) (typeID int64, unitPrice float64, shape string, ok bool) {
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
			if !subjects[id] {
				return 0, 0, "", false
			}
			if ships[id] && qty != 1 {
				return 0, 0, "", false
			}
			return id, price / float64(qty), tvContractShapeSingle, true
		}
	}
	// Fitted shape: one candidate hull, everything else
	// netted away.
	var hull int64
	for id, qty := range byType {
		if subjects[id] {
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
	return hull, price - net, tvContractShapeFitted, true
}

// ---------------------------------------------------------------------------
// Fulfilled contracts: the certain tier. The public corpus can
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
			if completed.IsZero() || now.Sub(completed) >= tvEvidenceRetention || completed.After(now) {
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
					price: unit, at: completed, source: tvSampleFulfilled, contractID: c.ContractID,
				})
				st.fulfilledSeen[c.ContractID] = true
				st.evidenceMoved = true
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
