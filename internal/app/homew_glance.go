package app

import (
	"context"
	"fmt"
	"html/template"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

func init() {
	choices := make([]string, len(glanceTiles))
	for i, t := range glanceTiles {
		choices[i] = t.ID + ":" + t.Label
	}
	figures := optMulti("figures", "Figures", glanceDefault, glanceMax, choices...)
	for _, t := range glanceTiles {
		if t.Requires != "" {
			figures = choiceRequires(figures, t.ID, t.Requires)
		}
	}
	registerHomeWidget(homeWidgetDef{
		ID: widgetGlance, Title: "At a Glance", Category: wcatOverview, Feature: "networth",
		Href:        "/characters/",
		Description: "The figures you pick, in one band: net worth, wallets, assets, training, orders, jobs and more.",
		Default:     wSize{3, 1},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "Your first figure, its 7-day change and a 30-day line."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "Your figures as tiles in one row, as many as fit."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Your figures as tiles, one per row."},
			{Name: "medium", Sizes: "2x2", Says: "Your figures in two columns, each with its line or meter."},
			{Name: "large", Sizes: "3x2", Says: "Your figures in three columns, each with its line or meter."},
		},
		Needs: glanceNeeds(),
		Options: []widgetOption{
			withHint(figures,
				"Pick up to six. They show in this order; the first one leads the smallest size."),
		},
		Build: buildGlanceWidget,
	})
}

// glanceDefault is the figures a card shows until its owner picks.
const glanceDefault = "networth,wallets,assets,training"

// glanceMax is the most figures a card shows: the large form's six.
const glanceMax = 6

// glanceTileDef is one figure At a Glance can show: its id (stored in
// the figures option; never rename one), its label, the snapshot kinds
// it reads, the module it needs (none for core figures), and how it is
// built from the render's shared data.
type glanceTileDef struct {
	ID       string
	Label    string
	Needs    []string
	Requires string
	Build    func(ctx context.Context, g *glanceBuild) glanceTile
}

// glanceTiles is the tile catalog, in the order the settings sheet
// lists it and the card shows the picks.
var glanceTiles = []glanceTileDef{
	{ID: "networth", Label: "Fleet net worth", Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders}, Build: glanceNetWorth},
	{ID: "wallets", Label: "Liquid wallets", Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders}, Build: glanceWallets},
	{ID: "assets", Label: "Hangar & asset value", Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders}, Build: glanceAssets},
	{ID: "training", Label: "Skill training", Needs: []string{esi.SnapSkillqueue}, Build: glanceTraining},
	{ID: "profit", Label: "Kept profit, 30 days", Build: glanceProfit},
	{ID: "escrow", Label: "Order escrow", Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders}, Build: glanceEscrow},
	{ID: "orders", Label: "Open orders", Needs: []string{esi.SnapOrders}, Build: glanceOrders},
	{ID: "undercut", Label: "Orders undercut", Build: glanceUndercut},
	{ID: "jobs", Label: "Jobs ready", Needs: []string{esi.SnapIndustryJobs}, Build: glanceJobs},
	{ID: "extractors", Label: "Extractors stopped", Needs: []string{esi.SnapPlanets, needPlanetLayouts}, Build: glanceExtractors},
	{ID: "mail", Label: "Unread mail", Needs: []string{esi.SnapMailLabels}, Requires: "mail", Build: glanceMail},
	{ID: "srp", Label: "Ship replacement pending", Requires: "srp", Build: glanceSRP},
	{ID: "kills", Label: "Kills, 7 days", Build: glanceKills},
	{ID: "nextfleet", Label: "Next fleet", Build: glanceNextFleet},
}

