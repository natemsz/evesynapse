package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"runtime"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/swarm"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The Admin page's Analytics section: first-party numbers only,
// read from the recorder's stored aggregates (stats_recorder.go)
// and from the tables that own each fact. The page is read-only:
// nothing here restarts, deploys, configures, or touches the
// host. Every read is bounded, and the section prints its own
// cost in its footer (queries run, milliseconds taken).
// ---------------------------------------------------------------------------

type analyticsRangeDef struct {
	Key    string
	Label  string
	Word   string // as in headings: "24h", "7 days", "30 days"
	Window time.Duration
	Step   int64 // traffic/host bucket seconds
	Hourly bool  // read the hourly rollups instead of per-minute rows
}

var analyticsRanges = []analyticsRangeDef{
	{Key: "24h", Label: "Last 24 hours", Word: "24h", Window: 24 * time.Hour, Step: 3600},
	{Key: "7d", Label: "7 days", Word: "7 days", Window: 7 * 24 * time.Hour, Step: 6 * 3600},
	{Key: "30d", Label: "30 days", Word: "30 days", Window: 30 * 24 * time.Hour, Step: 86400, Hourly: true},
}

func analyticsRangeFor(key string) analyticsRangeDef {
	for _, r := range analyticsRanges {
		if r.Key == key {
			return r
		}
	}
	return analyticsRanges[0]
}

type analyticsRangeLink struct {
	Key, Label string
	Active     bool
}

type analyticsKPI struct {
	Label string
	Value string
	Sub   template.HTML
}

type analyticsLegendItem struct {
	Class string // CSS swatch class
	Label string
}

type analyticsBar struct {
	Name  string
	Mono  bool
	Pct   float64 // bar width, 0..100
	Value string
	Bar   template.HTML // prebuilt SVG meter; geometry, not a style attribute
}

type analyticsPhaseSeg struct {
	Name  string
	Class string
	Pct   float64
	Label string // "Public data 38s"
}

type analyticsFresh struct {
	Name  string
	Age   string
	Class string // an-ok, an-warn-t, an-bad-t
}

type analyticsView struct {
	Range     string
	Word      string
	Links     []analyticsRangeLink
	EmptyNote string // set when the recorder has nothing yet

	KPIs []analyticsKPI

	Traffic     template.HTML
	Legend      []analyticsLegendItem
	Mix         string
	TrafficNote string

	CycleChart   template.HTML
	PhaseSegs    []analyticsPhaseSeg
	PhaseBar     template.HTML // prebuilt SVG phase split; geometry, not a style attribute
	CycleSummary string
	CycleNote    string

	HostLoadChart template.HTML
	HostMemChart  template.HTML
	HostLoadNote  string
	HostMemNote   string
	HostFacts     []string
	HostNote      string // set when no host samples exist

	ESIBars []analyticsBar
	ESINote string

	GrowthChart template.HTML
	GrowthNote  string

	TopBars []analyticsBar
	TopNote string

	ActivityBars []analyticsBar

	Fresh      []analyticsFresh
	FreshNote  string
	SwarmPeers []analyticsBar
	SwarmNote  string

	InvBars []analyticsBar
	InvNote string

	Cost string
}

// analyticsTotals carries the account figures handleAdmin
// already loaded, so the section does not query them twice.
type analyticsTotals struct {
	Accounts, Characters, Parked, SeenToday int64
	OK                                      bool
}

// analyticsBuild is one page build: it counts its own queries
// so the footer can print the section's cost.
type analyticsBuild struct {
	app     *Application
	ctx     context.Context
	queries int
	started time.Time
	rng     analyticsRangeDef
	from    time.Time
	to      time.Time
	now     time.Time
	totals  analyticsTotals

	esiData    esiWindowData
	swarmBytes int64
	invCache   []db.StatsInventoryLatestRow
	invLoaded  bool
}

func (app *Application) buildAnalytics(ctx context.Context, rangeKey string, totals analyticsTotals) *analyticsView {
	b := &analyticsBuild{app: app, ctx: ctx, started: time.Now(), totals: totals}
	b.rng = analyticsRangeFor(rangeKey)
	b.now = time.Now().UTC()
	b.to = b.now.Truncate(time.Minute).Add(time.Minute)
	// Align the window start to the bucket step: the SQL buckets
	// by epoch multiples of the step, and the chart indexing
	// below assumes the first bucket starts at from.
	fromUnix := b.to.Add(-b.rng.Window).Unix()
	b.from = time.Unix(fromUnix-fromUnix%b.rng.Step, 0).UTC()

	v := &analyticsView{Range: b.rng.Key, Word: b.rng.Word}
	for _, r := range analyticsRanges {
		v.Links = append(v.Links, analyticsRangeLink{Key: r.Key, Label: r.Label, Active: r.Key == b.rng.Key})
	}

	traffic := b.traffic()
	b.esiData = b.esiWindow()
	sum := b.cycleSummary()
	if traffic.total == 0 && sum.count == 0 {
		v.EmptyNote = "No analytics yet. The recorder writes its first rows within a minute of the worker running, and this section fills in from there."
	}
	b.kpis(v, traffic, sum)
	b.cycleSection(v, sum)
	b.host(v)
	b.esiSection(v)
	b.activitySection(v)
	b.growth(v)
	b.topPages(v)
	b.freshness(v)
	b.swarmPanel(v)
	b.inventory(v)
	v.finishBars()

	v.Cost = fmt.Sprintf("Analytics section: %d queries, %d ms to build. Counts are this instance's own records.",
		b.queries, time.Since(b.started).Milliseconds())
	return v
}

