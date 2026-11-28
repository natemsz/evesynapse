package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestCycleSnapshotCacheSharesOneRead: under a cycle context the
// stored snapshot behind a (character, kind) is read and decoded
// once however many passes ask, proved by deleting the stored
// row after the first read: the cycle still serves the shared
// decode, while a cache-free read no longer finds anything.
func TestCycleSnapshotCacheSharesOneRead(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Cache Tester")
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 11, TypeID: 34}, {OrderID: 12, TypeID: 35},
	})

	cycleCtx := withCycleSnapshotCache(ctx)
	orders, ok := app.loadOrdersSnapshot(cycleCtx, fixtureCharA)
	if !ok || len(orders) != 2 {
		t.Fatalf("first read: ok=%v, %d orders", ok, len(orders))
	}

	if _, err := conn.ExecContext(ctx, `DELETE FROM character_snapshots WHERE character_id = $1 AND kind = $2`, fixtureCharA, esi.SnapOrders); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}

	again, ok := app.loadOrdersSnapshot(cycleCtx, fixtureCharA)
	if !ok || len(again) != 2 {
		t.Fatalf("cycle read after delete: ok=%v, %d orders", ok, len(again))
	}
	viaGeneric, ok := loadSnapshot[[]esi.CharOrder](app, cycleCtx, fixtureCharA, esi.SnapOrders)
	if !ok || len(viaGeneric) != 2 {
		t.Fatalf("generic cycle read after delete: ok=%v, %d orders", ok, len(viaGeneric))
	}
	if _, ok := app.snapshotPayload(cycleCtx, fixtureCharA, esi.SnapOrders); !ok {
		t.Fatal("cycle payload read after delete: not served by the shared read")
	}

	if _, ok := app.loadOrdersSnapshot(ctx, fixtureCharA); ok {
		t.Fatal("a cache-free read saw the deleted snapshot")
	}
	if _, ok := app.snapshotPayload(ctx, fixtureCharA, esi.SnapOrders); ok {
		t.Fatal("a cache-free payload read saw the deleted snapshot")
	}
}

// TestWarmContractItemsSkipsStoredDetails: a contract whose item
// list is stored already, by any character's list, since the
// detail store is keyed by contract, is never fetched again;
// one that is not stored is fetched for (and here stays unstored:
// the transport refuses everything).
func TestWarmContractItemsSkipsStoredDetails(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	chA := seedCharacter(t, q, user.ID, fixtureCharA, "Contract Tester A")
	seedCharacter(t, q, user.ID, fixtureCharB, "Contract Tester B")

	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, esi.Contracts{
		{ContractID: 101, Status: "outstanding"},
		{ContractID: 102, Status: "outstanding"},
	})
	now := time.Now().UTC()
	// 101 stored from A's own list, 102 from B's: both count.
	for _, stored := range []db.UpsertContractDetailParams{
		{ContractID: 101, CharacterID: fixtureCharA, Payload: `[]`, FetchedAt: now},
		{ContractID: 102, CharacterID: fixtureCharB, Payload: `[]`, FetchedAt: now},
	} {
		if err := q.UpsertContractDetail(ctx, stored); err != nil {
			t.Fatalf("seed contract detail: %v", err)
		}
	}

	if fetched, limited := app.warmContractItems(ctx, chA); fetched != 0 || limited {
		t.Fatalf("warm over stored details: fetched=%d limited=%v", fetched, limited)
	}
	if n := transport.calls.Load(); n != 0 {
		t.Fatalf("stored contracts were fetched again: %d call(s)", n)
	}

	// An unstored contract joins the list: it is fetched for.
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, esi.Contracts{
		{ContractID: 101, Status: "outstanding"},
		{ContractID: 102, Status: "outstanding"},
		{ContractID: 103, Status: "outstanding"},
	})
	if fetched, _ := app.warmContractItems(ctx, chA); fetched != 0 {
		t.Fatalf("warm with a failing transport stored %d", fetched)
	}
	if n := transport.calls.Load(); n == 0 {
		t.Fatal("the unstored contract was never fetched for")
	}
	if _, err := q.GetContractDetail(ctx, 103); err == nil {
		t.Fatal("contract 103 was stored despite the failed fetch")
	}
}

// conStopSpammingTransport answers every call the way the EVE server
// does when one character asks for contract items too fast.
type conStopSpammingTransport struct{ calls atomic.Int64 }

func (s *conStopSpammingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return &http.Response{
		StatusCode: 520,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"ConStopSpamming, details: {\"remainingTime\": 14385554}"}`)),
	}, nil
}

// TestWarmContractItemsStopsOnServerError: a 5xx such as 520
// ConStopSpamming ends the character's item pass for the cycle. Each
// failed call spends ESI error budget, and the next contract would
// get the same answer.
func TestWarmContractItemsStopsOnServerError(t *testing.T) {
	transport := &conStopSpammingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	chA := seedCharacter(t, q, user.ID, fixtureCharA, "Contract Tester A")
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, esi.Contracts{
		{ContractID: 201, Status: "outstanding"},
		{ContractID: 202, Status: "outstanding"},
		{ContractID: 203, Status: "outstanding"},
	})
	if fetched, limited := app.warmContractItems(ctx, chA); fetched != 0 || limited {
		t.Fatalf("warm against 520s: fetched=%d limited=%v", fetched, limited)
	}
	if n := transport.calls.Load(); n != 1 {
		t.Fatalf("asked %d times after a 520, want 1", n)
	}
}

// TestWarmCorpContractItemsOneCharacterPerCorporation: characters of
// one corporation share its contract list, so within a cycle only one
// of them asks the corporation's items endpoint, and a 520 from it
// keeps every character of that corporation off it past the cycle.
func TestWarmCorpContractItemsOneCharacterPerCorporation(t *testing.T) {
	const corpID = 98000077
	ctx := context.Background()
	setup := func(transport http.RoundTripper) (*Application, db.Character, db.Character) {
		app, _, q := buildCorpTestApp(t, transport)
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		chA := seedCharacter(t, q, user.ID, fixtureCharA, "Corp Contracts A")
		chB := seedCharacter(t, q, user.ID, fixtureCharB, "Corp Contracts B")
		for _, id := range []int64{fixtureCharA, fixtureCharB} {
			if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: id, CorporationID: corpID, UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("seed corporation: %v", err)
			}
			seedSnapshot(t, q, id, esi.SnapCorpContracts, esi.Contracts{
				{ContractID: 301, Status: "outstanding"},
				{ContractID: 302, Status: "outstanding"},
			})
		}
		return app, chA, chB
	}

	refusing := &countingTransport{}
	app, chA, chB := setup(refusing)
	app.warmCorpContractItems(ctx, chA)
	asked := refusing.calls.Load()
	if asked == 0 {
		t.Fatal("the first character never asked for the corporation's items")
	}
	app.warmCorpContractItems(ctx, chB)
	if n := refusing.calls.Load(); n != asked {
		t.Fatalf("a second character of the corporation asked again in the same cycle: %d calls, want %d", n, asked)
	}

	spam := &conStopSpammingTransport{}
	app, chA, _ = setup(spam)
	app.warmCorpContractItems(ctx, chA)
	if n := spam.calls.Load(); n != 1 {
		t.Fatalf("asked %d times after a 520, want 1", n)
	}
	if app.corpContractWarm.claim(corpID, time.Now().Add(2*corpContractWarmEvery)) {
		t.Fatal("the corporation was open to warming again a cycle after a 520")
	}
}
