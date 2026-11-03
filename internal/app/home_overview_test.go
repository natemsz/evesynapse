package app

// Hermetic tests for the widget overview home. Same
// contract as the corp/intel suites: seeded snapshots drive the
// real router against a counting transport, and the transport
// must observe zero outbound calls — the overview renders
// cache-only, across every linked character.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
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
// (Charlie). Asserts fleet rows, the Briefing's re-link line,
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

	// --- Delta: a paused queue. Its entries carry no finish
	// dates, so the fleet row reads not training and counts
	// idle, the briefing's queueState verdict on the same
	// snapshot. ---
	seedOverviewCharacter(t, q, user.ID, 90000004, "Fixture Delta", "")
	seedSnapshot(t, q, 90000004, esi.SnapProfile, esi.Character{
		Name: "Fixture Delta", CorporationID: fixtureCorpA,
		Birthday: "2011-05-05T00:00:00Z", SecurityStatus: 0.1,
	})
	seedSnapshot(t, q, 90000004, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3302, QueuePosition: 0, FinishedLevel: 4},
		{SkillID: 3300, QueuePosition: 1, FinishedLevel: 5},
	})

	// --- Charlie: dead link. ---
	dead := seedOverviewCharacter(t, q, user.ID, 90000003, "Fixture Charlie", "")
	_ = dead
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateTokenDead,
		LinkStateAt: timeSet(now.Add(-2 * time.Hour)),
		CharacterID: 90000003,
	}); err != nil {
		t.Fatalf("park charlie: %v", err)
	}

	// Prices: The Forge's sweep has Tritanium at a typical
	// sell of 5.00 (house source; the guide average prices
	// nothing).
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 34, TypicalSell: 5, AvgDailyVolume: 1_000_000,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed region stat: %v", err)
	}
	// The sweep landing mid-test: the cache window lapses.
	app.forgeTypicalsMu.Lock()
	app.forgeTypicalsCache = nil
	app.forgeTypicalsMu.Unlock()

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}

	// Fleet rows + filter surface.
	mustContain(t, "/", body,
		`<span class="gt">Overview</span>`,
		"Fleet Overview",
		"Fixture Alpha", "Fixture Corp", "Trader",
		"Fixture Bravo", "Fixture Charlie", "Fixture Delta",
		`id="fleet-filter"`, `data-tag="PI"`,
		"Not training",   // Bravo's idle queue (Alpha trains)
		"re-link needed", // Charlie's fleet flag
	)

	// Two characters are idle: Bravo's empty queue and Delta's
	// paused one. The fleet's Not training count says both, and
	// exactly their two rows carry the idle flag.
	mustContain(t, "/", body, "Not training (2)")
	if got := strings.Count(body, `data-idle="1"`); got != 2 {
		t.Errorf("fleet rows flagged idle = %d, want 2 (Bravo's empty queue, Delta's paused queue)", got)
	}

	// What needs the user is the Briefing's now (Needs Attention is
	// retired): the re-link shows there, and a contract assigned to
	// someone else never does.
	mustContain(t, "/", body, "Fixture Charlie", "needs re-linking")
	if strings.Contains(body, "Needs Attention") || strings.Contains(body, `data-widget="attention"`) {
		t.Error("retired Needs Attention module still on the home")
	}
	if strings.Contains(body, "Someone elses") {
		t.Error("home leaked a contract assigned to someone else")
	}

	// Net worth: wallets 1000.5 + 2000 + assets 100×5 + escrow
	// 500 = 4,000.50. Estimate dated by the oldest input.
	mustContain(t, "/", body,
		"Net Worth", "4,000.50", "3,000.50", "500.00",
		"Estimate", "data as of",
	)

	// Industry + market cards.
	mustContain(t, "/", body,
		"Industry", "1 active job", "1 ready for delivery", "Manufacturing",
		"Market", "open orders", "<strong>2</strong>",
		"3,000,000.00", // sell 3 × 1,000,000
	)

	// The Overview bar's ESI pill, before any status is stored:
	// a neutral "checking" state, never a player count. The
	// retired Tranquility module is gone from the grid.
	mustContain(t, "/", body, `class="esi-pill esi-pending"`)
	if strings.Contains(body, "esi-tq") {
		t.Error("home shows a Tranquility player count with no status snapshot stored")
	}
	if strings.Contains(body, `data-widget="server"`) {
		t.Error("retired Tranquility module still on the default home")
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
	mustContain(t, "/", body, "Fleet Overview", "Net Worth", "Training Horizon", "Planetary Industry")
	if strings.Contains(body, `data-widget="skills"`) || strings.Contains(body, ">Market Watchlist</h3>") {
		t.Error("default home carries a retired or opt-in module")
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
	mustContain(t, "/", body, "Fleet Overview", "Net Worth")
	if got := saved(); !strings.Contains(got, `"fleet"`) || strings.Contains(got, `"market"`) {
		t.Errorf("persisted layout %q: want fleet without market", got)
	}

	// Move Net Worth above Planetary Industry: order flips on the page.
	if code, _ := post("up", widgetNetWorth); code != http.StatusSeeOther {
		t.Fatalf("move POST: status %d", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / after move: status %d", code)
	}
	nwAt := strings.Index(body, "<h3>Net Worth</h3>")
	piAt := strings.Index(body, "<h3>Planetary Industry</h3>")
	if nwAt < 0 || piAt < 0 || nwAt > piAt {
		t.Errorf("net worth should render above planetary industry after move-up (nw=%d pi=%d)", nwAt, piAt)
	}

	// Customize page mirrors state and offers every control.
	code, body = getPage(t, app, cookie, "/?customize=1")
	if code != http.StatusOK {
		t.Fatalf("GET /?customize=1: status %d", code)
	}
	mustContain(t, "/?customize=1", body,
		"Reset to the default layout", ">Done</a>",
		`name="widget" value="watchlist"`, ">Add<", "Move up",
	)

	// Reset restores the default set (market back).
	if code, _ := post("reset", ""); code != http.StatusSeeOther {
		t.Fatalf("reset POST: status %d", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / after reset: status %d", code)
	}
	mustContain(t, "/", body, "<h3>Market</h3>", "Fleet Overview")

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
	mustContain(t, "/", body, "Fleet Overview")
	if strings.Contains(body, "<h3>Market</h3>") || strings.Contains(body, "<h3>Net Worth</h3>") {
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

// doLayoutPost POSTs /home/layout, optionally as the fetch
// calls app.js makes (X-Requested-With), and returns status+body.
func doLayoutPost(t *testing.T, app *Application, form url.Values, cookie *http.Cookie, xhr bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/home/layout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if xhr {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestHomeLayoutOrderAction: the drag-save action accepts the
// whole arrangement at once — reorders persist, unknown and
// duplicate ids normalize away server-side, XHR callers get a
// bare 200 instead of the redirect, and anonymous POSTs bounce.
func TestHomeLayoutOrderAction(t *testing.T) {
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

	// Drag order: Market above Fleet, as one XHR save.
	code, body := doLayoutPost(t, app, url.Values{
		"action": {"order"}, "ids": {"market,fleet,pi"},
	}, cookie, true)
	if code != http.StatusOK {
		t.Fatalf("XHR order POST: status %d, want 200 (body %q)", code, body)
	}
	if body != `{"ok":true}` {
		t.Fatalf("XHR order POST body = %q, want {\"ok\":true}", body)
	}
	// The default layout's sizes travel with each module.
	if got := saved(); got != `[{"id":"market","w":1,"h":1},{"id":"fleet","w":3,"h":2},{"id":"pi","w":1,"h":2}]` {
		t.Fatalf("saved layout = %q, want dragged order", got)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mAt := strings.Index(body, `data-widget="market"`)
	fAt := strings.Index(body, `data-widget="fleet"`)
	if mAt < 0 || fAt < 0 || mAt > fAt {
		t.Errorf("market should render above fleet after drag (market=%d fleet=%d)", mAt, fAt)
	}

	// Unknown + duplicate ids normalize away; order survives.
	code, _ = doLayoutPost(t, app, url.Values{
		"action": {"order"}, "ids": {"watchlist,bogus,watchlist,fleet,fleet"},
	}, cookie, true)
	if code != http.StatusOK {
		t.Fatalf("XHR order POST (junk ids): status %d", code)
	}
	if got := saved(); got != `[{"id":"watchlist"},{"id":"fleet","w":3,"h":2}]` {
		t.Fatalf("saved layout = %q, want normalized [watchlist fleet]", got)
	}

	// The same action as a plain form POST still redirects.
	code, _ = doLayoutPost(t, app, url.Values{
		"action": {"order"}, "ids": {"fleet"},
	}, cookie, false)
	if code != http.StatusSeeOther {
		t.Fatalf("form order POST: status %d, want 303", code)
	}
	if got := saved(); got != `[{"id":"fleet","w":3,"h":2}]` {
		t.Fatalf("saved layout = %q, want [fleet]", got)
	}

	// Anonymous order POSTs bounce, signed-in XHR or not.
	code, _, _ = doReq(t, app, http.MethodPost, "/home/layout",
		url.Values{"action": {"order"}, "ids": {"fleet"}})
	if code != http.StatusSeeOther {
		t.Fatalf("anonymous order POST: status %d, want 303", code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestHomeLayoutToggleRoundTrips: the two saves behind the
// customize-mode controls — × removal (toggle off) and
// add-from-catalog (toggle on, appends at the end).
func TestHomeLayoutToggleRoundTrips(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// × on Net worth: the card leaves the home and the grid.
	code, _ := doLayoutPost(t, app, url.Values{
		"action": {"toggle"}, "widget": {widgetNetWorth},
	}, cookie, true)
	if code != http.StatusOK {
		t.Fatalf("XHR toggle-off POST: status %d, want 200", code)
	}
	code, body := getPage(t, app, cookie, "/?customize=1")
	if code != http.StatusOK {
		t.Fatalf("GET /?customize=1: status %d", code)
	}
	if strings.Contains(body, `data-widget="networth"`) {
		t.Error("net worth card still on the customize grid after × removal")
	}
	// …but it is offered in the add-module pop-up.
	mustContain(t, "/?customize=1 (after remove)", body, `data-add-widget="networth"`)

	// Add Market Watchlist from the catalog: appended after the survivors.
	code, _ = doLayoutPost(t, app, url.Values{
		"action": {"toggle"}, "widget": {widgetWatchlist},
	}, cookie, true)
	if code != http.StatusOK {
		t.Fatalf("XHR toggle-on POST: status %d, want 200", code)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	skAt := strings.Index(body, `data-widget="watchlist"`)
	mkAt := strings.Index(body, `data-widget="market"`)
	if skAt < 0 {
		t.Fatal("watchlist card missing after add-from-catalog")
	}
	if mkAt < 0 || skAt < mkAt {
		t.Errorf("added module should append at the end (watchlist=%d market=%d)", skAt, mkAt)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// renderedWidgetIDs extracts the home's widget ids in render order.
func renderedWidgetIDs(body string) []string {
	var ids []string
	rest := body
	for {
		i := strings.Index(rest, `data-widget="`)
		if i < 0 {
			return ids
		}
		rest = rest[i+len(`data-widget="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return ids
		}
		ids = append(ids, rest[:j])
		rest = rest[j:]
	}
}

func indexOfWidget(ids []string, want string) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}

// TestHomeLayoutPlaystylePresets: an account with no saved layout
// gets the preset for its stored link choice, a saved layout wins
// over the preset, a saved "[]" stays empty, a multi-activity choice
// blends with the universal widgets ahead of the specialists, and
// the demo account gets its showcase preset. Customize's reset
// restores the account's own preset.
func TestHomeLayoutPlaystylePresets(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	// The demo account first: the app caches its id on first use.
	demoUser, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create demo user: %v", err)
	}
	if err := q.MarkUserDemo(ctx, demoUser.ID); err != nil {
		t.Fatalf("mark demo user: %v", err)
	}
	seedCharacter(t, q, demoUser.ID, 90000105, "Fixture Demo")

	newAccount := func(charID int64, choice string) (db.User, *http.Cookie) {
		t.Helper()
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		seedCharacter(t, q, user.ID, charID, "Fixture Alpha")
		if choice != "" {
			if err := q.SetLinkChoice(ctx, db.SetLinkChoiceParams{
				UserID: user.ID, Choice: choice, UpdatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("set link choice: %v", err)
			}
		}
		return user, sessionCookie(t, app, user.ID, charID, "Fixture Alpha")
	}
	homeIDs := func(cookie *http.Cookie) []string {
		t.Helper()
		code, body := getPage(t, app, cookie, "/")
		if code != http.StatusOK {
			t.Fatalf("GET /: status %d", code)
		}
		return renderedWidgetIDs(body)
	}

	// A stored market choice with no saved layout yields the
	// market preset under the fixed head. The briefing section
	// renders only when it has items, so a bare account's render
	// starts at At a Glance; the tiling test pins the head itself.
	_, cookie := newAccount(90000101, `{"activities":["market"]}`)
	wantMarket := []string{"glance", "fleet", "tradeideas", "networth", "profitpulse", "walletflow", "watchlist", "market"}
	if got := homeIDs(cookie); strings.Join(got, ",") != strings.Join(wantMarket, ",") {
		t.Errorf("market preset = %v, want %v", got, wantMarket)
	}

	// Customize's reset stores the account's own preset.
	code, _ := doLayoutPost(t, app, url.Values{"action": {"reset"}}, cookie, false)
	if code != http.StatusSeeOther {
		t.Fatalf("reset POST: status %d, want 303", code)
	}
	if got := homeIDs(cookie); strings.Join(got, ",") != strings.Join(wantMarket, ",") {
		t.Errorf("home after reset = %v, want the market preset %v", got, wantMarket)
	}

	// A saved layout wins over the preset.
	savedUser, savedCookie := newAccount(90000102, `{"activities":["market"]}`)
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `[{"id":"fleet","w":3,"h":2}]`, ID: savedUser.ID,
	}); err != nil {
		t.Fatalf("save layout: %v", err)
	}
	if got := homeIDs(savedCookie); strings.Join(got, ",") != "fleet" {
		t.Errorf("saved layout home = %v, want [fleet] only", got)
	}

	// A saved "[]" stays deliberately empty.
	emptyUser, emptyCookie := newAccount(90000103, `{"activities":["market"]}`)
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `[]`, ID: emptyUser.ID,
	}); err != nil {
		t.Fatalf("save empty layout: %v", err)
	}
	if got := homeIDs(emptyCookie); len(got) != 0 {
		t.Errorf("empty saved layout home = %v, want no widgets", got)
	}

	// A multi-activity choice blends: the universal widgets stand
	// ahead of the single-playstyle specialists.
	_, blendCookie := newAccount(90000104, `{"activities":["industry","corporation"]}`)
	blend := homeIDs(blendCookie)
	for _, specialist := range []string{"prodline", "pulse", "upkeep"} {
		if indexOfWidget(blend, specialist) < 0 {
			t.Errorf("blend %v is missing specialist %q", blend, specialist)
		}
		for _, universal := range []string{"networth", "training"} {
			if indexOfWidget(blend, universal) > indexOfWidget(blend, specialist) {
				t.Errorf("blend %v: universal %q should stand ahead of specialist %q", blend, universal, specialist)
			}
		}
	}

	// The demo account gets its showcase preset: filled widgets
	// only, no upkeep (the seed holds no structures) and no trade
	// ideas (the seed holds no stored answer).
	demoIDs := homeIDs(sessionCookie(t, app, demoUser.ID, 90000105, "Fixture Demo"))
	for _, want := range []string{"glance", "fleet", "walletflow", "networth", "prodline", "pi", "profitpulse", "industry", "market", "training", "watchlist", "pulse"} {
		if indexOfWidget(demoIDs, want) < 0 {
			t.Errorf("demo preset %v is missing %q", demoIDs, want)
		}
	}
	for _, gone := range []string{"upkeep", "tradeideas"} {
		if indexOfWidget(demoIDs, gone) >= 0 {
			t.Errorf("demo preset %v carries %q, which the demo seed does not fill", demoIDs, gone)
		}
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// layoutTilesGrid reports whether a layout covers the three-wide
// grid with no gaps, under the grid's dense placement.
func layoutTilesGrid(items []homeLayoutItem) bool {
	type cell struct{ r, c int }
	occ := make(map[cell]bool)
	maxRow := 0
	for _, item := range items {
		placed := false
		for r := 0; !placed; r++ {
			for c := 0; c+item.W <= homeGridMax && !placed; c++ {
				free := true
				for dr := 0; dr < item.H && free; dr++ {
					for dc := 0; dc < item.W; dc++ {
						if occ[cell{r + dr, c + dc}] {
							free = false
						}
					}
				}
				if free {
					for dr := 0; dr < item.H; dr++ {
						for dc := 0; dc < item.W; dc++ {
							occ[cell{r + dr, c + dc}] = true
						}
					}
					if r+item.H > maxRow {
						maxRow = r + item.H
					}
					placed = true
				}
			}
		}
	}
	for r := 0; r < maxRow; r++ {
		for c := 0; c < homeGridMax; c++ {
			if !occ[cell{r, c}] {
				return false
			}
		}
	}
	return len(items) > 0
}

// TestHomePresetLayoutsTile: every preset, singles, blends, the
// everything choice and the demo layout, starts with the fixed head
// at its fixed sizes, stays inside every widget's size bounds, and
// tiles the grid with no gaps. Single-playstyle specialists stand
// expanded past their catalog size (Net Worth is already at its
// maximum catalog size), and fills stand at catalog size.
func TestHomePresetLayoutsTile(t *testing.T) {
	activities := []string{"pilots", "market", "industry", "corporation", "fitting"}
	var choices [][]string
	for _, a := range activities {
		choices = append(choices, []string{a})
	}
	for mask := 0; mask < 1<<len(activities); mask++ {
		var combo []string
		for i, a := range activities {
			if mask&(1<<i) != 0 {
				combo = append(combo, a)
			}
		}
		if len(combo) >= 2 {
			choices = append(choices, combo)
		}
	}
	choices = append(choices, []string{activityEverything})

	check := func(name string, layout []homeLayoutItem) {
		t.Helper()
		if len(layout) < 3 {
			t.Fatalf("%s preset = %v, want the fixed head plus a body", name, layoutIDs(layout))
		}
		head := []homeLayoutItem{
			{ID: widgetBriefing, W: 3, H: 1},
			{ID: widgetGlance, W: 3, H: 1},
			{ID: widgetFleet, W: 3, H: 2},
		}
		for i, want := range head {
			if layout[i] != want {
				t.Errorf("%s preset head[%d] = %v, want %v", name, i, layout[i], want)
			}
		}
		seen := make(map[string]bool)
		for _, item := range layout {
			if seen[item.ID] {
				t.Errorf("%s preset repeats %q", name, item.ID)
			}
			seen[item.ID] = true
			if w, h := widgetSize(item); w != item.W || h != item.H {
				t.Errorf("%s preset sizes %q at %dx%d, outside its bounds", name, item.ID, item.W, item.H)
			}
		}
		if !layoutTilesGrid(layout) {
			t.Errorf("%s preset does not tile the grid: %v", name, layout)
		}
	}

	for _, choice := range choices {
		layout, ok := presetForActivities(choice)
		if !ok {
			t.Fatalf("presetForActivities(%v) not ok", choice)
		}
		check(strings.Join(choice, "+"), layout)
	}
	check("demo", presetWithHead(demoHomeBody))

	for activity, pw := range playstyleWidgetsByActivity {
		layout, ok := presetForActivities([]string{activity})
		if !ok {
			t.Fatalf("presetForActivities([%s]) not ok", activity)
		}
		byID := make(map[string]homeLayoutItem, len(layout))
		for _, item := range layout {
			byID[item.ID] = item
		}
		for _, id := range pw.Specialists {
			item, present := byID[id]
			if !present {
				t.Errorf("%s preset is missing specialist %q", activity, id)
				continue
			}
			def, _ := widgetDefFor(id)
			if id == widgetNetWorth {
				if item.W != def.W || item.H != def.H {
					t.Errorf("%s preset sizes networth at %dx%d, want its maximum %dx%d", activity, item.W, item.H, def.W, def.H)
				}
			} else if item.W*item.H <= def.W*def.H {
				t.Errorf("%s preset sizes specialist %q at catalog size %dx%d, want expanded", activity, id, item.W, item.H)
			}
		}
		for _, id := range pw.Fill {
			item, present := byID[id]
			if !present {
				t.Errorf("%s preset is missing fill %q", activity, id)
				continue
			}
			def, _ := widgetDefFor(id)
			if item.W != def.W || item.H != def.H {
				t.Errorf("%s preset sizes fill %q at %dx%d, want catalog %dx%d", activity, id, item.W, item.H, def.W, def.H)
			}
		}
	}
}

// TestHomeCustomizeSurface: customize mode renders the live
// widget grid (draggable cards, add-module pop-up) on top of the
// plain no-JS module list, with end-user copy only — the old
// implementation-flavored hint is gone everywhere on Home.
func TestHomeCustomizeSurface(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/?customize=1")
	if code != http.StatusOK {
		t.Fatalf("GET /?customize=1: status %d", code)
	}
	mustContain(t, "/?customize=1", body,
		// Live draggable grid.
		`id="home-grid"`, `data-customize="1"`, `home-custom`,
		`data-widget="fleet"`, `data-widget="market"`,
		// The Customize menu: Done exits, Add a module, Theme and
		// Reset live behind one settings button; nothing to save.
		`aria-label="Customize settings"`, ">Done</a>", `id="add-module-btn"`, ">Add a module</a>",
		"Reset to the default layout", `data-open-theme-modal`, `id="theme-modal"`, `data-scheme-row="soe"`,
		// The pop-up: full catalog, name + plain description.
		`id="add-module-modal"`, `data-add-widget="watchlist"`,
		"The items you&#39;re watching",
		"Every module is already on your home.",
		// The no-JS foundation underneath: the module list forms.
		"<noscript>", `id="home-module-list"`,
		`name="widget" value="watchlist"`, "Move up", "Move down",
		// End-user copy.
		"Changes save as you make them.",
	)
	for _, gone := range []string{
		"Save changes",
		"no JavaScript needed", "Turn widgets on or off",
		"check the server log",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("customize page still carries developer copy %q", gone)
		}
	}

	// Plain home: no customize machinery leaks in, and the
	// not-yet-loaded states speak plainly (no warming jargon).
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		"Balances appear here once your characters have synced.",
	)
	for _, gone := range []string{
		`data-customize="1"`, `id="add-module-modal"`,
		"still warming", "the worker has synced",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("plain home carries %q, want it gone", gone)
		}
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

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
		"4,242.25",   // wallet from snapshot
		"105.53M SP", // total SP, compact in the sheet head
		"finishes",   // queue line's server-rendered finish
		"Now training",
	)
	if !strings.Contains(strings.ToLower(body), "training") {
		t.Error("character page lost the training section")
	}
	if strings.Contains(body, "Still syncing") {
		t.Error("character page stuck on warming with every snapshot seeded")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestMigration010Reopen proves the home-layout column guard is
// idempotent, like every schema before it.
func TestMigration010Reopen(t *testing.T) {
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var cols int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name IN ('home_layout')`).Scan(&cols); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if cols != 1 {
		t.Fatalf("home_layout columns: %d, want 1", cols)
	}
	conn.Close()
	pool.Close()
	conn, pool, err = store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	conn.Close()
	pool.Close()
}

// TestWidgetSizeBounds: a stored size is held inside the
// module's floor and ceiling alike — a band drawn 3 tall still
// stands one row, a KPI stack drawn 3 wide still stands one
// column — and every catalog module's ceiling clears its floor.
func TestWidgetSizeBounds(t *testing.T) {
	for _, c := range []struct {
		item         homeLayoutItem
		wantW, wantH int
	}{
		{homeLayoutItem{ID: widgetGlance, W: 3, H: 3}, 3, 1},   // a band is one row however tall it is drawn
		{homeLayoutItem{ID: widgetNetWorth, W: 3, H: 3}, 1, 2}, // a KPI stack is one column however wide
		{homeLayoutItem{ID: widgetIndustry, W: 3, H: 3}, 2, 3}, // width past the activity column gains nothing
		{homeLayoutItem{ID: widgetFleet, W: 1, H: 1}, 2, 2},    // the floor still wins from below
		{homeLayoutItem{ID: widgetFleet, W: 3, H: 3}, 3, 3},    // a table may use the whole grid
		{homeLayoutItem{ID: widgetBriefing, W: 1, H: 2}, 2, 1}, // floor on width, ceiling on height
	} {
		if w, h := widgetSize(c.item); w != c.wantW || h != c.wantH {
			t.Errorf("widgetSize(%+v) = %dx%d, want %dx%d", c.item, w, h, c.wantW, c.wantH)
		}
	}
	for _, def := range homeWidgetCatalog() {
		minW, minH := widgetMin(def.ID)
		maxW, maxH := widgetMax(def.ID)
		if maxW < minW || maxH < minH {
			t.Errorf("widget %s: ceiling %dx%d below floor %dx%d", def.ID, maxW, maxH, minW, minH)
		}
		if maxW > homeGridMax || maxH > homeGridMax {
			t.Errorf("widget %s: ceiling %dx%d past the grid", def.ID, maxW, maxH)
		}
	}
	// The heights a list is offered stop where its rows do.
	for _, c := range []struct{ total, want int }{
		{0, 1}, {3, 1}, {4, 2}, {8, 2}, {9, 3}, {40, 3},
	} {
		if got := capHeightFor(c.total); got != c.want {
			t.Errorf("capHeightFor(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}

// TestHomeLayoutSizeCeiling: the size action takes a size at the
// module's ceiling and refuses (in words the script can hear)
// one past it, leaving the saved layout alone.
func TestHomeLayoutSizeCeiling(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	postSize := func(widget, w, h string) (int, string) {
		t.Helper()
		form := url.Values{"action": {"size"}, "widget": {widget}, "w": {w}, "h": {h}}
		req := httptest.NewRequest(http.MethodPost, "/home/layout", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	saved := func() string {
		t.Helper()
		raw, err := q.GetUserHomeLayout(ctx, user.ID)
		if err != nil {
			t.Fatalf("read layout: %v", err)
		}
		return raw
	}

	// Net Worth is one column wide however it is drawn: w=3 is
	// past its ceiling — refused, nothing saved.
	if code, body := postSize(widgetNetWorth, "3", "1"); code != http.StatusBadRequest || !strings.Contains(body, `"ok":false`) {
		t.Fatalf("oversize POST: status %d body %q, want 400 ok:false", code, body)
	}
	if got := saved(); got != "" {
		t.Fatalf("oversize POST saved %q, want the layout untouched", got)
	}
	// At the ceiling it saves.
	if code, _ := postSize(widgetNetWorth, "1", "2"); code != http.StatusOK {
		t.Fatalf("at-ceiling POST: status %d, want 200", code)
	}
	if got := saved(); !strings.Contains(got, `"id":"networth","w":1,"h":2`) {
		t.Fatalf("saved layout %q: want the networth size at its ceiling", got)
	}
	// The band is one row however tall it is drawn: h=2 refuses.
	if code, _ := postSize(widgetGlance, "3", "2"); code != http.StatusBadRequest {
		t.Fatalf("glance oversize POST: status %d, want 400", code)
	}
	if got := saved(); strings.Contains(got, `"id":"glance","w":3,"h":2`) {
		t.Fatalf("saved layout %q: the refused glance size landed", got)
	}
}
