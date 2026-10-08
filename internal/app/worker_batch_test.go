package app

import (
	"context"
	"testing"

	db "evesynapse/internal/db/sqlc"
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
