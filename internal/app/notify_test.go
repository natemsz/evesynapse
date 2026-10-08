package app

// Notifications: what the worker pass announces, and above all what
// it keeps quiet about. Stored snapshots only; a transport that
// counts calls proves the pass never asks ESI for anything.

import (
	"context"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// notifyFixture is an app with one account and one character, and
// the means to run the pass at a chosen moment.
type notifyFixture struct {
	t      *testing.T
	app    *Application
	q      *db.Queries
	ctx    context.Context
	userID int64
	ch     db.Character
	calls  *countingTransport
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	return &notifyFixture{t: t, app: app, q: q, ctx: ctx, userID: user.ID, ch: ch, calls: transport}
}

// pass runs the worker's notification pass at the given moment and
// returns how many notifications it created.
func (f *notifyFixture) pass(now time.Time) int {
	f.t.Helper()
	chars, err := f.q.ListAllCharacters(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.app.notifyPass(f.ctx, chars, now)
}

// titles lists the account's notifications, oldest first.
func (f *notifyFixture) titles() []string {
	f.t.Helper()
	rows, err := f.q.ListNotifications(f.ctx, db.ListNotificationsParams{UserID: f.userID, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	out := make([]string, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].Kind+": "+rows[i].Title)
	}
	return out
}

func (f *notifyFixture) wantTitles(when string, want ...string) {
	f.t.Helper()
	got := f.titles()
	if len(got) != len(want) {
		f.t.Fatalf("%s: %d notification(s) %q, want %d %q", when, len(got), got, len(want), want)
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			f.t.Fatalf("%s: notification %d is %q, want it to contain %q", when, i+1, got[i], want[i])
		}
	}
}

func (f *notifyFixture) seed(kind string, payload any) {
	f.t.Helper()
	seedSnapshot(f.t, f.q, f.ch.CharacterID, kind, payload)
}

var notifyT0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// TestNotifyBaselineThenNews is the rule everything else rests on:
// what is already there when a source first becomes readable is not
// news, and each thing that happens afterwards is announced once.
func TestNotifyBaselineThenNews(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0

	// A character arrives with a history: unread mail, a finished
	// skill, a killmail, a finished job, a stopped extractor.
	f.seed(esi.SnapMail, esi.MailHeaders{
		{MailID: 1, From: 555, Subject: "Old news", Timestamp: rfc(now.Add(-48 * time.Hour))},
	})
	f.seed(esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, FinishedLevel: 4, FinishDate: rfc(now.Add(-time.Hour))},
		{SkillID: 3301, FinishedLevel: 5, FinishDate: rfc(now.Add(2 * time.Hour))},
	})
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 10, KillmailHash: "a"}})
	f.seed(esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 20, Status: "ready", ProductTypeID: 34, EndDate: rfc(now.Add(-time.Hour))},
		{JobID: 21, Status: "active", ProductTypeID: 35, EndDate: rfc(now.Add(3 * time.Hour))},
	})
	f.seed(esi.SnapCalendar, esi.CalendarEventSummaries{
		{EventID: 30, Title: "Standing fleet", EventDate: rfc(now.Add(24 * time.Hour))},
	})
	f.seed(esi.SnapPlanets, esi.Colonies{{PlanetID: fixturePlanetA, SolarSystemID: 30000142}})
	f.seed(esi.PlanetLayoutKind(fixturePlanetA), esi.PlanetLayout{Pins: []esi.PlanetPin{
		{PinID: 1, ExpiryTime: rfc(now.Add(-time.Hour)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 34}},
	}})

	if n := f.pass(now); n != 0 {
		t.Fatalf("the first pass created %d notification(s) %q; a character's history is not news", n, f.titles())
	}
	if n := f.pass(now.Add(time.Minute)); n != 0 {
		t.Fatalf("a second pass over the same data created %d notification(s) %q", n, f.titles())
	}

	// Then things happen. Time passes: the second skill and the
	// second job finish without any new data arriving.
	later := now.Add(4 * time.Hour)
	if n := f.pass(later); n != 2 {
		t.Fatalf("after the skill and the job finished: %d notification(s) %q, want 2", n, f.titles())
	}
	f.wantTitles("time passing", "skill: Fixture Ceo finished training", "industry: Fixture Ceo: the")

	// And new data arrives: a mail, a killmail, a calendar event, and
	// the extractor is restarted and stops again.
	f.seed(esi.SnapMail, esi.MailHeaders{
		{MailID: 1, From: 555, Subject: "Old news", Timestamp: rfc(now.Add(-48 * time.Hour))},
		{MailID: 2, From: 556, Subject: "Fleet tonight", Timestamp: rfc(later)},
		{MailID: 3, From: f.ch.CharacterID, Subject: "Note to self", Timestamp: rfc(later)},
		{MailID: 4, From: 557, Subject: "Already opened", Timestamp: rfc(later), IsRead: true},
	})
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 11, KillmailHash: "b"}, {KillmailID: 10, KillmailHash: "a"}})
	f.seed(esi.SnapCalendar, esi.CalendarEventSummaries{
		{EventID: 30, Title: "Standing fleet", EventDate: rfc(now.Add(24 * time.Hour))},
		{EventID: 31, Title: "Roam", EventDate: rfc(later.Add(6 * time.Hour))},
		{EventID: 32, Title: "Long over", EventDate: rfc(later.Add(-6 * time.Hour))},
	})
	f.seed(esi.PlanetLayoutKind(fixturePlanetA), esi.PlanetLayout{Pins: []esi.PlanetPin{
		{PinID: 1, ExpiryTime: rfc(later.Add(-10 * time.Minute)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 34}},
		{PinID: 2, ExpiryTime: rfc(later.Add(-10 * time.Minute)), ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 35}},
	}})
	if n := f.pass(later); n != 4 {
		t.Fatalf("after new data: %d notification(s), want 4 (one mail, one killmail, one event, one planet); all: %q", n, f.titles())
	}
	f.wantTitles("new data",
		"skill: ", "industry: ",
		"mail: Fixture Ceo has new mail", "calendar: Fixture Ceo has a new calendar event: Roam",
		"killmail: Fixture Ceo has a new killmail", "pi: Fixture Ceo: extractors have stopped on")
	if got := strings.Join(f.titles(), "\n"); !strings.Contains(got, "Fleet tonight") ||
		strings.Contains(got, "Note to self") || strings.Contains(got, "Already opened") || strings.Contains(got, "Long over") {
		t.Fatalf("the mail from oneself, the mail already read or the event already past was announced, or the new mail's subject is missing:\n%s", got)
	}

	// Nothing is ever said twice, however often the pass runs.
	for i := 1; i <= 3; i++ {
		if n := f.pass(later.Add(time.Duration(i) * time.Minute)); n != 0 {
			t.Fatalf("repeat pass %d created %d notification(s): %q", i, n, f.titles())
		}
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("the notification pass made %d outbound call(s); it reads stored data only", got)
	}
}

