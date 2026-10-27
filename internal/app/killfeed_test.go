package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// killJSON is a small ESI killmail: a Rifter (587) lost in Jita with
// a fitted autocannon (2873), a container (3465) holding a blueprint
// copy (singleton 2) and ammo (178), killed by one pilot.
func killJSON(id int64) string {
	return fmt.Sprintf(`{"killmail_id":%d,"killmail_time":"2026-10-21T05:00:00Z","solar_system_id":30000142,
	"victim":{"character_id":9001,"corporation_id":98000001,"alliance_id":99000001,"ship_type_id":587,"damage_taken":900,
	  "items":[{"flag":27,"item_type_id":2873,"quantity_destroyed":1,"singleton":0},
	           {"flag":5,"item_type_id":3465,"quantity_dropped":1,"singleton":0,"items":[
	              {"flag":0,"item_type_id":999,"quantity_dropped":1,"singleton":2},
	              {"flag":0,"item_type_id":178,"quantity_destroyed":100,"singleton":0}]}]},
	"attackers":[{"character_id":9002,"corporation_id":98000002,"alliance_id":99000002,"ship_type_id":11371,"weapon_type_id":2873,"damage_done":900,"final_blow":true}]}`, id)
}

func killPackageJSON(id, seq int64) string {
	return fmt.Sprintf(`{"killmail_id":%d,"hash":"h%d","esi":%s,"zkb":{"locationID":60003760,"totalValue":2500000,"points":5,"npc":false,"solo":true,"awox":false},"sequence_id":%d}`,
		id, id, killJSON(id), seq)
}

