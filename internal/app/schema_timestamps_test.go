package app

// Tests for the steps that turned the TEXT time columns into
// timestamptz (009 onwards): an install from before them, holding
// every shape of value the app used to write, comes through with
// the same instants; the columns have the type and nullability the
// step promises; and what the database hands back is in UTC.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/pgtest"
)

// openBeforeSchemaStep opens a fresh database with the schema brought
// up to, but not including, the given step: an install that has yet
// to run it. The handle is a plain one; openDB would finish the job.
func openBeforeSchemaStep(t *testing.T, dsn string, version int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, step := range schemaSteps() {
		if step.version >= version {
			break
		}
		if err := applySchemaStep(ctx, conn, step); err != nil {
			t.Fatalf("schema step %03d: %v", step.version, err)
		}
	}
	return conn
}

// timeColumn is one converted column and whether it may be NULL
// (it may where an empty string used to mean "never").
type timeColumn struct {
	table, column string
	nullable      bool
}

// accountTimeColumns are the columns step 009 converts.
var accountTimeColumns = []timeColumn{
	{"users", "created_at", false},
	{"users", "last_briefing_at", true},
	{"characters", "token_expiry", true},
	{"characters", "cached_until", true},
	{"characters", "created_at", false},
	{"characters", "updated_at", false},
	{"characters", "link_state_at", true},
	{"character_snapshots", "fetched_at", false},
	{"character_snapshots", "cached_until", true},
	{"global_snapshots", "fetched_at", false},
	{"global_snapshots", "cached_until", false},
	{"killmail_details", "fetched_at", false},
	{"contract_details", "fetched_at", false},
	{"war_details", "fetched_at", false},
	{"snapshot_fetch_state", "attempted_at", false},
	{"character_corporations", "updated_at", false},
	{"skill_plans", "created_at", false},
	{"widget_configs", "updated_at", false},
	{"wallet_history", "sampled_at", false},
	{"local_fittings", "created_at", false},
	{"local_fittings", "updated_at", false},
}

// marketTimeColumns are the columns step 010 converts.
var marketTimeColumns = []timeColumn{
	{"guide_prices_meta", "fetched_at", false},
	{"guide_prices_meta", "cached_until", false},
	{"guide_price_wants", "wanted_at", false},
	{"market_history_wants", "last_requested_at", false},
	{"market_fetch_state", "attempted_at", false},
	{"market_watchlist", "created_at", false},
	{"order_health", "computed_at", false},
	{"order_lifecycle", "first_seen_at", false},
	{"order_lifecycle", "last_seen_at", false},
	{"order_lifecycle", "closed_at", true},
	{"market_region_stats", "updated_at", false},
	{"market_station_stats", "updated_at", false},
	{"market_sweep_state", "started_at", false},
	{"market_sweep_state", "updated_at", false},
	{"market_station_leaderboard", "updated_at", false},
}

// recordTimeColumns are the columns step 011 converts.
var recordTimeColumns = []timeColumn{
	{"structure_names", "resolved_at", true},
	{"structure_context", "updated_at", false},
	{"planet_names", "resolved_at", true},
	{"pilot_records", "fetched_at", true},
	{"corporation_records", "fetched_at", true},
	{"alliance_records", "fetched_at", true},
	{"pilot_name_wants", "requested_at", false},
	{"pilot_name_wants", "resolved_at", true},
	{"pilot_name_wants", "next_try_at", true},
	{"type_details", "fetched_at", true},
}