// glanceNeeds is the union of every tile's needs: the card reads the
// snapshots any pick might show.
func glanceNeeds() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range glanceTiles {
		for _, n := range t.Needs {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// glanceTile is one figure as a tile: the l-stat parts, how it moved,
// and the line or meter the medium and large forms draw under it.
type glanceTile struct {
	ID, Label   string
	Value, Unit string
	Hero        string // an ISK figure in full ("4,640,044,553.19"), drawn as the live tiles draw it
	Full        string // the whole figure, for the title
	Sub         string
	SubHTML     template.HTML // a sub line with ISK figures in it
	SubHref     string        // the sub line as a link, when it is the next step
	SubIdle     bool          // the sub line is the idle pilots' link the Fleet Overview answers
	Href        string
	Tone        string // l-stat tone: "", "warn", "hot" or "ok"
	Change      string // "+1.24B (+3.1%)"
	ChangeText  string
	Since       string // the span the change compares: "7 days"
	Up          bool
	UpIsGood    bool
	Line        *wChart // the figure's last 30 days, when it has a history
	Meter       *glanceMeter
	days        []wDay // the history the line is drawn from
}

// glanceMeter is a share a tile without a history shows as a bar.
type glanceMeter struct {
	Pct   float64 // 0 to 100
	Label string  // what the bar measures, for readers who cannot see it
	Tone  string  // "" or "warn"
}

// ChangeTone is the class the change takes: good or bad news.
func (t glanceTile) ChangeTone() string {
	return wLead{Change: t.Change, Up: t.Up, UpIsGood: t.UpIsGood}.Tone()
}

// glanceView is what the At a Glance forms draw.
type glanceView struct {
	Tiles []glanceTile
	First glanceTile // small: the first figure, with its last 30 days
}

// glanceBuild is what the tile builders share for one card.
type glanceBuild struct {
	app *Application
	in  *widgetInput
	nw  *netWorthWidget
}

// netWorth is the account's net worth, read from the render's cache.
func (g *glanceBuild) netWorth(ctx context.Context) *netWorthWidget {
	if g.nw == nil {
		g.nw = g.in.Shared.NetWorth(ctx)
	}
	return g.nw
}

func buildGlanceWidget(ctx context.Context, app *Application, in *widgetInput) any {
	g := &glanceBuild{app: app, in: in}
	v := &glanceView{}
	picks := strings.Split(in.Opts.Get("figures"), ",")
	lines := in.Draws("medium") || in.Draws("large")
	for _, id := range picks {
		for _, def := range glanceTiles {
			if def.ID != id || (def.Requires != "" && !moduleCompiledIn(def.Requires)) {
				continue
			}
			t := def.Build(ctx, g)
			t.ID, t.Label = def.ID, def.Label
			if t.Value == "" {
				t.Value, t.Full = "-", "Not known yet"
			}
			if lines && len(t.days) > 1 {
				t.Line, _ = wLineChart(t.Label+" over the last 30 days", t.Label, 1, t.days)
			}
			v.Tiles = append(v.Tiles, t)
		}
	}
	if len(v.Tiles) > 0 && in.Draws("small") {
		f := v.Tiles[0]
		if len(f.days) > 1 {
			f.Line, _ = wLineChart(f.Label+" over the last 30 days", f.Label, 1, f.days)
		}
		v.First = f
	}
	return v
}

// glanceMonth is the last 30 sampled days of one net worth figure.
func glanceMonth(nw *netWorthWidget, pick func(netWorthDay) float64) []wDay {
	month := nw.days
	if len(month) > 30 {
		month = month[len(month)-30:]
	}
	out := make([]wDay, len(month))
	for i, d := range month {
		out[i] = wDay{At: d.At, Values: []float64{pick(d)}}
	}
	return out
}

// glanceISK fills a tile's figure as ISK, shown short.
func glanceISK(t *glanceTile, v float64, full string) {
	t.Value, t.Unit, t.Full, t.Hero = iskCompact(v), "ISK", full+" ISK", full
}

// glanceBar is a share the net worth widget wrote as a data-w width
// ("94.4"), as a tile's bar.
func glanceBar(w, label string) *glanceMeter {
	pct, err := strconv.ParseFloat(w, 64)
	if err != nil || w == "" {
		return nil
	}
	return &glanceMeter{Pct: clampPct(pct), Label: label}
}

// glanceTrend copies a net worth trend onto a tile.
func glanceTrend(t *glanceTile, tr *kpiTrend) {
	if tr != nil {
		t.Change, t.ChangeText, t.Up, t.UpIsGood, t.Since = tr.Delta, tr.Text, tr.Up, true, tr.Since
	}
}

func glanceNetWorth(ctx context.Context, g *glanceBuild) glanceTile {
	nw := g.netWorth(ctx)
	t := glanceTile{Href: "/wallet/"}
	if !nw.Any {
		return t
	}
	glanceISK(&t, nw.totalNow, nw.Total)
	glanceTrend(&t, nw.TotalTrend)
	// What it is made of, as the live tile says it.
	var parts []string
	if nw.Wallet != "" {
		parts = append(parts, "Wallets "+string(iskFig(nw.Wallet)))
	}
	if nw.AssetsKnown {
		parts = append(parts, "Assets "+string(iskFig(nw.Assets)))
	}
	t.SubHTML = template.HTML(strings.Join(parts, " · "))
	t.days = glanceMonth(nw, netWorthDay.Total)
	return t
}

func glanceWallets(ctx context.Context, g *glanceBuild) glanceTile {
	nw := g.netWorth(ctx)
	t := glanceTile{Href: "/wallet/"}
	if nw.Wallet == "" {
		return t
	}
	glanceISK(&t, nw.walletNow, nw.Wallet)
	glanceTrend(&t, nw.WalletTrend)
	if nw.WalletPct != "" {
		t.Sub = nw.WalletPct + " of net worth"
	}
	t.Meter = glanceBar(nw.WalletBar, "Share of net worth in wallets")
	t.days = glanceMonth(nw, func(d netWorthDay) float64 { return d.ISK })
	return t
}

func glanceAssets(ctx context.Context, g *glanceBuild) glanceTile {
	nw := g.netWorth(ctx)
	t := glanceTile{Href: "/assets/"}
	if !nw.AssetsKnown {
		return t
	}
	// Assets alone: buy-order escrow is a figure of its own.
	glanceISK(&t, nw.assetsNow-nw.escrowNow, nw.Assets)
	glanceTrend(&t, nw.AssetsTrend)
	switch {
	case nw.AssetsNote != "":
		t.Sub = nw.AssetsNote
	case nw.AssetsPct != "":
		t.Sub = nw.AssetsPct + " of items priced"
	}
	t.Meter = glanceBar(nw.AssetsBar, "Share of items priced")
	t.days = glanceMonth(nw, func(d netWorthDay) float64 { return d.Assets })
	return t
}

func glanceEscrow(ctx context.Context, g *glanceBuild) glanceTile {
	nw := g.netWorth(ctx)
	t := glanceTile{Href: "/wallet/"}
	if nw.Escrow == "" {
		return t
	}
	glanceISK(&t, nw.escrowNow, nw.Escrow)
	buys := 0
	for _, b := range g.in.Bundles {
		for _, o := range b.orders {
			if o.IsBuyOrder {
				buys++
			}
		}
	}
	t.Sub = pluralCount(int64(buys), "buy order", "buy orders")
	if nw.totalNow > 0 {
		t.Meter = &glanceMeter{Pct: clampPct(100 * nw.escrowNow / nw.totalNow), Label: "Share of net worth held in escrow"}
	}
	return t
}

func glanceTraining(_ context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: "/characters/"}
	pilots, training := 0, 0
	for _, b := range g.in.Bundles {
		if !b.queueKnown {
			continue
		}
		pilots++
		if on, _ := queueState(b.queue, g.in.Now); on {
			training++
		}
	}
	if pilots == 0 {
		return t
	}
	idle := pilots - training
	t.Value, t.Unit = fmt.Sprintf("%d / %d", training, pilots), "training"
	t.Full = fmt.Sprintf("%d of %d pilots training", training, pilots)
	if idle > 0 {
		t.Sub, t.SubHref, t.SubIdle, t.Tone = fmt.Sprintf("%d idle · show them", idle), "/characters/", true, "warn"
	} else {
		t.Sub = "Every queue is running"
	}
	m := &glanceMeter{Pct: 100 * float64(training) / float64(pilots), Label: "Pilots training"}
	if idle > 0 {
		m.Tone = "warn"
	}
	t.Meter = m
	return t
}

// glanceProfitDays is the kept profit tile's window: the Profit
// page's default.
const glanceProfitDays = 30

func glanceProfit(ctx context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: "/profit/"}
	if len(g.in.Chars) == 0 {
		return t
	}
	ids := make([]int64, len(g.in.Chars))
	for i, c := range g.in.Chars {
		ids[i] = c.CharacterID
	}
	acts, err := g.app.queries.ListProfitByActivity(ctx, db.ListProfitByActivityParams{
		OwnerKind: ownerChar, OwnerIds: ids,
		FromDay: g.in.Now.UTC().AddDate(0, 0, -glanceProfitDays+1).Format("2006-01-02"),
	})
	if err != nil {
		logging.Errorf("glance: profit by activity for user %d: %v", g.in.UserID, err)
		return t
	}
	gross, kept, ok := profitLabeledTotals(acts)
	if !ok {
		return t
	}
	t.Value, t.Unit, t.Full = iskCompact(kept), "ISK", signedISK(kept)+" ISK kept over 30 days"
	if kept > 0 {
		t.Value = "+" + t.Value
		t.Tone = "ok"
	} else if kept < 0 {
		t.Tone = "hot"
	}
	t.Sub = "Gross in " + iskCompact(gross) + " ISK"
	return t
}

