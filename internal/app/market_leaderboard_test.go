package app

import (
	"context"
	"net/url"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestLeaderboardStampIsTheNewestWriteInUTC: the page's "figures
// last gathered" line comes from the newest updated_at among the
// region's rows, printed in UTC, and a region nobody has swept yet
// has neither rows nor a stamp.
func TestLeaderboardStampIsTheNewestWriteInUTC(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	const forge, domain = int64(10000002), int64(10000043)
	for _, row := range []db.UpsertMarketStationLeaderboardParams{
		{RegionID: forge, LocationID: 60003760, SellOrders: 10, BuyOrders: 5, SellValue: 1000, BuyValue: 500,
			UpdatedAt: mustTime("2026-10-01T12:30:00Z")},
		// Written later, and with a zone offset: 14:45 at +02:00 is
		// 12:45 UTC, which is what the page has to say.
		{RegionID: forge, LocationID: 60003761, SellOrders: 3, BuyOrders: 1, SellValue: 300, BuyValue: 100,
			UpdatedAt: mustTime("2026-10-01T14:45:00+02:00")},
	} {
		if err := q.UpsertMarketStationLeaderboard(ctx, row); err != nil {
			t.Fatalf("seed leaderboard row: %v", err)
		}
	}

	view := app.buildLeaderboardView(ctx, url.Values{"region": {"10000002"}})
	if !view.HasData || len(view.Rows) != 2 {
		t.Fatalf("The Forge: HasData=%v with %d rows, want 2 rows", view.HasData, len(view.Rows))
	}
	if want := "Figures last gathered Oct 1, 12:45 PM"; view.AsOf != want {
		t.Errorf("AsOf = %q, want %q", view.AsOf, want)
	}

	empty := app.buildLeaderboardView(ctx, url.Values{"region": {"10000043"}})
	if empty.RegionID != domain {
		t.Fatalf("region = %d, want Domain", empty.RegionID)
	}
	if empty.HasData || len(empty.Rows) != 0 || empty.AsOf != "" {
		t.Errorf("a region with no rows: HasData=%v, %d rows, AsOf=%q", empty.HasData, len(empty.Rows), empty.AsOf)
	}
}
