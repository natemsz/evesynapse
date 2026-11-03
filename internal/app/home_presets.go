package app

import (
	"context"
	"sort"
)

// ---------------------------------------------------------------------------
// Per-playstyle default home layouts.
//
// An account with no saved home layout gets a preset instead of the
// one generic default: the demo account gets a showcase layout, and
// every other account gets the layout for the playstyle it picked in
// the link chooser (rememberedLink). Accounts with a saved layout,
// including a deliberately empty one ("[]"), keep theirs. The seam is
// buildHome plus the Customize reset; parseHomeLayout is untouched.
// ---------------------------------------------------------------------------

// homePresetHead is the fixed head of every preset, demo included:
// Briefing at the top, At a Glance under it, Fleet Overview under
// that, at these sizes.
var homePresetHead = []homeLayoutItem{
	{ID: widgetBriefing, W: 3, H: 1},
	{ID: widgetGlance, W: 3, H: 1},
	{ID: widgetFleet, W: 3, H: 2},
}

// playstyleWidgets maps a link activity to the home widgets that
// serve it: the specialists that expand for that playstyle, and the
// fill widgets that complete its layout at their catalog sizes.
type playstyleWidgets struct {
	Specialists []string
	Fill        []string
}

var playstyleWidgetsByActivity = map[string]playstyleWidgets{
	"pilots": {
		Specialists: []string{widgetTraining, widgetNetWorth},
	},
	"market": {
		Specialists: []string{widgetTradeIdeas, widgetMarket, widgetProfitPulse, widgetWatchlist},
		Fill:        []string{widgetWalletFlow, widgetNetWorth},
	},
	"industry": {
		Specialists: []string{widgetProdLine, widgetPI, widgetIndustry, widgetProfitPulse},
		Fill:        []string{widgetMarket, widgetWatchlist, widgetTraining, widgetNetWorth},
	},
	"corporation": {
		Specialists: []string{widgetPulse, widgetUpkeep, widgetWalletFlow},
		Fill:        []string{widgetNetWorth, widgetTraining, widgetPI, widgetIndustry, widgetMarket},
	},
	"fitting": {
		Specialists: []string{widgetTraining},
		Fill:        []string{widgetNetWorth, widgetWalletFlow, widgetMarket, widgetIndustry},
	},
}

// singlePlaystyleBodies is the body (under the fixed head) of each
// single-playstyle preset. Specialist widgets stand expanded, larger
// than their catalog size within their size bounds; the rest stand
// at their catalog sizes. Every body tiles the three-wide grid with
// no gaps. Net Worth is the one specialist that cannot grow: its
// catalog size is already its maximum.
var singlePlaystyleBodies = map[string][]homeLayoutItem{
	"pilots": {
		{ID: widgetTraining, W: 2, H: 2},
		{ID: widgetNetWorth, W: 1, H: 2},
		{ID: widgetPI, W: 1, H: 2},
		{ID: widgetWalletFlow, W: 2, H: 2},
		{ID: widgetIndustry, W: 1, H: 1},
		{ID: widgetWatchlist, W: 1, H: 1},
		{ID: widgetMarket, W: 1, H: 1},
	},
	"market": {
		{ID: widgetTradeIdeas, W: 2, H: 2},
		{ID: widgetNetWorth, W: 1, H: 2},
		{ID: widgetProfitPulse, W: 3, H: 2},
		{ID: widgetWalletFlow, W: 2, H: 2},
		{ID: widgetWatchlist, W: 1, H: 2},
		{ID: widgetMarket, W: 3, H: 1},
	},
	"industry": {
		{ID: widgetProdLine, W: 3, H: 2},
		{ID: widgetPI, W: 2, H: 2},
		{ID: widgetNetWorth, W: 1, H: 2},
		{ID: widgetProfitPulse, W: 3, H: 2},
		{ID: widgetTraining, W: 1, H: 2},
		{ID: widgetMarket, W: 1, H: 1},
		{ID: widgetWatchlist, W: 1, H: 1},
		{ID: widgetIndustry, W: 2, H: 1},
	},
	"corporation": {
		{ID: widgetWalletFlow, W: 3, H: 2},
		{ID: widgetPulse, W: 2, H: 2},
		{ID: widgetNetWorth, W: 1, H: 2},
		{ID: widgetUpkeep, W: 2, H: 1},
		{ID: widgetMarket, W: 1, H: 1},
		{ID: widgetTraining, W: 1, H: 2},
		{ID: widgetIndustry, W: 1, H: 1},
		{ID: widgetPI, W: 1, H: 2},
		{ID: widgetWatchlist, W: 1, H: 1},
	},
	"fitting": {
		{ID: widgetTraining, W: 2, H: 2},
		{ID: widgetNetWorth, W: 1, H: 2},
		{ID: widgetWalletFlow, W: 2, H: 2},
		{ID: widgetMarket, W: 1, H: 1},
		{ID: widgetIndustry, W: 1, H: 1},
	},
}

