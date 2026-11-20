package app

import (
	"context"
	"testing"
)

// TestSignInPlanAsksBeforeMovingOrCreating: a sign-in never moves a
// character off another account of the same player, and never
// creates an account for a signed-out character, without asking. A
// character that changed owners still joins the signed-in account
// at once, and a returning character still enters its own account.
func TestSignInPlanAsksBeforeMovingOrCreating(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	userA, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	linkFor(t, app, userA.ID, fixtureCharA, "hash-a")

	for _, c := range []struct {
		name      string
		session   int64
		char      int64
		hash      string
		step      string
		userID    int64
		outcome   string
		onAccount int64
	}{
		{"same player, other account", userB.ID, fixtureCharA, "hash-a", "move", userB.ID, "", userA.ID},
		{"new owner joins at once", userB.ID, fixtureCharA, "hash-new", "link", userB.ID, "added", userA.ID},
		{"already on this account", userA.ID, fixtureCharA, "hash-a", "link", userA.ID, "renewed", userA.ID},
		{"new character, signed in", userB.ID, fixtureCharB, "hash-b", "link", userB.ID, "added", 0},
		{"returning, signed out", 0, fixtureCharA, "hash-a", "link", userA.ID, "signin", userA.ID},
		{"new character, signed out", 0, fixtureCharB, "hash-b", "new", 0, "", 0},
		{"new owner, signed out", 0, fixtureCharA, "hash-new", "new", 0, "", userA.ID},
	} {
		got, err := app.planSignIn(ctx, c.session, c.char, c.hash)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := signInPlan{Step: c.step, UserID: c.userID, OnAccount: c.onAccount, Outcome: c.outcome}
		if got != want {
			t.Errorf("%s: plan %+v, want %+v", c.name, got, want)
		}
	}
}