// finishBars prebuilds the SVG meter for every bar row and the
// phase split for the cycle section: bar widths are SVG geometry,
// never an inline style (the CSP allows none).
func (v *analyticsView) finishBars() {
	for _, bars := range [][]analyticsBar{v.ESIBars, v.TopBars, v.ActivityBars, v.SwarmPeers, v.InvBars} {
		for i := range bars {
			bars[i].Bar = barSVG(bars[i].Pct)
		}
	}
	v.PhaseBar = phaseBarSVG(v.PhaseSegs)
}

func pctText(part, total int64) string {
	if total == 0 {
		return "0%"
	}
	p := float64(part) * 100 / float64(total)
	s := strconv.FormatFloat(p, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + "%"
}

func barPct(part, max int64) float64 {
	if max <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(max)
}

func msText(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", int64(ms+0.5))
	}
	return fmt.Sprintf("%.1f s", ms/1000)
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// --- Traffic ---------------------------------------------------------------

type trafficData struct {
	total   int64
	byClass map[int16]int64
	series  [][]int64 // [class index][bucket]
	buckets int
}

var trafficClasses = []int16{2, 3, 4, 5}
var trafficClassCSS = []string{"an-s2", "an-s3", "an-s4", "an-s5"}
var trafficClassLabel = []string{"2xx served", "3xx redirect", "4xx client error", "5xx server error"}

func (b *analyticsBuild) traffic() trafficData {
	out := trafficData{byClass: map[int16]int64{}}
	type row struct {
		Bucket      time.Time
		StatusClass int16
		Requests    int64
	}
	var rows []row
	b.queries++
	if b.rng.Hourly {
		rs, err := b.app.queries.StatsTrafficHours(b.ctx, db.StatsTrafficHoursParams{FromTs: b.from, ToTs: b.to, StepSeconds: float64(b.rng.Step)})
		if err == nil {
			for _, r := range rs {
				rows = append(rows, row{r.Bucket, r.StatusClass, r.Requests})
			}
		} else {
			logging.Errorf("admin analytics: traffic: %v", err)
		}
	} else {
		rs, err := b.app.queries.StatsTrafficMinutes(b.ctx, db.StatsTrafficMinutesParams{FromTs: b.from, ToTs: b.to, StepSeconds: float64(b.rng.Step)})
		if err == nil {
			for _, r := range rs {
				rows = append(rows, row{r.Bucket, r.StatusClass, r.Requests})
			}
		} else {
			logging.Errorf("admin analytics: traffic: %v", err)
		}
	}
	out.buckets = int(b.to.Sub(b.from).Seconds()) / int(b.rng.Step)
	out.series = make([][]int64, len(trafficClasses))
	for i := range out.series {
		out.series[i] = make([]int64, out.buckets)
	}
	classIdx := map[int16]int{2: 0, 3: 1, 4: 2, 5: 3}
	for _, r := range rows {
		out.total += r.Requests
		out.byClass[r.StatusClass] += r.Requests
		ci, ok := classIdx[r.StatusClass]
		if !ok {
			continue
		}
		idx := int(r.Bucket.Sub(b.from).Seconds()) / int(b.rng.Step)
		if idx < 0 || idx >= out.buckets {
			continue
		}
		out.series[ci][idx] += r.Requests
	}
	return out
}

// durationsAll reads the whole-site response histogram for the
// window, in bucket-index order.
func (b *analyticsBuild) durationsAll() []int64 {
	hits := make([]int64, len(statsDurationEdgesMs)+1)
	b.queries++
	add := func(bucket int16, n int64) {
		if bucket >= 0 && int(bucket) < len(hits) {
			hits[bucket] += n
		}
	}
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsDurationsAllHours(b.ctx, db.StatsDurationsAllHoursParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				add(r.Bucket, r.Hits)
			}
		} else {
			logging.Errorf("admin analytics: durations: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsDurationsAllMinutes(b.ctx, db.StatsDurationsAllMinutesParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				add(r.Bucket, r.Hits)
			}
		} else {
			logging.Errorf("admin analytics: durations: %v", err)
		}
	}
	return hits
}

// --- Cycles ------------------------------------------------------------------

type cycleSummaryData struct {
	count   int64
	avgMs   float64
	p95Ms   float64
	hasP95  bool
	fetches int64
}

