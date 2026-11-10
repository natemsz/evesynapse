package app

import (
	"fmt"
	"time"

	"evesynapse/internal/app/module"
)

// Dataset cadences: for each dataset the worker keeps warm, the
// rhythm it lands on in plain words and how old it may get before
// it reads as late ("warn") or stale ("bad"). This is the one
// place those figures live. The sidebar status modal, the admin
// analytics freshness list and the public status page all read
// them from here, and the bounds are derived from the worker's own
// gates, so a gate change moves every surface with it.

// datasetCadence is one dataset's rhythm and lateness bounds.
type datasetCadence struct {
	words     string        // the rhythm, e.g. "swept every 30 minutes"
	okUnder   time.Duration // age up to this reads ok
	warnUnder time.Duration // past okUnder and up to this reads late; beyond, stale
	// published, when set, anchors the age to the dataset's own
	// clock instead: a reading taken after the latest publish is
	// current however old it is, and okUnder/warnUnder then
	// measure how long the latest publish has gone unfetched.
	published func(now time.Time) time.Time
}

// state is the dataset's freshness as "ok", "warn" or "bad".
func (c datasetCadence) state(at, now time.Time) string {
	late := now.Sub(at)
	if c.published != nil {
		pub := c.published(now)
		if !at.Before(pub) {
			return "ok"
		}
		late = now.Sub(pub)
	}
	switch {
	case late > c.warnUnder:
		return "bad"
	case late > c.okUnder:
		return "warn"
	default:
		return "ok"
	}
}

// everyWords says a duration as a plain rhythm: "every 30 minutes",
// "every hour", "every 2 hours".
func everyWords(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "every hour"
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("every %d hours", d/time.Hour)
	case d == time.Minute:
		return "every minute"
	default:
		return fmt.Sprintf("every %d minutes", d/time.Minute)
	}
}

var (
	// Hub order books: a region re-sweeps once its stats are
	// regionSweepGate old, and a sweep takes well under a minute.
	cadenceMarketOrders = datasetCadence{
		words:     "swept " + everyWords(regionSweepGate),
		okUnder:   regionSweepGate + 15*time.Minute,
		warnUnder: 3 * regionSweepGate,
	}
	// Public contracts: each hub's list re-sweeps on
	// pubContractSweepGate.
	cadencePublicContracts = datasetCadence{
		words:     "swept " + everyWords(pubContractSweepGate),
		okUnder:   pubContractSweepGate + 15*time.Minute,
		warnUnder: 4 * pubContractSweepGate,
	}
	// Market history: ESI publishes one day at downtime, and each
	// pair is fetched once after that (module.HistoryCurrent).
	cadenceMarketHistory = datasetCadence{
		words:     "ESI publishes it once a day; fetched after each publish",
		okUnder:   time.Hour,
		warnUnder: 6 * time.Hour,
		published: module.HistoryPublishedAt,
	}
	// Sovereignty: the global snapshot pass refreshes it whenever
	// ESI's cache window ends.
	cadenceSovereignty = datasetCadence{
		words:     "checked each time ESI updates it",
		okUnder:   30 * time.Minute,
		warnUnder: 3 * time.Hour,
	}
	cadenceCharacterSync = datasetCadence{
		words:     "pilots sync about every minute",
		okUnder:   5 * time.Minute,
		warnUnder: 30 * time.Minute,
	}
	cadenceKillmails = datasetCadence{
		words:     "streams in as kills land",
		okUnder:   5 * time.Minute,
		warnUnder: 30 * time.Minute,
	}
	cadenceTrueValues = datasetCadence{
		words:     "recomputed as market data lands",
		okUnder:   30 * time.Minute,
		warnUnder: 13 * time.Hour,
	}
	cadenceSwarmPull = datasetCadence{
		words:     "pulls on the swarm cycle",
		okUnder:   15 * time.Minute,
		warnUnder: 2 * time.Hour,
	}
	cadenceIntelReports = datasetCadence{
		words:     "lands as pilots report",
		okUnder:   time.Hour,
		warnUnder: 24 * time.Hour,
	}
)
