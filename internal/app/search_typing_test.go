package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// Typing-intent guesses (schema 028, worker_typing.go): search
// boxes queue their top suggestions ahead of the pick at the
// typing ring, each keystroke replacing the last.

func TestTypingSliceComputation(t *testing.T) {
	for _, tc := range []struct {
		budget int
		want   int
	}{
		{50, 5},
		{120, 12},
		{180, 12},
		{500, 12},
	} {
		app := &Application{cfg: Config{workerFetches: tc.budget}}
		if got := app.typingSlice(); got != tc.want {
			t.Fatalf("typingSlice with budget %d = %d, want %d", tc.budget, got, tc.want)
		}
	}
	if got := (&Application{}).typingSlice(); got != 12 {
		t.Fatalf("typingSlice with default budget = %d, want 12", got)
	}
}

func typingLedgerCount(t *testing.T, conn *sql.DB, box string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM typing_guesses WHERE box = $1`, box).Scan(&n); err != nil {
		t.Fatalf("count guesses for box %q: %v", box, err)
	}
	return n
}

func historyWantPriority(t *testing.T, conn *sql.DB, regionID, typeID int64) (int64, bool) {
	t.Helper()
	var priority int64
	err := conn.QueryRowContext(context.Background(),
		`SELECT priority FROM market_history_wants WHERE region_id = $1 AND type_id = $2`,
		regionID, typeID).Scan(&priority)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read history want priority for %d in %d: %v", typeID, regionID, err)
	}
	return priority, true
}

func TestTypingGuessesQueueAtTypingTier(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	box := "7|market"
	app.noteSuggested(ctx, box, suggestPoolMarket, []suggestItem{
		{ID: 34, Name: "Tritanium"},
		{ID: 35, Name: "Pyerite"},
	})
	if n := typingLedgerCount(t, conn, box); n != 2 {
		t.Fatalf("ledger rows for box %q = %d, want 2", box, n)
	}
	for _, id := range []int64{34, 35} {
		priority, ok := historyWantPriority(t, conn, defaultMarketRegion, id)
		if !ok || priority != wantTyping {
			t.Fatalf("history want for %d: priority=%d present=%v, want %d/true", id, priority, ok, wantTyping)
		}
	}

	detailBox := "7|items:all"
	app.noteSuggested(ctx, detailBox, suggestPoolAll, []suggestItem{{ID: 36, Name: "Mexallon"}})
	if n := typingLedgerCount(t, conn, detailBox); n != 1 {
		t.Fatalf("ledger rows for box %q = %d, want 1", detailBox, n)
	}
	var priority int64
	if err := conn.QueryRowContext(ctx,
		`SELECT priority FROM type_details WHERE type_id = 36`).Scan(&priority); err != nil {
		t.Fatalf("read detail want priority: %v", err)
	}
	if priority != wantTyping {
		t.Fatalf("detail want priority = %d, want %d", priority, wantTyping)
	}
}

func TestTypingGuessesReplacePerKeystroke(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now().UTC()

	box, other := "7|market", "9|market"
	app.noteSuggested(ctx, box, suggestPoolMarket, []suggestItem{
		{ID: 34, Name: "Tritanium"},
		{ID: 35, Name: "Pyerite"},
	})
	app.noteSuggested(ctx, other, suggestPoolMarket, []suggestItem{{ID: 36, Name: "Mexallon"}})
	// Second keystroke in the first box replaces its guesses.
	app.noteSuggested(ctx, box, suggestPoolMarket, []suggestItem{{ID: 37, Name: "Isogen"}})

	if n := typingLedgerCount(t, conn, box); n != 1 {
		t.Fatalf("ledger rows for box %q = %d, want 1 (the latest keystroke)", box, n)
	}
	if _, err := q.GetTypingGuess(ctx, db.GetTypingGuessParams{
		Box: box, Kind: string(pageWantHistory), EntityID: 37, RegionID: defaultMarketRegion,
	}); err != nil {
		t.Fatalf("latest guess missing from ledger: %v", err)
	}
	if _, err := q.GetTypingGuess(ctx, db.GetTypingGuessParams{
		Box: box, Kind: string(pageWantHistory), EntityID: 34, RegionID: defaultMarketRegion,
	}); err != sql.ErrNoRows {
		t.Fatalf("replaced guess still in ledger: err=%v, want ErrNoRows", err)
	}
	for _, id := range []int64{34, 35} {
		var n int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM market_history_wants WHERE region_id = $1 AND type_id = $2`,
			defaultMarketRegion, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("replaced queue row for %d: count=%d err=%v, want 0/nil", id, n, err)
		}
	}
	if priority, ok := historyWantPriority(t, conn, defaultMarketRegion, 37); !ok || priority != wantTyping {
		t.Fatalf("latest guess want: priority=%d present=%v, want %d/true", priority, ok, wantTyping)
	}
	// The other box is untouched.
	if n := typingLedgerCount(t, conn, other); n != 1 {
		t.Fatalf("ledger rows for box %q = %d, want 1", other, n)
	}
	if priority, ok := historyWantPriority(t, conn, defaultMarketRegion, 36); !ok || priority != wantTyping {
		t.Fatalf("other box want: priority=%d present=%v, want %d/true", priority, ok, wantTyping)
	}

	// A guess the pilot opened keeps its viewed ring across a
	// replacement; a pending one goes.
	viewBox := "7|topbar"
	app.noteTypingPilotGuess(ctx, viewBox, 93300001, now)
	if err := app.wantPilot(ctx, 93300003, wantViewed); err != nil {
		t.Fatalf("seed viewed pilot: %v", err)
	}
	app.noteTypingPilotGuess(ctx, viewBox, 93300003, now)
	app.replaceTypingGuesses(ctx, viewBox)
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pilot_records WHERE character_id = 93300001`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("replaced pending pilot guess: count=%d err=%v, want 0/nil", n, err)
	}
	var priority int64
	if err := conn.QueryRowContext(ctx,
		`SELECT priority FROM pilot_records WHERE character_id = 93300003`).Scan(&priority); err != nil {
		t.Fatalf("read bumped pilot priority: %v", err)
	}
	if priority != wantViewed {
		t.Fatalf("opened guess priority = %d, want viewed %d", priority, wantViewed)
	}

	// A fetched detail is a record now, not a guess: replacement
	// leaves it alone.
	app.noteTypingDetailGuess(ctx, viewBox, 38, now)
	if err := q.SetTypeDetail(ctx, db.SetTypeDetailParams{
		TypeID: 38, Description: "has text", FetchedAt: timeSet(now),
	}); err != nil {
		t.Fatalf("seed fetched detail: %v", err)
	}
	app.replaceTypingGuesses(ctx, viewBox)
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM type_details WHERE type_id = 38`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("fetched detail after replace: count=%d err=%v, want 1/nil", n, err)
	}
}

