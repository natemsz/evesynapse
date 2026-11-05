package app

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The Admin page's ESI Details tab (/admin/esi/): how this
// instance's ESI traffic and the worker's backlogs stand right
// now, plus the per-family call counts over the usual ranges.
// Everything reads stored aggregates the worker's stats flush
// wrote (stats_recorder.go); nothing here calls ESI, restarts,
// or configures anything. The tab exists so the Analytics tab
// stays about the site's traffic and growth while this one is
// about the ESI pipe itself.
// ---------------------------------------------------------------------------

type analyticsBacklogTile struct {
	Name  string
	Depth string
	Delta string
	Spark template.HTML // prebuilt sparkline over the last cycles; geometry, not a style attribute
}

type esiDetailsView struct {
	Range string
	Word  string
	Links []analyticsRangeLink

	NowOK       string
	NowOKDelta  string
	NowErr      string
	NowErrDelta string
	NowChart    template.HTML
	NowNote     string

	Backlogs    []analyticsBacklogTile
	BacklogNote string

	ESIBars []analyticsBar
	ESINote string

	Cost string
}

// handleAdminESI renders the ESI Details tab: the right-now
// strip, the worker backlog tiles, and the per-family ESI
// section over the range the tab bar names (?range=).
func (app *Application) handleAdminESI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	data.ESIDetails = app.buildESIDetails(ctx, r.URL.Query().Get("range"))
	data.SectionTabs = adminTabs("esi")
	data.SectionSubTabs = adminRangeTabs("/admin/esi/", data.ESIDetails.Links)
	app.render(ctx, w, http.StatusOK, "admin_esi.html", data)
}

func (app *Application) buildESIDetails(ctx context.Context, rangeKey string) *esiDetailsView {
	b := app.newAnalyticsBuild(ctx, rangeKey, analyticsTotals{})
	v := &esiDetailsView{Range: b.rng.Key, Word: b.rng.Word}
	for _, r := range analyticsRanges {
		v.Links = append(v.Links, analyticsRangeLink{Key: r.Key, Label: r.Label, Active: r.Key == b.rng.Key})
	}
	b.nowStrip(v)
	b.backlogTiles(v)
	b.esiData = b.esiWindow()
	v.ESIBars, v.ESINote = b.esiSection()
	v.Cost = fmt.Sprintf("ESI details: %d queries, %d ms to build. Counts are this instance's own records.",
		b.queries, time.Since(b.started).Milliseconds())
	return v
}

// signedCount formats a window-over-window change the way the
// strip shows it: always signed, thousands separated.
func signedCount(n int64) string {
	if n < 0 {
		return "-" + esi.FormatInt(-n)
	}
	return "+" + esi.FormatInt(n)
}

// nowStrip reads the last ten minutes of stats_esi_minute: the
// successful and failed calls of the last 5 minutes against the
// 5 before, and the per-minute series of the last hour. The
// strip ignores the page range; it is always right now.
func (b *analyticsBuild) nowStrip(v *esiDetailsView) {
	to := b.to
	totals := func(from, until time.Time) (calls, errs int64) {
		b.queries++
		row, err := b.app.queries.StatsESITotalsWindow(b.ctx, db.StatsESITotalsWindowParams{FromTs: from, ToTs: until})
		if err != nil {
			logging.Errorf("admin esi: now totals: %v", err)
			return 0, 0
		}
		return row.Calls, row.Errors
	}
	recentCalls, recentErrs := totals(to.Add(-5*time.Minute), to)
	prevCalls, prevErrs := totals(to.Add(-10*time.Minute), to.Add(-5*time.Minute))
	recentOK, prevOK := recentCalls-recentErrs, prevCalls-prevErrs
	v.NowOK = esi.FormatInt(recentOK)
	v.NowOKDelta = signedCount(recentOK - prevOK)
	v.NowErr = esi.FormatInt(recentErrs)
	v.NowErrDelta = signedCount(recentErrs - prevErrs)

	from := to.Add(-60 * time.Minute)
	okSeries := make([]int64, 60)
	errSeries := make([]int64, 60)
	b.queries++
	rows, err := b.app.queries.StatsESISeries(b.ctx, db.StatsESISeriesParams{FromTs: from, ToTs: to})
	if err != nil {
		logging.Errorf("admin esi: now series: %v", err)
	}
	var any bool
	for _, r := range rows {
		idx := int(r.Minute.Sub(from).Minutes())
		if idx < 0 || idx >= 60 {
			continue
		}
		okSeries[idx] += r.Calls - r.Errors
		errSeries[idx] += r.Errors
		any = true
	}
	if any {
		v.NowChart = stackedBarsSVG([][]int64{okSeries, errSeries}, []string{"an-s2", "an-s5"}, 1180, 150)
	} else {
		v.NowNote = "No ESI calls recorded in the last hour yet."
	}
}

// backlogKinds fixes the tiles' order and their plain-English
// names; kinds the flush has never recorded are skipped.
var backlogKinds = []struct {
	kind string
	name string
}{
	{statsBacklogKillsToMerge, "Killmails waiting to merge"},
	{statsBacklogHistoryDue, "Market history pairs due"},
	{statsBacklogRecords, "Pilot and corporation records to resolve"},
}

// backlogSparkCycles is how many of the last cycle readings
// each tile's sparkline draws.
const backlogSparkCycles = 30

func (b *analyticsBuild) backlogTiles(v *esiDetailsView) {
	b.queries++
	rows, err := b.app.queries.StatsBacklogsRecent(b.ctx, backlogSparkCycles)
	if err != nil {
		logging.Errorf("admin esi: backlogs: %v", err)
	}
	byKind := map[string][]db.StatsBacklogsRecentRow{}
	for _, r := range rows {
		byKind[r.Backlog] = append(byKind[r.Backlog], r)
	}
	for _, k := range backlogKinds {
		rs := byKind[k.kind]
		if len(rs) == 0 {
			continue
		}
		latest := rs[len(rs)-1]
		tile := analyticsBacklogTile{Name: k.name, Depth: esi.FormatInt(latest.Depth)}
		switch {
		case len(rs) < 2:
			tile.Delta = "first reading"
		case latest.Depth == rs[len(rs)-2].Depth:
			tile.Delta = "unchanged since the previous cycle"
		default:
			tile.Delta = signedCount(latest.Depth-rs[len(rs)-2].Depth) + " since the previous cycle"
		}
		values := make([]float64, len(rs))
		var max float64
		for i, r := range rs {
			values[i] = float64(r.Depth)
			if values[i] > max {
				max = values[i]
			}
		}
		if max > 0 {
			tile.Spark = areaChartSVG(values, max, "an-line-a", 260, 56, nil, "", "", "")
		}
		v.Backlogs = append(v.Backlogs, tile)
	}
	if len(v.Backlogs) == 0 {
		v.BacklogNote = "No backlog readings yet. The worker records the depths at the end of every cycle."
	}
}
