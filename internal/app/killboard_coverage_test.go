package app

// Tests for the killboard's zKillboard coverage: the pilot and
// corporation history backfills ingest what they fetch into the
// kills store (a participation lands, a privately held kill is
// promoted rather than duplicated), and the signed-in killmail
// page lists a kill only the board holds.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// fakeZkillHistory serves canned history pages by request path
// and counts hits per path, so a test can prove a covered entity
// is not fetched again.
type fakeZkillHistory struct {
	pages map[string]string
	mu    sync.Mutex
	hits  map[string]int
}

func newFakeZkillHistory(pages map[string]string) *fakeZkillHistory {
	return &fakeZkillHistory{pages: pages, hits: map[string]int{}}
}

func (f *fakeZkillHistory) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		f.mu.Unlock()
		page, ok := f.pages[r.URL.Path]
		if !ok {
			http.Error(w, `{"error":"unknown path"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, page)
	})
}

func (f *fakeZkillHistory) hitCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// zkPartKillmail is a kill the pilot (91000001) took part in
// without the final blow: the class of kill the ESI recent list
// never shows for them.
const zkPartKillmail = `{"killmail_id":800001,"killmail_time":"2026-10-26T14:03:11Z","solar_system_id":30000142,` +
	`"victim":{"character_id":92000001,"corporation_id":98000009,"ship_type_id":587,"damage_taken":900,"items":[]},` +
	`"attackers":[{"character_id":92000002,"corporation_id":98000003,"ship_type_id":587,"damage_done":500,"final_blow":true,"weapon_type_id":2488},` +
	`{"character_id":91000001,"corporation_id":98000002,"ship_type_id":626,"damage_done":400,"final_blow":false,"weapon_type_id":2488}]}`

const zkPartEntry = `{"killmail_id":800001,"killmail_time":"2026-10-26T14:03:11Z","solar_system_id":30000142,` +
	`"victim":{"character_id":92000001,"corporation_id":98000009,"ship_type_id":587,"damage_taken":900,"items":[]},` +
	`"attackers":[{"character_id":92000002,"corporation_id":98000003,"ship_type_id":587,"damage_done":500,"final_blow":true,"weapon_type_id":2488},` +
	`{"character_id":91000001,"corporation_id":98000002,"ship_type_id":626,"damage_done":400,"final_blow":false,"weapon_type_id":2488}],` +
	`"zkb":{"locationID":0,"hash":"hash800001","totalValue":10.0,"points":1,"npc":false,"solo":false,"awox":false}}`

const zkLossEntry = `{"killmail_id":800002,"killmail_time":"2026-10-25T09:00:00Z","solar_system_id":30000142,` +
	`"victim":{"character_id":91000001,"corporation_id":98000002,"ship_type_id":587,"damage_taken":700,"items":[]},` +
	`"attackers":[{"character_id":92000003,"corporation_id":98000004,"ship_type_id":587,"damage_done":700,"final_blow":true,"weapon_type_id":2488}],` +
	`"zkb":{"locationID":0,"hash":"hash800002","totalValue":10.0,"points":1,"npc":false,"solo":false,"awox":false}}`

const zkCorpEntry = `{"killmail_id":800003,"killmail_time":"2026-10-24T20:00:00Z","solar_system_id":30000142,` +
	`"victim":{"character_id":92000007,"corporation_id":98000008,"ship_type_id":587,"damage_taken":800,"items":[]},` +
	`"attackers":[{"character_id":92000008,"corporation_id":98000007,"ship_type_id":587,"damage_done":600,"final_blow":true,"weapon_type_id":2488},` +
	`{"character_id":92000005,"corporation_id":98000002,"ship_type_id":626,"damage_done":200,"final_blow":false,"weapon_type_id":2488}],` +
	`"zkb":{"locationID":0,"hash":"hash800003","totalValue":10.0,"points":1,"npc":false,"solo":false,"awox":false}}`

func feedIDs(kills []db.Kill) map[int64]bool {
	out := map[int64]bool{}
	for _, k := range kills {
		out[k.KillmailID] = true
	}
	return out
}

func TestZkillHistoryBackfillIngests(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	hist := newFakeZkillHistory(map[string]string{
		"/characterID/91000001/":   "[" + zkPartEntry + "," + zkLossEntry + "]",
		"/corporationID/98000002/": "[" + zkCorpEntry + "]",
	})
	srv := httptest.NewServer(hist.handler())
	defer srv.Close()
	app.cfg.zkillAPIBaseURL = srv.URL + "/"
	ctx := context.Background()
	client := srv.Client()

	app.backfillPilot(ctx, client, 91000001)

	// The participation kill is in the store, public, from the
	// backfill.
	var pub bool
	var src string
	if err := app.db.QueryRowContext(ctx,
		`SELECT public, source FROM kills WHERE killmail_id = 800001`).Scan(&pub, &src); err != nil {
		t.Fatalf("read participation kill: %v", err)
	}
	if !pub {
		t.Error("participation kill stored non-public")
	}
	if src != "zkill" {
		t.Errorf("participation kill source = %q, want zkill", src)
	}
	// Its full killmail is stored for the detail views.
	payload, err := q.GetKillDetail(ctx, 800001)
	if err != nil {
		t.Fatalf("GetKillDetail: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("gunzip detail: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read detail: %v", err)
	}
	if !strings.Contains(string(raw), `"killmail_id":800001`) {
		t.Errorf("stored detail does not carry the killmail: %s", raw)
	}
	// The pilot's public feed shows the participation and the
	// loss.
	feed, err := q.ListKillsByCharacter(ctx, db.ListKillsByCharacterParams{
		EntityID: 91000001, BeforeTime: time.Now().Add(time.Hour).UTC(), BeforeID: 1 << 62, MaxRows: 10,
	})
	if err != nil {
		t.Fatalf("ListKillsByCharacter: %v", err)
	}
	ids := feedIDs(feed)
	if !ids[800001] || !ids[800002] {
		t.Errorf("pilot feed ids = %v, want 800001 and 800002", ids)
	}
	// The profile folded both and is marked backfilled.
	prof, err := q.GetPilotActivityProfile(ctx, 91000001)
	if err != nil {
		t.Fatalf("GetPilotActivityProfile: %v", err)
	}
	if prof.Kills != 1 || prof.Losses != 1 {
		t.Errorf("profile folds = %d kills, %d losses, want 1 and 1", prof.Kills, prof.Losses)
	}
	if !prof.BackfilledAt.Valid {
		t.Error("profile not marked backfilled")
	}
	// A covered pilot is not fetched again.
	app.backfillPilot(ctx, client, 91000001)
	if got := hist.hitCount("/characterID/91000001/"); got != 1 {
		t.Errorf("pilot history fetched %d times, want 1", got)
	}

	// The corporation backfill ingests a member's participation
	// the same way and marks the corporation covered.
	app.backfillCorp(ctx, client, 98000002)
	cfeed, err := q.ListKillsByCorporation(ctx, db.ListKillsByCorporationParams{
		EntityID: 98000002, BeforeTime: time.Now().Add(time.Hour).UTC(), BeforeID: 1 << 62, MaxRows: 10,
	})
	if err != nil {
		t.Fatalf("ListKillsByCorporation: %v", err)
	}
	if !feedIDs(cfeed)[800003] {
		t.Errorf("corp feed ids = %v, want 800003", feedIDs(cfeed))
	}
	if _, err := q.GetCorpKillCoverage(ctx, 98000002); err != nil {
		t.Errorf("corp coverage marker: %v", err)
	}
	app.backfillCorp(ctx, client, 98000002)
	if got := hist.hitCount("/corporationID/98000002/"); got != 1 {
		t.Errorf("corp history fetched %d times, want 1", got)
	}
}

func TestZkillBackfillPromotesPrivateKill(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	hist := newFakeZkillHistory(map[string]string{
		"/characterID/91000001/": "[" + zkPartEntry + "]",
	})
	srv := httptest.NewServer(hist.handler())
	defer srv.Close()
	app.cfg.zkillAPIBaseURL = srv.URL + "/"
	ctx := context.Background()

	// The kill is already held, privately, from an ESI pull.
	if _, err := app.ingestKill(ctx, json.RawMessage(zkPartKillmail), "hash800001", nil, "esi", false); err != nil {
		t.Fatalf("ingest private copy: %v", err)
	}
	feed, err := q.ListKillsByCharacter(ctx, db.ListKillsByCharacterParams{
		EntityID: 91000001, BeforeTime: time.Now().Add(time.Hour).UTC(), BeforeID: 1 << 62, MaxRows: 10,
	})
	if err != nil {
		t.Fatalf("ListKillsByCharacter: %v", err)
	}
	if feedIDs(feed)[800001] {
		t.Fatal("private kill already shows in the public feed")
	}

	app.backfillPilot(ctx, srv.Client(), 91000001)

	// One row, now public; the attackers were not duplicated.
	var n int
	var pub bool
	if err := app.db.QueryRowContext(ctx,
		`SELECT count(*), bool_or(public) FROM kills WHERE killmail_id = 800001`).Scan(&n, &pub); err != nil {
		t.Fatalf("read kill: %v", err)
	}
	if n != 1 || !pub {
		t.Errorf("kill rows = %d, public = %v, want 1 and true", n, pub)
	}
	var attackers int
	if err := app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM kill_attackers WHERE killmail_id = 800001`).Scan(&attackers); err != nil {
		t.Fatalf("count attackers: %v", err)
	}
	if attackers != 2 {
		t.Errorf("attacker rows = %d, want 2 (no duplicate ingest)", attackers)
	}
	feed, err = q.ListKillsByCharacter(ctx, db.ListKillsByCharacterParams{
		EntityID: 91000001, BeforeTime: time.Now().Add(time.Hour).UTC(), BeforeID: 1 << 62, MaxRows: 10,
	})
	if err != nil {
		t.Fatalf("ListKillsByCharacter: %v", err)
	}
	if !feedIDs(feed)[800001] {
		t.Error("promoted kill missing from the public feed")
	}
}

