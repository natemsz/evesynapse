package app

// The Profit page (/profit/): where the ISK came from. It reads the
// attribution the ledger pass stored (profit_ledger.go): net per
// activity over a span, the unattributed remainder beside it, the
// trades that earned most, and open positions marked at the house
// prices. Signed in; a player sees their own characters, and each
// corporation one of their characters can read the wallet of.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// profitActivityLabels names each activity for the page, in the
// order the table lists them.
var profitActivityLabels = []struct{ key, label, note string }{
	{actTrading, "Trading", "Sells less what the items cost you, less sales tax and broker fees"},
	{actIndustry, "Industry", "What jobs made, less materials and job fees"},
	{actBounties, "Bounties and missions", "Bounties, mission rewards and payouts"},
	{actPI, "Planetary industry", "Planetary goods sold, less import and export taxes"},
	{actContracts, "Contracts", "Contract prices, rewards and collateral, less contract fees"},
	{actSales, "Other item sales", "Items sold that you never bought or built here: loot, salvage, gifts"},
	{actLP, "LP stores", "LP store items sold, less the ISK and items they cost (LP counted as free)"},
	{actOther, "Other income", "Insurance and anything else paid in"},
	{actUpkeep, "Running costs", "Clones, skills, offices and other fees"},
	{actTransfers, "Transfers", "ISK moved between players and corporation wallets"},
}

type profitActivityRow struct {
	Key                string // the ledger activity key
	Label, Note        string
	Income, Costs, Net string
	NetDir             string // up | down | ""
	Share              string // of the absolute attributed total
	KeptPct            string // net as a share of gross in, "" when nothing came in
	FilterHref         string // sets activity=<key> on the page
	Active             bool   // the page's activity filter names this row
	Rhythm             *rhythmSpark
	net                float64
}

// profitItemRow is one item on the Trading tab over the window.
type profitItemRow struct {
	TypeID            int64
	Name              string
	Sold, Revenue     string
	Margin, MarginDir string
	MarginPct         string
	Covered           bool   // every unit sold is covered
	CoveredOf         string // "80 of 100 units" when not
	Delta, DeltaDir   string // against the previous window of equal length
	Gaps              []profitGap
	CanEnterBasis     bool
}

// profitGap is one reason units of an item have no margin, with how
// many.
type profitGap struct {
	Units, Label string
}

// tradeGapLabels names each typed gap for the page.
var tradeGapLabels = map[string]string{
	"before_history": "most likely bought before your wallet history here began",
	"ledger_hole":    "a gap in the wallet history: sync paused for over 30 days",
	"no_buy":         "no buy, build or entered cost: loot, contract items, gifts",
	"no_tax":         "no sales tax line matched the sale",
}

type profitPositionRow struct {
	TypeID            int64
	Name              string
	Quantity, Cost    string
	Value, Unrealized string
	Dir               string
	Source            string  // price tier note when not our own swept price
	Share             float64 // worth now as a percent of the table's total worth
	ShareW            float64 // the same share as a bar width in a 90-wide plot
	qty               int64
	value             float64
}

