package app

// ---------------------------------------------------------------------------
// Self-maintenance modes (v0.3.05): the binary can look after
// itself on the box, so keeping EveSynapse current is two short
// commands instead of a manual binary shuffle.
//
//   evesynapse -version
//       Print the version and exit.
//
//   evesynapse -update ...
//       Replace the program with a newer build and restart onto
//       it. That mode is its own package, internal/selfupdate; it
//       is handed Version() and needs nothing else from here.
//
//   evesynapse -refresh
//       Mark every cached download out of date so the next start
//       fetches fresh copies. Earned data (accounts, watchlist,
//       saved layouts, collected history) is never touched.
//
// This file is the version and -refresh. -refresh needs the
// application's configuration and database, which is why it lives
// here. It refuses to run while the server is up, which it learns
// from the server's pidfile (internal/pidfile).
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"evesynapse/internal/pidfile"
	"evesynapse/internal/store"
)

// Version returns the rendered product version: "v" followed by the
// contents of version.txt, the same string the page footer shows.
func Version() string { return appVersion }

// ---------------------------------------------------------------------------
// -refresh: mark every cached download out of date.
// ---------------------------------------------------------------------------

// cacheEpoch is the time every cache is rewound to: any freshness
// check in the app reads it as "as old as it gets".
var cacheEpoch = time.Unix(0, 0).UTC()

// RunRefresh implements `evesynapse -refresh` (cfg detected the
// same way the server detects it). It refuses to run while the
// server is up: the box flow is stop the service, run this,
// start the service.
func RunRefresh(args []string, cfg Config, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "Usage: evesynapse -refresh")
		return 2
	}
	pidPath := ""
	exe, err := os.Executable()
	if err != nil {
		// Without a resolvable pidfile there is no live-server
		// check to make; say so and carry on rather than fail.
		fmt.Fprintln(stdout, "(Couldn't check whether EveSynapse is running — continuing anyway.)")
	} else if pid, live, ok := pidfile.FindLive(pidfile.Candidates(filepath.Dir(exe))); ok && pidfile.ProcessCheck(pid, exe) {
		// Hand runRefresh the pidfile that names a running server,
		// so it refuses; with none found there is nothing to check.
		// A stale pidfile whose PID now belongs to some other
		// program does not count.
		pidPath = live
	}
	return runRefresh(cfg, pidPath, stdout, stderr)
}

