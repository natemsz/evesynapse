package app

// The acting character moves only on a real view, never on a
// swap.js hover prefetch. A prefetch renders a per-pilot
// ?character= page ahead of time so the click is instant, but it
// changes nothing a view would (core_prefetch.go) — including
// the session's character pin. These tests pin that down after
// the owner watched his active character flip from Burzrujat to
// Timorsham Vot on a plain home -> overview navigation: the
// hover over Timorsham's fleet-table link had silently re-pinned
// the session underneath him.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// prefetchGet drives the router like getPage but can carry the
// X-Swap-Prefetch header swap.js sends for hover prefetches, and
// adopts any session cookie the response sets (browser
// semantics).
func prefetchGet(t *testing.T, app *Application, cookie **http.Cookie, path string, prefetch bool) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if *cookie != nil {
		req.AddCookie(*cookie)
	}
	if prefetch {
		req.Header.Set("X-Swap-Prefetch", "1")
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "evesynapse_session" && c.Value != "" {
			cc := c
			*cookie = cc
		}
	}
	return rec.Code, rec.Body.String()
}

// actingCharacterName reads the session's acting character off
// the management page's active marker.
func actingCharacterName(t *testing.T, app *Application, cookie **http.Cookie) string {
	t.Helper()
	code, body := prefetchGet(t, app, cookie, "/characters/", false)
	if code != http.StatusOK {
		t.Fatalf("GET /characters/: status %d", code)
	}
	for _, name := range []string{"Burzrujat", "Khastira", "Timorsham Vot"} {
		if strings.Contains(body, ">"+name+"</a></strong> <small>· active</small>") {
			return name
		}
	}
	return ""
}

// overviewSubjectName reads the pilot the /character/ overview
// presents as its subject (its charselector's active option).
func overviewSubjectName(body string) string {
	for _, name := range []string{"Burzrujat", "Khastira", "Timorsham Vot"} {
		if strings.Contains(body, `class="charselector-option active" data-name="`+name+`"`) {
			return name
		}
	}
	return ""
}

// seedPrefetchPilots links three pilots to one account and seeds
// the snapshots the character overview reads.
func seedPrefetchPilots(t *testing.T, q *db.Queries) int64 {
	t.Helper()
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, 91001, "Burzrujat")
	seedCharacter(t, q, user.ID, 91002, "Khastira")
	seedCharacter(t, q, user.ID, 91003, "Timorsham Vot")
	for _, id := range []int64{91001, 91002, 91003} {
		seedSnapshot(t, q, id, esi.SnapLocation, esi.Location{SolarSystemID: 30000142})
		seedSnapshot(t, q, id, esi.SnapShip, esi.Ship{ShipTypeID: 587, ShipName: "Fixture"})
		seedSnapshot(t, q, id, esi.SnapOnline, esi.Online{Logins: 7})
		seedSnapshot(t, q, id, esi.SnapClones, esi.Clones{})
		seedSnapshot(t, q, id, esi.SnapImplants, esi.Implants{})
		seedSnapshot(t, q, id, esi.SnapFatigue, esi.Fatigue{})
	}
	return user.ID
}

func TestPrefetchCharacterPageDoesNotRepinActingCharacter(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	userID := seedPrefetchPilots(t, q)
	cookie := sessionCookie(t, app, userID, 91001, "Burzrujat")

	if got := actingCharacterName(t, app, &cookie); got != "Burzrujat" {
		t.Fatalf("baseline acting character = %q, want Burzrujat", got)
	}

	// The hover: the mouse crosses Timorsham Vot's pilot link on
	// the home fleet table (/character/?character=<id>) and
	// swap.js prefetches it. No click happens.
	code, body := prefetchGet(t, app, &cookie, "/character/?character=91003", true)
	if code != http.StatusOK {
		t.Fatalf("prefetch status = %d, want 200", code)
	}
	// The prefetch still renders Timorsham's page — the swap
	// cache needs the requested pilot's content, not the pinned
	// one's.
	if got := overviewSubjectName(body); got != "Timorsham Vot" {
		t.Errorf("prefetched overview subject = %q, want Timorsham Vot (the render still follows the link)", got)
	}
	// But the session must still act as Burzrujat, on this page
	// and every other.
	if got := actingCharacterName(t, app, &cookie); got != "Burzrujat" {
		t.Errorf("after prefetch, acting character = %q, want Burzrujat", got)
	}
	_, body = prefetchGet(t, app, &cookie, "/character/", false)
	if got := overviewSubjectName(body); got != "Burzrujat" {
		t.Errorf("overview subject after prefetch = %q, want Burzrujat", got)
	}

	// The assets page resolves its pilot the same way, and its
	// prefetch must not move the pin either.
	code, _ = prefetchGet(t, app, &cookie, "/assets/?character=91003", true)
	if code != http.StatusOK {
		t.Fatalf("assets prefetch status = %d, want 200", code)
	}
	if got := actingCharacterName(t, app, &cookie); got != "Burzrujat" {
		t.Errorf("after assets prefetch, acting character = %q, want Burzrujat", got)
	}

	// Control: a prefetch of a page with no character pick.
	prefetchGet(t, app, &cookie, "/", true)
	if got := actingCharacterName(t, app, &cookie); got != "Burzrujat" {
		t.Errorf("control failed: prefetch of / moved acting character to %q", got)
	}
}

func TestPrefetchCharacterSwitchDoesNotFlipActingCharacter(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	userID := seedPrefetchPilots(t, q)
	cookie := sessionCookie(t, app, userID, 91001, "Burzrujat")

	// Hovering a header-switcher entry must not execute the
	// switch: it is a state-changing GET, and a speculative one
	// changes nothing.
	prefetchGet(t, app, &cookie, "/characters/switch?character=91003", true)
	if got := actingCharacterName(t, app, &cookie); got != "Burzrujat" {
		t.Errorf("after switch prefetch, acting character = %q, want Burzrujat", got)
	}
}

func TestViewedCharacterPickStillRepinsActingCharacter(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	userID := seedPrefetchPilots(t, q)
	cookie := sessionCookie(t, app, userID, 91001, "Burzrujat")

	// A real click on a per-pilot link still re-pins: viewing a
	// pilot's page makes them the acting character, by design.
	prefetchGet(t, app, &cookie, "/character/?character=91003", false)
	if got := actingCharacterName(t, app, &cookie); got != "Timorsham Vot" {
		t.Errorf("after viewed pick, acting character = %q, want Timorsham Vot", got)
	}

	// Same for a viewed assets link.
	prefetchGet(t, app, &cookie, "/characters/switch?character=91001", false)
	prefetchGet(t, app, &cookie, "/assets/?character=91002", false)
	if got := actingCharacterName(t, app, &cookie); got != "Khastira" {
		t.Errorf("after viewed assets pick, acting character = %q, want Khastira", got)
	}

	// And the header switcher itself, clicked for real.
	prefetchGet(t, app, &cookie, "/characters/switch?character=91003", false)
	if got := actingCharacterName(t, app, &cookie); got != "Timorsham Vot" {
		t.Errorf("after real switch, acting character = %q, want Timorsham Vot", got)
	}
}
