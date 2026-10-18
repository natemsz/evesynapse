package store

// Tests for schema step 059 (the audit's missing indexes): a
// database that predates the step has none of them, applying
// the step adds exactly them plus the pg_trgm extension, and
// the entity_names trigram index is one the planner will
// actually use for a substring ILIKE.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"evesynapse/internal/pgtest"
)

var auditIndexNames = []string{
	"market_stats_fallback_type_idx",
	"pub_contracts_issued_idx",
	"pub_contracts_pending_items_idx",
	"pub_contracts_issuer_corp_idx",
	"pub_contracts_start_location_idx",
	"killmail_details_character_idx",
	"contract_details_character_idx",
	"pilot_records_drain_idx",
	"corporation_records_drain_idx",
	"alliance_records_drain_idx",
	"entity_names_name_trgm_idx",
}

func publicIndexCount(t *testing.T, conn *sql.DB, index string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, index,
	).Scan(&n); err != nil {
		t.Fatalf("look up index %s: %v", index, err)
	}
	return n
}

func TestAuditIndexesStep(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)

	// A database brought up to just before the step has none of
	// the indexes and no pg_trgm.
	conn := openBeforeSchemaStep(t, dsn, 59)
	for _, index := range auditIndexNames {
		if n := publicIndexCount(t, conn, index); n != 0 {
			t.Fatalf("index %s exists before step 059", index)
		}
	}
	var ext int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pg_extension WHERE extname = 'pg_trgm'`).Scan(&ext); err != nil {
		t.Fatalf("look up pg_trgm: %v", err)
	}
	if ext != 0 {
		t.Fatalf("pg_trgm present before step 059")
	}

	// Apply the step on its own, as migrateSchema does for a
	// database that is only missing this one.
	var step59 schemaStep
	found := false
	for _, s := range schemaSteps() {
		if s.version == 59 {
			step59, found = s, true
		}
	}
	if !found {
		t.Fatalf("schema step 059 not registered")
	}
	if err := applySchemaStep(ctx, conn, step59); err != nil {
		t.Fatalf("apply step 059: %v", err)
	}

	for _, index := range auditIndexNames {
		if n := publicIndexCount(t, conn, index); n != 1 {
			t.Fatalf("index %s: %d copies after step 059, want 1", index, n)
		}
	}
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pg_extension WHERE extname = 'pg_trgm'`).Scan(&ext); err != nil {
		t.Fatalf("look up pg_trgm: %v", err)
	}
	if ext != 1 {
		t.Fatalf("pg_trgm missing after step 059")
	}

	// The trigram index is what serves the substring half of the
	// contract search's issuer-name lookup (the kind half rides
	// the primary key). On one dedicated connection (so the SET
	// and the EXPLAIN share a session), with sequential scans
	// priced out, a substring ILIKE must go through the index.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO entity_names (kind, id, name) VALUES
		 ('character', 1, 'Synapse Tester'),
		 ('character', 2, 'Unrelated Pilot'),
		 ('corporation', 3, 'Synapse Holdings')`); err != nil {
		t.Fatalf("seed entity_names: %v", err)
	}
	sc, err := conn.Conn(ctx)
	if err != nil {
		t.Fatalf("dedicated conn: %v", err)
	}
	defer sc.Close()
	if _, err := sc.ExecContext(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	rows, err := sc.QueryContext(ctx,
		`EXPLAIN (COSTS OFF) SELECT id FROM entity_names WHERE name ILIKE '%synapse%'`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(plan.String(), "entity_names_name_trgm_idx") {
		t.Fatalf("plan does not use the trigram index:\n%s", plan.String())
	}
}
