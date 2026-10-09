package app

import (
	"context"
	"database/sql"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestOrderByDuePrefersUnfetched: a character with no snapshots is
// most overdue, ahead of one whose core snapshots are fresh — and
// the ordering holds with the batched freshness read.
func TestOrderByDuePrefersUnfetched(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	fresh := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Fresh")
	for _, kind := range coreSnapshotKinds {
		seedSnapshot(t, q, fixtureCharA, kind, `{}`)
	}
	cold := seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Cold")

	// No snapshots at all is most overdue (key zero).
	got := app.orderByDue(ctx, []db.Character{fresh, cold})
	if len(got) != 2 || got[0].CharacterID != cold.CharacterID || got[1].CharacterID != fresh.CharacterID {
		t.Fatalf("orderByDue = [%d %d], want [%d %d]",
			got[0].CharacterID, got[1].CharacterID, cold.CharacterID, fresh.CharacterID)
	}
	// Full core coverage on both: equal keys, so the input order
	// stands (stable sort).
	for _, kind := range coreSnapshotKinds {
		seedSnapshot(t, q, fixtureCharB, kind, `{}`)
	}
	got = app.orderByDue(ctx, []db.Character{fresh, cold})
	if len(got) != 2 || got[0].CharacterID != fresh.CharacterID {
		t.Fatalf("all-fresh orderByDue starts with %d, want input order kept", got[0].CharacterID)
	}
	// One expired window on the cold character pulls it overdue
	// again, ahead of the fresh one.
	if _, err := conn.ExecContext(ctx, `UPDATE character_snapshots SET cached_until = '2020-01-01T00:00:00Z' WHERE character_id = $1 AND kind = $2`, fixtureCharB, coreSnapshotKinds[0]); err != nil {
		t.Fatalf("expire a snapshot: %v", err)
	}
	got = app.orderByDue(ctx, []db.Character{fresh, cold})
	if len(got) != 2 || got[0].CharacterID != cold.CharacterID {
		t.Fatalf("expired orderByDue starts with %d, want %d first", got[0].CharacterID, cold.CharacterID)
	}
}

// TestRouteJournalParty: counterparties sort by party_type —
// characters to the name harvest, orgs to the flush sets — and an
// characters to the name harvest, orgs to the flush sets — and an
// untyped party is left alone.
func TestRouteJournalParty(t *testing.T) {
	chars, corps, alliances := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	routeJournalParty(chars, corps, alliances, 90000001, "character")
	routeJournalParty(chars, corps, alliances, 2000001, "corporation")
	routeJournalParty(chars, corps, alliances, 3000001, "alliance")
	routeJournalParty(chars, corps, alliances, 4000001, "")
	routeJournalParty(chars, corps, alliances, 0, "character")
	routeJournalParty(chars, corps, alliances, -5, "corporation")
	if !chars[90000001] || len(chars) != 1 {
		t.Fatalf("characters = %v", chars)
	}
	if !corps[2000001] || len(corps) != 1 {
		t.Fatalf("corporations = %v", corps)
	}
	if !alliances[3000001] || len(alliances) != 1 {
		t.Fatalf("alliances = %v", alliances)
	}
}

// TestFlushOrgWants: one flush notes every collected ID, dedupes
// repeats, and leaves existing rows' richer state alone (the
// priority floor, not a reset).
func TestFlushOrgWants(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	app.flushOrgWants(ctx, map[int64]bool{2000001: true, 2000002: true}, map[int64]bool{3000001: true})
	for _, id := range []int64{2000001, 2000002} {
		rec, err := q.GetCorporationRecord(ctx, id)
		if err != nil {
			t.Fatalf("corporation %d not noted: %v", id, err)
		}
		if rec.Priority != 1 {
			t.Fatalf("corporation %d priority = %d, want 1", id, rec.Priority)
		}
	}
	if _, err := q.GetAllianceRecord(ctx, 3000001); err != nil {
		t.Fatalf("alliance 3000001 not noted: %v", err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("flush made %d ESI calls, want 0 (queue writes only)", got)
	}
}

// TestDueOrderIgnoresWhatIsNeverRefetched: a character's place in the
// queue, and the worker's lateness figure, come from the datasets the
// worker actually refreshes. A week-old mail body, fetched once and
// never again, does not make its character the most overdue; and a
// dataset the character has not granted access to is not something it
// can be late with.
func TestDueOrderIgnoresWhatIsNeverRefetched(t *testing.T) {
	at := func(s string) sql.NullTime { return mustNullTime(s) }
	var all []db.ListSnapshotMetaForCharactersRow
	for _, kind := range coreSnapshotKinds {
		all = append(all, db.ListSnapshotMetaForCharactersRow{Kind: kind, CachedUntil: at("2026-10-09T05:00:00Z")})
	}
	want := mustNullTime("2026-10-09T05:00:00Z").Time

	// One-off snapshots from days ago change nothing.
	withOld := append(append([]db.ListSnapshotMetaForCharactersRow(nil), all...),
		db.ListSnapshotMetaForCharactersRow{Kind: esi.MailBodyKind(12), CachedUntil: at("2026-10-03T09:00:00Z")},
		db.ListSnapshotMetaForCharactersRow{Kind: esi.CalendarEventKind(7), CachedUntil: at("2026-10-01T00:00:00Z")})
	if got := dueKeyFromMeta(withOld, nil); !got.Equal(want) {
		t.Fatalf("with old one-off snapshots: due %v, want %v", got, want)
	}
	// A core dataset that is overdue does.
	late := append([]db.ListSnapshotMetaForCharactersRow(nil), all...)
	late[2].CachedUntil = at("2026-10-09T04:00:00Z")
	if got := dueKeyFromMeta(late, nil); !got.Equal(mustNullTime("2026-10-09T04:00:00Z").Time) {
		t.Fatalf("with one core dataset overdue: due %v", got)
	}
	// A core dataset never fetched is most due...
	missing := all[1:]
	if got := dueKeyFromMeta(missing, nil); !got.IsZero() {
		t.Fatalf("with a core dataset never fetched: due %v, want the zero time", got)
	}
	// ...unless the character has not granted access to it, when it
	// never will be fetched and is not counted.
	granted := scopeSet("esi-skills.read_skills.v1")
	locked := false
	var kept []db.ListSnapshotMetaForCharactersRow
	for _, row := range all {
		if kindLockedOut(granted, row.Kind) {
			locked = true
			continue
		}
		kept = append(kept, row)
	}
	if !locked {
		t.Fatal("the test needs a core dataset that one scope alone does not unlock")
	}
	if got := dueKeyFromMeta(kept, granted); !got.Equal(want) {
		t.Fatalf("with only the granted datasets stored: due %v, want %v", got, want)
	}
}
