package app

// Tests for conditional ESI fetches. A dataset that comes in one
// response is stored with the ETag it came with; the next refresh
// offers that ETag back, and when ESI answers 304 Not Modified the
// payload stays as it is and only its cache window moves.

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// etagESI plays an endpoint that supports conditional requests: it
// serves the current version of a body under its ETag, and answers
// 304 with no body to a request that already has that version.
type etagESI struct {
	mu      sync.Mutex
	etag    string    // the current version, quotes included
	body    string    // the current body
	expires time.Time // sent as Expires on every answer
	pages   string    // sent as X-Pages when not empty

	seen []etagRequest
}

type etagRequest struct {
	path        string
	ifNoneMatch string
	status      int
}

func (s *etagESI) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("ETag", s.etag)
	h.Set("Expires", s.expires.UTC().Format(http.TimeFormat))
	if s.pages != "" {
		h.Set("X-Pages", s.pages)
	}
	status, body := http.StatusOK, s.body
	if inm := req.Header.Get("If-None-Match"); inm != "" && inm == s.etag {
		status, body = http.StatusNotModified, ""
	}
	s.seen = append(s.seen, etagRequest{path: req.URL.Path, ifNoneMatch: req.Header.Get("If-None-Match"), status: status})
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (s *etagESI) set(etag, body string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.etag, s.body, s.expires = etag, body, expires
}

// last returns the most recent request the stub answered.
func (s *etagESI) last(t *testing.T) etagRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		t.Fatal("no request reached ESI")
	}
	return s.seen[len(s.seen)-1]
}

func (s *etagESI) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func mustSnapshot(t *testing.T, q *db.Queries, characterID int64, kind string) db.CharacterSnapshot {
	t.Helper()
	snap, err := q.GetSnapshot(context.Background(), db.GetSnapshotParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		t.Fatalf("read %s snapshot: %v", kind, err)
	}
	return snap
}

func TestSnapshotRefreshIsConditional(t *testing.T) {
	soon := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	later := soon.Add(20 * time.Minute)
	stub := &etagESI{etag: `"v1"`, body: `{"skills":[],"total_sp":100}`, expires: soon}
	app, _, q := buildCorpTestApp(t, stub)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// Nothing stored yet: an ordinary fetch, stored with its ETag.
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapSkills); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if got := stub.last(t); got.ifNoneMatch != "" || got.status != http.StatusOK {
		t.Fatalf("first fetch sent If-None-Match %q and got %d; want an unconditional 200", got.ifNoneMatch, got.status)
	}
	first := mustSnapshot(t, q, fixtureCharA, esi.SnapSkills)
	if first.Payload != `{"skills":[],"total_sp":100}` || first.Etag != `"v1"` {
		t.Fatalf("stored payload %q with ETag %q", first.Payload, first.Etag)
	}
	if first.CachedUntil.String != soon.UTC().Format(time.RFC3339) {
		t.Fatalf("cached_until %q, want the response's Expires %q", first.CachedUntil.String, soon.UTC().Format(time.RFC3339))
	}

	// Unchanged at ESI: the stored ETag is offered, ESI answers 304,
	// and the payload and ETag stay while the cache window moves on.
	stub.set(`"v1"`, `{"skills":[],"total_sp":100}`, later)
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapSkills); err != nil {
		t.Fatalf("conditional fetch: %v", err)
	}
	if got := stub.last(t); got.ifNoneMatch != `"v1"` || got.status != http.StatusNotModified {
		t.Fatalf("second fetch sent If-None-Match %q and got %d; want the stored ETag and a 304", got.ifNoneMatch, got.status)
	}
	kept := mustSnapshot(t, q, fixtureCharA, esi.SnapSkills)
	if kept.Payload != first.Payload || kept.Etag != first.Etag {
		t.Fatalf("a 304 changed the stored copy: payload %q, ETag %q", kept.Payload, kept.Etag)
	}
	if kept.CachedUntil.String != later.UTC().Format(time.RFC3339) {
		t.Fatalf("after a 304 cached_until is %q, want it renewed to %q", kept.CachedUntil.String, later.UTC().Format(time.RFC3339))
	}
	if !esi.SnapshotFresh(kept) {
		t.Fatal("a snapshot ESI just confirmed does not read as fresh")
	}

	// Changed at ESI: the same offer gets the new version back, and
	// that replaces the stored copy and its ETag.
	stub.set(`"v2"`, `{"skills":[],"total_sp":250}`, later)
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapSkills); err != nil {
		t.Fatalf("fetch after a change: %v", err)
	}
	if got := stub.last(t); got.ifNoneMatch != `"v1"` || got.status != http.StatusOK {
		t.Fatalf("third fetch sent If-None-Match %q and got %d; want the old ETag and a 200", got.ifNoneMatch, got.status)
	}
	changed := mustSnapshot(t, q, fixtureCharA, esi.SnapSkills)
	if changed.Payload != `{"skills":[],"total_sp":250}` || changed.Etag != `"v2"` {
		t.Fatalf("after a change the stored copy is %q with ETag %q", changed.Payload, changed.Etag)
	}
}