func checkTimeColumns(t *testing.T, conn *sql.DB, columns []timeColumn) {
	t.Helper()
	for _, col := range columns {
		var dataType, isNullable string
		err := conn.QueryRow(
			`SELECT data_type, is_nullable FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
			col.table, col.column,
		).Scan(&dataType, &isNullable)
		if err != nil {
			t.Errorf("%s.%s: %v", col.table, col.column, err)
			continue
		}
		if dataType != "timestamp with time zone" {
			t.Errorf("%s.%s is %s, want timestamp with time zone", col.table, col.column, dataType)
		}
		if got := isNullable == "YES"; got != col.nullable {
			t.Errorf("%s.%s nullable = %v, want %v", col.table, col.column, got, col.nullable)
		}
	}
}

// TestSchemaTimeColumnTypes: on a fresh database every converted
// column is a timestamptz, nullable exactly where "never" is a
// possible answer.
func TestSchemaTimeColumnTypes(t *testing.T) {
	conn, pool, err := openDB(context.Background(), pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	checkTimeColumns(t, conn, accountTimeColumns)
	checkTimeColumns(t, conn, marketTimeColumns)
	checkTimeColumns(t, conn, recordTimeColumns)

	// And none was missed: nothing named like a time is still text.
	// (The calendar days the market history and wallet history are
	// keyed by are dates, not times, and are named accordingly.)
	rows, err := conn.Query(
		`SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND data_type = 'text'
		   AND (column_name LIKE '%\_at' OR column_name LIKE '%\_until' OR column_name LIKE '%\_expiry')
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("list text columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("%s.%s is named like a time but is still text", table, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list text columns: %v", err)
	}
}

// TestSchemaTimestampStepKeepsStoredTimes: an install from before
// step 009 holds its times as text, in each of the shapes the app
// wrote. After the step every one of them is the same instant, an
// empty string has become NULL, and a value that never was a time
// has not stopped the upgrade.
func TestSchemaTimestampStepKeepsStoredTimes(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	old := openBeforeSchemaStep(t, dsn, 9)

	for _, stmt := range []string{
		// The column default wrote created_at without a zone, in UTC.
		`INSERT INTO users (id, created_at, last_briefing_at) VALUES
		   (101, '2026-03-04 05:06:07', ''),
		   (102, '2026-03-04 05:06:07', '2026-10-01T12:00:00Z')`,
		// One character that has never had a token or a link change,
		// one whose expiry was written with a zone offset.
		`INSERT INTO characters (character_id, user_id, name, token_expiry, cached_until, created_at, updated_at, link_state_at) VALUES
		   (90000001, 101, 'Alpha', NULL, NULL, '2026-03-04 05:06:07', '2026-03-05 00:00:00', NULL),
		   (90000002, 102, 'Beta', '2026-10-01T14:20:00+02:00', '1970-01-01T00:00:00Z', '2026-03-04 05:06:07', '2026-03-05 00:00:00', '2026-10-02T00:00:00Z')`,
		`INSERT INTO character_snapshots (character_id, kind, payload, fetched_at, cached_until) VALUES
		   (90000001, 'skills', '{}', '2026-10-01T12:00:00Z', '2026-10-01T13:00:00Z'),
		   (90000001, 'wallet', '0', '2026-10-01T12:00:00Z', NULL),
		   (90000001, 'assets', '[]', 'not a time', '')`,
		`INSERT INTO global_snapshots (kind, payload, fetched_at, cached_until) VALUES
		   ('incursions', '[]', '2026-10-01T12:00:00Z', '2026-10-01T12:05:00Z')`,
		`INSERT INTO snapshot_fetch_state (character_id, kind, state, attempted_at) VALUES
		   (90000001, 'skills', 'ok', '2026-10-01T12:00:00Z')`,
		`INSERT INTO wallet_history (user_id, character_id, day, balance, sampled_at) VALUES
		   (101, 90000001, '2026-10-01', 5, '2026-10-01T23:59:59Z')`,
		// A fit saved by the editor, and one left on the old defaults.
		`INSERT INTO local_fittings (id, user_id, name, created_at, updated_at) VALUES
		   (1, 101, 'saved', '2026-09-01T00:00:00Z', '2026-09-02T00:00:00Z')`,
		`INSERT INTO local_fittings (id, user_id, name) VALUES (2, 101, 'defaults')`,
	} {
		if _, err := old.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed the old schema: %v\n%s", err, stmt)
		}
	}
	old.Close()

	conn, pool, err := openDB(ctx, dsn)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	q := db.New(conn)
	checkTimeColumns(t, conn, accountTimeColumns)

	same := func(what string, got time.Time, want string) {
		t.Helper()
		if !got.Equal(mustTime(want)) {
			t.Errorf("%s = %s, want %s", what, rfc3339(got), want)
		}
	}
	sameOrNever := func(what string, got sql.NullTime, want string) {
		t.Helper()
		switch {
		case want == "" && got.Valid:
			t.Errorf("%s = %s, want it unset", what, rfc3339(got.Time))
		case want != "" && !got.Valid:
			t.Errorf("%s is unset, want %s", what, want)
		case want != "":
			same(what, got.Time, want)
		}
	}
	const epoch = "1970-01-01T00:00:00Z"

	never, err := q.GetUser(ctx, 101)
	if err != nil {
		t.Fatalf("read user 101: %v", err)
	}
	same("user 101 created_at", never.CreatedAt, "2026-03-04T05:06:07Z")
	sameOrNever("user 101 last_briefing_at", never.LastBriefingAt, "")
	briefed, err := q.GetUser(ctx, 102)
	if err != nil {
		t.Fatalf("read user 102: %v", err)
	}
	sameOrNever("user 102 last_briefing_at", briefed.LastBriefingAt, "2026-10-01T12:00:00Z")

	alpha, err := q.GetCharacter(ctx, 90000001)
	if err != nil {
		t.Fatalf("read character Alpha: %v", err)
	}
	sameOrNever("Alpha token_expiry", alpha.TokenExpiry, "")
	sameOrNever("Alpha cached_until", alpha.CachedUntil, "")
	sameOrNever("Alpha link_state_at", alpha.LinkStateAt, "")
	same("Alpha created_at", alpha.CreatedAt, "2026-03-04T05:06:07Z")
	same("Alpha updated_at", alpha.UpdatedAt, "2026-03-05T00:00:00Z")
	beta, err := q.GetCharacter(ctx, 90000002)
	if err != nil {
		t.Fatalf("read character Beta: %v", err)
	}
	sameOrNever("Beta token_expiry", beta.TokenExpiry, "2026-10-01T12:20:00Z")
	sameOrNever("Beta cached_until", beta.CachedUntil, epoch)
	sameOrNever("Beta link_state_at", beta.LinkStateAt, "2026-10-02T00:00:00Z")

	snapshots, err := q.ListSnapshotMetaByCharacter(ctx, 90000001)
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	wantSnapshots := map[string][2]string{
		"skills": {"2026-10-01T12:00:00Z", "2026-10-01T13:00:00Z"},
		"wallet": {"2026-10-01T12:00:00Z", ""},
		// Unreadable: as old as it gets, and no cache window.
		"assets": {epoch, ""},
	}
	if len(snapshots) != len(wantSnapshots) {
		t.Fatalf("%d snapshots after the upgrade, want %d", len(snapshots), len(wantSnapshots))
	}
	for _, snap := range snapshots {
		want := wantSnapshots[snap.Kind]
		same(snap.Kind+" fetched_at", snap.FetchedAt, want[0])
		sameOrNever(snap.Kind+" cached_until", snap.CachedUntil, want[1])
	}

	global, err := q.GetGlobalSnapshot(ctx, "incursions")
	if err != nil {
		t.Fatalf("read global snapshot: %v", err)
	}
	same("global fetched_at", global.FetchedAt, "2026-10-01T12:00:00Z")
	same("global cached_until", global.CachedUntil, "2026-10-01T12:05:00Z")

	state, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: 90000001, Kind: "skills"})
	if err != nil {
		t.Fatalf("read fetch state: %v", err)
	}
	same("fetch state attempted_at", state.AttemptedAt, "2026-10-01T12:00:00Z")

	sample, err := q.GetWalletHistorySample(ctx, db.GetWalletHistorySampleParams{UserID: 101, CharacterID: 90000001, Day: "2026-10-01"})
	if err != nil {
		t.Fatalf("read wallet sample: %v", err)
	}
	same("wallet sampled_at", sample.SampledAt, "2026-10-01T23:59:59Z")

	saved, err := q.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: 1, UserID: 101})
	if err != nil {
		t.Fatalf("read saved fit: %v", err)
	}
	same("saved fit created_at", saved.CreatedAt, "2026-09-01T00:00:00Z")
	same("saved fit updated_at", saved.UpdatedAt, "2026-09-02T00:00:00Z")
	defaults, err := q.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: 2, UserID: 101})
	if err != nil {
		t.Fatalf("read the fit left on defaults: %v", err)
	}
	same("default fit created_at", defaults.CreatedAt, epoch)
	same("default fit updated_at", defaults.UpdatedAt, epoch)

	// The defaults that wrote text now write a time.
	created, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create a user after the upgrade: %v", err)
	}
	// The database's clock and this process's are read separately
	// (and at different resolutions), so "now" is within a minute
	// either way, not "not after".
	if age := time.Since(created.CreatedAt); age.Abs() > time.Minute {
		t.Errorf("a new user's created_at is %s, want now", rfc3339(created.CreatedAt))
	}
}

