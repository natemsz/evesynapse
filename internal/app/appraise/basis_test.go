package appraise

import (
	"testing"

	"evesynapse/internal/app/module"
)

func TestPricingBasisLabelsAndClamp(t *testing.T) {
	domain := pricingBasis{RegionID: 10000043, HubName: "Domain", Mode: module.AppraisalModeBuy, Percent: 90}
	if got := tierLabelWithBasis(module.AppraisalTierTypicalBuy, domain); got != "Our Domain typical (buy)" {
		t.Errorf("buy label = %q", got)
	}
	if got := tierLabelWithBasis(module.AppraisalTierTypicalSplitSell, domain); got != "Our Domain typical (split, sell side only)" {
		t.Errorf("split-single label = %q", got)
	}
	if got := tierLabelWithBasis(module.AppraisalTierEstimate, domain); got != "Estimated sell" {
		t.Errorf("estimate label = %q, want its basis-independent label", got)
	}
	if got := domain.basisText(); got != "Priced from Domain typical buy prices at 90%." {
		t.Errorf("basis text = %q", got)
	}
	forge := pricingBasis{RegionID: 10000002, HubName: "The Forge", Mode: module.AppraisalModeSell, Percent: 100}
	if got := tierLabelWithBasis(module.AppraisalTierTypical, forge); got != "Our Forge typical (sell)" {
		t.Errorf("forge label = %q", got)
	}
	if got := forge.basisText(); got != "Priced from The Forge typical sell prices." {
		t.Errorf("default basis text = %q", got)
	}
	if clampPercent(10) != 50 || clampPercent(500) != 200 || clampPercent(90) != 90 {
		t.Error("percent clamp must bound to 50-200 and pass sane values through")
	}
	// A stored snapshot round-trips its basis; an old snapshot
	// without one reads as the default basis.
	b := forge.snapshot().basis()
	if b.RegionID != 10000002 || b.Mode != "sell" || b.Percent != 100 {
		t.Errorf("snapshot basis round-trip = %+v", b)
	}
	if old := (snapshotBasis{}).basis(); old.Mode != "sell" || old.Percent != 100 {
		t.Errorf("empty snapshot basis = %+v, want the sell/100 default", old)
	}
}
