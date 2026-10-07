package app

// v0.3.11 planet name resolution: the durable planet_names queue
// behind the PI surfaces. Renders note wants and never fetch;
// the worker resolves them through the public /universe/planets/
// endpoint; resolved names stick across restarts, 404s back off
// for days, transient errors leave the row pending.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// planetTransport answers /universe/planets/{id}/ with fixture
// names and counts fetches per id.
type planetTransport struct {
	mu      sync.Mutex
	fetches map[int64]int
	names   map[int64]string // id → name (absent = 404)
	fail    map[int64]bool   // 500: transient failure
}

func newPlanetTransport() *planetTransport {
	return &planetTransport{
		fetches: map[int64]int{},
		names:   map[int64]string{},
		fail:    map[int64]bool{},
	}
}

func (s *planetTransport) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (s *planetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if !strings.HasPrefix(path, "/universe/planets/") {
		return s.respond(404, `{"error":"unexpected path `+path+`"}`), nil
	}
	id, _ := strconv.ParseInt(strings.Trim(path[len("/universe/planets/"):], "/"), 10, 64)
	s.mu.Lock()
	s.fetches[id]++
	s.mu.Unlock()
	if s.fail[id] {
		return s.respond(500, `{"error":"boom"}`), nil
	}
	name, ok := s.names[id]
	if !ok {
		return s.respond(404, `{"error":"not found"}`), nil
	}
	return s.respond(200, fmt.Sprintf(`{"name":%q,"planet_id":%d,"system_id":30000142,"type_id":11}`, name, id)), nil
}

func (s *planetTransport) fetchCount(id int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches[id]
}

func TestPlanetResolveSuccessAndCacheHit(t *testing.T) {
	transport := newPlanetTransport()
	transport.names[40152063] = "Jita IV"
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	app.notePlanetIDs(ctx, 40152063)
	resolved, limited := app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if limited || resolved != 1 {
		t.Fatalf("resolve: resolved=%d limited=%v, want 1/false", resolved, limited)
	}
	if got := transport.fetchCount(40152063); got != 1 {
		t.Fatalf("fetches: %d, want 1", got)
	}
	row, err := q.GetPlanetName(ctx, 40152063)
	if err != nil || row.State != esi.PlanetResolved || row.Name != "Jita IV" {
		t.Fatalf("resolved row: %+v err=%v", row, err)
	}
	// The render tier answers from the durable row now.
	if got := app.planetDisplayName(ctx, 40152063); got != "Jita IV" {
		t.Fatalf("planetDisplayName: %q, want Jita IV", got)
	}
	// A fresh resolved row is not due: no repeat fetch next pass.
	resolved, _ = app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("second pass resolved %d, want 0", resolved)
	}
	if got := transport.fetchCount(40152063); got != 1 {
		t.Fatalf("fetches after second pass: %d, want 1 (cache hit)", got)
	}
}

func TestPlanetResolveNegativeBackoff(t *testing.T) {
	transport := newPlanetTransport()
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	const badID = int64(40999999)
	app.notePlanetIDs(ctx, badID)
	resolved, _ := app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("404 resolve: resolved=%d, want 0", resolved)
	}
	row, err := q.GetPlanetName(ctx, badID)
	if err != nil || row.State != esi.PlanetMissing {
		t.Fatalf("row after 404: %+v err=%v, want missing", row, err)
	}
	// The fresh negative is not re-asked within the window.
	app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if got := transport.fetchCount(badID); got != 1 {
		t.Fatalf("fetches: %d, want 1 (negative backoff)", got)
	}
	// The fallback stays honest meanwhile.
	if got := app.planetDisplayName(ctx, badID); got != "Planet #40999999" {
		t.Fatalf("planetDisplayName: %q, want the id fallback", got)
	}
}

func TestPlanetResolveTransientStaysPending(t *testing.T) {
	transport := newPlanetTransport()
	transport.fail[40152063] = true
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	app.notePlanetIDs(ctx, 40152063)
	resolved, _ := app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("transient resolve: resolved=%d, want 0", resolved)
	}
	row, err := q.GetPlanetName(ctx, 40152063)
	if err != nil || row.State != esi.PlanetPending {
		t.Fatalf("row after transient failure: %+v err=%v, want pending", row, err)
	}
}

