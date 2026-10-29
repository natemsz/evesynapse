package app

// Per-blueprint industry P&L (schema 113). Three figures per
// blueprint over the same runs:
//
//   - (a) realized: what the units actually sold (or marked lost)
//     brought, less their tax and each unit's share of its job's cost:
//     the materials as FIFO consumed them from the pool's stock when
//     the job was installed (any no lot covered at the install-day
//     price, flagged), plus the job cost ESI reports;
//   - (b) at the time: inputs at the install-day stat pack, output at
//     the delivery-day house price, the job cost as paid;
//   - (c) at today's prices: the same runs' recipe at today's stat
//     pack, in the region and on the side the player chose, less tax
//     and broker fees at the pool's observed rates and the job cost
//     scaled by how the inputs' value moved since.
//
// ESI reports neither the materials a job consumed nor the ME it ran
// at: materials are derived from the SDE recipe, the ME read when the
// job is first seen delivered (a blueprint cannot be researched while
// it runs a job) and the facility's material bonus the player
// declared, each stamped with its source. Recipe values are shared:
// one row per region, blueprint, ME and bonus, rewritten when the
// region's sweep completes.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// jobMaterialQty is one recipe line for a job's runs: at least one
// unit per run, after ME and the facility's material bonus.
func jobMaterialQty(base, runs, me int64, bonusPct float64) int64 {
	factor := (1 - float64(min(max(me, 0), 10))/100) * (1 - math.Min(math.Max(bonusPct, 0), 50)/100)
	return max(runs, int64(math.Ceil(float64(base*runs)*factor)))
}

