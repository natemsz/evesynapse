package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// refusalStubTransport answers the orders endpoint with a 401
// naming the missing scope while refuse is set, and with an empty
// order list once it clears. Every requested path is counted.
type refusalStubTransport struct {
	calls  atomic.Int64
	refuse atomic.Bool

	mu    sync.Mutex
	paths map[string]int
}

func (s *refusalStubTransport) respond(status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (s *refusalStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	s.mu.Lock()
	if s.paths == nil {
		s.paths = map[string]int{}
	}
	s.paths[req.URL.Path]++
	s.mu.Unlock()
	if req.URL.Path == fmt.Sprintf("/characters/%d/orders/", fixtureCharA) {
		if s.refuse.Load() {
			return s.respond(http.StatusUnauthorized, `{"error":"Unauthorized - Token is not valid for any required scope: esi-markets.read_character_orders.v1"}`)
		}
		return s.respond(http.StatusOK, `[]`)
	}
	return s.respond(http.StatusNotFound, `{"error":"unexpected path `+req.URL.Path+`"}`)
}

// callsTo reports how many requests hit paths containing substr.
func (s *refusalStubTransport) callsTo(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for path, count := range s.paths {
		if strings.Contains(path, substr) {
			n += count
		}
	}
	return n
}

// withGrants re-seeds a character holding exactly these scopes
// (the grantScopes helper in comms_mail_state_test.go) and returns
// the stored row.
func withGrants(t *testing.T, q *db.Queries, userID, characterID int64, name, scopes string) db.Character {
	t.Helper()
	grantScopes(t, q, userID, characterID, name, scopes)
	ch, err := q.GetCharacter(context.Background(), characterID)
	if err != nil {
		t.Fatalf("re-read character %d: %v", characterID, err)
	}
	return ch
}

// TestFetchRefusalSettlesAndRelinkClears proves the two layers of
// the refusal policy. Plan first: a kind whose required scope is
// not in the recorded grants is never requested at all. Backstop:
// a refusal for a kind the grants say is fetchable settles after
// one fetch, the next pass spends no call on it, and a re-link
// clears the record so the kind fetches again.
func TestFetchRefusalSettlesAndRelinkClears(t *testing.T) {
	transport := &refusalStubTransport{}
	transport.refuse.Store(true)
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// The manifest covers every kind the passes plan, including
	// the suffix-keyed families: a kind without an entry could
	// be planned without its scope ever being checked.
	planned := append([]string{}, coreSnapshotKinds...)
	planned = append(planned, economySnapshotKinds()...)
	planned = append(planned, commsSnapshotKinds()...)
	planned = append(planned, corpSnapshotKinds()...)
	planned = append(planned,
		esi.SnapPlanets, esi.PlanetLayoutKind(40100001),
		esi.MailBodyKind(501), esi.CalendarEventKind(88), esi.CalendarAttendeesKind(88))
	for _, kind := range planned {
		if _, known := kindRequiredScope(kind); !known {
			t.Errorf("kind %q has no entry in the kind-to-scope manifest", kind)
		}
	}

	// Plan exclusion, economy: grants name the wallet and skills
	// scopes only, so the pass must not request orders or
	// contracts at all (and must record nothing about them).
	chB := withGrants(t, q, user.ID, fixtureCharB, "Fixture Second",
		"esi-wallet.read_character_wallet.v1 esi-skills.read_skills.v1")
	app.refreshEconomySnapshots(ctx, chB)
	if n := transport.callsTo("/orders"); n != 0 {
		t.Fatalf("economy pass requested orders %d times for a login without the orders scope", n)
	}
	if n := transport.callsTo("/contracts"); n != 0 {
		t.Fatalf("economy pass requested contracts %d times for a login without the contracts scope", n)
	}
	if _, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: fixtureCharB, Kind: esi.SnapOrders}); !errorIsNoRows(err) {
		t.Fatalf("orders fetch state after plan exclusion: err=%v, want no row (exclusion records nothing)", err)
	}

	// Plan exclusion, corporation wallets: grants name the
	// membership scope only, so the corp pass must not request
	// any wallet path (balances or per-division journals).
	chC := withGrants(t, q, user.ID, fixtureMember, "Fixture Member",
		"esi-corporations.read_corporation_membership.v1")
	app.refreshCorpSnapshotsFor(ctx, chC, fixtureCorpA, true)
	if n := transport.callsTo("/wallets"); n != 0 {
		t.Fatalf("corp pass requested wallet paths %d times for a login without the corporation wallets scope", n)
	}
	if _, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: fixtureMember, Kind: esi.SnapCorpWallets}); !errorIsNoRows(err) {
		t.Fatalf("corp wallets fetch state after plan exclusion: err=%v, want no row", err)
	}

	// Backstop: character A has no recorded grants (unknown, so
	// nothing is excluded); ESI refuses orders anyway.
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	// First pass: one fetch, refused, recorded.
	if got := app.fetchCharKind(ctx, ch, esi.SnapOrders); got == corpFetchOK {
		t.Fatal("refused fetch reported OK")
	}
	if got := transport.callsTo("/orders/"); got != 1 {
		t.Fatalf("first pass made %d orders calls, want 1", got)
	}
	state, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: fixtureCharA, Kind: esi.SnapOrders})
	if err != nil {
		t.Fatalf("orders fetch state: %v", err)
	}
	if state.State != fetchStateError || !strings.HasPrefix(state.Detail, unauthorizedDetailPrefix) {
		t.Fatalf("orders fetch state: got %q/%q, want error/%s…", state.State, state.Detail, unauthorizedDetailPrefix)
	}
	if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapOrders}); !errorIsNoRows(err) {
		t.Fatalf("orders snapshot after 401: err=%v, want no row", err)
	}

	// Second pass: settled, no call spent.
	if got := app.fetchCharKind(ctx, ch, esi.SnapOrders); got != corpFetchSkipped {
		t.Fatalf("settled pass outcome = %v, want skipped", got)
	}
	if got := transport.callsTo("/orders/"); got != 1 {
		t.Fatalf("settled pass made %d orders calls in total, want 1", got)
	}

	// Re-link: the refusal record clears with the new grant.
	if _, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
		UserID:       user.ID,
		CharacterID:  fixtureCharA,
		Name:         "Fixture Ceo",
		Scopes:       "esi-markets.read_character_orders.v1",
		AccessToken:  "fixture",
		RefreshToken: "fixture",
		TokenExpiry:  mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("re-link: %v", err)
	}
	states, err := q.ListSnapshotFetchStatesByCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("list fetch states after re-link: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("re-link left %d fetch state rows, want 0", len(states))
	}

	// The kind fetches again and stores.
	transport.refuse.Store(false)
	if got := app.fetchCharKind(ctx, ch, esi.SnapOrders); got != corpFetchOK {
		t.Fatalf("post-relink outcome = %v, want OK", got)
	}
	if got := transport.callsTo("/orders/"); got != 2 {
		t.Fatalf("post-relink pass made %d orders calls in total, want 2", got)
	}
	if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapOrders}); err != nil {
		t.Fatalf("orders snapshot after re-link fetch: %v", err)
	}
}
