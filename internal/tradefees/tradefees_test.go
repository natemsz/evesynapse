package tradefees

import (
	"math"
	"testing"
)

func close(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func TestBrokerFeeRate(t *testing.T) {
	// No skills, no standings: the full 3%.
	if got := BrokerFeeRate(0, 0, 0); !close(got, 0.03) {
		t.Errorf("BrokerFeeRate(0,0,0) = %v, want 0.03", got)
	}
	// Broker Relations V alone: 3% - 1.5% = 1.5%.
	if got := BrokerFeeRate(5, 0, 0); !close(got, 0.015) {
		t.Errorf("BrokerFeeRate(5,0,0) = %v, want 0.015", got)
	}
	// Perfect skills and standings hit the 1% floor, not below.
	if got := BrokerFeeRate(5, 10, 10); !close(got, 0.01) {
		t.Errorf("BrokerFeeRate(5,10,10) = %v, want 0.01", got)
	}
	// Standings alone: 3% - 0.03%*5 - 0.02%*5 = 2.75%.
	if got := BrokerFeeRate(0, 5, 5); !close(got, 0.0275) {
		t.Errorf("BrokerFeeRate(0,5,5) = %v, want 0.0275", got)
	}
}

func TestSalesTaxRate(t *testing.T) {
	if got := SalesTaxRate(0); !close(got, 0.075) {
		t.Errorf("SalesTaxRate(0) = %v, want 0.075", got)
	}
	// Accounting V: 7.5% * (1 - 0.55) = 3.375%.
	if got := SalesTaxRate(5); !close(got, 0.03375) {
		t.Errorf("SalesTaxRate(5) = %v, want 0.03375", got)
	}
}

func TestNetRoundTrip(t *testing.T) {
	// Perfect rates, buy 87 sell 100: buy costs 87*1.01,
	// the sale returns 100*(1-0.03375-0.01) = 95.625.
	profit, buyCost := NetRoundTrip(87, 100, PerfectBrokerFee(), PerfectSalesTax())
	if !close(buyCost, 87.87) {
		t.Errorf("buy cost = %v, want 87.87", buyCost)
	}
	if !close(profit, 95.625-87.87) {
		t.Errorf("profit = %v, want %v", profit, 95.625-87.87)
	}
	// Net spread over the net buy cost.
	if got := NetSpreadPct(87, 100, PerfectBrokerFee(), PerfectSalesTax()); got < 8.82 || got > 8.83 {
		t.Errorf("NetSpreadPct = %v, want about 8.82", got)
	}
	// Net margin over the net sell proceeds.
	if got := NetMarginPct(87, 100, PerfectBrokerFee(), PerfectSalesTax()); got < 8.10 || got > 8.11 {
		t.Errorf("NetMarginPct = %v, want about 8.10", got)
	}
	// Gross gap unchanged: (100-87)/87 = 14.94%.
	if got := GrossSpreadPct(87, 100); got < 14.94 || got > 14.95 {
		t.Errorf("GrossSpreadPct = %v, want about 14.94", got)
	}
	// Level 0 rates can turn a thin gross gap negative.
	if got := NetSpreadPct(87, 100, BrokerFeeRate(0, 0, 0), SalesTaxRate(0)); got >= 0 {
		t.Errorf("NetSpreadPct at level 0 = %v, want negative", got)
	}
	// A cross-station haul with different broker fees per leg:
	// buy 87 at a 3% broker, sell 100 at a 1% broker with 3.375%
	// tax. Buy costs 89.61, the sale returns 95.625.
	profit, buyCost = NetHaul(87, 100, 0.03, 0.01, 0.03375)
	if !close(buyCost, 89.61) {
		t.Errorf("NetHaul buy cost = %v, want 89.61", buyCost)
	}
	if !close(profit, 95.625-89.61) {
		t.Errorf("NetHaul profit = %v, want %v", profit, 95.625-89.61)
	}
	if got := NetHaulMarginPct(87, 100, 0.03, 0.01, 0.03375); got < 6.71 || got > 6.72 {
		t.Errorf("NetHaulMarginPct = %v, want about 6.71", got)
	}
}
