package app

// Focused tests for the Sync page's name-coverage figure: the
// needed-ID set comes from the user's own snapshots, and the
// name lookups stay batched over those IDs (never the whole
// type_names / sde_types tables).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	grantTestAdmin(app, fixtureCharA)

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

	// The page reloads on a timer driven by app.js
	// (data-autorefresh), pausable and reduced-motion aware; only
	// readers without JavaScript get the meta-refresh. The old
	// always-on meta-refresh yanked focus and scroll mid-read.
	code, body := getPage(t, app, cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("sync page: status %d", code)
	}
	mustContain(t, "/sync/", body,
		`<body data-autorefresh="5">`,
		`<noscript><meta http-equiv="refresh" content="5"></noscript>`)
	if got := strings.Count(body, `http-equiv="refresh"`); got != 1 {
		t.Fatalf("/sync/ refreshes %d times in markup, want the one noscript fallback", got)
	}
	mustContain(t, "/sync/", body, "Names resolved 2 of 4 (50%)")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("sync page made %d outbound calls, want 0", got)
	}
}

// TestSyncAdminOnlyButPageStatusForEveryone pins the split inside
// /sync/: the Sync page and its buttons are admin-only, while
// /sync/page-status — the banner indicator's poll, which every
// signed-in page makes — answers any signed-in user. Gating the
// poll on admin left every other user's pages polling a 403
// forever.
func TestSyncAdminOnlyButPageStatusForEveryone(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// Not an admin: the page and its actions are refused...
	if code, _ := getPage(t, app, cookie, "/sync/"); code != http.StatusForbidden {
		t.Fatalf("GET /sync/ as a non-admin: status %d, want 403", code)
	}
	for _, path := range []string{"/sync/warm", "/sync/sde"} {
		if code, _ := postForm(t, app, cookie, path, url.Values{}); code != http.StatusForbidden {
			t.Fatalf("POST %s as a non-admin: status %d, want 403", path, code)
		}
	}
	// ...but the indicator's poll still answers.
	code, status := getPage(t, app, cookie, "/sync/page-status?page=%2Fmarket%2F")
	if code != http.StatusOK {
		t.Fatalf("GET /sync/page-status as a non-admin: status %d, want 200", code)
	}
	mustContain(t, "page status (non-admin)", status, `"pending":0`, `"loading":false`)

	// An admin gets the page.
	grantTestAdmin(app, fixtureCharA)
	if code, _ := getPage(t, app, cookie, "/sync/"); code != http.StatusOK {
		t.Fatalf("GET /sync/ as an admin: status %d, want 200", code)
	}

	// Signed out, the poll is not served: it bounces home like
	// every other signed-in route.
	req := httptest.NewRequest(http.MethodGet, "/sync/page-status?page=%2F", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /sync/page-status signed out: status %d, want 303", rec.Code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("sync routes made %d outbound calls, want 0", got)
	}
}