func glanceOrders(_ context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{}
	if moduleCompiledIn("orders") {
		t.Href = "/orders/"
	}
	known, sells, buys := false, 0, 0
	for _, b := range g.in.Bundles {
		if !b.ordersKnown {
			continue
		}
		known = true
		for _, o := range b.orders {
			if o.IsBuyOrder {
				buys++
			} else {
				sells++
			}
		}
	}
	if !known {
		return t
	}
	t.Value = fmt.Sprint(sells + buys)
	t.Full = pluralCount(int64(sells+buys), "open order", "open orders")
	t.Sub = fmt.Sprintf("%d sell, %d buy", sells, buys)
	return t
}

func glanceUndercut(ctx context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{}
	if moduleCompiledIn("orders") {
		t.Href = "/orders/"
	}
	health := g.in.Shared.market.orderHealth(ctx)
	if len(health) == 0 {
		return t
	}
	undercut := 0
	for _, h := range health {
		if h.Status == "undercut_station" || h.Status == "undercut_region" {
			undercut++
		}
	}
	t.Value = fmt.Sprint(undercut)
	t.Full = fmt.Sprintf("%d of %d orders checked are undercut", undercut, len(health))
	t.Sub = fmt.Sprintf("Of %s checked", pluralCount(int64(len(health)), "order", "orders"))
	m := &glanceMeter{Pct: 100 * float64(undercut) / float64(len(health)), Label: "Share of checked orders undercut"}
	if undercut > 0 {
		t.Tone, m.Tone = "warn", "warn"
	}
	t.Meter = m
	return t
}