// landDeliveredJobs stores the owner's newly delivered manufacturing
// and reaction jobs with their recipe facts: the units made and their
// house value that day (v1's industry line), the materials derived for
// their runs, those inputs at their install-day price, the job cost
// and the estimated item value. It returns the product and material
// types they move and the earliest install among them.
func (app *Application) landDeliveredJobs(ctx context.Context, src ledgerSource, now time.Time) ([]int64, time.Time, error) {
	var jobs esi.IndustryJobs
	if json.Unmarshal([]byte(src.jobs.Payload), &jobs) != nil {
		return nil, time.Time{}, nil
	}
	var delivered []esi.IndustryJob
	var ids []int64
	for _, j := range jobs {
		if j.Status == "delivered" && (j.ActivityID == 1 || j.ActivityID == 11 || j.ActivityID == 8) && j.ProductTypeID > 0 && j.BlueprintTypeID > 0 {
			delivered = append(delivered, j)
			ids = append(ids, j.JobID)
		}
	}
	if len(delivered) == 0 {
		return nil, time.Time{}, nil
	}
	known, err := app.queries.ListLedgerJobIDs(ctx, db.ListLedgerJobIDsParams{OwnerKind: src.kind, OwnerID: src.ownerID, JobIds: ids})
	if err != nil {
		return nil, time.Time{}, err
	}
	seen := map[int64]bool{}
	for _, id := range known {
		seen[id] = true
	}
	me := map[int64]int64{}
	if src.blueprint != nil {
		var bps esi.Blueprints
		if json.Unmarshal([]byte(src.blueprint.Payload), &bps) == nil {
			for _, b := range bps {
				me[b.ItemID] = b.MaterialEfficiency
			}
		}
	}
	prefs := app.profitPrefsFor(ctx, src.userID)
	bonuses := map[int64]float64{}
	if rows, err := app.queries.ListUserFacilityBonuses(ctx, src.userID); err == nil {
		for _, b := range rows {
			bonuses[b.FacilityID] = b.MaterialBonus
		}
	}
	typicals := app.TypicalSells(ctx)
	estimates := app.storedTrueValues(ctx)
	priceNow := func(typeID int64) (float64, bool) {
		return pickValuationPrice(typicals[typeID], estSellOf(estimates, typeID))
	}

	touched := map[int64]bool{}
	var earliest time.Time
	for _, j := range delivered {
		if seen[j.JobID] {
			continue
		}
		at, err := time.Parse(time.RFC3339, j.CompletedDate)
		if err != nil {
			if at, err = time.Parse(time.RFC3339, j.EndDate); err != nil {
				at = now.UTC()
			}
		}
		installed, err := time.Parse(time.RFC3339, j.StartDate)
		if err != nil {
			installed = at
		}
		p := db.UpsertLedgerJobParams{
			OwnerKind: src.kind, OwnerID: src.ownerID, JobID: j.JobID, At: at, ActivityID: j.ActivityID,
			ProductTypeID: j.ProductTypeID, BlueprintTypeID: j.BlueprintTypeID, Runs: j.Runs, MeSource: "unknown",
			BonusSource: "none_assumed", FacilityID: j.FacilityID, InstalledAt: sql.NullTime{Time: installed, Valid: true},
			JobCost: j.Cost, PriceRegion: prefs.IndustryRegion, InputsSource: "unpriced",
		}
		if p.FacilityID == 0 {
			p.FacilityID = j.StationID
		}
		if v, ok := me[j.BlueprintID]; ok {
			p.Me, p.MeSource = v, "at_delivery"
		}
		if b, ok := bonuses[p.FacilityID]; ok {
			p.MaterialBonus, p.BonusSource = b, "user_declared"
		}
		if j.ActivityID == 8 {
			// Invention takes its datacores whole: no ME, no structure bonus.
			p.Me, p.MeSource, p.MaterialBonus, p.BonusSource = 0, "unknown", 0, "none_assumed"
		}
		mats, units, err := app.jobRecipe(ctx, j, p.Me, p.MaterialBonus)
		p.Units = units
		if err == nil {
			app.priceJob(ctx, &p, mats, priceNow, prefs)
		}
		if j.ActivityID == 8 {
			p.Priced = false // a blueprint copy is never stock to sell
		}
		if err := app.queries.UpsertLedgerJob(ctx, p); err != nil {
			return nil, time.Time{}, err
		}
		if len(mats) > 0 {
			mp := db.UpsertJobMaterialsBatchParams{OwnerKind: src.kind, OwnerID: src.ownerID, JobID: j.JobID}
			for _, m := range mats {
				mp.TypeIds = append(mp.TypeIds, m.typeID)
				mp.Quantities = append(mp.Quantities, m.qty)
				mp.UnitsAtTime = append(mp.UnitsAtTime, m.unitAtTime)
				touched[m.typeID] = true
			}
			if err := app.queries.UpsertJobMaterialsBatch(ctx, mp); err != nil {
				return nil, time.Time{}, err
			}
		}
		touched[j.ProductTypeID] = true
		if earliest.IsZero() || installed.Before(earliest) {
			earliest = installed
		}
	}
	types := make([]int64, 0, len(touched))
	for id := range touched {
		types = append(types, id)
	}
	return types, earliest, nil
}

// jobMaterial is one derived input of a job.
type jobMaterial struct {
	typeID, base, qty int64
	unitAtTime        float64
}

// jobRecipe derives a job's inputs and output units from the SDE.
func (app *Application) jobRecipe(ctx context.Context, j esi.IndustryJob, me int64, bonus float64) ([]jobMaterial, int64, error) {
	if j.ActivityID == 8 {
		return app.inventionRecipe(ctx, j)
	}
	bp, err := app.queries.GetSDEBlueprint(ctx, j.BlueprintTypeID)
	if err != nil {
		return nil, 0, err
	}
	runs := j.SuccessfulRuns
	if runs <= 0 {
		runs = j.Runs
	}
	rows, err := app.queries.ListSDEBlueprintMaterials(ctx, j.BlueprintTypeID)
	if err != nil {
		return nil, 0, err
	}
	mats := make([]jobMaterial, 0, len(rows))
	for _, m := range rows {
		mats = append(mats, jobMaterial{typeID: m.MaterialTypeID, base: m.Quantity, qty: jobMaterialQty(m.Quantity, j.Runs, me, bonus)})
	}
	return mats, runs * max(bp.ProductQuantity, 1), nil
}

