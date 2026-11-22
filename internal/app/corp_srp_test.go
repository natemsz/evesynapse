package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// srpFixture is a corporation with a director who can send EVE mail,
// a pilot, an accountant, and an op the pilot lost a ship on.
type srpFixture struct {
	*notifyFixture
	mail                    *mailSendTransport
	director, pilot, acct   *http.Cookie
	pilotUser, acctUser, op int64
}

const (
	srpAccountant = int64(90000004)
	srpLossOnOp   = int64(5001)
	srpLossLater  = int64(5002)
	srpKill       = int64(5003)
)

func newSRPFixture(t *testing.T) *srpFixture {
	t.Helper()
	mail := &mailSendTransport{mailStatus: http.StatusCreated, mailBody: `424242`}
	app, _, q := buildCorpTestApp(t, mail)
	ctx := context.Background()
	user, _ := q.CreateUser(ctx)
	grantScopes(t, q, user.ID, fixtureCharA, "Fixture Ceo", mailSendScope)
	ch, _ := q.GetCharacter(ctx, fixtureCharA)
	f := &srpFixture{notifyFixture: &notifyFixture{t: t, app: app, q: q, ctx: ctx, userID: user.ID, ch: ch}, mail: mail}
	f.joinCorp(fixtureCharA, discordCorp, "Director")
	f.director = sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	pilot, _ := q.CreateUser(ctx)
	seedCharacter(t, q, pilot.ID, fixtureCharB, "Fixture Mate")
	f.joinCorp(fixtureCharB, discordCorp)
	f.pilotUser, f.pilot = pilot.ID, sessionCookie(t, app, pilot.ID, fixtureCharB, "Fixture Mate")

	acct, _ := q.CreateUser(ctx)
	seedCharacter(t, q, acct.ID, srpAccountant, "Fixture Accountant")
	f.joinCorp(srpAccountant, discordCorp, "Accountant")
	f.acctUser, f.acct = acct.ID, sessionCookie(t, app, acct.ID, srpAccountant, "Fixture Accountant")

	start := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	op, err := q.CreateOp(ctx, db.CreateOpParams{
		CorporationID: discordCorp, Title: "Stratop", StartsAt: start, DurationMinutes: 60,
		FcCharacterID: fixtureCharA, CreatedByCharacter: fixtureCharA, CreatedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.op = op

	lost := func(id, victim int64, at time.Time) esi.Killmail {
		return esi.Killmail{KillmailID: id, KillmailTime: at.Format(time.RFC3339), SolarSystemID: 30000142,
			Victim: esi.KillmailVictim{CharacterID: victim, CorporationID: discordCorp, ShipTypeID: 587}}
	}
	var refs []esi.KillmailRef
	for _, km := range []esi.Killmail{
		lost(srpLossOnOp, fixtureCharB, start.Add(20*time.Minute)),
		lost(srpLossLater, fixtureCharB, start.Add(2*time.Hour)),
		lost(srpKill, 93300088, start.Add(25*time.Minute)),
	} {
		raw, _ := json.Marshal(km)
		if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
			KillmailID: km.KillmailID, CharacterID: fixtureCharB, Hash: "abc", Payload: string(raw), FetchedAt: start,
		}); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, esi.KillmailRef{KillmailID: km.KillmailID, KillmailHash: "abc"})
	}
	seedSnapshot(t, q, fixtureCharB, esi.SnapKillmails, refs)
	return f
}

func (f *srpFixture) claim(killmail int64) string {
	return strconv.FormatInt(killmail, 10) + ":" + strconv.FormatInt(f.op, 10)
}

// request returns the request for the loss on the op.
func (f *srpFixture) request() db.SrpRequest {
	f.t.Helper()
	rows, err := f.q.ListSRPRequestsByUser(f.ctx, db.ListSRPRequestsByUserParams{UserID: f.pilotUser, RowLimit: 10})
	if err != nil || len(rows) != 1 {
		f.t.Fatalf("requests %+v (%v), want the one", rows, err)
	}
	return rows[0]
}

func (f *srpFixture) decide(cookie *http.Cookie, form url.Values) {
	f.post(cookie, "/srp/"+strconv.FormatInt(f.request().ID, 10)+"/decide", form)
}