func (b *analyticsBuild) cycleSummary() cycleSummaryData {
	b.queries++
	if b.rng.Hourly {
		row, err := b.app.queries.StatsCycleSummaryHours(b.ctx, db.StatsCycleSummaryHoursParams{FromTs: b.from, ToTs: b.to})
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				logging.Errorf("admin analytics: cycle summary: %v", err)
			}
			return cycleSummaryData{}
		}
		return cycleSummaryData{count: row.Cycles, avgMs: floatOf(row.AvgMs), fetches: row.Fetches}
	}
	row, err := b.app.queries.StatsCycleSummary(b.ctx, db.StatsCycleSummaryParams{FromTs: b.from, ToTs: b.to})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("admin analytics: cycle summary: %v", err)
		}
		return cycleSummaryData{}
	}
	out := cycleSummaryData{count: row.Cycles, avgMs: row.AvgMs, fetches: row.Fetches}
	if f := floatOf(row.P95Ms); f > 0 {
		out.p95Ms, out.hasP95 = f, f > 0
	}
	return out
}

func (b *analyticsBuild) cycleSection(v *analyticsView, sum cycleSummaryData) {
	if sum.count == 0 {
		v.CycleNote = "No worker cycles recorded in this window yet."
		return
	}
	step := int64(600)
	if b.rng.Key == "7d" {
		step = 7200
	}
	var values []float64
	b.queries++
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsCycleSeriesHours(b.ctx, db.StatsCycleSeriesHoursParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				values = append(values, r.AvgMs/1000)
			}
		} else {
			logging.Errorf("admin analytics: cycle series: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsCycleSeries(b.ctx, db.StatsCycleSeriesParams{FromTs: b.from, ToTs: b.to, StepSeconds: float64(step)}); err == nil {
			for _, r := range rs {
				values = append(values, r.AvgMs/1000)
			}
		} else {
			logging.Errorf("admin analytics: cycle series: %v", err)
		}
	}
	if len(values) > 0 {
		max := 72.0 // seconds; leaves headroom over the cadence line
		for _, val := range values {
			if val*1.1 > max {
				max = val * 1.1
			}
		}
		ref := 60.0
		v.CycleChart = areaChartSVG(values, max, "an-line-a", 1180, 200, &ref, "1m cadence",
			fmt.Sprintf("%ds", int(max+0.5)), "cycle average · "+b.rng.Word)
	}
	b.phases(v)
	summary := "avg " + shortDuration(time.Duration(sum.avgMs)*time.Millisecond)
	if sum.hasP95 {
		summary += " · p95 " + shortDuration(time.Duration(sum.p95Ms)*time.Millisecond)
	}
	v.CycleSummary = summary
}

var phaseOrder = []struct {
	name  string
	class string
}{
	{"Public data", "an-ph-public"},
	{"Pilot warming", "an-ph-warming"},
	{"True values", "an-ph-truevalues"},
	{"Intel", "an-ph-intel"},
	{"Swarm", "an-ph-swarm"},
	{"Other", "an-ph-other"},
}

// analyticsPhaseGroup folds the worker clock's raw phase names
// into the six display groups (see the mapping note in the
// report): known core phases by exact name, the intel and swarm
// module phases by substring, everything else Other.
func analyticsPhaseGroup(phase string) string {
	switch {
	case phase == "true values":
		return "True values"
	case strings.Contains(phase, "swarm"):
		return "Swarm"
	case strings.Contains(phase, "intel"):
		return "Intel"
	}
	switch phase {
	case "characters", "names", "foresight":
		return "Pilot warming"
	case "market data", "region sweep", "public contracts", "contract appraisal", "public records", "guide and indices":
		return "Public data"
	}
	return "Other"
}

func (b *analyticsBuild) phases(v *analyticsView) {
	type prow struct {
		Phase     string
		TookSumMs int64
		Samples   int64
	}
	var rows []prow
	b.queries++
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsCyclePhasesHours(b.ctx, db.StatsCyclePhasesHoursParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				rows = append(rows, prow{r.Phase, r.TookSumMs, r.Samples})
			}
		} else {
			logging.Errorf("admin analytics: phases: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsCyclePhasesWindow(b.ctx, db.StatsCyclePhasesWindowParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				rows = append(rows, prow{r.Phase, r.TookSumMs, r.Samples})
			}
		} else {
			logging.Errorf("admin analytics: phases: %v", err)
		}
	}
	avg := map[string]float64{}
	var totalAvg float64
	for _, r := range rows {
		if r.Samples == 0 {
			continue
		}
		a := float64(r.TookSumMs) / float64(r.Samples)
		avg[analyticsPhaseGroup(r.Phase)] += a
		totalAvg += a
	}
	if totalAvg <= 0 {
		return
	}
	for _, g := range phaseOrder {
		a := avg[g.name]
		if a <= 0 {
			continue
		}
		v.PhaseSegs = append(v.PhaseSegs, analyticsPhaseSeg{
			Name: g.name, Class: g.class, Pct: a / totalAvg * 100,
			Label: fmt.Sprintf("%s %s", g.name, shortDuration(time.Duration(a)*time.Millisecond)),
		})
	}
}

// --- KPIs --------------------------------------------------------------------

