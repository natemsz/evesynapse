package app

// Briefing routing: the card switches gate the deck, dismissals
// survive a card being switched off and on, the undercut digest
// honors its frequency, and the new cards render. Stored rows only.

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
)

// briefingHome renders the home page's briefing section.
func briefingHome(t *testing.T, app *Application, cookie *http.Cookie) string {
	t.Helper()
	_, body := getPage(t, app, cookie, "/")
	return briefingSection(t, body)
}

// storeCardsOff replaces the account's card switches.
func storeCardsOff(t *testing.T, app *Application, ctx context.Context, userID int64, off ...string) {
	t.Helper()
	if err := app.storeNotifyPrefs(ctx, userID, notifyPrefs{CardsOff: off}); err != nil {
		t.Fatalf("store cards off: %v", err)
	}
}

// TestBriefingCardSwitchHidesCard: switching a kind's card off
// keeps its lines off the deck, while other kinds still show.
func TestBriefingCardSwitchHidesCard(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedBriefingFixture(t, app, q, user.ID, now)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	before := briefingHome(t, app, cookie)
	if !strings.Contains(before, "has 3 unread mail") {
		t.Fatalf("briefing without switches lacks the mail card:\n%s", before)
	}

	storeCardsOff(t, app, ctx, user.ID, notifyMail)
	after := briefingHome(t, app, cookie)
	if strings.Contains(after, "has 3 unread mail") {
		t.Fatalf("mail card still on the deck with its card switched off:\n%s", after)
	}
	if !strings.Contains(after, "queue runs dry after that") {
		t.Fatalf("switching mail off hid the skills card too:\n%s", after)
	}

	// The industry card follows its own switch the same way.
	storeCardsOff(t, app, ctx, user.ID, notifyIndustry)
	after = briefingHome(t, app, cookie)
	if strings.Contains(after, "has 1 industry job finished") {
		t.Fatalf("industry card still on the deck with its card switched off:\n%s", after)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("briefing made %d outbound calls, want 0", got)
	}
}