// demoHomeBody is the demo account's showcase body: a broad mix over
// the demo seed (a trader main and an industry/PI alt) at catalog
// sizes. Corp Pulse closes the layout as one wide band; the seed
// fills it (corp members, wallets and tracking under a Director).
// Upkeep and Trade Ideas stay out: the seed holds no structures and
// no stored trade-ideas answer, so both would render empty.
var demoHomeBody = []homeLayoutItem{
	{ID: widgetWalletFlow, W: 2, H: 2},
	{ID: widgetNetWorth, W: 1, H: 2},
	{ID: widgetTraining, W: 1, H: 2},
	{ID: widgetProdLine, W: 2, H: 2},
	{ID: widgetPI, W: 1, H: 2},
	{ID: widgetProfitPulse, W: 2, H: 2},
	{ID: widgetIndustry, W: 1, H: 1},
	{ID: widgetMarket, W: 1, H: 1},
	{ID: widgetWatchlist, W: 1, H: 1},
	{ID: widgetPulse, W: 3, H: 1},
}

// blendCanonicalOrder breaks ties between widgets that serve the
// same number of selected playstyles: the order the blend prefers,
// most universal first.
var blendCanonicalOrder = []string{
	widgetNetWorth, widgetTraining, widgetMarket, widgetWalletFlow,
	widgetIndustry, widgetProfitPulse, widgetWatchlist, widgetPI,
	widgetProdLine, widgetTradeIdeas, widgetPulse, widgetUpkeep,
}

// blendFullSize is a widget's full size in a blend, used for the
// universal and crossover widgets. Widgets named by only one selected
// playstyle condense to their catalog size instead.
var blendFullSize = map[string]struct{ W, H int }{
	widgetNetWorth:    {1, 2},
	widgetTraining:    {2, 2},
	widgetMarket:      {2, 2},
	widgetWalletFlow:  {3, 2},
	widgetIndustry:    {2, 2},
	widgetProfitPulse: {3, 2},
	widgetWatchlist:   {1, 2},
	widgetPI:          {2, 2},
	widgetProdLine:    {2, 2},
	widgetTradeIdeas:  {2, 2},
	widgetPulse:       {2, 2},
	widgetUpkeep:      {2, 2},
}

// defaultLayoutFor is the layout an account with no saved layout
// gets: the demo preset for the demo user, else the preset for the
// account's stored link choice, else the generic default.
func (app *Application) defaultLayoutFor(ctx context.Context, userID int64) []homeLayoutItem {
	if app.isDemoUser(ctx, userID) {
		return presetWithHead(demoHomeBody)
	}
	if choice, ok := app.rememberedLink(ctx); ok {
		if layout, ok := presetForActivities(choice.Activities); ok {
			return layout
		}
	}
	return defaultHomeLayoutItems()
}

// presetForActivities resolves stored link activities to a preset:
// one playstyle gets its own layout, several get a blend, and the
// everything choice gets the full blend of all playstyles. ok is
// false when no known playstyle was picked.
func presetForActivities(activities []string) ([]homeLayoutItem, bool) {
	selected := selectedPlaystyles(activities)
	switch {
	case len(selected) == 0:
		return nil, false
	case len(selected) == 1:
		return presetWithHead(singlePlaystyleBodies[selected[0]]), true
	default:
		return presetWithHead(blendedBody(selected)), true
	}
}

// selectedPlaystyles normalizes stored activities: known ids only,
// no duplicates, in link chooser order, with the everything choice
// expanded to every playstyle.
func selectedPlaystyles(activities []string) []string {
	want := make(map[string]bool, len(activities))
	for _, a := range activities {
		if a == activityEverything {
			for _, act := range linkActivities {
				if act.ID != activityEverything {
					want[act.ID] = true
				}
			}
			continue
		}
		if _, ok := playstyleWidgetsByActivity[a]; ok {
			want[a] = true
		}
	}
	var out []string
	for _, act := range linkActivities {
		if want[act.ID] {
			out = append(out, act.ID)
		}
	}
	return out
}