type profitView struct {
	Owners       []assetCharLink
	Spans        []marketTrendChoice
	Grains       []marketTrendChoice
	Grain        string // the charts' bucket unit: "day" or "week"
	OwnerLabel   string
	Corp         bool
	Days         int
	HasData      bool
	Earned       string
	EarnedDir    string
	Remainder    string
	RemainderDir string
	Change       string
	ChangeDir    string
	Activities   []profitActivityRow
	SalesTax     string
	BrokerFees   string

	// Trading tab (schema 112).
	Items          []profitItemRow
	ItemsNet       string // covered margin over every item
	ItemsNetDir    string
	ItemsDelta     string
	ItemsDeltaDir  string
	ItemsCoverage  string // share of units sold that are covered
	BrokerLoose    string // broker fees tied to no item
	ItemsAsOf      string
	Pooled         bool
	BrokerInMargin bool
	BasisPool      string // "acct-1" etc. when a cost basis can be entered here
	Recomputed     string // day the pool refolded under a new method or members
	RecomputedWhy  string

	// Industry section (schema 113).
	Blueprints      []profitBlueprintRow
	Facilities      []profitFacilityRow
	Regions         []profitRegionChoice
	IndustryRegion  string
	IndustrySideBuy bool

	// LP section (schema 114).
	LPStores   []profitLPRow
	LPPicks    []profitLPPick
	LPValue    string // the player's own ISK per LP
	LPMedian   string // their usual realized ISK per LP
	LPDelta    string // net LP over the span
	WithLP     string // earned with that LP at the player's value
	WithLPDir  string
	Positions  []profitPositionRow
	OpenCost   string
	OpenValue  string
	Unrealized string
	UnrealDir  string

	// The overhaul's modes and the figures they drive.
	Modes         profitModes
	SplitLinks    []marketTrendChoice
	ScaleLinks    []marketTrendChoice
	TrendLinks    []marketTrendChoice
	EffLinks      []marketTrendChoice
	BulLinks      []marketTrendChoice
	ActivityLabel string // set when the activity filter is on
	ClearActHref  string
	UncoveredWhen string // "on Oct 14" under a bucket filter, "" for the whole span or no filter
	ClearUncHref  string
	Hero          *profitHero
	Charts        *pfCharts
	Turnover      []bulletBar

	// KPI additions: sparklines on the Earned and Net worth
	// change cards, the gap note, and the coverage card.
	EarnedSparkLine, EarnedSparkArea string
	ChangeSparkLine, ChangeSparkArea string
	ChangeNote                       string
	CoveragePct                      string // "" when nothing sold
	CoverageUncovered                string // pill label: "3 units need a cost"
	CoverageHref                     string
	ItemsUnits, ItemsCovUnits        int64

	// The Not Explained split: price drift on held stock
	// (estimate) and the wallet delta left over.
	NEDrift, NEDriftDir       string
	NEUnaccounted, NEUnaccDir string
	NEHas                     bool
	ItemGroups                []profitItemGroup
	UncoveredRaw              string // the raw uncovered= value, "" when off

	// Working state for the builders below.
	fromDay   string
	openValue float64
	assetsNow float64
	hasAssets bool
	hrefModes func(m profitModes) string // the page's link builder, bound to owner, span and grain
}

// modeHref builds a page link carrying the given modes, plus a
// fragment. A view built outside the handler (tests, the chart
// builder's own fixtures) has no link builder and gets "".
func (v *profitView) modeHref(m profitModes, frag string) string {
	if v.hrefModes == nil {
		return ""
	}
	return v.hrefModes(m) + frag
}

// profitModes is the page's display state from the query string:
// how the hero splits and scales, what the efficiency panel
// shows, how the turnover panel reads out, and the two filters.
type profitModes struct {
	Split     string // total | character | activity
	Scale     string // shared | indexed
	Trend     string // cumulative | daily
	Eff       string // efficiency | fees
	Bul       string // abs | pct
	Activity  string // activity key filter, "" for all
	Uncovered string // "" | "1" | a bucket start day
}

// profitItemGroup is a run of item rows under one subhead (the
// uncovered group), or the whole table under no subhead.
type profitItemGroup struct {
	Title string
	Rows  []profitItemRow
}

func dirOf(v float64) string {
	switch {
	case v > 0.005:
		return "up"
	case v < -0.005:
		return "down"
	}
	return ""
}

func signedISK(v float64) string {
	if v < 0 {
		return "-" + esi.FormatISK(-v)
	}
	return esi.FormatISK(v)
}

func (app *Application) handleProfit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	if userID == 0 {
		app.render(ctx, w, http.StatusOK, "profit.html", data)
		return
	}
	q := r.URL.Query()
	modes := profitModes{
		Split: module.QueryValue(q, "split"), Scale: module.QueryValue(q, "scale"),
		Trend: module.QueryValue(q, "trend"), Eff: module.QueryValue(q, "eff"),
		Bul: module.QueryValue(q, "bul"), Activity: module.QueryValue(q, "activity"),
		Uncovered: module.QueryValue(q, "uncovered"),
	}
	view, err := app.buildProfitView(ctx, userID, module.QueryValue(q, "owner"), module.QueryValue(q, "days"), module.QueryValue(q, "grain"), modes)
	if err != nil {
		logging.Errorf("profit: user %d: %v", userID, err)
		data.Error = "Profit figures are unavailable right now. Try again shortly."
	}
	data.Profit = view
	app.render(ctx, w, http.StatusOK, "profit.html", data)
}

