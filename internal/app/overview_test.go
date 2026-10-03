package app

// Hermetic tests for the Phase 1B widget overview home. Same
// contract as the corp/intel suites: seeded snapshots drive the
// real router against a counting transport, and the transport
// must observe zero outbound calls — the overview renders
// cache-only, across every linked character.

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedOverviewCharacter links one character to the user, tagged.
func seedOverviewCharacter(t *testing.T, q *db.Queries, userID, characterID int64, name, tags string) db.Character {
	t.Helper()
	ch := seedCharacter(t, q, userID, characterID, name)
	if tags != "" {
		if err := q.SetCharacterTags(context.Background(), db.SetCharacterTagsParams{
			Tags:        tags,
			CharacterID: characterID,
			UserID:      userID,
		}); err != nil {
			t.Fatalf("set tags: %v", err)
		}
		ch.Tags = tags
	}
	return ch
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// TestHomeOverviewMultiChar: one default-layout Home render over
// three characters in different states — training + wallet +
// asset + order data (Alpha), empty queue (Bravo), dead link
// (Charlie). Asserts fleet rows, attention items in rule order,
// net-worth arithmetic, industry/market cards, and zero network.
func TestHomeOverviewMultiChar(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// --- Alpha: healthy, training, rich in signals. ---
	seedOverviewCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "Trader, PI alt")
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{
		Name: "Fixture Alpha", CorporationID: fixtureCorpA,
		Birthday: "2009-12-24T00:00:00Z", SecurityStatus: 0.55,
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpInfo, esi.Corporation{Name: "Fixture Corp", Ticker: "FC"})
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{SolarSystemID: 30000142})
	seedSnapshot(t, q, fixtureCharA, esi.SnapShip, esi.Ship{ShipTypeID: 587, ShipName: "Boat"})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{Online: true, LastLogin: rfc(now.Add(-time.Hour))})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, QueuePosition: 0, FinishedLevel: 5, FinishDate: rfc(now.Add(48 * time.Hour))},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1000.5)
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		// Buy order: 500.00 escrow, far from expiry.
		{OrderID: 1, TypeID: 34, IsBuyOrder: true, Price: 5, VolumeRemain: 100, VolumeTotal: 100,
			Issued: rfc(now.Add(-time.Hour)), Duration: 90, Escrow: 500},
		// Sell order expiring in about an hour.
		{OrderID: 2, TypeID: 587, IsBuyOrder: false, Price: 1000000, VolumeRemain: 3, VolumeTotal: 3,
			Issued: rfc(now.Add(-23 * time.Hour)), Duration: 1},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 11, ActivityID: 1, Status: "ready", ProductTypeID: 587, BlueprintTypeID: 691,
			EndDate: rfc(now.Add(-time.Hour)), CompletedDate: rfc(now.Add(-time.Hour))},
		{JobID: 12, ActivityID: 1, Status: "active", ProductTypeID: 587, BlueprintTypeID: 691,
			EndDate: rfc(now.Add(3 * time.Hour))},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{
		{ContractID: 21, Status: "outstanding", Type: "courier", Title: "Haul my stuff",
			AssigneeID: fixtureCharA, AcceptorID: 0, DateExpired: rfc(now.Add(24 * time.Hour))},
		// Not ours to act on: assigned elsewhere.
		{ContractID: 22, Status: "outstanding", Type: "item_exchange", Title: "Someone elses",
			AssigneeID: 424242, AcceptorID: 0},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 31, TypeID: 34, Quantity: 100},
		{ItemID: 32, TypeID: 999999, Quantity: 7}, // unpriced: skipped
	})

	// --- Bravo: empty queue, plain wallet. ---
	seedOverviewCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo", "")
	seedSnapshot(t, q, fixtureCharB, esi.SnapProfile, esi.Character{
		Name: "Fixture Bravo", CorporationID: fixtureCorpA,
		Birthday: "2010-01-01T00:00:00Z", SecurityStatus: -1.2,
	})
	seedSnapshot(t, q, fixtureCharB, esi.SnapSkillqueue, esi.Skillqueue{})
	seedSnapshot(t, q, fixtureCharB, esi.SnapWallet, 2000.0)

	// --- Charlie: dead link. ---
	dead := seedOverviewCharacter(t, q, user.ID, 90000003, "Fixture Charlie", "")
	_ = dead
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateTokenDead,
		LinkStateAt: sql.NullString{String: rfc(now.Add(-2 * time.Hour)), Valid: true},
		CharacterID: 90000003,
	}); err != nil {
		t.Fatalf("park charlie: %v", err)
	}

	// Prices: Tritanium at 5.00 average.
	app.prices[34] = esi.MarketPrice{AveragePrice: 5}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}

	// Fleet rows + filter surface.
	mustContain(t, "/", body,
		`<span class="gt">Overview</span>`,
		"Fleet overview",
		"Fixture Alpha", "Fixture Corp", "Trader",
		"Fixture Bravo", "Fixture Charlie",
		`id="fleet-filter"`, `data-tag="PI"`,
		"Not training",   // Bravo's idle queue (Alpha trains)
		"re-link needed", // Charlie's fleet flag
	)

	// Attention feed, rule order: re-link (Charlie) > not
	// training (Bravo) > job ready (Alpha) > order expiring
	// (Alpha) > contract (Alpha). All five rules fire.
	mustContain(t, "/", body,
		"Needs attention",
		"Fixture Charlie needs re-linking",
		"Fixture Bravo isn&#39;t training",
		"job ready for delivery",
		"order expires in",
		"contract waiting for you: Haul my stuff",
	)
	relAt := strings.Index(body, "Fixture Charlie needs re-linking")
	ntAt := strings.Index(body, "Fixture Bravo isn")
	jrAt := strings.Index(body, "job ready for delivery")
	oeAt := strings.Index(body, "order expires in")
	ctAt := strings.Index(body, "contract waiting for you")
	if !(relAt >= 0 && relAt < ntAt && ntAt < jrAt && jrAt < oeAt && oeAt < ctAt) {
		t.Errorf("attention items out of rule order: relink=%d nottraining=%d jobready=%d order=%d contract=%d",
			relAt, ntAt, jrAt, oeAt, ctAt)
	}
	if strings.Contains(body, "Someone elses") {
		t.Error("attention feed leaked a contract assigned to someone else")
	}

	// Net worth: wallets 1000.5 + 2000 + assets 100×5 + escrow
	// 500 = 4,000.50. Estimate dated by the oldest input.
	mustContain(t, "/", body,
		"Net worth", "4,000.50", "3,000.50", "500.00",
		"Estimate", "data as of",
	)

	// Industry + market cards.
	mustContain(t, "/", body,
		"Industry", "1 active job", "1 ready for delivery", "Manufacturing",
		"Market", "open orders", "<strong>2</strong>",
		"3,000,000.00", // sell 3 × 1,000,000
	)

	// Server widget in the default layout, warming (no global
	// status seeded): title only, never the populated line.
	mustContain(t, "/", body, "Tranquility")
	if strings.Contains(body, "players online") {
		t.Error("home shows a players-online line with no status snapshot stored")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestHomeOverviewEmptyAccount: a signed-in account with no
// characters gets the link-your-first-character state, cleanly —
// and so does the dev-login-style userless session.
func TestHomeOverviewEmptyAccount(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)

	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	code, body := getPage(t, app, sessionCookie(t, app, user.ID, 0, ""), "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Link your first character", "No characters are linked")

	code, body = getPage(t, app, sessionCookie(t, app, 0, 0, ""), "/")
	if code != http.StatusOK {
		t.Fatalf("GET / (no user): status %d", code)
	}
	mustContain(t, "/ (no user)", body, "Link your first character")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestHomeLayoutRoundTrip: the Customize controls persist via
// POST /home/layout — toggle, reorder up/down, reset — and a
// hand-corrupted layout (unknown ids, duplicates) renders the
// surviving widgets instead of failing.
func TestHomeLayoutRoundTrip(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	saved := func() string {
		t.Helper()
		raw, err := q.GetUserHomeLayout(ctx, user.ID)
		if err != nil {
			t.Fatalf("read layout: %v", err)
		}
		return raw
	}
	post := func(action, widget string) (int, string) {
		t.Helper()
		code, _, _ := doReq(t, app, http.MethodPost, "/home/layout",
			url.Values{"action": {action}, "widget": {widget}}, cookie)
		return code, ""
	}

	// Default layout out of the box.
	if got := saved(); got != "" {
		t.Fatalf("fresh user layout = %q, want empty (default sentinel)", got)
	}
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Fleet overview", "Needs attention", "Net worth")
	if strings.Contains(body, ">Skills</h3>") {
		t.Error("skills widget on by default; want it opt-in only")
	}

	// Toggle Market off: the card disappears, layout persists.
	if code, _ := post("toggle", widgetMarket); code != http.StatusSeeOther {
		t.Fatalf("toggle POST: status %d", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / after toggle: status %d", code)
	}
	for _, gone := range []string{"<h3>Market</h3>"} {
		if strings.Contains(body, gone) {
			t.Errorf("market widget still rendered after toggle off (body has %q)", gone)
		}
	}
	mustContain(t, "/", body, "Fleet overview", "Net worth")
	if got := saved(); !strings.Contains(got, `"fleet"`) || strings.Contains(got, `"market"`) {
		t.Errorf("persisted layout %q: want fleet without market", got)
	}

	// Move Net worth above Attention: order flips on the page.
	if code, _ := post("up", widgetNetWorth); code != http.StatusSeeOther {
		t.Fatalf("move POST: status %d", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / after move: status %d", code)
	}
	nwAt := strings.Index(body, "<h3>Net worth</h3>")
	attAt := strings.Index(body, "<h3>Needs attention</h3>")
	if nwAt < 0 || attAt < 0 || nwAt > attAt {
		t.Errorf("net worth should render above attention after move-up (nw=%d att=%d)", nwAt, attAt)
	}

	// Customize page mirrors state and offers every control.
	code, body = getPage(t, app, cookie, "/?customize=1")
	if code != http.StatusOK {
		t.Fatalf("GET /?customize=1: status %d", code)
	}
	mustContain(t, "/?customize=1", body,
		"Customize home", "Reset to default layout",
		`name="widget" value="skills"`, ">Add<", "Move up",
	)

	// Reset restores the default set (market back).
	if code, _ := post("reset", ""); code != http.StatusSeeOther {
		t.Fatalf("reset POST: status %d", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / after reset: status %d", code)
	}
	mustContain(t, "/", body, "<h3>Market</h3>", "Fleet overview")

	// A corrupted layout (unknown ids, dupes) is normalized on
	// load: only the real widget renders.
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `["bogus","fleet","fleet","nope"]`, ID: user.ID,
	}); err != nil {
		t.Fatalf("seed bad layout: %v", err)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / with bad layout: status %d", code)
	}
	mustContain(t, "/", body, "Fleet overview")
	if strings.Contains(body, "<h3>Market</h3>") || strings.Contains(body, "Net worth") {
		t.Error("corrupted layout rendered widgets that were not in it")
	}

	// An anonymous layout POST bounces to sign-in, not a 500.
	code, _, _ = doReq(t, app, http.MethodPost, "/home/layout",
		url.Values{"action": {"toggle"}, "widget": {widgetFleet}})
	if code != http.StatusSeeOther {
		t.Fatalf("anonymous layout POST: status %d, want 303", code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestCharacterSheetFromSnapshots proves the character sheet —
// identity, wallet, skills, training — renders at /character/
// purely from worker-warmed snapshots (profile included), with
// zero outbound calls. This is the page that absorbed the old
// home sheet, fetch-free.
func TestCharacterSheetFromSnapshots(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{
		Name: "Fixture Alpha", CorporationID: fixtureCorpA,
		Birthday: "2009-12-24T13:44:00Z", SecurityStatus: 0.55,
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpInfo, esi.Corporation{Name: "Fixture Corp", Ticker: "FC"})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 4242.25)
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 105530570, UnallocatedSP: 250000,
		Skills: []esi.Skill{
			{SkillID: 3300, SkillpointsInSkill: 256000, TrainedSkillLevel: 5, ActiveSkillLevel: 5},
			{SkillID: 3301, SkillpointsInSkill: 45000, TrainedSkillLevel: 4, ActiveSkillLevel: 4},
		},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, QueuePosition: 0, FinishedLevel: 5, FinishDate: rfc(now.Add(48 * time.Hour))},
	})
	// Live-state snapshots so the page is fully loaded.
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{SolarSystemID: 30000142})
	seedSnapshot(t, q, fixtureCharA, esi.SnapShip, esi.Ship{ShipTypeID: 587, ShipName: "Fixture"})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{Logins: 7})
	seedSnapshot(t, q, fixtureCharA, esi.SnapClones, esi.Clones{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapImplants, esi.Implants{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFatigue, esi.Fatigue{})

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/character/")
	if code != http.StatusOK {
		t.Fatalf("GET /character/: status %d", code)
	}
	mustContain(t, "/character/", body,
		`<h1><span class="gt">Character</span></h1>`,
		"Fixture Corp", "2009-12-24", "0.55",
		"4,242.25",    // wallet from snapshot
		"105,530,570", // total SP
		"finishes",    // queue line's server-rendered finish
		"<h3>Training</h3>",
	)
	if !strings.Contains(strings.ToLower(body), "training") {
		t.Error("character page lost the training section")
	}
	if strings.Contains(body, "Still warming up") {
		t.Error("character page stuck on warming with every snapshot seeded")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestMigration010Reopen proves the home-layout column guard is
// idempotent, like every schema before it.
func TestMigration010Reopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	conn, err := openDB(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var cols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name IN ('home_layout')`).Scan(&cols); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if cols != 1 {
		t.Fatalf("home_layout columns: %d, want 1", cols)
	}
	conn.Close()
	conn, err = openDB(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	conn.Close()
}
