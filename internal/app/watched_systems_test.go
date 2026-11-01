package app

// Watched systems modal: the modal's save endpoint edits the
// one stored list (notifyPrefs.CampSystems) by name and by id,
// reports unknown names, duplicates and a full list instead of
// dropping them silently, and a notification settings save that
// no longer carries the old box leaves the list alone.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWatchedSystemsModalSave(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.95), (30002011, 'Bosena', 10000037, 0.1) ON CONFLICT DO NOTHING`)
	for i := 1; i <= 10; i++ {
		exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES ($1, $2, 10000001, 0.5) ON CONFLICT DO NOTHING`,
			int64(30002000+i), fmt.Sprintf("Filler %02d", i))
	}
	jar := []*http.Cookie{sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")}
	post := func(path string, form url.Values) {
		t.Helper()
		code, _, cookies := doReq(t, app, http.MethodPost, path, form, jar...)
		if code != http.StatusSeeOther {
			t.Fatalf("POST %s: status %d, want 303", path, code)
		}
		if len(cookies) > 0 {
			jar = cookies
		}
	}
	get := func(path string) string {
		t.Helper()
		code, body, cookies := doReq(t, app, http.MethodGet, path, nil, jar...)
		if code != 200 {
			t.Fatalf("GET %s: status %d", path, code)
		}
		if len(cookies) > 0 {
			jar = cookies
		}
		return body
	}
	watched := func() []int64 {
		t.Helper()
		return app.notifyPrefsFor(ctx, user.ID).CampSystems
	}
	save := func(form url.Values) { post("/settings/watched-systems", form) }

	// Add by name, resolved case-insensitively like the
	// settings box resolved it.
	save(url.Values{"add_name": {"jita"}, "return": {"/"}})
	if got := watched(); len(got) != 1 || got[0] != 30000142 {
		t.Fatalf("after add: watched = %v, want [30000142]", got)
	}
	if body := get("/"); !strings.Contains(body, "Jita added to your watched systems.") {
		t.Fatal("after add: the confirmation did not show")
	}

	// A duplicate is reported and changes nothing.
	save(url.Values{"add_name": {"Jita"}, "return": {"/"}})
	if got := watched(); len(got) != 1 {
		t.Fatalf("after duplicate: watched = %v, want one entry", got)
	}
	if body := get("/"); !strings.Contains(body, "Jita is already watched.") {
		t.Fatal("after duplicate: the report did not show")
	}

	// An unknown name is reported, not silently dropped.
	save(url.Values{"add_name": {"No Such System"}, "return": {"/"}})
	if got := watched(); len(got) != 1 || got[0] != 30000142 {
		t.Fatalf("after unknown: watched = %v, want [30000142]", got)
	}
	if body := get("/"); !strings.Contains(body, "Unknown system: No Such System.") {
		t.Fatal("after unknown: the report did not show")
	}

	// Remove by id.
	save(url.Values{"remove": {"30000142"}, "return": {"/settings#notifications"}})
	if got := watched(); len(got) != 0 {
		t.Fatalf("after remove: watched = %v, want empty", got)
	}
	if body := get("/settings"); !strings.Contains(body, "Jita is no longer watched.") {
		t.Fatal("after remove: the confirmation did not show")
	}

	// The cap is the stored list's own: nine in place, the
	// tenth fits, the eleventh is reported and refused.
	nine := make([]int64, 0, 9)
	for i := 1; i <= 9; i++ {
		nine = append(nine, int64(30002000+i))
	}
	if err := app.storeNotifyPrefs(ctx, user.ID, notifyPrefs{CampSystems: nine}); err != nil {
		t.Fatal(err)
	}
	save(url.Values{"add_name": {"Filler 10"}, "return": {"/"}})
	if got := watched(); len(got) != 10 {
		t.Fatalf("after tenth add: watched = %v, want 10 entries", got)
	}
	save(url.Values{"add_name": {"Bosena"}, "return": {"/"}})
	if got := watched(); len(got) != 10 {
		t.Fatalf("after eleventh add: watched = %v, want the same 10", got)
	}
	if body := get("/"); !strings.Contains(body, "The watched list is full at 10 systems.") {
		t.Fatal("after eleventh add: the full report did not show")
	}

	// A notification settings save no longer carries the camp
	// box; the stored list rides through untouched.
	post("/notifications/settings", url.Values{
		"channels_shown": {"1"}, "kinds_shown": {"1"}, "cards_shown": {"1"},
	})
	if got := watched(); len(got) != 10 {
		t.Fatalf("after settings save: watched = %v, want the same 10", got)
	}

	// The settings row offers the modal, not the box.
	body := get("/settings")
	if !strings.Contains(body, "Manage watched systems") || !strings.Contains(body, "Watched systems (10 of 10)") {
		t.Fatal("settings: the camp row does not offer the watched systems modal")
	}
	if strings.Contains(body, `name="camp_systems"`) {
		t.Fatal("settings: the camp box is still on the page")
	}
	if !strings.Contains(body, `id="watched-systems-modal"`) {
		t.Fatal("settings: the watched systems modal is not on the page")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("requests made %d outbound calls, want 0", got)
	}
}
