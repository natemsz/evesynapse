package buildplanner

import (
	"testing"

	"evesynapse/internal/buildplan"
	"evesynapse/internal/esi"
)

// TestPlanJobFeesSumsEveryBuild: the cash view's job fees are the
// sum over every build in the tree, buy leaves and cycle stand-ins
// add nothing, and one unknown fee makes the total unknown rather
// than a short real-looking number.
func TestPlanJobFeesSumsEveryBuild(t *testing.T) {
	bp := &buildplan.Blueprint{}
	root := &buildplan.Node{Blueprint: bp, Runs: 2, JobCost: 1000, JobKnown: true, Children: []*buildplan.Node{
		{Blueprint: bp, Runs: 1, JobCost: 250, JobKnown: true},
		{Blueprint: nil, JobKnown: true},                       // a buy leaf
		{Blueprint: bp, Runs: 1, Cycle: true, JobCost: 999},    // shown as a buy
		{Blueprint: bp, Runs: 0, JobCost: 999, JobKnown: true}, // covered by stock: no job
	}}
	fees, ok := planJobFees(root)
	if !ok || fees != 1250 {
		t.Fatalf("planJobFees = %v, %v; want 1250, true", fees, ok)
	}
	root.Children[0].JobKnown = false
	if _, ok := planJobFees(root); ok {
		t.Fatal("planJobFees reported a total with one fee unknown")
	}
}

// TestPlanVerdictCountsJobFees: the cash view shows the job fees,
// the profit after them, and states per-run profit and the margin
// after fees once every fee is known.
func TestPlanVerdictCountsJobFees(t *testing.T) {
	const product = 587
	root := &buildplan.Node{
		TypeID: product, Blueprint: &buildplan.Blueprint{}, Runs: 2, ProducedQty: 2,
		LineCost: 400, CostComplete: true, JobCost: 100, JobKnown: true,
	}
	prices := map[int64]esi.MarketPrice{product: {TypeID: product, AveragePrice: 500}}
	v := planVerdictFor(&buildplan.Result{Root: root}, prices, product)
	// Sell 1,000; materials 400; fees 100.
	if v.Profit != isk(600) || v.Fees != isk(100) || v.ProfitAfterFees != isk(500) || v.AfterFeesCls != "pos" {
		t.Fatalf("verdict = %+v; want profit 600 before fees, fees 100, 500 after", v)
	}
	if v.ProfitPerRun != isk(250) || v.MarginPct != "50.0% of the sell value" {
		t.Fatalf("per run %q, margin %q; want both after fees (250, 50.0%%)", v.ProfitPerRun, v.MarginPct)
	}
}