func (b *analyticsBuild) kpis(v *analyticsView, traffic trafficData, sum cycleSummaryData) {
	if traffic.total > 0 {
		v.Traffic = stackedBarsSVG(traffic.series, trafficClassCSS, 1180, 240)
		for i := range trafficClasses {
			v.Legend = append(v.Legend, analyticsLegendItem{Class: trafficClassCSS[i], Label: trafficClassLabel[i]})
		}
		parts := make([]string, 0, len(trafficClasses))
		for _, class := range trafficClasses {
			parts = append(parts, fmt.Sprintf("%dxx %s", class, pctText(traffic.byClass[class], traffic.total)))
		}
		v.Mix = "Status mix · " + strings.Join(parts, " · ")
	} else {
		v.TrafficNote = "No requests recorded in this window yet."
	}

	hits := b.durationsAll()
	median, hasMedian := statsPercentileMs(hits, 50)
	p95, _ := statsPercentileMs(hits, 95)

	rate := float64(traffic.total) / b.rng.Window.Hours()
	rateText := fmt.Sprintf("about %s an hour", esi.FormatInt(int64(rate+0.5)))
	if b.rng.Window >= 7*24*time.Hour {
		rateText = fmt.Sprintf("about %s a day", esi.FormatInt(int64(rate*24+0.5)))
	}
	v.KPIs = append(v.KPIs, analyticsKPI{
		Label: "Requests · " + b.rng.Word,
		Value: esi.FormatInt(traffic.total),
		Sub:   template.HTML(template.HTMLEscapeString(rateText)),
	})

	medianText, medianSub := "—", "no requests timed yet"
	if hasMedian {
		medianText = msText(median)
		medianSub = "p95 " + msText(p95)
	}
	v.KPIs = append(v.KPIs, analyticsKPI{Label: "Median page response", Value: medianText, Sub: template.HTML(template.HTMLEscapeString(medianSub))})

	cycleText, cycleSub := "—", "no cycles recorded yet"
	if sum.count > 0 {
		cycleText = shortDuration(time.Duration(sum.avgMs) * time.Millisecond)
		if sum.avgMs > 60000 {
			over := time.Duration(sum.avgMs-60000) * time.Millisecond
			cycleSub = fmt.Sprintf(`<span class="warn">%s over the 1m cadence</span>`, shortDuration(over))
		} else {
			cycleSub = "inside the 1m cadence"
		}
	}
	v.KPIs = append(v.KPIs, analyticsKPI{Label: "Worker cycle · avg", Value: cycleText, Sub: template.HTML(cycleSub)})

	budget := "error budget healthy"
	if b.app.esi.HardLimitActive(b.now) {
		budget = `<span class="warn">error budget limited</span>`
	} else if b.app.esi.ErrorBudgetLow() {
		budget = `<span class="warn">error budget low</span>`
	}
	v.KPIs = append(v.KPIs, analyticsKPI{Label: "ESI errors · " + b.rng.Word, Value: esi.FormatInt(b.esiData.totalErrs), Sub: template.HTML(budget)})

	sessions := int64(0)
	b.queries++
	if n, err := b.app.queries.StatsActiveSessions(b.ctx); err == nil {
		sessions = n
	} else {
		logging.Errorf("admin analytics: sessions: %v", err)
	}
	seenText := ""
	if b.totals.OK {
		seenText = fmt.Sprintf("%s accounts seen today", esi.FormatInt(b.totals.SeenToday))
	}
	v.KPIs = append(v.KPIs, analyticsKPI{Label: "Active sessions", Value: esi.FormatInt(sessions), Sub: template.HTML(template.HTMLEscapeString(seenText))})

	dbText, dbSub := "—", "no snapshot yet"
	if size, ok := b.inventoryDB(); ok {
		dbText = humanBytes(size)
		dbSub = "from the hourly snapshot"
	}
	v.KPIs = append(v.KPIs, analyticsKPI{Label: "Database", Value: dbText, Sub: template.HTML(template.HTMLEscapeString(dbSub))})
}

// --- Host ----------------------------------------------------------------------

func uptimeText(since time.Time) string {
	if since.IsZero() {
		// No recorded start (a partially built Application):
		// time.Since the zero time saturates at the maximum
		// duration, which would print as a 292-year uptime.
		return "unknown"
	}
	d := time.Since(since)
	if days := int(d.Hours()) / 24; days > 0 {
		return fmt.Sprintf("%dd %dh", days, int(d.Hours())%24)
	}
	return shortDuration(d)
}

