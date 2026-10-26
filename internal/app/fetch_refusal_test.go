package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// refusalStubTransport answers the orders endpoint with a 401
// naming the missing scope while refuse is set, and with an empty
// order list once it clears.
type refusalStubTransport struct {
	calls  atomic.Int64
	refuse atomic.Bool
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
	if req.URL.Path == fmt.Sprintf("/characters/%d/orders/", fixtureCharA) {
		if s.refuse.Load() {
			return s.respond(http.StatusUnauthorized, `{"error":"Unauthorized - Token is not valid for any required scope: esi-markets.read_character_orders.v1"}`)
		}
		return s.respond(http.StatusOK, `[]`)
	}
	return s.respond(http.StatusNotFound, `{"error":"unexpected path `+req.URL.Path+`"}`)
}

// TestFetchRefusalSettlesAndRelinkClears proves the shared refusal
// policy on the economy path: a 401 settles the (character, kind)
// after one fetch, the next pass spends no call on it, and a
// re-link clears the record so the kind fetches again.
func TestFetchRefusalSettlesAndRelinkClears(t *testing.T) {
	transport := &refusalStubTransport{}
	transport.refuse.Store(true)
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	// First pass: one fetch, refused, recorded.
	if got := app.fetchCharKind(ctx, ch, esi.SnapOrders); got == corpFetchOK {
		t.Fatal("refused fetch reported OK")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("first pass made %d calls, want 1", got)
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
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("settled pass made %d more calls, want 0", got-1)
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
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("post-relink pass made %d calls in total, want 2", got)
	}
	if _, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapOrders}); err != nil {
		t.Fatalf("orders snapshot after re-link fetch: %v", err)
	}
}
