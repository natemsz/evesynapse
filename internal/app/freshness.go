package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"evesynapse/internal/app/swarm"
	"evesynapse/internal/logging"
)

// Data freshness, shared by the sidebar status modal (and, in
// time, the public status page): for each dataset the app keeps
// warm, when it last landed and the cadence it lands on, so an
// age is always read against the clock that produced it. A
// figure a day old is stale for market orders and current for
// market history, because ESI only republishes history daily;
// no age is ever shown bare.
//
// The ok/warn thresholds are the admin analytics page's own
// (admin_analytics.go freshness): a row is "ok" while its age is
// inside the dataset's normal cadence, "warn" past it, and
// "bad" once it is beyond the outer bound.

type freshRow struct {
	Name  string `json:"name"`
	State string `json:"state"` // "ok", "warn", "bad" or "none"
	Text  string `json:"text"`  // the age, framed against the cadence
}

// freshSource is one dataset's place in the table: how to read
// its last landing and the cadence it lands on.
type freshSource struct {
	name      string
	cadence   string // words for the rhythm, e.g. "sweeps hourly"
	okUnder   time.Duration
	warnUnder time.Duration
}

func freshState(age time.Duration, ok bool, s freshSource) string {
	if !ok {
		return "none"
	}
	switch {
	case age > s.warnUnder:
		return "bad"
	case age > s.okUnder:
		return "warn"
	default:
		return "ok"
	}
}

// freshnessRows reads every dataset's last landing, the same
// bounded reads the admin analytics freshness section makes.
func (app *Application) freshnessRows(ctx context.Context, now time.Time) ([]freshRow, string) {
	var rows []freshRow
	var stalest time.Duration
	haveStalest := false
	add := func(s freshSource, at time.Time, ok bool) {
		if !ok {
			rows = append(rows, freshRow{Name: s.name, State: "none", Text: "no data yet · " + s.cadence})
			return
		}
		age := now.Sub(at)
		if age < 0 {
			age = 0
		}
		rows = append(rows, freshRow{
			Name:  s.name,
			State: freshState(age, true, s),
			Text:  shortDuration(age) + " ago · " + s.cadence,
		})
		if !haveStalest || age > stalest {
			stalest, haveStalest = age, true
		}
	}

	// Market orders and public contracts, per hub region.
	kinds := make([]string, 0, len(marketRegions))
	regionIDs := make([]int64, 0, len(marketRegions))
	for _, r := range marketRegions {
		kinds = append(kinds, regionSweepKind(r.ID))
		regionIDs = append(regionIDs, r.ID)
	}
	swept := map[string]time.Time{}
	if rs, err := app.queries.StatsFetchStatesByKinds(ctx, kinds); err == nil {
		for _, r := range rs {
			swept[r.Kind] = r.AttemptedAt
		}
	} else {
		logging.Errorf("freshness: fetch states: %v", err)
	}
	for _, r := range marketRegions {
		at, ok := swept[regionSweepKind(r.ID)]
		add(freshSource{"Market orders · " + r.Name, "sweeps hourly", 10 * time.Minute, 45 * time.Minute}, at, ok)
	}
	contractAt := map[int64]time.Time{}
	if rs, err := app.queries.StatsPubContractSweeps(ctx, regionIDs); err == nil {
		for _, r := range rs {
			contractAt[r.RegionID] = r.UpdatedAt
		}
	} else {
		logging.Errorf("freshness: contract sweeps: %v", err)
	}
	for _, r := range marketRegions {
		at, ok := contractAt[r.ID]
		add(freshSource{"Public contracts · " + r.Name, "sweeps hourly", 45 * time.Minute, 2 * time.Hour}, at, ok)
	}

	// Character sync, from the live worker status.
	syncSrc := freshSource{"Character sync · stalest active pilot", "pilots sync about every minute", 5 * time.Minute, 30 * time.Minute}
	status := app.snapshotWorkerStatus()
	t := status.Timing
	switch {
	case t.At.IsZero():
		add(syncSrc, time.Time{}, false)
	case t.NeverFetched:
		rows = append(rows, freshRow{Name: syncSrc.name, State: "bad", Text: "never fetched · " + syncSrc.cadence})
		haveStalest, stalest = true, time.Duration(1)<<62
	default:
		add(syncSrc, now.Add(-(t.Overdue + now.Sub(t.At))), true)
	}

	if row, err := app.queries.GetKillfeedState(ctx); err == nil {
		add(freshSource{"Killmails", "streams in as kills land", 5 * time.Minute, 30 * time.Minute}, row.UpdatedAt, true)
	} else {
		add(freshSource{"Killmails", "streams in as kills land", 5 * time.Minute, 30 * time.Minute}, time.Time{}, false)
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("freshness: killfeed state: %v", err)
		}
	}
	if at, ok := latestOf(app.queries.StatsTrueValuesLatest(ctx)); ok {
		add(freshSource{"True values", "recomputed as market data lands", 30 * time.Minute, 13 * time.Hour}, at, true)
	} else {
		add(freshSource{"True values", "recomputed as market data lands", 30 * time.Minute, 13 * time.Hour}, time.Time{}, false)
	}
	if at, ok := latestOf(app.queries.StatsMarketHistoryLatest(ctx)); ok {
		add(freshSource{"Market history", "ESI publishes it daily; fetched every few hours", 2 * time.Hour, 6 * time.Hour}, at, true)
	} else {
		add(freshSource{"Market history", "ESI publishes it daily; fetched every few hours", 2 * time.Hour, 6 * time.Hour}, time.Time{}, false)
	}
	if swarm.StateFor(app).Cfg.Enabled() {
		if at, ok := latestOf(app.queries.StatsSwarmLastApplied(ctx)); ok {
			add(freshSource{"Swarm pull", "pulls on the swarm cycle", 15 * time.Minute, 2 * time.Hour}, at, true)
		} else {
			add(freshSource{"Swarm pull", "pulls on the swarm cycle", 15 * time.Minute, 2 * time.Hour}, time.Time{}, false)
		}
	}
	if at, ok := latestOf(app.queries.StatsIntelLatest(ctx)); ok {
		add(freshSource{"Intel reports", "lands as reports arrive", time.Hour, 24 * time.Hour}, at, true)
	} else {
		add(freshSource{"Intel reports", "lands as reports arrive", time.Hour, 24 * time.Hour}, time.Time{}, false)
	}

	note := ""
	if haveStalest && stalest < time.Duration(1)<<60 {
		note = "Stalest active data right now: " + shortDuration(stalest) + "."
	}
	if t.Deferred > 0 {
		if note != "" {
			note += " "
		}
		note += fmt.Sprintf("Warming backlog: %d pilots deferred last cycle.", t.Deferred)
	}
	return rows, note
}

// handleStatusFreshness serves the sidebar status modal: ESI's
// health (the Overview pill's own read) plus the freshness rows.
func (app *Application) handleStatusFreshness(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, note := app.freshnessRows(ctx, time.Now())
	health := app.esiHealth(ctx)
	writeBasketJSON(w, http.StatusOK, map[string]any{
		"esi": map[string]any{
			"state":   health.State,
			"label":   health.Label,
			"players": health.Players,
			"title":   health.Title,
		},
		"rows": rows,
		"note": note,
	})
}
