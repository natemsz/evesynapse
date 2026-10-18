package app

// Tests for the public_contract swarm kind (swarm/share.go,
// swarm/trust.go): the shape gate's rule list, serve gates
// (stored and own and live only), demand from the pending
// queue, spot checks against the public items route, apply
// onto pending/absent/private rows, and stamp-guarded
// rollback.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/app/swarm"
	db "evesynapse/internal/db/sqlc"
)

// ptrToNull is the test file's own copy of the store's pointer
// conversion (the app's one is not visible from here in the
// shape this needs).
func ptrToNull(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// swarmContractFixture is one contract as the payload carries
// it and the seed writes it: the two must agree wherever a test
// compares them.
func swarmContractFixture(id int64, issued, expired time.Time) swarm.SwarmContractPayload {
	itemID := int64(123456)
	runs := int64(5)
	me := int64(10)
	te := int64(20)
	return swarm.SwarmContractPayload{
		ContractID: id, RegionID: 10000002, Type: "item_exchange",
		IssuerID: 90000001, IssuerCorporationID: 98000001,
		Title: "fixture", Price: 1000, Volume: 5,
		StartLocationID: 60003760,
		DateIssued:      issued.Unix(), DateExpired: expired.Unix(),
		Items: []swarm.SwarmContractItem{
			{RecordID: 1, TypeID: 34, Quantity: 100, IsIncluded: true},
			{RecordID: 2, TypeID: 587, ItemID: &itemID, Quantity: 1, IsIncluded: true,
				IsBlueprintCopy: true, Runs: &runs, MaterialEfficiency: &me, TimeEfficiency: &te},
		},
	}
}

func swarmContractJSON(t *testing.T, body swarm.SwarmContractPayload) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal contract payload: %v", err)
	}
	return string(raw)
}

// seedSwarmContract writes one header row through the sweep's
// own upsert, then sets its item state.
func seedSwarmContract(t *testing.T, app *Application, body swarm.SwarmContractPayload, state string, lastSeen time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := app.queries.UpsertPublicContract(ctx, db.UpsertPublicContractParams{
		ContractID: body.ContractID, RegionID: body.RegionID, Type: body.Type,
		IssuerID: body.IssuerID, IssuerCorporationID: body.IssuerCorporationID,
		ForCorporation: body.ForCorporation, Title: body.Title,
		Price: body.Price, Buyout: body.Buyout, Reward: body.Reward,
		Collateral: body.Collateral, Volume: body.Volume,
		StartLocationID: body.StartLocationID, EndLocationID: body.EndLocationID,
		DateIssued: time.Unix(body.DateIssued, 0).UTC(), DateExpired: time.Unix(body.DateExpired, 0).UTC(),
		DaysToComplete: body.DaysToComplete, LastCycle: 3, FirstSeen: lastSeen,
	}); err != nil {
		t.Fatalf("seed contract %d: %v", body.ContractID, err)
	}
	if err := app.queries.SetPublicContractItemsState(ctx, db.SetPublicContractItemsStateParams{
		ContractID: body.ContractID, ItemsState: state,
	}); err != nil {
		t.Fatalf("seed contract %d state: %v", body.ContractID, err)
	}
}

func seedSwarmContractItems(t *testing.T, app *Application, body swarm.SwarmContractPayload) {
	t.Helper()
	ctx := context.Background()
	for _, item := range body.Items {
		if err := app.queries.InsertPublicContractItem(ctx, db.InsertPublicContractItemParams{
			RecordID: item.RecordID, ContractID: body.ContractID, TypeID: item.TypeID,
			ItemID: ptrToNull(item.ItemID), Quantity: item.Quantity,
			IsIncluded: item.IsIncluded, IsBlueprintCopy: item.IsBlueprintCopy,
			Runs: ptrToNull(item.Runs), MaterialEfficiency: ptrToNull(item.MaterialEfficiency),
			TimeEfficiency: ptrToNull(item.TimeEfficiency),
		}); err != nil {
			t.Fatalf("seed contract item %d: %v", item.RecordID, err)
		}
	}
}