// profitCorpsFor lists the corporations a player may see profit for:
// those whose division journals one of their characters has fetched,
// which ESI allows only with the wallet roles.
func (app *Application) profitCorpsFor(ctx context.Context, chars []db.Character) map[int64]bool {
	out := map[int64]bool{}
	if len(chars) == 0 {
		return out
	}
	ids := make([]int64, len(chars))
	for i, ch := range chars {
		ids[i] = ch.CharacterID
	}
	corpOf := map[int64]int64{}
	if rows, err := app.queries.ListCharacterCorporations(ctx, ids); err == nil {
		for _, r := range rows {
			corpOf[r.CharacterID] = r.CorporationID
		}
	}
	kinds := make([]string, 0, walletDivisionCount)
	for d := 1; d <= walletDivisionCount; d++ {
		kinds = append(kinds, fmt.Sprintf("%s%d", esi.SnapCorpJournalPrefix, d))
	}
	withJournal := map[int64]bool{}
	if rows, err := app.queries.ListCharactersWithSnapshots(ctx, db.ListCharactersWithSnapshotsParams{CharacterIds: ids, Kinds: kinds}); err == nil {
		for _, id := range rows {
			withJournal[id] = true
		}
	}
	for _, ch := range chars {
		corp := corpOf[ch.CharacterID]
		if corp <= 0 || isNPCCorporation(corp) || !withJournal[ch.CharacterID] {
			continue
		}
		out[corp] = true
	}
	return out
}

