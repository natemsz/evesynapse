package app

import (
	"context"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// guessRateWindow is how far back the guess hit-rate table
// reaches. It mirrors the drain learn window (worker_typing.go);
// both say "recent guesses".
const guessRateWindow = 7 * 24 * time.Hour

// Serving metrics for the Sync page: whether pages render
// complete, how old what they show is, and what viewing costs.
// Renders and record ages are in-memory counters (this run);
// guess hit rates read the typing ledger, like the foresight
// table beside them.

// noteRender records one finished page render: full when nothing
// in its scope was still on its way.
func (app *Application) noteRender(full bool) {
	app.serveMu.Lock()
	defer app.serveMu.Unlock()
	app.serveRenders++
	if full {
		app.serveFull++
	}
}

// noteServedAge records the age of one rendered record.
func (app *Application) noteServedAge(age time.Duration) {
	if age < 0 {
		age = 0
	}
	app.serveMu.Lock()
	defer app.serveMu.Unlock()
	app.serveViews++
	switch {
	case age < time.Hour:
		app.serveAgeHour++
	case age >= 24*time.Hour:
		app.serveAgeOlder++
	}
}

// guessKindLabel names a typing-guess kind the way the pages do.
func guessKindLabel(kind string) string {
	switch kind {
	case string(pageWantPilot):
		return "Pilots"
	case string(pageWantCorporation):
		return "Corporations"
	case string(pageWantAlliance):
		return "Alliances"
	case string(pageWantHistory):
		return "Market history"
	case string(pageWantTypeDescription):
		return "Item details"
	default:
		return kind
	}
}

// syncGuessRow is one guess kind's hit rate over the window.
type syncGuessRow struct {
	Kind    string
	Queued  string
	Opened  string
	HitRate string
}

// serveMetricsView is the Sync page's "Serving pages" card.
type serveMetricsView struct {
	Renders     string // "82% fully loaded (1,204 of 1,468 this run)"
	RendersFull bool   // false until the first render lands
	Age         string // "91% under an hour old (1,300 of 1,429 views)"
	AgeFull     bool
	Calls       string // "37 per active pilot per day (1,204 calls, 33 pilots)"
	CallsFull   bool
	Guesses     []syncGuessRow
	GuessesLine string // "312 guesses, 41 opened (13%) over the last 7 days"
}

// serveMetrics builds the card: counters under one lock, then the
// guess ledger read.
func (app *Application) serveMetrics(ctx context.Context, now time.Time) *serveMetricsView {
	view := &serveMetricsView{}
	app.serveMu.Lock()
	renders, full := app.serveRenders, app.serveFull
	views, hour, older := app.serveViews, app.serveAgeHour, app.serveAgeOlder
	app.serveMu.Unlock()
	if renders > 0 {
		view.RendersFull = true
		view.Renders = percentOf(int64(full), int64(renders)) +
			" fully loaded (" + esi.FormatInt(int64(full)) + " of " + esi.FormatInt(int64(renders)) + " this run)"
	}
	if views > 0 {
		view.AgeFull = true
		view.Age = percentOf(int64(hour), int64(views)) + " under an hour old, " +
			percentOf(int64(older), int64(views)) + " over a day old (" +
			esi.FormatInt(int64(views)) + " views)"
	}
	if pilots := app.activity.activePilots(); pilots > 0 {
		calls := app.esi.CallsLastDay()
		view.CallsFull = true
		view.Calls = esi.FormatInt(calls/int64(pilots)) +
			" per active pilot per day (" + esi.FormatInt(calls) + " calls, " + esi.FormatInt(int64(pilots)) + " pilots)"
	}
	rows, err := app.queries.ListTypingGuessRates(ctx, now.Add(-guessRateWindow))
	if err != nil {
		logging.Errorf("sync: guess hit rates: %v", err)
		return view
	}
	var queued, opened int64
	for _, row := range rows {
		view.Guesses = append(view.Guesses, syncGuessRow{
			Kind:    guessKindLabel(row.Kind),
			Queued:  esi.FormatInt(row.Queued),
			Opened:  esi.FormatInt(row.Opened),
			HitRate: percentOf(row.Opened, row.Queued),
		})
		queued += row.Queued
		opened += row.Opened
	}
	if queued > 0 {
		view.GuessesLine = esi.FormatInt(queued) + " guesses, " + esi.FormatInt(opened) +
			" opened (" + percentOf(opened, queued) + ") over the last 7 days"
	}
	return view
}
