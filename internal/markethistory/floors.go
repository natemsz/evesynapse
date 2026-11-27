package markethistory

// Shared activity and move-threshold defaults across Trends, Market Movers,
// What changed, baskets, market browse, scanner, trade finder, and trade ideas.
const (
	// ForgeRegionID is The Forge (Jita).
	ForgeRegionID int64 = 10000002
	// DomainRegionID is Domain (Amarr).
	DomainRegionID int64 = 10000043
	// SinqLaisonRegionID is Sinq Laison (Dodixie).
	SinqLaisonRegionID int64 = 10000032
	// HeimatarRegionID is Heimatar (Rens).
	HeimatarRegionID int64 = 10000030
	// MetropolisRegionID is Metropolis (Hek).
	MetropolisRegionID int64 = 10000042

	// DefaultMinISKForge is the default 7-day average daily ISK traded floor
	// in The Forge (100M ISK/day).
	DefaultMinISKForge int64 = 100_000_000

	// DefaultMinISKHub is the default 7-day average daily ISK traded floor
	// in other trade hub regions (20M ISK/day).
	DefaultMinISKHub int64 = 20_000_000

	// DefaultMinTrades is the default 7-day average daily trade count
	// (order_count from market_history) across hub regions.
	DefaultMinTrades int64 = 10

	// DefaultChangeMoveForge is the smallest traded-median move (%) recorded
	// by the change feed in The Forge.
	DefaultChangeMoveForge = 10.0

	// DefaultChangeMoveMajorHub is the smallest traded-median move (%)
	// recorded in Domain, Sinq Laison, and other non-Forge regions.
	DefaultChangeMoveMajorHub = 15.0

	// DefaultChangeMoveMinorHub is the smallest traded-median move (%)
	// recorded in Heimatar and Metropolis, where regional books are thinner.
	DefaultChangeMoveMinorHub = 20.0
)

// DefaultMinISK returns the default 7-day average ISK traded per day floor
// for regionID: 100M in The Forge, 20M in other hub regions.
func DefaultMinISK(regionID int64) int64 {
	if regionID == ForgeRegionID {
		return DefaultMinISKForge
	}
	return DefaultMinISKHub
}

// DefaultChangeMinMove returns the minimum percentage move recorded by the
// change feed for regionID, scaled by hub liquidity: 10% in The Forge,
// 15% in Domain and Sinq Laison, and 20% in Heimatar and Metropolis.
func DefaultChangeMinMove(regionID int64) float64 {
	switch regionID {
	case ForgeRegionID:
		return DefaultChangeMoveForge
	case HeimatarRegionID, MetropolisRegionID:
		return DefaultChangeMoveMinorHub
	default:
		return DefaultChangeMoveMajorHub
	}
}
