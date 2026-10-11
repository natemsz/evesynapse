package app

// Wallet flow tests: window math, source/sink ranking, the net
// chart, and the per-character filter wiring, over seeded
// journal snapshots through the real router.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

func TestWalletFlowRenders(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, esi.WalletJournal{
		{ID: 3, Date: rfc(now.Add(-48 * time.Hour)), RefType: "bounty_prizes", Amount: 5000, Balance: 5000},
		{ID: 2, Date: rfc(now.Add(-24 * time.Hour)), RefType: "market_escrow", Amount: -2000, Balance: 3000},
		{ID: 1, Date: rfc(now.Add(-10 * 24 * time.Hour)), RefType: "bounty_prizes", Amount: 100, Balance: 100},
		// Outside the 30-day window: ignored entirely.
		{ID: 0, Date: rfc(now.Add(-40 * 24 * time.Hour)), RefType: "bounty_prizes", Amount: 999999, Balance: 999999},
	})
	setHomeLayout(t, q, user.ID, `[{"id":"walletflow"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		"Wallet flow",
		"5,100.00",  // 30-day income: 5000 + 100, not the aged 999999
		"-2,000.00", // 30-day spend
		"3,100.00",  // 30-day net
		"bounty_prizes",
		"market_escrow",
		"pchart", // cumulative net-flow chart
		`data-table-filter="walletflow-table"`,
		`data-search="Fixture Alpha"`,
	)
	if strings.Contains(body, "999,999") {
		t.Fatalf("out-of-window entry leaked into the widget")
	}
}
