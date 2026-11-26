package module

// IsTypicalTier reports whether a pricing tier is the swept-typical
// family: the tiers that speak from a region row, as opposed to the
// basis-independent estimate, abyssal, copy-median, fallback and
// build cost tiers.
func IsTypicalTier(tier string) bool {
	switch tier {
	case AppraisalTierTypical, AppraisalTierTypicalBuy,
		AppraisalTierTypicalSplit, AppraisalTierTypicalSplitSell,
		AppraisalTierTypicalSplitBuy:
		return true
	}
	return false
}

// TierLabel words a price tier for readers. The typical family
// names its hub (short name, such as "Forge") and its side, so a
// price is never ambiguous about its source; the basis-independent
// tiers keep their plain labels.
func TierLabel(tier, hub string) string {
	switch tier {
	case AppraisalTierTypical:
		return "Our " + hub + " typical (sell)"
	case AppraisalTierTypicalBuy:
		return "Our " + hub + " typical (buy)"
	case AppraisalTierTypicalSplit:
		return "Our " + hub + " typical (split)"
	case AppraisalTierTypicalSplitSell:
		return "Our " + hub + " typical (split, sell side only)"
	case AppraisalTierTypicalSplitBuy:
		return "Our " + hub + " typical (split, buy side only)"
	case AppraisalTierEstimate:
		return "Estimated sell"
	case AppraisalTierAbyssal:
		return "Our abyssal estimate"
	case AppraisalTierCopyMedian:
		return "Our copy median"
	case AppraisalTierResearchMedian:
		return "Our research median"
	case AppraisalTierFallback:
		return "Fuzzwork aggregate"
	case AppraisalTierBuildCost:
		return "Build cost"
	}
	return "Not priced"
}
