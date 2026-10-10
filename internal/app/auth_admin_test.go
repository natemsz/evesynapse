package app

import (
	"context"
	"errors"
	"net/http"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestAdminDoesNotFollowATransferredCharacter: an admin character that
// changes EVE owner does not make its new owner an administrator, on a
// new account or by skipping the sign-up lists, until the operator
// names it again on purpose.
func TestAdminDoesNotFollowATransferredCharacter(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	grantTestAdmin(app, fixtureCharA)
	signIn := func(sessionUser int64, owner string) int64 {
		t.Helper()
		userID, err := app.resolveSignInUser(ctx, sessionUser, fixtureCharA, owner)
		if err != nil {
			t.Fatalf("sign in as %s: %v", owner, err)
		}
		if _, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
			UserID: userID, CharacterID: fixtureCharA, Name: "Fixture Admin", OwnerHash: owner,
			AccessToken: "fixture", RefreshToken: "fixture", TokenExpiry: mustNullTime("2999-01-01T00:00:00Z"),
		}); err != nil {
			t.Fatalf("link as %s: %v", owner, err)
		}
		return userID
	}
	admin := func(userID int64) int {
		t.Helper()
		code, _ := getPage(t, app, sessionCookie(t, app, userID, fixtureCharA, "Fixture Admin"), "/admin/")
		return code
	}

	first := signIn(0, "owner-one")
	if code := admin(first); code != http.StatusOK {
		t.Fatalf("the admin character's first owner: /admin/ %d, want 200", code)
	}

	// With sign-up restricted, the recorded owner still gets in by
	// being an administrator; a new owner of the same character does not.
	app.cfg.signUp.characterIDs = map[int64]bool{fixtureCharB: true}
	if err := app.mayCreateAccount(ctx, fixtureCharA, "owner-one"); err != nil {
		t.Fatalf("the recorded owner was refused sign-up: %v", err)
	}
	if err := app.mayCreateAccount(ctx, fixtureCharA, "owner-two"); !errors.Is(err, errSignUpNotAllowed) {
		t.Fatalf("a new owner of the admin character skipped the sign-up lists: %v", err)
	}
	app.cfg.signUp = signUpPolicy{}

	// The character is transferred and its new owner signs in.
	second := signIn(0, "owner-two")
	if second == first {
		t.Fatal("the new owner landed on the previous owner's account")
	}
	if code := admin(second); code != http.StatusForbidden {
		t.Fatalf("the new owner of a transferred admin character: /admin/ %d, want 403", code)
	}
	if ch, _ := q.GetCharacter(ctx, fixtureCharA); ch.UserID != second || app.adminAmong([]db.Character{ch}) {
		t.Fatal("the transferred character still counts as an administrator")
	}

	// Named again on purpose (taken out of the setting, restarted, put
	// back): the status goes to whoever owns it then.
	delete(app.cfg.adminCharIDs, fixtureCharA)
	app.admins.load(ctx, app)
	grantTestAdmin(app, fixtureCharA)
	if code := admin(second); code != http.StatusOK {
		t.Fatalf("after being named again: /admin/ %d, want 200", code)
	}
}
