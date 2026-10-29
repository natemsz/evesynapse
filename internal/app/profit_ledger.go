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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"evesynapse/internal/app/module"
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

// ledgerLot is one stock movement of a type: a buy, job output or
// entered basis (adds), or a sell, a job's material use, or units the
// player sold off-market or lost (removes).
type ledgerLot struct {
	At        time.Time
	RefID     int64  // transaction id; minus the job id for job output and material use
	Owner     int64  // the pool member that traded it (0 for a stored or entered lot)
	Kind      string // lotTxn, lotJob, lotBasis, lotConsume, lotManualSale, lotLost
	PreferRef int64  // a removal the player tied to a job takes that job's lot first
	Sell      bool
	Quantity  int64
	Unit      float64
}

// Lot kinds, as ListPoolLotsForTypeSince names them.
const (
	lotTxn        = "txn"
	lotJob        = "job"
	lotBasis      = "basis"
	lotConsume    = "consume"
	lotManualSale = "msale"
	lotLost       = "lost"
)

// manualBasisRef is where entered cost basis lots' ref ids start
// (below every job's).
const manualBasisRef = -4000000000000000

// isJobRef reports whether a lot's ref id is a job's output.
func isJobRef(ref int64) bool { return ref < 0 && ref > manualBasisRef }

// lotTake is part of a removal settled against one open lot. Mixed
// says bought or entered stock sat open beside the job lot it took.
type lotTake struct {
	RefID    int64
	Quantity int64
	Unit     float64
	Mixed    bool
}

// realizedSale is one removal as the matching settled it.
type realizedSale struct {
	RefID            int64
	Owner            int64
	Kind             string
	PreferRef        int64
	At               time.Time
	Unit             float64
	Quantity         int64 // covered by earlier lots
	Revenue, Cost    float64
	UnmatchedQty     int64
	UnmatchedRevenue float64
	Takes            []lotTake
}

// openLot is stock still held from a buy, job output or entered basis.
type openLot struct {
	Quantity int64
	Unit     float64
	RefID    int64
}

// matchFIFO settles removals against the earliest open lots. Lots
// must be in time order. It returns every removal's settlement and
// the lots left open, oldest first.
func matchFIFO(lots []ledgerLot) ([]realizedSale, []openLot) {
	return matchFIFOFrom(nil, lots)
}

