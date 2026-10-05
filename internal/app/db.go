package app

import (
	"database/sql"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers as "sqlite"
)

// openDB opens the SQLite database at path and applies the embedded
// schemas on first boot (001 on an empty database, 002 when the
// snapshot tables are absent, 003 for the SDE tables, 004 for the
// killmail detail store, 005 for the corporation tables), plus the
// sessions table the scs sqlite3store expects.
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		return nil, err
	}
	// The scs sqlite3store expects a sessions table; create it if needed.
	// (users/characters live in schema/001_init.sql and are managed via sqlc.)
	_, err = conn.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		data BLOB NOT NULL,
		expiry REAL NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	_, err = conn.Exec(`CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions (expiry)`)
	if err != nil {
		return nil, err
	}
	// Apply the initial schema on first boot: a fresh DB_PATH has no
	// users/characters tables, and nothing else in the app creates them.
	// This is one-time creation of the sqlc-managed schema, not a
	// migration framework — later schema changes need new schema files.
	var usersTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&usersTables); err != nil {
		return nil, err
	}
	if usersTables == 0 {
		if err := applySchema(conn, initSchema); err != nil {
			return nil, err
		}
	}
	// Schema 002 (snapshot + type-name caches), applied the same
	// guarded way: only when its tables don't exist yet. On an
	// existing database this adds the new tables alongside the old
	// ones without disturbing them.
	var snapshotTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'character_snapshots'`).Scan(&snapshotTables); err != nil {
		return nil, err
	}
	if snapshotTables == 0 {
		if err := applySchema(conn, snapshotsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 003 (SDE static data), applied the same guarded way.
	var sdeTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_types'`).Scan(&sdeTables); err != nil {
		return nil, err
	}
	if sdeTables == 0 {
		if err := applySchema(conn, sdeSchema); err != nil {
			return nil, err
		}
	}
	// Schema 004 (module sweep: killmail detail store), applied the
	// same guarded way.
	var killmailTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'killmail_details'`).Scan(&killmailTables); err != nil {
		return nil, err
	}
	if killmailTables == 0 {
		if err := applySchema(conn, moduleSweepSchema); err != nil {
			return nil, err
		}
	}
	// Schema 005 (corporation cluster: fetch-state log, character →
	// corporation map, item names), applied the same guarded way.
	var corpTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'snapshot_fetch_state'`).Scan(&corpTables); err != nil {
		return nil, err
	}
	if corpTables == 0 {
		if err := applySchema(conn, corpSchema); err != nil {
			return nil, err
		}
	}
	// Schema 006 (economy cluster: contract detail store), applied
	// the same guarded way.
	var contractTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'contract_details'`).Scan(&contractTables); err != nil {
		return nil, err
	}
	if contractTables == 0 {
		if err := applySchema(conn, economySchema); err != nil {
			return nil, err
		}
	}
	// Schema 007 (intel cluster: global public-data store + war
	// detail store), applied the same guarded way.
	var intelTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'global_snapshots'`).Scan(&intelTables); err != nil {
		return nil, err
	}
	if intelTables == 0 {
		if err := applySchema(conn, intelSchema); err != nil {
			return nil, err
		}
	}
	// Schema 008 (marketable SDE types: market group + published
	// flag), guarded on the columns themselves: an existing
	// database gets the ALTERs, a new one gets them right after
	// 003 creates the base table.
	var marketCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sde_types') WHERE name IN ('market_group_id', 'published')`).Scan(&marketCols); err != nil {
		return nil, err
	}
	if marketCols < 2 {
		if err := applySchema(conn, marketableSchema); err != nil {
			return nil, err
		}
	}
	// Schema 009 (multi-character foundation: owner hash, tags,
	// link state on characters), guarded on the columns
	// themselves like 008.
	var foundationCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('characters') WHERE name IN ('owner_hash', 'tags', 'link_state', 'link_state_at')`).Scan(&foundationCols); err != nil {
		return nil, err
	}
	if foundationCols < 4 {
		if err := applySchema(conn, characterFoundationSchema); err != nil {
			return nil, err
		}
	}
	// Schema 010 (Phase 1B widget home: per-account home layout
	// JSON). Column-existence guard, same as schema 009's columns.
	var homeLayoutCols int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name IN ('home_layout')`,
	).Scan(&homeLayoutCols); err != nil {
		return nil, err
	}
	if homeLayoutCols < 1 {
		if err := applySchema(conn, homeLayoutSchema); err != nil {
			return nil, err
		}
	}
	// Schema 011 (Phase 3 industry planner: blueprint static
	// data), applied the same guarded way: only when its table
	// doesn't exist yet.
	var plannerTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_blueprints'`).Scan(&plannerTables); err != nil {
		return nil, err
	}
	if plannerTables == 0 {
		if err := applySchema(conn, industryPlannerSchema); err != nil {
			return nil, err
		}
	}
	// Schema 012 (Phase 4 skill plans: dogma skill graph +
	// user plans), applied the same guarded way.
	var skillPlanTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_skill_meta'`).Scan(&skillPlanTables); err != nil {
		return nil, err
	}
	if skillPlanTables == 0 {
		if err := applySchema(conn, skillPlansSchema); err != nil {
			return nil, err
		}
	}
	// Schema 013 (Phase 5 market history + alerts: history rows,
	// wants, fetch state, watchlist, order health), applied the
	// same guarded way.
	var marketHistoryTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'market_history'`).Scan(&marketHistoryTables); err != nil {
		return nil, err
	}
	if marketHistoryTables == 0 {
		if err := applySchema(conn, marketHistorySchema); err != nil {
			return nil, err
		}
	}
	// Schema 014 (player structure-name resolution queue), applied
	// the same guarded way.
	var structureNameTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'structure_names'`).Scan(&structureNameTables); err != nil {
		return nil, err
	}
	if structureNameTables == 0 {
		if err := applySchema(conn, structureNamesSchema); err != nil {
			return nil, err
		}
	}
	// Schema 015 (public pilot records + item type details: the
	// wants queues behind /pilot/ and the item details page),
	// applied the same guarded way.
	var pilotTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pilot_records'`).Scan(&pilotTables); err != nil {
		return nil, err
	}
	if pilotTables == 0 {
		if err := applySchema(conn, pilotRecordsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 016 (pilot drain priority: viewed wants outrank the
	// proactively noted orbit), guarded on the column itself like
	// 008/009.
	var pilotPriorityCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pilot_records') WHERE name = 'priority'`).Scan(&pilotPriorityCols); err != nil {
		return nil, err
	}
	if pilotPriorityCols < 1 {
		if err := applySchema(conn, pilotPrioritySchema); err != nil {
			return nil, err
		}
	}
	// Schema 017 (Phase 6 briefing: the account's last-looked
	// anchor for the home digest), column-guarded like 010.
	var briefingAnchorCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'last_briefing_at'`).Scan(&briefingAnchorCols); err != nil {
		return nil, err
	}
	if briefingAnchorCols < 1 {
		if err := applySchema(conn, briefingAnchorSchema); err != nil {
			return nil, err
		}
	}
	// Schema 018 (item descriptions bulk-cached from the SDE
	// invTypes dump), column-guarded like 008/009: every database
	// that lacks the column — fresh ones included, since 003
	// predates it — gains it here.
	var sdeDescriptionCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sde_types') WHERE name = 'description'`).Scan(&sdeDescriptionCols); err != nil {
		return nil, err
	}
	if sdeDescriptionCols < 1 {
		if err := applySchema(conn, sdeTypeDescriptionsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 019 (daily wallet-history sampler: one balance row
	// per character per day, written by the worker from snapshots
	// it already keeps), applied the same guarded way.
	var walletHistoryTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'wallet_history'`).Scan(&walletHistoryTables); err != nil {
		return nil, err
	}
	if walletHistoryTables == 0 {
		if err := applySchema(conn, walletHistorySchema); err != nil {
			return nil, err
		}
	}
	// Schema 020 (per-widget configuration: the orders widget's
	// scope + merge mode first), applied the same guarded way.
	var widgetConfigTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'widget_configs'`).Scan(&widgetConfigTables); err != nil {
		return nil, err
	}
	if widgetConfigTables == 0 {
		if err := applySchema(conn, widgetConfigsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 021 (stored market guide for always-on asset
	// valuation), applied the same guarded way.
	var guidePricesTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'guide_prices'`).Scan(&guidePricesTables); err != nil {
		return nil, err
	}
	if guidePricesTables == 0 {
		if err := applySchema(conn, guidePricesSchema); err != nil {
			return nil, err
		}
	}
	// Schema 022 (pilot name-resolution wants: topbar searches
	// for pilots nobody has warmed yet), applied the same guarded
	// way.
	var pilotNameWantTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pilot_name_wants'`).Scan(&pilotNameWantTables); err != nil {
		return nil, err
	}
	if pilotNameWantTables == 0 {
		if err := applySchema(conn, pilotNameWantsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 023 (v0.3.08 structure-name provenance: every cached
	// structure name records whether ESI, a corp structure list,
	// or—later—a community dataset provided it), column-guarded
	// like 016–018.
	var structureSourceCols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('structure_names') WHERE name = 'source'`).Scan(&structureSourceCols); err != nil {
		return nil, err
	}
	if structureSourceCols < 1 {
		if err := applySchema(conn, structureNameProvenanceSchema); err != nil {
			return nil, err
		}
	}
	// Schema 024 (v0.3.10 market category tree: the
	// invMarketGroups browse hierarchy), applied the same guarded
	// way: only when its table doesn't exist yet.
	var marketGroupTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_market_groups'`).Scan(&marketGroupTables); err != nil {
		return nil, err
	}
	if marketGroupTables == 0 {
		if err := applySchema(conn, marketGroupsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 025 (v0.3.11 planet names: the durable resolution
	// queue behind the PI surfaces, so "Planet #<id>" fallbacks
	// resolve in the background via the public planets endpoint
	// and stay resolved across restarts), applied the same
	// guarded way.
	var planetNameTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'planet_names'`).Scan(&planetNameTables); err != nil {
		return nil, err
	}
	if planetNameTables == 0 {
		if err := applySchema(conn, planetNamesSchema); err != nil {
			return nil, err
		}
	}
	// Schema 026 (v0.3.12 public corporation & alliance records:
	// the wants queues behind /corporation/ and /alliance/,
	// mirroring pilot_records), applied the same guarded way.
	var orgRecordTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'corporation_records'`).Scan(&orgRecordTables); err != nil {
		return nil, err
	}
	if orgRecordTables == 0 {
		if err := applySchema(conn, orgRecordsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 027 (v0.3.14 guide-price wants: the durable note a
	// kill view leaves when it has no prices to value with, so
	// the worker refreshes the stored guide on the urgent tick
	// instead of kill values waiting on a Market visit), applied
	// the same guarded way.
	var guidePriceWantTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'guide_price_wants'`).Scan(&guidePriceWantTables); err != nil {
		return nil, err
	}
	if guidePriceWantTables == 0 {
		if err := applySchema(conn, guidePriceWantsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 028 (v0.3.16 structure context: owner/system/type
	// facts distilled from corporation structure snapshots behind
	// the structure page), applied the same guarded way.
	var structureContextTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'structure_context'`).Scan(&structureContextTables); err != nil {
		return nil, err
	}
	if structureContextTables == 0 {
		if err := applySchema(conn, structureContextSchema); err != nil {
			return nil, err
		}
	}
	// Schema 029 (v0.3.21 dogma fitting data: full type attributes,
	// attribute types, effects + decoded modifiers, type effects),
	// applied the same guarded way.
	var dogmaFittingTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sde_type_attributes'`).Scan(&dogmaFittingTables); err != nil {
		return nil, err
	}
	if dogmaFittingTables == 0 {
		if err := applySchema(conn, dogmaFittingSchema); err != nil {
			return nil, err
		}
	}
	// Schema 030 (v0.3.21 local fittings: fits built in the fitting
	// simulator, stored per user), applied the same guarded way.
	var localFittingTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'local_fittings'`).Scan(&localFittingTables); err != nil {
		return nil, err
	}
	if localFittingTables == 0 {
		if err := applySchema(conn, localFittingsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 031 (P1 region stats platform: per-(region, type)
	// book statistics from the worker's whole-region sweeps,
	// plus their once-a-day snapshots), applied the same
	// guarded way.
	var regionStatsTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'market_region_stats'`).Scan(&regionStatsTables); err != nil {
		return nil, err
	}
	if regionStatsTables == 0 {
		if err := applySchema(conn, regionStatsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 032 (P2 spread scanner: per-(station, type) book
	// statistics from the same whole-region sweeps as 031), applied
	// the same guarded way.
	var stationStatsTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'market_station_stats'`).Scan(&stationStatsTables); err != nil {
		return nil, err
	}
	if stationStatsTables == 0 {
		if err := applySchema(conn, stationStatsSchema); err != nil {
			return nil, err
		}
	}
	// Schema 033 (P4 own-order archaeology: append-only per-order
	// lifecycle ledger distilled from order snapshots), applied
	// the same guarded way.
	var orderLifecycleTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'order_lifecycle'`).Scan(&orderLifecycleTables); err != nil {
		return nil, err
	}
	if orderLifecycleTables == 0 {
		if err := applySchema(conn, orderLifecycleSchema); err != nil {
			return nil, err
		}
	}
	// Schema 034 (P1 sweep upgrade: disk-staged whole-region
	// sweeps -- one state row per region in mid-sweep plus the
	// orders its fetched pages landed), applied the same
	// guarded way.
	var sweepStagingTables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'market_sweep_state'`).Scan(&sweepStagingTables); err != nil {
		return nil, err
	}
	if sweepStagingTables == 0 {
		if err := applySchema(conn, sweepStagingSchema); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

// statement (the modernc driver Exec handles one statement at a time).
// Full-line -- comments are stripped first: they may contain ";" and
// would otherwise break the naive split.
func applySchema(conn *sql.DB, script string) error {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