func runRefresh(cfg Config, pidPath string, stdout, stderr io.Writer) int {
	if pidPath != "" {
		if _, live := pidfile.LivePID(pidPath); live {
			fmt.Fprintln(stderr, "EveSynapse is still running. Stop it first (sudo systemctl stop evesynapse), then run this again.")
			return 1
		}
	}
	conn, pool, err := store.Open(context.Background(), cfg.databaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't open the EveSynapse database: %v\n", err)
		return 1
	}
	defer conn.Close()
	defer pool.Close()

	steps, err := expireCaches(conn)
	if err != nil {
		fmt.Fprintf(stderr, "The refresh didn't finish: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "Refresh ready — EveSynapse will fetch fresh copies of everything when it next starts:")
	for _, step := range steps {
		fmt.Fprintf(stdout, "  • %s\n", step)
	}
	fmt.Fprintln(stdout, "Your accounts, characters, watchlist, saved layouts, and collected history were not touched.")
	return 0
}

// cacheExpiry is one store's row-expire statement plus the
// sentence the refresh summary reports for it.
type cacheExpiry struct {
	label string
	sql   string
	args  []any
}

// expireCaches rewinds every cache the worker consults to the
// epoch and reports what it did, one human sentence per store.
// Payloads are kept — only the freshness stamps move — so pages
// keep showing the last known data until the fresh copies land,
// and anything the worker can't refetch (a parked character, a
// settled "not there" answer) simply stays as it was.
//
// Earned data is deliberately absent from this list: users,
// characters (tokens, tags, link state), sessions, home layouts
// and widget settings, the watchlist, skill plans, the daily
// wallet history, and the collected market history all survive.
func expireCaches(conn *sql.DB) ([]string, error) {
	expiries := []cacheExpiry{
		{
			label: "Character data (skills, wallets, assets, mail, contracts…)",
			// The ETag goes too: with it, the next fetch would only
			// ask ESI whether the copy is current and keep it. A
			// refresh is for downloading everything again.
			sql:  `UPDATE character_snapshots SET fetched_at = $1, cached_until = $2, etag = ''`,
			args: []any{cacheEpoch, cacheEpoch},
		},
		{
			label: "Character profiles",
			sql:   `UPDATE characters SET cached_until = $1 WHERE cached_until IS NOT NULL`,
			args:  []any{cacheEpoch},
		},
		{
			label: "Public data (incursions, faction warfare…)",
			sql:   `UPDATE global_snapshots SET fetched_at = $1, cached_until = $2, etag = ''`,
			args:  []any{cacheEpoch, cacheEpoch},
		},
		{
			// The stored /markets/prices/ mirror: an epoch
			// cached_until puts it outside ESI's window, so the
			// worker fetches it again on the first cycle.
			label: "Market price guide",
			sql:   `UPDATE guide_prices_meta SET fetched_at = $1, cached_until = $2`,
			args:  []any{cacheEpoch, cacheEpoch},
		},
		{
			// Retry timers, not data: rewinding attempted_at
			// lifts the "asked recently, wait" backoffs so the
			// refetch starts on the first cycle, not the next day.
			label: "Fetch retry timers",
			sql:   `UPDATE snapshot_fetch_state SET attempted_at = $1`,
			args:  []any{cacheEpoch},
		},
		{
			label: "Market fetch timers",
			sql:   `UPDATE market_fetch_state SET attempted_at = $1`,
			args:  []any{cacheEpoch},
		},
		{
			// Ready pilot records past their week-old cutoff are
			// drained again in the background; 'missing' rows are
			// settled answers and stay settled.
			label: "Pilot records",
			sql:   `UPDATE pilot_records SET fetched_at = $1 WHERE state = 'ready'`,
			args:  []any{cacheEpoch},
		},
		{
			// A missing fetched_at is exactly how the item page
			// asks for a description, so clearing it re-asks for
			// every description the ESI fallback ever supplied.
			label: "Item descriptions",
			sql:   `UPDATE type_details SET fetched_at = NULL WHERE fetched_at IS NOT NULL`,
		},
		{
			// Resolved names re-check after 30 days, missing ones
			// after a day; the epoch puts both past their windows.
			label: "Structure names",
			sql:   `UPDATE structure_names SET resolved_at = $1 WHERE state IN ('resolved', 'missing')`,
			args:  []any{cacheEpoch},
		},
	}

	var steps []string
	for _, expiry := range expiries {
		res, err := conn.Exec(expiry.sql, expiry.args...)
		if err != nil {
			return steps, fmt.Errorf("%s: %w", expiry.label, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return steps, fmt.Errorf("%s: %w", expiry.label, err)
		}
		steps = append(steps, fmt.Sprintf("%s: %s marked out of date", expiry.label, pluralCount(n, "entry", "entries")))
	}

	// The static game data (types, stations, systems…) re-imports
	// in its usual checked, all-or-nothing way on next start: any
	// marker other than the current one makes the importer run,
	// and a successful import writes the marker back.
	if _, err := conn.Exec(
		`INSERT INTO sde_meta (key, value) VALUES ('sde_import_version', 'refresh')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		return steps, fmt.Errorf("static game data: %w", err)
	}
	steps = append(steps, "Static game data (items, stations, systems…): will be re-imported")

	return steps, nil
}

// pluralCount formats "1 entry" / "12 entries" for the summary.
func pluralCount(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