func (app *Application) buildProfitView(ctx context.Context, userID int64, ownerRaw, daysRaw, grainRaw string, modes profitModes) (*profitView, error) {
	view := &profitView{Days: 30}
	// Mode values normalize to their defaults; anything else in
	// the query is ignored rather than echoed into links.
	switch modes.Split {
	case "character", "activity":
	default:
		modes.Split = "total"
	}
	if modes.Scale != "indexed" {
		modes.Scale = "shared"
	}
	if modes.Trend != "daily" {
		modes.Trend = "cumulative"
	}
	if modes.Eff != "fees" {
		modes.Eff = "efficiency"
	}
	if modes.Bul != "pct" {
		modes.Bul = "abs"
	}
	if modes.Activity != "" {
		ok := false
		for _, l := range profitActivityLabels {
			ok = ok || l.key == modes.Activity
		}
		if !ok {
			modes.Activity = ""
		}
	}
	if modes.Uncovered != "1" {
		if _, err := time.Parse("2006-01-02", modes.Uncovered); err != nil {
			modes.Uncovered = ""
		}
	}
	view.Modes = modes
	view.UncoveredRaw = modes.Uncovered
	switch daysRaw {
	case "7":
		view.Days = 7
	case "90":
		view.Days = 90
	case "365":
		view.Days = 365
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return view, err
	}
	corps := app.profitCorpsFor(ctx, chars)
	kind, ids := ownerChar, make([]int64, 0, len(chars))
	for _, ch := range chars {
		ids = append(ids, ch.CharacterID)
	}
	view.OwnerLabel = "All your characters"
	ownerParam := ""
	switch {
	case strings.HasPrefix(ownerRaw, "corp-"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(ownerRaw, "corp-"), 10, 64)
		if corps[id] {
			kind, ids, view.Corp, ownerParam = ownerCorp, []int64{id}, true, ownerRaw
			view.OwnerLabel = app.CorpDisplayName(ctx, id)
		}
	case ownerRaw != "":
		id, _ := strconv.ParseInt(ownerRaw, 10, 64)
		for _, ch := range chars {
			if ch.CharacterID == id {
				ids, ownerParam, view.OwnerLabel = []int64{id}, ownerRaw, ch.Name
			}
		}
	}

	// The charts' grain: an explicit grain=day|week wins; absent
	// one, spans up to 90 days read by day and the year by week.
	view.Grain = grainRaw
	chosen := view.Grain == "day" || view.Grain == "week"
	if !chosen {
		view.Grain = "day"
		if view.Days > 90 {
			view.Grain = "week"
		}
	}

	// One link builder for the whole page: every link carries the
	// full current state (owner, span, grain, modes) and changes
	// only what its group owns, so no control resets another.
	mkHref := func(owner string, days int, grain string, grainChosen bool, m profitModes) string {
		q := url.Values{}
		q.Set("days", strconv.Itoa(days))
		if owner != "" {
			q.Set("owner", owner)
		}
		if grainChosen {
			q.Set("grain", grain)
		}
		if m.Split != "total" {
			q.Set("split", m.Split)
		}
		if m.Scale != "shared" {
			q.Set("scale", m.Scale)
		}
		if m.Trend != "cumulative" {
			q.Set("trend", m.Trend)
		}
		if m.Eff != "efficiency" {
			q.Set("eff", m.Eff)
		}
		if m.Bul != "abs" {
			q.Set("bul", m.Bul)
		}
		if m.Activity != "" {
			q.Set("activity", m.Activity)
		}
		if m.Uncovered != "" {
			q.Set("uncovered", m.Uncovered)
		}
		return "/profit/?" + q.Encode()
	}
	href := func(owner string, days int) string { return mkHref(owner, days, view.Grain, chosen, modes) }
	view.hrefModes = func(m profitModes) string { return mkHref(ownerParam, view.Days, view.Grain, chosen, m) }
	modeLinks := func(set func(m *profitModes, v string), opts [][2]string, cur string) []marketTrendChoice {
		var out []marketTrendChoice
		for _, o := range opts {
			m := modes
			set(&m, o[0])
			out = append(out, marketTrendChoice{Label: o[1], Href: view.hrefModes(m), Active: o[0] == cur})
		}
		return out
	}
	view.SplitLinks = modeLinks(func(m *profitModes, v string) { m.Split = v },
		[][2]string{{"total", "Total"}, {"character", "By character"}, {"activity", "By activity"}}, modes.Split)
	view.ScaleLinks = modeLinks(func(m *profitModes, v string) { m.Scale = v },
		[][2]string{{"shared", "Shared scale"}, {"indexed", "Indexed"}}, modes.Scale)
	view.TrendLinks = modeLinks(func(m *profitModes, v string) { m.Trend = v },
		[][2]string{{"cumulative", "Cumulative"}, {"daily", "Daily net"}}, modes.Trend)
	view.EffLinks = modeLinks(func(m *profitModes, v string) { m.Eff = v },
		[][2]string{{"efficiency", "Efficiency"}, {"fees", "Fee deep-dive"}}, modes.Eff)
	view.BulLinks = modeLinks(func(m *profitModes, v string) { m.Bul = v },
		[][2]string{{"abs", "Absolute ISK"}, {"pct", "Retention %"}}, modes.Bul)
	view.Owners = append(view.Owners, assetCharLink{Href: href("", view.Days), Name: "All your characters", Active: ownerParam == ""})
	for _, ch := range chars {
		o := strconv.FormatInt(ch.CharacterID, 10)
		view.Owners = append(view.Owners, assetCharLink{ID: ch.CharacterID, Href: href(o, view.Days), Name: ch.Name, Active: ownerParam == o, Image: portraitURL(ch.CharacterID, 64)})
	}
	corpIDs := make([]int64, 0, len(corps))
	for id := range corps {
		corpIDs = append(corpIDs, id)
	}
	sort.Slice(corpIDs, func(i, j int) bool { return corpIDs[i] < corpIDs[j] })
	for _, id := range corpIDs {
		o := "corp-" + strconv.FormatInt(id, 10)
		view.Owners = append(view.Owners, assetCharLink{ID: id, Href: href(o, view.Days), Name: app.CorpDisplayName(ctx, id), Active: ownerParam == o, Image: fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=64", id)})
	}
	for _, d := range []int{7, 30, 90, 365} {
		view.Spans = append(view.Spans, marketTrendChoice{Label: fmt.Sprintf("%d days", d), Href: href(ownerParam, d), Active: d == view.Days})
	}
	for _, g := range []struct{ unit, label string }{{"day", "By day"}, {"week", "By week"}} {
		view.Grains = append(view.Grains, marketTrendChoice{Label: g.label, Href: mkHref(ownerParam, view.Days, g.unit, true, modes), Active: g.unit == view.Grain})
	}
	if modes.Activity != "" {
		m := modes
		m.Activity = ""
		view.ClearActHref = view.hrefModes(m)
		for _, l := range profitActivityLabels {
			if l.key == modes.Activity {
				view.ActivityLabel = l.label
			}
		}
	}
	if modes.Uncovered != "" {
		m := modes
		m.Uncovered = ""
		view.ClearUncHref = view.hrefModes(m) + "#items"
		if modes.Uncovered != "1" {
			if d, err := time.Parse("2006-01-02", modes.Uncovered); err == nil {
				if view.Grain == "week" {
					view.UncoveredWhen = " in the week of " + d.Format("Jan 2")
				} else {
					view.UncoveredWhen = " on " + d.Format("Jan 2")
				}
			}
		}
	}
	m := modes
	m.Uncovered = "1"
	view.CoverageHref = view.hrefModes(m) + "#items"
	if len(ids) == 0 {
		return view, nil
	}

	from := time.Now().UTC().AddDate(0, 0, -view.Days+1)
	fromDay := from.Format("2006-01-02")
	view.fromDay = fromDay
	fromAt, _ := time.Parse("2006-01-02", fromDay)

	acts, err := app.queries.ListProfitByActivity(ctx, db.ListProfitByActivityParams{OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return view, err
	}
	byAct := map[string]db.ListProfitByActivityRow{}
	for _, a := range acts {
		byAct[a.Activity] = a
	}
	var earned, absTotal, remainder float64
	for _, l := range profitActivityLabels {
		a, ok := byAct[l.key]
		if !ok {
			continue
		}
		net := a.Income - a.Costs
		earned += net
		absTotal += math.Abs(net)
		row := profitActivityRow{
			Key: l.key, Label: l.label, Note: l.note, Income: esi.FormatISK(a.Income), Costs: esi.FormatISK(a.Costs),
			Net: signedISK(net), NetDir: dirOf(net), net: net, Active: modes.Activity == l.key,
		}
		if a.Income > 0 {
			row.KeptPct = fmt.Sprintf("%.1f%%", net/a.Income*100)
		}
		fm := modes
		fm.Activity = l.key
		row.FilterHref = view.hrefModes(fm)
		view.Activities = append(view.Activities, row)
	}
	view.Turnover = buildBullets(byAct, modes.Bul)
	for i := range view.Activities {
		if absTotal > 0 {
			share := math.Abs(view.Activities[i].net) / absTotal * 100
			view.Activities[i].Share = fmt.Sprintf("%.0f%%", share)
			if share > 0 && share < 0.5 {
				view.Activities[i].Share = "<1%"
			}
		}
	}
	sort.SliceStable(view.Activities, func(i, j int) bool { return view.Activities[i].net > view.Activities[j].net })
	view.HasData = len(view.Activities) > 0
	view.Earned, view.EarnedDir = signedISK(earned), dirOf(earned)
	if r, ok := byAct[actUnattributed]; ok && !view.Corp {
		remainder = r.Income - r.Costs
		view.Remainder, view.RemainderDir = signedISK(remainder), dirOf(remainder)
		view.Change, view.ChangeDir = signedISK(earned+remainder), dirOf(earned+remainder)
		if remainder > 0 {
			view.ChangeNote = "The lead over Earned is ISK now held as stock and assets"
		} else if remainder < 0 {
			view.ChangeNote = "Earned ran ahead of net worth: value left stock and assets, or losses landed"
		}
	}

	feesBroker := 0.0
	if fees, err := app.queries.SumTradingFeesFrom(ctx, db.SumTradingFeesFromParams{OwnerKind: kind, OwnerIds: ids, FromAt: fromAt}); err == nil {
		view.SalesTax, view.BrokerFees = esi.FormatISK(fees.SalesTax), esi.FormatISK(fees.BrokerFees)
		feesBroker = fees.BrokerFees
	}

	prefs := app.profitPrefsFor(ctx, userID)
	view.Pooled, view.BrokerInMargin = prefs.Pooled, prefs.BrokerInMargin
	// Positions and fold state sit with the pool: the account when
	// books are pooled, else each character alone; a corporation is
	// its own.
	poolKind, poolIDs := kind, ids
	if kind == ownerChar && prefs.Pooled {
		poolKind, poolIDs = poolAcct, []int64{userID}
	}
	if len(poolIDs) == 1 {
		view.BasisPool = poolKind + "-" + strconv.FormatInt(poolIDs[0], 10)
	}
	if st, err := app.queries.GetLedgerPoolState(ctx, db.GetLedgerPoolStateParams{PoolKind: poolKind, PoolID: poolIDs[0]}); err == nil &&
		(st.RefoldReason == "method" || st.RefoldReason == "members") && time.Since(st.RefoldedAt) < 14*24*time.Hour {
		view.Recomputed = st.RefoldedAt.UTC().Format("2006-01-02")
		view.RecomputedWhy = st.RefoldReason
	}
	if err := app.attachTradeItems(ctx, view, kind, ids, feesBroker, prefs.BrokerInMargin); err != nil {
		return view, err
	}

	if kind == ownerChar {
		if err := app.attachIndustry(ctx, view, userID, poolKind, poolIDs, prefs); err != nil {
			return view, err
		}
		if err := app.attachLP(ctx, view, poolKind, poolIDs, ids, prefs); err != nil {
			return view, err
		}
		if lp, err := app.queries.SumProfitLPDelta(ctx, db.SumProfitLPDeltaParams{OwnerKind: kind, OwnerIds: ids, FromDay: fromDay}); err == nil && lp != 0 {
			view.LPDelta = esi.FormatInt(lp)
			if prefs.LPValue > 0 {
				v := earned + prefs.LPValue*float64(lp)
				view.WithLP, view.WithLPDir = signedISK(v), dirOf(v)
			}
		}
	}
	if err := app.attachProfitPositions(ctx, view, poolKind, poolIDs); err != nil {
		return view, err
	}
	// Blueprint rows with stock still held link to that item's
	// row in Open positions.
	if len(view.Positions) > 0 {
		held := map[int64]bool{}
		for _, p := range view.Positions {
			held[p.TypeID] = true
		}
		for i := range view.Blueprints {
			if view.Blueprints[i].Stock != "" && held[view.Blueprints[i].TypeID] {
				view.Blueprints[i].StockHref = "#pos-" + strconv.FormatInt(view.Blueprints[i].TypeID, 10)
			}
		}
	}
	if !view.Corp && view.Change != "" {
		app.attachNotExplained(ctx, view, remainder, fromDay)
		// The latest net worth sample's split (wallets versus
		// assets and escrow), the home chart's two parts, for the
		// hero gap's composition.
		if hist, err := app.queries.ListUserWalletHistory(ctx, userID); err == nil {
			latest := map[int64]db.WalletHistory{}
			for _, h := range hist {
				if h.NetWorth.Valid {
					latest[h.CharacterID] = h
				}
			}
			for _, h := range latest {
				view.assetsNow += h.NetWorth.Float64 - h.Balance
				view.hasAssets = true
			}
		}
	}
	names := map[int64]string{}
	for _, ch := range chars {
		names[ch.CharacterID] = ch.Name
	}
	if view.Charts, err = app.buildProfitCharts(ctx, view, kind, ids, names, fromDay, prefs.BrokerInMargin); err != nil {
		return view, err
	}
	if view.Charts != nil {
		view.Hero = view.Charts.Hero
	}
	return view, nil
}

// attachNotExplained splits the Not Explained remainder into the
// part repricing explains and the part nothing explains. The
// drift estimate reprices the stock held now at the span's first
// stat-pack prices against its current appraisal: a value move
// that never crossed the wallet. It is an estimate, and the page
// labels it one; the delta is whatever is left.
func (app *Application) attachNotExplained(ctx context.Context, view *profitView, remainder float64, fromDay string) {
	view.NEHas = true
	view.NEUnaccounted, view.NEUnaccDir = signedISK(remainder), dirOf(remainder)
	if len(view.Positions) == 0 {
		// Nothing held, so nothing can reprice: the drift is
		// zero, stated rather than left blank.
		view.NEDrift, view.NEDriftDir = signedISK(0), dirOf(0)
		return
	}
	held := make([]profitPositionRow, len(view.Positions))
	copy(held, view.Positions)
	sort.Slice(held, func(i, j int) bool { return held[i].value > held[j].value })
	limit := min(len(held), 20)
	var drift float64
	priced := false
	for _, p := range held[:limit] {
		if p.qty <= 0 || p.value <= 0 {
			continue
		}
		rows, err := app.queries.ListMarketRegionStatsDailyRecent(ctx, db.ListMarketRegionStatsDailyRecentParams{
			RegionID: defaultMarketRegion, TypeID: p.TypeID, Since: fromDay, RowLimit: int32(view.Days) + 2,
		})
		if err != nil || len(rows) == 0 || rows[0].TypicalSell <= 0 {
			continue
		}
		drift += float64(p.qty) * (p.value/float64(p.qty) - rows[0].TypicalSell)
		priced = true
	}
	if !priced {
		return
	}
	view.NEDrift, view.NEDriftDir = signedISK(drift), dirOf(drift)
	rest := remainder - drift
	view.NEUnaccounted, view.NEUnaccDir = signedISK(rest), dirOf(rest)
}

// attachProfitPositions marks the open positions at the house prices
// in The Forge, through the stat pack loader.
func (app *Application) attachProfitPositions(ctx context.Context, view *profitView, kind string, ids []int64) error {
	pos, err := app.queries.ListTradePositions(ctx, db.ListTradePositionsParams{OwnerKind: kind, OwnerIds: ids})
	if err != nil || len(pos) == 0 {
		return err
	}
	typeIDs := make([]int64, 0, len(pos))
	for _, p := range pos {
		typeIDs = append(typeIDs, p.TypeID)
	}
	packs, err := app.loadStatPacks(ctx, defaultMarketRegion, typeIDs)
	if err != nil {
		return err
	}
	now := time.Now()
	var cost, value float64
	for _, p := range pos {
		row := profitPositionRow{TypeID: p.TypeID, Name: app.typeNameOrID(ctx, p.TypeID), Quantity: esi.FormatInt(p.Quantity), Cost: esi.FormatISK(p.Cost), qty: p.Quantity}
		cost += p.Cost
		if pk := packs[p.TypeID]; pk != nil {
			price := resolveAppraisalPriceMode(pk.Own, pk.Estimate, pk.Fallback, now, module.AppraisalModeSell)
			if price.ok {
				v := price.unit * float64(p.Quantity)
				value += v
				row.value = v
				row.Value = esi.FormatISK(v)
				row.Unrealized, row.Dir = signedISK(v-p.Cost), dirOf(v-p.Cost)
				switch price.tier {
				case module.AppraisalTierEstimate:
					row.Source = "estimate"
				case module.AppraisalTierFallback:
					row.Source = fallbackSourceLabel + ", " + ageText(now, price.asOf)
				}
			}
		}
		view.Positions = append(view.Positions, row)
	}
	if value > 0 {
		for i := range view.Positions {
			view.Positions[i].Share = view.Positions[i].value / value * 100
			view.Positions[i].ShareW = view.Positions[i].value / value * 90
		}
	}
	view.openValue = value
	view.OpenCost, view.OpenValue = esi.FormatISK(cost), esi.FormatISK(value)
	view.Unrealized, view.UnrealDir = signedISK(value-cost), dirOf(value-cost)
	return nil
}
