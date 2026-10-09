package tests

import (
	"testing"

	"evesynapse/internal/apptest"
)

// TestSwitcherMotionAndAdminShortcuts pins two things in the served
// assets. The top bar's character switcher opens and closes with the
// navigation's motion and closes on a click elsewhere, like the
// notification list. And quick jump offers Sync and Admin only where
// the menu has them, which is for an administrator.
func TestSwitcherMotionAndAdminShortcuts(t *testing.T) {
	rig := apptest.Build(t, &apptest.CountingTransport{})
	q := rig.Queries()
	user, err := q.CreateUser(t.Context())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Fixture Alpha")
	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Fixture Alpha")

	code, css := apptest.GetPage(t, rig, cookie, "/static/style.css")
	if code != 200 {
		t.Fatalf("/static/style.css status = %d", code)
	}
	apptest.MustContain(t, "/static/style.css", css,
		"html.js .topbar-switcher .menu {\n  opacity: 0;\n  transform: translateY(-0.25rem);\n  transition: opacity 140ms ease-out, transform 180ms ease-out;\n}",
		"html.js .topbar-switcher.is-open .menu {\n  opacity: 1;\n  transform: none;\n}",
		"html.js .topbar-switcher .menu { transition: none; }")

	code, js := apptest.GetPage(t, rig, cookie, "/static/app.js")
	if code != 200 {
		t.Fatalf("/static/app.js status = %d", code)
	}
	apptest.MustContain(t, "/static/app.js", js,
		`document.querySelector("details.topbar-switcher")`,
		`if (!switcher.contains(ev.target)) close();`,
		// Escape leaves the switcher to close itself, with its motion.
		`details.branch[open]:not(.topbar-switcher)`,
		`var adminOnlyPages = { "/sync/": true, "/admin/": true };`,
		`var isAdmin = !!document.querySelector(".sidenav-utility");`,
		`if (adminOnlyPages[quickJumpPages[i].url] && !isAdmin) continue;`)
}