func TestTypingSliceCapInDrain(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now().UTC()

	for i := int64(0); i < 20; i++ {
		if err := q.UpsertPilotWant(ctx, db.UpsertPilotWantParams{
			CharacterID: 93400001 + i, Priority: wantTyping, NotedAt: timeSet(now),
		}); err != nil {
			t.Fatalf("seed typing want: %v", err)
		}
	}
	drains := func(limit int64) []int64 {
		t.Helper()
		ids, err := q.ListPilotDrains(ctx, db.ListPilotDrainsParams{
			TypingPriority: wantTyping,
			StaleCutoff:    now.Add(-pilotStaleAfter),
			TypingLimit:    limit,
			DrainLimit:     100,
		})
		if err != nil {
			t.Fatalf("list drains: %v", err)
		}
		return ids
	}
	if ids := drains(int64(app.typingSlice())); len(ids) != app.typingSlice() {
		t.Fatalf("drain with the typing slice returned %d rows, want %d", len(ids), app.typingSlice())
	}
	if ids := drains(100); len(ids) != 20 {
		t.Fatalf("drain with a wide typing limit returned %d rows, want 20", len(ids))
	}
}

func TestDrainOrderViewedTypingOrbit(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now().UTC()

	seedPilot := func(id, closeness int64, noted time.Time) {
		t.Helper()
		if err := q.UpsertPilotWant(ctx, db.UpsertPilotWantParams{
			CharacterID: id, Priority: closeness, NotedAt: timeSet(noted),
		}); err != nil {
			t.Fatalf("seed pilot %d: %v", id, err)
		}
	}
	seedPilot(93500001, wantOrbit, now.Add(-2*time.Minute))
	seedPilot(93500002, wantTyping, now.Add(-time.Minute))
	seedPilot(93500003, wantViewed, now)
	ids, err := q.ListPilotDrains(ctx, db.ListPilotDrainsParams{
		TypingPriority: wantTyping,
		StaleCutoff:    now.Add(-pilotStaleAfter),
		TypingLimit:    int64(app.typingSlice()),
		DrainLimit:     10,
	})
	if err != nil {
		t.Fatalf("list drains: %v", err)
	}
	want := []int64{93500003, 93500002, 93500001}
	if len(ids) != len(want) {
		t.Fatalf("drain order = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("drain order = %v, want %v (viewed, then typing, then orbit)", ids, want)
		}
	}

	seedHistory := func(region, id, closeness int64) {
		t.Helper()
		if err := q.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: region, TypeID: id, LastRequestedAt: now, Priority: closeness,
		}); err != nil {
			t.Fatalf("seed history want %d: %v", id, err)
		}
	}
	seedHistory(defaultMarketRegion, 41, wantTyping)
	seedHistory(defaultMarketRegion, 42, wantViewed)
	wants, err := q.ListMarketHistoryWants(ctx, db.ListMarketHistoryWantsParams{
		LastRequestedAt: time.Time{},
		TypingPriority:  wantTyping,
		TypingLimit:     int64(app.typingSlice()),
	})
	if err != nil {
		t.Fatalf("list history wants: %v", err)
	}
	if len(wants) != 2 || wants[0].TypeID != 42 || wants[1].TypeID != 41 {
		t.Fatalf("history drain order = %v, want viewed 42 then typing 41", wants)
	}
}