// TestNotifyEachPlanetHasItsOwnBaseline: a planet's layout arrives
// after the colony list. An extractor that had already stopped when
// its layout first became readable is history, not news.
func TestNotifyEachPlanetHasItsOwnBaseline(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	f.seed(esi.SnapPlanets, esi.Colonies{{PlanetID: fixturePlanetA}, {PlanetID: fixturePlanetB}})
	f.seed(esi.PlanetLayoutKind(fixturePlanetA), esi.PlanetLayout{Pins: []esi.PlanetPin{
		{PinID: 1, ExpiryTime: rfc(now.Add(time.Hour)), ExtractorDetails: &esi.PlanetExtractor{}},
	}})
	if n := f.pass(now); n != 0 {
		t.Fatalf("first pass: %d notification(s)", n)
	}
	// Planet B's layout turns up a cycle later, its extractor long stopped.
	f.seed(esi.PlanetLayoutKind(fixturePlanetB), esi.PlanetLayout{Pins: []esi.PlanetPin{
		{PinID: 9, ExpiryTime: rfc(now.Add(-72 * time.Hour)), ExtractorDetails: &esi.PlanetExtractor{}},
	}})
	if n := f.pass(now.Add(time.Minute)); n != 0 {
		t.Fatalf("a planet first seen with a stopped extractor was announced: %q", f.titles())
	}
	// Planet A's extractor then runs out: that is news.
	if n := f.pass(now.Add(2 * time.Hour)); n != 1 {
		t.Fatalf("after planet A's extractor stopped: %d notification(s) %q, want 1", n, f.titles())
	}
}

