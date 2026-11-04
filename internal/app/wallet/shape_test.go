package wallet

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

func TestRefCategory(t *testing.T) {
	for ref, want := range map[string]string{
		"market_transaction": "market", "brokers_fee": "fees", "transaction_tax": "fees",
		"industry_job_tax": "fees", "manufacturing": "industry", "bounty_prizes": "income",
		"agent_mission_reward": "income", "player_donation": "",
	} {
		if got := refCategory(ref); got != want {
			t.Errorf("refCategory(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestWalletChoiceKeepsItsQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/wallet/?character=9&range=7d&isk=short&cat=fees&ref=brokers_fee&txn=5", nil)
	c := parseWalletChoice(r, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	if c.rng != "7d" || c.exact || c.cat != "fees" || c.ref != "brokers_fee" || c.txn != 5 {
		t.Fatalf("parsed %+v", c)
	}
	got := c.href("/wallet/", map[string]string{"cat": ""})
	for _, want := range []string{"character=9", "range=7d", "isk=short", "ref=brokers_fee"} {
		if !strings.Contains(got, want) {
			t.Errorf("href %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "cat=") || strings.Contains(got, "txn=") {
		t.Errorf("href %q keeps a cleared choice or the open trade", got)
	}
	if !inRange("2026-10-29T00:00:00Z", c.cutoff) || inRange("2026-10-27T00:00:00Z", c.cutoff) {
		t.Error("the 7-day range cut at the wrong day")
	}
}

// A sale nets its total less the sales tax the journal charged for
// that transaction (context_id is the transaction ID), and the
// balance after it comes from its own journal entry.
func TestShapeTradeMatchesSalesTax(t *testing.T) {
	journal := esi.WalletJournal{
		{ID: 11, RefType: "market_transaction", Amount: 1000, Balance: 5000, ContextID: 77},
		{ID: 12, RefType: "transaction_tax", Amount: -36, Balance: 4964, ContextID: 77},
		{ID: 13, RefType: "transaction_tax", Amount: -99, ContextID: 78},
	}
	txns := esi.WalletTransactions{
		{TransactionID: 77, TypeID: 34, UnitPrice: 10, Quantity: 100, ClientID: 3, JournalRefID: 11},
		{TransactionID: 76, TypeID: 34, UnitPrice: 8, Quantity: 50, ClientID: 3, IsBuy: true},
	}
	view := &walletView{}
	(&mod{}).shapeTrade(view, walletChoice{exact: true}, journal, txns, walletTxnRow{Side: "Sell", Item: "Tritanium"}, txns[0])
	tr := view.Trade
	if tr == nil || view.Depth != 2 {
		t.Fatal("no trade in the inspector")
	}
	if tr.Tax != "36.00 ISK" || tr.Net != "964.00 ISK" || tr.BalanceAt != "5,000.00 ISK" {
		t.Errorf("tax %q, net %q, balance %q", tr.Tax, tr.Net, tr.BalanceAt)
	}
	if tr.Spread != "+25.0%" || !strings.HasPrefix(tr.WithTrades, "1 other trade") {
		t.Errorf("spread %q, with %q", tr.Spread, tr.WithTrades)
	}
}

func TestCSVCellNeverReadsAsAFormula(t *testing.T) {
	for in, want := range map[string]string{"=SUM(A1)": "'=SUM(A1)", "+1": "'+1", "@x": "'@x", "Tritanium": "Tritanium", "": ""} {
		if got := cell(in); got != want {
			t.Errorf("cell(%q) = %q, want %q", in, got, want)
		}
	}
}
