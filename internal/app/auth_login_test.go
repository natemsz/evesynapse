package app

// The plain login (identity-only SSO) resolves to the character's
// existing account and nothing else: it creates no account, stores
// no token, and never changes the scopes sign-up chose. Anything
// that is not a plain return is handed to the flow that owns the
// case. These tests pin those decisions.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func TestIdentityLoginDecisions(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const charID = int64(90000077)
	const scopes = "esi-skills.read_skills.v1 esi-wallet.read_character_wallet.v1"
	if _, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
		UserID: user.ID, CharacterID: charID, Name: "Returning Pilot", Scopes: scopes, OwnerHash: "owner-a",
		AccessToken: "fixture", RefreshToken: "fixture", TokenExpiry: mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("link character: %v", err)
	}
	userCount := func() int {
		users, err := q.ListUsers(ctx)
		if err != nil {
			t.Fatalf("list users: %v", err)
		}
		return len(users)
	}

	// A returning pilot lands on their account, and the login
	// leaves the link exactly as sign-up left it.
	gotUser, target := app.identityLogin(ctx, charID, "owner-a")
	if target != "" || gotUser != user.ID {
		t.Fatalf("known character: got (%d, %q), want (%d, \"\")", gotUser, target, user.ID)
	}
	ch, err := q.GetCharacter(ctx, charID)
	if err != nil {
		t.Fatalf("get character: %v", err)
	}
	if ch.Scopes != scopes {
		t.Fatalf("identity login changed scopes: got %q, want %q", ch.Scopes, scopes)
	}

	// An unknown character is sent to sign up, and no account is
	// created for it here.
	if _, target := app.identityLogin(ctx, 90000078, "owner-b"); target != "/auth/link" {
		t.Fatalf("unknown character: target = %q, want /auth/link", target)
	}
	if n := userCount(); n != 1 {
		t.Fatalf("identity login created accounts: %d users, want 1", n)
	}

	// A character that changed EVE accounts belongs to its new
	// owner, who must sign up in their own right.
	if _, target := app.identityLogin(ctx, charID, "owner-b"); target != "/auth/link" {
		t.Fatalf("owner changed: target = %q, want /auth/link", target)
	}

	// A dead stored token cannot be revived by identity alone:
	// the full re-link asks EVE for the grants again.
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState: linkStateTokenDead, LinkStateAt: timeSet(time.Now().UTC()), CharacterID: charID,
	}); err != nil {
		t.Fatalf("set link state: %v", err)
	}
	if _, target := app.identityLogin(ctx, charID, "owner-a"); target != "/auth/eve" {
		t.Fatalf("token dead: target = %q, want /auth/eve", target)
	}
}

func TestPlainLoginAsksForIdentityOnly(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	app.cfg.eveClientID = "fixture-client"
	app.cfg.eveClientSecret = "fixture-secret"
	app.cfg.eveCallbackURL = "https://eve.example.org/auth/callback"

	rec := get(app, nil, "/auth/login")
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login: status %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://login.eveonline.com/v2/oauth/authorize") {
		t.Fatalf("login redirect goes to %q, want CCP authorize", loc)
	}
	if strings.Contains(loc, "scope=") {
		t.Fatalf("plain login must ask for no scopes, redirect: %q", loc)
	}
	if !strings.Contains(loc, "state=") {
		t.Fatalf("login redirect carries no state: %q", loc)
	}
}
