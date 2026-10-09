package app

import (
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// cycle runs the notification pass the way the worker does after a
// cycle, and returns how many notifications it created.
func (f *notifyFixture) cycle(now time.Time) int {
	f.t.Helper()
	chars, err := f.q.ListAllCharacters(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.app.notifyCycle(f.ctx, chars, now)
}

// TestNotifyCycleReadsOnlyWhatChanged: after a worker cycle an
// account's stored data is read when new data was stored for one of
// its characters, or when it has gone unread for the recheck time;
// news that comes with the clock is at most that late. Ops are looked
// at every time regardless.
func TestNotifyCycleReadsOnlyWhatChanged(t *testing.T) {
	f := newNotifyFixture(t)
	now := notifyT0
	const corp, director = int64(98000001), int64(95000001)
	if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
		CharacterID: f.ch.CharacterID, CorporationID: corp, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	mail := func(ids ...int64) {
		var headers esi.MailHeaders
		for _, id := range ids {
			headers = append(headers, esi.MailHeader{MailID: id, From: 555, Subject: "Fixture", Timestamp: rfc(now)})
		}
		f.seed(esi.SnapMail, headers)
	}
	mail(1)
	f.seed(esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, FinishedLevel: 4, FinishDate: rfc(now.Add(3*time.Minute + 30*time.Second))},
	})

	// The first time an account is met its data is read: the baseline.
	if n := f.cycle(now); n != 0 {
		t.Fatalf("the first cycle announced %d: %q", n, f.titles())
	}
	// From here its place in the recheck round is fixed by the test,
	// not by its id.
	f.app.notifyQuiet.read[f.userID] = now

	// A mail lands in the database with nothing saying data changed
	// (which cannot happen through the ESI client): it is not read.
	mail(1, 2)
	if n := f.cycle(now.Add(time.Minute)); n != 0 {
		t.Fatalf("an account with no new data was read anyway: %q", f.titles())
	}
	// A new op is somebody else's doing: announced in that same quiet
	// minute.
	if _, err := f.q.CreateOp(f.ctx, db.CreateOpParams{
		CorporationID: corp, Title: "Structure bash", StartsAt: now.Add(48 * time.Hour), DurationMinutes: 60,
		FcCharacterID: director, CreatedByCharacter: director, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.cycle(now.Add(90 * time.Second)); n != 1 {
		t.Fatalf("a new op in a quiet minute: %d announced, want 1: %q", n, f.titles())
	}
	f.wantTitles("quiet minute", "op: ")

	// New data for the character: read at once.
	f.app.esi.NoteCharacterChanged(f.ch.CharacterID)
	if n := f.cycle(now.Add(2 * time.Minute)); n != 1 {
		t.Fatalf("after new data: %d announced, want the mail: %q", n, f.titles())
	}
	f.wantTitles("new data", "op: ", "mail: ")

	// The skill finishes with no new data: not noticed until the
	// account has gone unread for the recheck time, and then it is.
	if n := f.cycle(now.Add(4 * time.Minute)); n != 0 {
		t.Fatalf("read again before the recheck time with no new data: %q", f.titles())
	}
	if n := f.cycle(now.Add(2*time.Minute + notifyQuietRecheck)); n != 1 {
		t.Fatalf("at the recheck time: %d announced, want the skill: %q", n, f.titles())
	}
	f.wantTitles("recheck", "op: ", "mail: ", "skill: ")
}

// TestNotifyQuietLog: the rule on its own, and that accounts met in
// the same cycle do not all come round for their recheck together.
func TestNotifyQuietLog(t *testing.T) {
	var l notifyQuietLog
	now := notifyT0
	for id := int64(1); id <= 10; id++ {
		if !l.due(id, false, now) {
			t.Fatalf("account %d was not read the first time it was met", id)
		}
		if l.due(id, false, now.Add(30*time.Second)) {
			t.Fatalf("account %d was read again half a minute later with no new data", id)
		}
	}
	if !l.due(1, true, now.Add(time.Minute)) {
		t.Fatal("new data did not have the account read")
	}
	// Minute by minute, the other nine come round in more than one
	// cycle, and all within the recheck time.
	rounds := map[time.Duration]int{}
	for id := int64(2); id <= 10; id++ {
		for m := time.Minute; m <= notifyQuietRecheck; m += time.Minute {
			if l.due(id, false, now.Add(m)) {
				rounds[m]++
				break
			}
			if m == notifyQuietRecheck {
				t.Fatalf("account %d was not read within the recheck time", id)
			}
		}
	}
	if len(rounds) < 3 {
		t.Fatalf("nine accounts came round in %d cycle(s): %v", len(rounds), rounds)
	}

	l.forget(1)
	if !l.due(1, false, now.Add(61*time.Second)) {
		t.Fatal("an account whose pass failed was not read again")
	}
}