func (b *analyticsBuild) host(v *analyticsView) {
	b.queries++
	latest, err := b.app.queries.StatsHostLatest(b.ctx)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("admin analytics: host latest: %v", err)
		}
		v.HostNote = "No host samples yet. The worker samples this host once a minute; where /proc is unavailable this panel stays empty."
		return
	}
	type hpt struct {
		load float64
		mem  float64
		mtot int64
	}
	var pts []hpt
	b.queries++
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsHostSeriesHours(b.ctx, db.StatsHostSeriesHoursParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				pts = append(pts, hpt{r.Load1, r.MemUsedBytes, r.MemTotalBytes})
			}
		} else {
			logging.Errorf("admin analytics: host series: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsHostSeries(b.ctx, db.StatsHostSeriesParams{FromTs: b.from, ToTs: b.to, StepSeconds: float64(b.rng.Step)}); err == nil {
			for _, r := range rs {
				pts = append(pts, hpt{r.Load1, r.MemUsedBytes, r.MemTotalBytes})
			}
		} else {
			logging.Errorf("admin analytics: host series: %v", err)
		}
	}
	if len(pts) > 0 {
		loads := make([]float64, len(pts))
		mems := make([]float64, len(pts))
		peak, memMax := 0.0, 0.0
		for i, p := range pts {
			loads[i] = p.load
			if p.load > peak {
				peak = p.load
			}
			mems[i] = p.mem / 1e9
			if float64(p.mtot)/1e9 > memMax {
				memMax = float64(p.mtot) / 1e9
			}
		}
		capacity := float64(runtime.NumCPU())
		loadMax := capacity
		if peak*1.1 > loadMax {
			loadMax = peak * 1.1
		}
		v.HostLoadChart = areaChartSVG(loads, loadMax, "an-line-a", 560, 130, &capacity,
			fmt.Sprintf("capacity %d OCPU", runtime.NumCPU()), fmt.Sprintf("%.1f", loadMax), "")
		if memMax > 0 {
			v.HostMemChart = areaChartSVG(mems, memMax, "an-line-b", 560, 130, nil, "", fmt.Sprintf("%.0f GB", memMax), "")
		}
		v.HostLoadNote = fmt.Sprintf("Load average · 1m. Now %.2f · peak %.2f in the window.", latest.Load1, peak)
		v.HostMemNote = fmt.Sprintf("Memory used · of %s. Now %s · app process %s.",
			humanBytes(latest.MemTotalBytes), humanBytes(latest.MemUsedBytes), humanBytes(latest.AppRssBytes))
	}
	v.HostFacts = append(v.HostFacts, fmt.Sprintf("Goroutines %s", esi.FormatInt(latest.Goroutines)))
	if latest.DiskTotalBytes > 0 {
		v.HostFacts = append(v.HostFacts, fmt.Sprintf("Disk %s of %s", humanBytes(latest.DiskUsedBytes), humanBytes(latest.DiskTotalBytes)))
	}
	v.HostFacts = append(v.HostFacts,
		"Uptime "+uptimeText(b.app.startedAt),
		"Sampled once a minute by the worker, read from /proc")
}

// --- ESI + Activity --------------------------------------------------------------

type esiWindowData struct {
	bars       []analyticsBar
	totalCalls int64
	totalErrs  int64
	families   int
}

func (b *analyticsBuild) esiWindow() esiWindowData {
	out := esiWindowData{}
	type erow struct {
		Family string
		Calls  int64
		Errors int64
	}
	var rows []erow
	b.queries++
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsESIWindowHours(b.ctx, db.StatsESIWindowHoursParams{FromTs: b.from, ToTs: b.to, Lim: 12}); err == nil {
			for _, r := range rs {
				rows = append(rows, erow{r.Family, r.Calls, r.Errors})
			}
		} else {
			logging.Errorf("admin analytics: esi window: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsESIWindowMinutes(b.ctx, db.StatsESIWindowMinutesParams{FromTs: b.from, ToTs: b.to, Lim: 12}); err == nil {
			for _, r := range rs {
				rows = append(rows, erow{r.Family, r.Calls, r.Errors})
			}
		} else {
			logging.Errorf("admin analytics: esi window: %v", err)
		}
	}
	var max int64
	for _, r := range rows {
		out.totalCalls += r.Calls
		out.totalErrs += r.Errors
		if r.Calls > max {
			max = r.Calls
		}
	}
	out.families = len(rows)
	for _, r := range rows {
		val := esi.FormatInt(r.Calls)
		if r.Errors > 0 {
			val += " · " + esi.FormatInt(r.Errors) + " err"
		}
		name := r.Family
		if name != "" {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		out.bars = append(out.bars, analyticsBar{Name: name, Pct: barPct(r.Calls, max), Value: val})
	}
	return out
}

func (b *analyticsBuild) esiSection(v *analyticsView) {
	v.ESIBars = b.esiData.bars
	if b.esiData.totalCalls > 0 {
		v.ESINote = fmt.Sprintf("%s calls in total across %d families.", esi.FormatInt(b.esiData.totalCalls), b.esiData.families)
	} else {
		v.ESINote = "No ESI calls recorded in this window yet."
	}
}

// eventsWindow reads the recorder event kinds for the window.
func (b *analyticsBuild) eventsWindow() map[string]int64 {
	out := map[string]int64{}
	b.queries++
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsEventsWindowHours(b.ctx, db.StatsEventsWindowHoursParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				out[r.Kind] = r.Count
			}
		} else {
			logging.Errorf("admin analytics: events: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsEventsWindowMinutes(b.ctx, db.StatsEventsWindowMinutesParams{FromTs: b.from, ToTs: b.to}); err == nil {
			for _, r := range rs {
				out[r.Kind] = r.Count
			}
		} else {
			logging.Errorf("admin analytics: events: %v", err)
		}
	}
	return out
}

