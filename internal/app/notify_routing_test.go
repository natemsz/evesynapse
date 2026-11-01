package app

// Notification routing: the per-kind switches gate delivery, and
// the new kinds fire once per transition. Stored snapshots only;
// the counting transport proves the pass never asks ESI.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestNotifyOffKindStaysSilentAndDoesNotReplay: switching a kind
// off keeps its news quiet, and switching it back on starts from
// then, not from what it missed.
func TestNotifyOffKindStaysSilentAndDoesNotReplay(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	f.seed(esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, FinishedLevel: 4, FinishDate: rfc(now.Add(-time.Hour))},
	})
	if err := f.app.storeNotifyPrefs(f.ctx, f.userID, notifyPrefs{Off: []string{notifySkill}}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	if n := f.pass(now); n != 0 {
		t.Fatalf("pass with skill off created %d notification(s) %q, want 0", n, f.titles())
	}
	if err := f.app.storeNotifyPrefs(f.ctx, f.userID, notifyPrefs{}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	if n := f.pass(now.Add(time.Minute)); n != 0 {
		t.Fatalf("pass after switching skill back on created %d notification(s) %q, want 0 (no backlog)", n, f.titles())
	}
	f.seed(esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, FinishedLevel: 4, FinishDate: rfc(now.Add(-time.Hour))},
		{SkillID: 3301, FinishedLevel: 5, FinishDate: rfc(now.Add(-time.Minute))},
	})
	if n := f.pass(now.Add(2 * time.Minute)); n != 1 {
		t.Fatalf("pass with a newly finished skill created %d notification(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("skill back on", "skill: Fixture Ceo finished training")
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("pass made %d outbound calls, want 0", got)
	}
}

// TestNotifyIndustryFollowsItsSwitch: the industry kind is gated
// the same way.
func TestNotifyIndustryFollowsItsSwitch(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	f.seed(esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 20, Status: "ready", ProductTypeID: 34, EndDate: rfc(now.Add(-time.Hour))},
	})
	if err := f.app.storeNotifyPrefs(f.ctx, f.userID, notifyPrefs{Off: []string{notifyIndustry}}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	if n := f.pass(now); n != 0 {
		t.Fatalf("pass with industry off created %d notification(s) %q, want 0", n, f.titles())
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("pass made %d outbound calls, want 0", got)
	}
}

// contractFixture seeds one contract snapshot for the fixture
// character and runs the pass.
func contractFixturePass(t *testing.T, f *notifyFixture, contracts []esi.Contract, now time.Time) int {
	t.Helper()
	f.seed(esi.SnapContracts, contracts)
	return f.pass(now)
}

