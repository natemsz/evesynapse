package app

import (
	"context"
	"database/sql"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestSnapshotMetaMatchesTheFullRows: the payload-free listing
// reports the same snapshots, in the same order, with the same
// freshness, as reading the full rows does.
func TestSnapshotMetaMatchesTheFullRows(t *testing.T) {
	_, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Beta")

	// Fresh, stale, and one with no expiry recorded at all.
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, `{"skills":[]}`)
	for kind, until := range map[string]sql.NullTime{
		esi.SnapWallet: mustNullTime("2000-01-01T00:00:00Z"),
		esi.SnapAssets: {},
	} {
		if err := q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
			CharacterID: fixtureCharA,
			Kind:        kind,
			Payload:     `[]`,
			FetchedAt:   mustTime("2026-01-01T00:00:00Z"),
			CachedUntil: until,
		}); err != nil {
			t.Fatalf("seed %s: %v", kind, err)
		}
	}
	// Another character's snapshot must not leak into the listing.
	seedSnapshot(t, q, fixtureCharB, esi.SnapLocation, `{}`)

	full, err := q.ListSnapshotsByCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("list full rows: %v", err)
	}
	meta, err := q.ListSnapshotMetaByCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("list meta: %v", err)
	}
	if len(meta) != len(full) || len(meta) != 3 {
		t.Fatalf("meta lists %d snapshots, full rows %d; want 3 each", len(meta), len(full))
	}
	fresh := 0
	for i := range full {
		if meta[i].Kind != full[i].Kind || meta[i].FetchedAt != full[i].FetchedAt || meta[i].CachedUntil != full[i].CachedUntil {
			t.Errorf("row %d: meta %+v does not match the full row (%s, %s, %+v)", i, meta[i], full[i].Kind, full[i].FetchedAt, full[i].CachedUntil)
		}
		if esi.CacheWindowOpen(meta[i].CachedUntil) != esi.SnapshotFresh(full[i]) {
			t.Errorf("%s: meta freshness %v, full-row freshness %v", full[i].Kind, esi.CacheWindowOpen(meta[i].CachedUntil), esi.SnapshotFresh(full[i]))
		}
		if esi.CacheWindowOpen(meta[i].CachedUntil) {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d snapshots read as fresh, want only the one with a future expiry", fresh)
	}
}
