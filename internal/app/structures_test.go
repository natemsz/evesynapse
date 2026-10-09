package app

// Structure-resolution tiers: multi-character attempts
// (first success wins, one character's 403 never poisons the
// cache), the corporation-structure tier, and provenance
// precedence (ESI truth outranks lower-trust sources).

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

	"github.com/alexedwards/scs/pgxstore"
	scs "github.com/alexedwards/scs/v2"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

// structureAttemptTransport answers /universe/structures/{id}/
// per calling character (the bearer token is "tok-<characterID>"),
// so tests can watch the attempt order.
type structureAttemptTransport struct {
	mu       sync.Mutex
	attempts []int64
	deny     map[int64]bool // 403: this character cannot name it
	fail     map[int64]bool // 500: transient failure
	name     string
}

func (s *structureAttemptTransport) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (s *structureAttemptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if !strings.Contains(path, "/universe/structures/") {
		return s.respond(404, `{"error":"unexpected path `+path+`"}`), nil
	}
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer tok-")
	charID, _ := strconv.ParseInt(token, 10, 64)
	s.mu.Lock()
	s.attempts = append(s.attempts, charID)
	s.mu.Unlock()
	switch {
	case s.fail[charID]:
		return s.respond(500, `{"error":"boom"}`), nil
	case s.deny[charID]:
		return s.respond(403, `{"error":"forbidden"}`), nil
	default:
		return s.respond(200, fmt.Sprintf(`{"name":%q,"solar_system_id":30000142,"type_id":35834}`, s.name)), nil
	}
}

func (s *structureAttemptTransport) attemptOrder() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.attempts...)
}

// buildStructureTestApp mirrors buildCorpTestApp but issues
// per-character bearer tokens so the transport can tell callers
// apart.
func buildStructureTestApp(t *testing.T, transport http.RoundTripper) (*Application, *db.Queries) {
	t.Helper()
	conn, pool, err := store.Open(context.Background(), pgtest.FreshDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close(); pool.Close() })
	queries := db.New(conn)

	sessionManager := scs.New()
	sessionManager.Store = pgxstore.New(pool)
	sessionManager.Lifetime = 24 * time.Hour
	sessionManager.Cookie.Name = "evesynapse_session"

	client := esi.New(&http.Client{Transport: transport}, queries,
		func(_ context.Context, ch db.Character) (string, error) {
			return fmt.Sprintf("tok-%d", ch.CharacterID), nil
		})

	app := &Application{
		cfg:           Config{workerTiersOff: true},
		sessions:      sessionManager,
		queries:       queries,
		esi:           client,
		db:            conn,
		corpCache:     make(map[int64]corpCacheEntry),
		prices:        make(map[int64]esi.MarketPrice),
		priorityChars: make(map[int64]bool),
	}
	return app, queries
}

func scopedCharacter(t *testing.T, q *db.Queries, userID, characterID int64, name, updatedAt string) db.Character {
	t.Helper()
	ch := seedCharacter(t, q, userID, characterID, name)
	ch.Scopes = "esi-universe.read_structures.v1"
	ch.UpdatedAt = mustTime(updatedAt)
	return ch
}

func TestStructureMultiCharacterFirstSuccessWins(t *testing.T) {
	transport := &structureAttemptTransport{
		deny: map[int64]bool{fixtureCharA: true},
		name: "Perimeter >> Tranquility Trading Tower",
	}
	app, q := buildStructureTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Alpha is the most recently active, so Alpha is asked first —
	// and cannot name it. Bravo (a different user's character) can.
	alpha := scopedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "2026-06-01T00:00:00Z")
	bravo := scopedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo", "2026-01-01T00:00:00Z")

	const structureID = int64(1044752365771)
	app.noteStructureIDs(ctx, structureID)
	resolved, limited := app.resolveStructureNames(ctx, []db.Character{alpha, bravo}, &fetchBudget{left: 120})
	if limited || resolved != 1 {
		t.Fatalf("resolve: resolved=%d limited=%v, want 1/false", resolved, limited)
	}
	if got := transport.attemptOrder(); len(got) != 2 || got[0] != fixtureCharA || got[1] != fixtureCharB {
		t.Fatalf("attempt order: %v, want [%d %d]", got, fixtureCharA, fixtureCharB)
	}
	row, err := q.GetStructureName(ctx, structureID)
	if err != nil || row.State != esi.StructureResolved || row.Name != transport.name {
		t.Fatalf("resolved row: %+v err=%v", row, err)
	}
	if row.Source != esi.StructureSourceESI {
		t.Fatalf("resolved source: %q, want %q", row.Source, esi.StructureSourceESI)
	}
}

