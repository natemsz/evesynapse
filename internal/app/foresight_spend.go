package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Foresight: spending the allowance where it is worth most, and
// learning what that is (see foresight.go).
// ---------------------------------------------------------------------------

// foresightOdds is the chance, as learned so far, that a target of a
// kind of event is followed by its page being opened: for everyone,
// and for one account.
type foresightOdds struct {
	all  map[string][2]int64           // kind -> resolved, opened
	mine map[int64]map[string][2]int64 // account -> kind -> resolved, opened
}

// chance weighs the record against the assumption. With no record it
// is the assumed chance; with a long one it is the record.
func (o foresightOdds) chance(kind string) float64 {
	seen := o.all[kind]
	return (float64(seen[1]) + foresightKindByID(kind).Chance*foresightAssumedWeight) / (float64(seen[0]) + foresightAssumedWeight)
}

// chanceFor is the same for one account: its own record weighed
// against everyone's.
func (o foresightOdds) chanceFor(userID int64, kind string) float64 {
	seen := o.mine[userID][kind]
	return (float64(seen[1]) + o.chance(kind)*foresightAccountWeight) / (float64(seen[0]) + foresightAccountWeight)
}

func (app *Application) foresightOdds(ctx context.Context, users []int64, now time.Time) foresightOdds {
	odds := foresightOdds{all: map[string][2]int64{}, mine: map[int64]map[string][2]int64{}}
	since := now.Add(-foresightLearnWindow)
	rates, err := app.queries.ListForesightRates(ctx, db.ListForesightRatesParams{Since: since, Now: now})
	if err != nil {
		logging.Errorf("foresight: read rates: %v", err)
	}
	for _, r := range rates {
		odds.all[r.EventKind] = [2]int64{r.Resolved, r.Opened}
	}
	if len(users) == 0 {
		return odds
	}
	own, err := app.queries.ListForesightUserRates(ctx, db.ListForesightUserRatesParams{UserIds: users, Since: since, Now: now})
	if err != nil {
		logging.Errorf("foresight: read account rates: %v", err)
	}
	for _, r := range own {
		if odds.mine[r.UserID] == nil {
			odds.mine[r.UserID] = map[string][2]int64{}
		}
		odds.mine[r.UserID][r.EventKind] = [2]int64{r.Resolved, r.Opened}
	}
	return odds
}

// foresightScore is what one target is worth a call for: the chance
// its page is opened, times how stale the data is (0 to 1), over what
// warming it costs.
func foresightScore(chance, stale float64, cost int) float64 {
	if cost < 1 {
		cost = 1
	}
	return chance * stale / float64(cost)
}