// TestSchemaMarketTimestampStepKeepsStoredTimes: the same upgrade
// for the market tables (step 010). The one column that could say
// "never" is an order's closed_at: an open order held an empty
// string there and holds NULL now, and everything that picked open
// or closed orders by that still picks the same ones, in the same
// order.
func TestSchemaMarketTimestampStepKeepsStoredTimes(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	old := openBeforeSchemaStep(t, dsn, 10)

	for _, stmt := range []string{
		`INSERT INTO users (id) VALUES (101)`,
		`INSERT INTO characters (character_id, user_id, name) VALUES (90000001, 101, 'Alpha')`,
		// Order 1 is still open, 2 closed first, 3 closed last.
		`INSERT INTO order_lifecycle (character_id, order_id, type_id, location_id, region_id, first_seen_at, last_seen_at, closed_at, close_kind) VALUES
		   (90000001, 1, 34, 60003760, 10000002, '2026-09-29T00:00:00Z', '2026-10-01T00:00:00Z', '', ''),
		   (90000001, 2, 34, 60003760, 10000002, '2026-09-20T00:00:00Z', '2026-09-21T00:00:00Z', '2026-09-21T06:00:00Z', 'filled'),
		   (90000001, 3, 34, 60003760, 10000002, '2026-09-25T00:00:00Z', '2026-09-28T00:00:00Z', '2026-09-28T12:00:00Z', 'ended')`,
		`INSERT INTO guide_prices_meta (id, fetched_at, cached_until) VALUES
		   (1, '2026-10-01T12:00:00Z', '2026-10-01T13:00:00Z')`,
		`INSERT INTO market_fetch_state (kind, state, attempted_at) VALUES
		   ('history_10000002_34', 'ok', '2026-10-01T12:00:00Z')`,
		`INSERT INTO market_history_wants (region_id, type_id, last_requested_at) VALUES
		   (10000002, 34, '2026-10-01T12:00:00Z')`,
		`INSERT INTO market_watchlist (user_id, type_id, region_id, created_at) VALUES
		   (101, 34, 10000002, '2026-08-01T00:00:00Z')`,
		`INSERT INTO market_region_stats (region_id, type_id, updated_at) VALUES
		   (10000002, 34, '2026-10-01T12:30:00Z'),
		   (10000002, 35, '2026-10-01T12:45:00Z')`,
		`INSERT INTO market_sweep_state (region_id, started_at, updated_at) VALUES
		   (10000002, '2026-10-01T12:00:00Z', '2026-10-01T12:10:00Z')`,
	} {
		if _, err := old.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed the old schema: %v\n%s", err, stmt)
		}
	}
	old.Close()

	conn, pool, err := openDB(ctx, dsn)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	q := db.New(conn)
	checkTimeColumns(t, conn, marketTimeColumns)

	same := func(what string, got time.Time, want string) {
		t.Helper()
		if !got.Equal(mustTime(want)) {
			t.Errorf("%s = %s, want %s", what, rfc3339(got), want)
		}
	}

	open, err := q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: 90000001, OrderID: 1})
	if err != nil {
		t.Fatalf("read the open order: %v", err)
	}
	if open.ClosedAt.Valid {
		t.Errorf("the open order reads as closed at %s", rfc3339(open.ClosedAt.Time))
	}
	same("open order first_seen_at", open.FirstSeenAt, "2026-09-29T00:00:00Z")
	same("open order last_seen_at", open.LastSeenAt, "2026-10-01T00:00:00Z")
	closed, err := q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: 90000001, OrderID: 2})
	if err != nil {
		t.Fatalf("read a closed order: %v", err)
	}
	if !closed.ClosedAt.Valid {
		t.Fatal("a closed order reads as open")
	}
	same("closed order closed_at", closed.ClosedAt.Time, "2026-09-21T06:00:00Z")

	orderIDs := func(rows []db.OrderLifecycle) []int64 {
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = row.OrderID
		}
		return ids
	}
	wantIDs := func(what string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: orders %v, want %v", what, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: orders %v, want %v", what, got, want)
				return
			}
		}
	}
	stillOpen, err := q.ListOpenOrderLifecycleByCharacter(ctx, 90000001)
	if err != nil {
		t.Fatalf("list open orders: %v", err)
	}
	wantIDs("open orders", orderIDs(stillOpen), 1)
	done, err := q.ListClosedOrderLifecycleByCharacter(ctx, db.ListClosedOrderLifecycleByCharacterParams{CharacterID: 90000001, RowLimit: 10})
	if err != nil {
		t.Fatalf("list closed orders: %v", err)
	}
	wantIDs("closed orders, newest first", orderIDs(done), 3, 2)
	// Newest close first, the open order after every closed one —
	// where '' used to sort.
	all, err := q.ListOrderLifecycleByUser(ctx, 101)
	if err != nil {
		t.Fatalf("list a user's orders: %v", err)
	}
	wantIDs("a user's orders", orderIDs(all), 3, 2, 1)

	// Closing only ever touches an open order, and pruning only a
	// closed one older than the cutoff.
	reclose := db.CloseOrderLifecycleParams{
		ClosedAt: mustTime("2026-10-05T00:00:00Z"), CloseKind: "ended", CharacterID: 90000001, OrderID: 2,
	}
	if err := q.CloseOrderLifecycle(ctx, reclose); err != nil {
		t.Fatalf("close an already closed order: %v", err)
	}
	if again, err := q.GetOrderLifecycle(ctx, db.GetOrderLifecycleParams{CharacterID: 90000001, OrderID: 2}); err != nil {
		t.Fatalf("re-read the closed order: %v", err)
	} else {
		same("an already closed order's closed_at", again.ClosedAt.Time, "2026-09-21T06:00:00Z")
	}
	if err := q.PruneOldOrderLifecycle(ctx, db.PruneOldOrderLifecycleParams{
		ClosedBefore: mustTime("2026-09-25T00:00:00Z"), RowLimit: 10,
	}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	left, err := q.ListOrderLifecycleByUser(ctx, 101)
	if err != nil {
		t.Fatalf("list after the prune: %v", err)
	}
	wantIDs("orders after pruning those closed before the 25th", orderIDs(left), 3, 1)

	meta, err := q.GetGuidePricesMeta(ctx)
	if err != nil {
		t.Fatalf("read the price guide's bookkeeping: %v", err)
	}
	same("price guide fetched_at", meta.FetchedAt, "2026-10-01T12:00:00Z")
	same("price guide cached_until", meta.CachedUntil, "2026-10-01T13:00:00Z")

	state, err := q.GetMarketFetchState(ctx, "history_10000002_34")
	if err != nil {
		t.Fatalf("read market fetch state: %v", err)
	}
	same("market fetch attempted_at", state.AttemptedAt, "2026-10-01T12:00:00Z")

	// A cutoff is compared as a time now, not as text.
	for cutoff, want := range map[string]int{"2026-10-01T12:00:00Z": 1, "2026-10-01T12:00:01Z": 0} {
		wants, err := q.ListMarketHistoryWants(ctx, mustTime(cutoff))
		if err != nil {
			t.Fatalf("list history wants since %s: %v", cutoff, err)
		}
		if len(wants) != want {
			t.Errorf("%d history want(s) since %s, want %d", len(wants), cutoff, want)
		}
	}

	entry, err := q.GetWatchlistEntry(ctx, db.GetWatchlistEntryParams{UserID: 101, TypeID: 34, RegionID: 10000002})
	if err != nil {
		t.Fatalf("read watchlist entry: %v", err)
	}
	same("watchlist created_at", entry.CreatedAt, "2026-08-01T00:00:00Z")

	// The newest write in a region is still found, as a time.
	stats, err := q.GetMarketRegionStatsStamp(ctx, 10000002)
	if err != nil {
		t.Fatalf("read the region's newest stat: %v", err)
	}
	if newest, ok := stats.Stamp.(time.Time); !ok {
		t.Errorf("the region's newest stat came back as %T, want a time", stats.Stamp)
	} else {
		same("region's newest stat", newest, "2026-10-01T12:45:00Z")
	}
	if stats.RowCount != 2 {
		t.Errorf("%d region stats, want 2", stats.RowCount)
	}
	// And with no rows at all there is no stamp, not an error.
	none, err := q.GetMarketRegionStatsStamp(ctx, 10000043)
	if err != nil {
		t.Fatalf("read the newest stat of a region with none: %v", err)
	}
	if none.Stamp != nil || none.RowCount != 0 {
		t.Errorf("a region with no stats reports %v over %d rows", none.Stamp, none.RowCount)
	}

	sweep, err := q.GetMarketSweepState(ctx, 10000002)
	if err != nil {
		t.Fatalf("read sweep state: %v", err)
	}
	same("sweep started_at", sweep.StartedAt, "2026-10-01T12:00:00Z")
	same("sweep updated_at", sweep.UpdatedAt, "2026-10-01T12:10:00Z")
}

