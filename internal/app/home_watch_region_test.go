package app

// Audit 2.8e, Home half: a watched global-market type (PLEX)
// stores its history in the global region, whatever region the
// watch was saved under. The Home market health line and the
// briefing watch-move line must read — and name — that region,
// like the Market page does, not the watch's saved region.

import (
	"context"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/markethistory"
)

func TestHomeWatchlistGlobalTypeReadsGlobalRegion(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id, published) VALUES (44992, 'PLEX', 0, 1)`); err != nil {
		t.Fatalf("seed PLEX type: %v", err)
	}
	// The watch is saved under The Forge (10000002), as the
	// watchlist form defaults; PLEX history lives in the
	// global region (19000001) and nowhere else.
	if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
		UserID: user.ID, TypeID: 44992, RegionID: 10000002,
		ThresholdPct: 5, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed watchlist: %v", err)
	}
	// +10.0% over 7 days in the global region.
	for _, d := range []struct {
		ago int
		avg float64
	}{{7, 5_000_000}, {3, 5_250_000}, {0, 5_500_000}} {
		day := now.AddDate(0, 0, -d.ago).Format(markethistory.DateLayout)
		if err := q.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: 19000001, TypeID: 44992, Date: day, Average: d.avg,
			Highest: d.avg, Lowest: d.avg, Volume: 5000, OrderCount: 90,
		}); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}

	if line := app.marketHealthLine(ctx, user.ID); !strings.Contains(line, "watchlist: 1 moving") {
		t.Fatalf("market health line = %q, want the PLEX watch counted as moving", line)
	}

	bundles := []*charSnaps{{ch: db.Character{CharacterID: 777, UserID: user.ID, Name: "Fixture"}}}
	lines := app.briefingMarketLines(ctx, bundles, map[int64]bool{}, map[int64]string{})
	found := false
	for _, l := range lines {
		if strings.Contains(l.html, "in Global market") {
			found = true
		}
	}
	if !found {
		t.Fatalf("briefing lines = %v, want a watch-move line naming the Global market", lines)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home watchlist reads made %d outbound calls, want 0", got)
	}
}