// foresightStaleness says how stale a target's data is, from 0 (as
// fresh as ESI will give it: a call would buy nothing) to 1, and what
// warming it is expected to cost in calls.
func (app *Application) foresightStaleness(ctx context.Context, t db.ForesightTarget, now time.Time) (stale float64, cost int) {
	key := marketKey{RegionID: t.RegionID, TypeID: t.SubjectID}
	switch t.TargetKind {
	case targetBook:
		age, pages, held := app.books.age(key, now)
		if held && age < bookCacheTTL {
			return 0, pages
		}
		if pages == 0 {
			pages = 1
			if t.RegionID == defaultMarketRegion {
				pages = 2 // The Forge's books are the long ones
			}
		}
		return 1, pages
	case targetHistory:
		if app.marketFetchDue(ctx, marketFetchKind("history", key), historyRefetchGate) {
			return 1, 1
		}
		return 0, 1
	case targetSnapshot:
		meta, err := app.queries.ListSnapshotMetaByCharacter(ctx, t.SubjectID)
		if err != nil {
			return 1, 1
		}
		for _, snap := range meta {
			if snap.Kind != t.Detail {
				continue
			}
			if esi.CacheWindowOpen(snap.CachedUntil) {
				return 0, 1
			}
			return clamp01(now.Sub(snap.FetchedAt).Minutes() / 30), 1
		}
		return 1, 1
	case targetCorporation:
		if rec, err := app.queries.GetCorporationRecord(ctx, t.SubjectID); err == nil && rec.State == orgStateReady && rec.FetchedAt.Valid {
			return staleByDay(now.Sub(rec.FetchedAt.Time)), 1
		}
		return 1, 1
	case targetAlliance:
		if rec, err := app.queries.GetAllianceRecord(ctx, t.SubjectID); err == nil && rec.State == orgStateReady && rec.FetchedAt.Valid {
			return staleByDay(now.Sub(rec.FetchedAt.Time)), 1
		}
		return 1, 1
	}
	return 0, 1
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// staleByDay: a public record under six hours old is fresh; it is
// fully stale at a day.
func staleByDay(age time.Duration) float64 {
	if age < 6*time.Hour {
		return 0
	}
	return clamp01(age.Hours() / 24)
}

// foresightWarm warms one target and reports the calls it made, or
// set in motion. limited: ESI said to stop.
func (app *Application) foresightWarm(ctx context.Context, t db.ForesightTarget, now time.Time) (calls int, limited bool) {
	key := marketKey{RegionID: t.RegionID, TypeID: t.SubjectID}
	switch t.TargetKind {
	case targetBook:
		if _, _, _, err := app.fetchOrderBook(ctx, t.RegionID, t.SubjectID); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return 0, true
			}
			logging.Errorf("foresight: order book %d in region %d: %v", t.SubjectID, t.RegionID, err)
			return 1, false
		}
		_, pages, _ := app.books.age(key, now)
		if pages < 1 {
			pages = 1
		}
		return pages, false
	case targetHistory:
		_, limited := app.fetchAndStoreHistory(ctx, key)
		return 1, limited
	case targetSnapshot:
		// The character pass makes the call, at its next round: this
		// lifts the tier's hold on that one dataset (worker_tiers.go).
		app.activity.boost(t.SubjectID, t.Detail, now)
		return 1, false
	case targetCorporation:
		// Foresight names come from the account's own records, so
		// they queue at the orbit ring, like the name harvest.
		if err := app.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{
			CorporationID: t.SubjectID, Priority: wantOrbit, NotedAt: timeSet(now),
		}); err != nil {
			logging.Errorf("foresight: want corporation %d: %v", t.SubjectID, err)
		}
		return 1, false
	case targetAlliance:
		if err := app.queries.UpsertAllianceWant(ctx, db.UpsertAllianceWantParams{
			AllianceID: t.SubjectID, Priority: wantOrbit, NotedAt: timeSet(now),
		}); err != nil {
			logging.Errorf("foresight: want alliance %d: %v", t.SubjectID, err)
		}
		return 1, false
	}
	return 0, false
}