// priceJob fills a landing job's values: output and materials at the
// house prices now (v1's industry line, which also prices the output
// lot), inputs at the stat pack of the install day in the player's
// region and side (house prices now where a type had no row that
// day, stamped), and the estimated item value from CCP's adjusted
// prices, which only the game's job cost formula uses.
func (app *Application) priceJob(ctx context.Context, p *db.UpsertLedgerJobParams, mats []jobMaterial, priceNow func(int64) (float64, bool), prefs profitPrefs) {
	unit, ok := priceNow(p.ProductTypeID)
	p.Priced = ok && p.Units > 0 && len(mats) > 0
	p.OutputValue = float64(p.Units) * unit
	ids := make([]int64, 0, len(mats))
	for _, m := range mats {
		ids = append(ids, m.typeID)
	}
	day := map[int64]float64{}
	if rows, err := app.queries.ListRegionStatsDailyForTypes(ctx, db.ListRegionStatsDailyForTypesParams{
		RegionID: prefs.IndustryRegion, Day: p.InstalledAt.Time.UTC().Format("2006-01-02"), TypeIds: ids,
	}); err == nil {
		for _, r := range rows {
			v := r.TypicalSell
			if prefs.IndustrySide == module.AppraisalModeBuy {
				v = r.TypicalBuy
			}
			if v > 0 {
				day[r.TypeID] = v
			}
		}
	}
	adjusted := map[int64]float64{}
	if rows, err := app.queries.ListAdjustedPrices(ctx, ids); err == nil {
		for _, r := range rows {
			adjusted[r.TypeID] = r.AdjustedPrice
		}
	}
	p.InputsSource = "install_day"
	for i := range mats {
		m := &mats[i]
		now, okNow := priceNow(m.typeID)
		if !okNow {
			p.Priced = false
		}
		p.MaterialsCost += float64(m.qty) * now
		if v, ok := day[m.typeID]; ok {
			m.unitAtTime = v
		} else if okNow {
			m.unitAtTime = now
			if p.InputsSource == "install_day" {
				p.InputsSource = "at_landing"
			}
		} else {
			p.InputsSource = "unpriced"
		}
		p.InputsAtTime += float64(m.qty) * m.unitAtTime
		p.Eiv += adjusted[m.typeID] * float64(m.base*p.Runs)
	}
}

// writeJobSettlements stores what the matching settled for jobs in a
// type's stream from the checkpoint on: the stock each job's materials
// drew (a consumption), and what became of job output (a sale's takes
// from job lots, clean or beside bought stock, or tied by the player;
// units the player sold off-market or marked lost).
func writeJobSettlements(ctx context.Context, qtx *db.Queries, pool ledgerPool, typeID int64, cp time.Time, sales []realizedSale) error {
	if err := qtx.DeleteJobOutputLinksFrom(ctx, db.DeleteJobOutputLinksFromParams{PoolKind: pool.kind, PoolID: pool.id, TypeID: typeID, FromAt: cp}); err != nil {
		return err
	}
	p := db.InsertJobOutputLinksBatchParams{PoolKind: pool.kind, PoolID: pool.id, TypeID: typeID}
	link := func(job int64, kind string, ref, owner int64, at time.Time, units int64, revenue float64) {
		p.JobIds = append(p.JobIds, job)
		p.LinkKinds = append(p.LinkKinds, kind)
		p.RefIds = append(p.RefIds, ref)
		p.OwnerIds = append(p.OwnerIds, owner)
		p.Ats = append(p.Ats, at.UTC().Format(time.RFC3339))
		p.Units = append(p.Units, units)
		p.Revenues = append(p.Revenues, revenue)
	}
	for _, s := range sales {
		switch s.Kind {
		case lotConsume:
			if err := qtx.UpdateJobMaterialFIFO(ctx, db.UpdateJobMaterialFIFOParams{
				OwnerKind: pool.ownerKind, OwnerID: s.Owner, JobID: -s.RefID, TypeID: typeID, FifoQty: s.Quantity, FifoCost: s.Cost,
			}); err != nil {
				return err
			}
		case lotTxn:
			for _, t := range s.Takes {
				if !isJobRef(t.RefID) {
					continue
				}
				kind := "fifo_clean"
				switch {
				case t.RefID == s.PreferRef:
					kind = "linked"
				case t.Mixed:
					kind = "fifo_mixed"
				}
				link(-t.RefID, kind, s.RefID, s.Owner, s.At, t.Quantity, float64(t.Quantity)*s.Unit)
			}
		case lotContract:
			for _, t := range s.Takes {
				if isJobRef(t.RefID) {
					link(-t.RefID, "contract", s.RefID, s.Owner, s.At, t.Quantity, float64(t.Quantity)*s.Unit)
				}
			}
		case lotManualSale, lotLost:
			units := s.Quantity + s.UnmatchedQty
			kind, revenue := "lost", 0.0
			if s.Kind == lotManualSale {
				kind, revenue = "manual_sale", float64(units)*s.Unit
			}
			link(-s.PreferRef, kind, s.RefID, s.Owner, s.At, units, revenue)
		}
	}
	if len(p.JobIds) == 0 {
		return nil
	}
	return qtx.InsertJobOutputLinksBatch(ctx, p)
}