func (f *srpFixture) told(userID int64) string {
	rows, _ := f.q.ListNotifications(f.ctx, db.ListNotificationsParams{UserID: userID, Limit: 100})
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].Kind+": "+rows[i].Title)
	}
	return strings.Join(out, "\n")
}

// TestSRPRequest: a pilot can claim their own loss on an op it fell
// inside, once, and nothing else.
func TestSRPRequest(t *testing.T) {
	f := newSRPFixture(t)
	_, body := getPage(t, f.app, f.pilot, srpPath)
	mustContain(t, "srp page for the pilot", body, `value="`+f.claim(srpLossOnOp)+`"`, "fleet: Stratop", "1 other recent loss was not during an op")
	if strings.Contains(body, f.claim(srpKill)) || strings.Contains(body, "Mark paid") {
		t.Fatal("a kill is offered as a loss, or a pilot is offered the handler's controls")
	}

	f.post(f.acct, "/srp/request", url.Values{"loss": {f.claim(srpLossOnOp)}})
	f.post(f.pilot, "/srp/request", url.Values{"loss": {f.claim(srpLossLater)}})
	f.post(f.pilot, "/srp/request", url.Values{"loss": {f.claim(srpKill)}})
	if rows, _ := f.q.ListOpenSRPRequests(f.ctx, db.ListOpenSRPRequestsParams{OwnerKind: ownerCorporation, OwnerID: discordCorp, RowLimit: 100}); len(rows) != 0 {
		t.Fatalf("%d request(s) made from somebody else's loss, a loss outside the op, or a kill", len(rows))
	}

	// The loss is valued at our own prices: the hull's Forge
	// typical sell, never CCP's published average.
	seedOwnStat(t, f.q, 10000002, 587, 345678, 0, time.Now())
	f.post(f.pilot, "/srp/request", url.Values{"loss": {f.claim(srpLossOnOp)}, "note": {"primaried on landing"}})
	f.post(f.pilot, "/srp/request", url.Values{"loss": {f.claim(srpLossOnOp)}})
	req := f.request()
	if req.CharacterID != fixtureCharB || req.OpID != f.op || req.KillmailHash != "abc" || req.ShipTypeID != 587 || req.Status != srpOpen {
		t.Fatalf("request %+v", req)
	}
	if req.LossValue != 345678 {
		t.Fatalf("loss value %v, want the hull's own typical sell 345678", req.LossValue)
	}
	_, body = getPage(t, f.app, f.pilot, srpPath)
	mustContain(t, "srp page after asking", body, "Your requests", "OPEN", "Withdraw")
	if strings.Contains(body, f.claim(srpLossOnOp)) {
		t.Fatal("a claimed loss is still offered")
	}

	f.post(f.acct, "/srp/"+strconv.FormatInt(req.ID, 10)+"/withdraw", url.Values{})
	f.request()
	f.post(f.pilot, "/srp/"+strconv.FormatInt(req.ID, 10)+"/withdraw", url.Values{})
	if rows, _ := f.q.ListOpenSRPRequests(f.ctx, db.ListOpenSRPRequestsParams{OwnerKind: ownerCorporation, OwnerID: discordCorp, RowLimit: 100}); len(rows) != 0 {
		t.Fatal("the pilot could not withdraw their own request")
	}
}

