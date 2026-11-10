package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestLiveFeedHubPinsNonBlockingPublishAndScope pins the live feed
// hub (livefeed.go): a new public kill reaches a subscribed stream
// with its resolved row, a subscriber scoped to another system
// hears nothing, a subscriber that never reads cannot stall the
// killfeed, and an unsubscribed stream leaves the hub.
func TestLiveFeedHubPinsNonBlockingPublishAndScope(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()

	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatal(err)
	}

	all, ok := app.killLive.subscribe("", 0, false, false)
	if !ok {
		t.Fatal("unscoped subscribe refused")
	}
	other, ok := app.killLive.subscribe("system", 30000144, false, false)
	if !ok {
		t.Fatal("scoped subscribe refused")
	}

	// 1. A new public kill reaches the unscoped stream only.
	if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(700011)), "h", &killZKB{}, "zkill", true, false); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-all.ch:
		var row killRow
		data := strings.TrimSpace(string(frame[len("event:kill\ndata:"):]))
		if err := json.Unmarshal([]byte(data), &row); err != nil {
			t.Fatalf("kill event is not a killRow: %v", err)
		}
		if row.ID != 700011 || row.SystemName != "Jita" {
			t.Errorf("kill event row id=%d system=%q, want 700011 in Jita", row.ID, row.SystemName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no kill event on the unscoped stream within 5s")
	}
	select {
	case frame := <-other.ch:
		t.Errorf("scoped stream heard a kill outside its system: %q", frame)
	case <-time.After(200 * time.Millisecond):
	}

	// 2. A subscriber that never reads cannot stall the killfeed:
	// fill its channel, then ingest again and require the ingest
	// to finish. The kill is dropped for that subscriber, not
	// queued behind it.
	slow, ok := app.killLive.subscribe("", 0, false, false)
	if !ok {
		t.Fatal("slow subscribe refused")
	}
	for i := 0; i < liveFeedChanCap; i++ {
		slow.ch <- []byte("x")
	}
	done := make(chan error, 1)
	go func() {
		_, err := app.ingestKill(ctx, json.RawMessage(killJSON(700012)), "h", &killZKB{}, "zkill", true, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ingest with a stalled subscriber: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ingest blocked behind a stalled live-feed subscriber")
	}
	if n := len(slow.ch); n != liveFeedChanCap {
		t.Errorf("slow subscriber channel holds %d frames, want %d (the kill dropped)", n, liveFeedChanCap)
	}

	// 3. A big-only stream learns about a kill at price time, not
	// at insert time: the value is unknown until the appraisal
	// lands, so the insert publishes nothing to it and the pricing
	// publishes the whole kill.
	bigSub, ok := app.killLive.subscribe("", 0, true, false)
	if !ok {
		t.Fatal("big subscribe refused")
	}
	k := db.Kill{KillmailID: 700021, SystemID: 30000142, RegionID: 10000002, ShipTypeID: 587}
	row := killRow{ID: 700021, ShipTypeID: 587, ShipName: "Rifter", SystemID: 30000142, SystemName: "Jita"}
	app.killLive.publishKill(row, k)
	select {
	case frame := <-bigSub.ch:
		t.Errorf("big-only stream heard an unpriced kill: %q", frame)
	case <-time.After(200 * time.Millisecond):
	}
	priced := k
	priced.ValueOurs = sql.NullFloat64{Float64: 2e9, Valid: true}
	app.killLive.publishKillPriced(row, priced, "2.00B", true)
	select {
	case frame := <-bigSub.ch:
		if !strings.Contains(string(frame), "event:kill\n") {
			t.Errorf("big-only stream got a non-kill frame at price time: %q", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("big-only stream heard nothing once the kill priced big")
	}

	// 4. Unsubscribe removes the streams from the hub.
	app.killLive.unsubscribe(all)
	app.killLive.unsubscribe(other)
	app.killLive.unsubscribe(slow)
	app.killLive.unsubscribe(bigSub)
	if n := app.killLive.count(); n != 0 {
		t.Errorf("hub holds %d subscribers after unsubscribe, want 0", n)
	}
}