func TestFullNameSearchNoDoubleQueue(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	nameWants := func() int {
		t.Helper()
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pilot_name_wants`).Scan(&n); err != nil {
			t.Fatalf("count name wants: %v", err)
		}
		return n
	}
	records := func() int {
		t.Helper()
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pilot_records`).Scan(&n); err != nil {
			t.Fatalf("count pilot records: %v", err)
		}
		return n
	}

	// The full name queues exactly one resolution want and no record.
	hits := getSearchHits(t, app, cookie, "/search.json?q=Unwarmed%20Stranger")
	if len(hits) != 1 || hits[0].Kind != "pilot-pending" {
		t.Fatalf("full-name hits = %+v, want one pilot-pending row", hits)
	}
	if n := nameWants(); n != 1 {
		t.Fatalf("name wants after full-name search = %d, want 1", n)
	}
	if n := records(); n != 0 {
		t.Fatalf("pilot records after full-name search = %d, want 0", n)
	}
	// Asking again does not queue again.
	getSearchHits(t, app, cookie, "/search.json?q=Unwarmed%20Stranger")
	if n := nameWants(); n != 1 {
		t.Fatalf("name wants after second search = %d, want 1", n)
	}
	// A partial keystroke queues its own distinct want (the
	// documented per-string behavior), still no record.
	getSearchHits(t, app, cookie, "/search.json?q=Unwarmed%20Stran")
	if n := nameWants(); n != 2 {
		t.Fatalf("name wants after partial search = %d, want 2 (one per distinct string)", n)
	}
	if n := records(); n != 0 {
		t.Fatalf("pilot records after partial search = %d, want 0", n)
	}
}

