package app

// Tests for the persistent-shell navigation (static/swap.js): every
// page still renders the full document on the no-JS path (the swap
// only ever replaces #main-content in a browser that opts in), the
// swap script itself is served versioned like the other assets, and
// the pages swap.js leaves alone (simulator, saved fits) keep the
// page scripts they need.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func getSwapPage(t *testing.T, app *Application, cookie *http.Cookie, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestSwapScriptServed(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	version := staticAssetVersion()

	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/swap.js?v="+version, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("versioned swap script: status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != cacheForever {
		t.Errorf("versioned swap script Cache-Control = %q, want %q", got, cacheForever)
	}
	// The script's behavior contract lives in the JS tests
	// (internal/app/jstest); asserting on its source text here
	// would only make renames fail twice.
	if rec.Body.Len() == 0 {
		t.Errorf("versioned swap script served an empty body")
	}

	// Unversioned it revalidates with an ETag like the rest.
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/swap.js", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" {
		t.Errorf("unversioned swap script: status %d ETag %q, want 200 with an ETag",
			rec.Code, rec.Header().Get("ETag"))
	}
}

func TestPagesRenderFullShellNoJS(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	version := staticAssetVersion()

	shell := []string{
		`<a class="skip-link" href="#main-content">`,
		`<main id="main-content">`,
		`src="/static/app.js?v=` + version + `"`,
		`src="/static/swap.js?v=` + version + `"`,
		`</html>`,
	}
	// Authed pages and the anonymous public pages alike render the
	// whole shell: without JavaScript the browser shows exactly
	// this, and the swap only replaces the main region.
	for _, path := range []string{"/", "/characters/", "/market/", "/fittings/public/"} {
		code, body := getSwapPage(t, app, cookie, path)
		if code != http.StatusOK {
			t.Errorf("GET %s (authed): status %d, want 200", path, code)
			continue
		}
		mustContain(t, path+" (authed)", body, shell...)
	}
	for _, path := range []string{"/fittings/public/"} {
		code, body := getSwapPage(t, app, nil, path)
		if code != http.StatusOK {
			t.Errorf("GET %s (anonymous): status %d, want 200", path, code)
			continue
		}
		mustContain(t, path+" (anonymous)", body, shell...)
	}
	// The anonymous front door is the landing page: a standalone
	// document with its own slim header, deliberately outside the
	// app shell (signed-in members skip it and get the shell).
	code, body := getSwapPage(t, app, nil, "/")
	if code != http.StatusOK {
		t.Errorf("GET / (anonymous): status %d, want 200", code)
	} else {
		mustContain(t, "/ (anonymous landing)", body,
			`<header class="ld-header">`, "Your keys, your call.", `</html>`)
	}
}

var stylesheetHrefRE = regexp.MustCompile(`href="(/static/[^"]+\.css\?v=[^"]+)"`)

func TestFullLoadsCarryEachStylesheetOnce(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	version := staticAssetVersion()

	// The full document is the swap's contract for page assets:
	// the swap adopts exactly the stylesheet links it carries, so
	// each one must appear exactly once — and each page must
	// carry its own sheet.
	pages := map[string]string{
		"/intel/kills/":      "/static/killboard.css",
		"/map/":              "/static/map.css",
		"/contracts/search/": "/static/contracts.css",
		"/planner/":          "/static/plan.css",
	}
	for path, sheet := range pages {
		code, body := getSwapPage(t, app, cookie, path)
		if code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, code)
			continue
		}
		seen := map[string]int{}
		for _, m := range stylesheetHrefRE.FindAllStringSubmatch(body, -1) {
			seen[m[1]]++
		}
		for href, n := range seen {
			if n != 1 {
				t.Errorf("GET %s: stylesheet %s linked %d times, want exactly 1", path, href, n)
			}
		}
		own := sheet + "?v=" + version
		if !strings.Contains(body, `href="`+own+`"`) {
			t.Errorf("GET %s: its own stylesheet %s is not linked", path, own)
		}
	}
}

func TestSwapExcludedPagesKeepPageScripts(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	version := staticAssetVersion()

	// The simulator and saved fits always load whole (the swap
	// denylist names them), so their full documents keep fit.js —
	// and the shell around them, swap script included. The
	// planner's full document carries plan.js the same way; a
	// swap adopts it when the shell does not have it yet.
	for path, script := range map[string]string{
		"/fittings/":       `src="/static/fit.js?v=` + version + `"`,
		"/fittings/saved/": `src="/static/fit.js?v=` + version + `"`,
		"/planner/":        `src="/static/plan.js?v=` + version + `"`,
	} {
		code, body := getSwapPage(t, app, cookie, path)
		if code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, code)
			continue
		}
		mustContain(t, path, body,
			`<main id="main-content">`,
			`src="/static/swap.js?v=`+version+`"`,
			script)
	}
}
