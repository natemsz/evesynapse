package app

// ---------------------------------------------------------------------------
// Axon's intel API: the workbench's data for Axon's own intel windows
// (Threads, Map, Feed, Report), which draw it themselves instead of
// loading the site. Signed and token-scoped like the rest of the Axon
// API (intel_tokens.go), so Axon needs no site sign-in:
//
//	GET /api/intel/v1/threads            the workbench's thread rows
//	GET /api/intel/v1/feed?after=<id>    the activity log, newer than after
//	GET /api/intel/v1/reports/{id}       one report broken down
//
// Every answer is built by the same code the workbench page renders
// from (dashboardThreads, feedReports, buildWorkbench), so the windows
// and the page never disagree. The map is the site's public map
// payload (/map/universe.json), fetched by Axon as it is.
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

func writeAPIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// handleAPIThreads is GET /api/intel/v1/threads.
func (app *Application) handleAPIThreads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentityFor(ctx, tokenUser(ctx), 0)
	rows := app.dashboardThreads(ctx, app.accessibleChannels(ctx, id), time.Now())
	writeAPIJSON(w, map[string]any{"ok": true, "threads": rows, "recent": int(dashRecent / time.Second)})
}

// handleAPIFeed is GET /api/intel/v1/feed.
func (app *Application) handleAPIFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentityFor(ctx, tokenUser(ctx), 0)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	reports := app.feedReports(ctx, id, app.accessibleChannels(ctx, id), after, intelFeedTop(), 1<<62, intelFeedPage)
	if reports == nil {
		reports = []reportView{}
	}
	writeAPIJSON(w, map[string]any{"ok": true, "reports": reports})
}

// handleAPIReport is GET /api/intel/v1/reports/{reportID}: the
// workbench's breakdown of one report, for a channel the token's
// account can read.
func (app *Application) handleAPIReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentityFor(ctx, tokenUser(ctx), 0)
	rid, _ := strconv.ParseInt(chi.URLParam(r, "reportID"), 10, 64)
	rep, err := app.queries.GetIntelReportFull(ctx, rid)
	if err != nil {
		intelAPIError(w, http.StatusNotFound, "no such report")
		return
	}
	c, ok := app.channelAccess(ctx, id, rep.ChannelID)
	if !ok {
		intelAPIError(w, http.StatusNotFound, "no such report")
		return
	}
	v := app.buildWorkbench(ctx, rep, c)
	writeAPIJSON(w, map[string]any{"ok": true, "report": v, "thread": app.reportThread(ctx, rep.GroupID)})
}