// industryTradeRates are a pool's observed sales tax and broker fee
// rates over the last 90 days, which today's figure charges on output.
func (app *Application) industryTradeRates(ctx context.Context, pool ledgerPool, now time.Time) (tax, broker float64) {
	r, err := app.queries.GetPoolTradeRates(ctx, db.GetPoolTradeRatesParams{OwnerKind: pool.ownerKind, OwnerIds: pool.members, Since: now.AddDate(0, 0, -90)})
	if err != nil || r.Revenue <= 0 {
		return 0, 0
	}
	return math.Min(r.Tax/r.Revenue, 0.15), math.Min(r.Broker/r.Revenue, 0.15)
}

// foldIndustrySummary rewrites a character pool's per-blueprint rows
// and today's figure for them, computing any recipe value they ask
// for that no sweep has stored yet.
func (app *Application) foldIndustrySummary(ctx context.Context, pool ledgerPool, now time.Time) error {
	if pool.ownerKind != ownerChar {
		return nil // corporation jobs are not ingested
	}
	prefs := app.profitPrefsFor(ctx, pool.userID)
	tax, broker := app.industryTradeRates(ctx, pool, now)
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteIndustrySummaryForPool(ctx, db.DeleteIndustrySummaryForPoolParams{PoolKind: pool.kind, PoolID: pool.id}); err != nil {
		return err
	}
	if err := qtx.FoldIndustrySummary(ctx, db.FoldIndustrySummaryParams{
		PoolKind: pool.kind, PoolID: pool.id, RegionID: prefs.IndustryRegion, Side: prefs.IndustrySide,
		TaxRate: tax, BrokerRate: broker, FoldedAt: now.UTC(), OwnerKind: pool.ownerKind, OwnerIds: pool.members,
	}); err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	missing, err := app.queries.ListMissingRecipeKeysForPool(ctx, db.ListMissingRecipeKeysForPoolParams{PoolKind: pool.kind, PoolID: pool.id})
	if err != nil {
		return err
	}
	for _, k := range missing {
		v, err := app.computeRecipeValue(ctx, k.RegionID, k.BlueprintTypeID, k.Me, k.BonusBp, now)
		if err != nil {
			continue
		}
		if err := app.queries.UpsertIndustryRecipeValue(ctx, v); err != nil {
			return err
		}
	}
	return app.queries.ApplyIndustryRecipeValues(ctx, db.ApplyIndustryRecipeValuesParams{PoolKind: pool.kind, PoolID: pool.id})
}

