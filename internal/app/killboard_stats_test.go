package app

import (
	"bytes"
	"context"
	"encoding/json"

	"fmt"
	"github.com/klauspost/compress/gzip"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestKillboardStatsStoredAtIngest pins the precompute rule for
// killboard statistics: a kill folded at ingest is what the scoped
// board and the headline read, and no page render scans kills.
func TestKillboardStatsStoredAtIngest(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	mk := func(id int64) string {
		return fmt.Sprintf(`{"killmail_id":%d,"killmail_time":%q,"solar_system_id":30000142,
"victim":{"character_id":9001,"corporation_id":98000001,"alliance_id":99000001,"ship_type_id":587,"damage_taken":900,"items":[]},
"attackers":[{"character_id":9002,"corporation_id":98000002,"alliance_id":99000002,"ship_type_id":11371,"weapon_type_id":2873,"damage_done":900,"final_blow":true}]}`, id, ts)
	}
	for _, id := range []int64{710001, 710002} {
		if _, err := app.ingestKill(ctx, json.RawMessage(mk(id)), "h", &killZKB{Solo: true}, "zkill", true, false); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now().Add(-90 * 24 * time.Hour).UTC()
	st, err := q.GetKillEntityStats(ctx, db.GetKillEntityStatsParams{Kind: "corporation", EntityID: 98000002, Since: since})
	if err != nil {
		t.Fatalf("GetKillEntityStats: %v", err)
	}
	if st.Kills != 2 || st.Solo != 2 {
		t.Errorf("stored corp stats kills=%d solo=%d, want 2/2", st.Kills, st.Solo)
	}
	loss, err := q.GetKillEntityStats(ctx, db.GetKillEntityStatsParams{Kind: "corporation", EntityID: 98000001, Since: since})
	if err != nil {
		t.Fatalf("GetKillEntityStats losses: %v", err)
	}
	if loss.Losses != 2 {
		t.Errorf("stored victim corp losses=%d, want 2", loss.Losses)
	}
	// The headline is refreshed by the worker pass, then read stored.
	app.refreshKillboardHeadline(ctx)
	hs, htop := app.killboardHeadline(ctx, app.newKillNames(ctx))
	if hs.KillsDay != 2 {
		t.Errorf("headline kills_day=%d, want 2", hs.KillsDay)
	}
	found := false
	for _, r := range htop.Alliances {
		if r.ID == 99000002 && r.Count == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("headline top alliances = %+v, want alliance 99000002 with 2", htop.Alliances)
	}
	_ = q
}

// TestRepricedKillMovesCountedISK: a kill counted at one value and
// priced again later moves ISK destroyed (attackers) and ISK lost
// (victim) to the new value, without counting the kill twice.
func TestRepricedKillMovesCountedISK(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	raw := fmt.Sprintf(`{"killmail_id":710101,"killmail_time":%q,"solar_system_id":30000142,
"victim":{"character_id":9101,"corporation_id":98000101,"ship_type_id":587,"damage_taken":900,"items":[]},
"attackers":[{"character_id":9102,"corporation_id":98000102,"ship_type_id":11371,"weapon_type_id":2873,"damage_done":900,"final_blow":true}]}`,
		at.Format("2006-01-02T15:04:05Z"))
	if _, err := app.ingestKill(ctx, json.RawMessage(raw), "h", &killZKB{}, "zkill", true, false); err != nil {
		t.Fatal(err)
	}
	if err := app.foldKillStats(ctx, 710101); err != nil {
		t.Fatal(err)
	}
	if err := app.storeKillPrices(ctx, 710101, at, []killLine{{typeID: 587, destroyed: 1}}, map[int64]float64{587: 5_000_000}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	since := at.Add(-24 * time.Hour)
	killer, err := q.GetKillEntityStats(ctx, db.GetKillEntityStatsParams{Kind: "corporation", EntityID: 98000102, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if killer.Kills != 1 || killer.IskKilled != 5_000_000 {
		t.Errorf("attacker corp kills=%d isk_killed=%v, want 1 and 5000000", killer.Kills, killer.IskKilled)
	}
	victim, err := q.GetKillEntityStats(ctx, db.GetKillEntityStatsParams{Kind: "corporation", EntityID: 98000101, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if victim.Losses != 1 || victim.IskLost != 5_000_000 {
		t.Errorf("victim corp losses=%d isk_lost=%v, want 1 and 5000000", victim.Losses, victim.IskLost)
	}
}

// TestKillEntityFeedServesAlliancePage pins the stored feed the
// entity boards page through: kills land on it at ingest, pages
// follow the (time, id) cursor, and it serves after the
// involvement detail is gone.
func TestKillEntityFeedServesAlliancePage(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`); err != nil {
		t.Fatal(err)
	}
	mk := func(id int64, ago time.Duration) string {
		ts := time.Now().Add(-ago).UTC().Format("2006-01-02T15:04:05Z")
		return fmt.Sprintf(`{"killmail_id":%d,"killmail_time":%q,"solar_system_id":30000142,
"victim":{"character_id":9001,"corporation_id":98000001,"alliance_id":99000001,"ship_type_id":587,"damage_taken":900,"items":[]},
"attackers":[{"character_id":9002,"corporation_id":98000002,"alliance_id":99000002,"ship_type_id":11371,"weapon_type_id":2873,"damage_done":900,"final_blow":true}]}`, id, ts)
	}
	for _, s := range []struct {
		id   int64
		solo bool
	}{{720001, false}, {720002, false}, {720003, true}} {
		ago := time.Duration(720004-s.id) * time.Hour
		if _, err := app.ingestKill(ctx, json.RawMessage(mk(s.id, ago)), "h", &killZKB{Solo: s.solo}, "zkill", true, false); err != nil {
			t.Fatal(err)
		}
	}
	page := func(bt time.Time, bid int64, solo bool, n int64) []db.Kill {
		t.Helper()
		rows, err := q.ListKillsByEntityFeed(ctx, db.ListKillsByEntityFeedParams{Kind: "alliance", EntityID: 99000002, BeforeTime: bt, BeforeID: bid, MinValue: 0, SoloOnly: solo, MaxRows: n})
		if err != nil {
			t.Fatalf("ListKillsByEntityFeed: %v", err)
		}
		return rows
	}
	bt, bid := time.Now().Add(time.Hour).UTC(), int64(1<<62)
	first := page(bt, bid, false, 2)
	if len(first) != 2 || first[0].KillmailID != 720003 || first[1].KillmailID != 720002 {
		ids := []int64{}
		for _, k := range first {
			ids = append(ids, k.KillmailID)
		}
		t.Fatalf("first feed page = %v, want [720003 720002]", ids)
	}
	rest := page(first[1].KillmailTime, first[1].KillmailID, false, 2)
	if len(rest) != 1 || rest[0].KillmailID != 720001 {
		t.Fatalf("second feed page = %v, want [720001]", rest)
	}
	if solo := page(bt, bid, true, 10); len(solo) != 1 || solo[0].KillmailID != 720003 {
		t.Fatalf("solo feed = %v, want [720003]", solo)
	}
	// The victim alliance pages the same kills as losses.
	if losses := page(bt, bid, false, 10); len(losses) != 3 {
		t.Fatalf("attacker feed = %d kills, want 3", len(losses))
	}
	vpage, err := q.ListKillsByEntityFeed(ctx, db.ListKillsByEntityFeedParams{Kind: "alliance", EntityID: 99000001, BeforeTime: bt, BeforeID: bid, MaxRows: 10})
	if err != nil || len(vpage) != 3 {
		t.Fatalf("victim alliance feed = %v, %v; want 3 kills", vpage, err)
	}
	// A private kill of our own pilot feeds the account map
	// layer (public stats never count it): the victim side
	// totals four losses for the pilot, three public, one not.
	priv := fmt.Sprintf(`{"killmail_id":720010,"killmail_time":%q,"solar_system_id":30000142,
"victim":{"character_id":9001,"corporation_id":98000001,"alliance_id":99000001,"ship_type_id":587,"damage_taken":10,"items":[]},
"attackers":[{"character_id":9999,"corporation_id":98000009,"alliance_id":99000009,"ship_type_id":587,"weapon_type_id":2873,"damage_done":10,"final_blow":true}]}`, time.Now().Add(-30*time.Minute).UTC().Format("2006-01-02T15:04:05Z"))
	if _, err := app.ingestKill(ctx, json.RawMessage(priv), "h", nil, "esi", false, true); err != nil {
		t.Fatal(err)
	}
	mine, err := q.ListAccountKillsBySystem(ctx, db.ListAccountKillsBySystemParams{CharacterIds: []int64{9001}, Since: time.Now().Add(-24 * time.Hour).UTC()})
	if err != nil {
		t.Fatalf("ListAccountKillsBySystem: %v", err)
	}
	if len(mine) != 1 || mine[0].Losses != 4 || mine[0].Kills != 0 {
		t.Fatalf("account kills by system = %+v, want one system with 4 losses, 0 kills", mine)
	}
	// Stored for how it is used: the feed serves with the
	// involvement record deleted, as it must after retention.
	if _, err := conn.Exec(`DELETE FROM kill_involvement`); err != nil {
		t.Fatal(err)
	}
	if again := page(bt, bid, false, 10); len(again) != 3 {
		t.Fatalf("feed after involvement prune = %d kills, want 3", len(again))
	}
}

// TestKillDetailRawReadsGzipAndZstd pins the stored-detail
// decode: rows written before the zstd switch are gzip and
// must keep reading, new rows are zstd.
func TestKillDetailRawReadsGzipAndZstd(t *testing.T) {
	raw := []byte(killJSON(730001))
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := killDetailRaw(buf.Bytes())
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("gzip detail round trip: %v, equal=%v", err, bytes.Equal(got, raw))
	}
	got, err = killDetailRaw(compressKillDetail(raw))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("zstd detail round trip: %v, equal=%v", err, bytes.Equal(got, raw))
	}
	if _, err := killDetailRaw([]byte("not compressed")); err == nil {
		t.Error("garbage payload decoded, want error")
	}
}

// TestKillFeedBackfillWalksToTheEndOnce pins the legacy feed
// sweep: a kill stored before the feed existed gets its rows from
// the cursor walk, the walk records that it reached the end of the
// table, and from then on it never runs again.
func TestKillFeedBackfillWalksToTheEndOnce(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	raw := fmt.Sprintf(`{"killmail_id":720001,"killmail_time":%q,"solar_system_id":30000142,
"victim":{"character_id":9001,"corporation_id":98000001,"alliance_id":99000001,"ship_type_id":587,"damage_taken":900,"items":[]},
"attackers":[{"character_id":9002,"corporation_id":98000002,"alliance_id":99000002,"ship_type_id":11371,"weapon_type_id":2873,"damage_done":900,"final_blow":true}]}`, ts)
	if _, err := app.ingestKill(ctx, json.RawMessage(raw), "h", &killZKB{}, "zkill", true, false); err != nil {
		t.Fatal(err)
	}
	feedRows := func() int {
		var n int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM kill_entity_feed WHERE killmail_id = 720001`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if feedRows() == 0 {
		t.Fatal("ingest wrote no feed rows")
	}
	// Legacy state: the kill exists, its feed rows do not.
	if _, err := conn.Exec(`DELETE FROM kill_entity_feed WHERE killmail_id = 720001`); err != nil {
		t.Fatal(err)
	}
	app.backfillMissingKillFeeds(ctx)
	if feedRows() == 0 {
		t.Fatal("backfill restored no feed rows")
	}
	var done bool
	if err := conn.QueryRow(`SELECT done_at IS NOT NULL FROM kill_feed_backfill_state WHERE id = 1`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if !done || !app.killFeedBackfillDone {
		t.Fatal("a walk that reached the end of the table did not record it was done")
	}
	// Done: the walk never reads the table again, even after a
	// restart (the stored flag, not the in-memory one, decides).
	if _, err := conn.Exec(`DELETE FROM kill_entity_feed WHERE killmail_id = 720001`); err != nil {
		t.Fatal(err)
	}
	app.killFeedBackfillDone = false
	app.backfillMissingKillFeeds(ctx)
	if feedRows() != 0 {
		t.Fatal("a finished walk ran again")
	}
}

// countStoredKills brings kills a test wrote straight into the kills
// table onto the pages, as production does for kills stored before
// the counting queue: the feed sweep writes their feed rows and the
// one time rebuild counts them into the daily aggregates.
func countStoredKills(t *testing.T, app *Application, ctx context.Context) {
	t.Helper()
	app.backfillMissingKillFeeds(ctx)
	if err := app.rebuildKillStats(ctx); err != nil {
		t.Fatalf("rebuild kill stats: %v", err)
	}
}

// TestKillStatsSecondPass: the rebuild's first pass counts kills
// and ISK without waiting for the attacker restore; the second pass,
// run once the restore drains, counts everything again into freshly
// made tables (ships flown from the restored rows), and only then
// does the retention prune's gate open.
func TestKillStatsSecondPass(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	seed := func(id int64, attackers bool) {
		t.Helper()
		at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id%100) * time.Hour)
		if _, err := app.db.ExecContext(ctx, `
			INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, victim_corporation_id,
				ship_type_id, attacker_count, fb_character_id, source, ingested_at, value_ours)
			VALUES ($1, $2, 30000142, 93000001, 98000009, 587, 1, 90000001, 'zkill', $2, 1000000)`, id, at); err != nil {
			t.Fatalf("seed kill %d: %v", id, err)
		}
		if _, err := app.db.ExecContext(ctx, `
			INSERT INTO kill_involvement (killmail_id, character_id, corporation_id) VALUES ($1, 90000001, 98000002)`, id); err != nil {
			t.Fatalf("seed involvement %d: %v", id, err)
		}
		if attackers {
			if _, err := app.db.ExecContext(ctx, `
				INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, ship_type_id, final_blow)
				VALUES ($1, 0, 90000001, 98000002, 587, true)`, id); err != nil {
				t.Fatalf("seed attackers %d: %v", id, err)
			}
		}
	}
	// Two kills, one still missing its attacker rows.
	seed(950001, true)
	seed(950002, false)
	totals := func() (kills int64, isk float64, flown int64) {
		t.Helper()
		if err := app.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(kills), 0), COALESCE(SUM(isk_killed), 0) FROM kill_entity_daily
			WHERE kind = 'character' AND entity_id = 90000001`).Scan(&kills, &isk); err != nil {
			t.Fatal(err)
		}
		if err := app.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(flown), 0) FROM kill_entity_daily_ships
			WHERE kind = 'character' AND entity_id = 90000001`).Scan(&flown); err != nil {
			t.Fatal(err)
		}
		return
	}

	app.runKillStatsFirstPass(ctx)
	if k, isk, flown := totals(); k != 2 || isk != 2000000 || flown != 1 {
		t.Fatalf("after the first pass: kills %d, ISK %.0f, flown %d; want 2, 2000000, 1", k, isk, flown)
	}
	if app.killStatsComplete(ctx) {
		t.Fatal("the prune gate opened before the second pass")
	}

	// The restore reads the missing rows back, then the second pass.
	if _, err := app.db.ExecContext(ctx, `
		INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, ship_type_id, final_blow)
		VALUES (950002, 0, 90000001, 98000002, 587, true)`); err != nil {
		t.Fatal(err)
	}
	if err := app.completeKillStats(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if k, isk, flown := totals(); k != 2 || isk != 2000000 || flown != 2 {
		t.Fatalf("after the second pass: kills %d, ISK %.0f, flown %d; want 2, 2000000, 2", k, isk, flown)
	}
	if !app.killStatsComplete(ctx) {
		t.Fatal("the prune gate stayed shut after the second pass")
	}
}
