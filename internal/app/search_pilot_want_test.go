package app

// Topbar pilot-name warming: a character name no local tier
// knows must not silently miss. The search notes one
// name-resolution want, answers with a "searching" row, and —
// once the worker has stored the pilot record — returns the
// real pilot suggestion. The handler itself never fetches.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func getSearchHits(t *testing.T, app *Application, cookie *http.Cookie, path string) []searchHit {
	t.Helper()
	code, body := getPage(t, app, cookie, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, code)
	}
	var hits []searchHit
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("GET %s: decode hits: %v (%q)", path, err, body)
	}
	return hits
}

func TestTopbarSearchUnwarmedPilotNameWant(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// Unknown name: one pending row, one want, no fetches.
	hits := getSearchHits(t, app, cookie, "/search.json?q=Unwarmed%20Stranger")
	if len(hits) != 1 || hits[0].Kind != "pilot-pending" || !strings.Contains(hits[0].Name, "Unwarmed Stranger") {
		t.Fatalf("unwarmed hits = %+v, want one pilot-pending row", hits)
	}
	want, err := q.GetPilotNameWant(ctx, "unwarmed stranger")
	if err != nil {
		t.Fatalf("pilot name want: %v", err)
	}
	if want.State != "pending" || want.DisplayName != "Unwarmed Stranger" {
		t.Fatalf("pilot name want = %+v, want pending Unwarmed Stranger", want)
	}

	// The same name in another case: still one want.
	hits = getSearchHits(t, app, cookie, "/search.json?q=unwarmed%20stranger")
	if len(hits) != 1 || hits[0].Kind != "pilot-pending" {
		t.Fatalf("repeat hits = %+v, want the pending row again", hits)
	}
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pilot_name_wants`).Scan(&count); err != nil {
		t.Fatalf("count pilot name wants: %v", err)
	}
	if count != 1 {
		t.Fatalf("pilot name wants = %d, want exactly 1", count)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("search handler made %d outbound calls, want 0", got)
	}

	// Simulate the worker's result: the name resolved and the
	// pilot record landed. The same search now offers the pilot.
	strangerID := int64(93300077)
	if err := q.SetPilotNameWantReady(ctx, db.SetPilotNameWantReadyParams{
		CharacterID: strangerID, ResolvedAt: "2026-10-04T00:00:00Z", NormalizedName: "unwarmed stranger",
	}); err != nil {
		t.Fatalf("settle name want: %v", err)
	}
	payload, err := json.Marshal(pilotPayload{
		Profile: esi.Character{Name: "Unwarmed Stranger"},
		Corp:    esi.Corporation{Name: "Stranger Corp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: strangerID, Payload: string(payload),
		State: pilotStateReady, FetchedAt: "2026-10-04T00:00:00Z",
	}); err != nil {
		t.Fatalf("store pilot record: %v", err)
	}
	hits = getSearchHits(t, app, cookie, "/search.json?q=Unwarmed%20Stranger")
	if len(hits) != 1 || hits[0].Kind != "pilot" || hits[0].ID != strangerID || hits[0].Name != "Unwarmed Stranger" {
		t.Fatalf("warmed hits = %+v, want the pilot suggestion", hits)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("search handler made %d outbound calls, want 0", got)
	}

	// The autocomplete ships the pending-row treatment: shown
	// but never a pick, with a bounded re-ask while it warms.
	_, js := getPage(t, app, cookie, "/static/app.js")
	mustContain(t, "/static/app.js (pilot-name search)", js,
		"pilot-pending", "pendingRetries", `cache: "no-store"`)
}

// pilotNameResolveTransport answers the public name lookup with
// one known character; every other name exact-misses against it.
type pilotNameResolveTransport struct{ calls atomic.Int64 }

func (s *pilotNameResolveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	body := `{"error":"unexpected path"}`
	status := http.StatusInternalServerError
	if req.URL.Path == "/universe/ids/" {
		body = `{"characters":[{"id":93300077,"name":"Unwarmed Stranger"}]}`
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestPilotNameWantResolutionQueuesPilotRecord(t *testing.T) {
	transport := &pilotNameResolveTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	note := func(display string) {
		t.Helper()
		if err := q.UpsertPilotNameWant(ctx, db.UpsertPilotNameWantParams{
			NormalizedName: normalizePilotName(display),
			DisplayName:    display,
			RequestedAt:    "2026-10-04T00:00:00Z",
		}); err != nil {
			t.Fatalf("note want %q: %v", display, err)
		}
	}

	note("Unwarmed Stranger")
	resolved, limited := app.refreshPilotNameWants(ctx, &fetchBudget{left: 10})
	if resolved != 1 || limited {
		t.Fatalf("resolution: resolved=%d limited=%v, want 1/false", resolved, limited)
	}
	want, err := q.GetPilotNameWant(ctx, "unwarmed stranger")
	if err != nil {
		t.Fatalf("get want: %v", err)
	}
	if want.State != "ready" || want.CharacterID != 93300077 {
		t.Fatalf("want = %+v, want ready for 93300077", want)
	}
	rec, err := q.GetPilotRecord(ctx, 93300077)
	if err != nil {
		t.Fatalf("pilot record: %v", err)
	}
	if rec.State != pilotStatePending {
		t.Fatalf("pilot record state = %q, want pending (queued by the resolution)", rec.State)
	}

	// A name ESI does not exact-match settles as missing, once.
	note("Ghost Pilot")
	resolved, limited = app.refreshPilotNameWants(ctx, &fetchBudget{left: 10})
	if resolved != 1 || limited {
		t.Fatalf("missing resolution: resolved=%d limited=%v, want 1/false", resolved, limited)
	}
	want, err = q.GetPilotNameWant(ctx, "ghost pilot")
	if err != nil {
		t.Fatalf("get missing want: %v", err)
	}
	if want.State != "missing" {
		t.Fatalf("want = %+v, want missing", want)
	}

	// Settled wants are never due again.
	resolved, _ = app.refreshPilotNameWants(ctx, &fetchBudget{left: 10})
	if resolved != 0 {
		t.Fatalf("settled pass resolved %d, want 0", resolved)
	}
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("name lookups = %d, want 2 (one per distinct name)", got)
	}
}