func glanceJobs(_ context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: "/industry/jobs/"}
	known, ready, running := false, 0, 0
	for _, b := range g.in.Bundles {
		if !b.jobsKnown {
			continue
		}
		known = true
		for _, j := range b.jobs {
			switch j.Status {
			case "ready":
				ready++
			case "active", "paused":
				// A job whose end has passed is ready to deliver,
				// whatever the snapshot last said.
				if end, ok := parseRFC3339(j.EndDate); ok && !end.After(g.in.Now) {
					ready++
				} else {
					running++
				}
			}
		}
	}
	if !known {
		return t
	}
	t.Value = fmt.Sprint(ready)
	t.Full = pluralCount(int64(ready), "job ready to deliver", "jobs ready to deliver")
	t.Sub = fmt.Sprintf("%d running", running)
	if ready > 0 {
		t.Tone = "ok"
	}
	if all := ready + running; all > 0 {
		t.Meter = &glanceMeter{Pct: 100 * float64(ready) / float64(all), Label: "Share of jobs ready to deliver"}
	}
	return t
}

func glanceExtractors(_ context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: "/planets/"}
	known, stopped, all := false, 0, 0
	for _, b := range g.in.Bundles {
		if !b.planetsKnown {
			continue
		}
		known = true
		for _, colony := range b.colonies {
			layout := b.layouts[colony.PlanetID]
			if layout == nil {
				continue
			}
			for _, ex := range layoutExtractors(*layout) {
				if !ex.ExpiryOK {
					continue
				}
				all++
				if ex.Expiry.Before(g.in.Now) {
					stopped++
				}
			}
		}
	}
	if !known {
		return t
	}
	t.Value = fmt.Sprint(stopped)
	t.Full = fmt.Sprintf("%d of %d extractors stopped", stopped, all)
	switch {
	case all == 0:
		t.Sub = "No extractors read yet"
	case stopped == 0:
		t.Sub = "All " + pluralCount(int64(all), "extractor", "extractors") + " running"
	default:
		t.Sub = fmt.Sprintf("Of %d", all)
		t.Tone = "hot"
	}
	if all > 0 {
		m := &glanceMeter{Pct: 100 * float64(stopped) / float64(all), Label: "Share of extractors stopped"}
		if stopped > 0 {
			m.Tone = "warn"
		}
		t.Meter = m
	}
	return t
}