func (b *analyticsBuild) activitySection(v *analyticsView) {
	events := b.eventsWindow()
	b.swarmBytes = events["swarm_bytes"]

	b.queries++
	intelN, err := b.app.queries.CountIntelReportsWindow(b.ctx, db.CountIntelReportsWindowParams{FromTs: b.from, ToTs: b.to})
	if err != nil {
		logging.Errorf("admin analytics: intel count: %v", err)
	}
	b.queries++
	appraisalN, err := b.app.queries.CountAppraisalsWindow(b.ctx, db.CountAppraisalsWindowParams{FromTs: b.from, ToTs: b.to})
	if err != nil {
		logging.Errorf("admin analytics: appraisal count: %v", err)
	}
	b.queries++
	fitsN, err := b.app.queries.CountFitsSavedWindow(b.ctx, db.CountFitsSavedWindowParams{FromTs: b.from, ToTs: b.to})
	if err != nil {
		logging.Errorf("admin analytics: fits count: %v", err)
	}
	b.queries++
	linkedN, err := b.app.queries.CountCharactersLinkedWindow(b.ctx, db.CountCharactersLinkedWindowParams{FromTs: b.from, ToTs: b.to})
	if err != nil {
		logging.Errorf("admin analytics: linked count: %v", err)
	}

	acts := []struct {
		name string
		n    int64
	}{
		{"Contract searches", events[statsEventContractSearch]},
		{"Intel reports filed", intelN},
		{"Appraisals run", appraisalN},
		{"Sign-ins", events[statsEventSignIn]},
		{"Fits saved", fitsN},
		{"Characters linked", linkedN},
	}
	var max int64
	for _, a := range acts {
		if a.n > max {
			max = a.n
		}
	}
	for _, a := range acts {
		v.ActivityBars = append(v.ActivityBars, analyticsBar{Name: a.name, Pct: barPct(a.n, max), Value: esi.FormatInt(a.n)})
	}
}

// --- Growth ------------------------------------------------------------------------

func (b *analyticsBuild) growth(v *analyticsView) {
	from := b.now.Add(-14 * 24 * time.Hour).Truncate(24 * time.Hour)
	b.queries++
	rows, err := b.app.queries.StatsSignupsByDay(b.ctx, from)
	if err != nil {
		logging.Errorf("admin analytics: signups: %v", err)
	}
	byDay := map[string]int64{}
	for _, r := range rows {
		byDay[r.Day.Format("2006-01-02")] = r.Signups
	}
	values := make([]int64, 14)
	for i := 0; i < 14; i++ {
		values[i] = byDay[from.Add(time.Duration(i)*24*time.Hour).Format("2006-01-02")]
	}
	v.GrowthChart = barsSVG(values, "an-s2", 520, 120)
	note := "New accounts per day, last 14 days."
	if b.totals.OK {
		note += fmt.Sprintf(" %s accounts, %s linked characters in total.", esi.FormatInt(b.totals.Accounts), esi.FormatInt(b.totals.Characters))
	}
	v.GrowthNote = note
}

// --- Top pages ------------------------------------------------------------------------

func (b *analyticsBuild) topPages(v *analyticsView) {
	type grow struct {
		RouteGroup string
		Requests   int64
	}
	var groups []grow
	b.queries++
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsTopGroupsHours(b.ctx, db.StatsTopGroupsHoursParams{FromTs: b.from, ToTs: b.to, Lim: 10}); err == nil {
			for _, r := range rs {
				groups = append(groups, grow{r.RouteGroup, r.Requests})
			}
		} else {
			logging.Errorf("admin analytics: top groups: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsTopGroupsMinutes(b.ctx, db.StatsTopGroupsMinutesParams{FromTs: b.from, ToTs: b.to, Lim: 10}); err == nil {
			for _, r := range rs {
				groups = append(groups, grow{r.RouteGroup, r.Requests})
			}
		} else {
			logging.Errorf("admin analytics: top groups: %v", err)
		}
	}
	if len(groups) == 0 {
		v.TopNote = "No page traffic recorded in this window yet."
		return
	}
	groupNames := make([]string, 0, len(groups))
	for _, g := range groups {
		groupNames = append(groupNames, g.RouteGroup)
	}
	acc := map[string][]int64{}
	b.queries++
	collect := func(group string, bucket int16, hits int64) {
		h := acc[group]
		if h == nil {
			h = make([]int64, len(statsDurationEdgesMs)+1)
			acc[group] = h
		}
		if bucket >= 0 && int(bucket) < len(h) {
			h[bucket] += hits
		}
	}
	if b.rng.Hourly {
		if rs, err := b.app.queries.StatsDurationsForGroupsHours(b.ctx, db.StatsDurationsForGroupsHoursParams{FromTs: b.from, ToTs: b.to, Groups: groupNames}); err == nil {
			for _, r := range rs {
				collect(r.RouteGroup, r.Bucket, r.Hits)
			}
		} else {
			logging.Errorf("admin analytics: group durations: %v", err)
		}
	} else {
		if rs, err := b.app.queries.StatsDurationsForGroupsMinutes(b.ctx, db.StatsDurationsForGroupsMinutesParams{FromTs: b.from, ToTs: b.to, Groups: groupNames}); err == nil {
			for _, r := range rs {
				collect(r.RouteGroup, r.Bucket, r.Hits)
			}
		} else {
			logging.Errorf("admin analytics: group durations: %v", err)
		}
	}
	var max int64
	for _, g := range groups {
		if g.Requests > max {
			max = g.Requests
		}
	}
	for _, g := range groups {
		val := esi.FormatInt(g.Requests)
		if m, ok := statsPercentileMs(acc[g.RouteGroup], 50); ok {
			val += " · " + msText(m)
		}
		v.TopBars = append(v.TopBars, analyticsBar{Name: g.RouteGroup, Mono: true, Pct: barPct(g.Requests, max), Value: val})
	}
	v.TopNote = "Route template, requests, median response."
}