// TestNotifyKindSwitchedOff: a kind the user does not want is not
// announced, and switching it back on does not replay what happened
// while it was off.
func TestNotifyKindSwitchedOff(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	f.seed(esi.SnapMail, esi.MailHeaders{})
	f.seed(esi.SnapKillmails, []esi.KillmailRef{})
	f.pass(now) // baselines

	setPrefs := func(blob string) {
		t.Helper()
		if err := f.q.UpsertWidgetConfig(f.ctx, db.UpsertWidgetConfigParams{
			UserID: f.userID, WidgetID: notifyConfigID, Config: blob, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	setPrefs(`{"off":["mail","no-such-kind"]}`)
	f.seed(esi.SnapMail, esi.MailHeaders{{MailID: 1, From: 555, Subject: "While you were out"}})
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 10}})
	if n := f.pass(now.Add(time.Minute)); n != 1 {
		t.Fatalf("with mail switched off: %d notification(s) %q, want only the killmail", n, f.titles())
	}
	f.wantTitles("mail off", "killmail: ")

	setPrefs(`{}`)
	if n := f.pass(now.Add(2 * time.Minute)); n != 0 {
		t.Fatalf("switching mail back on replayed %d old notification(s): %q", n, f.titles())
	}
	f.seed(esi.SnapMail, esi.MailHeaders{
		{MailID: 1, From: 555, Subject: "While you were out"},
		{MailID: 2, From: 555, Subject: "After"},
	})
	if n := f.pass(now.Add(3 * time.Minute)); n != 1 {
		t.Fatalf("mail back on, one new mail: %d notification(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("mail on again", "killmail: ", "mail: Fixture Ceo has new mail: After")

	// Settings that cannot be read as settings mean everything is on.
	for _, blob := range []string{"", "not json", `{"off":"mail"}`, `[]`} {
		if p := parseNotifyPrefs(blob); len(p.Off) != 0 {
			t.Errorf("parseNotifyPrefs(%q) switched off %q, want nothing", blob, p.Off)
		}
	}
	if p := parseNotifyPrefs(`{"off":["mail","mail","skill","bogus"]}`); len(p.Off) != 2 || !p.off("mail") || !p.off("skill") {
		t.Errorf("parseNotifyPrefs kept %q, want mail and skill once each", p.Off)
	}
}

// TestNotifyWatchListIsAStandingState: a watched price past its
// threshold is announced when it gets there, not on every pass while
// it stays, and again if it comes back inside and leaves once more.
func TestNotifyWatchListIsAStandingState(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	const typeID = int64(34)
	history := func(date string, average float64) {
		t.Helper()
		if err := f.q.UpsertMarketHistory(f.ctx, db.UpsertMarketHistoryParams{
			RegionID: forge, TypeID: typeID, Date: date,
			Average: average, Highest: average, Lowest: average, Volume: 100, OrderCount: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.q.UpsertWatchlistEntry(f.ctx, db.UpsertWatchlistEntryParams{
		UserID: f.userID, TypeID: typeID, RegionID: forge, ThresholdPct: 10, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	history("2026-09-25", 100)
	history("2026-10-02", 101)
	if n := f.pass(now); n != 0 {
		t.Fatalf("a quiet price: %d notification(s) %q", n, f.titles())
	}
	// It jumps 30% in a week.
	history("2026-10-03", 102)
	history("2026-10-10", 133)
	if n := f.pass(now.Add(time.Minute)); n != 1 {
		t.Fatalf("after the jump: %d notification(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("jump", "watch: ")
	if n := f.pass(now.Add(2 * time.Minute)); n != 0 {
		t.Fatalf("still past the threshold: announced again (%q)", f.titles())
	}
	// The figure moves on while it stays past the threshold: the same alert.
	history("2026-10-10", 140)
	if n := f.pass(now.Add(150 * time.Second)); n != 0 {
		t.Fatalf("the move growing from 30%% to 37%% was announced as a new alert: %q", f.titles())
	}
	// It settles: a week on, the price is where it was a week before.
	history("2026-10-17", 134)
	if n := f.pass(now.Add(3 * time.Minute)); n != 0 {
		t.Fatalf("the price settling was announced: %q", f.titles())
	}
	// And jumps again: a new alert.
	history("2026-10-24", 180)
	if n := f.pass(now.Add(4 * time.Minute)); n != 1 {
		t.Fatalf("after a second jump: %d new notification(s), want 1; all: %q", n, f.titles())
	}
}

// TestNotifyAccountsAreSeparate: one account's events never reach
// another, and removing an account takes its notifications with it.
func TestNotifyAccountsAreSeparate(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherCh := seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Alt")
	f.seed(esi.SnapKillmails, []esi.KillmailRef{})
	seedSnapshot(t, f.q, otherCh.CharacterID, esi.SnapKillmails, []esi.KillmailRef{})
	f.pass(now)

	seedSnapshot(t, f.q, otherCh.CharacterID, esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 77}})
	if n := f.pass(now.Add(time.Minute)); n != 1 {
		t.Fatalf("%d notification(s) created, want 1", n)
	}
	f.wantTitles("the first account") // none
	rows, err := f.q.ListNotifications(f.ctx, db.ListNotificationsParams{UserID: other.ID, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].CharacterID != otherCh.CharacterID || rows[0].ReadAt.Valid {
		t.Fatalf("the second account has %d notification(s) (%v), want one unread for its own character", len(rows), err)
	}

	if _, err := f.app.db.ExecContext(f.ctx, `DELETE FROM users WHERE id = $1`, other.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	var left int
	if err := f.app.db.QueryRowContext(f.ctx,
		`SELECT (SELECT COUNT(*) FROM notifications WHERE user_id = $1) + (SELECT COUNT(*) FROM notification_seen WHERE user_id = $1)`,
		other.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d notification row(s) outlived their account (%v)", left, err)
	}
}

// TestNotifySeenKeysAreRenewedAndPruned: a key that is still current
// is kept alive, so a mail left unread for months is not announced a
// second time; a key that stopped being current is eventually dropped.
func TestNotifySeenKeysAreRenewedAndPruned(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	notifyPrunedAt.Store(0)
	f.seed(esi.SnapMail, esi.MailHeaders{})
	f.seed(esi.SnapKillmails, []esi.KillmailRef{})
	f.pass(now)
	f.seed(esi.SnapMail, esi.MailHeaders{{MailID: 1, From: 555, Subject: "Kept unread"}})
	f.seed(esi.SnapKillmails, []esi.KillmailRef{{KillmailID: 10}})
	if n := f.pass(now.Add(time.Minute)); n != 2 {
		t.Fatalf("%d notification(s), want 2", n)
	}
	// The killmail rolls off the recent list; the mail stays unread.
	f.seed(esi.SnapKillmails, []esi.KillmailRef{})
	total := 2
	for day := 1; day <= 120; day++ {
		total += f.pass(now.Add(time.Duration(day) * 24 * time.Hour))
	}
	if total != 2 {
		t.Fatalf("over four months %d notification(s) were created, want the original 2: %q", total, f.titles())
	}
	seen := func(key string) bool {
		t.Helper()
		keys, err := f.q.ListNotificationSeenKeys(f.ctx, db.ListNotificationSeenKeysParams{UserID: f.userID, EventKeys: []string{key}})
		if err != nil {
			t.Fatal(err)
		}
		return len(keys) == 1
	}
	if !seen("mail|90000001|1") {
		t.Fatal("the key of a mail that is still unread was dropped; it would be announced again")
	}
	var renewed time.Time
	if err := f.app.db.QueryRowContext(f.ctx, `SELECT MIN(seen_at) FROM notification_seen WHERE user_id = $1 AND event_key IN ('mail|90000001|1', 'base|mail|90000001')`, f.userID).Scan(&renewed); err != nil {
		t.Fatal(err)
	}
	if age := now.Add(120 * 24 * time.Hour).Sub(renewed); age > 2*notifySeenTouch {
		t.Fatalf("keys still current were last renewed %s ago; they survive only by being re-created, which would swallow real news", age)
	}
	if seen("killmail|90000001|10") {
		t.Fatal("the key of a killmail gone from the list four months ago is still stored")
	}
	// The notifications themselves do not pile up for ever either.
	if got := f.titles(); len(got) != 0 {
		t.Fatalf("notifications from four months ago are still stored: %q", got)
	}
}
