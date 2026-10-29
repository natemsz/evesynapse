package app

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// The ledger fold end to end: a sale is matched to the oldest buys,
// trading carries its sales tax and broker fees, a sale no buy
// covered is an other sale, bounties stand apart, and the stock still
// held stays an open position at its cost.
func TestProfitLedgerFold(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Trader")
	ch, err := q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("read character: %v", err)
	}
	now := time.Now().UTC()
	at := func(h int) string { return now.Add(time.Duration(-h) * time.Hour).Format(time.RFC3339) }

	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletTxns, esi.WalletTransactions{
		{TransactionID: 1, Date: at(30), TypeID: 34, Quantity: 5, UnitPrice: 100, IsBuy: true},
		{TransactionID: 2, Date: at(20), TypeID: 34, Quantity: 5, UnitPrice: 120, IsBuy: true},
		{TransactionID: 3, Date: at(10), TypeID: 34, Quantity: 6, UnitPrice: 150},
		{TransactionID: 4, Date: at(5), TypeID: 35, Quantity: 2, UnitPrice: 50},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, esi.WalletJournal{
		{ID: 11, Date: at(10), RefType: "transaction_tax", Amount: -20},
		{ID: 12, Date: at(30), RefType: "brokers_fee", Amount: -10},
		{ID: 13, Date: at(8), RefType: "bounty_prizes", Amount: 1000},
		{ID: 14, Date: at(10), RefType: "market_transaction", Amount: 900},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{{ItemID: 1, TypeID: 34, Quantity: 4}})

	app.refreshProfitLedger(ctx, ch, now)

	acts, err := q.ListProfitByActivity(ctx, db.ListProfitByActivityParams{
		OwnerKind: ownerChar, OwnerIds: []int64{fixtureCharA}, FromDay: now.AddDate(0, 0, -7).Format("2006-01-02"),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string][2]float64{}
	for _, a := range acts {
		got[a.Activity] = [2]float64{a.Income, a.Costs}
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	// 6 sold at 150 = 900 against 5@100 + 1@120 = 620, plus 30 in fees.
	if g := got[actTrading]; !near(g[0], 900) || !near(g[1], 650) {
		t.Errorf("trading = %v, want in 900, costs 650", g)
	}
	if g := got[actSales]; !near(g[0], 100) || !near(g[1], 0) {
		t.Errorf("other sales = %v, want in 100 (2 sold with no buy)", g)
	}
	if g := got[actBounties]; !near(g[0], 1000) {
		t.Errorf("bounties = %v, want in 1000", g)
	}
	if _, ok := got[actMarket]; ok {
		t.Error("market_transaction journal rows counted; the transactions speak for trade")
	}
	pos, err := q.ListTradePositions(ctx, db.ListTradePositionsParams{OwnerKind: poolAcct, OwnerIds: []int64{user.ID}})
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 1 || pos[0].Quantity != 4 || !near(pos[0].Cost, 480) {
		t.Errorf("positions = %+v, want 4 of type 34 at 480 (the remaining 120s)", pos)
	}
}

// A refold from the fold checkpoint settles identically to a full
// refold: a buy that lands late, dated inside the checkpoint window
// but before a sale already settled, re-settles that sale, and the
// stored open lots carry the older buys across the checkpoint.
func TestProfitRefoldFromCheckpointMatchesFullFold(t *testing.T) {
	ctx := context.Background()
	now1 := time.Now().UTC()
	now2 := now1.AddDate(0, 0, 2)
	day := func(base time.Time, d int) string {
		return base.AddDate(0, 0, d).Add(6 * time.Hour).Format(time.RFC3339)
	}
	first := esi.WalletTransactions{
		{TransactionID: 1, Date: day(now1, -100), TypeID: 34, Quantity: 10, UnitPrice: 100, IsBuy: true},
		{TransactionID: 2, Date: day(now1, -90), TypeID: 34, Quantity: 10, UnitPrice: 110, IsBuy: true},
		{TransactionID: 3, Date: day(now1, -80), TypeID: 34, Quantity: 12, UnitPrice: 200},
		{TransactionID: 4, Date: day(now1, -50), TypeID: 34, Quantity: 5, UnitPrice: 200},
		{TransactionID: 5, Date: day(now1, -30), TypeID: 34, Quantity: 10, UnitPrice: 130, IsBuy: true},
		{TransactionID: 6, Date: day(now1, -20), TypeID: 34, Quantity: 8, UnitPrice: 200},
	}
	late := esi.WalletTransactions{
		{TransactionID: 7, Date: day(now1, -35), TypeID: 34, Quantity: 4, UnitPrice: 120, IsBuy: true},
		{TransactionID: 8, Date: day(now1, -10), TypeID: 34, Quantity: 3, UnitPrice: 200},
	}
	realized := func(app *Application) [][4]float64 {
		rows, err := app.db.QueryContext(ctx, `SELECT transaction_id, quantity, revenue, cost FROM trade_realized WHERE owner_kind = 'char' AND owner_id = $1 ORDER BY transaction_id`, fixtureCharA)
		if err != nil {
			t.Fatalf("read realized: %v", err)
		}
		defer rows.Close()
		var out [][4]float64
		for rows.Next() {
			var r [4]float64
			if err := rows.Scan(&r[0], &r[1], &r[2], &r[3]); err != nil {
				t.Fatalf("scan realized: %v", err)
			}
			out = append(out, r)
		}
		return out
	}
	setup := func(t *testing.T) (*Application, *db.Queries) {
		app, _, q := buildCorpTestApp(t, &countingTransport{})
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		seedCharacter(t, q, user.ID, fixtureCharA, "Trader")
		return app, q
	}

	// Incremental: fold the first window, then land the late rows
	// and fold again from the checkpoint the first fold stored.
	appA, qA := setup(t)
	chA, err := qA.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("read character: %v", err)
	}
	seedSnapshot(t, qA, fixtureCharA, esi.SnapWalletTxns, first)
	seedSnapshot(t, qA, fixtureCharA, esi.SnapAssets, []esi.Asset{{ItemID: 1, TypeID: 34, Quantity: 9}})
	appA.refreshProfitLedger(ctx, chA, now1)
	second := append(append(esi.WalletTransactions{}, first...), late...)
	seedSnapshot(t, qA, fixtureCharA, esi.SnapWalletTxns, second)
	seedSnapshot(t, qA, fixtureCharA, esi.SnapAssets, []esi.Asset{{ItemID: 1, TypeID: 34, Quantity: 6}})
	appA.refreshProfitLedger(ctx, chA, now2)
	gotA := realized(appA)

	// Reference: the whole history in one fold.
	appB, qB := setup(t)
	chB, err := qB.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("read character: %v", err)
	}
	seedSnapshot(t, qB, fixtureCharA, esi.SnapWalletTxns, second)
	seedSnapshot(t, qB, fixtureCharA, esi.SnapAssets, []esi.Asset{{ItemID: 1, TypeID: 34, Quantity: 6}})
	appB.refreshProfitLedger(ctx, chB, now2)
	gotB := realized(appB)

	if len(gotA) != len(gotB) {
		t.Fatalf("realized rows: checkpoint fold %v, full fold %v", gotA, gotB)
	}
	for i := range gotA {
		if gotA[i] != gotB[i] {
			t.Errorf("realized row %d: checkpoint fold %v, full fold %v", i, gotA[i], gotB[i])
		}
	}
	// The late buy at 120 steps ahead of the 130s the -20 sale had
	// originally settled against: 3 at 110, 4 at 120, 1 at 130.
	for _, r := range gotB {
		if r[0] == 6 && r[3] != 940 {
			t.Errorf("sale 6 cost = %v, want 940 after the late buy re-settles it", r[3])
		}
	}
	posA, err := qA.ListTradePositions(ctx, db.ListTradePositionsParams{OwnerKind: poolAcct, OwnerIds: []int64{chA.UserID}})
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(posA) != 1 || posA[0].Quantity != 6 || math.Abs(posA[0].Cost-780) > 1e-6 {
		t.Errorf("positions = %+v, want 6 of type 34 at 780", posA)
	}
}

