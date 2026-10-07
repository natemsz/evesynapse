package app

// Tests for who counts as an administrator: the account, through
// any of its characters listed in EVE_ADMIN_CHARACTER_IDS — not
// only while that character is the one selected in the header.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

func TestAdminBelongsToTheAccount(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	grantTestAdmin(app, fixtureCharA)
	ctx := context.Background()

	admin, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create admin account: %v", err)
	}
	seedCharacter(t, q, admin.ID, fixtureCharA, "Admin Main")
	seedCharacter(t, q, admin.ID, fixtureCharB, "Admin Alt")
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	seedCharacter(t, q, other.ID, fixtureMember, "Plain Member")

	// Acting as the alt, the account is still an admin's: the pages
	// open and the nav offers them.
	asAlt := sessionCookie(t, app, admin.ID, fixtureCharB, "Admin Alt")
	for _, path := range []string{"/admin/", "/sync/"} {
		if code, _ := getPage(t, app, asAlt, path); code != http.StatusOK {
			t.Errorf("GET %s acting as the admin's alt: status %d, want 200", path, code)
		}
	}
	_, body := getPage(t, app, asAlt, "/characters/")
	mustContain(t, "/characters/ (admin's alt)", body, `href="/admin/"`, `href="/sync/"`)

	// Another account gets neither the pages nor the links.
	asOther := sessionCookie(t, app, other.ID, fixtureMember, "Plain Member")
	for _, path := range []string{"/admin/", "/sync/"} {
		if code, _ := getPage(t, app, asOther, path); code != http.StatusForbidden {
			t.Errorf("GET %s from a non-admin account: status %d, want 403", path, code)
		}
	}
	_, body = getPage(t, app, asOther, "/characters/")
	if strings.Contains(body, `href="/admin/"`) || strings.Contains(body, `href="/sync/"`) {
		t.Error("a non-admin account is shown the Admin/Sync nav links")
	}

	// An admin character that changed EVE accounts, and has not been
	// re-verified, no longer makes its account an admin's.
	if err := q.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateOwnerChanged,
		LinkStateAt: mustNullTime("2026-10-01T00:00:00Z"),
		CharacterID: fixtureCharA,
	}); err != nil {
		t.Fatalf("flag owner change: %v", err)
	}
	if code, _ := getPage(t, app, asAlt, "/admin/"); code != http.StatusForbidden {
		t.Errorf("GET /admin/ with the admin character flagged owner_changed: status %d, want 403", code)
	}
}