func TestStructureAllFailNegativeCachedAfterExhaustion(t *testing.T) {
	transport := &structureAttemptTransport{
		deny: map[int64]bool{fixtureCharA: true, fixtureCharB: true},
		name: "Never Seen",
	}
	app, q := buildStructureTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	alpha := scopedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "2026-06-01T00:00:00Z")
	bravo := scopedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo", "2026-01-01T00:00:00Z")

	const structureID = int64(1044752365999)
	app.noteStructureIDs(ctx, structureID)
	resolved, _ := app.resolveStructureNames(ctx, []db.Character{alpha, bravo}, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("all-deny resolve: resolved=%d, want 0", resolved)
	}
	if got := transport.attemptOrder(); len(got) != 2 {
		t.Fatalf("attempts: %v, want both characters tried", got)
	}
	row, err := q.GetStructureName(ctx, structureID)
	if err != nil || row.State != esi.StructureMissing {
		t.Fatalf("row after exhaustion: %+v err=%v, want missing", row, err)
	}
	// Fresh negative entry: the next pass asks nobody again.
	app.resolveStructureNames(ctx, []db.Character{alpha, bravo}, &fetchBudget{left: 120})
	if got := transport.attemptOrder(); len(got) != 2 {
		t.Fatalf("fresh negative was re-asked: attempts %v", got)
	}
}

func TestStructureTransientErrorBlocksNegativeCache(t *testing.T) {
	transport := &structureAttemptTransport{
		deny: map[int64]bool{fixtureCharA: true},
		fail: map[int64]bool{fixtureCharB: true},
		name: "Never Seen",
	}
	app, q := buildStructureTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	alpha := scopedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "2026-06-01T00:00:00Z")
	bravo := scopedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo", "2026-01-01T00:00:00Z")

	const structureID = int64(1044752365888)
	app.noteStructureIDs(ctx, structureID)
	resolved, _ := app.resolveStructureNames(ctx, []db.Character{alpha, bravo}, &fetchBudget{left: 120})
	if resolved != 0 {
		t.Fatalf("transient resolve: resolved=%d, want 0", resolved)
	}
	row, err := q.GetStructureName(ctx, structureID)
	if err != nil || row.State != esi.StructurePending {
		t.Fatalf("row after transient failure: %+v err=%v, want pending (no negative cache)", row, err)
	}
}

func TestStructureCorpTierResolvesWithoutESI(t *testing.T) {
	transport := &structureAttemptTransport{name: "Never Seen"}
	app, q := buildStructureTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	const structureID = int64(1044752365771)
	seedSnapshot(t, q, ch.CharacterID, esi.SnapCorpStructures, esi.CorpStructures{
		{StructureID: structureID, CorporationID: 777000, Name: "Corp Home", TypeID: 35834, SystemID: 30000142},
	})
	app.noteStructureIDs(ctx, structureID)
	// No ESI candidates at all: the corp list alone resolves it.
	resolved, _ := app.resolveStructureNames(ctx, nil, &fetchBudget{left: 120})
	if resolved != 1 {
		t.Fatalf("corp-tier resolve: resolved=%d, want 1", resolved)
	}
	if got := transport.attemptOrder(); len(got) != 0 {
		t.Fatalf("corp tier made %d ESI attempts, want 0", len(got))
	}
	row, err := q.GetStructureName(ctx, structureID)
	if err != nil || row.State != esi.StructureResolved || row.Name != "Corp Home" {
		t.Fatalf("corp-tier row: %+v err=%v", row, err)
	}
	if row.Source != esi.StructureSourceCorp {
		t.Fatalf("corp-tier source: %q, want %q", row.Source, esi.StructureSourceCorp)
	}
}

