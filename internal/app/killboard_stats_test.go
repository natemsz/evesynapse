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
