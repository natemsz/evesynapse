package app

// ---------------------------------------------------------------------------
// Intel threads (schema 095): one engagement as the dashboard shows
// it.
//
// A group (intel_groups.go) is one channel's reports on one system
// inside a sitting, and its composite is that system's picture. An
// engagement moves: a gang jumps a gate and the reports in the next
// system found a new group. A thread links them. When a group's
// composite is first stored, the group joins the thread of the
// freshest group in the same channel on its own system or a
// neighbouring one (one jump, sde_system_jumps) whose latest moment
// is within intelThreadWindow of the new group's; with none, the
// group founds a thread named for itself. A thread that has run for
// intelThreadSpan takes no more groups, so a quiet region's steady
// trickle of reports never chains into one endless thread.
//
// Unlike gang fusion (intel_gangs.go), which decides that two
// sightings are the same pilots, a thread asks only whether the
// reports belong together on the map and in time; two gangs one
// jump apart share a thread. It is set once and never moved:
// renders read it stored.
// ---------------------------------------------------------------------------

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// intelThreadWindow is how close in time a new group's
	// first moment must be to a neighbouring group's latest for
	// the two to share a thread: a gate or two of travel.
	intelThreadWindow = 30 * time.Minute
	// intelThreadSpan caps how long one thread may run and still
	// take groups.
	intelThreadSpan = 6 * time.Hour
)

// assignGroupThread settles the thread of a group whose composite
// was just stored: a group already in one keeps it.
func (app *Application) assignGroupThread(ctx context.Context, comp *intelComposite) {
	if comp == nil || comp.GroupID <= 0 {
		return
	}
	if tid, err := app.queries.GetIntelGroupThread(ctx, comp.GroupID); err != nil || tid != 0 {
		return
	}
	thread := app.threadFor(ctx, comp)
	if thread == 0 {
		thread = comp.GroupID
	}
	if err := app.queries.SetIntelGroupThread(ctx, db.SetIntelGroupThreadParams{GroupID: comp.GroupID, ThreadID: thread}); err != nil {
		logging.Warnf("intel: thread of group %d: %v", comp.GroupID, err)
	}
}

// threadFor is the thread a group joins, or 0 to found its own.
func (app *Application) threadFor(ctx context.Context, comp *intelComposite) int64 {
	at := comp.LatestAt
	if len(comp.Members) > 0 {
		at = comp.Members[0].SeenAt
		for _, m := range comp.Members {
			if m.SeenAt.Before(at) {
				at = m.SeenAt
			}
		}
	}
	if at.IsZero() || comp.SystemID == 0 {
		return 0
	}
	systems := []int64{comp.SystemID}
	if ns, err := app.queries.ListSDESystemNeighbours(ctx, comp.SystemID); err == nil {
		systems = append(systems, ns...)
	}
	rows, err := app.queries.ListThreadCandidates(ctx, db.ListThreadCandidatesParams{
		ChannelID: comp.ChannelID, GroupID: comp.GroupID, SystemIds: systems,
		Since: at.Add(-intelThreadWindow), Until: at.Add(intelThreadWindow),
	})
	if err != nil {
		logging.Warnf("intel: thread candidates for group %d: %v", comp.GroupID, err)
		return 0
	}
	for _, r := range rows {
		span, err := app.queries.GetIntelThreadSpan(ctx, r.ThreadID)
		if err != nil || at.Sub(span.FirstAt) > intelThreadSpan {
			continue
		}
		return r.ThreadID
	}
	return 0
}
