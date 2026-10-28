package app

// The profit ledger (schema 104). ESI keeps 30 days of wallet
// journal and transactions; the snapshots hold the latest window
// only. After each character's pass the worker folds whatever those
// snapshots carry that the ledger has not seen into durable rows,
// for the character and for every corporation division journal the
// character can read, and then writes the attribution from them:
//
//   - trading: each sell matched first-in first-out against the
//     owner's earlier buys and job output of that type; revenue less
//     the matched cost, with sales tax and broker fees from the
//     journal as trading costs;
//   - industry: a delivered job's output valued at the house price
//     that day, less its materials at the same prices (after the
//     blueprint's material efficiency) and the journal's job fees.
//     The output then stands as stock at that value, so selling it
//     later is a trade against that basis;
//   - planetary industry and other sales: sells no lot covers, split
//     by whether the type is a planetary commodity, with the
//     planetary taxes as planetary costs;
//   - bounties and missions, contracts, transfers and upkeep from the
//     journal's own entry types, each with its own fees;
//   - for characters, the day's unattributed remainder: the net worth
//     change no activity explains (price moves on held stock, losses,
//     anything the ledger cannot see).
//
// Prices are the house order's (swept typicals, then stored
// estimates); nothing here reads CCP's guide average. It runs on
// local reads only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	ownerChar = "char"
	ownerCorp = "corp"
)

// Activities as stored in ledger_journal and profit_daily.
const (
	actTrading      = "trading"
	actIndustry     = "industry"
	actBounties     = "bounties"
	actPI           = "pi"
	actContracts    = "contracts"
	actSales        = "sales"
	actTransfers    = "transfers"
	actUpkeep       = "upkeep"
	actOther        = "other"
	actUnattributed = "unattributed"
	actMarket       = "market" // market buys and sells: the transactions speak for them
	actEscrow       = "escrow" // cash parked in buy orders: net worth counts escrow
)

// journalActivity files a journal entry type under its activity.
func journalActivity(refType string, amount float64) string {
	switch refType {
	case "market_transaction":
		return actMarket
	case "market_escrow":
		return actEscrow
	case "transaction_tax", "brokers_fee", "market_provider_tax":
		return actTrading
	case "bounty_prizes", "bounty_prize", "agent_mission_reward", "agent_mission_time_bonus_reward",
		"ess_escrow_transfer", "corporate_reward_payout", "daily_goal_payouts", "project_discovery_reward",
		"resource_wars_reward", "agent_mission_collateral_refunded":
		return actBounties
	case "industry_job_tax", "manufacturing", "researching_technology", "researching_time_productivity",
		"researching_material_productivity", "copying", "reverse_engineering", "reaction", "reprocessing_tax":
		return actIndustry
	case "planetary_import_tax", "planetary_export_tax", "planetary_construction":
		return actPI
	case "contract_price", "contract_reward", "contract_collateral", "contract_collateral_refund",
		"contract_collateral_payout", "contract_deposit", "contract_deposit_refund", "contract_brokers_fee",
		"contract_sales_tax", "contract_price_payment_corp", "contract_reward_deposited", "contract_reward_refund",
		"contract_auction_bid", "contract_auction_bid_refund", "contract_auction_sold", "contract_auction_bid_corp",
		"contract_deposit_sales_tax", "contract_reversal", "contract_reward_deposited_corp", "contract_brokers_fee_corp":
		return actContracts
	case "player_donation", "player_trading", "corporation_account_withdrawal", "union_payment",
		"corporate_reward_tax", "cash_out", "structure_gate_jump_payment":
		return actTransfers
	}
	if amount < 0 {
		return actUpkeep // clone, skill, office, war and other running costs
	}
	return actOther // insurance and the rest
}

// ---------------------------------------------------------------------------
// First-in first-out matching.
// ---------------------------------------------------------------------------

// ledgerLot is one stock movement of a type: a buy or job output
// (adds), or a sell (removes).
type ledgerLot struct {
	At       time.Time
	RefID    int64 // transaction id; minus the job id for job output
	Sell     bool
	Quantity int64
	Unit     float64
}

// realizedSale is one sell as the matching settled it.
type realizedSale struct {
	RefID            int64
	At               time.Time
	Quantity         int64 // covered by earlier lots
	Revenue, Cost    float64
	UnmatchedQty     int64
	UnmatchedRevenue float64
}

// openLot is stock still held from a buy or job output.
type openLot struct {
	Quantity int64
	Unit     float64
}

