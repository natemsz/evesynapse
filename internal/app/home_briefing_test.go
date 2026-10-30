package app

// Hermetic tests for the Briefing home module: seeded
// snapshots drive the real router against a counting transport —
// the digest renders cache-only (zero outbound calls), its lines
// fire and stay quiet on the rule fixtures, the cap and the
// per-character dedupe hold, and the "since you last looked"
// anchor obeys its window semantics (first visit 24h, bounded to
// 7 days, advancing only after a render that includes the module).

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
		"/character/?character=90000001&view=1", // own character links to its sheet, as a view
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
		// Deep links: every card lands on the character-scoped
		// page for the thing it warns about — the undercut line
		// on that character's orders (not the market front page),
		// skill lines on the sheet's skills section, and the
		// per-character pages for jobs, planets, contracts,
		// calendar and mail.
		"<a href=\"/orders/?character=90000001&view=1\">Orders</a>",
		"<a href=\"/character/?character=90000001&view=1#skills\">Skills</a>",
		"<a href=\"/industry/jobs/?character=90000001&view=1\">My Jobs</a>",
		"<a href=\"/planets/?character=90000001&view=1\">Colonies</a>",
		"<a href=\"/contracts/?character=90000001&view=1\">Contracts</a>",
		"<a href=\"/calendar/?character=90000001&view=1\">Calendar</a>",
		"<a href=\"/mail/?character=90000001&view=1\">Mail</a>",
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

	anchor := func() sql.NullTime {
		t.Helper()
		raw, err := q.GetUserBriefingAnchor(ctx, user.ID)
		if err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		return raw
	}
	if got := anchor(); got.Valid {
		t.Fatalf("anchor before first render = %v, want unset", got.Time)
	}

	// First visit: the 24h window shows the recent expiry only,
	// and the anchor lands on the render time.
	_, body := getPage(t, app, cookie, "/")
	section := briefingSection(t, body)
	mustContain(t, "briefing first visit", section, "1 market order expired since you last looked")
	first := anchor()
	if !first.Valid || time.Since(first.Time) > 5*time.Minute {
		t.Fatalf("anchor after first render = %v, want ~now", first.Time)
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
		LastBriefingAt: timeSet(now.Add(-30 * 24 * time.Hour)), ID: user.ID,
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
	if raw.Valid {
		t.Errorf("anchor = %v after a render without the module, want untouched", raw.Time)
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
	// Count rule mentions in the visible text only: card markup
	// (the category icon's class name) legitimately repeats the
	// word without repeating the line.
	visibleText := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(section, " ")
	if n := strings.Count(visibleText, "contract"); n > 2 {
		t.Errorf("briefing repeats the contract rule (%d mentions), want one aggregated line", n)
	}
	rows := strings.Count(section, `<div class="brief-row"`)
	if rows > briefingCap {
		t.Errorf("briefing lists %d lines, want at most %d", rows, briefingCap)
	}
	if rows == 0 {
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
	mustContain(t, "briefing quiet", section, "Nothing needs your attention right now.")

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
		`<link rel="preload" as="font" type="font/woff2" href="/static/fonts/saira-sc-400.woff2" crossorigin>`,
		`<link rel="preload" as="font" type="font/woff2" href="/static/fonts/jetbrains-mono.woff2" crossorigin>`,
		`<link rel="stylesheet" href="/static/style.css?v=`+staticAssetVersion()+`">`,
		`<a href="https://gitlab.com/natemsz/evesynapse" target="_blank" rel="noopener noreferrer">EveSynapse</a> <span class="version-number">`+appVersion+`</span> · Made in Seattle, WA by <a href="https://natems.dev" target="_blank" rel="noopener noreferrer">natemsz</a>`,
	)

	// The font file itself rides the immutable cache header.
	req := httptest.NewRequest(http.MethodGet, "/static/fonts/saira-sc-400.woff2", nil)
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

	readAnchor := func() sql.NullTime {
		t.Helper()
		raw, err := q.GetUserBriefingAnchor(ctx, user.ID)
		if err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		return raw
	}

	// An anchor half a minute old is already inside the step: a
	// repeat home render must leave the stored value exactly as
	// it was -- proof the render performed no write.
	fresh := now.Add(-30 * time.Second).Truncate(time.Second)
	if err := q.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: timeSet(fresh), ID: user.ID,
	}); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	code, _ := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	if got := readAnchor(); !got.Valid || !got.Time.Equal(fresh) {
		t.Fatalf("anchor after repeat render = %v, want untouched %v", got.Time, fresh)
	}

	// An anchor older than the step still advances to ~now.
	stale := now.Add(-2 * time.Hour).Truncate(time.Second)
	if err := q.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: timeSet(stale), ID: user.ID,
	}); err != nil {
		t.Fatalf("seed stale anchor: %v", err)
	}
	code, _ = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / after stale anchor: status %d", code)
	}
	got := readAnchor()
	if !got.Valid || got.Time.Equal(stale) || time.Since(got.Time) > 5*time.Minute {
		t.Fatalf("anchor after stale render = %v, want advanced to ~now", got.Time)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home renders made %d outbound calls, want 0", got)
	}
}

