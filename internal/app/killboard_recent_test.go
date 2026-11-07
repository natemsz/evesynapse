package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestRecentKillsRingPinsServingAndLowRAM pins the resident in-memory
// recent kills feed (killboard_recent.go):
// 1. Ingesting public kills populates the resident ring in reverse-chronological order.
// 2. Private kills are excluded from the ring.
// 3. The unpaginated /intel/kills/ front page serves from memory and matches SQL.
// 4. LOW_RAM_MODE=1 bypasses the memory ring and serves SQL.
// 5. Paging past the ring window falls back to SQL.
func TestRecentKillsRingPinsServingAndLowRAM(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()

	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatal(err)
	}

	// 1. Ingest two public kills and one private kill.
	for _, id := range []int64{700001, 700002} {
		if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(id)), "h", &killZKB{Solo: id == 700002}, "zkill", true, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(700003)), "h", nil, "esi", false, true); err != nil {
		t.Fatal(err)
	}

	app.recentKills.mu.RLock()
	ringLen := len(app.recentKills.kills)
	app.recentKills.mu.RUnlock()
	if ringLen != 2 {
		t.Fatalf("ring has %d kills, want 2 public kills", ringLen)
	}

	// 2. Front-page query serves from memory.
	bt := time.Now().Add(time.Hour).UTC()
	bid := int64(1 << 62)
	memKills, ok := app.getRecentKills(ctx, bt, bid, 0, false, killPageSize)
	if !ok || len(memKills) != 2 {
		t.Fatalf("getRecentKills ok=%v len=%d, want true/2", ok, len(memKills))
	}
	if memKills[0].KillmailID != 700002 || memKills[1].KillmailID != 700001 {
		t.Errorf("kills order = [%d, %d], want [700002, 700001]", memKills[0].KillmailID, memKills[1].KillmailID)
	}

	// 3. Matches SQL path.
	sqlKills, err := app.queries.ListKillsRecent(ctx, db.ListKillsRecentParams{
		BeforeTime: bt,
		BeforeID:   bid,
		MinValue:   0,
		SoloOnly:   false,
		MaxRows:    killPageSize,
	})
	if err != nil {
		t.Fatalf("ListKillsRecent (SQL): %v", err)
	}
	if len(sqlKills) != len(memKills) {
		t.Fatalf("SQL kills len=%d != mem kills len=%d", len(sqlKills), len(memKills))
	}

	// 4. Low-RAM mode: getRecentKills returns false, forces SQL path.
	app.cfg.lowRAMMode = true
	if _, ok := app.getRecentKills(ctx, bt, bid, 0, false, killPageSize); ok {
		t.Errorf("getRecentKills in low-RAM mode = true, want false")
	}
	app.cfg.lowRAMMode = false

	// 5. Paging past the ring buffer: falls back to SQL.
	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := app.getRecentKills(ctx, oldTime, 100, 0, false, killPageSize); ok {
		t.Errorf("getRecentKills with old before-cursor = true, want false")
	}
}
