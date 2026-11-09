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
	if err := app.db.QueryRow(`SELECT COUNT(*), bool_and(public) FROM kills`).Scan(&n, &public); err != nil {
		t.Fatal(err)
	}
	if n != 2 || !public {
		t.Errorf("stream kills: %d stored, public %v", n, public)
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
	// The lines are priced once and stored: renders read these.
	prices, err := q.GetKillLinePrices(ctx, 600001)
	if err != nil || len(prices) == 0 {
		t.Fatalf("stored line prices: %v (%d rows)", err, len(prices))
	}
	byType := map[int64]float64{}
	for _, p := range prices {
		byType[p.TypeID] = p.UnitPrice
	}
	if byType[587] != 400000 || byType[2873] != 50000 {
		t.Errorf("stored line prices: %v, want hull 400000 and gun 50000", byType)
	}

	// The stream carries it: now public, with zKillboard's flags.
	if err := app.ingestKillPackage(ctx, []byte(killPackageJSON(600001, 1))); err != nil {
		t.Fatal(err)
	}
	var solo bool
	_ = conn.QueryRow(`SELECT public, source, solo FROM kills WHERE killmail_id = 600001`).Scan(&public, &source, &solo)
	if !public || source != "zkill" || !solo {
		t.Errorf("after the stream: public %v source %s solo %v", public, source, solo)
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
		if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(id)), "h", &killZKB{Solo: id == 700002}, "zkill", true, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(700003)), "h", nil, "esi", false, true); err != nil {
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
	if head, _, _ := strings.Cut(body, `class="kb-kill-link"`); !strings.Contains(head, `<li class="kb-head"`) ||
		!strings.Contains(head, `>Ship</span><span class="kb-h-main">Victim</span><span class="kb-h-atk">Final blow</span><span class="kb-h-where">System</span><span class="kb-h-value">Est. value</span>`) {
		t.Errorf("the feed's header row does not label its columns ahead of the first kill")
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
	if _, ent := get("/intel/kills/?character=9002"); !strings.Contains(ent, `href="/pilot/?character=9002"`) {
		t.Errorf("the scope banner on a pilot's feed links their name to their profile")
	}
	if _, sys := get("/intel/kills/?system=30000142"); strings.Contains(sys, "/pilot/?character=") {
		t.Errorf("a non-pilot scope keeps the plain scope name, no profile link")
	}

	code, page := get("/intel/kill/700001/")
	if code != http.StatusOK || !strings.Contains(page, "High slots") || !strings.Contains(page, `class="kp-a fb"`) {
		t.Errorf("kill page: %d, fitting or attackers missing", code)
	}
	if !strings.Contains(page, `class="kp-info" href="/pilot/?character=9001"`) {
		t.Errorf("the victim's name carries an info link to their profile")
	}
	if !strings.Contains(page, `class="kp-info" href="/pilot/?character=9002"`) {
		t.Errorf("an attacker's name carries an info link to their profile")
	}
	if code, _ := get("/intel/kill/700003/"); code != http.StatusNotFound {
		t.Errorf("a private kill's page must not exist: %d", code)
	}
}

// TestKillboardNamesResolveFromView: a name the board shows as
// a placeholder settles because the board was viewed. The first
// render of a pilot's feed shows the uncached parties as ID
// placeholders and notes the misses; the resolution pass (driven
// synchronously here, kicked in the background in production)
// settles them into the caches, and the next render shows the
// names.
func TestKillboardNamesResolveFromView(t *testing.T) {
	stub := &bulkNamesStub{
		known: map[int64]string{
			9001: "Victim Pilot", 9002: "Attacker Pilot",
			98000001: "Victim Corp", 98000002: "Attacker Corp",
			99000001: "Victim Alliance", 99000002: "Attacker Alliance",
		},
		cats: map[int64]string{
			9001: "character", 9002: "character",
			98000001: "corporation", 98000002: "corporation",
			99000001: "alliance", 99000002: "alliance",
		},
	}
	app, conn, _ := buildCorpTestApp(t, stub)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ingestKill(ctx, json.RawMessage(killJSON(700001)), "h", &killZKB{}, "zkill", true, false); err != nil {
		t.Fatal(err)
	}
	get := func(path string) string {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	first := get("/intel/kills/?character=9001")
	if !strings.Contains(first, "Pilot #9001") {
		t.Fatalf("the first render should show the uncached pilot as a placeholder")
	}
	if strings.Contains(first, "Victim Pilot") {
		t.Fatalf("the first render showed a name nothing had resolved yet")
	}
	app.resolveNotedNames(ctx)
	if name, ok := app.esi.CachedCharacterName(9001); !ok || name != "Victim Pilot" {
		t.Fatalf("pilot cache after the pass: %q, %v; want the resolved name", name, ok)
	}
	if name, ok := app.esi.CachedCorpName(98000001); !ok || name != "Victim Corp" {
		t.Fatalf("corp cache after the pass: %q, %v; want the resolved name", name, ok)
	}
	if name, ok := app.esi.CachedAllianceName(99000001); !ok || name != "Victim Alliance" {
		t.Fatalf("alliance cache after the pass: %q, %v; want the resolved name", name, ok)
	}
	second := get("/intel/kills/?character=9001")
	if !strings.Contains(second, "Victim Pilot") || strings.Contains(second, "Pilot #9001") {
		t.Errorf("the second render should show the resolved pilot name")
	}
}

// TestKillboardScopedStats: a character, corporation or
// alliance board's strip and rail show that entity's figures,
// not New Eden's, while the unscoped board keeps the global
// headline. The seed gives the pilot, his corporation and his
// alliance the same record (2 kills, 1 of them flagged solo,
// 1 loss, 40M destroyed, the biggest worth 30M) inside a board
// whose global day is 4 kills and 1.06B destroyed, so the two
// can never be confused. One kill carries two attackers from
// the corporation: it still counts and values once.
func TestKillboardScopedStats(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30000142, 'Jita', 10000002, 0.946), (30002187, 'Amarr', 10000043, 1.0)`)
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge'), (10000043, 'Domain')`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (587, 'Rifter'), (626, 'Vexor'), (22456, 'Sabre')`)
	now := time.Now().UTC()
	kill := func(id int64, at time.Time, system, victim, victimCorp, victimAlliance, ship int64, value float64, solo bool, fb, fbCorp, fbAlliance int64) {
		t.Helper()
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, region_id, victim_character_id, victim_corporation_id,
			victim_alliance_id, ship_type_id, attacker_count, fb_character_id, fb_corporation_id, fb_alliance_id,
			solo, value_zkill, value_ours, source, public, ingested_at)
			VALUES ($1, $2, $3, 10000002, $4, $5, $6, $7, 1, $8, $9, $10, $11, $12, $12, 'zkill', true, $2)`,
			id, at, system, victim, victimCorp, victimAlliance, ship, fb, fbCorp, fbAlliance, solo, value)
	}
	attacker := func(killID int64, seq int, char, corp, alliance, ship int64, fb bool) {
		t.Helper()
		exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, alliance_id, ship_type_id, final_blow)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, killID, seq, char, corp, alliance, ship, fb)
		exec(`INSERT INTO kill_involvement (killmail_id, character_id, corporation_id, alliance_id)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, killID, char, corp, alliance)
	}
	const pilot, corp, alliance = 9201, 98201, 99201
	kill(800001, now.Add(-time.Hour), 30000142, 9401, 98401, 99401, 587, 10000000, true, pilot, corp, alliance)
	attacker(800001, 0, pilot, corp, alliance, 626, true)
	attacker(800001, 1, 9202, corp, alliance, 587, false)
	kill(800002, now.Add(-2*time.Hour), 30000142, pilot, corp, alliance, 22456, 20000000, false, 9301, 98301, 99301)
	attacker(800002, 0, 9301, 98301, 99301, 587, true)
	kill(800003, now.Add(-3*time.Hour), 30002187, 9402, 98401, 99401, 626, 30000000, false, pilot, corp, alliance)
	attacker(800003, 0, pilot, corp, alliance, 626, true)
	kill(800004, now.Add(-30*time.Minute), 30000142, 9403, 98401, 99401, 587, 999000000, false, 9301, 98301, 99301)
	attacker(800004, 0, 9301, 98301, 99301, 587, true)
	// An NPC loss for the same entity, two days back: inside the
	// 90-day scope window, outside the global day. NPC losses
	// count, so every scope's losses and most-lost include it.
	exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, region_id, victim_character_id, victim_corporation_id,
		victim_alliance_id, ship_type_id, attacker_count, fb_character_id, fb_corporation_id, fb_alliance_id,
		solo, npc, value_zkill, value_ours, source, public, ingested_at)
		VALUES (800005, $1, 30000142, 10000002, 9201, 98201, 99201, 22456, 1, 0, 0, 0, false, true, 5000000, 5000000, 'zkill', true, $1)`,
		now.Add(-48*time.Hour))

	// Stored kills reach the pages through the worker pass:
	// the fold into the daily aggregates and the headline
	// refresh run here, as killboardMaintenance runs them.
	app.backfillKillStats(ctx)
	app.refreshKillboardHeadline(ctx)
	get := func(path string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	scopedStrip := []string{
		`<span class="l-stat-label">Kills</span><span class="l-stat-value">2</span><span class="l-stat-sub">1 solo · last 90 days</span>`,
		`<span class="l-stat-label">Losses</span><span class="l-stat-value">2</span><span class="l-stat-sub">last 90 days</span>`,
		`<span class="l-stat-label">ISK destroyed</span><span class="l-stat-value">40.00M</span><span class="l-stat-sub">last 90 days</span>`,
		`<span class="l-stat-label">Biggest kill</span>`,
		`<span class="l-stat-value">30.00M</span>`,
	}
	for _, tc := range []struct{ name, path, aria string }{
		{"character", "/intel/kills/?character=9201", `aria-label="Pilot #9201: last 90 days"`},
		{"corporation", "/intel/kills/?corporation=98201", `aria-label="Corporation #98201: last 90 days"`},
		{"alliance", "/intel/kills/?alliance=99201", `aria-label="Alliance #99201: last 90 days"`},
	} {
		body := get(tc.path)
		if !strings.Contains(body, tc.aria) {
			t.Errorf("%s board: strip aria %q missing", tc.name, tc.aria)
		}
		for _, want := range scopedStrip {
			if !strings.Contains(body, want) {
				t.Errorf("%s board: strip lacks %q", tc.name, want)
			}
		}
		if strings.Contains(body, "Last hour") || strings.Contains(body, "Destroyed today") {
			t.Errorf("%s board: the global strip leaked in", tc.name)
		}
		if !strings.Contains(body, `href="/intel/kill/800003/"`) {
			t.Errorf("%s board: the biggest kill is not the entity's 30M kill", tc.name)
		}
	}

	char := get("/intel/kills/?character=9201")
	for _, want := range []string{
		`<h2>Top ships</h2>`, `<span class="kb-top-n">Vexor</span><span class="kb-top-c">2</span>`,
		`<h2>Most lost</h2>`, `<span class="kb-top-n">Sabre</span><span class="kb-top-c">2</span>`,
		`<h2>Top systems</h2>`,
	} {
		if !strings.Contains(char, want) {
			t.Errorf("character board rail lacks %q", want)
		}
	}
	if strings.Contains(char, "Top killers") || strings.Contains(char, "Hottest systems") {
		t.Errorf("character board rail kept the global headings")
	}

	global := get("/intel/kills/")
	for _, want := range []string{
		`aria-label="The last day across New Eden"`,
		`<span class="l-stat-label">Last hour</span><span class="l-stat-value">1</span>`,
		`<span class="l-stat-label">Last day</span><span class="l-stat-value">4</span>`,
		`<span class="l-stat-label">Destroyed today</span><span class="l-stat-value">1.06B</span>`,
		`<span class="l-stat-value">999.00M</span>`,
		`<h2>Top killers</h2>`, `<h2>Hottest systems</h2>`,
	} {
		if !strings.Contains(global, want) {
			t.Errorf("unscoped board lacks %q", want)
		}
	}
	if strings.Contains(global, "<h2>Top ships</h2>") {
		t.Errorf("unscoped board took the scoped rail")
	}

	// With a stored activity profile, the character board reads
	// its kills and losses from the profile; solo kills and ISK
	// destroyed come from the all-time stored aggregates (the
	// 10M solo kill and the 30M kill
	// seeded above), and the biggest kill stays the 30M one.
	exec(`INSERT INTO pilot_activity_profiles (character_id, backfilled_at, updated_at, kills, losses, final_blows, gang_total, gang_samples, usual_ship_type_id, last_active_at)
		VALUES ($1, $2, $3, 137, 42, 98, 480, 137, 22456, $3)`, pilot, now.Add(-24*time.Hour), now)
	profiled := get("/intel/kills/?character=9201")
	for _, want := range []string{
		`aria-label="Pilot #9201: every recorded kill and loss`,
		`<span class="l-stat-label">Kills</span><span class="l-stat-value">137</span><span class="l-stat-sub">1 solo · every recorded kill and loss</span>`,
		`<span class="l-stat-label">Losses</span><span class="l-stat-value">42</span><span class="l-stat-sub">every recorded kill and loss</span>`,
		`<span class="l-stat-label">ISK destroyed</span><span class="l-stat-value">40.00M</span><span class="l-stat-sub">every recorded kill and loss</span>`,
		`<span class="l-stat-value">30.00M</span>`,
		`href="/intel/kill/800003/"`,
	} {
		if !strings.Contains(profiled, want) {
			t.Errorf("profile-backed character board lacks %q", want)
		}
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
