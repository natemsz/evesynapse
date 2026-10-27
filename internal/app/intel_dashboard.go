package app

// ---------------------------------------------------------------------------
// The intel workbench as a dashboard (/intel/workbench/, and every
// report's /intel/workbench/{id}/): one row per recent thread
// (intel_threads.go) across the reader's channels, the map beside
// them following whatever is open, the open report's breakdown
// below (intel_workbench.go), and the feed as the activity log at
// the bottom. A row's Details opens that thread's newest report in
// place (dashboard.js); without JS it is a plain link to the same
// page with that report open.
//
// Everything renders from stored rows: each thread's newest group
// composite carries the count, the reporter and the moment, so the
// page reads one composite per thread and nothing else per row.
// /intel/threads.json is the same rows, for the page's live poll.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// dashThreadWindow is how far back the dashboard lists threads.
	dashThreadWindow = 6 * time.Hour
	// dashThreadCap bounds the rows one render carries.
	dashThreadCap = 40
	// dashRecent is how long a thread's newest report counts as
	// recent: its map ping keeps pulsing until then (or until it
	// is viewed), as the map's intel rings fade over the same span.
	dashRecent = 30 * time.Minute
)

// threadRow is one dashboard row: "12 hostiles in 1DQ1-A | D-Scan by
// Some Scout in 1DQ1-A at 11:20 | Details". The JSON names are
// dashboard.js's (its ThreadRow typedef mirrors them).
type threadRow struct {
	ID       int64 `json:"id"`     // the thread
	ReportID int64 `json:"report"` // its newest report: what Details opens
	Hostiles int   `json:"n"`
	// Count and Noun stand in when no hostiles are counted: what
	// the newest scan saw ("5 ships", "12 pilots").
	Count      int       `json:"count,omitempty"`
	Noun       string    `json:"noun,omitempty"`
	Clear      bool      `json:"clear,omitempty"`
	SystemID   int64     `json:"sys"`
	SystemName string    `json:"sysName"`
	SecText    string    `json:"sec"`
	SecTier    int       `json:"tier"`
	Systems    int       `json:"systems"` // distinct systems the thread spans
	Kind       string    `json:"kind"`    // the newest report's kind, in words
	Reporter   string    `json:"by,omitempty"`
	ChName     string    `json:"chn,omitempty"`
	At         int64     `json:"at"`
	Ago        string    `json:"ago"`
	Time       time.Time `json:"-"`
}

// intelDashboardView is the workbench page's dashboard: the thread
// rows above the open report, the map beside them, the activity log
// below.
type intelDashboardView struct {
	Channels  int
	Threads   []threadRow
	Reports   []reportView // the activity log: the feed's first page
	LastID    int64
	RecentSec int // dashRecent, for the map's pings
	Map       *mapPageView
	// OpenThread is the thread of the report open below the list:
	// its row is marked.
	OpenThread int64
}

