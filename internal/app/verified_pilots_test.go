package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/standing"
)

// A verified pilot has no characters row and manages nothing, even with
// an admin place stored against it, and a sign-in by a new owner ends
// the places the character held.
func TestVerifiedPilotCannotManageAnything(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	const pilot = int64(90000501)

	if err := app.recordVerifiedPilot(ctx, pilot, "Walk On", "owner-a"); err != nil {
		t.Fatalf("record pilot: %v", err)
	}
	if _, err := q.GetCharacter(ctx, pilot); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a verified pilot has a characters row (err = %v)", err)
	}

	now := time.Now().UTC()
	community, err := q.CreateCommunity(ctx, db.CreateCommunityParams{
		Slug: "walk-ons", Name: "Walk Ons", JoinPolicy: "open", CreatedByCharacter: pilot, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create community: %v", err)
	}
	if _, err := q.AddCommunityMember(ctx, db.AddCommunityMemberParams{
		CommunityID: community.ID, CharacterID: pilot, Name: "Walk On",
		Role: standing.CommunityAdmin, State: "active", JoinedAt: now, AddedBy: pilot,
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	owner := standing.Owner{Kind: standing.OwnerCommunity, ID: community.ID}
	st, err := app.standingFor(ctx, 0)
	if err != nil {
		t.Fatalf("standing: %v", err)
	}
	for _, permission := range []string{permSRP, permDoctrines, permFleets} {
		if app.ownerPermits(ctx, st, owner, permission) {
			t.Errorf("a verified pilot runs the community's %s", permission)
		}
	}
	if owners := app.GroupOwners(ctx, 0); len(owners) != 0 {
		t.Errorf("a verified pilot keeps groups for %v", owners)
	}

	if err := app.recordVerifiedPilot(ctx, pilot, "Walk On", "owner-b"); err != nil {
		t.Fatalf("record new owner: %v", err)
	}
	if _, err := q.GetCommunityMember(ctx, db.GetCommunityMemberParams{CommunityID: community.ID, CharacterID: pilot}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a sold character kept its community place (err = %v)", err)
	}

	// The previous owner removing a stale link leaves the new owner's
	// place alone.
	const sold = int64(90000502)
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
		UserID: user.ID, CharacterID: sold, Name: "Sold", OwnerHash: "owner-a",
		AccessToken: "fixture", RefreshToken: "fixture", TokenExpiry: mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := app.recordVerifiedPilot(ctx, sold, "Sold", "owner-b"); err != nil {
		t.Fatalf("record new owner: %v", err)
	}
	if _, err := q.AddCommunityMember(ctx, db.AddCommunityMemberParams{
		CommunityID: community.ID, CharacterID: sold, Name: "Sold",
		Role: standing.CommunityMember, State: "active", JoinedAt: now, AddedBy: sold,
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := q.DeleteCharacter(ctx, db.DeleteCharacterParams{CharacterID: sold, UserID: user.ID}); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := q.GetCommunityMember(ctx, db.GetCommunityMemberParams{CommunityID: community.ID, CharacterID: sold}); err != nil {
		t.Errorf("a stale link's removal ended the new owner's place: %v", err)
	}
}