// clampPct keeps a share inside 0 to 100.
func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// glanceMail is the unread EVE mail across the card's pilots, from the
// same mail labels snapshot the Mail widget reads.
func glanceMail(_ context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: "/mail/"}
	known := false
	var unread int64
	pilots := 0
	for _, b := range g.in.Bundles {
		if b.mailLabels == nil {
			continue
		}
		known = true
		if b.mailLabels.TotalUnreadCount > 0 {
			unread += b.mailLabels.TotalUnreadCount
			pilots++
		}
	}
	if !known {
		return t
	}
	t.Value = fmt.Sprint(unread)
	t.Full = pluralCount(unread, "unread mail", "unread mails")
	if unread == 0 {
		t.Sub = "No unread mail"
		return t
	}
	t.Sub = pluralCount(int64(pilots), "pilot has unread mail", "pilots have unread mail")
	return t
}

// glanceSRP is what the account's open ship replacement claims would
// pay, as the Ship Replacement widget's lead counts it.
func glanceSRP(ctx context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: srpPath}
	open, err := g.app.queries.CountOpenSRPRequestsByUser(ctx, g.in.UserID)
	if err != nil {
		logging.Errorf("home: open ship replacement claims of user %d: %v", g.in.UserID, err)
		return t
	}
	glanceISK(&t, open.Suggested, esi.FormatISK(open.Suggested))
	if open.Requests == 0 {
		t.Sub = "No open claims"
		return t
	}
	t.Sub = pluralCount(open.Requests, "open claim", "open claims")
	return t
}

// glanceKills is the card's pilots' kills over the last 7 days, with the
// ISK efficiency the Kills and Losses widget shows: our own appraisal
// only.
func glanceKills(ctx context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: "/intel/kills/"}
	ids := make([]int64, 0, len(g.in.Chars))
	for _, c := range g.in.Chars {
		ids = append(ids, c.CharacterID)
	}
	if len(ids) == 0 {
		return t
	}
	k, _ := g.app.killsWindow(ctx, ids, g.in.Now.UTC(), 7)
	t.Value = fmt.Sprint(k.Kills)
	t.Full = fmt.Sprintf("%s and %s in the last 7 days", pluralCount(k.Kills, "kill", "kills"), pluralCount(k.Losses, "loss", "losses"))
	t.Sub = pluralCount(k.Losses, "loss", "losses")
	if k.Efficiency != "" {
		t.Sub += ", " + k.Efficiency + " ISK efficiency"
		tone := ""
		if k.Tone() == "bad" {
			tone = "warn"
		}
		t.Meter = &glanceMeter{Pct: clampPct(k.Pct), Label: "ISK efficiency over 7 days", Tone: tone}
	}
	if k.Unpriced > 0 {
		t.Sub += fmt.Sprintf(", %d not valued yet", k.Unpriced)
	}
	return t
}

// glanceNextFleet is the running fleet, else the next one, that the
// account can open on the Fleets page, from the render's shared fleets.
func glanceNextFleet(ctx context.Context, g *glanceBuild) glanceTile {
	t := glanceTile{Href: fleetsPath}
	for _, f := range g.in.Shared.Fleets(ctx) {
		if !f.Access.View {
			continue
		}
		row := g.app.homeFleetRowOf(ctx, f, g.in.Now)
		t.Sub, t.SubHref = row.Title, row.Href
		if row.Live {
			t.Value, t.Full = "Now", row.Title+": "+row.Running
			t.Tone = "ok"
		} else {
			t.Value, t.Full = strings.TrimPrefix(row.Left, "in "), row.Title+" starts "+row.Left
		}
		return t
	}
	t.Value, t.Full, t.Sub = "None", "No fleet running or coming up in the next 7 days", "None in the next 7 days"
	return t
}