// matchFIFO settles sells against the earliest open lots. Lots must
// be in time order. It returns every sell's settlement and the lots
// left open, oldest first.
func matchFIFO(lots []ledgerLot) ([]realizedSale, []openLot) {
	var open []openLot
	var sales []realizedSale
	for _, l := range lots {
		if l.Quantity <= 0 {
			continue
		}
		if !l.Sell {
			open = append(open, openLot{Quantity: l.Quantity, Unit: l.Unit})
			continue
		}
		s := realizedSale{RefID: l.RefID, At: l.At}
		left := l.Quantity
		for left > 0 && len(open) > 0 {
			take := min(left, open[0].Quantity)
			s.Quantity += take
			s.Revenue += float64(take) * l.Unit
			s.Cost += float64(take) * open[0].Unit
			open[0].Quantity -= take
			left -= take
			if open[0].Quantity == 0 {
				open = open[1:]
			}
		}
		if left > 0 {
			s.UnmatchedQty = left
			s.UnmatchedRevenue = float64(left) * l.Unit
		}
		sales = append(sales, s)
	}
	return sales, open
}

// capOpenLots trims open lots, newest first, down to what the owner
// still holds: stock used in industry, lost, or moved away is no
// longer a position.
func capOpenLots(open []openLot, held int64) (qty int64, cost float64) {
	var total int64
	for _, l := range open {
		total += l.Quantity
	}
	drop := total - max(held, 0)
	for i := len(open) - 1; i >= 0 && drop > 0; i-- {
		d := min(drop, open[i].Quantity)
		open[i].Quantity -= d
		drop -= d
	}
	for _, l := range open {
		qty += l.Quantity
		cost += float64(l.Quantity) * l.Unit
	}
	return qty, cost
}

// ---------------------------------------------------------------------------
// The pass.
// ---------------------------------------------------------------------------

// ledgerSource is one owner's snapshots to fold: the journal and
// transaction windows (several for a corporation, one per division),
// its delivered jobs and blueprints, and what it holds now.
type ledgerSource struct {
	kind      string
	ownerID   int64
	journals  []db.CharacterSnapshot
	txns      []db.CharacterSnapshot
	jobs      *db.CharacterSnapshot
	blueprint *db.CharacterSnapshot
	assets    *db.CharacterSnapshot
	orders    *db.CharacterSnapshot
}

// refreshProfitLedger folds one character's snapshots, and the
// corporation division journals it holds, into the ledger.
// Best-effort: failures are logged and never break the cycle.
func (app *Application) refreshProfitLedger(ctx context.Context, ch db.Character, now time.Time) {
	get := func(kind string) *db.CharacterSnapshot {
		snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
		if err != nil {
			return nil
		}
		return &snap
	}
	src := ledgerSource{kind: ownerChar, ownerID: ch.CharacterID,
		jobs: get(esi.SnapIndustryJobs), blueprint: get(esi.SnapBlueprints),
		assets: get(esi.SnapAssets), orders: get(esi.SnapOrders)}
	if s := get(esi.SnapWalletJournal); s != nil {
		src.journals = append(src.journals, *s)
	}
	if s := get(esi.SnapWalletTxns); s != nil {
		src.txns = append(src.txns, *s)
	}
	if err := app.foldLedger(ctx, src, now); err != nil {
		logging.Errorf("worker: profit ledger for character %d: %v", ch.CharacterID, err)
	}

	corp, err := app.queries.GetCharacterCorporation(ctx, ch.CharacterID)
	if err != nil || corp.CorporationID <= 0 || isNPCCorporation(corp.CorporationID) {
		return
	}
	csrc := ledgerSource{kind: ownerCorp, ownerID: corp.CorporationID,
		assets: get(esi.SnapCorpAssets), orders: get(esi.SnapCorpOrders)}
	for d := 1; d <= walletDivisionCount; d++ {
		if s := get(fmt.Sprintf("%s%d", esi.SnapCorpJournalPrefix, d)); s != nil {
			csrc.journals = append(csrc.journals, *s)
		}
		if s := get(fmt.Sprintf("%s%d", esi.SnapCorpTxnsPrefix, d)); s != nil {
			csrc.txns = append(csrc.txns, *s)
		}
	}
	if len(csrc.journals) == 0 && len(csrc.txns) == 0 {
		return
	}
	if err := app.foldLedger(ctx, csrc, now); err != nil {
		logging.Errorf("worker: profit ledger for corporation %d: %v", corp.CorporationID, err)
	}
}

