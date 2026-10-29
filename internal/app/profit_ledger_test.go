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
	pos, err := q.ListTradePositions(ctx, db.ListTradePositionsParams{OwnerKind: ownerChar, OwnerIds: []int64{fixtureCharA}})
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 1 || pos[0].Quantity != 4 || !near(pos[0].Cost, 480) {
		t.Errorf("positions = %+v, want 4 of type 34 at 480 (the remaining 120s)", pos)
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