// TestBriefingDismissalSurvivesCardToggle: a dismissed line is
// still dismissed after its card is switched off and back on.
func TestBriefingDismissalSurvivesCardToggle(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedBriefingFixture(t, app, q, user.ID, now)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	post := func(path, key string) int {
		form := "key=" + url.QueryEscape(key)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	mailKey := briefingKey(fixtureCharA, "mail")
	if code := post("/home/briefing/dismiss", mailKey); code != http.StatusSeeOther {
		t.Fatalf("dismiss: status %d, want %d", code, http.StatusSeeOther)
	}
	section := briefingHome(t, app, cookie)
	if !strings.Contains(section, "Dismissed (1)") {
		t.Fatalf("dismissed mail line not in the dismissed list:\n%s", section)
	}

	storeCardsOff(t, app, ctx, user.ID, notifyMail)
	section = briefingHome(t, app, cookie)
	if strings.Contains(section, "has 3 unread mail") {
		t.Fatalf("dismissed mail line returns while its card is off:\n%s", section)
	}

	storeCardsOff(t, app, ctx, user.ID)
	section = briefingHome(t, app, cookie)
	if strings.Count(section, "has 3 unread mail") != 1 || !strings.Contains(section, "Dismissed (1)") {
		t.Fatalf("dismissed mail line not back in the dismissed list:\n%s", section)
	}
}

// TestUndercutDigestFrequency: the digest shows at most once per
// the account's chosen window, and the watch lines are unaffected.
func TestUndercutDigestFrequency(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedMarketSignals(t, app, q, user.ID, 1)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	if err := app.storeNotifyPrefs(ctx, user.ID, notifyPrefs{UndercutDigestHours: 24}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	first := briefingHome(t, app, cookie)
	if !strings.Contains(first, "undercut right now") {
		t.Fatalf("first digest missing:\n%s", first)
	}
	second := briefingHome(t, app, cookie)
	if strings.Contains(second, "undercut right now") {
		t.Fatalf("digest repeated inside its frequency window:\n%s", second)
	}
	prefs := app.notifyPrefsFor(ctx, user.ID)
	if prefs.UndercutDigestShown == 0 {
		t.Fatal("shown digest did not stamp its hour")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("briefing made %d outbound calls, want 0", got)
	}
}

// TestUndercutDigestEveryTime: zero hours means every render.
func TestUndercutDigestEveryTime(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedMarketSignals(t, app, q, user.ID, 1)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	for i := 0; i < 2; i++ {
		if section := briefingHome(t, app, cookie); !strings.Contains(section, "undercut right now") {
			t.Fatalf("render %d missing the every-time digest:\n%s", i+1, section)
		}
	}
}

// TestUndercutDigestOpensOrders: the digest links to My Orders,
// where the beaten rows already live.
func TestUndercutDigestOpensOrders(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedMarketSignals(t, app, q, user.ID, 1)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	section := briefingHome(t, app, cookie)
	if !strings.Contains(section, `/orders/?character=`) || !strings.Contains(section, "undercut right now") {
		t.Fatalf("digest does not open My Orders:\n%s", section)
	}
}

// TestBriefingCampCard: a camp in a watched system cards while a
// character is online, and the card switch hides it.
func TestBriefingCampCard(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	const rancer = int64(30003504)
	seedCampKills(t, conn, rancer, now)
	seedOnlineNow(t, q, fixtureCharA, true)
	if err := app.storeNotifyPrefs(ctx, user.ID, notifyPrefs{CampSystems: []int64{rancer}}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	section := briefingHome(t, app, cookie)
	if !strings.Contains(section, "4 kills in the last hour") {
		t.Fatalf("camp card missing:\n%s", section)
	}

	storeCardsOff(t, app, ctx, user.ID, notifyCamp)
	_, nondigest := getPage(t, app, cookie, "/")
	if strings.Contains(nondigest, "4 kills in the last hour") {
		t.Fatal("camp card still on the deck with its card switched off")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("briefing made %d outbound calls, want 0", got)
	}
}

// TestBriefingCampCardNeedsOnline: no online character, no card.
func TestBriefingCampCardNeedsOnline(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCampKills(t, conn, 30003504, now)
	seedOnlineNow(t, q, fixtureCharA, false)
	if err := app.storeNotifyPrefs(ctx, user.ID, notifyPrefs{CampSystems: []int64{30003504}}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	_, body := getPage(t, app, cookie, "/")
	if strings.Contains(body, "4 kills in the last hour") {
		t.Fatal("camp card shown with nobody online")
	}
}

// TestBriefingContractEvents: accepted and finished contracts
// card under the contract kind's switch.
func TestBriefingContractEvents(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{
		{ContractID: 21, Type: "item_exchange", Title: "Spare hulls", Status: "in_progress",
			IssuerID: fixtureCharA, AcceptorID: 424242, DateAccepted: rfc(now.Add(-time.Hour))},
		{ContractID: 22, Type: "courier", Title: "Haul fast", Status: "finished",
			IssuerID: 424242, AcceptorID: fixtureCharA, DateCompleted: rfc(now.Add(-time.Hour))},
	})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	section := briefingHome(t, app, cookie)
	if !strings.Contains(section, "Spare hulls was accepted") || !strings.Contains(section, "Haul fast finished") {
		t.Fatalf("contract event cards missing:\n%s", section)
	}

	storeCardsOff(t, app, ctx, user.ID, notifyContract)
	_, nondigest := getPage(t, app, cookie, "/")
	if strings.Contains(nondigest, "Spare hulls was accepted") || strings.Contains(nondigest, "Haul fast finished") {
		t.Fatal("contract cards still on the deck with the card switched off")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("briefing made %d outbound calls, want 0", got)
	}
}

// TestBriefingRelinkCardGate: the re-login card follows its switch.
func TestBriefingRelinkCardGate(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState: linkStateTokenDead, LinkStateAt: mustNullTime(now.Format(time.RFC3339)), CharacterID: fixtureCharA,
	}); err != nil {
		t.Fatalf("park token: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	section := briefingHome(t, app, cookie)
	if !strings.Contains(section, "needs re-linking") {
		t.Fatalf("re-login card missing:\n%s", section)
	}

	storeCardsOff(t, app, ctx, user.ID, notifyRelink)
	_, body := getPage(t, app, cookie, "/")
	if strings.Contains(body, "needs re-linking") {
		t.Fatal("re-login card still on the deck with its card switched off")
	}
}