// isNPCCorporation: NPC corporations hold the 1000000 to 2000000 range.
func isNPCCorporation(id int64) bool { return id >= 1000000 && id < 2000000 }

// newestFetch is the latest fetch among snapshots.
func newestFetch(snaps ...*db.CharacterSnapshot) time.Time {
	var t time.Time
	for _, s := range snaps {
		if s != nil && s.FetchedAt.After(t) {
			t = s.FetchedAt
		}
	}
	return t
}

func ptrs(snaps []db.CharacterSnapshot) []*db.CharacterSnapshot {
	out := make([]*db.CharacterSnapshot, len(snaps))
	for i := range snaps {
		out[i] = &snaps[i]
	}
	return out
}

// foldLedger lands one owner's new rows and rewrites the attribution
// from the earliest day they touch. A pass with nothing new only
// refreshes a character's remainder from today's net worth sample.
func (app *Application) foldLedger(ctx context.Context, src ledgerSource, now time.Time) error {
	state, err := app.queries.GetLedgerState(ctx, db.GetLedgerStateParams{OwnerKind: src.kind, OwnerID: src.ownerID})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	journalAt := newestFetch(ptrs(src.journals)...)
	txnsAt := newestFetch(ptrs(src.txns)...)
	jobsAt := newestFetch(src.jobs)
	changed := !state.JournalAt.Valid || journalAt.After(state.JournalAt.Time) ||
		!state.TxnsAt.Valid || txnsAt.After(state.TxnsAt.Time) ||
		!state.JobsAt.Valid || jobsAt.After(state.JobsAt.Time)
	today := now.UTC().Format("2006-01-02")
	if !changed {
		if src.kind == ownerChar {
			return app.foldRemainder(ctx, src.ownerID, now.UTC().AddDate(0, 0, -1).Format("2006-01-02"))
		}
		return nil
	}

	earliest := now.UTC()
	touchedTypes := map[int64]bool{}
	note := func(t time.Time) {
		if t.Before(earliest) {
			earliest = t
		}
	}

	for _, snap := range src.journals {
		var journal esi.WalletJournal
		if json.Unmarshal([]byte(snap.Payload), &journal) != nil || len(journal) == 0 {
			continue
		}
		p := db.InsertLedgerJournalBatchParams{OwnerKind: src.kind, OwnerID: src.ownerID}
		for _, e := range journal {
			if _, err := time.Parse(time.RFC3339, e.Date); err != nil || e.ID == 0 {
				continue
			}
			p.EntryIds = append(p.EntryIds, e.ID)
			p.Ats = append(p.Ats, e.Date)
			p.RefTypes = append(p.RefTypes, e.RefType)
			p.Activities = append(p.Activities, journalActivity(e.RefType, e.Amount))
			p.Amounts = append(p.Amounts, e.Amount)
			p.FirstParties = append(p.FirstParties, e.FirstPartyID)
			p.SecondParties = append(p.SecondParties, e.SecondPartyID)
			p.ContextIds = append(p.ContextIds, e.ContextID)
			p.ContextTypes = append(p.ContextTypes, e.ContextType)
		}
		if len(p.EntryIds) == 0 {
			continue
		}
		ats, err := app.queries.InsertLedgerJournalBatch(ctx, p)
		if err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		for _, t := range ats {
			note(t)
		}
	}

	for _, snap := range src.txns {
		var txns esi.WalletTransactions
		if json.Unmarshal([]byte(snap.Payload), &txns) != nil || len(txns) == 0 {
			continue
		}
		p := db.InsertLedgerTxnBatchParams{OwnerKind: src.kind, OwnerID: src.ownerID}
		for _, t := range txns {
			if _, err := time.Parse(time.RFC3339, t.Date); err != nil || t.TransactionID == 0 || t.Quantity <= 0 {
				continue
			}
			p.TransactionIds = append(p.TransactionIds, t.TransactionID)
			p.Ats = append(p.Ats, t.Date)
			p.TypeIds = append(p.TypeIds, t.TypeID)
			p.Quantities = append(p.Quantities, t.Quantity)
			p.UnitPrices = append(p.UnitPrices, t.UnitPrice)
			p.IsBuys = append(p.IsBuys, t.IsBuy)
			p.LocationIds = append(p.LocationIds, t.LocationID)
			p.JournalRefIds = append(p.JournalRefIds, t.JournalRefID)
		}
		if len(p.TransactionIds) == 0 {
			continue
		}
		rows, err := app.queries.InsertLedgerTxnBatch(ctx, p)
		if err != nil {
			return fmt.Errorf("transactions: %w", err)
		}
		for _, r := range rows {
			note(r.At)
			touchedTypes[r.TypeID] = true
		}
	}

	if src.jobs != nil {
		jobTypes, jobEarliest, err := app.landDeliveredJobs(ctx, src, now)
		if err != nil {
			return fmt.Errorf("jobs: %w", err)
		}
		for _, id := range jobTypes {
			touchedTypes[id] = true
		}
		if !jobEarliest.IsZero() {
			note(jobEarliest)
		}
	}

	held := app.ledgerHoldings(src)
	types := make([]int64, 0, len(touchedTypes))
	for id := range touchedTypes {
		types = append(types, id)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	for _, typeID := range types {
		if err := app.refoldType(ctx, src, typeID, held[typeID], now); err != nil {
			return fmt.Errorf("type %d: %w", typeID, err)
		}
	}

	fromDay := earliest.UTC().Format("2006-01-02")
	if fromDay > today {
		fromDay = today
	}
	fromAt, _ := time.Parse("2006-01-02", fromDay)
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteProfitDailyFrom(ctx, db.DeleteProfitDailyFromParams{OwnerKind: src.kind, OwnerID: src.ownerID, FromDay: fromDay}); err != nil {
		return err
	}
	if err := qtx.FoldProfitDaily(ctx, db.FoldProfitDailyParams{OwnerKind: src.kind, OwnerID: src.ownerID, FromAt: fromAt, FromDay: fromDay}); err != nil {
		return err
	}
	if src.kind == ownerChar {
		if err := qtx.FoldProfitRemainder(ctx, db.FoldProfitRemainderParams{CharacterID: src.ownerID, FromDay: fromDay}); err != nil {
			return err
		}
	}
	if err := qtx.UpsertLedgerState(ctx, db.UpsertLedgerStateParams{
		OwnerKind: src.kind, OwnerID: src.ownerID,
		JournalAt: sql.NullTime{Time: journalAt, Valid: !journalAt.IsZero()},
		TxnsAt:    sql.NullTime{Time: txnsAt, Valid: !txnsAt.IsZero()},
		JobsAt:    sql.NullTime{Time: jobsAt, Valid: !jobsAt.IsZero()},
		FoldedAt:  sql.NullTime{Time: now.UTC(), Valid: true},
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// foldRemainder rewrites a character's remainder rows from a day on,
// after a new net worth sample with no new ledger rows.
func (app *Application) foldRemainder(ctx context.Context, characterID int64, fromDay string) error {
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteProfitRemainderFrom(ctx, db.DeleteProfitRemainderFromParams{OwnerID: characterID, FromDay: fromDay}); err != nil {
		return err
	}
	if err := qtx.FoldProfitRemainder(ctx, db.FoldProfitRemainderParams{CharacterID: characterID, FromDay: fromDay}); err != nil {
		return err
	}
	return tx.Commit()
}

// refoldType rematches one type's whole history for an owner and
// rewrites its realized sales and its open position.
func (app *Application) refoldType(ctx context.Context, src ledgerSource, typeID, held int64, now time.Time) error {
	rows, err := app.queries.ListLedgerLotsForType(ctx, db.ListLedgerLotsForTypeParams{OwnerKind: src.kind, OwnerID: src.ownerID, TypeID: typeID})
	if err != nil {
		return err
	}
	lots := make([]ledgerLot, 0, len(rows))
	for _, r := range rows {
		lots = append(lots, ledgerLot{At: r.At, RefID: r.RefID, Sell: r.IsSell.Valid && r.IsSell.Bool, Quantity: r.Quantity, Unit: r.UnitPrice})
	}
	sales, open := matchFIFO(lots)
	planetary, _ := app.queries.IsPlanetaryType(ctx, typeID)

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteTradeRealizedForType(ctx, db.DeleteTradeRealizedForTypeParams{OwnerKind: src.kind, OwnerID: src.ownerID, TypeID: typeID}); err != nil {
		return err
	}
	for _, s := range sales {
		if err := qtx.InsertTradeRealized(ctx, db.InsertTradeRealizedParams{
			OwnerKind: src.kind, OwnerID: src.ownerID, TransactionID: s.RefID,
			Day: s.At.UTC().Format("2006-01-02"), TypeID: typeID,
			Quantity: s.Quantity, Revenue: s.Revenue, Cost: s.Cost,
			UnmatchedQty: s.UnmatchedQty, UnmatchedRevenue: s.UnmatchedRevenue, Planetary: planetary,
		}); err != nil {
			return err
		}
	}
	qty, cost := capOpenLots(open, held)
	if qty > 0 {
		err = qtx.UpsertTradePosition(ctx, db.UpsertTradePositionParams{OwnerKind: src.kind, OwnerID: src.ownerID, TypeID: typeID, Quantity: qty, Cost: cost, UpdatedAt: now.UTC()})
	} else {
		err = qtx.DeleteTradePosition(ctx, db.DeleteTradePositionParams{OwnerKind: src.kind, OwnerID: src.ownerID, TypeID: typeID})
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ledgerHoldings is what an owner holds per type now: assets plus
// stock listed in open sell orders.
func (app *Application) ledgerHoldings(src ledgerSource) map[int64]int64 {
	held := map[int64]int64{}
	if src.assets != nil {
		var assets []esi.Asset
		if json.Unmarshal([]byte(src.assets.Payload), &assets) == nil {
			for _, a := range assets {
				held[a.TypeID] += a.Quantity
			}
		}
	}
	if src.orders != nil {
		var orders []esi.CharOrder
		if json.Unmarshal([]byte(src.orders.Payload), &orders) == nil {
			for _, o := range orders {
				if !o.IsBuyOrder {
					held[o.TypeID] += o.VolumeRemain
				}
			}
		}
	}
	return held
}

// landDeliveredJobs values the owner's newly delivered manufacturing
// and reaction jobs at today's house prices and stores them. It
// returns the product types they add stock of and the earliest
// delivery among them.
func (app *Application) landDeliveredJobs(ctx context.Context, src ledgerSource, now time.Time) ([]int64, time.Time, error) {
	var jobs esi.IndustryJobs
	if json.Unmarshal([]byte(src.jobs.Payload), &jobs) != nil {
		return nil, time.Time{}, nil
	}
	var delivered []esi.IndustryJob
	var ids []int64
	for _, j := range jobs {
		if j.Status == "delivered" && (j.ActivityID == 1 || j.ActivityID == 11) && j.ProductTypeID > 0 {
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
	typicals := app.TypicalSells(ctx)
	estimates := app.storedTrueValues(ctx)
	price := func(typeID int64) (float64, bool) {
		return pickValuationPrice(typicals[typeID], estSellOf(estimates, typeID))
	}
	var types []int64
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
		units, outValue, matCost, priced := app.valueJob(ctx, j, me[j.BlueprintID], price)
		if _, err := app.queries.InsertLedgerJob(ctx, db.InsertLedgerJobParams{
			OwnerKind: src.kind, OwnerID: src.ownerID, JobID: j.JobID, At: at,
			ActivityID: j.ActivityID, ProductTypeID: j.ProductTypeID, Units: units,
			OutputValue: outValue, MaterialsCost: matCost, Priced: priced,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, time.Time{}, err
		}
		if priced {
			types = append(types, j.ProductTypeID)
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return types, earliest, nil
}

// valueJob prices one delivered job: the units it made, their value,
// and its materials after material efficiency (each line at least one
// unit per run). priced is false when the blueprint, the output, or
// any material has no house price: the job is then kept but not
// counted, an honest gap.
func (app *Application) valueJob(ctx context.Context, j esi.IndustryJob, me int64, price func(int64) (float64, bool)) (units int64, output, materials float64, priced bool) {
	bp, err := app.queries.GetSDEBlueprint(ctx, j.BlueprintTypeID)
	if err != nil {
		return 0, 0, 0, false
	}
	runs := j.SuccessfulRuns
	if runs <= 0 {
		runs = j.Runs
	}
	units = runs * max(bp.ProductQuantity, 1)
	unit, ok := price(j.ProductTypeID)
	if !ok || units <= 0 {
		return units, 0, 0, false
	}
	mats, err := app.queries.ListSDEBlueprintMaterials(ctx, j.BlueprintTypeID)
	if err != nil || len(mats) == 0 {
		return units, 0, 0, false
	}
	factor := 1 - float64(min(max(me, 0), 10))/100
	for _, m := range mats {
		p, ok := price(m.MaterialTypeID)
		if !ok {
			return units, 0, 0, false
		}
		qty := max(runs, int64(math.Ceil(float64(m.Quantity*j.Runs)*factor)))
		materials += float64(qty) * p
	}
	return units, float64(units) * unit, materials, true
}