// TestSRPHandling: directors handle requests, and whoever they hand it
// to; a handler is told of a new request and the pilot of the answer;
// the pay list and the paid note go to the game as EVE mail.
func TestSRPHandling(t *testing.T) {
	f := newSRPFixture(t)
	now := time.Now()
	f.pass(now)
	f.post(f.pilot, "/srp/request", url.Values{"loss": {f.claim(srpLossOnOp)}})
	f.pass(now.Add(time.Minute))
	f.wantTitles("after a request", "srp_new: SRP request: Fixture Mate lost a")
	if told := f.told(f.acctUser) + f.told(f.pilotUser); told != "" {
		t.Fatalf("somebody who does not handle requests was told of one: %q", told)
	}

	_, body := getPage(t, f.app, f.director, srpPath)
	mustContain(t, "srp page for a director", body, "Fixture Mate", "NOT ON ATTENDANCE", "Mail me the pay list", "corporation settings")
	if err := f.q.InsertManualAttendance(f.ctx, db.InsertManualAttendanceParams{OpID: f.op, CharacterID: fixtureCharB, SeenAt: now}); err != nil {
		t.Fatal(err)
	}
	_, body = getPage(t, f.app, f.director, srpPath)
	mustContain(t, "srp page once attendance is ticked", body, "ATTENDED", "Ticked present by hand.")

	// Not a handler: nothing an accountant or the pilot posts counts.
	f.decide(f.acct, url.Values{"action": {"paid"}, "payout": {"5m"}})
	f.decide(f.pilot, url.Values{"action": {"paid"}, "payout": {"5m"}})
	f.post(f.pilot, "/corporations/settings/save", url.Values{"corporation": {"98000001"}, "srp_who": {"eve_role:Accountant"}})
	f.post(f.acct, "/srp/paylist", url.Values{"corporation": {"98000001"}, "character": {"90000004"}})
	if f.request().Status != srpOpen || len(f.mail.sent()) != 0 {
		t.Fatal("somebody who does not handle requests answered one, or got the pay list")
	}
	if _, page := getPage(t, f.app, f.acct, srpPath); strings.Contains(page, "Mark paid") {
		t.Fatal("an accountant is offered the handler's controls before being handed them")
	}

	// The director hands it to accountants; one denies, with a reason.
	f.post(f.director, "/corporations/settings/save", url.Values{"corporation": {"98000001"}, "srp_who": {"member"}, "srp_policy": {"x"}})
	f.post(f.director, "/corporations/settings/save", url.Values{"corporation": {"98000001"}, "srp_who": {"eve_role:Accountant"}, "srp_policy": {"Doctrine ships only."}})
	_, body = getPage(t, f.app, f.pilot, srpPath)
	mustContain(t, "policy for a pilot", body, "Doctrine ships only.")
	f.decide(f.acct, url.Values{"action": {"deny"}})
	if f.request().Status != srpOpen {
		t.Fatal("a request was denied without a reason")
	}
	f.decide(f.acct, url.Values{"action": {"deny"}, "note": {"not a doctrine ship"}})
	if req := f.request(); req.Status != srpDenied || req.HandledBy != srpAccountant {
		t.Fatalf("after denying: %+v", req)
	}
	f.pass(now.Add(2 * time.Minute))
	if told := f.told(f.pilotUser); !strings.Contains(told, "srp_done: SRP denied") || !strings.Contains(told, "not a doctrine ship") {
		t.Fatalf("the pilot was told %q", told)
	}

	// Opened again, mailed as a pay list, then paid with a note to the pilot.
	f.decide(f.director, url.Values{"action": {"reopen"}})
	f.post(f.director, "/srp/paylist", url.Values{"corporation": {"98000001"}, "character": {"90000001"}})
	f.decide(f.director, url.Values{"action": {"paid"}, "payout": {"nonsense"}, "mail": {"1"}})
	if f.request().Status != srpOpen {
		t.Fatal("a request was marked paid without an amount")
	}
	f.decide(f.director, url.Values{"action": {"paid"}, "payout": {"12.5m"}, "mail": {"1"}})
	f.decide(f.acct, url.Values{"action": {"deny"}, "note": {"too late"}})
	if req := f.request(); req.Status != srpPaid || req.Payout != 12500000 || req.HandledBy != fixtureCharA {
		t.Fatalf("after paying: %+v", req)
	}
	sent := f.mail.sent()
	if len(sent) != 2 {
		t.Fatalf("%d EVE mail(s) sent, want the pay list and the paid note: %q", len(sent), sent)
	}
	for i, want := range [][]string{
		{`"recipient_id":90000001`, `showinfo:1377//90000002`, `killReport:5001:abc`, `"approved_cost":0`, "SRP pay list"},
		{`"recipient_id":90000002`, `killReport:5001:abc`, "12,500,000 ISK", "SRP paid"},
	} {
		// Go's JSON writes < and > as < and >; the links are checked on their insides.
		for _, part := range want {
			if !strings.Contains(sent[i], part) {
				t.Fatalf("mail %d lacks %q: %s", i, part, sent[i])
			}
		}
	}
	f.pass(now.Add(3 * time.Minute))
	if told := f.told(f.pilotUser); !strings.Contains(told, "srp_done: SRP paid") || !strings.Contains(told, "12,500,000 ISK") {
		t.Fatalf("the pilot was told %q", told)
	}
}

