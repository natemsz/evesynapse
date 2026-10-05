package app

// Hermetic tests for the Phase 6 Briefing home module: seeded
// snapshots drive the real router against a counting transport —
// the digest renders cache-only (zero outbound calls), its lines
// fire and stay quiet on the rule fixtures, the cap and the
// per-character dedupe hold, and the "since you last looked"
// anchor obeys its window semantics (first visit 24h, bounded to
// 7 days, advancing only after a render that includes the module).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedBriefingFixture links Alpha and loads snapshots so nearly
// every briefing rule fires at once.
func seedBriefingFixture(t *testing.T, app *Application, q *db.Queries, userID int64, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 587, Name: "Rifter"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}

	// Queue ending within the day.
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, QueuePosition: 0, FinishedLevel: 5, FinishDate: rfc(now.Add(3 * time.Hour))},
	})
	// A ready job that finished inside the window.
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 11, ActivityID: 1, Status: "ready", ProductTypeID: 587, BlueprintTypeID: 691,
			EndDate: rfc(now.Add(-time.Hour)), CompletedDate: rfc(now.Add(-time.Hour))},
	})
	// One expired order inside the window, one long before it.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrdersHistory, esi.CharOrderHistory{
		{CharOrder: esi.CharOrder{OrderID: 51, TypeID: 34, Issued: rfc(now.Add(-25 * time.Hour)), Duration: 1}, State: "expired"},
		{CharOrder: esi.CharOrder{OrderID: 52, TypeID: 34, Issued: rfc(now.Add(-10 * 24 * time.Hour)), Duration: 1}, State: "expired"},
	})
	// One open order expiring within the day.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 700, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 10,
			VolumeRemain: 5, VolumeTotal: 5, Issued: rfc(now.Add(-22 * time.Hour)), Duration: 1},
	})
	// Two stopped extractors on one planet; one running dry soon.
	planetID := int64(40123456)
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, []esi.Colony{{PlanetID: planetID, SolarSystemID: 30000142}})
	seedSnapshot(t, q, fixtureCharA, esi.PlanetLayoutKind(planetID), esi.PlanetLayout{Pins: []esi.PlanetPin{
		{PinID: 1, ExpiryTime: rfc(now.Add(-2 * time.Hour)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 2393}},
		{PinID: 2, ExpiryTime: rfc(now.Add(-time.Hour)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 2393}},
		{PinID: 3, ExpiryTime: rfc(now.Add(6 * time.Hour)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 2393}},
	}})
	// Contracts: one waiting answer, one courier due within a day.
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{
		{ContractID: 21, Status: "outstanding", Type: "item_exchange", Title: "Waiting one",
			AssigneeID: fixtureCharA, AcceptorID: 0, DateExpired: rfc(now.Add(24 * time.Hour))},
		{ContractID: 22, Status: "in_progress", Type: "courier", Title: "Haul fast",
			IssuerID: 424242, AcceptorID: fixtureCharA,
			DateAccepted: rfc(now.Add(-26 * time.Hour)), DaysToComplete: 2},
	})
	// A calendar event inside the next day.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCalendar, esi.CalendarEventSummaries{
		{EventID: 31, EventDate: rfc(now.Add(5 * time.Hour)), Title: "Fleet op"},
		{EventID: 32, EventDate: rfc(now.Add(72 * time.Hour)), Title: "Next week"},
	})
	// Unread mail, newest from a named sender.
	seedSnapshot(t, q, fixtureCharA, esi.SnapMailLabels, esi.MailLabels{TotalUnreadCount: 3})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMail, esi.MailHeaders{
		{MailID: 41, From: 90000111, Subject: "Hi", Timestamp: rfc(now.Add(-time.Hour)), IsRead: false},
		{MailID: 42, From: 90000112, Subject: "Old", Timestamp: rfc(now.Add(-5 * time.Hour)), IsRead: true},
	})
	app.esi.StoreCharacterName(90000111, "Sender One")
	// Market signals: undercut verdict + watched move.
	seedMarketSignals(t, app, q, userID, 1)
}