func TestSwarmContractPayloadShape(t *testing.T) {
	now := time.Now().UTC()
	base := swarmContractFixture(7001, now.Add(-time.Hour), now.Add(24*time.Hour))
	key := fmt.Sprintf("%d", base.ContractID)
	if err := swarm.CheckSwarmPayload(swarm.SwarmKindContract, key, swarmContractJSON(t, base)); err != nil {
		t.Fatalf("valid contract refused: %v", err)
	}
	cases := map[string]func(*swarm.SwarmContractPayload){
		"another contract":  func(b *swarm.SwarmContractPayload) { b.ContractID = 7002 },
		"no region":         func(b *swarm.SwarmContractPayload) { b.RegionID = 0 },
		"unknown type":      func(b *swarm.SwarmContractPayload) { b.Type = "loan" },
		"no issuer":         func(b *swarm.SwarmContractPayload) { b.IssuerID = 0 },
		"no issuer corp":    func(b *swarm.SwarmContractPayload) { b.IssuerCorporationID = 0 },
		"negative price":    func(b *swarm.SwarmContractPayload) { b.Price = -1 },
		"negative volume":   func(b *swarm.SwarmContractPayload) { b.Volume = -0.5 },
		"negative days":     func(b *swarm.SwarmContractPayload) { b.DaysToComplete = -1 },
		"expired at issued": func(b *swarm.SwarmContractPayload) { b.DateExpired = b.DateIssued },
		"title too long":    func(b *swarm.SwarmContractPayload) { b.Title = strings.Repeat("x", 1025) },
		"too many items": func(b *swarm.SwarmContractPayload) {
			b.Items = nil
			for i := 0; i <= swarm.MaxSwarmContractItems; i++ {
				b.Items = append(b.Items, swarm.SwarmContractItem{RecordID: int64(i + 1), TypeID: 34, Quantity: 1})
			}
		},
		"item record zero": func(b *swarm.SwarmContractPayload) { b.Items[0].RecordID = 0 },
		"item record dup":  func(b *swarm.SwarmContractPayload) { b.Items[1].RecordID = b.Items[0].RecordID },
		"item type zero":   func(b *swarm.SwarmContractPayload) { b.Items[0].TypeID = 0 },
		"item quantity 0":  func(b *swarm.SwarmContractPayload) { b.Items[0].Quantity = 0 },
	}
	for name, mutate := range cases {
		body := swarmContractFixture(7001, now.Add(-time.Hour), now.Add(24*time.Hour))
		mutate(&body)
		if err := swarm.CheckSwarmPayload(swarm.SwarmKindContract, key, swarmContractJSON(t, body)); err == nil {
			t.Fatalf("%s: malformed contract staged clean", name)
		}
	}
	if err := swarm.CheckSwarmPayload(swarm.SwarmKindContract, "7002", swarmContractJSON(t, base)); err == nil {
		t.Fatalf("payload under another key staged clean")
	}
	if !swarm.ValidSwarmKind(swarm.SwarmKindContract) {
		t.Fatalf("public_contract is not a valid swarm kind")
	}
}

