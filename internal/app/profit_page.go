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
	Label, Note        string
	Income, Costs, Net string
	NetDir             string // up | down | ""
	Share              string // of the absolute attributed total
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
	Source            string // price tier note when not our own swept price
}

type profitOwnerChoice struct {
	Href, Label string
	Active      bool
}

type profitView struct {
	Owners       []profitOwnerChoice
	Spans        []marketTrendChoice
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
	Chart      *profitChart
	Charts     *pfCharts
	Donut      *pfDonut
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
	view, err := app.buildProfitView(ctx, userID, module.QueryValue(q, "owner"), module.QueryValue(q, "days"))
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

func (app *Application) buildProfitView(ctx context.Context, userID int64, ownerRaw, daysRaw string) (*profitView, error) {
	view := &profitView{Days: 30}
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

	href := func(owner string, days int) string {
		h := "/profit/?days=" + strconv.Itoa(days)
		if owner != "" {
			h += "&owner=" + owner
		}
		return h
	}
	view.Owners = append(view.Owners, profitOwnerChoice{Href: href("", view.Days), Label: "All your characters", Active: ownerParam == ""})
	for _, ch := range chars {
		o := strconv.FormatInt(ch.CharacterID, 10)
		view.Owners = append(view.Owners, profitOwnerChoice{Href: href(o, view.Days), Label: ch.Name, Active: ownerParam == o})
	}
	corpIDs := make([]int64, 0, len(corps))
	for id := range corps {
		corpIDs = append(corpIDs, id)
	}
	sort.Slice(corpIDs, func(i, j int) bool { return corpIDs[i] < corpIDs[j] })
	for _, id := range corpIDs {
		o := "corp-" + strconv.FormatInt(id, 10)
		view.Owners = append(view.Owners, profitOwnerChoice{Href: href(o, view.Days), Label: app.CorpDisplayName(ctx, id), Active: ownerParam == o})
	}
	for _, d := range []int{7, 30, 90, 365} {
		view.Spans = append(view.Spans, marketTrendChoice{Label: fmt.Sprintf("%d days", d), Href: href(ownerParam, d), Active: d == view.Days})
	}
	if len(ids) == 0 {
		return view, nil
	}

	from := time.Now().UTC().AddDate(0, 0, -view.Days+1)
	fromDay := from.Format("2006-01-02")
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
		view.Activities = append(view.Activities, profitActivityRow{
			Label: l.label, Note: l.note, Income: esi.FormatISK(a.Income), Costs: esi.FormatISK(a.Costs),
			Net: signedISK(net), NetDir: dirOf(net), net: net,
		})
	}
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
	view.Donut = buildIncomeDonut(acts)
	view.Earned, view.EarnedDir = signedISK(earned), dirOf(earned)
	if r, ok := byAct[actUnattributed]; ok && !view.Corp {
		remainder = r.Income - r.Costs
		view.Remainder, view.RemainderDir = signedISK(remainder), dirOf(remainder)
		view.Change, view.ChangeDir = signedISK(earned+remainder), dirOf(earned+remainder)
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

	daily, err := app.queries.ListProfitDaily(ctx, db.ListProfitDailyParams{OwnerKind: kind, OwnerIds: ids, FromDay: fromDay})
	if err != nil {
		return view, err
	}
	view.Chart = buildProfitChart(daily, !view.Corp)
	names := map[int64]string{}
	for _, ch := range chars {
		names[ch.CharacterID] = ch.Name
	}
	if view.Charts, err = app.buildProfitCharts(ctx, view, kind, ids, names, fromDay, prefs.BrokerInMargin); err != nil {
		return view, err
	}
	return view, nil
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
		row := profitPositionRow{TypeID: p.TypeID, Name: app.typeNameOrID(ctx, p.TypeID), Quantity: esi.FormatInt(p.Quantity), Cost: esi.FormatISK(p.Cost)}
		cost += p.Cost
		if pk := packs[p.TypeID]; pk != nil {
			price := resolveAppraisalPriceMode(pk.Own, pk.Estimate, pk.Fallback, now, module.AppraisalModeSell)
			if price.ok {
				v := price.unit * float64(p.Quantity)
				value += v
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
	view.OpenCost, view.OpenValue = esi.FormatISK(cost), esi.FormatISK(value)
	view.Unrealized, view.UnrealDir = signedISK(value-cost), dirOf(value-cost)
	return nil
}

// profitChart is the cumulative earned line and, for characters, the
// cumulative net worth change line, on one signed scale.
type profitChart struct {
	Width, Height int
	EarnedPoints  string
	ChangePoints  string
	ZeroY         float64
	High, Low     string
	First, Last   string
	Days          int
}

func buildProfitChart(rows []db.ListProfitDailyRow, withChange bool) *profitChart {
	if len(rows) < 2 {
		return nil
	}
	const w, h, pad = 600, 160, 8
	earned := make([]float64, len(rows))
	change := make([]float64, len(rows))
	var e, c float64
	lo, hi := 0.0, 0.0
	for i, r := range rows {
		e += r.Attributed
		c += r.Attributed + r.Unattributed
		earned[i], change[i] = e, c
		lo, hi = math.Min(lo, e), math.Max(hi, e)
		if withChange {
			lo, hi = math.Min(lo, c), math.Max(hi, c)
		}
	}
	if hi == lo {
		hi = lo + 1
	}
	n := len(rows)
	x := func(i int) float64 { return float64(i) * float64(w) / float64(n-1) }
	y := func(v float64) float64 { return pad + (hi-v)/(hi-lo)*float64(h-2*pad) }
	line := func(vs []float64) string {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = strconv.FormatFloat(x(i), 'f', 1, 64) + "," + strconv.FormatFloat(y(v), 'f', 1, 64)
		}
		return strings.Join(parts, " ")
	}
	ch := &profitChart{Width: w, Height: h, Days: n, EarnedPoints: line(earned), ZeroY: y(0),
		High: signedISK(hi), Low: signedISK(lo), First: rows[0].Day, Last: rows[n-1].Day}
	if withChange {
		ch.ChangePoints = line(change)
	}
	return ch
}