// computeRecipeValue prices one recipe per run at today's stat pack
// in a region: inputs and output on both sides through the house
// order (own swept typical, stored estimate, labeled fallback), and
// the estimated item value. Per-run quantities are unrounded (the
// game rounds per job, by its runs). A line with no price on a side
// leaves the recipe unpriced and names the type.
func (app *Application) computeRecipeValue(ctx context.Context, regionID, bpID, me int64, bonusBP int32, now time.Time) (db.UpsertIndustryRecipeValueParams, error) {
	v := db.UpsertIndustryRecipeValueParams{RegionID: regionID, BlueprintTypeID: bpID, Me: me, BonusBp: bonusBP, ComputedAt: now.UTC()}
	bp, err := app.queries.GetSDEBlueprint(ctx, bpID)
	if err != nil {
		return v, err
	}
	mats, err := app.queries.ListSDEBlueprintMaterials(ctx, bpID)
	if err != nil || len(mats) == 0 {
		return v, errors.New("no recipe")
	}
	v.ProductTypeID, v.UnitsPerRun = bp.ProductTypeID, max(bp.ProductQuantity, 1)
	ids := []int64{bp.ProductTypeID}
	for _, m := range mats {
		ids = append(ids, m.MaterialTypeID)
	}
	packs, err := app.loadStatPacks(ctx, regionID, ids)
	if err != nil {
		return v, err
	}
	adjusted := map[int64]float64{}
	if rows, err := app.queries.ListAdjustedPrices(ctx, ids); err == nil {
		for _, r := range rows {
			adjusted[r.TypeID] = r.AdjustedPrice
		}
	}
	v.Priced = true
	price := func(typeID int64, mode string) float64 {
		pk := packs[typeID]
		if pk == nil {
			v.Priced, v.MissingTypeID = false, typeID
			return 0
		}
		p := resolveAppraisalPriceMode(pk.Own, pk.Estimate, pk.Fallback, now, mode)
		if !p.ok {
			if v.Priced {
				v.Priced, v.MissingTypeID = false, typeID
			}
			return 0
		}
		if p.tier == module.AppraisalTierFallback {
			v.FallbackUsed = true
		}
		return p.unit
	}
	factor := (1 - float64(min(max(me, 0), 10))/100) * (1 - math.Min(math.Max(float64(bonusBP)/100, 0), 50)/100)
	for _, m := range mats {
		qty := math.Max(1, float64(m.Quantity)*factor)
		v.InputsSell += qty * price(m.MaterialTypeID, module.AppraisalModeSell)
		v.InputsBuy += qty * price(m.MaterialTypeID, module.AppraisalModeBuy)
		v.Eiv += adjusted[m.MaterialTypeID] * float64(m.Quantity)
	}
	units := float64(v.UnitsPerRun)
	v.OutputSell = units * price(bp.ProductTypeID, module.AppraisalModeSell)
	v.OutputBuy = units * price(bp.ProductTypeID, module.AppraisalModeBuy)
	return v, nil
}

// refreshIndustryRecipeValues reprices, after a region's sweep, every
// recipe some summary asks for in that region, and sets today's
// figure on those summaries. Best-effort: a failure is logged.
func (app *Application) refreshIndustryRecipeValues(ctx context.Context, regionID int64, now time.Time) {
	keys, err := app.queries.ListIndustryRecipeKeys(ctx, regionID)
	if err != nil {
		logging.Errorf("worker: industry recipe values for %s: %v", marketRegionLabel(regionID), err)
		return
	}
	for _, k := range keys {
		v, err := app.computeRecipeValue(ctx, regionID, k.BlueprintTypeID, k.Me, k.BonusBp, now)
		if err != nil {
			continue
		}
		if err := app.queries.UpsertIndustryRecipeValue(ctx, v); err != nil {
			logging.Errorf("worker: industry recipe value %d: %v", k.BlueprintTypeID, err)
			return
		}
	}
	if err := app.queries.ApplyIndustryRecipeValues(ctx, db.ApplyIndustryRecipeValuesParams{RegionID: regionID}); err != nil {
		logging.Errorf("worker: industry today's figures for %s: %v", marketRegionLabel(regionID), err)
	}
}