func TestSwarmContractServe(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	issued, expires := now.Add(-time.Hour), now.Add(24*time.Hour)

	// Stored, own, live: served, stamped at the header's last_seen.
	own := swarmContractFixture(7101, issued, expires)
	seedSwarmContract(t, app, own, "stored", now.Add(-10*time.Minute))
	seedSwarmContractItems(t, app, own)
	payload, fetchedAt, ok := swarm.StateFor(app).ServeSwarmKey(ctx, swarm.SwarmKindContract, "7101")
	if !ok {
		t.Fatalf("stored own contract not served")
	}
	if !fetchedAt.Equal(now.Add(-10 * time.Minute)) {
		t.Fatalf("serve stamp %v, want the header last_seen %v", fetchedAt, now.Add(-10*time.Minute))
	}
	var served swarm.SwarmContractPayload
	if err := json.Unmarshal([]byte(payload), &served); err != nil {
		t.Fatalf("served payload: %v", err)
	}
	if served.ContractID != 7101 || len(served.Items) != 2 || served.DateIssued != issued.Unix() || served.DateExpired != expires.Unix() {
		t.Fatalf("served payload %+v", served)
	}
	if served.Items[0].ItemID != nil {
		t.Fatalf("non-singleton item served with item_id %v, want null", *served.Items[0].ItemID)
	}
	if served.Items[1].ItemID == nil || *served.Items[1].ItemID != 123456 || served.Items[1].Runs == nil || *served.Items[1].Runs != 5 {
		t.Fatalf("singleton item served wrong: %+v", served.Items[1])
	}

	// Stored but swarm-supplied: a miss — a peer's row is never
	// re-served under this instance's signature.
	supplied := swarmContractFixture(7102, issued, expires)
	stamp := now.Add(-20 * time.Minute)
	seedSwarmContract(t, app, supplied, "stored", stamp)
	seedSwarmContractItems(t, app, supplied)
	if err := app.queries.PutSwarmApplied(ctx, db.PutSwarmAppliedParams{
		Kind: swarm.SwarmKindContract, Key: "7102", Source: "peer-a", Signer: "peer-a",
		FetchedAt: stamp, AppliedAt: now, Verified: true,
		RowStamp: sql.NullTime{Time: stamp, Valid: true},
	}); err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if _, _, ok := swarm.StateFor(app).ServeSwarmKey(ctx, swarm.SwarmKindContract, "7102"); ok {
		t.Fatalf("swarm-supplied contract re-served")
	}

	// Expired and pending rows are misses.
	dead := swarmContractFixture(7103, now.Add(-48*time.Hour), now.Add(-time.Hour))
	seedSwarmContract(t, app, dead, "stored", now.Add(-30*time.Minute))
	seedSwarmContractItems(t, app, dead)
	if _, _, ok := swarm.StateFor(app).ServeSwarmKey(ctx, swarm.SwarmKindContract, "7103"); ok {
		t.Fatalf("expired contract served")
	}
	pending := swarmContractFixture(7104, issued, expires)
	seedSwarmContract(t, app, pending, "pending", now.Add(-5*time.Minute))
	if _, _, ok := swarm.StateFor(app).ServeSwarmKey(ctx, swarm.SwarmKindContract, "7104"); ok {
		t.Fatalf("pending contract served")
	}
}

func TestSwarmContractDemandIncludesPending(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	pending := swarmContractFixture(7201, now.Add(-time.Hour), now.Add(time.Hour))
	seedSwarmContract(t, app, pending, "pending", now)
	stored := swarmContractFixture(7202, now.Add(-time.Hour), now.Add(time.Hour))
	seedSwarmContract(t, app, stored, "stored", now)
	found := false
	for _, k := range swarm.StateFor(app).SwarmDemandKeys(ctx) {
		if k.Kind == swarm.SwarmKindContract && k.Key == "7201" {
			found = true
		}
		if k.Kind == swarm.SwarmKindContract && k.Key == "7202" {
			t.Fatalf("stored contract demanded")
		}
	}
	if !found {
		t.Fatalf("pending contract missing from demand")
	}
}

// swarmContractESIItems is the fixture's item list as ESI's
// public items route serves it.
const swarmContractESIItems = `[{"record_id":1,"type_id":34,"quantity":100,"is_included":true,"is_blueprint_copy":false},{"record_id":2,"type_id":587,"item_id":123456,"quantity":1,"is_included":true,"is_blueprint_copy":true,"runs":5,"material_efficiency":10,"time_efficiency":20}]`

// stageContractOnB stages one signed contract entry on the
// pair's B side, from A's identity, for a pending header B
// already holds.
func stageContractOnB(t *testing.T, p *swarmPair, body swarm.SwarmContractPayload) time.Time {
	t.Helper()
	ctx := context.Background()
	seedSwarmContract(t, p.b, body, "pending", time.Now().UTC())
	now := time.Now().UTC()
	entry := signedBy(t, swarm.StateFor(p.a).SwarmID, swarm.SwarmKindContract,
		fmt.Sprintf("%d", body.ContractID), swarmContractJSON(t, body), now)
	if err := swarm.StateFor(p.b).StageSwarmEntry(ctx, entry, "instance-a", now); err != nil {
		t.Fatalf("stage contract: %v", err)
	}
	return now
}