func TestPlanetResolvePersistsWarmedNameWithoutFetch(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	// The colonies harvest warmed the name in-process; the
	// durable pass persists it without spending a fetch.
	app.esi.StorePlaceName(40152063, "Jita IV")
	app.notePlanetIDs(ctx, 40152063)
	resolved, _ := app.resolvePlanetNames(ctx, &fetchBudget{left: 120})
	if resolved != 1 {
		t.Fatalf("persist-warmed resolve: resolved=%d, want 1", resolved)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("persist made %d outbound calls, want 0", got)
	}
	row, err := q.GetPlanetName(ctx, 40152063)
	if err != nil || row.State != esi.PlanetResolved || row.Name != "Jita IV" {
		t.Fatalf("persisted row: %+v err=%v", row, err)
	}
}

// TestPlanetRenderFallbackThenName drives the colonies page the
// way the user sees it: "Planet #40152063" before the worker has
// resolved anything (with the want noted and zero outbound
// calls), the real name once the durable cache holds it.
func TestPlanetRenderFallbackThenName(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, esi.Colonies{
		{PlanetID: 40152063, SolarSystemID: 30000142, PlanetType: "temperate", NumPins: 1, UpgradeLevel: 0, LastUpdate: rfc(now.Add(-time.Hour))},
	})
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/planets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("planets page: status %d", code)
	}
	mustContain(t, "/planets/ (unresolved)", body, "Planet #40152063")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("planets render made %d outbound calls, want 0", got)
	}
	// The render noted the want for the worker.
	row, err := q.GetPlanetName(ctx, 40152063)
	if err != nil || row.State != esi.PlanetPending {
		t.Fatalf("want after render: %+v err=%v, want pending", row, err)
	}

	// The worker's answer lands in the durable cache...
	if err := q.SetPlanetName(ctx, db.SetPlanetNameParams{
		PlanetID: 40152063, Name: "Jita IV", State: esi.PlanetResolved,
		ResolvedAt: timeSet(now.UTC()),
	}); err != nil {
		t.Fatalf("seed resolved planet: %v", err)
	}
	// ...and the next render shows it, still without fetching.
	code, body = getPage(t, app, cookie, "/planets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("planets page (resolved): status %d", code)
	}
	mustContain(t, "/planets/ (resolved)", body, "Jita IV")
	if strings.Contains(body, "Planet #40152063") {
		t.Fatal("resolved render still shows the id fallback")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("resolved render made %d outbound calls, want 0", got)
	}
}

// TestPlanetAttentionLineResolves covers the user's screenshot:
// the home attention line reads "extractor on Planet #40152063
// has expired" until the name lands, then names the planet.
func TestPlanetAttentionLineResolves(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedSnapshot(t, q, fixtureCharA, esi.SnapPlanets, esi.Colonies{
		{PlanetID: 40152063, SolarSystemID: 30000142, PlanetType: "temperate", NumPins: 1, UpgradeLevel: 0, LastUpdate: rfc(now.Add(-time.Hour))},
	})
	seedSnapshot(t, q, fixtureCharA, esi.PlanetLayoutKind(40152063), esi.PlanetLayout{
		Pins: []esi.PlanetPin{
			{PinID: 1, TypeID: 990201, ExpiryTime: rfc(now.Add(-time.Hour)),
				ExtractorDetails: &esi.PlanetExtractor{ProductTypeID: 34, CycleTime: 1800, QtyPerCycle: 1200, Heads: []esi.PlanetExtractorHead{{HeadID: 0}}}},
		},
	})
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: `["attention"]`,
		ID:         user.ID,
	}); err != nil {
		t.Fatalf("set home layout: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home: status %d", code)
	}
	mustContain(t, "/ (unresolved attention)", body, "extractor on Planet #40152063 has expired")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home render made %d outbound calls, want 0", got)
	}

	if err := q.SetPlanetName(ctx, db.SetPlanetNameParams{
		PlanetID: 40152063, Name: "Jita IV", State: esi.PlanetResolved,
		ResolvedAt: timeSet(now.UTC()),
	}); err != nil {
		t.Fatalf("seed resolved planet: %v", err)
	}
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("home (resolved): status %d", code)
	}
	mustContain(t, "/ (resolved attention)", body, "extractor on Jita IV has expired")
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home (resolved) render made %d outbound calls, want 0", got)
	}
}
