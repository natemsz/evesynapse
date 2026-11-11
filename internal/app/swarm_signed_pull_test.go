package app

// Tests for signed swarm pulls (swarm/api.go): a pull proves who
// sent it with the puller's own pinned key, and nothing else lets
// it in: a bearer alone is refused. A failed signature is never a
// trust verdict; authentication is not data.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/app/swarm"
	db "evesynapse/internal/db/sqlc"
)

// swarmPullServe posts one pull body with explicit headers and
// an optional bearer, the way swarmServe cannot express.
func swarmPullServe(t *testing.T, app *Application, body string, headers map[string]string, bearer string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/swarm/pull", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// swarmPullHeaders signs body as id at the given unix time.
func swarmPullHeaders(id *swarm.SwarmIdentity, body string, at int64) map[string]string {
	return map[string]string{
		"X-Swarm-Instance":  id.ID,
		"X-Swarm-Timestamp": strconv.FormatInt(at, 10),
		"X-Swarm-Signature": swarm.SignSwarmPull(id, at, []byte(body)),
	}
}

func TestSwarmPullSignedAuth(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	if err := app.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed name: %v", err)
	}
	puller := swarmTestPeer(t, app, "peer-puller")
	body := `{"keys":[{"kind":"type_name","key":"34"}]}`
	now := time.Now().Unix()
	signed := func() int64 {
		t.Helper()
		return swarm.StateFor(app).SwarmCounterMap(ctx)[swarm.SwarmCounterPullsAuthSigned]
	}

	// A valid signature serves, with no bearer at all, and is
	// counted.
	code, resp := swarmPullServe(t, app, body, swarmPullHeaders(puller, body, now), "")
	if code != http.StatusOK || !strings.Contains(resp, "Tritanium") {
		t.Fatalf("signed pull: status %d body %q, want 200 with the name", code, resp)
	}
	if n := signed(); n != 1 {
		t.Fatalf("signed pulls counted %d, want 1", n)
	}

	// A bearer proves only that the caller holds the secret: alone,
	// or beside a signature that does not verify, it is refused.
	other, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint other: %v", err)
	}
	other.ID = puller.ID
	stale := now - int64(time.Hour/time.Second)
	stranger, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint stranger: %v", err)
	}
	stranger.ID = "peer-unknown"
	excluded := swarmTestPeer(t, app, "peer-excluded")
	if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: excluded.ID, Excluded: true, ExcludeNote: "manual: test"}); err != nil {
		t.Fatalf("exclude peer: %v", err)
	}
	for _, c := range []struct {
		name    string
		headers map[string]string
	}{
		{"bearer alone", nil},
		{"another key under the puller's name", swarmPullHeaders(other, body, now)},
		{"a stale timestamp", swarmPullHeaders(puller, body, stale)},
		{"an instance never pinned", swarmPullHeaders(stranger, body, now)},
		{"an excluded peer", swarmPullHeaders(excluded, body, now)},
	} {
		if code, _ := swarmPullServe(t, app, body, c.headers, swarmTestSecret); code != http.StatusForbidden {
			t.Errorf("%s with the bearer: status %d, want 403", c.name, code)
		}
	}
	// Refused pulls count nowhere and cost the peer nothing.
	if n := signed(); n != 1 {
		t.Fatalf("signed pulls counted %d after refusals, want 1", n)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, puller.ID)
	if err != nil || peer.GoodChecks != 0 || peer.BadChecks != 0 {
		t.Fatalf("auth failure touched the ledger: %+v err %v", peer, err)
	}
}

// TestSwarmPullClientSigns: a client carrying its instance identity
// signs its pulls and is served; a client without one does not
// pull at all. End to end over the pair rig.
func TestSwarmPullClientSigns(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	if err := p.a.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	keys := []swarm.SwarmPullKey{{Kind: swarm.SwarmKindTypeName, Key: "34"}}

	swarm.StateFor(p.b).SwarmCli.ID = swarm.StateFor(p.b).SwarmID
	entries, err := swarm.StateFor(p.b).SwarmCli.PullEntries(ctx, p.urlA, keys)
	if err != nil || len(entries) != 1 {
		t.Fatalf("signed client pull: entries %d err %v, want 1", len(entries), err)
	}
	if n := swarm.StateFor(p.a).SwarmCounterMap(ctx)[swarm.SwarmCounterPullsAuthSigned]; n != 1 {
		t.Fatalf("A counted %d signed pulls, want 1", n)
	}

	swarm.StateFor(p.b).SwarmCli.ID = nil
	if _, err := swarm.StateFor(p.b).SwarmCli.PullEntries(ctx, p.urlA, keys); err == nil {
		t.Fatal("a client without an identity pulled")
	}
}