// inventionRecipe derives an invention job's inputs and output from
// the SDE's invention rows: every attempt takes its datacores, and
// each success makes one T2 blueprint copy of the SDE's runs, so the
// output is copy runs (what a T2 build spends). Its product, a copy,
// has no market price: the job never stands as stock.
func (app *Application) inventionRecipe(ctx context.Context, j esi.IndustryJob) ([]jobMaterial, int64, error) {
	runs, err := app.queries.GetSDEInvention(ctx, db.GetSDEInventionParams{BlueprintTypeID: j.BlueprintTypeID, ProductTypeID: j.ProductTypeID})
	if err != nil {
		return nil, 0, err
	}
	rows, err := app.queries.ListSDEInventionMaterials(ctx, j.BlueprintTypeID)
	if err != nil {
		return nil, 0, err
	}
	mats := make([]jobMaterial, 0, len(rows))
	for _, m := range rows {
		mats = append(mats, jobMaterial{typeID: m.MaterialTypeID, base: m.Quantity, qty: m.Quantity * j.Runs})
	}
	return mats, max(j.SuccessfulRuns, 0) * max(runs, 1), nil
}

// landContractSales stores the lines of the character's completed item
// exchange contracts it issued (not for its corporation) that it sold
// items on: each included item leaves the pool's stock when the
// contract completed. ESI prices a contract whole, so its price is
// split across the lines by their house value, or by quantity when no
// line has one. It returns the types they move and the earliest
// completion among the new ones.
func (app *Application) landContractSales(ctx context.Context, src ledgerSource) ([]int64, time.Time, error) {
	if src.kind != ownerChar {
		return nil, time.Time{}, nil
	}
	var contracts esi.Contracts
	if json.Unmarshal([]byte(src.contracts.Payload), &contracts) != nil {
		return nil, time.Time{}, nil
	}
	typicals := app.TypicalSells(ctx)
	estimates := app.storedTrueValues(ctx)
	touched := map[int64]bool{}
	var earliest time.Time
	for _, c := range contracts {
		if c.Type != "item_exchange" || c.Status != "finished" || c.IssuerID != src.ownerID || c.ForCorporation || c.Price <= 0 {
			continue
		}
		at, err := time.Parse(time.RFC3339, c.DateCompleted)
		if err != nil {
			continue
		}
		detail, err := app.queries.GetContractDetail(ctx, c.ContractID)
		if err != nil {
			continue // its items have not been fetched yet
		}
		var items []esi.ContractItem
		if json.Unmarshal([]byte(detail.Payload), &items) != nil {
			continue
		}
		qty := map[int64]int64{}
		for _, it := range items {
			if it.IsIncluded && it.Quantity > 0 {
				qty[it.TypeID] += it.Quantity
			}
		}
		if len(qty) == 0 {
			continue
		}
		p := db.InsertLedgerContractItemsBatchParams{OwnerKind: src.kind, OwnerID: src.ownerID, ContractID: c.ContractID, At: at, SplitBy: "value"}
		value, units := 0.0, int64(0)
		weight := map[int64]float64{}
		for t, n := range qty {
			if price, ok := pickValuationPrice(typicals[t], estSellOf(estimates, t)); ok {
				weight[t] = price * float64(n)
				value += weight[t]
			}
			units += n
		}
		if value <= 0 {
			p.SplitBy = "quantity"
		}
		for t, n := range qty {
			share := float64(n) / float64(units)
			if value > 0 {
				share = weight[t] / value
			}
			p.TypeIds = append(p.TypeIds, t)
			p.Quantities = append(p.Quantities, n)
			p.PriceShares = append(p.PriceShares, c.Price*share)
		}
		rows, err := app.queries.InsertLedgerContractItemsBatch(ctx, p)
		if err != nil {
			return nil, time.Time{}, err
		}
		for _, t := range rows {
			touched[t] = true
		}
		if len(rows) > 0 && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	types := make([]int64, 0, len(touched))
	for t := range touched {
		types = append(types, t)
	}
	return types, earliest, nil
}