// TestSRPClaimNeedsTheFleetAndPaysALossOnce: on a community's fleet only
// a pilot on its attendance can claim, with or without an account, and
// only their own loss. One loss is claimed once across every owner, and
// only the fleet owner's handlers can answer it.
func TestSRPClaimNeedsTheFleetAndPaysALossOnce(t *testing.T) {
	f := newSRPFixture(t)
	const walkOn, walkOnLoss, community = int64(93300500), int64(5101), int64(7)
	start := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	id, err := f.q.CreateFleet(f.ctx, db.CreateFleetParams{
		OwnerKind: ownerCommunity, OwnerID: community, FleetType: fleetTypeCommunity, Visibility: fleetPublic, JoinGate: joinAnyone,
		Title: "Open roam", StartsAt: start, DurationMinutes: 60, DoctrineMode: doctrineNone,
		FcCharacterID: fixtureCharA, CreatedByCharacter: fixtureCharA, CreatedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.ExecContext(f.ctx, `INSERT INTO kills (killmail_id, hash, killmail_time, system_id, victim_character_id, ship_type_id, source, public, ingested_at, value_ours)
		VALUES ($1, 'walkon', $2, 30000142, $3, 587, 'stream', true, now(), 50000000)`, walkOnLoss, start.Add(20*time.Minute), walkOn); err != nil {
		t.Fatal(err)
	}
	if err := f.app.recordVerifiedPilot(f.ctx, walkOn, "Walk On", "owner-walk-on"); err != nil {
		t.Fatal(err)
	}
	session, err := f.app.sessions.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	f.app.sessions.Put(session, sessionPilotID, walkOn)
	f.app.sessions.Put(session, sessionPilotOwner, "owner-walk-on")
	token, _, err := f.app.sessions.Commit(session)
	if err != nil {
		t.Fatal(err)
	}
	pilot := &http.Cookie{Name: "evesynapse_session", Value: token, Path: "/"}
	path := opURL(id) + "srp"
	requests := func() []db.SrpRequest {
		t.Helper()
		rows, err := f.q.ListSRPRequestsOnOp(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	// Not on the fleet: the tab is closed and a claim stores nothing.
	if code, _ := getPage(t, f.app, pilot, path); code != http.StatusNotFound {
		t.Fatalf("tab for a pilot not on the fleet answered %d", code)
	}
	f.post(pilot, path+"/claim", url.Values{"killmail": {strconv.FormatInt(walkOnLoss, 10)}})
	if rows := requests(); len(rows) != 0 {
		t.Fatalf("a pilot not on the fleet claimed: %+v", rows)
	}
	if code, _ := getPage(t, f.app, &http.Cookie{Name: "none", Value: "none"}, path); code != http.StatusNotFound {
		t.Fatalf("tab for an anonymous viewer answered %d", code)
	}

	// A corporation fleet kept from a member is neither offered nor
	// claimable, though the loss is theirs and in its time.
	if err := f.q.InsertOpAccessRule(f.ctx, db.InsertOpAccessRuleParams{OpID: f.op, Action: accessView, RuleKind: "group", RuleRef: "99"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE ops SET visibility = 'restricted' WHERE id = $1`, f.op); err != nil {
		t.Fatal(err)
	}
	if _, body := getPage(t, f.app, f.pilot, srpPath); strings.Contains(body, f.claim(srpLossOnOp)) || strings.Contains(body, "fleet: Stratop") {
		t.Fatal("a fleet kept from a member is offered for a claim")
	}
	f.post(f.pilot, "/srp/request", url.Values{"loss": {f.claim(srpLossOnOp)}})
	if rows, _ := f.q.ListSRPRequestsByUser(f.ctx, db.ListSRPRequestsByUserParams{UserID: f.pilotUser, RowLimit: 10}); len(rows) != 0 {
		t.Fatalf("a member claimed on a fleet kept from them: %+v", rows)
	}
	if err := f.q.DeleteOpAccessRules(f.ctx, f.op); err != nil {
		t.Fatal(err)
	}

	// On the attendance, a pilot with no account claims their own loss
	// from the public killboard, and nobody else's.
	for _, character := range []int64{walkOn, fixtureCharB} {
		if err := f.q.InsertManualAttendance(f.ctx, db.InsertManualAttendanceParams{OpID: id, CharacterID: character, SeenAt: start}); err != nil {
			t.Fatal(err)
		}
	}
	// Seen in the fleet, but not when the ship died: no claim.
	sighted := func(from, to time.Time) {
		t.Helper()
		if _, err := f.app.db.ExecContext(f.ctx, `UPDATE op_attendance SET source = 'fleet', first_seen_at = $1, last_seen_at = $2 WHERE op_id = $3 AND character_id = $4`, from, to, id, walkOn); err != nil {
			t.Fatal(err)
		}
	}
	sighted(start.Add(45*time.Minute), start.Add(46*time.Minute))
	f.post(pilot, path+"/claim", url.Values{"killmail": {strconv.FormatInt(walkOnLoss, 10)}})
	if rows := requests(); len(rows) != 0 {
		t.Fatalf("a pilot claimed a loss from outside their time in the fleet: %+v", rows)
	}
	sighted(start.Add(10*time.Minute), start.Add(25*time.Minute))
	if code, body := getPage(t, f.app, pilot, path); code != http.StatusOK || !strings.Contains(body, `value="5101"`) {
		t.Fatalf("tab for a pilot on the fleet: %d, loss offered=%v", code, strings.Contains(body, `value="5101"`))
	}
	f.post(pilot, path+"/claim", url.Values{"killmail": {strconv.FormatInt(srpLossOnOp, 10)}})
	if rows := requests(); len(rows) != 0 {
		t.Fatalf("a pilot claimed someone else's loss: %+v", rows)
	}
	f.post(pilot, path+"/claim", url.Values{"killmail": {strconv.FormatInt(walkOnLoss, 10)}})
	rows := requests()
	if len(rows) != 1 || rows[0].CharacterID != walkOn || rows[0].UserID.Valid || rows[0].OwnerKind != ownerCommunity ||
		rows[0].OwnerID != community || rows[0].CorporationID != 0 || rows[0].LossValue != 50000000 {
		t.Fatalf("request %+v", rows)
	}

	// A handler of another owner cannot answer it.
	f.post(f.director, "/srp/"+strconv.FormatInt(rows[0].ID, 10)+"/decide", url.Values{"action": {"paid"}, "payout": {"5m"}})
	if req, err := f.q.GetSRPRequest(f.ctx, rows[0].ID); err != nil || req.Status != srpOpen {
		t.Fatalf("another owner's handler answered it: %+v, %v", req, err)
	}

	// Paid and opened again: the pilot cannot withdraw it to free the
	// loss for a second claim, and the payment stays on record.
	if n, err := f.q.DecideSRPRequest(f.ctx, db.DecideSRPRequestParams{
		ID: rows[0].ID, Status: srpPaid, Payout: 7000000, HandledBy: fixtureCharA, HandledAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("pay: %d, %v", n, err)
	}
	if n, err := f.q.ReopenSRPRequest(f.ctx, rows[0].ID); err != nil || n != 1 {
		t.Fatalf("reopen: %d, %v", n, err)
	}
	f.post(pilot, path+"/"+strconv.FormatInt(rows[0].ID, 10)+"/withdraw", url.Values{})
	if req, err := f.q.GetSRPRequest(f.ctx, rows[0].ID); err != nil || req.PreviousPayout != 7000000 || !req.AnsweredAt.Valid {
		t.Fatalf("after paying, reopening and withdrawing: %+v, %v", req, err)
	}

	// The same loss claimed on a second fleet creates nothing.
	f.post(f.pilot, path+"/claim", url.Values{"killmail": {strconv.FormatInt(srpLossOnOp, 10)}})
	f.post(f.pilot, "/srp/request", url.Values{"loss": {f.claim(srpLossOnOp)}})
	mine, err := f.q.ListSRPRequestsByUser(f.ctx, db.ListSRPRequestsByUserParams{UserID: f.pilotUser, RowLimit: 10})
	if err != nil || len(mine) != 1 || mine[0].OpID != id {
		t.Fatalf("one loss on two fleets: %+v, %v", mine, err)
	}
}
