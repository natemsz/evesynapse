package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"evesynapse/internal/app/swarm"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

// Data freshness, shared by the sidebar status modal (and, in
// time, the public status page): for each dataset the app keeps
// warm, when it last landed and the cadence it lands on, so an
// age is always read against the clock that produced it. A
// figure a day old is stale for market orders and current for
// market history, because ESI only republishes history daily;
// no age is ever shown bare.
//
// The cadences and their ok/warn bounds live in cadence.go, shared
// with the admin analytics freshness list and the status page: a
// row is "ok" while its age is inside the dataset's normal cadence,
// "warn" past it, and "bad" once it is beyond the outer bound.

type freshRow struct {
	Name  string `json:"name"`
	State string `json:"state"` // "ok", "warn", "bad" or "none"
	Text  string `json:"text"`  // the age, framed against the cadence
}

// freshReading is one dataset's last landing, read against its
// cadence (cadence.go). ok is false when it has not landed yet;
// never marks a dataset that is due but was never fetched at all.
type freshReading struct {
	name  string
	cad   datasetCadence
	at    time.Time
	ok    bool
	never bool
}

// state is the reading's freshness: the cadence's verdict, "none"
// before a first landing, "bad" when it was never fetched.
func (r freshReading) state(now time.Time) string {
	switch {
	case r.never:
		return "bad"
	case !r.ok:
		return "none"
	}
	return r.cad.state(r.at, now)
}

// freshnessReadings reads every dataset's last landing: bounded
// reads, one per dataset or per small set of hub rows. The sidebar
// modal and the admin analytics page both build from this list, so
// they always show the same datasets on the same cadences. withIntel
// adds the intel reports row the admin page carries. It also
// returns the worker timing the character row was read from and how
// many queries it made.
func (app *Application) freshnessReadings(ctx context.Context, now time.Time, withIntel bool) ([]freshReading, workerTiming, int) {
	var out []freshReading
	queries := 0
	latest := func(name string, cad datasetCadence, v interface{}, err error) {
		queries++
		at, ok := latestOf(v, err)
		out = append(out, freshReading{name: name, cad: cad, at: at, ok: ok})
	}

	// Market orders and public contracts, per hub region.
	kinds := make([]string, 0, len(marketRegions))
	regionIDs := make([]int64, 0, len(marketRegions))
	for _, r := range marketRegions {
		kinds = append(kinds, regionSweepKind(r.ID))
		regionIDs = append(regionIDs, r.ID)
	}
	queries++
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
		out = append(out, freshReading{name: "Market orders · " + r.Name, cad: cadenceMarketOrders, at: at, ok: ok})
	}
	queries++
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
		out = append(out, freshReading{name: "Public contracts · " + r.Name, cad: cadencePublicContracts, at: at, ok: ok})
	}

	// Character sync, from the live worker status: the stalest
	// active pilot's overdue figure at the last cycle, plus the
	// time since that cycle ran.
	t := app.snapshotWorkerStatus().Timing
	sync := freshReading{name: "Character sync · stalest active pilot", cad: cadenceCharacterSync}
	switch {
	case t.At.IsZero():
	case t.NeverFetched:
		sync.never = true
	default:
		sync.at, sync.ok = now.Add(-(t.Overdue + now.Sub(t.At))), true
	}
	out = append(out, sync)

	queries++
	kills := freshReading{name: "Killmails", cad: cadenceKillmails}
	if row, err := app.queries.GetKillfeedState(ctx); err == nil {
		kills.at, kills.ok = row.UpdatedAt, true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("freshness: killfeed state: %v", err)
	}
	out = append(out, kills)

	v, err := app.queries.StatsTrueValuesLatest(ctx)
	latest("True values", cadenceTrueValues, v, err)
	v, err = app.queries.StatsMarketHistoryLatest(ctx)
	latest("Market history", cadenceMarketHistory, v, err)

	queries++
	sov := freshReading{name: "Sovereignty", cad: cadenceSovereignty}
	if snap, err := app.queries.GetGlobalSnapshot(ctx, esi.GlobalSovMap); err == nil {
		sov.at, sov.ok = snap.FetchedAt, true
	}
	out = append(out, sov)

	if swarm.StateFor(app).Cfg.Enabled() {
		v, err = app.queries.StatsSwarmLastApplied(ctx)
		latest("Swarm pull", cadenceSwarmPull, v, err)
	}
	if withIntel {
		v, err = app.queries.StatsIntelLatest(ctx)
		latest("Intel reports", cadenceIntelReports, v, err)
	}
	return out, t, queries
}

// stalestReading is the oldest landed age across the readings, for
// the "stalest active data" note; ok is false when nothing landed.
// A dataset never fetched counts as stalest of all.
func stalestReading(readings []freshReading, now time.Time) (time.Duration, bool, bool) {
	var stalest time.Duration
	have := false
	for _, r := range readings {
		if r.never {
			return 0, false, true
		}
		if !r.ok {
			continue
		}
		age := now.Sub(r.at)
		if age < 0 {
			age = 0
		}
		if !have || age > stalest {
			stalest, have = age, true
		}
	}
	return stalest, have, false
}

// freshnessRows frames every reading for the sidebar modal: its
// age read against its cadence, never a bare age.
func (app *Application) freshnessRows(ctx context.Context, now time.Time) ([]freshRow, string) {
	readings, t, _ := app.freshnessReadings(ctx, now, false)
	rows := make([]freshRow, 0, len(readings))
	for _, r := range readings {
		row := freshRow{Name: r.name, State: r.state(now)}
		switch {
		case r.never:
			row.Text = "never fetched · " + r.cad.words
		case !r.ok:
			row.Text = "no data yet · " + r.cad.words
		default:
			age := now.Sub(r.at)
			if age < 0 {
				age = 0
			}
			row.Text = shortDuration(age) + " ago · " + r.cad.words
		}
		rows = append(rows, row)
	}
	note := ""
	if stalest, have, never := stalestReading(readings, now); have && !never {
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
	degraded := ""
	if areas := app.degradedRouteAreas(ctx, time.Now()); len(areas) > 0 {
		degraded = "CCP reports problems with " + strings.Join(areas, ", ") + " right now."
	} else if app.esiErrorShareHigh(ctx, time.Now()) {
		degraded = "ESI calls are failing right now."
	}
	writeBasketJSON(w, http.StatusOK, map[string]any{
		"esi": map[string]any{
			"state":   health.State,
			"label":   health.Label,
			"players": health.Players,
			"title":   health.Title,
		},
		"rows":     rows,
		"note":     note,
		"degraded": degraded,
	})
}
