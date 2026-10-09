package tests

import (
	"strings"
	"testing"

	"evesynapse/internal/apptest"
)

// TestInstallAppLink: the sidebar carries an "Install app" link that
// is hidden as served, so a browser that cannot install the site, or
// already has, never shows it; the stylesheet keeps it hidden against
// the link's own display rule; and the script shows it only on the
// browser's say-so and opens the browser's own dialog on a click.
func TestInstallAppLink(t *testing.T) {
	rig := apptest.Build(t, &apptest.CountingTransport{})
	q := rig.Queries()
	user, err := q.CreateUser(t.Context())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Fixture Alpha")
	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Fixture Alpha")

	code, body := apptest.GetPage(t, rig, cookie, "/characters/")
	if code != 200 {
		t.Fatalf("/characters/ status = %d", code)
	}
	apptest.MustContain(t, "/characters/", body,
		`<button type="button" id="install-app" class="install-app navlink" data-tip="Install app" hidden>`,
		`<span class="nav-label">Install app</span></button>`)
	// It sits with the theme and sign-out links, ahead of them.
	if i, j := strings.Index(body, `id="install-app"`), strings.Index(body, `id="theme-toggle"`); i < 0 || j < 0 || i > j {
		t.Errorf("the install link is not just ahead of the theme link (at %d and %d)", i, j)
	}

	code, css := apptest.GetPage(t, rig, cookie, "/static/style.css")
	if code != 200 {
		t.Fatalf("/static/style.css status = %d", code)
	}
	apptest.MustContain(t, "/static/style.css", css, "#install-app[hidden] { display: none; }")

	code, js := apptest.GetPage(t, rig, cookie, "/static/app.js")
	if code != 200 {
		t.Fatalf("/static/app.js status = %d", code)
	}
	apptest.MustContain(t, "/static/app.js", js,
		`document.getElementById("install-app")`,
		`window.addEventListener("beforeinstallprompt"`,
		`held.prompt();`,
		`window.addEventListener("appinstalled"`)
}
