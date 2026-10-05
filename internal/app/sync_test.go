package app

// Focused tests for the Sync page's name-coverage figure: the
// needed-ID set comes from the user's own snapshots, and the
// name lookups stay batched over those IDs (never the whole
// type_names / sde_types tables).

import (
	"context"
	"net/http"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func TestSyncNameCoverageFromBatchedLookups(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// Skills reference type IDs 100, 101; assets reference
	// 102, 103.
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{Skills: []esi.Skill{
		{SkillID: 100}, {SkillID: 101},
	}})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 1, TypeID: 102}, {ItemID: 2, TypeID: 103},
	})
	// Only 100 (name cache) and 102 (SDE) resolve: 2 of 4.
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 100, Name: "Skill One"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES (102, 'Asset One', 18)`); err != nil {
		t.Fatalf("seed sde type: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("sync page: status %d", code)
	}
	mustContain(t, "/sync/", body, "Names resolved 2 of 4 (50%)")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("sync page made %d outbound calls, want 0", got)
	}
}