// matchFIFOFrom is matchFIFO starting from lots already open (a fold
// checkpoint's remainder). The caller's slice is never mutated. A
// removal tied to a job takes from that job's lot first, then FIFO.
func matchFIFOFrom(start []openLot, lots []ledgerLot) ([]realizedSale, []openLot) {
	open := make([]openLot, len(start))
	copy(open, start)
	var sales []realizedSale
	for _, l := range lots {
		if l.Quantity <= 0 {
			continue
		}
		if !l.Sell {
			open = append(open, openLot{Quantity: l.Quantity, Unit: l.Unit, RefID: l.RefID})
			continue
		}
		s := realizedSale{RefID: l.RefID, Owner: l.Owner, Kind: l.Kind, PreferRef: l.PreferRef, At: l.At, Unit: l.Unit}
		mixed := false
		for _, o := range open {
			if !isJobRef(o.RefID) {
				mixed = true
				break
			}
		}
		left := l.Quantity
		take := func(i int) {
			n := min(left, open[i].Quantity)
			s.Quantity += n
			s.Revenue += float64(n) * l.Unit
			s.Cost += float64(n) * open[i].Unit
			s.Takes = append(s.Takes, lotTake{RefID: open[i].RefID, Quantity: n, Unit: open[i].Unit, Mixed: mixed})
			open[i].Quantity -= n
			left -= n
		}
		if l.PreferRef != 0 {
			for i := 0; i < len(open) && left > 0; i++ {
				if open[i].RefID == l.PreferRef {
					take(i)
				}
			}
			kept := open[:0]
			for _, o := range open {
				if o.Quantity > 0 {
					kept = append(kept, o)
				}
			}
			open = kept
		}
		for left > 0 && len(open) > 0 {
			take(0)
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
// its delivered jobs and blueprints, what it holds now, and its
// orders (whose issued times tie broker fees to an item).
type ledgerSource struct {
	kind          string
	ownerID       int64
	journals      []db.CharacterSnapshot
	txns          []db.CharacterSnapshot
	jobs          *db.CharacterSnapshot
	blueprint     *db.CharacterSnapshot
	assets        *db.CharacterSnapshot
	orders        *db.CharacterSnapshot
	ordersHistory *db.CharacterSnapshot
	userID        int64 // the account, for a character: its industry preferences
}

// Pool kinds (schema 112). A character's sales match against the
// stock of its whole account by default ('acct', keyed by user id), or
// against its own alone ('char') when the player keeps per-character
// books; a corporation is always its own pool.
const poolAcct = "acct"

// ledgerPool is the set of owners whose lots one FIFO stream holds.
type ledgerPool struct {
	kind      string  // poolAcct | ownerChar | ownerCorp
	id        int64   // user, character or corporation id
	ownerKind string  // the members' kind in the ledger tables
	members   []int64 // sorted
	userID    int64   // the account, for character pools
}

func (p ledgerPool) memberKey() string {
	parts := make([]string, len(p.members))
	for i, id := range p.members {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// profitPrefs are a player's profit preferences (schemas 112, 113),
// with the defaults when none are stored: pooled books, broker fees in
// the item margin, industry priced in The Forge at sell typicals.
type profitPrefs struct {
	Pooled, BrokerInMargin bool
	IndustryRegion         int64
	IndustrySide           string // module.AppraisalModeSell or AppraisalModeBuy
}

func (app *Application) profitPrefsFor(ctx context.Context, userID int64) profitPrefs {
	p, err := app.queries.GetUserProfitPrefs(ctx, userID)
	if err != nil {
		return profitPrefs{Pooled: true, BrokerInMargin: true, IndustryRegion: defaultMarketRegion, IndustrySide: module.AppraisalModeSell}
	}
	return profitPrefs{Pooled: p.Pooled, BrokerInMargin: p.BrokerInMargin, IndustryRegion: p.IndustryRegion, IndustrySide: p.IndustrySide}
}

// profitPoolFor is the pool a character's trades match in.
func (app *Application) profitPoolFor(ctx context.Context, ch db.Character) ledgerPool {
	solo := ledgerPool{kind: ownerChar, id: ch.CharacterID, ownerKind: ownerChar, members: []int64{ch.CharacterID}, userID: ch.UserID}
	if !app.profitPrefsFor(ctx, ch.UserID).Pooled {
		return solo
	}
	chars, err := app.queries.ListCharactersByUser(ctx, ch.UserID)
	if err != nil || len(chars) == 0 {
		return solo
	}
	p := ledgerPool{kind: poolAcct, id: ch.UserID, ownerKind: ownerChar, userID: ch.UserID}
	for _, c := range chars {
		p.members = append(p.members, c.CharacterID)
	}
	sort.Slice(p.members, func(i, j int) bool { return p.members[i] < p.members[j] })
	return p
}

// profitPoolForCharacter is profitPoolFor by character id.
func (app *Application) profitPoolForCharacter(ctx context.Context, characterID int64) ledgerPool {
	ch, err := app.queries.GetCharacter(ctx, characterID)
	if err != nil {
		return ledgerPool{kind: ownerChar, id: characterID, ownerKind: ownerChar, members: []int64{characterID}}
	}
	return app.profitPoolFor(ctx, ch)
}

// ledgerPoolLocks serializes folds of one pool: two characters of an
// account can pass at once, and both rewrite the account pool's rows.
var ledgerPoolLocks sync.Map

func lockLedgerPool(p ledgerPool) func() {
	m, _ := ledgerPoolLocks.LoadOrStore(p.kind+":"+strconv.FormatInt(p.id, 10), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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
	src := ledgerSource{kind: ownerChar, ownerID: ch.CharacterID, userID: ch.UserID,
		jobs: get(esi.SnapIndustryJobs), blueprint: get(esi.SnapBlueprints),
		assets: get(esi.SnapAssets), orders: get(esi.SnapOrders), ordersHistory: get(esi.SnapOrdersHistory)}
	if s := get(esi.SnapWalletJournal); s != nil {
		src.journals = append(src.journals, *s)
	}
	if s := get(esi.SnapWalletTxns); s != nil {
		src.txns = append(src.txns, *s)
	}
	if err := app.foldLedger(ctx, src, app.profitPoolFor(ctx, ch), now); err != nil {
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
	cpool := ledgerPool{kind: ownerCorp, id: corp.CorporationID, ownerKind: ownerCorp, members: []int64{corp.CorporationID}}
	if err := app.foldLedger(ctx, csrc, cpool, now); err != nil {
		logging.Errorf("worker: profit ledger for corporation %d: %v", corp.CorporationID, err)
	}
}

// isNPCCorporation: NPC corporations hold the 1000000 to 2000000 range.
func isNPCCorporation(id int64) bool { return id >= 1000000 && id < 2000000 }

// snapshotHash fingerprints snapshot payloads, so the fold can tell
// a refetched window from a changed one: ESI answers the same 30
// days again on most passes, and only new content is worth landing.
func snapshotHash(snaps ...*db.CharacterSnapshot) string {
	h := sha256.New()
	for _, s := range snaps {
		if s == nil {
			continue
		}
		h.Write([]byte(s.Payload))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

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

const (
	// esiWalletWindow is how far back ESI serves journal and
	// transactions: two fetches further apart than this leave a hole.
	esiWalletWindow = 30 * 24 * time.Hour
	// taxNeighbourSpan is how far before a landed sale the tax pairing
	// is redone: a group's check reads its neighbouring groups.
	taxNeighbourSpan = 7 * 24 * time.Hour
	// brokerRecheckSpan is how far back broker fees are retied each
	// pass: an order's issued time reaches the ledger with the next
	// orders fetch, after the fee itself.
	brokerRecheckSpan = 3 * 24 * time.Hour
	// historyGrace: units sold this soon after a pool's history begins,
	// with no lot to cover them, were most likely bought before it.
	historyGrace = 90 * 24 * time.Hour
)

// foldLedger lands one owner's new rows and rewrites the attribution
// of the pool it trades in from the earliest day they touch. A pool
// whose members changed (or that has never folded under this method)
// refolds from its whole stored ledger. A pass with nothing new only
// rolls the item windows and a character's remainder forward.
func (app *Application) foldLedger(ctx context.Context, src ledgerSource, pool ledgerPool, now time.Time) error {
	unlock := lockLedgerPool(pool)
	defer unlock()
	state, err := app.queries.GetLedgerState(ctx, db.GetLedgerStateParams{OwnerKind: src.kind, OwnerID: src.ownerID})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	pstate, err := app.queries.GetLedgerPoolState(ctx, db.GetLedgerPoolStateParams{PoolKind: pool.kind, PoolID: pool.id})
	newPool := errors.Is(err, sql.ErrNoRows)
	if err != nil && !newPool {
		return err
	}
	full := newPool || pstate.Members != pool.memberKey()
	journalAt := newestFetch(ptrs(src.journals)...)
	txnsAt := newestFetch(ptrs(src.txns)...)
	jobsAt := newestFetch(src.jobs)
	journalHash := snapshotHash(ptrs(src.journals)...)
	txnsHash := snapshotHash(ptrs(src.txns)...)
	jobsHash := snapshotHash(src.jobs)
	changed := journalHash != state.JournalHash || txnsHash != state.TxnsHash || jobsHash != state.JobsHash
	today := now.UTC().Format("2006-01-02")
	if !changed && !full {
		return app.foldQuiet(ctx, src, today)
	}

	earliest := now.UTC()
	landed := false
	touchedTypes := map[int64]bool{}
	note := func(t time.Time) {
		if t.Before(earliest) {
			earliest = t
		}
	}
	if changed {
		if landed, err = app.landLedgerRows(ctx, src, state, journalAt, now, note, touchedTypes); err != nil {
			return err
		}
	}

	members := pool.members
	fromAt := earliest
	if full {
		start, err := app.queries.GetLedgerCoverageStart(ctx, db.GetLedgerCoverageStartParams{OwnerKind: pool.ownerKind, OwnerIds: members})
		if err != nil {
			return err
		}
		fromAt = start
		if err := app.resetPool(ctx, pool); err != nil {
			return err
		}
		types, err := app.queries.ListPoolTypes(ctx, db.ListPoolTypesParams{OwnerKind: pool.ownerKind, OwnerIds: members, PoolKind: pool.kind, PoolID: pool.id})
		if err != nil {
			return err
		}
		touchedTypes = map[int64]bool{}
		for _, id := range types {
			touchedTypes[id] = true
		}
	}

	// Tax and broker ties for the owners whose rows moved: every
	// member on a full refold, else this owner around what landed.
	taxFrom := fromAt.Add(-taxNeighbourSpan)
	brokerFrom := now.UTC().Add(-brokerRecheckSpan)
	if fromAt.Before(brokerFrom) {
		brokerFrom = fromAt
	}
	retie := []int64{src.ownerID}
	if full {
		retie = members
	}
	for _, id := range retie {
		if full || landed {
			if err := app.queries.DeleteSaleTaxFrom(ctx, db.DeleteSaleTaxFromParams{OwnerKind: pool.ownerKind, OwnerID: id, FromAt: taxFrom}); err != nil {
				return err
			}
			if err := app.queries.FoldSaleTax(ctx, db.FoldSaleTaxParams{OwnerKind: pool.ownerKind, OwnerID: id, FromAt: taxFrom}); err != nil {
				return fmt.Errorf("sale tax: %w", err)
			}
		}
		if err := app.queries.DeleteBrokerAllocFrom(ctx, db.DeleteBrokerAllocFromParams{OwnerKind: pool.ownerKind, OwnerID: id, FromAt: brokerFrom}); err != nil {
			return err
		}
		if err := app.queries.FoldBrokerAlloc(ctx, db.FoldBrokerAllocParams{OwnerKind: pool.ownerKind, OwnerID: id, FromAt: brokerFrom}); err != nil {
			return fmt.Errorf("broker fees: %w", err)
		}
	}

	if len(touchedTypes) > 0 {
		held := app.poolHoldings(ctx, src, pool)
		gaps := app.poolGapContext(ctx, pool)
		types := make([]int64, 0, len(touchedTypes))
		for id := range touchedTypes {
			types = append(types, id)
		}
		sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
		for _, typeID := range types {
			if err := app.refoldType(ctx, pool, typeID, held[typeID], gaps, now); err != nil {
				return fmt.Errorf("type %d: %w", typeID, err)
			}
		}
	}

	// Each member's rollups from the earliest day anything moved: the
	// pool's FIFO reaches every member's sales, and this owner's tax
	// and broker ties reach back further.
	rollFrom := fromAt
	for _, t := range []time.Time{taxFrom, brokerFrom} {
		if t.Before(rollFrom) {
			rollFrom = t
		}
	}
	fromDay := rollFrom.UTC().Format("2006-01-02")
	if fromDay > today {
		fromDay = today
	}
	dayAt, _ := time.Parse("2006-01-02", fromDay)
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	for _, id := range members {
		if err := foldOwnerRollups(ctx, qtx, pool.ownerKind, id, fromDay, dayAt, today); err != nil {
			return err
		}
	}
	if err := qtx.UpsertLedgerState(ctx, db.UpsertLedgerStateParams{
		OwnerKind: src.kind, OwnerID: src.ownerID,
		JournalAt:   sql.NullTime{Time: journalAt, Valid: !journalAt.IsZero()},
		TxnsAt:      sql.NullTime{Time: txnsAt, Valid: !txnsAt.IsZero()},
		JobsAt:      sql.NullTime{Time: jobsAt, Valid: !jobsAt.IsZero()},
		JournalHash: journalHash, TxnsHash: txnsHash, JobsHash: jobsHash,
		FoldedAt: sql.NullTime{Time: now.UTC(), Valid: true},
	}); err != nil {
		return err
	}
	ps := db.UpsertLedgerPoolStateParams{PoolKind: pool.kind, PoolID: pool.id, Members: pool.memberKey(),
		FoldedAt: now.UTC(), RefoldedAt: pstate.RefoldedAt, RefoldReason: pstate.RefoldReason}
	if full {
		ps.RefoldedAt = now.UTC()
		switch {
		case newPool && state.FoldedAt.Valid:
			ps.RefoldReason = "method" // first fold since pooled matching shipped
		case newPool:
			ps.RefoldReason = "new"
		case pstate.Members == "":
			ps.RefoldReason = "basis" // a manual cost basis was added
		default:
			ps.RefoldReason = "members"
		}
	}
	if err := qtx.UpsertLedgerPoolState(ctx, ps); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(touchedTypes) > 0 {
		if err := app.foldIndustrySummary(ctx, pool, now); err != nil {
			logging.Errorf("worker: industry summary for %s %d: %v", pool.kind, pool.id, err)
		}
	}
	return nil
}

// foldOwnerRollups rewrites one owner's item rollups and daily
// attribution from a day on, and its item windows as of today.
func foldOwnerRollups(ctx context.Context, qtx *db.Queries, kind string, id int64, fromDay string, fromAt time.Time, today string) error {
	if err := qtx.DeleteTradeItemDailyFrom(ctx, db.DeleteTradeItemDailyFromParams{OwnerKind: kind, OwnerID: id, FromDay: fromDay}); err != nil {
		return err
	}
	if err := qtx.FoldTradeItemDaily(ctx, db.FoldTradeItemDailyParams{OwnerKind: kind, OwnerID: id, FromDay: fromDay}); err != nil {
		return fmt.Errorf("item daily: %w", err)
	}
	if err := qtx.DeleteTradeItemWindows(ctx, db.DeleteTradeItemWindowsParams{OwnerKind: kind, OwnerID: id}); err != nil {
		return err
	}
	if err := qtx.FoldTradeItemWindows(ctx, db.FoldTradeItemWindowsParams{OwnerKind: kind, OwnerID: id, AsOfDay: today}); err != nil {
		return fmt.Errorf("item windows: %w", err)
	}
	if err := qtx.DeleteProfitDailyFrom(ctx, db.DeleteProfitDailyFromParams{OwnerKind: kind, OwnerID: id, FromDay: fromDay}); err != nil {
		return err
	}
	if err := qtx.FoldProfitDaily(ctx, db.FoldProfitDailyParams{OwnerKind: kind, OwnerID: id, FromAt: fromAt, FromDay: fromDay}); err != nil {
		return err
	}
	if kind == ownerChar {
		return qtx.FoldProfitRemainder(ctx, db.FoldProfitRemainderParams{CharacterID: id, FromDay: fromDay})
	}
	return nil
}

// foldQuiet is a pass with nothing new: the item windows roll forward
// when the day changed, and a character's remainder takes today's net
// worth sample.
func (app *Application) foldQuiet(ctx context.Context, src ledgerSource, today string) error {
	asOf, err := app.queries.GetTradeItemWindowAsOf(ctx, db.GetTradeItemWindowAsOfParams{OwnerKind: src.kind, OwnerID: src.ownerID})
	if err == nil && asOf != "" && asOf < today {
		tx, err := app.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		qtx := app.queries.WithTx(tx)
		if err := qtx.DeleteTradeItemWindows(ctx, db.DeleteTradeItemWindowsParams{OwnerKind: src.kind, OwnerID: src.ownerID}); err != nil {
			return err
		}
		if err := qtx.FoldTradeItemWindows(ctx, db.FoldTradeItemWindowsParams{OwnerKind: src.kind, OwnerID: src.ownerID, AsOfDay: today}); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if src.kind == ownerChar {
		yesterday, _ := time.Parse("2006-01-02", today)
		return app.foldRemainder(ctx, src.ownerID, yesterday.AddDate(0, 0, -1).Format("2006-01-02"))
	}
	return nil
}

// landLedgerRows stores what one owner's snapshots carry that the
// ledger has not seen: journal rows (noting a hole when two fetches
// sit further apart than ESI's window), transactions, delivered jobs,
// and the issued time of every order. It reports whether any journal,
// transaction or job row was new.
func (app *Application) landLedgerRows(ctx context.Context, src ledgerSource, state db.LedgerState, journalAt, now time.Time, note func(time.Time), touched map[int64]bool) (bool, error) {
	landed := false
	for _, snap := range src.journals {
		var journal esi.WalletJournal
		if json.Unmarshal([]byte(snap.Payload), &journal) != nil || len(journal) == 0 {
			continue
		}
		p := db.InsertLedgerJournalBatchParams{OwnerKind: src.kind, OwnerID: src.ownerID}
		var oldest time.Time
		for _, e := range journal {
			at, err := time.Parse(time.RFC3339, e.Date)
			if err != nil || e.ID == 0 {
				continue
			}
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
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
		if state.JournalAt.Valid && journalAt.Sub(state.JournalAt.Time) > esiWalletWindow && oldest.After(state.JournalAt.Time) {
			if err := app.queries.InsertLedgerHole(ctx, db.InsertLedgerHoleParams{OwnerKind: src.kind, OwnerID: src.ownerID, FromAt: state.JournalAt.Time, ToAt: oldest}); err != nil {
				return false, err
			}
		}
		ats, err := app.queries.InsertLedgerJournalBatch(ctx, p)
		if err != nil {
			return false, fmt.Errorf("journal: %w", err)
		}
		if len(ats) > 0 {
			landed = true
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
			return false, fmt.Errorf("transactions: %w", err)
		}
		if len(rows) > 0 {
			landed = true
		}
		for _, r := range rows {
			note(r.At)
			touched[r.TypeID] = true
		}
	}

	if src.jobs != nil {
		jobTypes, jobEarliest, err := app.landDeliveredJobs(ctx, src, now)
		if err != nil {
			return false, fmt.Errorf("jobs: %w", err)
		}
		for _, id := range jobTypes {
			touched[id] = true
		}
		if !jobEarliest.IsZero() {
			landed = true
			note(jobEarliest)
		}
	}

	if err := app.landOrderIssues(ctx, src); err != nil {
		return false, fmt.Errorf("orders: %w", err)
	}
	return landed, nil
}

// landOrderIssues records the issued time of every order the owner's
// order snapshots show, open and closed.
func (app *Application) landOrderIssues(ctx context.Context, src ledgerSource) error {
	p := db.InsertLedgerOrderIssuesParams{OwnerKind: src.kind, OwnerID: src.ownerID}
	add := func(orderID, typeID int64, issued string) {
		if _, err := time.Parse(time.RFC3339, issued); err != nil || orderID == 0 {
			return
		}
		p.OrderIds = append(p.OrderIds, orderID)
		p.Issueds = append(p.Issueds, issued)
		p.TypeIds = append(p.TypeIds, typeID)
	}
	if src.orders != nil {
		if src.kind == ownerCorp {
			var orders esi.CorpOrders
			if json.Unmarshal([]byte(src.orders.Payload), &orders) == nil {
				for _, o := range orders {
					add(o.OrderID, o.TypeID, o.Issued)
				}
			}
		} else {
			var orders esi.CharOrders
			if json.Unmarshal([]byte(src.orders.Payload), &orders) == nil {
				for _, o := range orders {
					add(o.OrderID, o.TypeID, o.Issued)
				}
			}
		}
	}
	if src.ordersHistory != nil {
		var hist esi.CharOrderHistory
		if json.Unmarshal([]byte(src.ordersHistory.Payload), &hist) == nil {
			for _, o := range hist {
				add(o.OrderID, o.TypeID, o.Issued)
			}
		}
	}
	if len(p.OrderIds) == 0 {
		return nil
	}
	return app.queries.InsertLedgerOrderIssues(ctx, p)
}

// resetPool clears what a pool refolding from scratch will rewrite:
// its realized sales and any its members left under an earlier pool,
// and the checkpoints, open lots and positions of its key and of the
// keys its members may have folded under before (their account, or
// each of them alone).
func (app *Application) resetPool(ctx context.Context, pool ledgerPool) error {
	if err := app.queries.DeleteJobOutputLinksForPool(ctx, db.DeleteJobOutputLinksForPoolParams{PoolKind: pool.kind, PoolID: pool.id}); err != nil {
		return err
	}
	if err := app.queries.DeleteTradeRealizedForPool(ctx, db.DeleteTradeRealizedForPoolParams{
		PoolKind: pool.kind, PoolID: pool.id, OwnerKind: pool.ownerKind, OwnerIds: pool.members,
	}); err != nil {
		return err
	}
	keys := []struct {
		kind string
		id   int64
	}{{pool.kind, pool.id}}
	if pool.ownerKind == ownerChar {
		keys = append(keys, struct {
			kind string
			id   int64
		}{poolAcct, pool.userID})
		for _, id := range pool.members {
			keys = append(keys, struct {
				kind string
				id   int64
			}{ownerChar, id})
		}
	}
	for _, k := range keys {
		if err := app.queries.DeletePoolCheckpoints(ctx, db.DeletePoolCheckpointsParams{PoolKind: k.kind, PoolID: k.id}); err != nil {
			return err
		}
		if err := app.queries.DeletePoolOpenLots(ctx, db.DeletePoolOpenLotsParams{PoolKind: k.kind, PoolID: k.id}); err != nil {
			return err
		}
		if err := app.queries.DeletePoolPositions(ctx, db.DeletePoolPositionsParams{PoolKind: k.kind, PoolID: k.id}); err != nil {
			return err
		}
	}
	return nil
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

// foldCheckpointLag is how far behind the fold's checkpoint trails:
// a type's realized rows are final once they are older than this,
// and a refold only rematches from the checkpoint on. New lots reach
// the ledger from ESI's 30-day windows, so nothing new can land
// behind a checkpoint that trails by more than that window.
const foldCheckpointLag = 40 * 24 * time.Hour

// gapContext is what types a pool's uncovered units: when its ledger
// begins, and the holes in its members' journals.
type gapContext struct {
	start time.Time
	holes []db.ListLedgerHolesRow
}

func (app *Application) poolGapContext(ctx context.Context, pool ledgerPool) gapContext {
	var g gapContext
	g.start, _ = app.queries.GetLedgerCoverageStart(ctx, db.GetLedgerCoverageStartParams{OwnerKind: pool.ownerKind, OwnerIds: pool.members})
	g.holes, _ = app.queries.ListLedgerHoles(ctx, db.ListLedgerHolesParams{OwnerKind: pool.ownerKind, OwnerIds: pool.members})
	return g
}

// gapKind says why a sale's unmatched units have no cost: a hole in
// the ledger within the grace before it, the sale falling soon after
// the pool's history begins (bought before it, most likely), or no
// buy, job or entered basis at all (loot, contract items, gifts).
func (g gapContext) gapKind(at time.Time) string {
	for _, h := range g.holes {
		if !h.ToAt.After(at) && at.Sub(h.ToAt) <= historyGrace {
			return "ledger_hole"
		}
	}
	if !g.start.IsZero() && at.Sub(g.start) <= historyGrace {
		return "before_history"
	}
	return "no_buy"
}

// dropInternalTrades removes fills between two members of one pool:
// a sell and a buy of the same quantity at the same price in the same
// second by different members move stock inside the pool, so neither
// is a trade. Lots must be in time order.
func dropInternalTrades(lots []ledgerLot) []ledgerLot {
	type fill struct {
		sec, qty int64
		unit     float64
	}
	buys := map[fill][]int{}
	for i, l := range lots {
		if !l.Sell && l.Kind == lotTxn && l.Owner != 0 {
			k := fill{l.At.Unix(), l.Quantity, l.Unit}
			buys[k] = append(buys[k], i)
		}
	}
	used := make([]bool, len(lots))
	for i, s := range lots {
		if !s.Sell || s.Kind != lotTxn {
			continue
		}
		k := fill{s.At.Unix(), s.Quantity, s.Unit}
		for _, j := range buys[k] {
			if !used[j] && lots[j].Owner != s.Owner {
				used[i], used[j] = true, true
				break
			}
		}
	}
	out := lots[:0:0]
	for i, l := range lots {
		if !used[i] {
			out = append(out, l)
		}
	}
	return out
}

// refoldType rematches one type's lots across a pool from its fold
// checkpoint on and rewrites the realized sales in that span and the
// open position. The lots still open at the new checkpoint are
// stored, so the next refold starts there instead of at the pool's
// first trade: the work per fold stays inside one ESI window plus the
// stored remainder, however long the pool has traded.
func (app *Application) refoldType(ctx context.Context, pool ledgerPool, typeID, held int64, gaps gapContext, now time.Time) error {
	var cp time.Time
	if at, err := app.queries.GetTradeFoldCheckpoint(ctx, db.GetTradeFoldCheckpointParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeID: typeID}); err == nil {
		cp = at
	}
	rows, err := app.queries.ListPoolLotsForTypeSince(ctx, db.ListPoolLotsForTypeSinceParams{
		OwnerKind: pool.ownerKind, OwnerIds: pool.members, PoolKind: pool.kind, PoolID: pool.id, TypeID: typeID, Since: cp,
	})
	if err != nil {
		return err
	}
	lots := make([]ledgerLot, 0, len(rows))
	for _, r := range rows {
		lots = append(lots, ledgerLot{At: r.At, RefID: r.RefID, Owner: r.OwnerID, Kind: r.Kind, PreferRef: r.PreferRef,
			Sell: r.IsSell.Valid && r.IsSell.Bool, Quantity: r.Quantity, Unit: r.UnitPrice})
	}
	if len(pool.members) > 1 {
		lots = dropInternalTrades(lots)
	}
	var prefix []ledgerLot
	if !cp.IsZero() {
		stored, err := app.queries.ListTradeOpenLots(ctx, db.ListTradeOpenLotsParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeID: typeID})
		if err != nil {
			return err
		}
		for _, r := range stored {
			prefix = append(prefix, ledgerLot{At: cp, RefID: r.RefID, Quantity: r.Quantity, Unit: r.UnitPrice})
		}
	}
	cut := now.UTC().Add(-foldCheckpointLag).Truncate(24 * time.Hour)
	if cut.Before(cp) {
		cut = cp
	}
	head := lots
	var tail []ledgerLot
	for i, l := range lots {
		if !l.At.Before(cut) {
			head, tail = lots[:i], lots[i:]
			break
		}
	}
	sales, openAtCut := matchFIFO(append(prefix, head...))
	salesTail, open := matchFIFOFrom(openAtCut, tail)
	sales = append(sales, salesTail...)
	planetary, _ := app.queries.IsPlanetaryType(ctx, typeID)

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteTradeRealizedForPoolTypeFrom(ctx, db.DeleteTradeRealizedForPoolTypeFromParams{PoolKind: pool.kind, PoolID: pool.id, TypeID: typeID, FromDay: cp.UTC().Format("2006-01-02")}); err != nil {
		return err
	}
	if len(sales) > 0 {
		p := db.InsertTradeRealizedBatchParams{OwnerKind: pool.ownerKind, TypeID: typeID, Planetary: planetary, PoolKind: pool.kind, PoolID: pool.id}
		for _, s := range sales {
			if s.Kind != lotTxn {
				continue
			}
			gap := ""
			if s.UnmatchedQty > 0 {
				gap = gaps.gapKind(s.At)
			}
			p.OwnerIds = append(p.OwnerIds, s.Owner)
			p.TransactionIds = append(p.TransactionIds, s.RefID)
			p.Days = append(p.Days, s.At.UTC().Format("2006-01-02"))
			p.Quantities = append(p.Quantities, s.Quantity)
			p.Revenues = append(p.Revenues, s.Revenue)
			p.Costs = append(p.Costs, s.Cost)
			p.UnmatchedQtys = append(p.UnmatchedQtys, s.UnmatchedQty)
			p.UnmatchedRevenues = append(p.UnmatchedRevenues, s.UnmatchedRevenue)
			p.GapKinds = append(p.GapKinds, gap)
		}
		if err := qtx.InsertTradeRealizedBatch(ctx, p); err != nil {
			return err
		}
	}
	if err := writeJobSettlements(ctx, qtx, pool, typeID, cp, sales); err != nil {
		return err
	}
	if err := qtx.DeleteTradeOpenLots(ctx, db.DeleteTradeOpenLotsParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeID: typeID}); err != nil {
		return err
	}
	if len(openAtCut) > 0 {
		p := db.InsertTradeOpenLotsBatchParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeID: typeID}
		for i, l := range openAtCut {
			p.Seqs = append(p.Seqs, int64(i))
			p.Quantities = append(p.Quantities, l.Quantity)
			p.UnitPrices = append(p.UnitPrices, l.Unit)
			p.RefIds = append(p.RefIds, l.RefID)
		}
		if err := qtx.InsertTradeOpenLotsBatch(ctx, p); err != nil {
			return err
		}
	}
	if err := qtx.UpsertTradeFoldCheckpoint(ctx, db.UpsertTradeFoldCheckpointParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeID: typeID, CheckpointAt: cut}); err != nil {
		return err
	}
	qty, cost := capOpenLots(open, held)
	if qty > 0 {
		err = qtx.UpsertTradePosition(ctx, db.UpsertTradePositionParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeID: typeID, Quantity: qty, Cost: cost, UpdatedAt: now.UTC()})
	} else {
		err = qtx.DeleteTradePosition(ctx, db.DeleteTradePositionParams{OwnerKind: pool.kind, OwnerID: pool.id, TypeID: typeID})
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// poolHoldings is what a pool holds per type now: each member's assets
// plus stock listed in its open sell orders. A corporation's come from
// the snapshots its reader just fetched.
func (app *Application) poolHoldings(ctx context.Context, src ledgerSource, pool ledgerPool) map[int64]int64 {
	if pool.ownerKind == ownerCorp {
		return ledgerHoldings(src)
	}
	held := map[int64]int64{}
	for _, id := range pool.members {
		m := src
		if id != src.ownerID {
			m = ledgerSource{}
			if s, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: id, Kind: esi.SnapAssets}); err == nil {
				m.assets = &s
			}
			if s, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: id, Kind: esi.SnapOrders}); err == nil {
				m.orders = &s
			}
		}
		for t, q := range ledgerHoldings(m) {
			held[t] += q
		}
	}
	return held
}

// ledgerHoldings is what one owner holds per type now: assets plus
// stock listed in open sell orders.
func ledgerHoldings(src ledgerSource) map[int64]int64 {
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