func TestMarketSuggestQueuesTypingGuesses(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (900, 'Fixture Category')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (910, 'Fixture Minerals', 900)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (34, 'Tritanium', 910, 5, 1),
		   (35, 'Tritanium Alloy', 910, 5, 1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed SDE: %v", err)
		}
	}

	code, _ := getPage(t, app, cookie, "/market/suggest?q=tri")
	if code != http.StatusOK {
		t.Fatalf("suggest: status %d", code)
	}
	box := fmt.Sprintf("%d|market", user.ID)
	if n := typingLedgerCount(t, conn, box); n == 0 {
		t.Fatal("market suggest queued no guesses")
	}
	var typed int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM market_history_wants WHERE priority = $1`, wantTyping).Scan(&typed); err != nil || typed == 0 {
		t.Fatalf("typing history wants = %d err=%v, want at least 1", typed, err)
	}
}

func TestGuessHitOnOpen(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	box := "7|market"
	readGuess := func() db.TypingGuess {
		t.Helper()
		g, err := q.GetTypingGuess(ctx, db.GetTypingGuessParams{
			Box: box, Kind: string(pageWantHistory), EntityID: 34, RegionID: defaultMarketRegion,
		})
		if err != nil {
			t.Fatalf("read guess: %v", err)
		}
		return g
	}

	app.noteTypingHistoryGuess(ctx, box, defaultMarketRegion, 34, time.Now().UTC())
	if g := readGuess(); g.HitAt.Valid {
		t.Fatal("fresh guess already marked hit")
	}
	app.markGuessHit(ctx, pageWantHistory, 34, defaultMarketRegion)
	if g := readGuess(); !g.HitAt.Valid {
		t.Fatal("guess still unhit after the open")
	}
	first := readGuess().HitAt.Time
	app.markGuessHit(ctx, pageWantHistory, 34, defaultMarketRegion)
	if g := readGuess(); !g.HitAt.Time.Equal(first) {
		t.Fatal("second open moved the hit; the first one stands")
	}
}

func TestGuessHitWindowStale(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	if err := q.InsertTypingGuess(ctx, db.InsertTypingGuessParams{
		Box: "7|market", Kind: string(pageWantHistory), EntityID: 35, RegionID: defaultMarketRegion,
		NotedAt: time.Now().UTC().Add(-11 * time.Minute),
	}); err != nil {
		t.Fatalf("seed stale guess: %v", err)
	}
	app.markGuessHit(ctx, pageWantHistory, 35, defaultMarketRegion)
	g, err := q.GetTypingGuess(ctx, db.GetTypingGuessParams{
		Box: "7|market", Kind: string(pageWantHistory), EntityID: 35, RegionID: defaultMarketRegion,
	})
	if err != nil {
		t.Fatalf("read stale guess: %v", err)
	}
	if g.HitAt.Valid {
		t.Fatal("stale guess marked hit after the window")
	}
}

// TestTypingBoxIgnoresUnknownPools: the items feed answers unknown
// pools from the full feed, and the guess box is the same
// normalized one — otherwise each junk pool value mints an orphan
// box whose guesses no keystroke ever replaces.
func TestTypingBoxIgnoresUnknownPools(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 18, 2, 1)`); err != nil {
		t.Fatalf("seed sde type: %v", err)
	}
	seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	cookie := sessionCookie(t, app, user.ID, 90000100, "Pilot 0")

	for _, pool := range []string{"junk-one", "junk-two"} {
		code, body := getPage(t, app, cookie, "/items/search.json?q=trit&pool="+pool)
		if code != http.StatusOK {
			t.Fatalf("search.json with pool %q: status %d", pool, code)
		}
		mustContain(t, "search.json with pool "+pool, body, "Tritanium")
	}

	// Both keystrokes collapsed into the single normalized box (the
	// second replacing the first), and no orphan boxes exist.
	wantBox := fmt.Sprintf("%d|items:all", user.ID)
	if n := typingLedgerCount(t, conn, wantBox); n != 1 {
		t.Fatalf("ledger rows for box %q = %d, want 1 (the latest keystroke)", wantBox, n)
	}
	var orphans int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM typing_guesses WHERE box LIKE '%junk%'`).Scan(&orphans); err != nil {
		t.Fatalf("count orphan boxes: %v", err)
	}
	if orphans != 0 {
		t.Errorf("orphan guess boxes for junk pools: %d rows, want 0", orphans)
	}
}

// TestTypingConcurrentKeystrokesDoNotMix: two keystrokes landing
// in one box at once must read as two whole steps — the ledger
// ends holding exactly one keystroke's guesses, never a mixture
// of both.
func TestTypingConcurrentKeystrokesDoNotMix(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	box := "7|market"
	for round := 0; round < 40; round++ {
		a, b := int64(1000+round*2), int64(1001+round*2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			app.noteSuggested(ctx, box, suggestPoolMarket, []suggestItem{{ID: a}})
		}()
		go func() {
			defer wg.Done()
			app.noteSuggested(ctx, box, suggestPoolMarket, []suggestItem{{ID: b}})
		}()
		wg.Wait()
		rows, err := conn.QueryContext(ctx,
			`SELECT entity_id FROM typing_guesses WHERE box = $1`, box)
		if err != nil {
			t.Fatalf("round %d: read ledger: %v", round, err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatalf("round %d: scan ledger: %v", round, err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) != 1 || (ids[0] != a && ids[0] != b) {
			t.Fatalf("round %d: ledger holds %v, want exactly [%d] or [%d]", round, ids, a, b)
		}
	}
}
