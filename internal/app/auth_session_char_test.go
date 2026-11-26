package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"

	"github.com/alexedwards/scs/v2"
)

// TestSessionCharIDRoundTrip: the acting character survives the
// session as a full int64. EVE IDs reach past 32 bits, so the old
// int() conversion corrupted them on 32-bit builds.
func TestSessionCharIDRoundTrip(t *testing.T) {
	sm := scs.New()
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	const big = int64(9_876_543_210)
	putSessionCharID(sm, ctx, big)
	if got := sessionCharID(sm, ctx); got != big {
		t.Fatalf("sessionCharID = %d, want %d", got, big)
	}
}

// TestSessionCharIDLegacyInt: sessions written before the int64
// switch (a plain int) still read.
func TestSessionCharIDLegacyInt(t *testing.T) {
	sm := scs.New()
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	sm.Put(ctx, sessionCharacterID, int(95465499))
	if got := sessionCharID(sm, ctx); got != 95465499 {
		t.Fatalf("sessionCharID = %d, want 95465499", got)
	}
}

// TestSessionCharIDMissing: no acting character reads as zero.
func TestSessionCharIDMissing(t *testing.T) {
	sm := scs.New()
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	if got := sessionCharID(sm, ctx); got != 0 {
		t.Fatalf("sessionCharID = %d, want 0", got)
	}
}

// TestPickCharacterViewDoesNotPin: a view pick (?view=1, the
// links notifications and the briefing carry) renders the named
// character's page without moving the session's acting character.
// A plain pick still pins: the character switcher relies on it.
func TestPickCharacterViewDoesNotPin(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	run := func(target string) (active, pin int64) {
		req := httptest.NewRequest(http.MethodGet, "https://example.com"+target, nil)
		req.AddCookie(cookie)
		app.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, a, _, err := app.pickCharacter(r.Context(), r)
			if err != nil {
				t.Fatalf("pickCharacter: %v", err)
			}
			active, pin = a.CharacterID, sessionCharID(app.sessions, r.Context())
		})).ServeHTTP(httptest.NewRecorder(), req)
		return active, pin
	}

	if active, pin := run("/character/?character=90000002&view=1"); active != fixtureCharB || pin != fixtureCharA {
		t.Errorf("view pick: active %d, pin %d, want Bravo (%d) shown with the pin still on Alpha (%d)",
			active, pin, fixtureCharB, fixtureCharA)
	}
	if active, pin := run("/character/?character=90000002"); active != fixtureCharB || pin != fixtureCharB {
		t.Errorf("plain pick: active %d, pin %d, want both on Bravo (%d)", active, pin, fixtureCharB)
	}
}

// TestCharacterPageSyncsTopbarNameAndPortrait: when a plain character
// request pins a different character mid-handler, the rendered topbar
// shows the new character's name alongside its portrait rather than
// leaving the previous character's name in place.
func TestCharacterPageSyncsTopbarNameAndPortrait(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo")
	seedSnapshot(t, q, fixtureCharB, esi.SnapOnline, esi.Online{})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/character/?character=90000002")
	if code != http.StatusOK {
		t.Fatalf("GET /character/?character=90000002 = %d", code)
	}
	wantPortrait := `src="https://images.evetech.net/characters/90000002/portrait?size=32"`
	if !strings.Contains(body, wantPortrait) {
		t.Errorf("topbar missing Bravo's portrait (%s)", wantPortrait)
	}
	if !strings.Contains(body, `<span class="topbar-character-name">Fixture Bravo</span>`) {
		t.Errorf("topbar name did not update to Fixture Bravo alongside Bravo's portrait")
	}
}

// TestOverviewPreservesViewPickAcrossLinksAndBannerDismiss: a view
// pick (?view=1) keeps view=1 on sheet links and on the trained-banner
// dismiss return path, so following or dismissing a skill notification
// for another pilot never moves the session's acting character.
func TestOverviewPreservesViewPickAcrossLinksAndBannerDismiss(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo")
	seedSnapshot(t, q, fixtureCharB, esi.SnapOnline, esi.Online{})
	seedSnapshot(t, q, fixtureCharB, esi.SnapSkills, esi.Skills{})
	if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
		UserID: user.ID, CharacterID: fixtureCharB, Kind: notifySkill,
		Title: "Fixture Bravo finished training Mechanics V",
		Url:   "/character/?character=90000002", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert notification: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	post := func(path string, form url.Values) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		for _, c := range rec.Result().Cookies() {
			if c.Name == "evesynapse_session" && c.Value != "" {
				cookie = c
			}
		}
		return rec.Code, rec.Header().Get("Location")
	}

	// Opening via /notifications/open keeps Alpha pinned even when the
	// stored notification lacked &view=1, and redirects with view=1.
	code, where := post("/notifications/open", url.Values{"kind": {notifySkill}})
	if code != http.StatusSeeOther || where != "/character/?character=90000002&view=1" {
		t.Fatalf("POST /notifications/open = %d to %q, want 303 to /character/?character=90000002&view=1", code, where)
	}
	_, charsBody := prefetchGet(t, app, &cookie, "/characters/", false)
	if !strings.Contains(charsBody, ">Fixture Alpha</a> <small>· active</small>") {
		t.Errorf("acting character moved away from Fixture Alpha after opening notification")
	}

	// Render the view-picked sheet and check the dismiss return target & links preserve view=1.
	code, sheet := getPage(t, app, cookie, where)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", where, code)
	}
	if !strings.Contains(sheet, `value="/character/?character=90000002&amp;view=1"`) {
		t.Errorf("trained banner dismiss return URL dropped view=1")
	}
	if !strings.Contains(sheet, `surface=browse&amp;view=1`) {
		t.Errorf("overview tab links dropped view=1")
	}
	if !strings.Contains(sheet, `<input type="hidden" name="view" value="1">`) {
		t.Errorf("overview toolbar search form dropped view=1")
	}

	// Dismissing the banner follows the return URL and must still keep Alpha active.
	code, where = post("/notifications/read", url.Values{
		"kind":      {notifySkill},
		"character": {"90000002"},
		"return":    {"/character/?character=90000002&view=1"},
	})
	if code != http.StatusSeeOther || where != "/character/?character=90000002&view=1" {
		t.Fatalf("POST /notifications/read = %d to %q", code, where)
	}
	code, afterDismiss := prefetchGet(t, app, &cookie, where, false)
	if code != http.StatusOK {
		t.Fatalf("GET %s after dismiss = %d", where, code)
	}
	if !strings.Contains(afterDismiss, `<span class="topbar-character-name">Fixture Alpha</span>`) {
		t.Errorf("topbar name moved off Fixture Alpha after banner dismiss")
	}
	_, charsBody = prefetchGet(t, app, &cookie, "/characters/", false)
	if !strings.Contains(charsBody, ">Fixture Alpha</a> <small>· active</small>") {
		t.Errorf("acting character moved away from Fixture Alpha after banner dismiss")
	}
}
