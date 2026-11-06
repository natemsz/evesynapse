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
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestPilotBackfillRestoresPrePermanenceProfile pins the one
// exception to the backfill's once-only rule: a profile stamped
// before kill involvement became permanent (the schema 127
// step's applied_at, set to a fixed time here) is walked once
// more on offer, and its stamp moves past the cutoff, while a
// profile stamped after the cutoff is left alone. Without the
// exception, pilots whose involvement was pruned before it was
// kept would understate their record forever. The stale pilot
// here is production-shaped: the original walk already folded
// the kills into the aggregates, and the fold's markers are
// gone (the retention prune takes them on the same line as
// the detail), so the restoration walk must restore the
// involvement by ingest alone and leave the counts exactly
// where the first walk put them.
func TestPilotBackfillRestoresPrePermanenceProfile(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := conn.ExecContext(ctx,
		`UPDATE schema_migrations SET applied_at = $1 WHERE version = 127`, cutoff); err != nil {
		t.Fatalf("set involvement cutoff: %v", err)
	}
	hist := newFakeZkillHistory(map[string]string{
		"/characterID/91000001/": "[" + zkPartEntry + "," + zkLossEntry + "]",
		"/characterID/91000002/": "[" + zkCorpEntry + "]",
	})
	srv := httptest.NewServer(hist.handler())
	defer srv.Close()
	app.cfg.zkillAPIBaseURL = srv.URL + "/"
	client := srv.Client()

	stamp := func(id int64, at time.Time) {
		t.Helper()
		if err := q.ClaimPilotActivityProfile(ctx, db.ClaimPilotActivityProfileParams{CharacterID: id, UpdatedAt: at}); err != nil {
			t.Fatalf("claim profile %d: %v", id, err)
		}
		if err := q.MarkPilotActivityBackfilled(ctx, db.MarkPilotActivityBackfilledParams{
			CharacterID: id, BackfilledAt: sql.NullTime{Time: at, Valid: true},
		}); err != nil {
			t.Fatalf("stamp profile %d: %v", id, err)
		}
	}
	const stalePilot, freshPilot = int64(91000001), int64(91000002)

	// The stale pilot's original walk: the two kills folded
	// into the aggregates, the stamp set before the cutoff,
	// and the fold markers since pruned.
	var walked []zkillHistoryEntry
	if err := json.Unmarshal([]byte("["+zkPartEntry+","+zkLossEntry+"]"), &walked); err != nil {
		t.Fatalf("decode walked history: %v", err)
	}
	if err := q.ClaimPilotActivityProfile(ctx, db.ClaimPilotActivityProfileParams{CharacterID: stalePilot, UpdatedAt: cutoff.Add(-time.Hour)}); err != nil {
		t.Fatalf("claim stale profile: %v", err)
	}
	for _, e := range walked {
		if _, err := app.applyPilotKill(ctx, stalePilot, e.killmailFull, false); err != nil {
			t.Fatalf("fold walked kill %d: %v", e.KillmailID, err)
		}
	}
	if err := q.MarkPilotActivityBackfilled(ctx, db.MarkPilotActivityBackfilledParams{
		CharacterID: stalePilot, BackfilledAt: sql.NullTime{Time: cutoff.Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("stamp stale profile: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM pilot_activity_applied WHERE character_id = $1`, stalePilot); err != nil {
		t.Fatalf("prune fold markers: %v", err)
	}
	stamp(freshPilot, cutoff.Add(time.Hour))

	// The restoration listing names the stale pilot only.
	stale, err := q.ListStalePilotProfiles(ctx, db.ListStalePilotProfilesParams{Before: cutoff, MaxRows: 25})
	if err != nil {
		t.Fatalf("ListStalePilotProfiles: %v", err)
	}
	if len(stale) != 1 || stale[0] != stalePilot {
		t.Fatalf("stale profiles = %v, want [%d]", stale, stalePilot)
	}

	// The stale pilot is walked again: the history is fetched,
	// the participation lands in the permanent involvement
	// record, the aggregates keep the first walk's counts, and
	// the stamp moves past the cutoff.
	app.backfillPilot(ctx, client, stalePilot)
	if got := hist.hitCount("/characterID/91000001/"); got != 1 {
		t.Errorf("stale pilot history fetched %d times, want 1", got)
	}
	var involvement int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM kill_involvement WHERE character_id = $1`, stalePilot).Scan(&involvement); err != nil {
		t.Fatalf("count involvement: %v", err)
	}
	if involvement != 1 {
		t.Errorf("involvement rows for the stale pilot = %d, want 1 (the participation)", involvement)
	}
	prof, err := q.GetPilotActivityProfile(ctx, stalePilot)
	if err != nil {
		t.Fatalf("GetPilotActivityProfile: %v", err)
	}
	if prof.Kills != 1 || prof.Losses != 1 {
		t.Errorf("restored profile counts = %d kills, %d losses, want the first walk's 1 and 1", prof.Kills, prof.Losses)
	}
	if !prof.BackfilledAt.Valid || !prof.BackfilledAt.Time.After(cutoff) {
		t.Errorf("restored stamp = %v, want a time past the cutoff %v", prof.BackfilledAt, cutoff)
	}
	// And the exception has closed behind them: a second offer
	// fetches nothing.
	app.backfillPilot(ctx, client, stalePilot)
	if got := hist.hitCount("/characterID/91000001/"); got != 1 {
		t.Errorf("restored pilot history fetched %d times, want still 1", got)
	}

	// The fresh pilot is left alone: no fetch, stamp untouched.
	app.backfillPilot(ctx, client, freshPilot)
	if got := hist.hitCount("/characterID/91000002/"); got != 0 {
		t.Errorf("fresh pilot history fetched %d times, want 0", got)
	}
	prof, err = q.GetPilotActivityProfile(ctx, freshPilot)
	if err != nil {
		t.Fatalf("GetPilotActivityProfile: %v", err)
	}
	if !prof.BackfilledAt.Valid || !prof.BackfilledAt.Time.Equal(cutoff.Add(time.Hour)) {
		t.Errorf("fresh stamp = %v, want the original %v", prof.BackfilledAt, cutoff.Add(time.Hour))
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
	if _, err := app.ingestKill(ctx, json.RawMessage(zkPartKillmail), "hash800001", nil, "esi", false, true); err != nil {
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

// TestKillInvolvementFiling pins the filing rules end to end.
// The stream stores a kill no registered character touched,
// exactly as it stores every kill. A kill a registered character
// fought in without the final blow shows in the character's
// board kills, the list My Killmails unions in. And a history
// walk reaches back without a depth cap: a walked kill past the
// retention cutoff lands in its post-prune shape (summary row
// and priced lines, no attackers, no stored killmail), while a
// walked kill inside the year lands with full detail.
func TestKillInvolvementFiling(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	seedCharacter(t, q, user.ID, 9002, "Wing")

	if _, err := app.db.ExecContext(ctx, `INSERT INTO market_region_stats
		(region_id, type_id, typical_sell, updated_at) VALUES (10000002, 587, 400000, now())`); err != nil {
		t.Fatalf("seed region stats: %v", err)
	}

	streamKill := func(id int64, esi string) {
		t.Helper()
		frame := fmt.Sprintf(`{"killmail_id":%d,"hash":"h%d","esi":%s,`+
			`"zkb":{"locationID":60003760,"totalValue":400000,"points":1,"npc":false,"solo":false,"awox":false},"sequence_id":1}`,
			id, id, esi)
		if err := app.ingestKillPackage(ctx, []byte(frame)); err != nil {
			t.Fatalf("stream kill %d: %v", id, err)
		}
	}

	// A stream kill between strangers only: stored, public.
	streamKill(800101, `{"killmail_id":800101,"killmail_time":"2026-10-21T10:00:00Z","solar_system_id":30000142,`+
		`"victim":{"character_id":9103,"corporation_id":98000101,"ship_type_id":587,"damage_taken":900,"items":[]},`+
		`"attackers":[{"character_id":9104,"corporation_id":98000102,"ship_type_id":587,"weapon_type_id":2873,"damage_done":900,"final_blow":true}]}`)
	stranger, err := q.GetKill(ctx, 800101)
	if err != nil {
		t.Fatalf("stream kill with no registered involvement was not stored: %v", err)
	}
	if !stranger.Public {
		t.Error("stream kill stored non-public")
	}

	// A stream kill the registered character fought in without
	// the final blow: in their board kills; the strangers' kill
	// is not.
	streamKill(800102, `{"killmail_id":800102,"killmail_time":"2026-10-21T11:00:00Z","solar_system_id":30000142,`+
		`"victim":{"character_id":9106,"corporation_id":98000101,"ship_type_id":587,"damage_taken":905,"items":[]},`+
		`"attackers":[{"character_id":9107,"corporation_id":98000102,"ship_type_id":587,"weapon_type_id":2873,"damage_done":900,"final_blow":true},`+
		`{"character_id":9002,"corporation_id":98000111,"ship_type_id":587,"damage_done":5,"final_blow":false}]}`)
	feed, err := q.ListKillsByCharacter(ctx, db.ListKillsByCharacterParams{
		EntityID: 9002, BeforeTime: time.Now().Add(time.Hour).UTC(), BeforeID: 1 << 62, MaxRows: 10,
	})
	if err != nil {
		t.Fatalf("ListKillsByCharacter: %v", err)
	}
	ids := feedIDs(feed)
	if !ids[800102] {
		t.Error("non-final-blow involvement kill missing from the character's board kills")
	}
	if ids[800101] {
		t.Error("a kill the character never touched is in their board kills")
	}

	// The history walk: one page holding a kill inside the year
	// and a loss three years back. The old one still arrives
	// (no depth cap) but lands pruned; the young one lands whole.
	youngAt := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02T15:04:05Z")
	oldAt := time.Now().UTC().AddDate(-3, 0, 0).Format("2006-01-02T15:04:05Z")
	youngEntry := fmt.Sprintf(`{"killmail_id":800201,"killmail_time":%q,"solar_system_id":30000142,`+
		`"victim":{"character_id":9108,"corporation_id":98000101,"ship_type_id":587,"damage_taken":905,"items":[]},`+
		`"attackers":[{"character_id":9109,"corporation_id":98000102,"ship_type_id":587,"weapon_type_id":2873,"damage_done":900,"final_blow":true},`+
		`{"character_id":9002,"corporation_id":98000111,"ship_type_id":587,"damage_done":5,"final_blow":false}],`+
		`"zkb":{"locationID":0,"hash":"hash800201","totalValue":10.0,"points":1,"npc":false,"solo":false,"awox":false}}`, youngAt)
	oldEntry := fmt.Sprintf(`{"killmail_id":800202,"killmail_time":%q,"solar_system_id":30000142,`+
		`"victim":{"character_id":9002,"corporation_id":98000111,"ship_type_id":587,"damage_taken":900,"items":[]},`+
		`"attackers":[{"character_id":9110,"corporation_id":98000102,"ship_type_id":587,"weapon_type_id":2873,"damage_done":900,"final_blow":true}],`+
		`"zkb":{"locationID":0,"hash":"hash800202","totalValue":10.0,"points":1,"npc":false,"solo":false,"awox":false}}`, oldAt)
	hist := newFakeZkillHistory(map[string]string{
		"/characterID/9002/": "[" + youngEntry + "," + oldEntry + "]",
	})
	srv := httptest.NewServer(hist.handler())
	defer srv.Close()
	app.cfg.zkillAPIBaseURL = srv.URL + "/"

	app.backfillPilot(ctx, srv.Client(), 9002)

	old, err := q.GetKill(ctx, 800202)
	if err != nil {
		t.Fatalf("three-year-old walked kill was not stored: %v", err)
	}
	if !old.KillmailTime.Before(time.Now().AddDate(-2, 0, 0)) {
		t.Errorf("walked old kill time = %v, want more than two years back", old.KillmailTime)
	}
	lines, err := q.GetKillLinePrices(ctx, 800202)
	if err != nil {
		t.Fatalf("old kill line prices: %v", err)
	}
	hullPriced := false
	for _, l := range lines {
		if l.TypeID == 587 && l.UnitPrice > 0 {
			hullPriced = true
		}
	}
	if !hullPriced {
		t.Errorf("old kill priced lines = %+v, want the hull priced", lines)
	}
	var attackers int
	if err := app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM kill_attackers WHERE killmail_id = 800202`).Scan(&attackers); err != nil {
		t.Fatalf("count old kill attackers: %v", err)
	}
	if attackers != 0 {
		t.Errorf("old kill stored %d attacker rows, want none", attackers)
	}
	if _, err := q.GetKillDetail(ctx, 800202); err == nil {
		t.Error("old kill stored its full killmail, want post-prune shape only")
	}

	if err := app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM kill_attackers WHERE killmail_id = 800201`).Scan(&attackers); err != nil {
		t.Fatalf("count young kill attackers: %v", err)
	}
	if attackers != 2 {
		t.Errorf("young walked kill stored %d attacker rows, want 2", attackers)
	}
	if _, err := q.GetKillDetail(ctx, 800201); err != nil {
		t.Errorf("young walked kill lost its stored killmail: %v", err)
	}
}

// TestKillValueSurvivesPrune: a kill priced at ingest keeps its
// value credited after its detail is pruned. The kill's attacker
// detail and stored killmail go at the retention cutoff; the
// pilot's kill count, the ISK destroyed and the kill's place in
// the pilot's board kills stay, read from the permanent
// involvement record.
func TestKillValueSurvivesPrune(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()

	if _, err := app.db.ExecContext(ctx, `INSERT INTO market_region_stats
		(region_id, type_id, typical_sell, updated_at) VALUES (10000002, 587, 400000, now())`); err != nil {
		t.Fatalf("seed region stats: %v", err)
	}

	// One kill, priced at ingest from the region stats: a
	// 400,000 ISK hull, destroyed by two attackers. The pilot
	// asserted on is the one without the final blow.
	at := time.Now().UTC().Add(-2 * time.Hour)
	esi := fmt.Sprintf(`{"killmail_id":800301,"killmail_time":%q,"solar_system_id":30000142,`+
		`"victim":{"character_id":9201,"corporation_id":98000101,"ship_type_id":587,"damage_taken":900,"items":[]},`+
		`"attackers":[{"character_id":9202,"corporation_id":98000201,"alliance_id":99000201,"ship_type_id":587,"weapon_type_id":2873,"damage_done":800,"final_blow":true},`+
		`{"character_id":9203,"corporation_id":98000201,"alliance_id":99000201,"ship_type_id":587,"damage_done":100,"final_blow":false}]}`,
		at.Format("2006-01-02T15:04:05Z"))
	frame := fmt.Sprintf(`{"killmail_id":800301,"hash":"h800301","esi":%s,`+
		`"zkb":{"locationID":60003760,"totalValue":400000,"points":1,"npc":false,"solo":false,"awox":false},"sequence_id":1}`, esi)
	if err := app.ingestKillPackage(ctx, []byte(frame)); err != nil {
		t.Fatalf("ingest kill: %v", err)
	}
	stored, err := q.GetKill(ctx, 800301)
	if err != nil {
		t.Fatalf("stored kill: %v", err)
	}
	if !stored.ValueOurs.Valid || stored.ValueOurs.Float64 != 400000 {
		t.Fatalf("kill value_ours = %v, want 400000 priced at ingest", stored.ValueOurs)
	}

	since := at.Add(-time.Hour)
	stats := func() db.PilotKillStatsRow {
		t.Helper()
		row, err := q.PilotKillStats(ctx, db.PilotKillStatsParams{CharacterID: 9203, Since: since})
		if err != nil {
			t.Fatalf("PilotKillStats: %v", err)
		}
		return row
	}
	if got := stats(); got.Kills != 1 || got.IskKilled != 400000 {
		t.Fatalf("stats before prune = %+v, want 1 kill and 400000 ISK destroyed", got)
	}

	// The retention prune, run as the daily pass runs it once
	// the kill is past the cutoff.
	if _, err := q.PruneKillDetails(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("prune details: %v", err)
	}
	if _, err := q.PruneKillAttackers(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("prune attackers: %v", err)
	}
	var attackers int
	if err := app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM kill_attackers WHERE killmail_id = 800301`).Scan(&attackers); err != nil {
		t.Fatalf("count attackers: %v", err)
	}
	if attackers != 0 {
		t.Fatalf("attacker rows after prune = %d, want 0 (the prune must still take the detail)", attackers)
	}
	if _, err := q.GetKillDetail(ctx, 800301); err == nil {
		t.Fatal("stored killmail survived the prune, want it gone")
	}

	if got := stats(); got.Kills != 1 || got.IskKilled != 400000 {
		t.Errorf("stats after prune = %+v, want 1 kill and 400000 ISK destroyed", got)
	}
	feed, err := q.ListKillsByCharacter(ctx, db.ListKillsByCharacterParams{
		EntityID: 9203, BeforeTime: time.Now().Add(time.Hour).UTC(), BeforeID: 1 << 62, MaxRows: 10,
	})
	if err != nil {
		t.Fatalf("ListKillsByCharacter: %v", err)
	}
	if !feedIDs(feed)[800301] {
		t.Error("pruned kill missing from the attacker's board kills")
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
	if _, err := app.ingestKill(ctx, json.RawMessage(boardKill), "boardhash", zkb, "zkill", true, false); err != nil {
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

// TestKillmailPagesPastFifty: the killmail page pages past its
// first 50 rows. Page 1 holds the board's newest 50 kills and an
// Older link; page 2, read through that link, holds the older
// kills with no repeats and no gaps, and the walk ends there.
func TestKillmailPagesPastFifty(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	seedCharacter(t, q, user.ID, 90000001, "Pager One")

	// The recent list is loaded but empty: every row on both
	// pages comes from the board.
	seedSnapshot(t, q, 90000001, esi.SnapKillmails, []esi.KillmailRef{})

	// Sixty board kills an hour apart, the pilot on the
	// attacking side of each: more than one page of history.
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 60; i++ {
		id := int64(900000 + i)
		at := base.Add(time.Duration(i) * time.Hour)
		if _, err := app.db.ExecContext(ctx, `
			INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, victim_corporation_id,
				ship_type_id, attacker_count, fb_character_id, source, ingested_at)
			VALUES ($1, $2, 30000142, 93000001, 98000009, 587, 2, 93000002, 'zkill', $2)`, id, at); err != nil {
			t.Fatalf("seed kill %d: %v", id, err)
		}
		if _, err := app.db.ExecContext(ctx, `
			INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, ship_type_id, final_blow)
			VALUES ($1, 0, 93000002, 98000003, 587, true), ($1, 1, 90000001, 98000002, 587, false)`, id); err != nil {
			t.Fatalf("seed attackers %d: %v", id, err)
		}
		if _, err := app.db.ExecContext(ctx, `
			INSERT INTO kill_involvement (killmail_id, character_id, corporation_id)
			VALUES ($1, 93000002, 98000003), ($1, 90000001, 98000002)`, id); err != nil {
			t.Fatalf("seed involvement %d: %v", id, err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO sde_systems (system_id, name, security) VALUES (30000142, 'Jita', 0.9)`,
		`INSERT INTO sde_types (type_id, name) VALUES (587, 'Rifter')`,
	} {
		if _, err := app.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	// rowIDs reads the page's kill links in render order.
	rowIDs := func(body string) []int64 {
		var ids []int64
		rest := body
		for {
			i := strings.Index(rest, "kill=")
			if i < 0 {
				return ids
			}
			rest = rest[i+len("kill="):]
			j := 0
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			id, err := strconv.ParseInt(rest[:j], 10, 64)
			if err != nil {
				t.Fatalf("parse row kill id: %v", err)
			}
			ids = append(ids, id)
		}
	}

	cookie := sessionCookie(t, app, user.ID, 90000001, "Pager One")
	code, body := getPage(t, app, cookie, "/killmails/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /killmails/ = %d, want 200", code)
	}
	first := rowIDs(body)
	if len(first) != maxKillmailsShown {
		t.Fatalf("page 1 rows = %d, want %d", len(first), maxKillmailsShown)
	}
	for i, id := range first {
		if want := int64(900060 - i); id != want {
			t.Fatalf("page 1 row %d = kill %d, want %d", i, id, want)
		}
	}

	// The Older link carries the cursor of the oldest board row
	// on the page: kill 900011, eleven hours past the base.
	bi := strings.Index(body, "before=")
	if bi < 0 {
		t.Fatalf("page 1 has no Older kills link")
	}
	hs := strings.LastIndex(body[:bi], `href="`) + len(`href="`)
	he := bi + strings.Index(body[bi:], `"`)
	older := strings.ReplaceAll(body[hs:he], "&amp;", "&")
	wantCursor := fmt.Sprintf("before=%d.%d", base.Add(11*time.Hour).Unix(), 900011)
	if !strings.Contains(older, wantCursor) {
		t.Fatalf("Older href %q does not carry cursor %q", older, wantCursor)
	}

	code, body = getPage(t, app, cookie, older)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", older, code)
	}
	second := rowIDs(body)
	if len(second) != 10 {
		t.Fatalf("page 2 rows = %d, want 10", len(second))
	}
	for i, id := range second {
		if want := int64(900010 - i); id != want {
			t.Fatalf("page 2 row %d = kill %d, want %d", i, id, want)
		}
	}
	if strings.Contains(body, "Older kills") {
		t.Errorf("page 2 still offers an Older kills link at the end of the history")
	}
}