// TestGetCachedUsesTheStoredCopyOnNotModified: a page-side read of
// a stale snapshot that ESI confirms unchanged decodes the stored
// payload; a stored payload that does not decode is downloaded
// again in full rather than merely confirmed.
func TestGetCachedUsesTheStoredCopyOnNotModified(t *testing.T) {
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	stub := &etagESI{etag: `"v1"`, body: `{"skills":[],"total_sp":100}`, expires: past}
	app, _, q := buildCorpTestApp(t, stub)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// Stored, and already past its cache window.
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapSkills); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}
	var skills esi.Skills
	if err := app.esi.GetCached(ctx, ch, esi.SnapSkills, &skills); err != nil {
		t.Fatalf("GetCached over a stale snapshot: %v", err)
	}
	if got := stub.last(t); got.ifNoneMatch != `"v1"` || got.status != http.StatusNotModified {
		t.Fatalf("GetCached sent If-None-Match %q and got %d; want a conditional 304", got.ifNoneMatch, got.status)
	}
	if skills.TotalSP != 100 {
		t.Fatalf("decoded total_sp %d from the stored copy, want 100", skills.TotalSP)
	}

	// A stored copy that cannot be decoded: fresh by its timestamp,
	// useless by its content. It must be fetched without the ETag.
	if err := q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: fixtureCharA,
		Kind:        esi.SnapSkills,
		Payload:     `this is not json`,
		FetchedAt:   "2026-01-01T00:00:00Z",
		CachedUntil: sql.NullString{String: "2999-01-01T00:00:00Z", Valid: true},
		Etag:        `"v1"`,
	}); err != nil {
		t.Fatalf("store an undecodable copy: %v", err)
	}
	skills = esi.Skills{}
	if err := app.esi.GetCached(ctx, ch, esi.SnapSkills, &skills); err != nil {
		t.Fatalf("GetCached over an undecodable copy: %v", err)
	}
	if got := stub.last(t); got.ifNoneMatch != "" || got.status != http.StatusOK {
		t.Fatalf("the refetch sent If-None-Match %q and got %d; want an unconditional 200", got.ifNoneMatch, got.status)
	}
	if skills.TotalSP != 100 {
		t.Fatalf("decoded total_sp %d after the full refetch, want 100", skills.TotalSP)
	}
	if got := mustSnapshot(t, q, fixtureCharA, esi.SnapSkills).Payload; got != `{"skills":[],"total_sp":100}` {
		t.Fatalf("the undecodable copy was not replaced: %q", got)
	}
}