// briefingSection slices the briefing module out of a home body.
func briefingSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<h3>Briefing</h3>`)
	if start < 0 {
		t.Fatal("briefing module missing from home body")
	}
	rest := body[start:]
	if end := strings.Index(rest, "</section>"); end >= 0 {
		return rest[:end]
	}
	return rest
}

func TestBriefingDigestRules(t *testing.T) {
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
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	section := briefingSection(t, body)
	mustContain(t, "briefing", section,
		"/character/?character=90000001", // own character links to its sheet
		"2 extractors have stopped across 1 planet",
		"runs dry in",
		"1 market order expired since you last looked",
		"most recently <a href=\"/items/type/34/\">Tritanium</a>",
		"has 1 industry job finished and waiting for delivery",
		"first up: <a href=\"/items/type/587/\">Rifter</a>",
		"sell order undercut right now",
		"finishes", "queue runs dry after that",
		"1 contract waiting for your answer",
		"courier: Haul fast is due in",
		"“Fleet op” starts in",
		"has 3 unread mail",
		"newest from <a href=\"/pilot/?character=90000111\">Sender One</a>",
		"<a href=\"/items/type/34/\">Tritanium</a> moved up 8.0% over 7 days",
		"Since ",
	)
	if strings.Contains(section, "Next week") {
		t.Error("calendar event three days out appears in the briefing")
	}

	// Severity order: stopped extractors (blocking) precede the
	// mailbox news (informational).
	stoppedAt := strings.Index(section, "extractors have stopped")
	mailAt := strings.Index(section, "unread mail")
	if stoppedAt < 0 || mailAt < 0 || stoppedAt > mailAt {
		t.Errorf("briefing order: stopped=%d mail=%d, want stopped first", stoppedAt, mailAt)
	}

	// Briefing-only layout: the PI lines must still work (planet
	// layouts load for the briefing, not just attention/PI).
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `[{"id":"briefing"}]`, ID: user.ID,
	}); err != nil {
		t.Fatalf("set layout: %v", err)
	}
	_, body = getPage(t, app, cookie, "/")
	mustContain(t, "briefing-only layout", briefingSection(t, body), "2 extractors have stopped across 1 planet")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home render made %d outbound calls, want 0", got)
	}
}

func TestBriefingWindowAnchorSemantics(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrdersHistory, esi.CharOrderHistory{
		// Expired 1h ago: inside the first-visit 24h window.
		{CharOrder: esi.CharOrder{OrderID: 51, TypeID: 34, Issued: rfc(now.Add(-25 * time.Hour)), Duration: 1}, State: "expired"},
		// Expired 3d ago: outside it.
		{CharOrder: esi.CharOrder{OrderID: 52, TypeID: 34, Issued: rfc(now.Add(-4 * 24 * time.Hour)), Duration: 1}, State: "expired"},
	})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	anchor := func() string {
		t.Helper()
		raw, err := q.GetUserBriefingAnchor(ctx, user.ID)
		if err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		return raw
	}
	if got := anchor(); got != "" {
		t.Fatalf("anchor before first render = %q, want empty", got)
	}

	// First visit: the 24h window shows the recent expiry only,
	// and the anchor lands on the render time.
	_, body := getPage(t, app, cookie, "/")
	section := briefingSection(t, body)
	mustContain(t, "briefing first visit", section, "1 market order expired since you last looked")
	first := anchor()
	firstAt, ok := parseRFC3339(first)
	if !ok || time.Since(firstAt) > 5*time.Minute {
		t.Fatalf("anchor after first render = %q, want ~now", first)
	}

	// Second look a moment later: the 1h-old expiry is now behind
	// the anchor, so the digest quiets down on that rule.
	_, body = getPage(t, app, cookie, "/")
	section = briefingSection(t, body)
	if strings.Contains(section, "market order expired since you last looked") {
		t.Errorf("expired-order line still shown after the anchor passed it:\n%s", section)
	}

	// 7-day bound: an anchor 30 days old only reaches back a week.
	if err := q.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: rfc(now.Add(-30 * 24 * time.Hour)), ID: user.ID,
	}); err != nil {
		t.Fatalf("set anchor: %v", err)
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrdersHistory, esi.CharOrderHistory{
		{CharOrder: esi.CharOrder{OrderID: 53, TypeID: 34, Issued: rfc(now.Add(-3 * 24 * time.Hour)), Duration: 1}, State: "expired"},
		{CharOrder: esi.CharOrder{OrderID: 54, TypeID: 34, Issued: rfc(now.Add(-11 * 24 * time.Hour)), Duration: 1}, State: "expired"},
	})
	_, body = getPage(t, app, cookie, "/")
	section = briefingSection(t, body)
	mustContain(t, "briefing 7d bound", section, "1 market order expired since you last looked")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home renders made %d outbound calls, want 0", got)
	}
}

func TestBriefingAnchorUntouchedWhenModuleRemoved(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	// A saved layout without the briefing module.
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `[{"id":"fleet"}]`, ID: user.ID,
	}); err != nil {
		t.Fatalf("seed layout: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	if strings.Contains(body, "<h3>Briefing</h3>") {
		t.Error("briefing rendered despite being removed from the layout")
	}
	raw, err := q.GetUserBriefingAnchor(ctx, user.ID)
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	if raw != "" {
		t.Errorf("anchor = %q after a render without the module, want untouched", raw)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home render made %d outbound calls, want 0", got)
	}
}

func TestBriefingCapAndDedupe(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// Fifteen waiting contracts aggregate to ONE line (dedupe
	// per character+rule), and fifteen calendar events flood the
	// rest past the cap.
	var contracts []esi.Contract
	for i := 0; i < 15; i++ {
		contracts = append(contracts, esi.Contract{
			ContractID: int64(100 + i), Status: "outstanding", Type: "item_exchange",
			Title: fmt.Sprintf("Deal %d", i), AssigneeID: fixtureCharA, AcceptorID: 0,
			DateExpired: rfc(now.Add(24 * time.Hour)),
		})
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, contracts)
	var events esi.CalendarEventSummaries
	for i := 0; i < 15; i++ {
		events = append(events, esi.CalendarEventSummary{
			EventID: int64(200 + i), EventDate: rfc(now.Add(time.Duration(i+1) * time.Hour)), Title: fmt.Sprintf("Op %d", i),
		})
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapCalendar, events)

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	_, body := getPage(t, app, cookie, "/")
	section := briefingSection(t, body)
	mustContain(t, "briefing dedupe", section, "15 contracts waiting for your answer")
	if n := strings.Count(section, "contract"); n > 2 {
		t.Errorf("briefing repeats the contract rule (%d mentions), want one aggregated line", n)
	}
	li := strings.Count(section, "<li>")
	if li > briefingCap {
		t.Errorf("briefing lists %d lines, want at most %d", li, briefingCap)
	}
	if li == 0 {
		t.Fatal("briefing lists no lines despite the flood")
	}
	mustContain(t, "briefing cap", section, "more need your attention")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home render made %d outbound calls, want 0", got)
	}
}

func TestBriefingQuietAndSlots(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// Quiet account: nothing seeded, calm line, no rules fired.
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	_, body := getPage(t, app, cookie, "/")
	section := briefingSection(t, body)
	mustContain(t, "briefing quiet", section, "All quiet — nothing needs you right now.")

	// An industrially-active character whose slots stand idle:
	// Mass Production IV → 5 manufacturing slots, none in use.
	if _, err := app.db.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (3392, 'Mass Production', 1), (3406, 'Laboratory Operation', 1)`); err != nil {
		t.Fatalf("seed slot skills: %v", err)
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{Skills: []esi.Skill{
		{SkillID: 3392, ActiveSkillLevel: 4, TrainedSkillLevel: 4},
	}})
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 11, ActivityID: 1, Status: "delivered", ProductTypeID: 587,
			EndDate: rfc(now.Add(-72 * time.Hour)), CompletedDate: rfc(now.Add(-72 * time.Hour))},
	})
	_, body = getPage(t, app, cookie, "/")
	section = briefingSection(t, body)
	mustContain(t, "briefing slots", section, "has 5 of 5 manufacturing slots idle")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home renders made %d outbound calls, want 0", got)
	}
}

