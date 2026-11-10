// Package tradefees is the one shared home for the market fee
// math: broker fees and sales tax on a round trip. Every surface
// that quotes a spread, margin or profit after fees calls these
// functions, so no surface keeps its own copy of the formulas.
// The formulas follow CCP's Broker Fee and Sales Tax article
// (updated 2026-03-02); the skill type IDs are verified against
// ESI (Broker Relations 3446, Accounting 16622).
package tradefees

// Skill type IDs for the two skills that set the rates.
const (
	// BrokerRelations reduces the broker fee by 0.3% per level.
	BrokerRelationsSkill = 3446
	// Accounting reduces sales tax by 11% per level.
	AccountingSkill = 16622
)

// MaxSkillLevel is the level a perfect character holds.
const MaxSkillLevel = 5

// PerfectStanding is the standing a perfect character holds
// with every NPC faction and corporation.
const PerfectStanding = 10.0

// baseBrokerFee is the broker fee before skill and standing
// reductions, and baseSalesTax is the sales tax before the
// Accounting reduction.
const (
	baseBrokerFee = 0.03
	baseSalesTax  = 0.075
)

// minBrokerFee is the floor the broker fee never drops below,
// even at perfect skills and standings.
const minBrokerFee = 0.01

// BrokerFeeRate is the broker fee on one order at an NPC
// station: 3% minus 0.3% per Broker Relations level, minus
// 0.03% per point of unmodified faction standing with the
// station's owner, minus 0.02% per point of unmodified
// corporation standing, floored at 1%. Standings are the
// unmodified values ESI reports, not the effective values
// after skills.
func BrokerFeeRate(brokerRelations int, factionStanding, corpStanding float64) float64 {
	rate := baseBrokerFee -
		0.003*float64(brokerRelations) -
		0.0003*factionStanding -
		0.0002*corpStanding
	if rate < minBrokerFee {
		return minBrokerFee
	}
	return rate
}

// SalesTaxRate is the sales tax on one sale: 7.5% reduced by
// 11% per Accounting level. At level V that is about 3.37%.
func SalesTaxRate(accounting int) float64 {
	return baseSalesTax * (1 - 0.11*float64(accounting))
}

// PerfectBrokerFee is the broker fee at perfect skills and
// standings (the floor, 1%).
func PerfectBrokerFee() float64 {
	return BrokerFeeRate(MaxSkillLevel, PerfectStanding, PerfectStanding)
}

// PerfectSalesTax is the sales tax at Accounting V.
func PerfectSalesTax() float64 {
	return SalesTaxRate(MaxSkillLevel)
}

// NetRoundTrip prices one unit bought at buy and sold at sell
// at an NPC station: the buy costs the price plus the broker
// fee, the sale returns the price minus sales tax and the
// broker fee. It returns the net profit and the net buy cost
// (the denominator a spread percentage divides by).
func NetRoundTrip(buy, sell, broker, salesTax float64) (profit, buyCost float64) {
	return NetHaul(buy, sell, broker, broker, salesTax)
}

// NetHaul prices one unit bought at buy in one station and
// sold at sell in another: the buy costs the price plus the
// origin broker fee, the sale returns the price minus sales
// tax and the destination broker fee. It returns the net
// profit and the net buy cost.
func NetHaul(buy, sell, buyBroker, sellBroker, salesTax float64) (profit, buyCost float64) {
	buyCost = buy * (1 + buyBroker)
	proceeds := sell * (1 - salesTax - sellBroker)
	return proceeds - buyCost, buyCost
}

// NetHaulMarginPct is the net margin on a cross-station round
// trip as a percentage of the net buy cost: the figure Trade
// Finder quotes.
func NetHaulMarginPct(buy, sell, buyBroker, sellBroker, salesTax float64) float64 {
	profit, buyCost := NetHaul(buy, sell, buyBroker, sellBroker, salesTax)
	if buyCost == 0 {
		return 0
	}
	return profit / buyCost * 100
}

// NetSpreadPct is the net spread on a round trip as a
// percentage of the net buy cost: the figure the spread card
// and the scanner quote.
func NetSpreadPct(buy, sell, broker, salesTax float64) float64 {
	profit, buyCost := NetRoundTrip(buy, sell, broker, salesTax)
	if buyCost == 0 {
		return 0
	}
	return profit / buyCost * 100
}

// NetMarginPct is the net margin on a round trip as a
// percentage of the net sell proceeds: the figure the trader
// stats and Trade Finder quote, keeping their existing
// denominator (profit over the sell price, now net).
func NetMarginPct(buy, sell, broker, salesTax float64) float64 {
	profit, _ := NetRoundTrip(buy, sell, broker, salesTax)
	proceeds := sell * (1 - salesTax - broker)
	if proceeds == 0 {
		return 0
	}
	return profit / proceeds * 100
}

// GrossSpreadPct is the raw price gap as a percentage of the
// buy price, the spread card's secondary line.
func GrossSpreadPct(buy, sell float64) float64 {
	if buy == 0 {
		return 0
	}
	return (sell - buy) / buy * 100
}
