package app

import (
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// The SRP notification source counts an account's corporations
// through its working characters only: a character whose link has
// been revoked must not keep the account's SRP notifications alive.

func seedSRPCorp(t *testing.T, f *notifyFixture) {
	t.Helper()
	if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
		CharacterID: f.ch.CharacterID, CorporationID: fixtureCorpA, UpdatedAt: notifyT0,
	}); err != nil {
		t.Fatalf("seed character corporation: %v", err)
	}
}

func seedPaidSRPRequest(t *testing.T, f *notifyFixture, decidedAt time.Time) {
	t.Helper()
	opID, err := f.q.CreateOp(f.ctx, db.CreateOpParams{
		CorporationID: fixtureCorpA, Title: "Fixture op",
		StartsAt: decidedAt.Add(-2 * time.Hour), DurationMinutes: 120,
		FcCharacterID: f.ch.CharacterID, CreatedByCharacter: f.ch.CharacterID,
		CreatedAt: decidedAt.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create op: %v", err)
	}
	id, err := f.q.CreateSRPRequest(f.ctx, db.CreateSRPRequestParams{
		CorporationID: fixtureCorpA, OpID: opID, KillmailID: 777001, KillmailHash: "fixturehash",
		CharacterID: f.ch.CharacterID, UserID: f.userID, ShipTypeID: 34,
		SolarSystemID: 30000142, LostAt: decidedAt.Add(-time.Hour), LossValue: 12000000,
		CreatedAt: decidedAt.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create srp request: %v", err)
	}
	if _, err := f.q.DecideSRPRequest(f.ctx, db.DecideSRPRequestParams{
		Status: "paid", Payout: 10000000, HandledBy: 987654,
		HandledAt: timeSet(decidedAt), ID: id,
	}); err != nil {
		t.Fatalf("decide srp request: %v", err)
	}
}

// TestNotifySRPPaidAnnounced: a paid request is announced to the
// pilot who lost the ship once it is decided.
func TestNotifySRPPaidAnnounced(t *testing.T) {
	f := newNotifyFixture(t)
	seedSRPCorp(t, f)
	if n := f.pass(notifyT0); n != 0 {
		t.Fatalf("baseline pass created %d notification(s) %q", n, f.titles())
	}
	seedPaidSRPRequest(t, f, notifyT0.Add(30*time.Minute))
	if n := f.pass(notifyT0.Add(time.Hour)); n != 1 {
		t.Fatalf("after the request was paid: %d notification(s) %q, want 1", n, f.titles())
	}
	f.wantTitles("paid", "srp_done: SRP paid")
}

// TestNotifySRPRevokedLinkStaysQuiet: the account's only character
// has a revoked link, so the SRP source never opens for it and a
// decision on its request is not announced.
func TestNotifySRPRevokedLinkStaysQuiet(t *testing.T) {
	f := newNotifyFixture(t)
	seedSRPCorp(t, f)
	if err := f.q.SetCharacterLinkState(f.ctx, db.SetCharacterLinkStateParams{
		LinkState: linkStateTokenDead, LinkStateAt: timeSet(notifyT0), CharacterID: f.ch.CharacterID,
	}); err != nil {
		t.Fatalf("revoke link: %v", err)
	}
	if n := f.pass(notifyT0); n != 0 {
		t.Fatalf("baseline pass created %d notification(s) %q", n, f.titles())
	}
	seedPaidSRPRequest(t, f, notifyT0.Add(30*time.Minute))
	if n := f.pass(notifyT0.Add(time.Hour)); n != 0 {
		t.Fatalf("a revoked link's SRP decision was announced: %d notification(s) %q", n, f.titles())
	}
}
