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
		"Fixture Bravo", "Fixture Charlie",
		`id="fleet-filter"`, `data-tag="PI"`,
		"Not training",   // Bravo's idle queue (Alpha trains)
		"re-link needed", // Charlie's fleet flag
	)

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
		// The Overview menu: Done exits, Add a module and Reset
		// live behind one settings button; nothing to save.
		`aria-label="Overview settings"`, ">Done</a>", `id="add-module-btn"`, ">Add a module</a>",
		"Reset to the default layout",
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
		"4,242.25",    // wallet from snapshot
		"105,530,570", // total SP
		"finishes",    // queue line's server-rendered finish
		"<h3>Training</h3>",
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