func TestBriefingFontPreloadAndFooter(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "home head", body,
		`<link rel="preload" as="font" type="font/woff2" href="/static/fonts/shentox-regular.woff2" crossorigin>`,
		`<link rel="preload" as="font" type="font/woff2" href="/static/fonts/univers-next-pro-medium-condensed.woff2" crossorigin>`,
		`<link rel="stylesheet" href="/static/style.css?v=v0.3.30.002">`,
		`Powered by EveSynapse v0.3.30.002 🏓 by <a href="https://github.com/natemsz" target="_blank" rel="noopener noreferrer">natemsz</a>`,
	)

	// The font file itself rides the immutable cache header.
	req := httptest.NewRequest(http.MethodGet, "/static/fonts/shentox-regular.woff2", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("font GET: status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") || !strings.Contains(got, "max-age=31536000") {
		t.Errorf("font Cache-Control = %q, want public immutable year", got)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("renders made %d outbound calls, want 0", got)
	}
}

func TestBriefingAnchorWriteSkippedWhenFresh(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	readAnchor := func() string {
		t.Helper()
		raw, err := q.GetUserBriefingAnchor(ctx, user.ID)
		if err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		return raw
	}

	// An anchor half a minute old is already inside the step: a
	// repeat home render must leave the stored value untouched
	// byte for byte -- proof the render performed no write.
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339)
	if err := q.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: fresh, ID: user.ID,
	}); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	code, _ := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	if got := readAnchor(); got != fresh {
		t.Fatalf("anchor after repeat render = %q, want untouched %q", got, fresh)
	}

	// An anchor older than the step still advances to ~now.
	stale := now.Add(-2 * time.Hour).Format(time.RFC3339)
	if err := q.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: stale, ID: user.ID,
	}); err != nil {
		t.Fatalf("seed stale anchor: %v", err)
	}
	code, _ = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / after stale anchor: status %d", code)
	}
	got := readAnchor()
	at, ok := parseRFC3339(got)
	if !ok || got == stale || time.Since(at) > 5*time.Minute {
		t.Fatalf("anchor after stale render = %q, want advanced to ~now", got)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home renders made %d outbound calls, want 0", got)
	}
}