func TestSwarmContractCheckPassApplies(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	p.pinA(t)
	now := time.Now().UTC()
	body := swarmContractFixture(7301, now.Add(-time.Hour), now.Add(24*time.Hour))
	p.stubB.contractItems = map[int64]string{7301: swarmContractESIItems}
	stageContractOnB(t, p, body)
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d, want 1", got)
	}
	row, err := p.b.queries.GetPublicContract(ctx, 7301)
	if err != nil || row.ItemsState != "stored" {
		t.Fatalf("applied contract: %+v err %v, want items stored", row, err)
	}
	items, err := p.b.queries.ListPublicContractItems(ctx, 7301)
	if err != nil || len(items) != 2 {
		t.Fatalf("applied items: %d err %v, want 2", len(items), err)
	}
	if items[0].ItemID.Valid || !items[1].ItemID.Valid || items[1].ItemID.Int64 != 123456 {
		t.Fatalf("applied item IDs: %+v", items)
	}
	prov, err := p.b.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindContract, Key: "7301"})
	if err != nil || !prov.RowStamp.Valid || !prov.RowStamp.Time.Equal(row.LastSeen) {
		t.Fatalf("provenance %+v err %v, want its stamp on the row's last_seen", prov, err)
	}
	if counters := swarm.StateFor(p.b).SwarmCounterMap(ctx); counters[swarm.SwarmCounterAppliedContracts] != 1 {
		t.Fatalf("applied contracts counter %d, want 1", counters[swarm.SwarmCounterAppliedContracts])
	}
}

func TestSwarmContractCheckMismatchDiscards(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	p.pinA(t)
	now := time.Now().UTC()
	body := swarmContractFixture(7302, now.Add(-time.Hour), now.Add(24*time.Hour))
	// ESI's truth has one more Tritanium than the entry claims.
	p.stubB.contractItems = map[int64]string{7302: strings.Replace(swarmContractESIItems, `"quantity":100`, `"quantity":101`, 1)}
	stageContractOnB(t, p, body)
	if got := p.spotRuns(t); got != 0 {
		t.Fatalf("applied %d, want 0 for a mismatched entry", got)
	}
	if staged := stagedKeys(t, p.b); staged[swarm.SwarmKindContract+"/7302"] != "" {
		t.Fatalf("mismatched entry still staged")
	}
	row, err := p.b.queries.GetPublicContract(ctx, 7302)
	if err != nil || row.ItemsState != "pending" {
		t.Fatalf("contract after discard: %+v err %v, want still pending", row, err)
	}
	if items, err := p.b.queries.ListPublicContractItems(ctx, 7302); err != nil || len(items) != 0 {
		t.Fatalf("discarded entry wrote %d items", len(items))
	}
}

func TestSwarmContractVerifyDeferAndFail(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	body := swarmContractFixture(7303, now.Add(-time.Hour), now.Add(24*time.Hour))
	payload := swarmContractJSON(t, body)
	verify := func() swarm.SwarmCheckVerdict {
		t.Helper()
		verdict, _, stop := swarm.StateFor(p.b).SwarmVerifyAgainstESI(ctx, &fetchBudget{left: 5}, nil, swarm.SwarmKindContract, "7303", payload)
		if stop {
			t.Fatalf("canned ESI reported the error limit")
		}
		return verdict
	}
	// No stub entry: the route 404s — the contract ended, which
	// convicts nobody; the entry defers.
	if got := verify(); got != swarm.SwarmCheckDefer {
		t.Fatalf("404 verdict %d, want Defer", got)
	}
	// A 400 is the route refusing the contents as not public:
	// that is a wrong entry, and it fails.
	p.stubB.contractStatus = map[int64]int{7303: 400}
	if got := verify(); got != swarm.SwarmCheckFail {
		t.Fatalf("400 verdict %d, want Fail", got)
	}
	// Matching contents pass.
	p.stubB.contractStatus = nil
	p.stubB.contractItems = map[int64]string{7303: swarmContractESIItems}
	if got := verify(); got != swarm.SwarmCheckPass {
		t.Fatalf("matching verdict %d, want Pass", got)
	}
}

