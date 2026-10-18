package app

// Tests for signed swarm pulls (swarm/api.go): a pull proves
// itself with the puller's own key on top of the shared-secret
// bearer, and the server counts which way it let each served
// pull in. A failed signature falls back to the bearer and is
// never a trust verdict — authentication is not data.

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

	counters := func() (int64, int64) {
		t.Helper()
		m := swarm.StateFor(app).SwarmCounterMap(ctx)
		return m[swarm.SwarmCounterPullsAuthSigned], m[swarm.SwarmCounterPullsAuthLegacy]
	}

	// A valid signature serves with no bearer at all, and is
	// counted as signed.
	code, resp := swarmPullServe(t, app, body, swarmPullHeaders(puller, body, now), "")
	if code != http.StatusOK || !strings.Contains(resp, "Tritanium") {
		t.Fatalf("signed pull: status %d body %q, want 200 with the name", code, resp)
	}
	if signed, legacy := counters(); signed != 1 || legacy != 0 {
		t.Fatalf("counters after signed pull: signed %d legacy %d, want 1/0", signed, legacy)
	}

	// A bad signature (another key signing under the puller's
	// name) with a valid bearer still serves, counted as legacy,
	// and the failed signature costs the peer nothing: no check
	// mark, no shape reject.
	other, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint other: %v", err)
	}
	other.ID = puller.ID
	code, _ = swarmPullServe(t, app, body, swarmPullHeaders(other, body, now), swarmTestSecret)
	if code != http.StatusOK {
		t.Fatalf("bad signature with bearer: status %d, want 200", code)
	}
	if signed, legacy := counters(); signed != 1 || legacy != 1 {
		t.Fatalf("counters after legacy fallback: signed %d legacy %d, want 1/1", signed, legacy)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, puller.ID)
	if err != nil || peer.GoodChecks != 0 || peer.BadChecks != 0 {
		t.Fatalf("auth failure touched the ledger: %+v err %v", peer, err)
	}

	// The same bad signature without a bearer is refused, and
	// refused pulls count nowhere.
	code, _ = swarmPullServe(t, app, body, swarmPullHeaders(other, body, now), "")
	if code != http.StatusForbidden {
		t.Fatalf("bad signature without bearer: status %d, want 403", code)
	}
	if signed, legacy := counters(); signed != 1 || legacy != 1 {
		t.Fatalf("counters after refused pull: signed %d legacy %d, want 1/1", signed, legacy)
	}

	// An instance this server has never pinned cannot sign its
	// way in, bearer or none — signed-only here, so 403.
	stranger, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint stranger: %v", err)
	}
	stranger.ID = "peer-unknown"
	code, _ = swarmPullServe(t, app, body, swarmPullHeaders(stranger, body, now), "")
	if code != http.StatusForbidden {
		t.Fatalf("unknown instance signed-only: status %d, want 403", code)
	}

	// A stale timestamp makes the signature invalid even from
	// the right key: alone it is refused, with a bearer it
	// serves as legacy.
	stale := now - int64(time.Hour/time.Second)
	code, _ = swarmPullServe(t, app, body, swarmPullHeaders(puller, body, stale), "")
	if code != http.StatusForbidden {
		t.Fatalf("stale signature without bearer: status %d, want 403", code)
	}
	code, _ = swarmPullServe(t, app, body, swarmPullHeaders(puller, body, stale), swarmTestSecret)
	if code != http.StatusOK {
		t.Fatalf("stale signature with bearer: status %d, want 200", code)
	}
	if signed, legacy := counters(); signed != 1 || legacy != 2 {
		t.Fatalf("counters after stale fallback: signed %d legacy %d, want 1/2", signed, legacy)
	}

	// An excluded peer's valid signature is not valid at all:
	// exclusion closes the signed path like it closes pulls.
	excluded := swarmTestPeer(t, app, "peer-excluded")
	if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: excluded.ID, Excluded: true, ExcludeNote: "manual: test"}); err != nil {
		t.Fatalf("exclude peer: %v", err)
	}
	code, _ = swarmPullServe(t, app, body, swarmPullHeaders(excluded, body, now), "")
	if code != http.StatusForbidden {
		t.Fatalf("excluded peer signed-only: status %d, want 403", code)
	}
	if signed, legacy := counters(); signed != 1 || legacy != 2 {
		t.Fatalf("counters after excluded pull: signed %d legacy %d, want 1/2", signed, legacy)
	}
}

// TestSwarmPullClientDualSend: a client carrying its instance
// identity signs its pulls (counted signed at the server) and
// still sends the bearer; the same client without an identity
// pulls the legacy way. End to end over the pair rig.
func TestSwarmPullClientDualSend(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	if err := p.a.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	// A pins B the way gossip would, so it can verify B's pull
	// signatures under the key B signs with.
	now := time.Now().UTC()
	if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "instance-b", BaseUrl: "http://127.0.0.1:9", PublicKey: swarm.StateFor(p.b).SwarmID.PublicString(),
		Source: "manual", FirstSeen: now,
	}); err != nil {
		t.Fatalf("pin B on A: %v", err)
	}
	keys := []swarm.SwarmPullKey{{Kind: swarm.SwarmKindTypeName, Key: "34"}}

	swarm.StateFor(p.b).SwarmCli.ID = swarm.StateFor(p.b).SwarmID
	entries, err := swarm.StateFor(p.b).SwarmCli.PullEntries(ctx, p.urlA, keys)
	if err != nil || len(entries) != 1 {
		t.Fatalf("signed client pull: entries %d err %v, want 1", len(entries), err)
	}
	counters := swarm.StateFor(p.a).SwarmCounterMap(ctx)
	if counters[swarm.SwarmCounterPullsAuthSigned] != 1 || counters[swarm.SwarmCounterPullsAuthLegacy] != 0 {
		t.Fatalf("A counters after signed pull: %+v, want signed 1 legacy 0", counters)
	}

	swarm.StateFor(p.b).SwarmCli.ID = nil
	entries, err = swarm.StateFor(p.b).SwarmCli.PullEntries(ctx, p.urlA, keys)
	if err != nil || len(entries) != 1 {
		t.Fatalf("bearer-only client pull: entries %d err %v, want 1", len(entries), err)
	}
	counters = swarm.StateFor(p.a).SwarmCounterMap(ctx)
	if counters[swarm.SwarmCounterPullsAuthSigned] != 1 || counters[swarm.SwarmCounterPullsAuthLegacy] != 1 {
		t.Fatalf("A counters after legacy pull: %+v, want signed 1 legacy 1", counters)
	}
}