// TestSchemaRecordTimestampStepKeepsStoredTimes: the same upgrade
// for the name and record caches (step 011). Most of these rows
// exist before they are filled in, so "not yet" is the common case
// here: it was an empty string and is NULL now, and each queue still
// hands the worker the same rows in the same order.
func TestSchemaRecordTimestampStepKeepsStoredTimes(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	old := openBeforeSchemaStep(t, dsn, 11)

	for _, stmt := range []string{
		// Structures: one waiting, one resolved long ago, one resolved
		// just now, one found missing long ago.
		`INSERT INTO structure_names (structure_id) VALUES (1001)`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at) VALUES
		   (1002, 'Old Keep', 'resolved', '2026-01-01T00:00:00Z'),
		   (1003, 'New Keep', 'resolved', '2026-10-01T00:00:00Z'),
		   (1004, '', 'missing', '2026-01-01T00:00:00Z')`,
		`INSERT INTO structure_context (structure_id, owner_corporation_id, updated_at) VALUES
		   (1002, 98000001, '2026-09-30T08:00:00Z')`,
		`INSERT INTO planet_names (planet_id) VALUES (2001)`,
		`INSERT INTO planet_names (planet_id, name, state, resolved_at) VALUES
		   (2002, 'Jita IV', 'resolved', '2026-09-30T08:00:00Z')`,
		// Pilots: two waiting (one asked for by a page), two ready
		// (one stale, one fresh), one settled as missing.
		`INSERT INTO pilot_records (character_id, priority) VALUES (3001, 0), (3002, 1)`,
		`INSERT INTO pilot_records (character_id, payload, state, fetched_at) VALUES
		   (3003, '{}', 'ready', '2026-01-01T00:00:00Z'),
		   (3004, '{}', 'ready', '2026-10-01T00:00:00Z'),
		   (3005, '', 'missing', '2026-01-01T00:00:00Z')`,
		`INSERT INTO corporation_records (corporation_id) VALUES (4001)`,
		`INSERT INTO corporation_records (corporation_id, payload, state, fetched_at) VALUES
		   (4002, '{}', 'ready', '2026-01-01T00:00:00Z')`,
		`INSERT INTO alliance_records (alliance_id) VALUES (5001)`,
		// Name lookups: one new, one that failed and may retry now,
		// one that failed and has to wait.
		`INSERT INTO pilot_name_wants (normalized_name, display_name, state, requested_at) VALUES
		   ('new pilot', 'New Pilot', 'pending', '2026-10-01T10:00:00Z')`,
		`INSERT INTO pilot_name_wants (normalized_name, display_name, state, requested_at, resolved_at, next_try_at, attempts) VALUES
		   ('retry pilot', 'Retry Pilot', 'error', '2026-10-01T09:00:00Z', '2026-10-01T09:01:00Z', '2026-10-01T09:31:00Z', 1),
		   ('wait pilot', 'Wait Pilot', 'error', '2026-10-01T08:00:00Z', '2026-10-01T11:59:00Z', '2026-10-01T12:29:00Z', 1)`,
		// Item descriptions: one asked for, one answered.
		`INSERT INTO type_details (type_id) VALUES (34)`,
		`INSERT INTO type_details (type_id, description, fetched_at) VALUES
		   (35, 'A mineral.', '2026-09-30T08:00:00Z')`,
	} {
		if _, err := old.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed the old schema: %v\n%s", err, stmt)
		}
	}
	old.Close()

	conn, pool, err := openDB(ctx, dsn)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	q := db.New(conn)
	checkTimeColumns(t, conn, recordTimeColumns)

	sameOrNever := func(what string, got sql.NullTime, want string) {
		t.Helper()
		if got := rfc3339Or(got, ""); got != want {
			t.Errorf("%s = %q, want %q", what, got, want)
		}
	}
	wantIDs := func(what string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: %v, want %v", what, got, want)
				return
			}
		}
	}
	// "Now" for the queues: every cutoff below is taken from it.
	now := mustTime("2026-10-01T12:00:00Z")

	waiting, err := q.GetStructureName(ctx, 1001)
	if err != nil {
		t.Fatalf("read the waiting structure: %v", err)
	}
	sameOrNever("waiting structure resolved_at", waiting.ResolvedAt, "")
	named, err := q.GetStructureName(ctx, 1002)
	if err != nil {
		t.Fatalf("read the named structure: %v", err)
	}
	sameOrNever("named structure resolved_at", named.ResolvedAt, "2026-01-01T00:00:00Z")
	// Waiting first, then what is past its re-check window; the one
	// resolved today is left alone.
	structures, err := q.ListStructureResolutions(ctx, db.ListStructureResolutionsParams{
		ResolvedCutoff:  now.Add(-30 * 24 * time.Hour),
		MissingCutoff:   now.Add(-24 * time.Hour),
		ResolutionLimit: 10,
	})
	if err != nil {
		t.Fatalf("list structures to resolve: %v", err)
	}
	wantIDs("structures to resolve", structures, 1001, 1002, 1004)
	context1002, err := q.GetStructureContext(ctx, 1002)
	if err != nil {
		t.Fatalf("read structure context: %v", err)
	}
	if got := rfc3339(context1002.UpdatedAt); got != "2026-09-30T08:00:00Z" {
		t.Errorf("structure context updated_at = %s", got)
	}

	planet, err := q.GetPlanetName(ctx, 2001)
	if err != nil {
		t.Fatalf("read the waiting planet: %v", err)
	}
	sameOrNever("waiting planet resolved_at", planet.ResolvedAt, "")
	planets, err := q.ListPlanetResolutions(ctx, db.ListPlanetResolutionsParams{
		ResolvedCutoff:  now.Add(-30 * 24 * time.Hour),
		MissingCutoff:   now.Add(-24 * time.Hour),
		ResolutionLimit: 10,
	})
	if err != nil {
		t.Fatalf("list planets to resolve: %v", err)
	}
	wantIDs("planets to resolve", planets, 2001)

	pending, err := q.GetPilotRecord(ctx, 3001)
	if err != nil {
		t.Fatalf("read the waiting pilot: %v", err)
	}
	sameOrNever("waiting pilot fetched_at", pending.FetchedAt, "")
	ready, err := q.GetPilotRecord(ctx, 3004)
	if err != nil {
		t.Fatalf("read the ready pilot: %v", err)
	}
	sameOrNever("ready pilot fetched_at", ready.FetchedAt, "2026-10-01T00:00:00Z")
	// Waiting rows first (the page's own request ahead of the rest),
	// then the stale record. Fresh and settled ones are not due.
	pilots, err := q.ListPilotDrains(ctx, db.ListPilotDrainsParams{StaleCutoff: now.Add(-7 * 24 * time.Hour), DrainLimit: 10})
	if err != nil {
		t.Fatalf("list pilots to fetch: %v", err)
	}
	wantIDs("pilots to fetch", pilots, 3002, 3001, 3003)
	corps, err := q.ListCorporationDrains(ctx, db.ListCorporationDrainsParams{StaleCutoff: now.Add(-7 * 24 * time.Hour), DrainLimit: 10})
	if err != nil {
		t.Fatalf("list corporations to fetch: %v", err)
	}
	wantIDs("corporations to fetch", corps, 4001, 4002)
	alliances, err := q.ListAllianceDrains(ctx, db.ListAllianceDrainsParams{StaleCutoff: now.Add(-7 * 24 * time.Hour), DrainLimit: 10})
	if err != nil {
		t.Fatalf("list alliances to fetch: %v", err)
	}
	wantIDs("alliances to fetch", alliances, 5001)

	// Oldest request first; the lookup still waiting out its retry
	// delay is not offered.
	due, err := q.ListDuePilotNameWants(ctx, db.ListDuePilotNameWantsParams{Now: now, Lim: 10})
	if err != nil {
		t.Fatalf("list due name lookups: %v", err)
	}
	if len(due) != 2 || due[0].NormalizedName != "retry pilot" || due[1].NormalizedName != "new pilot" {
		t.Errorf("due name lookups: %+v, want retry pilot then new pilot", due)
	} else {
		sameOrNever("retry lookup next_try_at", due[0].NextTryAt, "2026-10-01T09:31:00Z")
		sameOrNever("new lookup resolved_at", due[1].ResolvedAt, "")
		sameOrNever("new lookup next_try_at", due[1].NextTryAt, "")
	}
	// Settling a lookup clears its retry time again.
	if err := q.SetPilotNameWantMissing(ctx, db.SetPilotNameWantMissingParams{
		ResolvedAt: timeSet(now), NormalizedName: "retry pilot",
	}); err != nil {
		t.Fatalf("settle a lookup: %v", err)
	}
	settled, err := q.GetPilotNameWant(ctx, "retry pilot")
	if err != nil {
		t.Fatalf("re-read the settled lookup: %v", err)
	}
	sameOrNever("settled lookup resolved_at", settled.ResolvedAt, "2026-10-01T12:00:00Z")
	sameOrNever("settled lookup next_try_at", settled.NextTryAt, "")

	wanted, err := q.ListTypeDetailWants(ctx, 10)
	if err != nil {
		t.Fatalf("list wanted item descriptions: %v", err)
	}
	wantIDs("wanted item descriptions", wanted, 34)
	answered, err := q.GetTypeDetail(ctx, 35)
	if err != nil {
		t.Fatalf("read an answered item description: %v", err)
	}
	sameOrNever("answered description fetched_at", answered.FetchedAt, "2026-09-30T08:00:00Z")
}

// TestStoredTimesReadBackInUTC: whatever zone the machine or the
// database session is in, a time read from the database is in UTC,
// so every page prints EVE time without having to ask for it.
func TestStoredTimesReadBackInUTC(t *testing.T) {
	ctx := context.Background()
	conn, pool, err := openDB(ctx, pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer pool.Close()
	defer conn.Close()
	q := db.New(conn)

	// One connection, so the session zone set here is the one the
	// read below runs in.
	conn.SetMaxOpenConns(1)
	if _, err := conn.ExecContext(ctx, `SET TIME ZONE 'Asia/Tokyo'`); err != nil {
		t.Fatalf("set the session zone: %v", err)
	}
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	east := time.FixedZone("east", 9*60*60)
	written := time.Date(2026, 10, 6, 21, 0, 0, 0, east)
	if err := q.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: timeSet(written), ID: user.ID,
	}); err != nil {
		t.Fatalf("write anchor: %v", err)
	}
	got, err := q.GetUserBriefingAnchor(ctx, user.ID)
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	if !got.Valid || !got.Time.Equal(written) {
		t.Fatalf("read back %v, want the instant %v", got.Time, written)
	}
	if got.Time.Location() != time.UTC {
		t.Errorf("read back in zone %q, want UTC", got.Time.Location())
	}
	if text := got.Time.Format(time.RFC3339); text != "2026-10-06T12:00:00Z" {
		t.Errorf("printed as %q, want 2026-10-06T12:00:00Z", text)
	}
}
