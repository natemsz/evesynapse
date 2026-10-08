package app

// Hermetic tests for the polish pass: the branched categorical
// top nav (markup, active-branch marking, favicon route), the
// suppression of the SSO login-error banner for signed-in
// sessions, and the two worker log-noise fixes (corp asset-name
// 404 backoff, remembered non-character IDs).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"evesynapse/internal/esi"
)

// TestPolishNavAndFavicon drives the real router through three
// pages in different nav branches and the favicon route, with a
// transport that must stay silent throughout.
func TestPolishNavAndFavicon(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// Fresh character snapshots so /character/ serves from the
	// snapshot cache (its zero-network contract) rather than
	// fetching missing data live.
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{SolarSystemID: 30000142})
	seedSnapshot(t, q, fixtureCharA, esi.SnapShip, esi.Ship{ShipTypeID: 587, ShipName: "Fixture"})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{Logins: 7})
	seedSnapshot(t, q, fixtureCharA, esi.SnapClones, esi.Clones{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapImplants, esi.Implants{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFatigue, esi.Fatigue{})

	// Home: branched nav markup and the Home branch marked.
	// (Name-only session: the full home sheet fetches live when
	// snapshots are missing, as before; the header is what this
	// test is about.)
	homeCookie := sessionCookie(t, app, user.ID, 0, "")
	code, body := getPage(t, app, homeCookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		`<details class="branch"`,
		`data-nav-category="pilot"`,
		`href="/corporations/members/"`,
		`href="/corporations/killmails/"`,
		`href="/intel/fw/"`,
		`href="/contracts/"`,
		`<a class="navlink active" href="/" data-tip="Home">`,
		`<span class="nav-label">Home</span>`)

	// Character page: the Character branch is the marked one.
	code, body = getPage(t, app, cookie, "/character/")
	if code != http.StatusOK {
		t.Fatalf("GET /character/: status %d", code)
	}
	mustContain(t, "/character/", body, `<details class="branch active"`)

	// Intel page: a different branch marked (wars lists as empty
	// state here — only the header marking matters).
	code, body = getPage(t, app, cookie, "/intel/wars/")
	if code != http.StatusOK {
		t.Fatalf("GET /intel/wars/: status %d", code)
	}
	mustContain(t, "/intel/wars/", body, `<details class="branch active"`)

	// Favicon: browsers' default request must not 404.
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /favicon.ico: status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/x-icon" {
		t.Errorf("favicon content type %q, want image/x-icon", ct)
	}
	// An ICO starts 00 00 01 00 (reserved, type 1 = icon), then the image count.
	if body := rec.Body.Bytes(); len(body) < 6 || body[0] != 0 || body[1] != 0 || body[2] != 1 || body[3] != 0 {
		t.Error("favicon body is not an ICO file")
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestHomeLoginErrorBannerVisibility: the /?error=... banner from
// a failed SSO callback is shown to signed-out visitors only. A
// stale duplicate callback landing after a successful sign-in
// must not paint a failure banner over a working sheet.
func TestHomeLoginErrorBannerVisibility(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/?error=state")
	if code != http.StatusOK {
		t.Fatalf("signed-in home: status %d", code)
	}
	if strings.Contains(body, "Please try logging in again") {
		t.Error("signed-in home rendered the login-error banner")
	}

	// Signed out, the same URL still explains the failed attempt.
	req := httptest.NewRequest(http.MethodGet, "/?error=state", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed-out home: status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Please try logging in again") {
		t.Error("signed-out home lost the login-error banner")
	}
}

// names404Transport answers the corp asset-names POST with a 404
// (and everything else 500) while counting the POSTs.
type names404Transport struct{ posts atomic.Int64 }

func (s *names404Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	status := http.StatusInternalServerError
	if strings.Contains(req.URL.Path, "/assets/names/") {
		s.posts.Add(1)
		status = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"fixture"}`)),
	}, nil
}

// TestWarmCorpAssetNames404BacksOff: a 404 from the names POST is
// recorded and backed off — the second cycle makes no new call.
func TestWarmCorpAssetNames404BacksOff(t *testing.T) {
	transport := &names404Transport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpAssets, []esi.Asset{
		{ItemID: 123456789, TypeID: 587, Quantity: 1, IsSingleton: true,
			LocationID: 60003760, LocationType: "station", LocationFlag: "CorpSAG1"},
	})

	app.warmCorpAssetNames(ctx, ch, fixtureCorpA)
	state, detail, found := app.corpKindState(ctx, fixtureCharA, corpAssetNamesKind)
	if !found || state != fetchStateError {
		t.Fatalf("after 404: fetch state = (%q, %q, %v), want error recorded", state, detail, found)
	}

	app.warmCorpAssetNames(ctx, ch, fixtureCorpA)
	if got := transport.posts.Load(); got != 1 {
		t.Fatalf("names POST made %d calls across two cycles, want 1 (backoff)", got)
	}
}

// char422Transport answers GET /characters/1000051/ with a 422
// (an NPC corporation ID harvested as a "character") and counts.
type char422Transport struct{ calls atomic.Int64 }

func (s *char422Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	status := http.StatusInternalServerError
	if req.URL.Path == "/characters/1000051/" {
		status = 422
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"fixture"}`)),
	}, nil
}

// TestWarmCharacterNameRemembersNonCharacters: one definitive 422
// is remembered; the warm pass never asks about the ID again.
func TestWarmCharacterNameRemembersNonCharacters(t *testing.T) {
	transport := &char422Transport{}
	app, _, _ := buildCorpTestApp(t, transport)
	ctx := context.Background()
	budget := &warmBudget{left: 10}

	if app.warmCharacterName(ctx, budget, 1000051) {
		t.Fatal("first warm of a non-character ID reported success")
	}
	if !app.esi.CharacterNameMissed(1000051) {
		t.Fatal("422 answer was not remembered as a non-character")
	}
	if app.warmCharacterName(ctx, budget, 1000051) {
		t.Fatal("second warm of a non-character ID reported success")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("character endpoint called %d times, want 1", got)
	}
}
