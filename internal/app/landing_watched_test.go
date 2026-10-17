package app

// The landing strip's PLEX mover prices from the global market
// region (19000001), which is NOT one of the five hub regions
// the sweeps cover. Its history only ever arrives through the
// want drain, so the whole path has to carry a global-region
// want verbatim: queue, candidate build, fetch, store. Any
// step that quietly remapped or dropped a non-hub region
// would starve the strip's first mover forever — this test
// walks a (19000001, 44992) want end to end against the stub
// transport and checks the rows land under the global region.

import (
	"context"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

func TestLandingPLEXWantDrainsFromGlobalRegion(t *testing.T) {
	transport := &marketStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	if err := app.QueueHistoryWant(ctx, globalMarketRegion, 44992); err != nil {
		t.Fatalf("queue PLEX want: %v", err)
	}

	keys, err := app.historyCandidates(ctx)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	found := false
	for _, k := range keys {
		if k.TypeID != 44992 {
			continue
		}
		if k.RegionID != globalMarketRegion {
			t.Fatalf("PLEX candidate remapped to region %d, want the global market %d", k.RegionID, globalMarketRegion)
		}
		found = true
	}
	if !found {
		t.Fatalf("candidates missing the PLEX want; got %v", keys)
	}

	allowance := &fetchBudget{left: 4}
	stored, limited := app.warmMarketHistory(ctx, allowance)
	if limited {
		t.Fatal("history pass reported the error limit on a healthy stub")
	}
	if stored != 1 {
		t.Fatalf("history pass stored %d pairs, want 1 (the PLEX want)", stored)
	}
	rows, err := q.ListMarketHistory(ctx, db.ListMarketHistoryParams{
		RegionID: globalMarketRegion, TypeID: 44992, RowLimit: 90,
	})
	if err != nil || len(rows) != 2 {
		t.Fatalf("stored PLEX history: rows=%d err=%v, want 2 days under region %d", len(rows), err, globalMarketRegion)
	}
}