// dashboardThreads builds the rows for the reader's channels.
func (app *Application) dashboardThreads(ctx context.Context, channels []accessChannel, now time.Time) []threadRow {
	if len(channels) == 0 {
		return []threadRow{}
	}
	ids := make([]int64, 0, len(channels))
	chName := map[int64]string{}
	for _, c := range channels {
		ids = append(ids, c.ID)
		chName[c.ID] = c.Name
	}
	rows, err := app.queries.ListRecentThreads(ctx, db.ListRecentThreadsParams{ChannelIds: ids, Since: now.Add(-dashThreadWindow)})
	if err != nil {
		logging.Warnf("intel: dashboard threads: %v", err)
		return []threadRow{}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LatestAt.Time.After(rows[j].LatestAt.Time) })
	if len(rows) > dashThreadCap {
		rows = rows[:dashThreadCap]
	}
	tids := make([]int64, len(rows))
	for i, r := range rows {
		tids[i] = r.ThreadID
	}
	spans := map[int64]map[int64]bool{}
	if sys, err := app.queries.ListThreadsSystems(ctx, tids); err == nil {
		for _, s := range sys {
			if spans[s.ThreadID] == nil {
				spans[s.ThreadID] = map[int64]bool{}
			}
			spans[s.ThreadID][s.SystemID] = true
		}
	}
	names := app.newKillNames(ctx)
	out := make([]threadRow, 0, len(rows))
	for _, r := range rows {
		var comp intelComposite
		if json.Unmarshal([]byte(r.Composite), &comp) != nil || len(comp.Members) == 0 {
			continue
		}
		newest := comp.Members[0]
		for _, m := range comp.Members {
			if m.SeenAt.After(newest.SeenAt) {
				newest = m
			}
		}
		row := threadRow{ID: r.ThreadID, ReportID: newest.ReportID, SystemID: r.SystemID,
			Systems: max(len(spans[r.ThreadID]), 1), Reporter: newest.Reporter, ChName: chName[r.ChannelID],
			At: newest.SeenAt.Unix(), Ago: ageText(now, newest.SeenAt), Time: newest.SeenAt}
		if comp.Counts != nil {
			row.Hostiles = comp.Counts.Hostile
		}
		// A scan's picture counts what it saw when no standing
		// split speaks: the d-scan's ships, the local's pilots.
		if row.Hostiles == 0 {
			switch {
			case comp.DScan != nil && comp.DScan.Ships > 0:
				row.Count, row.Noun = comp.DScan.Ships, "ship"
			case comp.Local != nil && len(comp.Local.Pilots) > 0:
				row.Count, row.Noun = len(comp.Local.Pilots), "pilot"
			}
		}
		row.Kind = dashKindLabel(newest)
		row.Clear = newest.Kind == "clear"
		sys := names.system(r.SystemID)
		row.SystemName = sys.Name
		row.SecText, row.SecTier = secRound(sys.Security)
		out = append(out, row)
	}
	return out
}

// dashKindLabel is a member's kind in the row's words: a scan's
// rail label ("D-Scan", "Local") as it stands, a report kind as
// the feed's badge says it.
func dashKindLabel(m compMember) string {
	if m.ScanKind != "" {
		return m.Kind
	}
	var rv reportView
	applyKind(&rv, m.Kind)
	if rv.KindLabel != "" {
		return rv.KindLabel
	}
	return "Report"
}

// workbenchDashboard is the workbench page's thread list, map and
// activity log for the reader, with openThread (the open report's
// thread, 0 for none) marked.
func (app *Application) workbenchDashboard(ctx context.Context, id intelIdentity, openThread int64) *intelDashboardView {
	channels := app.accessibleChannels(ctx, id)
	v := &intelDashboardView{Channels: len(channels), RecentSec: int(dashRecent / time.Second),
		Map: app.loadMapPageView(ctx), OpenThread: openThread}
	v.Threads = app.dashboardThreads(ctx, channels, time.Now())
	v.Reports = app.feedReports(ctx, id, channels, 0, intelFeedTop(), 1<<62, intelFeedPage)
	for _, rep := range v.Reports {
		v.LastID = max(v.LastID, rep.ID)
	}
	return v
}

// reportThread is the thread a stored report's group stands in, 0
// when none is recorded.
func (app *Application) reportThread(ctx context.Context, groupID int64) int64 {
	if groupID <= 0 {
		return 0
	}
	tid, err := app.queries.GetIntelGroupThread(ctx, groupID)
	if err != nil {
		return 0
	}
	return tid
}

// handleIntelDashboard is GET /intel/workbench/: the workbench with
// the newest thread open, or with none when nothing is recent.
func (app *Application) handleIntelDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	dash := app.workbenchDashboard(ctx, id, 0)
	data := app.publicPage(ctx)
	if len(dash.Threads) > 0 {
		if rep, err := app.queries.GetIntelReportFull(ctx, dash.Threads[0].ReportID); err == nil {
			if c, ok := app.channelAccess(ctx, id, rep.ChannelID); ok {
				data.Workbench = app.buildWorkbench(ctx, rep, c)
				dash.OpenThread = dash.Threads[0].ID
			}
		}
	}
	data.IntelDashboard = dash
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: "Intel Workbench", Description: "Recent intel across your channels, threaded by engagement, on the map.", CanonicalPath: "/intel/workbench/"})
	app.render(ctx, w, http.StatusOK, "intel_workbench.html", data)
}

// handleIntelThreadsJSON is GET /intel/threads.json: the dashboard's
// rows, for its live poll.
func (app *Application) handleIntelThreadsJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentity(ctx)
	rows := app.dashboardThreads(ctx, app.accessibleChannels(ctx, id), time.Now())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"threads": rows, "recent": int(dashRecent / time.Second)})
}
