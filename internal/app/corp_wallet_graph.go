package app

import (
	"time"

	"evesynapse/internal/esi"
)

// corpWalletGraph builds the balance-over-time chart for one
// corporation wallet division from data the page already holds: the
// division's journal (every entry carries the balance after it) and the
// current balance as the last point, dated when the wallet snapshot was
// fetched. The line covers the journal window ESI keeps (about a
// month), and the note says so. With fewer than two points the chart is
// left out.
func corpWalletGraph(journal esi.CorpJournal, balance float64, balanceKnown bool, balanceAt time.Time) (*balanceChart, string) {
	asWallet := make(esi.WalletJournal, 0, len(journal))
	for _, e := range journal {
		asWallet = append(asWallet, esi.WalletJournalEntry{Date: e.Date, Balance: e.Balance})
	}
	var now *balancePoint
	if balanceKnown && !balanceAt.IsZero() {
		now = &balancePoint{At: balanceAt, Balance: balance}
	}
	points := buildBalanceSeries(asWallet, nil, now)
	if chart, ok := buildBalanceChart(points); ok {
		chart.Label = "Division balance over time"
		return &chart, ""
	}
	if len(points) == 1 {
		return nil, "Balance history is building — one point recorded so far (" + esi.FormatISK(points[0].Balance) + " ISK). The graph draws itself once the journal has a second."
	}
	return nil, "No wallet activity in this division's journal yet — the graph starts with its first entries."
}