// TestKillfeedFollowsStream: from its stored cursor the follower
// ingests each file, skips a sequence the stream no longer holds
// (behind the latest), and waits at the head for the next.
func TestKillfeedFollowsStream(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/sequence.json":
			fmt.Fprint(w, `{"sequence":1005}`)
		case "/1002.json":
			fmt.Fprint(w, killPackageJSON(500002, 1002))
		case "/1004.json":
			fmt.Fprint(w, killPackageJSON(500004, 1004))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	app, _, q := buildCorpTestApp(t, nil)
	app.cfg.killfeedBaseURL = srv.URL + "/"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.SetKillfeedState(ctx, db.SetKillfeedStateParams{Sequence: 1002, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { app.runKillfeed(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := q.GetKillfeedState(context.Background())
		if err == nil && st.Sequence == 1005 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor never reached the head: %+v %v", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	var n int
	var public bool
	var zkill sql.NullFloat64
	if err := app.db.QueryRow(`SELECT COUNT(*), bool_and(public), max(value_zkill) FROM kills`).Scan(&n, &public, &zkill); err != nil {
		t.Fatal(err)
	}
	if n != 2 || !public || zkill.Float64 != 2500000 {
		t.Errorf("stream kills: %d stored, public %v, zkill %v", n, public, zkill)
	}
	var attackers int
	_ = app.db.QueryRow(`SELECT COUNT(*) FROM kill_attackers WHERE killmail_id = 500002`).Scan(&attackers)
	if attackers != 1 {
		t.Errorf("attackers stored: %d", attackers)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["/1003.json"] != 1 {
		t.Errorf("the expired sequence should be asked once and skipped, asked %d", asked["/1003.json"])
	}
}

// TestKillboardMergeAndPrice: a character's ESI killmail is stored
// private, priced by our own appraisal (hull, fitting and the
// container's contents; the blueprint copy counts nothing), and
// turns public when the stream carries it.
func TestKillboardMergeAndPrice(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, q, user.ID, 9001, "Victim Pilot")
	if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
		KillmailID: 600001, CharacterID: 9001, Hash: "h600001", Payload: killJSON(600001), FetchedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := conn.Exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, updated_at) VALUES
		(10000002, 587, 400000, $1), (10000002, 2873, 50000, $1), (10000002, 3465, 10000, $1), (10000002, 178, 10, $1)`, now); err != nil {
		t.Fatal(err)
	}

	app.killboardMaintenance(ctx)
	var public bool
	var source string
	var value, dropped sql.NullFloat64
	var cov sql.NullFloat64
	if err := conn.QueryRow(`SELECT public, source, value_ours, value_dropped_ours, value_coverage FROM kills WHERE killmail_id = 600001`).
		Scan(&public, &source, &value, &dropped, &cov); err != nil {
		t.Fatalf("merged kill: %v", err)
	}
	if public || source != "esi" {
		t.Errorf("a kill only our character's token reached stays private: public %v source %s", public, source)
	}
	// 400k hull + 50k gun + 10k can + 100 x 10 ammo; the copy is out.
	if want := 400000.0 + 50000 + 10000 + 1000; math.Abs(value.Float64-want) > 1 {
		t.Errorf("value = %v, want %v", value.Float64, want)
	}
	if math.Abs(dropped.Float64-10000) > 1 || cov.Float64 != 1 {
		t.Errorf("dropped %v coverage %v", dropped.Float64, cov.Float64)
	}

	// The stream carries it: now public, zKillboard's figures added.
	if err := app.ingestKillPackage(ctx, []byte(killPackageJSON(600001, 1))); err != nil {
		t.Fatal(err)
	}
	var solo bool
	var zkill sql.NullFloat64
	_ = conn.QueryRow(`SELECT public, source, solo, value_zkill FROM kills WHERE killmail_id = 600001`).Scan(&public, &source, &solo, &zkill)
	if !public || source != "zkill" || !solo || zkill.Float64 != 2500000 {
		t.Errorf("after the stream: public %v source %s solo %v zkill %v", public, source, solo, zkill)
	}
}

// TestKillLines: the hull first, containers' contents walked,
// blueprint copies left out.
func TestKillLines(t *testing.T) {
	var km killmailFull
	if err := json.Unmarshal([]byte(killJSON(1)), &km); err != nil {
		t.Fatal(err)
	}
	lines := killLines(km)
	var got []string
	for _, l := range lines {
		got = append(got, fmt.Sprintf("%d:%d/%d", l.typeID, l.destroyed, l.dropped))
	}
	if want := "587:1/0 2873:1/0 3465:0/1 178:100/0"; strings.Join(got, " ") != want {
		t.Errorf("lines = %s, want %s", strings.Join(got, " "), want)
	}
	total, _, cov := killValue(lines, map[int64]float64{587: 100, 178: 1})
	if total != 200 || cov != 0.5 {
		t.Errorf("value %v coverage %v", total, cov)
	}
}

// TestKillboardPages: the feed shows public kills only, scoped and
// narrowed; the kill page shows a public kill's fitting and attackers
// and keeps a private one hidden.
func TestKillboardPages(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{700001, 700002} {
		if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(id)), "h", &killZKB{TotalValue: 2e9, Solo: id == 700002}, "zkill", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(700003)), "h", nil, "esi", false); err != nil {
		t.Fatal(err)
	}
	app.killboardMaintenance(ctx)

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	code, body := get("/intel/kills/")
	if code != http.StatusOK || !strings.Contains(body, `href="/intel/kill/700001/"`) || !strings.Contains(body, `href="/intel/kill/700002/"`) {
		t.Fatalf("feed: %d, public kills missing", code)
	}
	if strings.Contains(body, "/intel/kill/700003/") {
		t.Errorf("a private kill must never be listed")
	}
	if !strings.Contains(body, "Jita") || !strings.Contains(body, `class="sec sec-9"`) {
		t.Errorf("feed rows name the system with its security")
	}
	row := func(id string) string { return `class="kb-kill-link" href="/intel/kill/` + id + `/"` }
	if _, solo := get("/intel/kills/?solo=1"); strings.Contains(solo, row("700001")) || !strings.Contains(solo, row("700002")) {
		t.Errorf("solo narrows to lone-attacker kills")
	}
	if _, sys := get("/intel/kills/?system=30000142"); !strings.Contains(sys, `class="kb-scope"`) || !strings.Contains(sys, "/intel/kill/700001/") {
		t.Errorf("a system scope shows its pill and its kills")
	}
	if _, ent := get("/intel/kills/?character=9002"); !strings.Contains(ent, "/intel/kill/700001/") {
		t.Errorf("a pilot's feed includes their kills as an attacker")
	}

	code, page := get("/intel/kill/700001/")
	if code != http.StatusOK || !strings.Contains(page, "High slots") || !strings.Contains(page, `class="kp-a fb"`) {
		t.Errorf("kill page: %d, fitting or attackers missing", code)
	}
	if code, _ := get("/intel/kill/700003/"); code != http.StatusNotFound {
		t.Errorf("a private kill's page must not exist: %d", code)
	}
}

// TestKillfeedRetriesFailedIngest: a package whose ingest
// fails is not skipped — the cursor stays on its sequence and
// the same file is asked again until it lands.
func TestKillfeedRetriesFailedIngest(t *testing.T) {
	var mu sync.Mutex
	asks := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/1002.json" {
			mu.Lock()
			asks++
			n := asks
			mu.Unlock()
			if n == 1 {
				fmt.Fprint(w, `not a kill package`)
				return
			}
			fmt.Fprint(w, killPackageJSON(500002, 1002))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	app, _, q := buildCorpTestApp(t, nil)
	app.cfg.killfeedBaseURL = srv.URL + "/"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := q.SetKillfeedState(ctx, db.SetKillfeedStateParams{Sequence: 1002, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { app.runKillfeed(ctx); close(done) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		_ = app.db.QueryRow(`SELECT COUNT(*) FROM kills WHERE killmail_id = 500002`).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the failed kill was never retried into the store (asks so far: %d)", asks)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if asks < 2 {
		t.Fatalf("sequence asked %d times, want a retry after the failed ingest", asks)
	}
}