// --- Freshness ---------------------------------------------------------------------------

func (b *analyticsBuild) freshness(v *analyticsView) {
	now := b.now
	var stalest time.Duration
	haveStalest := false
	add := func(name string, at time.Time, ok bool, okUnder, warnUnder time.Duration) {
		if !ok {
			v.Fresh = append(v.Fresh, analyticsFresh{Name: name, Age: "no data", Class: "an-bad-t"})
			return
		}
		age := now.Sub(at)
		if age < 0 {
			age = 0
		}
		class := "an-ok"
		if age > warnUnder {
			class = "an-bad-t"
		} else if age > okUnder {
			class = "an-warn-t"
		}
		v.Fresh = append(v.Fresh, analyticsFresh{Name: name, Age: shortDuration(age), Class: class})
		if !haveStalest || age > stalest {
			stalest, haveStalest = age, true
		}
	}

	// Market orders, per hub region.
	kinds := make([]string, 0, len(marketRegions))
	regionIDs := make([]int64, 0, len(marketRegions))
	for _, r := range marketRegions {
		kinds = append(kinds, regionSweepKind(r.ID))
		regionIDs = append(regionIDs, r.ID)
	}
	b.queries++
	swept := map[string]time.Time{}
	if rs, err := b.app.queries.StatsFetchStatesByKinds(b.ctx, kinds); err == nil {
		for _, r := range rs {
			swept[r.Kind] = r.AttemptedAt
		}
	} else {
		logging.Errorf("admin analytics: fetch states: %v", err)
	}
	for _, r := range marketRegions {
		at, ok := swept[regionSweepKind(r.ID)]
		add("Market orders · "+r.Name, at, ok, 10*time.Minute, 45*time.Minute)
	}

	// Public contracts, per hub region.
	b.queries++
	contractAt := map[int64]time.Time{}
	if rs, err := b.app.queries.StatsPubContractSweeps(b.ctx, regionIDs); err == nil {
		for _, r := range rs {
			contractAt[r.RegionID] = r.UpdatedAt
		}
	} else {
		logging.Errorf("admin analytics: contract sweeps: %v", err)
	}
	for _, r := range marketRegions {
		at, ok := contractAt[r.ID]
		add("Public contracts · "+r.Name, at, ok, 45*time.Minute, 2*time.Hour)
	}

	// Character sync, from the live worker status: the stalest
	// active pilot's overdue figure at the last cycle, plus the
	// time since that cycle ran.
	status := b.app.snapshotWorkerStatus()
	t := status.Timing
	switch {
	case t.At.IsZero():
		add("Character sync · stalest active pilot", time.Time{}, false, 0, 0)
	case t.NeverFetched:
		v.Fresh = append(v.Fresh, analyticsFresh{
			Name:  "Character sync · stalest active pilot",
			Age:   "never fetched",
			Class: "an-bad-t",
		})
		haveStalest, stalest = true, time.Duration(1)<<62
	default:
		age := t.Overdue + now.Sub(t.At)
		add("Character sync · stalest active pilot", now.Add(-age), true, 5*time.Minute, 30*time.Minute)
	}

	b.queries++
	if row, err := b.app.queries.GetKillfeedState(b.ctx); err == nil {
		add("Killmails", row.UpdatedAt, true, 5*time.Minute, 30*time.Minute)
	} else {
		add("Killmails", time.Time{}, false, 0, 0)
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("admin analytics: killfeed state: %v", err)
		}
	}

	b.queries++
	if at, ok := latestOf(b.app.queries.StatsTrueValuesLatest(b.ctx)); ok {
		add("True values", at, true, 30*time.Minute, 13*time.Hour)
	} else {
		add("True values", time.Time{}, false, 0, 0)
	}
	b.queries++
	if at, ok := latestOf(b.app.queries.StatsMarketHistoryLatest(b.ctx)); ok {
		add("Market history", at, true, 2*time.Hour, 6*time.Hour)
	} else {
		add("Market history", time.Time{}, false, 0, 0)
	}
	if swarm.StateFor(b.app).Cfg.Enabled() {
		b.queries++
		if at, ok := latestOf(b.app.queries.StatsSwarmLastApplied(b.ctx)); ok {
			add("Swarm pull", at, true, 15*time.Minute, 2*time.Hour)
		} else {
			add("Swarm pull", time.Time{}, false, 0, 0)
		}
	}
	b.queries++
	if at, ok := latestOf(b.app.queries.StatsIntelLatest(b.ctx)); ok {
		add("Intel reports", at, true, time.Hour, 24*time.Hour)
	} else {
		add("Intel reports", time.Time{}, false, 0, 0)
	}

	parts := []string{}
	if haveStalest && stalest < time.Duration(1)<<60 {
		parts = append(parts, "Stalest active data right now: "+shortDuration(stalest)+".")
	}
	backlog := []string{}
	if t.Deferred > 0 {
		backlog = append(backlog, fmt.Sprintf("%d pilots deferred last cycle", t.Deferred))
	}
	if b.totals.OK && b.totals.Parked > 0 {
		backlog = append(backlog, fmt.Sprintf("%s parked (sign-in needed)", esi.FormatInt(b.totals.Parked)))
	}
	if len(backlog) > 0 {
		parts = append(parts, "Warming backlog: "+strings.Join(backlog, ", ")+".")
	}
	parts = append(parts, "Freshness is the product promise, so it lives on this page permanently.")
	v.FreshNote = strings.Join(parts, " ")
}