// TestPaginatedSnapshotsAreNeverConditional: a dataset spread over
// several pages has an ETag per page and none for the whole, so it
// is stored without one and always downloaded in full.
func TestPaginatedSnapshotsAreNeverConditional(t *testing.T) {
	stub := &etagESI{etag: `"page-one"`, body: `[{"item_id":1,"type_id":34}]`, expires: time.Now().Add(time.Hour), pages: "1"}
	app, _, q := buildCorpTestApp(t, stub)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	for fetch := 1; fetch <= 2; fetch++ {
		if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapAssets); err != nil {
			t.Fatalf("assets fetch %d: %v", fetch, err)
		}
		if got := stub.last(t); got.ifNoneMatch != "" {
			t.Fatalf("assets fetch %d sent If-None-Match %q", fetch, got.ifNoneMatch)
		}
	}
	if got := mustSnapshot(t, q, fixtureCharA, esi.SnapAssets).Etag; got != "" {
		t.Fatalf("a paginated snapshot was stored with ETag %q", got)
	}
}

func TestGlobalSnapshotRefreshIsConditional(t *testing.T) {
	soon := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	later := soon.Add(time.Hour)
	stub := &etagESI{etag: `"fw-1"`, body: `[{"faction_id":500001,"pilots":10}]`, expires: soon}
	app, _, q := buildCorpTestApp(t, stub)
	ctx := context.Background()

	if err := app.esi.FetchAndStoreGlobalSnapshot(ctx, esi.GlobalFWStats); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if got := stub.last(t); got.ifNoneMatch != "" {
		t.Fatalf("first fetch sent If-None-Match %q", got.ifNoneMatch)
	}

	stub.set(`"fw-1"`, `[{"faction_id":500001,"pilots":10}]`, later)
	if err := app.esi.FetchAndStoreGlobalSnapshot(ctx, esi.GlobalFWStats); err != nil {
		t.Fatalf("conditional fetch: %v", err)
	}
	if got := stub.last(t); got.ifNoneMatch != `"fw-1"` || got.status != http.StatusNotModified {
		t.Fatalf("second fetch sent If-None-Match %q and got %d; want a conditional 304", got.ifNoneMatch, got.status)
	}
	snap, err := q.GetGlobalSnapshot(ctx, esi.GlobalFWStats)
	if err != nil {
		t.Fatalf("read global snapshot: %v", err)
	}
	if snap.Payload != `[{"faction_id":500001,"pilots":10}]` || snap.Etag != `"fw-1"` {
		t.Fatalf("a 304 changed the stored copy: %q with ETag %q", snap.Payload, snap.Etag)
	}
	if snap.CachedUntil != later.UTC().Format(time.RFC3339) {
		t.Fatalf("after a 304 cached_until is %q, want %q", snap.CachedUntil, later.UTC().Format(time.RFC3339))
	}
}

// TestRefreshCommandForgetsETags: `evesynapse -refresh` exists to
// download everything again. With the ETags left in place the next
// fetch would only ask whether each copy is current, and keep it.
func TestRefreshCommandForgetsETags(t *testing.T) {
	stub := &etagESI{etag: `"v1"`, body: `{"skills":[],"total_sp":100}`, expires: time.Now().Add(time.Hour)}
	app, conn, q := buildCorpTestApp(t, stub)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapSkills); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}
	if err := app.esi.FetchAndStoreGlobalSnapshot(ctx, esi.GlobalFWStats); err != nil {
		t.Fatalf("seed global fetch: %v", err)
	}

	if _, err := expireCaches(conn); err != nil {
		t.Fatalf("expireCaches: %v", err)
	}
	if got := mustSnapshot(t, q, fixtureCharA, esi.SnapSkills).Etag; got != "" {
		t.Fatalf("after a refresh the character snapshot still has ETag %q", got)
	}
	if snap, err := q.GetGlobalSnapshot(ctx, esi.GlobalFWStats); err != nil || snap.Etag != "" {
		t.Fatalf("after a refresh the global snapshot has ETag %q (err %v)", snap.Etag, err)
	}

	before := stub.count()
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, esi.SnapSkills); err != nil {
		t.Fatalf("fetch after the refresh: %v", err)
	}
	if got := stub.last(t); stub.count() != before+1 || got.ifNoneMatch != "" || got.status != http.StatusOK {
		t.Fatalf("the fetch after a refresh sent If-None-Match %q and got %d; want a full download", got.ifNoneMatch, got.status)
	}
}