// A corporation's profit shows only to a player one of whose
// characters has fetched its division journals: asking for another
// falls back to the player's own characters.
func TestProfitCorpNeedsWalletAccess(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Trader")
	if _, err := app.db.ExecContext(ctx, `INSERT INTO profit_daily (owner_kind, owner_id, day, activity, income, costs) VALUES ('corp', 98000077, $1, 'bounties', 123456789, 0)`, time.Now().UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("seed corp profit: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Trader")
	code, body := getPage(t, app, cookie, "/profit/?owner=corp-98000077")
	if code != 200 {
		t.Fatalf("GET /profit/: status %d", code)
	}
	if strings.Contains(body, "123,456,789") || strings.Contains(body, "corp-98000077") {
		t.Error("profit page shows a corporation the player has no wallet access to")
	}
}

// Pooled trading by item: an alt's buy prices the main's sale, the
// sale's tax pairs by its second, a broker fee ties to the one order
// placed within the minute, units no lot covers are typed, and a fill
// between the two characters is no trade at all.
func TestProfitPooledTradingItems(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Main")
	seedCharacter(t, q, user.ID, fixtureCharB, "Alt")
	now := time.Now().UTC().Truncate(time.Second)
	at := func(h int) string { return now.Add(time.Duration(-h) * time.Hour).Format(time.RFC3339) }

	seedSnapshot(t, q, fixtureCharB, esi.SnapWalletTxns, esi.WalletTransactions{
		{TransactionID: 1, Date: at(30), TypeID: 34, Quantity: 10, UnitPrice: 100, IsBuy: true},
		{TransactionID: 5, Date: at(3), TypeID: 36, Quantity: 2, UnitPrice: 50, IsBuy: true},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletTxns, esi.WalletTransactions{
		{TransactionID: 2, Date: at(10), TypeID: 34, Quantity: 6, UnitPrice: 150},
		{TransactionID: 3, Date: at(5), TypeID: 35, Quantity: 4, UnitPrice: 50},
		{TransactionID: 4, Date: at(3), TypeID: 36, Quantity: 2, UnitPrice: 50},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, esi.WalletJournal{
		{ID: 11, Date: at(10), RefType: "transaction_tax", Amount: -18},
		{ID: 12, Date: at(5), RefType: "transaction_tax", Amount: -4},
		{ID: 13, Date: at(3), RefType: "transaction_tax", Amount: -2},
		{ID: 14, Date: at(12), RefType: "brokers_fee", Amount: -12},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrdersHistory, esi.CharOrderHistory{
		{CharOrder: esi.CharOrder{OrderID: 77, TypeID: 34, Issued: now.Add(-12*time.Hour + 20*time.Second).Format(time.RFC3339)}, State: "expired"},
	})
	for _, id := range []int64{fixtureCharB, fixtureCharA} {
		ch, err := q.GetCharacter(ctx, id)
		if err != nil {
			t.Fatalf("read character: %v", err)
		}
		app.refreshProfitLedger(ctx, ch, now)
	}

	var cost float64
	if err := app.db.QueryRowContext(ctx, `SELECT cost FROM trade_realized WHERE owner_kind = 'char' AND owner_id = $1 AND transaction_id = 2`, fixtureCharA).Scan(&cost); err != nil || cost != 600 {
		t.Fatalf("main's sale cost = %v (%v), want 600 from the alt's buy", cost, err)
	}
	rows, err := q.ListTradeItems(ctx, db.ListTradeItemsParams{OwnerKind: ownerChar, OwnerIds: []int64{fixtureCharA, fixtureCharB}, Win: 7, BrokerInMargin: true, RowCap: 100})
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	items := map[int64]db.ListTradeItemsRow{}
	for _, r := range rows {
		items[r.TypeID] = r
	}
	if r := items[34]; r.Units != 6 || r.CovUnits != 6 || math.Abs(r.CovRevenue-r.CovCost-r.CovTax-r.Broker-270) > 1e-6 {
		t.Errorf("type 34 = %+v, want 6 covered units at 900 - 600 - 18 tax - 12 broker = 270", r)
	}
	if r := items[35]; r.CovUnits != 0 || r.GapBeforeHistory != 4 {
		t.Errorf("type 35 = %+v, want 4 units bought before the history began", r)
	}
	if _, ok := items[36]; ok {
		t.Error("a fill between the main and the alt counted as a trade")
	}
	if len(rows) == 0 || rows[0].TypeID != 34 {
		t.Errorf("first row = %+v, want the fully covered item first", rows)
	}
}
