package app

// Hermetic tests for the economy cluster (module sweep, cluster 3),
// following corp_pages_test.go's two-layer approach:
//
//   - Render tests: seeded economy snapshots + a contract_details
//     row drive the real chi router with a counting stub transport,
//     proving every new page renders fixture content, that a
//     recorded 403 explains itself, and that no handler makes an
//     outbound call.
//   - Worker tests: a path-routing stub transport stands in for
//     ESI, proving refreshEconomySnapshots stores the bounded
//     wallet windows, warmContractItems stores contract details,
//     a 403 is recorded without writing a snapshot, and a second
//     pass settles to zero calls.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func TestEconomyPagesRenderFromSnapshots(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	recently := now.Add(-time.Hour).Format(time.RFC3339)

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")

	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18), (1230, 'Veldspar', 0), (990001, 'Fixture Blueprint', 0), (990002, 'Fixture Copy Blueprint', 0)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}

	// Wallet: balance + one journal line + one transaction.
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, `1234567.89`)
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletJournal, []esi.WalletJournalEntry{
		{ID: 1, Date: recently, RefType: "market_transaction", Amount: -5500, Balance: 1234567.89, FirstPartyID: fixtureCharA, SecondPartyID: fixtureMember, Description: "Tritanium purchase"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWalletTxns, []esi.WalletTransaction{
		{TransactionID: 9, Date: recently, TypeID: 34, LocationID: 60003760, UnitPrice: 5.5, Quantity: 1000, ClientID: fixtureMember, IsBuy: true},
	})

	// Orders: one open, one expired.
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, []esi.CharOrder{
		{OrderID: 1, TypeID: 587, LocationID: 60003760, RegionID: 10000002, IsBuyOrder: true, Price: 300000, VolumeTotal: 5, VolumeRemain: 3, Range: "station", Issued: now.AddDate(0, 0, -10).Format(time.RFC3339), Duration: 90},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrdersHistory, []esi.CharOrderHistoryEntry{
		{CharOrder: esi.CharOrder{OrderID: 2, TypeID: 34, LocationID: 60003760, RegionID: 10000002, Price: 5.5, VolumeTotal: 1000, VolumeRemain: 400, Issued: now.AddDate(0, 0, -40).Format(time.RFC3339)}, State: "expired"},
	})

	// Contracts: one item exchange with warmed items, one courier.
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{
		{ContractID: 555, IssuerID: fixtureMember, AssigneeID: fixtureCharA, Type: "item_exchange", Status: "outstanding", Title: "Fixture Deal", Price: 1000000, DateIssued: recently, DateExpired: now.Add(24 * time.Hour).Format(time.RFC3339)},
		{ContractID: 556, IssuerID: fixtureCharA, Type: "courier", Status: "in_progress", Reward: 500000, Collateral: 2000000, StartLocationID: 60003760, EndLocationID: 60003760, DateIssued: recently, DateExpired: now.Add(48 * time.Hour).Format(time.RFC3339)},
	})
	if err := q.UpsertContractDetail(ctx, db.UpsertContractDetailParams{
		ContractID:  555,
		CharacterID: fixtureCharA,
		Payload:     `[{"record_id":1,"type_id":34,"quantity":100,"raw_quantity":100,"is_included":true,"is_singleton":false},{"record_id":2,"type_id":990001,"quantity":1,"raw_quantity":-1,"is_included":true,"is_singleton":true}]`,
		FetchedAt:   "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed contract detail: %v", err)
	}

	// Industry: one active manufacturing job, a BPO + a BPC, mining.
	seedSnapshot(t, q, fixtureCharA, esi.SnapIndustryJobs, []esi.IndustryJob{
		{JobID: 1, ActivityID: 1, BlueprintTypeID: 990001, ProductTypeID: 587, FacilityID: 60003760, Runs: 5, Status: "active", StartDate: recently, EndDate: now.Add(2 * time.Hour).Format(time.RFC3339), Cost: 12345.67},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapBlueprints, []esi.Blueprint{
		{ItemID: 1, TypeID: 990001, LocationID: 60003760, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
		{ItemID: 2, TypeID: 990002, LocationID: 60003760, LocationFlag: "Hangar", MaterialEfficiency: 8, TimeEfficiency: 14, Quantity: -2, Runs: 5},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMining, []esi.MiningEntry{
		{Date: "2026-09-30", TypeID: 1230, SolarSystemID: 30000142, Quantity: 5000},
	})

	app.esi.StoreCharacterName(fixtureCharA, "Fixture Ceo")
	app.esi.StoreCharacterName(fixtureMember, "Member One")

	// Character B: journal refused (stale-scope 403) and an
	// item-exchange contract whose items have not warmed yet.
	if err := q.UpsertSnapshotFetchState(ctx, db.UpsertSnapshotFetchStateParams{
		CharacterID: fixtureCharB,
		Kind:        esi.SnapWalletJournal,
		State:       fetchStateError,
		Detail:      forbiddenDetailPrefix + " — this character's login predates the current scope list; sign in again to re-grant scopes.",
		AttemptedAt: now.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed fetch state: %v", err)
	}
	seedSnapshot(t, q, fixtureCharB, esi.SnapContracts, []esi.Contract{
		{ContractID: 777, IssuerID: fixtureCharB, Type: "item_exchange", Status: "outstanding", DateIssued: recently, DateExpired: now.Add(24 * time.Hour).Format(time.RFC3339)},
	})

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/wallet/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("wallet page: status %d", code)
	}
	mustContain(t, "/wallet/", body,
		"1,234,567.89", "market transaction", "-5,500.00", "Tritanium",
		"Member One", "Jita 4 - Moon 4 - Caldari Navy Assembly Plant", "Buy")

	code, body = getPage(t, app, cookie, "/orders/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("orders page: status %d", code)
	}
	mustContain(t, "/orders/", body,
		"Rifter", "300,000.00", "3 / 5", "The Forge", "Station", "Tritanium", "expired")

	code, body = getPage(t, app, cookie, "/contracts/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("contracts page: status %d", code)
	}
	mustContain(t, "/contracts/", body,
		"Fixture Deal", "item exchange", "outstanding", "Member One",
		"Tritanium × 100", "BPO", "courier", "Courier:")

	code, body = getPage(t, app, cookie, "/industry/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("industry page: status %d", code)
	}
	mustContain(t, "/industry/", body,
		"Manufacturing", "Fixture Blueprint", "Rifter", "active",
		"BPO", "∞", "10%", "BPC", "Veldspar", "Jita", "12,345.67")

	// Character B: the 403 explains itself; unwarmed items note.
	code, body = getPage(t, app, cookie, "/wallet/?character=90000002")
	if code != http.StatusOK {
		t.Fatalf("wallet page (B): status %d", code)
	}
	mustContain(t, "/wallet/ (B)", body, "ESI refused (403)", "sign in again")

	code, body = getPage(t, app, cookie, "/contracts/?character=90000002")
	if code != http.StatusOK {
		t.Fatalf("contracts page (B): status %d", code)
	}
	mustContain(t, "/contracts/ (B)", body, "Item details are still warming up.")

	// Warming copy where nothing has landed yet.
	code, body = getPage(t, app, cookie, "/industry/?character=90000002")
	if code != http.StatusOK {
		t.Fatalf("industry page (B): status %d", code)
	}
	mustContain(t, "/industry/ (B)", body, "Still warming up")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// econStubTransport serves the economy endpoints for the worker
// test. Journal page 1 has 150 entries with X-Pages: 2, page 2 has
// 200 more — the stored window must trim to 300 in 2 calls.
type econStubTransport struct {
	calls   atomic.Int64
	journal atomic.Int64
	txns    atomic.Int64
	forbid  atomic.Bool // contracts list answers 403 when set
}

func (s *econStubTransport) respond(status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

func journalPage(highID, count int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"date":"2026-09-30T00:00:00Z","ref_type":"bounty","description":"fixture"}`, highID-i)
	}
	b.WriteString("]")
	return b.String()
}

func (s *econStubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	path := req.URL.Path
	base := fmt.Sprintf("/characters/%d", fixtureCharA)
	switch {
	case path == base+"/wallet/journal/":
		s.journal.Add(1)
		resp, err := s.respond(http.StatusOK, "")
		if err != nil {
			return resp, err
		}
		if req.URL.Query().Get("page") == "2" {
			resp.Header.Set("X-Pages", "2")
			resp.Body = io.NopCloser(strings.NewReader(journalPage(1000, 200)))
		} else {
			resp.Header.Set("X-Pages", "2")
			resp.Body = io.NopCloser(strings.NewReader(journalPage(1150, 150)))
		}
		return resp, nil
	case path == base+"/wallet/transactions/":
		s.txns.Add(1)
		if req.URL.Query().Get("from_id") != "" {
			return s.respond(http.StatusOK, `[]`)
		}
		return s.respond(http.StatusOK, `[{"transaction_id":100,"date":"2026-09-30T00:00:00Z","type_id":34,"location_id":60003760,"unit_price":5.5,"quantity":10,"client_id":93300001,"is_buy":true,"is_personal":true,"journal_ref_id":1},{"transaction_id":99,"date":"2026-09-29T00:00:00Z","type_id":34,"location_id":60003760,"unit_price":5.4,"quantity":5,"client_id":93300001,"is_buy":false,"is_personal":true,"journal_ref_id":2}]`)
	case path == base+"/orders/":
		return s.respond(http.StatusOK, `[]`)
	case path == base+"/orders/history/":
		return s.respond(http.StatusOK, `[]`)
	case path == base+"/contracts/" && s.forbid.Load():
		return s.respond(http.StatusForbidden, `{"error":"forbidden"}`)
	case path == base+"/contracts/":
		return s.respond(http.StatusOK, `[{"contract_id":555,"issuer_id":90000001,"issuer_corporation_id":98000001,"assignee_id":0,"acceptor_id":0,"type":"item_exchange","status":"outstanding","availability":"public","for_corporation":false,"price":1000,"date_issued":"2026-09-30T00:00:00Z","date_expired":"2026-10-30T00:00:00Z"}]`)
	case path == base+"/contracts/555/items/":
		return s.respond(http.StatusOK, `[{"record_id":1,"type_id":34,"quantity":100,"raw_quantity":100,"is_included":true,"is_singleton":false}]`)
	case path == base+"/industry/jobs/":
		return s.respond(http.StatusOK, `[]`)
	case path == base+"/blueprints/":
		return s.respond(http.StatusOK, `[]`)
	case path == base+"/mining/":
		return s.respond(http.StatusOK, `[]`)
	default:
		return s.respond(http.StatusNotFound, `{"error":"unexpected path `+path+`"}`)
	}
}

// TestRefreshEconomySnapshots proves the worker contract: the
// journal window merges pages and trims to 300, contract items
// warm into the detail store, a 403 is recorded (no snapshot
// written), and a second pass with fresh snapshots is silent.
func TestRefreshEconomySnapshots(t *testing.T) {
	transport := &econStubTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	if got := app.refreshEconomySnapshots(ctx, ch); got != len(economySnapshotKinds()) {
		t.Fatalf("first pass: stored %d snapshots, want %d", got, len(economySnapshotKinds()))
	}
	if got := transport.journal.Load(); got != 2 {
		t.Fatalf("journal calls: got %d, want 2 (two pages)", got)
	}
	if got := transport.txns.Load(); got != 2 {
		t.Fatalf("transaction calls: got %d, want 2 (initial + one empty step)", got)
	}

	snap, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapWalletJournal})
	if err != nil {
		t.Fatalf("journal snapshot: %v", err)
	}
	var journal esi.WalletJournal
	if err := json.Unmarshal([]byte(snap.Payload), &journal); err != nil {
		t.Fatalf("decode journal snapshot: %v", err)
	}
	if len(journal) != 300 {
		t.Fatalf("journal window: got %d entries, want 300 (trimmed)", len(journal))
	}

	// Contract items warm into the detail store.
	fetched, limited := app.warmContractItems(ctx, ch)
	if limited || fetched != 1 {
		t.Fatalf("warm contract items: fetched=%d limited=%v, want 1/false", fetched, limited)
	}
	detail, err := q.GetContractDetail(ctx, 555)
	if err != nil {
		t.Fatalf("contract detail 555: %v", err)
	}
	items, ok, err := app.loadContractItems(ctx, 555)
	if err != nil || !ok || len(items) != 1 || items[0].TypeID != 34 {
		t.Fatalf("load contract items: %+v ok=%v err=%v (detail %+v)", items, ok, err, detail)
	}

	// Second pass: everything fresh, detail stored — zero calls.
	before := transport.calls.Load()
	app.refreshEconomySnapshots(ctx, ch)
	if fetched, _ := app.warmContractItems(ctx, ch); fetched != 0 {
		t.Fatalf("second warm pass fetched %d, want 0", fetched)
	}
	if got := transport.calls.Load(); got != before {
		t.Fatalf("second pass made %d calls, want 0", got-before)
	}

	// A 403 on the contracts list is recorded state, not a
	// poisoned snapshot, and backs off on the next pass.
	transport.forbid.Store(true)
	for _, kind := range []string{esi.SnapContracts} {
		// Force staleness so the pass refetches.
		if err := q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
			CharacterID: fixtureCharA,
			Kind:        kind,
			Payload:     `[]`,
			FetchedAt:   "2026-01-01T00:00:00Z",
			CachedUntil: sql.NullString{String: "2026-01-01T00:00:00Z", Valid: true},
		}); err != nil {
			t.Fatalf("stale snapshot %s: %v", kind, err)
		}
	}
	app.refreshEconomySnapshots(ctx, ch)
	st, err := q.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: fixtureCharA, Kind: esi.SnapContracts})
	if err != nil {
		t.Fatalf("contracts fetch state: %v", err)
	}
	if st.State != fetchStateError || !strings.HasPrefix(st.Detail, forbiddenDetailPrefix) {
		t.Fatalf("contracts fetch state: got %q/%q, want error/%s…", st.State, st.Detail, forbiddenDetailPrefix)
	}
	snap, err = q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: fixtureCharA, Kind: esi.SnapContracts})
	if err != nil {
		t.Fatalf("contracts snapshot after 403: %v", err)
	}
	if snap.Payload != `[]` {
		t.Fatalf("contracts snapshot rewritten by a 403: %q", snap.Payload)
	}
	// The refused kind sits inside its backoff and the fresh
	// kinds skip: the next pass must not refetch the journal.
	app.refreshEconomySnapshots(ctx, ch)
	if got := transport.journal.Load(); got != 2 {
		t.Fatalf("journal refetched despite freshness/backoff: %d calls", got)
	}
}