func TestStructureCorpMateAttemptedFirst(t *testing.T) {
	transport := &structureAttemptTransport{
		deny: map[int64]bool{fixtureCharB: true},
		name: "Dockable By Corp Mate",
	}
	app, q := buildStructureTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Alpha is more recently active but in another corp; Bravo is
	// in the owning corp, so Bravo is asked first despite that.
	alpha := scopedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha", "2026-06-01T00:00:00Z")
	bravo := scopedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo", "2026-01-01T00:00:00Z")
	for _, cc := range []struct {
		characterID, corporationID int64
	}{{fixtureCharA, 999000}, {fixtureCharB, 777000}} {
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
			CharacterID: cc.characterID, CorporationID: cc.corporationID, UpdatedAt: mustTime("2026-01-01T00:00:00Z"),
		}); err != nil {
			t.Fatalf("seed character corp: %v", err)
		}
	}

	const structureID = int64(1044752365771)
	// The corp list knows the owner but (deliberately) not the
	// name here, so tier 1 runs and its ordering is observable.
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpStructures, esi.CorpStructures{
		{StructureID: structureID, CorporationID: 777000, TypeID: 35834, SystemID: 30000142},
	})
	app.noteStructureIDs(ctx, structureID)
	resolved, _ := app.resolveStructureNames(ctx, []db.Character{alpha, bravo}, &fetchBudget{left: 120})
	if resolved != 1 {
		t.Fatalf("corp-mate resolve: resolved=%d, want 1", resolved)
	}
	if got := transport.attemptOrder(); len(got) != 2 || got[0] != fixtureCharB || got[1] != fixtureCharA {
		t.Fatalf("attempt order: %v, want corp-mate %d first", got, fixtureCharB)
	}
}

func TestStructureProvenancePrecedence(t *testing.T) {
	app, q := buildStructureTestApp(t, &structureAttemptTransport{})
	ctx := context.Background()
	const structureID = int64(1044752365771)
	stamp := time.Now().UTC()

	seed := func(name, source string) {
		t.Helper()
		if err := q.SetStructureName(ctx, db.SetStructureNameParams{
			StructureID: structureID, Name: name, State: esi.StructureResolved,
			ResolvedAt: timeSet(stamp), Source: source,
		}); err != nil {
			t.Fatalf("seed %s: %v", source, err)
		}
	}
	current := func() db.StructureName {
		t.Helper()
		row, err := q.GetStructureName(ctx, structureID)
		if err != nil {
			t.Fatalf("read row: %v", err)
		}
		return row
	}

	// A community name lands first (lowest trust)...
	seed("Community Guess", esi.StructureSourceCommunity)
	// ...a corp-list name outranks it...
	if !app.storeStructureName(ctx, structureID, "Corp Truth", esi.StructureResolved, esi.StructureSourceCorp, stamp) {
		t.Fatal("corp name should overwrite community")
	}
	if row := current(); row.Name != "Corp Truth" || row.Source != esi.StructureSourceCorp {
		t.Fatalf("after corp store: %+v", row)
	}
	// ...another community name cannot displace it...
	if app.storeStructureName(ctx, structureID, "Community Override", esi.StructureResolved, esi.StructureSourceCommunity, stamp) {
		t.Fatal("community name overwrote corp name")
	}
	if row := current(); row.Name != "Corp Truth" {
		t.Fatalf("after community attempt: %+v", row)
	}
	// ...the authenticated lookup outranks both...
	if !app.storeStructureName(ctx, structureID, "ESI Truth", esi.StructureResolved, esi.StructureSourceESI, stamp) {
		t.Fatal("ESI name should overwrite corp")
	}
	// ...and nothing lower-trust displaces ESI truth afterwards.
	if app.storeStructureName(ctx, structureID, "Corp Override", esi.StructureResolved, esi.StructureSourceCorp, stamp) {
		t.Fatal("corp name overwrote ESI name")
	}
	if row := current(); row.Name != "ESI Truth" || row.Source != esi.StructureSourceESI {
		t.Fatalf("final row: %+v", row)
	}
}
