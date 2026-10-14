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
		// The donuts: all income is bounties, all spend escrow.
		`aria-label="Income by type, last 30 days"`,
		`aria-label="Spend by type, last 30 days"`,
		`<span class="pie-l">bounty prizes</span> <b>100.0%</b>`,
		`<span class="pie-l">market escrow</span> <b>100.0%</b>`,
	)
	if strings.Contains(body, "999,999") {
		t.Fatalf("out-of-window entry leaked into the widget")
	}
}

// TestFlowPieShares: each slice is its ref type's share of the
// side's total, spend counted by size; past five types the rest
// fold into Other; one type alone draws the whole ring.
func TestFlowPieShares(t *testing.T) {
	if buildFlowPie(map[string]float64{}) != nil {
		t.Fatal("an empty side drew a donut")
	}
	pie := buildFlowPie(map[string]float64{
		"a": -600, "b": -200, "c": -100, "d": -50, "e": -30, "f": -15, "g": -5,
	})
	var labels, pcts []string
	for _, s := range pie.Slices {
		labels = append(labels, s.Label)
		pcts = append(pcts, s.Pct)
	}
	if got := strings.Join(labels, ","); got != "a,b,c,d,e,Other" {
		t.Errorf("slices %s, want the five largest and Other", got)
	}
	if got := strings.Join(pcts, ","); got != "60.0%,20.0%,10.0%,5.0%,3.0%,2.0%" {
		t.Errorf("shares %s, want 60/20/10/5/3/2", got)
	}
	if pie.Slices[5].Index != flowPieSlices {
		t.Errorf("Other wears palette %d, want the Other colour %d", pie.Slices[5].Index, flowPieSlices)
	}
	whole := buildFlowPie(map[string]float64{"bounty_prizes": 10})
	if len(whole.Slices) != 1 || whole.Slices[0].Pct != "100.0%" || strings.Count(whole.Slices[0].D, "M") != 2 {
		t.Errorf("a single type should draw the whole ring as two halves: %+v", whole.Slices)
	}
}
