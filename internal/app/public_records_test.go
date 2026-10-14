package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestPublicRecordsAnonymous: the public record pages and the topbar
// search serve visitors without a session from stored rows only. A
// record the instance holds (its own fetch, or swarm data it applied:
// both land in the same tables) renders; a record that is only staged
// from a peer is never shown, and a miss renders the not-yet-known
// state with its lookup queued for the worker. Nothing asks ESI.
func TestPublicRecordsAnonymous(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	const held, staged, unknown = int64(98000001), int64(98000002), int64(98000003)

	payload := func(name, ticker string) string {
		raw, _ := json.Marshal(corporationRecordPayload{Corp: esi.Corporation{Name: name, Ticker: ticker}})
		return string(raw)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: held, Payload: payload("Held Holdings", "HELD"), State: orgStateReady,
		FetchedAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	// A peer's entry waiting in staging: not yet verified, never fact.
	if err := q.StageSwarmEntry(ctx, db.StageSwarmEntryParams{
		Kind: swarmKindCorporation, Key: strconv.FormatInt(staged, 10), Payload: payload("Staged Shadow", "STG"),
		Signer: "peer", Signature: "sig", FetchedAt: now, ReceivedAt: now, Source: "pull",
	}); err != nil {
		t.Fatalf("stage entry: %v", err)
	}

	page := func(path string) string {
		t.Helper()
		code, body := getSwapPage(t, app, nil, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s (anonymous): status %d, want 200", path, code)
		}
		return body
	}
	mustContain(t, "held corporation", page("/corporation/?corporation="+strconv.FormatInt(held, 10)), "Held Holdings")
	for _, id := range []int64{staged, unknown} {
		body := page("/corporation/?corporation=" + strconv.FormatInt(id, 10))
		if strings.Contains(body, "Staged Shadow") {
			t.Fatal("a staged, unverified swarm entry was served")
		}
		// The miss is queued for the worker, as a signed-in view queues it.
		rec, err := q.GetCorporationRecord(ctx, id)
		if err != nil || rec.State != orgStatePending {
			t.Errorf("corporation %d: record %+v (%v), want a pending lookup", id, rec, err)
		}
	}
	page("/pilot/?character=" + strconv.FormatInt(unknown, 10))
	if rec, err := q.GetPilotRecord(ctx, unknown); err != nil || rec.State != pilotStatePending {
		t.Errorf("pilot %d: record %+v (%v), want a pending lookup", unknown, rec, err)
	}
	page("/alliance/?alliance=" + strconv.FormatInt(unknown, 10))
	page("/system/?system=30000142")
	page("/station/?station=60003760")
	page("/structure/?structure=1000000000001")
	page("/labels/corporation-fragment?id=" + strconv.FormatInt(held, 10))
	mustContain(t, "/search.json", page("/search.json?q=Held"), `"Held Holdings"`)

	if n := transport.calls.Load(); n != 0 {
		t.Errorf("anonymous record renders made %d outbound calls, want 0", n)
	}
}