// The same count-bearing alert across three or more characters
// collapses into one card carrying the total and each character's
// count (the mockup's combined briefing cards); two characters keep
// their detailed lines, and large spreads fold the tail into
// "and N more" with the counts already inside the total.
func TestBriefingAggregatesAcrossCharacters(t *testing.T) {
	mk := func(rule string, count int, prefix string) briefingLine {
		return briefingLine{html: prefix + " detail", Rank: 1, key: rule + prefix,
			aggRule: rule, aggCount: count, aggPrefix: prefix}
	}
	lines := []briefingLine{
		mk("pi-stopped", 12, "Burzrujat"),
		mk("mail", 3, "Khastira"),
		mk("pi-stopped", 10, "Khastira"),
		mk("mail", 122, "Pinkamena"),
		mk("pi-stopped", 12, "Pinkamena"),
	}
	out := aggregateBriefing(lines)
	pi, mail := 0, 0
	for _, l := range out {
		if strings.Contains(l.html, "extractors have stopped") {
			pi++
			for _, want := range []string{"34 extractors have stopped", "Burzrujat (12)", "Khastira (10)", "Pinkamena (12)", "/planets/"} {
				if !strings.Contains(l.html, want) {
					t.Errorf("combined extractor card is missing %q: %s", want, l.html)
				}
			}
		}
		if l.aggRule == "mail" {
			mail++
		}
	}
	if pi != 1 {
		t.Errorf("three characters' stopped extractors made %d cards, want 1", pi)
	}
	if mail != 2 {
		t.Errorf("two characters' mail made %d lines, want the 2 detailed lines", mail)
	}

	five := []briefingLine{}
	for i, n := range []int{1, 2, 3, 4, 5} {
		five = append(five, mk("mail", n, fmt.Sprintf("Char%d", i)))
	}
	out = aggregateBriefing(five)
	if len(out) != 1 || !strings.Contains(out[0].html, "15 unread mails") ||
		!strings.Contains(out[0].html, "and 1 more") || strings.Contains(out[0].html, "Char4") {
		t.Errorf("five-way mail spread did not fold into one card: %+v", out)
	}
}

// Aggregated cards link each character's segment to that
// character's own destination for the warning — their planets,
// their unread mail — not to the character sheet.
func TestBriefingAggregatesDeepLinks(t *testing.T) {
	mk := func(rule string, count int, name, href string) briefingLine {
		return briefingLine{html: "detail", Rank: 1, key: rule + name,
			aggRule: rule, aggCount: count, aggPrefix: "sheet:" + name, aggName: name, aggHref: href}
	}
	lines := []briefingLine{
		mk("pi-stopped", 4, "Alpha", "/planets/?character=11"),
		mk("pi-stopped", 2, "Bravo", "/planets/?character=22"),
		mk("pi-stopped", 1, "Charlie", "/planets/?character=33"),
		mk("mail", 5, "Alpha", "/mail/?character=11"),
		mk("mail", 6, "Bravo", "/mail/?character=22"),
		mk("mail", 7, "Charlie", "/mail/?character=33"),
	}
	out := aggregateBriefing(lines)
	if len(out) != 2 {
		t.Fatalf("aggregated lines = %d, want 2 combined cards: %+v", len(out), out)
	}
	joined := ""
	for _, l := range out {
		joined += l.html + "\n"
	}
	for _, want := range []string{
		"<a href=\"/planets/?character=11\">Alpha</a> (4)",
		"<a href=\"/planets/?character=22\">Bravo</a> (2)",
		"<a href=\"/planets/?character=33\">Charlie</a> (1)",
		"<a href=\"/mail/?character=11\">Alpha</a> (5)",
		"<a href=\"/mail/?character=22\">Bravo</a> (6)",
		"<a href=\"/mail/?character=33\">Charlie</a> (7)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("combined cards missing deep segment %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "sheet:") {
		t.Errorf("combined cards fell back to sheet segments:\n%s", joined)
	}
}

// TestBriefingDismissedLines: the briefing groups its lines into
// one stack per kind; a dismissed line leaves the stacks and
// renders in the dismissed list while it still applies, and
// restoring it brings it back.
func TestBriefingDismissedLines(t *testing.T) {
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

	_, body := getPage(t, app, cookie, "/")
	section := briefingSection(t, body)
	mustContain(t, "briefing stacks", section, "brief-stack", ">Mail<", "has 3 unread mail")
	if strings.Contains(section, "Dismissed (") {
		t.Fatal("dismissed list offered before anything was dismissed")
	}

	if code := post("/home/briefing/dismiss", mailKey); code != http.StatusSeeOther {
		t.Fatalf("dismiss: status %d, want %d", code, http.StatusSeeOther)
	}
	_, body = getPage(t, app, cookie, "/")
	section = briefingSection(t, body)
	mustContain(t, "briefing dismissed", section, "Dismissed (1)", "brief-dismissed-modal")
	if n := strings.Count(section, "has 3 unread mail"); n != 1 {
		t.Errorf("mail line appears %d times after dismissal, want 1 (the dismissed list only)", n)
	}

	if code := post("/home/briefing/restore", mailKey); code != http.StatusSeeOther {
		t.Fatalf("restore: status %d, want %d", code, http.StatusSeeOther)
	}
	_, body = getPage(t, app, cookie, "/")
	section = briefingSection(t, body)
	mustContain(t, "briefing restored", section, "has 3 unread mail")
	if strings.Contains(section, "Dismissed (") {
		t.Error("dismissed list still offered after the restore")
	}
}