func TestKillmailPageListsBoardOnlyKill(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	seedCharacter(t, q, user.ID, 90000001, "Board Owner")

	// The ESI recent list knows one kill: the owner's final blow
	// on Ref Victim, with its detail stored.
	seedSnapshot(t, q, 90000001, esi.SnapKillmails,
		[]esi.KillmailRef{{KillmailID: 7001, KillmailHash: "refhash"}})
	refDetail := `{"killmail_id":7001,"killmail_time":"2026-10-21T05:00:00Z","solar_system_id":30000142,` +
		`"victim":{"character_id":93000009,"corporation_id":98000009,"ship_type_id":587,"damage_taken":100,"items":[]},` +
		`"attackers":[{"character_id":90000001,"corporation_id":98000002,"ship_type_id":587,"damage_done":100,"final_blow":true}]}`
	if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
		KillmailID: 7001, CharacterID: 90000001, Hash: "refhash", Payload: refDetail, FetchedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertKillmailDetail: %v", err)
	}

	// The board alone holds a newer kill: the owner took part
	// without the final blow, so the recent list cannot show it.
	boardKill := `{"killmail_id":800010,"killmail_time":"2026-10-22T05:00:00Z","solar_system_id":30000142,` +
		`"victim":{"character_id":93000001,"corporation_id":98000009,"ship_type_id":626,"damage_taken":300,"items":[]},` +
		`"attackers":[{"character_id":93000002,"corporation_id":98000003,"ship_type_id":587,"damage_done":200,"final_blow":true,"weapon_type_id":2488},` +
		`{"character_id":90000001,"corporation_id":98000002,"ship_type_id":587,"damage_done":100,"final_blow":false,"weapon_type_id":2488}]}`
	zkb := &killZKB{Hash: "boardhash"}
	if _, err := app.ingestKill(ctx, json.RawMessage(boardKill), "boardhash", zkb, "zkill", true); err != nil {
		t.Fatalf("ingest board kill: %v", err)
	}

	for _, stmt := range []string{
		`INSERT INTO sde_systems (system_id, name, security) VALUES (30000142, 'Jita', 0.9)`,
		`INSERT INTO sde_types (type_id, name) VALUES (587, 'Rifter'), (626, 'Vexor')`,
	} {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	app.esi.StoreCharacterName(93000001, "Board Victim")
	app.esi.StoreCharacterName(93000009, "Ref Victim")
	app.esi.StoreCharacterName(93000002, "Board Striker")
	app.esi.StoreCharacterName(90000001, "Board Owner")

	cookie := sessionCookie(t, app, user.ID, 90000001, "Board Owner")
	code, body := getPage(t, app, cookie, "/killmails/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /killmails/ = %d, want 200", code)
	}
	mustContain(t, "/killmails/", body, "Board Victim", "Ref Victim")
	// Newest first: the board-only kill (Oct 22) ahead of the
	// recent list's kill (Oct 21).
	bi, ri := strings.Index(body, "Board Victim"), strings.Index(body, "Ref Victim")
	if bi > ri {
		t.Errorf("board-only kill listed after the older ESI kill (positions %d, %d)", bi, ri)
	}
}
