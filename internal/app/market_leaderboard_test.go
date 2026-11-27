package app

import (
	"context"
	"evesynapse/internal/app/leaderboard"
	"net/url"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestLeaderboardStampIsTheNewestWriteInUTC: the page's "figures
// last gathered" line comes from the newest updated_at among the
// region's rows, printed in UTC, and a region nobody has swept yet
// has neither rows nor a stamp.
func TestLeaderboardStampIsTheNewestWriteInUTC(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4-4', 30000142), (60003761, 'Jita 4-10', 30000142)`); err != nil {
		t.Fatalf("seed stations: %v", err)
	}

	const forge, domain = int64(10000002), int64(10000043)
	for _, row := range []db.UpsertMarketStationLeaderboardParams{
		{RegionID: forge, LocationID: 60003760, SellOrders: 10, BuyOrders: 5, SellValue: 1000, BuyValue: 500,
			UpdatedAt: mustTime("2026-10-01T12:30:00Z")},
		// Written later, and with a zone offset: 14:45 at +02:00 is
		// 12:45 UTC, which is what the page has to say. Higher orders
		// than 60003760, lower value, so value vs orders sorts flip them.
		{RegionID: forge, LocationID: 60003761, SellOrders: 30, BuyOrders: 10, SellValue: 300, BuyValue: 100,
			UpdatedAt: mustTime("2026-10-01T14:45:00+02:00")},
		// Non-hub global market row must never consume a hub ranking slot.
		{RegionID: globalMarketRegion, LocationID: 60099999, SellOrders: 9999, BuyOrders: 9999, SellValue: 9e12, BuyValue: 9e12,
			UpdatedAt: mustTime("2026-10-02T00:00:00Z")},
	} {
		if err := q.UpsertMarketStationLeaderboard(ctx, row); err != nil {
			t.Fatalf("seed leaderboard row: %v", err)
		}
	}

	view := leaderboard.BuildView(ctx, app, url.Values{"region": {"10000002"}})
	if !view.HasData || len(view.Rows) != 2 {
		t.Fatalf("The Forge: HasData=%v with %d rows, want 2 rows", view.HasData, len(view.Rows))
	}
	if view.Rows[0].Station.StationID != 60003760 || view.Rows[1].Station.StationID != 60003761 {
		t.Errorf("value sort order = [%d, %d], want [60003760, 60003761]", view.Rows[0].Station.StationID, view.Rows[1].Station.StationID)
	}
	if want := "Figures last gathered Oct 1, 12:45 PM"; view.AsOf != want {
		t.Errorf("AsOf = %q, want %q", view.AsOf, want)
	}

	byOrders := leaderboard.BuildView(ctx, app, url.Values{"sort": {"orders"}})
	if len(byOrders.Rows) != 2 || byOrders.Rows[0].Station.StationID != 60003761 || byOrders.Rows[1].Station.StationID != 60003760 {
		t.Errorf("all-regions orders sort = %+v, want [60003761, 60003760] without global market", byOrders.Rows)
	}
	if want := "Figures last gathered Oct 1, 12:45 PM"; byOrders.AsOf != want {
		t.Errorf("all-regions AsOf = %q, want %q (excluding global market stamp)", byOrders.AsOf, want)
	}

	empty := leaderboard.BuildView(ctx, app, url.Values{"region": {"10000043"}})
	if empty.RegionID != domain {
		t.Fatalf("region = %d, want Domain", empty.RegionID)
	}
	if empty.HasData || len(empty.Rows) != 0 || empty.AsOf != "" {
		t.Errorf("a region with no rows: HasData=%v, %d rows, AsOf=%q", empty.HasData, len(empty.Rows), empty.AsOf)
	}
}