// foresightSpend warms the pending targets most worth it, from what
// the cycle may still fetch. A target whose data is already fresh is
// settled at no cost: the prediction still stands, and still teaches.
func (app *Application) foresightSpend(ctx context.Context, allowance *fetchBudget, now time.Time) (stored int, limited bool) {
	if app.cfg.foresightOff {
		return 0, false
	}
	st := &app.foresight
	if live, err := app.queries.CountForesightLive(ctx, now); err == nil {
		st.live.Store(live)
	}
	st.mu.Lock()
	prune := now.Sub(st.lastPrune) >= time.Hour
	if prune {
		st.lastPrune = now
	}
	st.mu.Unlock()
	if prune {
		if err := app.queries.PruneForesightTargets(ctx, now.Add(-foresightKeep)); err != nil {
			logging.Errorf("foresight: prune: %v", err)
		}
	}

	pending, err := app.queries.ListForesightPending(ctx, db.ListForesightPendingParams{Now: now, RowLimit: foresightPendingRead})
	if err != nil {
		logging.Errorf("foresight: read pending targets: %v", err)
		return 0, false
	}
	if len(pending) == 0 {
		return 0, false
	}
	seen := map[int64]bool{}
	var users []int64
	for _, t := range pending {
		if !seen[t.UserID] {
			seen[t.UserID] = true
			users = append(users, t.UserID)
		}
	}
	odds := app.foresightOdds(ctx, users, now)

	type candidate struct {
		target db.ForesightTarget
		score  float64
		cost   int
	}
	settle := func(t db.ForesightTarget, calls int) {
		if err := app.queries.MarkForesightWarmed(ctx, db.MarkForesightWarmedParams{ID: t.ID, WarmedAt: timeSet(now), Calls: int64(calls)}); err != nil {
			logging.Errorf("foresight: settle target %d: %v", t.ID, err)
		}
	}
	var queue []candidate
	for _, t := range pending {
		stale, cost := app.foresightStaleness(ctx, t, now)
		if stale <= 0 {
			settle(t, 0)
			continue
		}
		queue = append(queue, candidate{t, foresightScore(odds.chanceFor(t.UserID, t.EventKind), stale, cost), cost})
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].score > queue[j].score })

	left := app.cfg.foresightFetches
	for _, c := range queue {
		if ctx.Err() != nil || app.esi.ErrorBudgetLow() {
			break
		}
		// One that does not fit is passed over, not waited behind: a
		// cheaper one further down may still fit.
		if c.cost > left {
			continue
		}
		if !allowance.take() {
			break
		}
		calls, limited := app.foresightWarm(ctx, c.target, now)
		if limited {
			return stored, true
		}
		left -= max(calls, 1)
		settle(c.target, calls)
		stored++
	}
	return stored, false
}

// ---------------------------------------------------------------------------
// What it cost and what came of it, for the Sync page.
// ---------------------------------------------------------------------------

type foresightRow struct {
	Event   string
	Events  int64
	Targets int64
	Calls   int64
	Opened  int64
	HitRate string // of the targets that ran their course, the share opened
	Warm    string // of those opened, the share warmed before the page was
	Chance  string // the chance in use now
}

type foresightStats struct {
	Off       bool
	Days      int
	Rows      []foresightRow
	Calls     int64
	Opened    int64
	PerOpened string // calls spent for each target that was opened
	PerCycle  int
}

func percentOf(part, whole int64) string {
	if whole <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", (part*100+whole/2)/whole)
}

func (app *Application) foresightStats(ctx context.Context, now time.Time) *foresightStats {
	stats := &foresightStats{Off: app.cfg.foresightOff, Days: int(foresightLearnWindow / (24 * time.Hour)), PerCycle: app.cfg.foresightFetches}
	if stats.Off {
		return stats
	}
	rows, err := app.queries.ListForesightSummary(ctx, db.ListForesightSummaryParams{Since: now.Add(-foresightLearnWindow), Now: now})
	if err != nil {
		logging.Errorf("foresight: read summary: %v", err)
		return stats
	}
	odds := app.foresightOdds(ctx, nil, now)
	found := map[string]db.ListForesightSummaryRow{}
	for _, r := range rows {
		found[r.EventKind] = r
	}
	for _, kind := range foresightKinds {
		r := found[kind.ID]
		stats.Rows = append(stats.Rows, foresightRow{
			Event: kind.Title, Events: r.Events, Targets: r.Targets, Calls: r.Calls, Opened: r.Opened,
			HitRate: percentOf(r.Opened, r.Resolved), Warm: percentOf(r.OpenedWarm, r.Opened),
			Chance: fmt.Sprintf("%.0f%%", odds.chance(kind.ID)*100),
		})
		stats.Calls += r.Calls
		stats.Opened += r.Opened
	}
	stats.PerOpened = "—"
	if stats.Opened > 0 {
		stats.PerOpened = fmt.Sprintf("%.1f", float64(stats.Calls)/float64(stats.Opened))
	}
	return stats
}