// TestNotifyContractTransitions: accepted, finished and expired
// each announce once; repeat polls stay quiet, and contracts that
// were already finished when first seen are history, not news.
func TestNotifyContractTransitions(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	mine := []esi.Contract{
		{ContractID: 1, Type: "item_exchange", Title: "Spare hulls", Status: "outstanding",
			IssuerID: fixtureCharA, AcceptorID: 0, AssigneeID: fixtureCharB, DateExpired: rfc(now.Add(24 * time.Hour))},
		{ContractID: 2, Type: "courier", Title: "Haul fast", Status: "in_progress",
			IssuerID: 424242, AcceptorID: fixtureCharA, DateAccepted: rfc(now.Add(-time.Hour))},
		{ContractID: 3, Type: "item_exchange", Title: "Old deal", Status: "finished",
			IssuerID: fixtureCharA, AcceptorID: 424242, DateCompleted: rfc(now.Add(-48 * time.Hour))},
	}
	if n := contractFixturePass(t, f, mine, now); n != 0 {
		t.Fatalf("first pass created %d notification(s) %q; history is not news", n, f.titles())
	}
	if n := contractFixturePass(t, f, mine, now.Add(time.Minute)); n != 0 {
		t.Fatalf("second pass created %d notification(s) %q, want 0", n, f.titles())
	}

	// Someone takes up contract 1.
	mine[0].Status = "in_progress"
	mine[0].AcceptorID = fixtureCharB
	if n := contractFixturePass(t, f, mine, now.Add(2*time.Minute)); n != 1 {
		t.Fatalf("accepted pass created %d notification(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("accepted", "contract: Fixture Ceo: Spare hulls was accepted")

	// The courier finishes, and contract 3 stays finished history.
	mine[1].Status = "finished"
	mine[1].DateCompleted = rfc(now.Add(3 * time.Minute))
	if n := contractFixturePass(t, f, mine, now.Add(4*time.Minute)); n != 1 {
		t.Fatalf("finished pass created %d notification(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("finished", "contract: Fixture Ceo: Spare hulls was accepted", "contract: Fixture Ceo: Haul fast finished")

	// A courier that already failed on arrival is history, not news.
	mine = append(mine, esi.Contract{ContractID: 4, Type: "courier", Title: "Late boxes", Status: "failed",
		IssuerID: 424242, AcceptorID: fixtureCharA})
	if n := contractFixturePass(t, f, mine, now.Add(5*time.Minute)); n != 0 {
		t.Fatalf("failed-arrival pass created %d notification(s) %q, want 0", n, f.titles())
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("pass made %d outbound calls, want 0", got)
	}
}

// TestNotifyContractExpired: a lapsed outstanding contract and a
// failed one both read as expired, once each.
func TestNotifyContractExpired(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	lapsed := []esi.Contract{
		{ContractID: 5, Type: "courier", Status: "outstanding",
			IssuerID: fixtureCharA, AcceptorID: 0, DateExpired: rfc(now.Add(-24 * time.Hour))},
		{ContractID: 6, Type: "item_exchange", Title: "Gone wrong", Status: "failed",
			IssuerID: fixtureCharA, AcceptorID: 424242},
	}
	// Both arrive already expired: history, not news.
	if n := contractFixturePass(t, f, lapsed, now); n != 0 {
		t.Fatalf("baseline created %d notification(s) %q, want 0", n, f.titles())
	}
	// Fresh outstanding contracts that lapse and fail afterwards.
	fresh := []esi.Contract{
		{ContractID: 7, Type: "courier", Title: "Fresh haul", Status: "outstanding",
			IssuerID: fixtureCharA, AcceptorID: 0, DateExpired: rfc(now.Add(time.Hour))},
		{ContractID: 8, Type: "item_exchange", Title: "Fresh deal", Status: "outstanding",
			IssuerID: fixtureCharA, AcceptorID: 0, DateExpired: rfc(now.Add(time.Hour))},
	}
	if n := contractFixturePass(t, f, fresh, now.Add(time.Minute)); n != 0 {
		t.Fatalf("fresh baseline created %d notification(s) %q, want 0", n, f.titles())
	}
	fresh[0].Status = "failed"
	fresh[1].DateExpired = rfc(now.Add(-time.Minute))
	if n := contractFixturePass(t, f, fresh, now.Add(2*time.Minute)); n != 2 {
		t.Fatalf("expired pass created %d notification(s) %q, want 2", n, f.titles())
	}
	got := f.titles()
	if len(got) != 2 || !strings.Contains(got[0], "Fresh haul expired") || !strings.Contains(got[1], "Fresh deal expired") {
		t.Fatalf("expired titles = %q, want both expiries", got)
	}
}

// TestNotifyRelinkTransition: a parked token notifies once, on the
// transition, and a merely changed owner stays quiet.
func TestNotifyRelinkTransition(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	if n := f.pass(now); n != 0 {
		t.Fatalf("linked pass created %d notification(s) %q, want 0", n, f.titles())
	}
	if err := f.q.SetCharacterLinkState(f.ctx, db.SetCharacterLinkStateParams{
		LinkState: linkStateTokenDead, LinkStateAt: mustNullTime(now.Format(time.RFC3339)), CharacterID: fixtureCharA,
	}); err != nil {
		t.Fatalf("park token: %v", err)
	}
	if n := f.pass(now.Add(time.Minute)); n != 1 {
		t.Fatalf("parked pass created %d notification(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("parked", "relogin: Fixture Ceo needs to sign in again")
	if n := f.pass(now.Add(2 * time.Minute)); n != 0 {
		t.Fatalf("repeat pass created %d notification(s) %q, want 0", n, f.titles())
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("pass made %d outbound calls, want 0", got)
	}
}

// TestNotifyRelinkIgnoresOwnerChanged: only a dead token pages.
func TestNotifyRelinkIgnoresOwnerChanged(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	if err := f.q.SetCharacterLinkState(f.ctx, db.SetCharacterLinkStateParams{
		LinkState: linkStateOwnerChanged, LinkStateAt: mustNullTime(now.Format(time.RFC3339)), CharacterID: fixtureCharA,
	}); err != nil {
		t.Fatalf("mark owner change: %v", err)
	}
	if n := f.pass(now); n != 0 {
		t.Fatalf("owner-changed pass created %d notification(s) %q, want 0", n, f.titles())
	}
}

// TestNotifyDigestNeverNotifies: the undercut digest is a card
// only; beaten orders never become one notification each.
func TestNotifyDigestNeverNotifies(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	if err := f.q.UpsertOrderLifecycle(f.ctx, db.UpsertOrderLifecycleParams{
		CharacterID: fixtureCharA, OrderID: 11, TypeID: 34, LocationID: 60003760, RegionID: 10000002,
		ListedPrice: 6.10, VolumeTotal: 1000, VolumeRemainLast: 800,
		FirstSeenAt: now.Add(-time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatalf("seed lifecycle: %v", err)
	}
	if err := f.q.UpdateOrderLifecycleBook(f.ctx, db.UpdateOrderLifecycleBookParams{
		Beaten: 1, CheckedAt: now, OutbidEvents: 2, BeatingPrice: 6.00,
		CharacterID: fixtureCharA, OrderID: 11,
	}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
	if n := f.pass(now); n != 0 {
		t.Fatalf("beaten-order pass created %d notification(s) %q, want 0", n, f.titles())
	}
	for _, title := range f.titles() {
		if strings.HasPrefix(title, notifyUndercut+":") {
			t.Fatalf("undercut digest notified: %q", title)
		}
	}
}

// seedCampKills stores a three-kill gate camp on one gate in the
// given system, the same shape the system page test proves a camp
// from.
func seedCampKills(t *testing.T, conn *sql.DB, systemID int64, now time.Time) {
	t.Helper()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(context.Background(), s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES ($1, 'Rancer', 10000048, 0.4) ON CONFLICT DO NOTHING`, systemID)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (22456, 'Sabre'), (587, 'Rifter'), (17738, 'Machariel') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_celestials (item_id, system_id, name) VALUES (50000001, $1, 'Stargate (Otou)') ON CONFLICT DO NOTHING`, systemID)
	kill := func(id int64, ago time.Duration, victimCorp int64, public bool, alliance, loc int64, attackers ...int64) {
		t.Helper()
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, victim_corporation_id, ship_type_id, location_id, source, public, ingested_at, value_zkill)
			VALUES ($1, $2, $3, $4, $5, 587, $7, 'stream', $6, now(), 10000000)`,
			id, now.Add(-ago), systemID, 92000000+id, victimCorp, public, loc)
		for i, a := range attackers {
			exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, alliance_id, ship_type_id, final_blow)
				VALUES ($1, $2, $3, 98000001, $5, 22456, $4)`, id, i, a, i == 0, alliance)
		}
	}
	kill(10, 5*time.Minute, 1, true, 99000001, 50000001, 91000001, 91000002)
	kill(11, 15*time.Minute, 2, true, 99000001, 50000001, 91000001)
	kill(12, 25*time.Minute, 3, true, 99000001, 50000001, 91000001, 91000003)
	kill(13, 35*time.Minute, 4, true, 99000002, 50000002, 91000009)
	kill(14, 40*time.Minute, 5, true, 99000001, 50000001, 91000001)
	kill(16, 2*time.Hour, 7, true, 99000003, 0, 91000020)
	kill(17, 3*time.Hour, 8, true, 99000003, 0, 91000021)
	kill(15, 2*time.Minute, 6, false, 99000001, 50000001, 91000001)
}

// seedOnlineNow stores a fresh online snapshot for the character.
func seedOnlineNow(t *testing.T, q *db.Queries, characterID int64, online bool) {
	t.Helper()
	payload := `{"online":false}`
	if online {
		payload = `{"online":true}`
	}
	if err := q.UpsertSnapshot(context.Background(), db.UpsertSnapshotParams{
		CharacterID: characterID, Kind: esi.SnapOnline, Payload: payload,
		FetchedAt: time.Now().UTC(), CachedUntil: mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed online: %v", err)
	}
}

// TestNotifyCampAlerts: a camp in a watched system pages while a
// character is online, once per camp; offline or unwatched, quiet.
func TestNotifyCampAlerts(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	const rancer = int64(30003504)
	now := time.Now().UTC()
	seedCampKills(t, conn, rancer, now)
	if err := app.storeNotifyPrefs(ctx, user.ID, notifyPrefs{CampSystems: []int64{rancer}}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	chars, err := q.ListAllCharacters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	titles := func() []string {
		rows, err := q.ListNotifications(ctx, db.ListNotificationsParams{UserID: user.ID, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for i := len(rows) - 1; i >= 0; i-- {
			out = append(out, rows[i].Kind+": "+rows[i].Title)
		}
		return out
	}

	// Offline: the camp stands, but nobody is online to care.
	seedOnlineNow(t, q, fixtureCharA, false)
	if n := app.notifyPass(ctx, chars, now); n != 0 {
		t.Fatalf("offline pass created %d notification(s), want 0", n)
	}
	// Online: the camp pages once, then stays quiet.
	seedOnlineNow(t, q, fixtureCharA, true)
	if n := app.notifyPass(ctx, chars, now.Add(time.Minute)); n != 1 {
		t.Fatalf("online pass created %d notification(s), want 1", n)
	}
	got := titles()
	if len(got) != 1 || !strings.Contains(got[0], "Gate camp in Rancer") {
		t.Fatalf("camp titles = %q, want the Rancer camp", got)
	}
	if n := app.notifyPass(ctx, chars, now.Add(2*time.Minute)); n != 0 {
		t.Fatalf("repeat pass created %d notification(s), want 0", n)
	}
	// Unwatched systems stay quiet.
	if err := app.storeNotifyPrefs(ctx, user.ID, notifyPrefs{}); err != nil {
		t.Fatalf("store prefs: %v", err)
	}
	if n := app.notifyPass(ctx, chars, now.Add(3*time.Minute)); n != 0 {
		t.Fatalf("unwatched pass created %d notification(s), want 0", n)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("pass made %d outbound calls, want 0", got)
	}
}