// presetWithHead puts the fixed head over a preset body and
// normalizes the result.
func presetWithHead(body []homeLayoutItem) []homeLayoutItem {
	items := make([]homeLayoutItem, 0, len(homePresetHead)+len(body))
	items = append(items, homePresetHead...)
	items = append(items, body...)
	return normalizePreset(items)
}

// normalizePreset clamps every size to its widget's bounds and drops
// ids this build's catalog does not have (a module that is not built
// in), duplicates keeping the first.
func normalizePreset(items []homeLayoutItem) []homeLayoutItem {
	out := make([]homeLayoutItem, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if seen[item.ID] {
			continue
		}
		if _, ok := widgetDefFor(item.ID); !ok {
			continue
		}
		seen[item.ID] = true
		w, h := widgetSize(item)
		out = append(out, homeLayoutItem{ID: item.ID, W: w, H: h})
	}
	return out
}

// blendedBody blends the selected playstyles' widgets into one body
// under the fixed head. A widget that serves more than one selected
// playstyle (crossover or universal information) comes first at full
// size; a widget named by only one playstyle condenses toward its
// catalog size. Every widget stands two rows tall, so the body packs
// into bands three cells wide with no gaps.
func blendedBody(selected []string) []homeLayoutItem {
	refs := make(map[string]int)
	for _, activity := range selected {
		pw := playstyleWidgetsByActivity[activity]
		for _, id := range append(append([]string(nil), pw.Specialists...), pw.Fill...) {
			refs[id]++
		}
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	rank := make(map[string]int, len(blendCanonicalOrder))
	for i, id := range blendCanonicalOrder {
		rank[id] = i
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if refs[ids[i]] != refs[ids[j]] {
			return refs[ids[i]] > refs[ids[j]]
		}
		return rank[ids[i]] < rank[ids[j]]
	})

	items := make([]homeLayoutItem, 0, len(ids))
	for _, id := range ids {
		w, h := blendSize(id, refs[id])
		items = append(items, homeLayoutItem{ID: id, W: w, H: h})
	}
	return packBlendBands(items)
}

// blendSize is a widget's size in a blend: full size when more than
// one selected playstyle names it, catalog size otherwise. The
// height is two rows either way, within every widget's bounds, so
// bands pack without gaps.
func blendSize(id string, refCount int) (w, h int) {
	if refCount > 1 {
		if full, ok := blendFullSize[id]; ok {
			return full.W, full.H
		}
	}
	def, ok := widgetDefFor(id)
	if !ok {
		return 1, 2
	}
	w = def.W
	if full, ok := blendFullSize[id]; ok && w > full.W {
		w = full.W
	}
	return w, 2
}

// packBlendBands flexes widths within each widget's bounds so the
// ordered items partition into bands exactly three cells wide. The
// search prefers each widget's target width, then the nearest width
// to it, and preserves the blend's order.
func packBlendBands(items []homeLayoutItem) []homeLayoutItem {
	if len(items) == 0 {
		return items
	}
	widths := make([]int, len(items))
	var search func(i, filled int) bool
	search = func(i, filled int) bool {
		if i == len(items) {
			return filled == 0
		}
		minW, _ := widgetMin(items[i].ID)
		maxW, _ := widgetMax(items[i].ID)
		for _, w := range widthsByPreference(items[i].W, minW, maxW) {
			next := filled + w
			if next > homeGridMax {
				continue
			}
			if next == homeGridMax {
				next = 0
			}
			widths[i] = w
			if search(i+1, next) {
				return true
			}
		}
		return false
	}
	if !search(0, 0) {
		return items
	}
	out := make([]homeLayoutItem, len(items))
	for i, item := range items {
		out[i] = homeLayoutItem{ID: item.ID, W: widths[i], H: item.H}
	}
	return out
}

// widthsByPreference lists a widget's allowed widths, target first,
// then outward from it.
func widthsByPreference(target, minW, maxW int) []int {
	if target < minW {
		target = minW
	}
	if target > maxW {
		target = maxW
	}
	out := []int{target}
	for d := 1; d <= homeGridMax; d++ {
		if target+d <= maxW {
			out = append(out, target+d)
		}
		if target-d >= minW {
			out = append(out, target-d)
		}
	}
	return out
}