// floatOf unwraps the interface{} sqlc gives a coalesced
// aggregate: a float64, or 0 when the value is null.
func floatOf(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// latestOf unwraps the interface{} a max(timestamptz) read
// returns through sqlc: a time.Time, or nil when no row exists.
func latestOf(v interface{}, err error) (time.Time, bool) {
	if err != nil {
		return time.Time{}, false
	}
	if at, ok := v.(time.Time); ok {
		return at, true
	}
	return time.Time{}, false
}

// --- Swarm panel ---------------------------------------------------------------------------

func (b *analyticsBuild) swarmPanel(v *analyticsView) {
	mod := swarm.StateFor(b.app)
	if !mod.Cfg.Enabled() {
		v.SwarmNote = "The shared cache is off on this instance; it fetches everything itself."
		return
	}
	counters := mod.SwarmCounterMap(b.ctx)
	var applied int64
	for name, n := range counters {
		if strings.HasPrefix(name, "applied_") {
			applied += n
		}
	}
	if mod.SwarmID != nil {
		name := "This instance"
		if origin := b.app.PublicOrigin(); origin != "" {
			name = origin
		}
		v.SwarmPeers = append(v.SwarmPeers, analyticsBar{Name: name, Pct: 100, Value: "trust 100 · this instance"})
	}
	for _, p := range mod.LoadSwarmPeers(b.ctx) {
		pct := 0.0
		if n, err := strconv.ParseFloat(strings.TrimSuffix(p.Trust, "%"), 64); err == nil {
			pct = n
		}
		v.SwarmPeers = append(v.SwarmPeers, analyticsBar{
			Name:  p.Display,
			Pct:   pct,
			Value: fmt.Sprintf("trust %s · %s", strings.TrimSuffix(p.Trust, "%"), strings.ToLower(p.Role)),
		})
	}
	note := fmt.Sprintf("Pulls applied %s · failed %s since this instance started counting.",
		esi.FormatInt(applied), esi.FormatInt(counters["pulls_failed"]))
	if b.swarmBytes > 0 {
		note += fmt.Sprintf(" %s of signed data served to peers in the window.", humanBytes(b.swarmBytes))
	}
	v.SwarmNote = note
}

// --- Inventory ---------------------------------------------------------------------------

func (b *analyticsBuild) inventoryRows() []db.StatsInventoryLatestRow {
	if b.invLoaded {
		return b.invCache
	}
	b.invLoaded = true
	b.queries++
	rows, err := b.app.queries.StatsInventoryLatest(b.ctx, 14)
	if err != nil {
		logging.Errorf("admin analytics: inventory: %v", err)
		return nil
	}
	b.invCache = rows
	return rows
}

func (b *analyticsBuild) inventoryDB() (int64, bool) {
	for _, r := range b.inventoryRows() {
		if r.Name == "" {
			return r.SizeBytes, true
		}
	}
	return 0, false
}

func (b *analyticsBuild) inventory(v *analyticsView) {
	rows := b.inventoryRows()
	if len(rows) == 0 {
		v.InvNote = "No inventory snapshot yet. The worker snapshots the catalog once an hour."
		return
	}
	var max int64
	for _, r := range rows {
		if r.Name != "" && r.SizeBytes > max {
			max = r.SizeBytes
		}
	}
	shown := 0
	for _, r := range rows {
		if r.Name == "" || shown >= 10 {
			continue
		}
		shown++
		v.InvBars = append(v.InvBars, analyticsBar{
			Name:  r.Name,
			Mono:  true,
			Pct:   barPct(r.SizeBytes, max),
			Value: fmt.Sprintf("%s rows · %s", esi.FormatInt(r.RowsEst), humanBytes(r.SizeBytes)),
		})
	}
	note := "Largest tables by total size, from the worker's hourly snapshot; row counts are the planner's estimates."
	b.queries++
	if n, err := b.app.queries.StatsPubContractsDeparted(b.ctx); err == nil && n > 0 {
		note += fmt.Sprintf(" pub_contracts includes %s departed contracts held for 7 days as sale evidence.", esi.FormatInt(n))
	}
	v.InvNote = note
}
