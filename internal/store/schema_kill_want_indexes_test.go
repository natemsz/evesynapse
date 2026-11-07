package store

// Tests for schema step 129: applying the step adds the kills
// ingest-time index and the history-want request-time index.

import (
	"context"
	"testing"

	"evesynapse/internal/pgtest"
)

func TestKillAndWantIndexesStep(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.FreshDSN(t)
	names := []string{"kills_ingested_idx", "market_history_wants_requested_idx"}

	conn := openBeforeSchemaStep(t, dsn, 129)
	for _, index := range names {
		if n := publicIndexCount(t, conn, index); n != 0 {
			t.Fatalf("index %s exists before step 129", index)
		}
	}
	var step schemaStep
	found := false
	for _, s := range schemaSteps() {
		if s.version == 129 {
			step, found = s, true
		}
	}
	if !found {
		t.Fatalf("schema step 129 not registered")
	}
	if err := applySchemaStep(ctx, conn, step); err != nil {
		t.Fatalf("apply step 129: %v", err)
	}
	for _, index := range names {
		if n := publicIndexCount(t, conn, index); n != 1 {
			t.Fatalf("index %s: %d copies after step 129, want 1", index, n)
		}
	}
}