func TestSwarmContractApplyOntoPrivateConsumes(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	body := swarmContractFixture(7401, now.Add(-time.Hour), now.Add(24*time.Hour))
	seedSwarmContract(t, app, body, "private", now.Add(-time.Hour))
	changed := body
	changed.Title = "peer title"
	changed.Price = 9999
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindContract, "7401",
		swarmContractJSON(t, changed),
		swarm.SwarmOrigin{Source: "peer-a", Signer: "peer-a", FetchedAt: now, Verified: true}, now); err != nil {
		t.Fatalf("apply onto private: %v", err)
	}
	row, err := app.queries.GetPublicContract(ctx, 7401)
	if err != nil || row.ItemsState != "private" || row.Title != "fixture" || row.Price != 1000 {
		t.Fatalf("private row rewritten by apply: %+v err %v", row, err)
	}
	if items, err := app.queries.ListPublicContractItems(ctx, 7401); err != nil || len(items) != 0 {
		t.Fatalf("apply onto private wrote %d items", len(items))
	}
	if _, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindContract, Key: "7401"}); err == nil {
		t.Fatalf("consumed entry left provenance")
	}
	if counters := swarm.StateFor(app).SwarmCounterMap(ctx); counters[swarm.SwarmCounterAppliedContracts] != 0 {
		t.Fatalf("consumed entry counted as applied")
	}
}

func TestSwarmContractApplyNoRowAndRollback(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	stamp := now.Add(-time.Hour).Truncate(time.Microsecond)
	body := swarmContractFixture(7501, now.Add(-2*time.Hour), now.Add(24*time.Hour))
	origin := swarm.SwarmOrigin{Source: "peer-a", Signer: "peer-a", FetchedAt: stamp, Verified: true}
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindContract, "7501",
		swarmContractJSON(t, body), origin, now); err != nil {
		t.Fatalf("apply with no row: %v", err)
	}
	row, err := app.queries.GetPublicContract(ctx, 7501)
	if err != nil || row.ItemsState != "stored" || !row.LastSeen.Equal(stamp) {
		t.Fatalf("applied row: %+v err %v, want stored at the entry stamp", row, err)
	}
	if items, err := app.queries.ListPublicContractItems(ctx, 7501); err != nil || len(items) != 2 {
		t.Fatalf("applied items: %d err %v, want 2", len(items), err)
	}
	prov, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindContract, Key: "7501"})
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}

	// Rollback takes the swarm's copy back: header and items.
	took, err := swarm.StateFor(app).SwarmRollBackOne(ctx, prov)
	if err != nil || !took {
		t.Fatalf("rollback: took %v err %v, want true", took, err)
	}
	if _, err := app.queries.GetPublicContract(ctx, 7501); err == nil {
		t.Fatalf("contract survived its rollback")
	}
	if items, err := app.queries.ListPublicContractItems(ctx, 7501); err != nil || len(items) != 0 {
		t.Fatalf("rollback left %d items", len(items))
	}

	// Applied again, then re-seen by this instance's own sweep:
	// the stamp no longer matches, and rollback refuses — the
	// row is this instance's own now.
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindContract, "7501",
		swarmContractJSON(t, body), origin, now); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	seedSwarmContract(t, app, body, "stored", now)
	took, err = swarm.StateFor(app).SwarmRollBackOne(ctx, prov)
	if err != nil || took {
		t.Fatalf("rollback of own row: took %v err %v, want false", took, err)
	}
	if _, err := app.queries.GetPublicContract(ctx, 7501); err != nil {
		t.Fatalf("own contract removed by rollback: %v", err)
	}
	if items, err := app.queries.ListPublicContractItems(ctx, 7501); err != nil || len(items) != 2 {
		t.Fatalf("own contract's items after refused rollback: %d err %v, want 2", len(items), err)
	}
}
