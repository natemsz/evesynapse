package app

// Public contracts (schema 055): the sweep stores the list,
// fetches each contract's items exactly once (a courier's
// refusal included), retires contracts on expiry and on two
// consecutive absences -- never one -- and the dogma probe
// turns singleton items into the abyssal corpus, sightings
// and all, exactly once per instance.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

// pubFixture is a scripted ESI for the public contract
// routes and the dogma dynamic route. Tests rewrite its
// pages between passes to move the universe forward.
type pubFixture struct {
	mu       sync.Mutex
	pages    map[int64][]esi.PublicContract // region -> its whole current list (served one page of 2 per request when long)
	pageSize int
	items    map[int64][]esi.PublicContractItem
	itemsErr map[int64]int // contract -> forced HTTP status on the items route
	bids     map[int64][]esi.PublicContractBid
	dynamic  map[int64]esi.DynamicItem // item_id -> dynamic payload (absent = 404)

	listCalls  map[int64]int
	itemsCalls map[int64]int
	bidsCalls  map[int64]int
	dogmaCalls map[int64]int
}

func newPubFixture() *pubFixture {
	return &pubFixture{
		pages:      map[int64][]esi.PublicContract{},
		pageSize:   2,
		items:      map[int64][]esi.PublicContractItem{},
		itemsErr:   map[int64]int{},
		bids:       map[int64][]esi.PublicContractBid{},
		dynamic:    map[int64]esi.DynamicItem{},
		listCalls:  map[int64]int{},
		itemsCalls: map[int64]int{},
		bidsCalls:  map[int64]int{},
		dogmaCalls: map[int64]int{},
	}
}

func pubResponse(status int, body []byte, pages int) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	if pages > 0 {
		header.Set("X-Pages", fmt.Sprint(pages))
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

func (f *pubFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := req.URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")

	// /contracts/public/{region}/?page=N
	if len(parts) == 3 && parts[0] == "contracts" && parts[1] == "public" {
		var regionID int64
		fmt.Sscanf(parts[2], "%d", &regionID)
		page := 1
		fmt.Sscanf(req.URL.Query().Get("page"), "%d", &page)
		f.listCalls[regionID]++
		all := f.pages[regionID]
		totalPages := (len(all) + f.pageSize - 1) / f.pageSize
		if totalPages == 0 {
			totalPages = 1
		}
		start := (page - 1) * f.pageSize
		var slice []esi.PublicContract
		if start < len(all) {
			end := start + f.pageSize
			if end > len(all) {
				end = len(all)
			}
			slice = all[start:end]
		}
		if slice == nil {
			slice = []esi.PublicContract{}
		}
		body, _ := json.Marshal(slice)
		return pubResponse(http.StatusOK, body, totalPages), nil
	}
	// /contracts/public/items/{id}/ and /contracts/public/bids/{id}/
	if len(parts) == 4 && parts[0] == "contracts" && parts[1] == "public" {
		var id int64
		fmt.Sscanf(parts[3], "%d", &id)
		switch parts[2] {
		case "items":
			f.itemsCalls[id]++
			if status, forced := f.itemsErr[id]; forced {
				return pubResponse(status, []byte(`{"error":"fixture refusal"}`), 0), nil
			}
			items := f.items[id]
			if items == nil {
				items = []esi.PublicContractItem{}
			}
			body, _ := json.Marshal(items)
			return pubResponse(http.StatusOK, body, 0), nil
		case "bids":
			f.bidsCalls[id]++
			bids := f.bids[id]
			if bids == nil {
				bids = []esi.PublicContractBid{}
			}
			body, _ := json.Marshal(bids)
			return pubResponse(http.StatusOK, body, 0), nil
		}
	}
	// /dogma/dynamic/items/{type}/{item}/
	if len(parts) == 5 && parts[0] == "dogma" && parts[1] == "dynamic" {
		var itemID int64
		fmt.Sscanf(parts[4], "%d", &itemID)
		f.dogmaCalls[itemID]++
		if dyn, ok := f.dynamic[itemID]; ok {
			body, _ := json.Marshal(dyn)
			return pubResponse(http.StatusOK, body, 0), nil
		}
		return pubResponse(http.StatusNotFound, []byte(`{"error":"not dynamic"}`), 0), nil
	}
	return pubResponse(http.StatusNotFound, []byte(`{"error":"unrouted fixture path `+path+`"}`), 0), nil
}

func (f *pubFixture) calls() (list, items, bids, dogma map[int64]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := func(m map[int64]int) map[int64]int {
		out := make(map[int64]int, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return cp(f.listCalls), cp(f.itemsCalls), cp(f.bidsCalls), cp(f.dogmaCalls)
}

func pubContractJSON(id, region int64, typ string, price float64, expires time.Time) esi.PublicContract {
	return esi.PublicContract{
		ContractID: id, IssuerID: 90001, IssuerCorporationID: 98001,
		Type: typ, Title: "fixture", Price: price,
		StartLocationID: 60003760, EndLocationID: 60003760,
		DateIssued:  time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		DateExpired: expires.Format(time.RFC3339),
	}
}

func seedPubRegions(t *testing.T, app *Application, regionIDs ...int64) {
	t.Helper()
	ctx := context.Background()
	for _, id := range regionIDs {
		if _, err := app.db.ExecContext(ctx,
			`INSERT INTO sde_regions (region_id, name) VALUES ($1, $2)`, id, fmt.Sprint(id)); err != nil {
			t.Fatalf("seed region %d: %v", id, err)
		}
	}
}

// agePubFetchStates makes every market fetch-state row old
// enough that the next pass finds every gate due again -- the
// test's way of moving the 30-minute cadence forward.
func agePubFetchStates(t *testing.T, app *Application) {
	t.Helper()
	if _, err := app.db.ExecContext(context.Background(),
		`UPDATE market_fetch_state SET attempted_at = attempted_at - INTERVAL '31 minutes'`); err != nil {
		t.Fatalf("age fetch states: %v", err)
	}
}

func runPubPass(t *testing.T, app *Application) {
	t.Helper()
	allowance := &fetchBudget{left: pubContractFetchesPerCycle}
	if _, limited := app.refreshPublicContracts(context.Background(), allowance); limited {
		t.Fatal("pass cut short by the fixture's error limit")
	}
}

func TestPublicContractsSweepDeathAndItems(t *testing.T) {
	fx := newPubFixture()
	app, _, q := buildCorpTestApp(t, fx)
	ctx := context.Background()
	seedPubRegions(t, app, 10000002, 10000047)
	now := time.Now().UTC()

	bpcRuns, bpcME, bpcTE := int64(10), int64(5), int64(8)
	c1 := pubContractJSON(101, 10000002, "item_exchange", 5000000, now.Add(7*24*time.Hour))
	c2 := pubContractJSON(102, 10000002, "auction", 0, now.Add(2*24*time.Hour))
	c2.Buyout = 9000000
	c3 := pubContractJSON(103, 10000002, "courier", 0, now.Add(24*time.Hour))
	c3.Reward, c3.Collateral, c3.Volume = 1000000, 50000000, 12000
	c4 := pubContractJSON(104, 10000002, "auction", 0, now.Add(30*24*time.Hour))
	c4.Buyout = 20000000
	c5 := pubContractJSON(105, 10000002, "item_exchange", 1000, now.Add(-time.Hour)) // already expired
	fx.pages[10000002] = []esi.PublicContract{c1, c2, c3, c4, c5}
	fx.pages[10000047] = []esi.PublicContract{}

	fx.items[101] = []esi.PublicContractItem{
		{RecordID: 1001, TypeID: 4242, ItemID: 9001, Quantity: 1, IsIncluded: true},
		{RecordID: 1002, TypeID: 34, Quantity: 100, IsIncluded: true},
		{RecordID: 1003, TypeID: 999, Quantity: 1, IsIncluded: true,
			IsBlueprintCopy: true, Runs: &bpcRuns, MaterialEfficiency: &bpcME, TimeEfficiency: &bpcTE},
		{RecordID: 1004, TypeID: 44992, Quantity: 2500, IsIncluded: false}, // the want side
	}
	fx.items[104] = []esi.PublicContractItem{
		{RecordID: 1005, TypeID: 4243, ItemID: 9002, Quantity: 1, IsIncluded: true},
	}
	fx.itemsErr[103] = http.StatusBadRequest // courier cargo is private
	fx.bids[102] = []esi.PublicContractBid{
		{BidID: 501, Amount: 1000000, DateBid: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		{BidID: 502, Amount: 1500000, DateBid: now.Add(-time.Hour).Format(time.RFC3339)},
	}
	fx.bids[104] = []esi.PublicContractBid{
		{BidID: 601, Amount: 900000, DateBid: now.Add(-3 * time.Hour).Format(time.RFC3339)},
		{BidID: 602, Amount: 1200000, DateBid: now.Add(-time.Hour).Format(time.RFC3339)},
	}
	fx.dynamic[9001] = esi.DynamicItem{
		CreatedBy: 90001, SourceTypeID: 1111, MutatorTypeID: 2222,
		DogmaAttributes: []esi.DynamicItemAttribute{{AttributeID: 54, Value: 28750}},
	}
	fx.dynamic[9002] = esi.DynamicItem{
		CreatedBy: 90001, SourceTypeID: 1112, MutatorTypeID: 2223,
		DogmaAttributes: []esi.DynamicItemAttribute{{AttributeID: 54, Value: 25000}},
	}

	// ---- Pass 1: the corpus is born.
	runPubPass(t, app)

	if n, err := q.CountPublicContracts(ctx); err != nil || n != 4 {
		t.Fatalf("contracts after pass 1 = %d, %v; want 4 (the expired one already retired)", n, err)
	}
	got1, err := q.GetPublicContract(ctx, 101)
	if err != nil {
		t.Fatalf("contract 101: %v", err)
	}
	if got1.ItemsState != "stored" || got1.Price != 5000000 || got1.LastCycle != 1 {
		t.Fatalf("contract 101 = state %q price %v cycle %d", got1.ItemsState, got1.Price, got1.LastCycle)
	}
	items, err := q.ListPublicContractItems(ctx, 101)
	if err != nil || len(items) != 4 {
		t.Fatalf("items of 101 = %d, %v; want 4", len(items), err)
	}
	if !items[0].ItemID.Valid || items[0].ItemID.Int64 != 9001 {
		t.Fatalf("singleton row item_id = %+v, want 9001", items[0].ItemID)
	}
	if !items[2].IsBlueprintCopy || !items[2].Runs.Valid || items[2].Runs.Int64 != 10 ||
		items[2].MaterialEfficiency.Int64 != 5 || items[2].TimeEfficiency.Int64 != 8 {
		t.Fatalf("BPC row = %+v, want copy runs 10 ME 5 TE 8", items[2])
	}
	if items[3].IsIncluded {
		t.Fatal("want-side row stored as included")
	}
	courier, err := q.GetPublicContract(ctx, 103)
	if err != nil || courier.ItemsState != "private" {
		t.Fatalf("courier items_state = %q, %v; want private", courier.ItemsState, err)
	}
	if n, _ := q.CountPublicContractItems(ctx); n != 5 {
		t.Fatalf("item rows = %d, want 5 (4 on 101, 1 on 104, none on the courier)", n)
	}
	bids, err := q.ListPublicContractBids(ctx, 102)
	if err != nil || len(bids) != 2 || bids[0].Amount != 1500000 {
		t.Fatalf("bids of 102 = %+v, %v; want 2, highest first", bids, err)
	}
	ab, err := q.GetAbyssalItem(ctx, 9001)
	if err != nil {
		t.Fatalf("abyssal item 9001: %v", err)
	}
	if ab.SourceTypeID != 1111 || ab.MutatorTypeID != 2222 || !strings.Contains(string(ab.Attributes), "28750") {
		t.Fatalf("abyssal item 9001 = %+v", ab)
	}
	sightings, err := q.ListAbyssalSightingsForItem(ctx, 9001)
	if err != nil || len(sightings) != 1 || sightings[0].ContractID != 101 ||
		sightings[0].Price != 5000000 || sightings[0].RegionID != 10000002 || sightings[0].DepartedAt.Valid {
		t.Fatalf("sightings of 9001 = %+v, %v; want one live sighting on 101", sightings, err)
	}
	list1, items1, bids1, dogma1 := fx.calls()
	if items1[101] != 1 || items1[103] != 1 || bids1[102] != 1 || dogma1[9001] != 1 || dogma1[9002] != 1 {
		t.Fatalf("pass 1 fetch counts: items %v bids %v dogma %v", items1, bids1, dogma1)
	}
	_ = list1

	// ---- Pass 2, no aging: the cadence gate holds everything.
	runPubPass(t, app)
	list2, items2, bids2, dogma2 := fx.calls()
	if fmt.Sprint(list2) != fmt.Sprint(list1) || fmt.Sprint(items2) != fmt.Sprint(items1) ||
		fmt.Sprint(bids2) != fmt.Sprint(bids1) || fmt.Sprint(dogma2) != fmt.Sprint(dogma1) {
		t.Fatalf("ungated pass fetched: list %v->%v items %v->%v bids %v->%v dogma %v->%v",
			list1, list2, items1, items2, bids1, bids2, dogma1, dogma2)
	}

	// ---- Pass 3 (cycle 2): 104 vanishes from the list. One
	// absence is not a death.
	fx.pages[10000002] = []esi.PublicContract{c1, c2, c3}
	agePubFetchStates(t, app)
	runPubPass(t, app)
	if _, err := q.GetPublicContract(ctx, 104); err != nil {
		t.Fatalf("contract 104 died on a single absence: %v", err)
	}

	// ---- Pass 4 (cycle 3): 104 absent again -- it leaves,
	// as a vanishing; 101's expiry passes while it is still
	// listed -- it leaves as an expiry. Their items and bids
	// go with them; the corpus keeps its rows and sightings.
	c1Expired := c1
	c1Expired.DateExpired = now.Add(-time.Hour).Format(time.RFC3339)
	fx.pages[10000002] = []esi.PublicContract{c1Expired, c2, c3}
	agePubFetchStates(t, app)
	runPubPass(t, app)

	if _, err := q.GetPublicContract(ctx, 104); err == nil {
		t.Fatal("contract 104 survived two consecutive absences")
	}
	if _, err := q.GetPublicContract(ctx, 101); err == nil {
		t.Fatal("contract 101 survived its expiry")
	}
	if n, _ := q.CountPublicContracts(ctx); n != 2 {
		t.Fatalf("contracts after retirements = %d, want 2 (auction + courier)", n)
	}
	if n, _ := q.CountPublicContractItems(ctx); n != 0 {
		t.Fatalf("item rows after retirements = %d, want 0", n)
	}
	if _, err := q.GetAbyssalItem(ctx, 9002); err != nil {
		t.Fatalf("abyssal corpus lost item 9002 with its contract: %v", err)
	}
	// The vanished auction's exit record keeps its term and
	// its bid state at exit: it left early, with two bids on
	// it, the top one 1.2M -- facts the deleted contract and
	// bid rows can no longer tell anyone.
	s2, err := q.ListAbyssalSightingsForItem(ctx, 9002)
	if err != nil || len(s2) != 1 || !s2[0].DepartedAt.Valid ||
		!s2[0].DepartKind.Valid || s2[0].DepartKind.String != "vanished" {
		t.Fatalf("sighting of 9002 = %+v, %v; want departed as vanished", s2, err)
	}
	if s2[0].ContractType != "auction" || !s2[0].BidCount.Valid || s2[0].BidCount.Int64 != 2 ||
		!s2[0].TopBid.Valid || s2[0].TopBid.Float64 != 1200000 {
		t.Fatalf("sighting of 9002 exit facts = type %q bids %+v top %+v; want auction, 2 bids, top 1.2M",
			s2[0].ContractType, s2[0].BidCount, s2[0].TopBid)
	}
	if !s2[0].DateExpired.Valid || !s2[0].DateExpired.Time.Equal(now.Add(30*24*time.Hour).Truncate(time.Second)) {
		t.Fatalf("sighting of 9002 term = %+v; want the listing's expiry kept", s2[0].DateExpired)
	}
	if s2[0].DepartedAt.Time.After(s2[0].DateExpired.Time) {
		t.Fatal("vanished sighting departed after its own term: kind should have been expired")
	}
	s1, err := q.ListAbyssalSightingsForItem(ctx, 9001)
	if err != nil || len(s1) != 1 || !s1[0].DepartKind.Valid || s1[0].DepartKind.String != "expired" {
		t.Fatalf("sighting of 9001 = %+v, %v; want departed as expired", s1, err)
	}
	if s1[0].ContractType != "item_exchange" || !s1[0].BidCount.Valid || s1[0].BidCount.Int64 != 0 || s1[0].TopBid.Valid {
		t.Fatalf("sighting of 9001 exit facts = type %q bids %+v top %+v; want exchange, 0 bids, no top bid",
			s1[0].ContractType, s1[0].BidCount, s1[0].TopBid)
	}

	// ---- Pass 5: item 9001 is relisted on a new contract.
	// The corpus links the two listings by instance: a second
	// sighting opens for the same item, after the first one's
	// departure.
	c6 := pubContractJSON(106, 10000002, "item_exchange", 6500000, now.Add(30*24*time.Hour))
	fx.pages[10000002] = []esi.PublicContract{c2, c3, c6}
	fx.items[106] = []esi.PublicContractItem{
		{RecordID: 1006, TypeID: 4242, ItemID: 9001, Quantity: 1, IsIncluded: true},
	}
	agePubFetchStates(t, app)
	runPubPass(t, app)

	s1, err = q.ListAbyssalSightingsForItem(ctx, 9001)
	if err != nil || len(s1) != 2 {
		t.Fatalf("sightings of 9001 after relist = %d, %v; want 2 (expired listing + relist)", len(s1), err)
	}
	if s1[1].ContractID != 106 || s1[1].DepartedAt.Valid || s1[1].Price != 6500000 {
		t.Fatalf("relist sighting = %+v; want a live sighting on 106 at 6.5M", s1[1])
	}
	if s1[1].FirstSeenAt.Before(s1[0].DepartedAt.Time) {
		t.Fatal("relist sighting opens before the first listing departed: sequence broken")
	}
}

func TestPublicContractProbePlainItemOnce(t *testing.T) {
	fx := newPubFixture()
	app, _, q := buildCorpTestApp(t, fx)
	ctx := context.Background()
	seedPubRegions(t, app, 10000002)
	now := time.Now().UTC()

	c := pubContractJSON(201, 10000002, "item_exchange", 100, now.Add(24*time.Hour))
	fx.pages[10000002] = []esi.PublicContract{c}
	fx.items[201] = []esi.PublicContractItem{
		{RecordID: 2001, TypeID: 606, ItemID: 9100, Quantity: 1, IsIncluded: true}, // an ordinary singleton: 404
	}
	runPubPass(t, app)

	var isDynamic bool
	if err := app.db.QueryRowContext(ctx,
		`SELECT is_dynamic FROM abyssal_probes WHERE item_id = 9100`).Scan(&isDynamic); err != nil {
		t.Fatalf("probe row for 9100: %v", err)
	}
	if isDynamic {
		t.Fatal("ordinary item probed as dynamic")
	}
	if n, _ := q.CountAbyssalItems(ctx); n != 0 {
		t.Fatalf("abyssal items = %d, want 0", n)
	}
	_, _, _, dogma1 := fx.calls()
	agePubFetchStates(t, app)
	runPubPass(t, app)
	_, _, _, dogma2 := fx.calls()
	if dogma2[9100] != dogma1[9100] || dogma2[9100] != 1 {
		t.Fatalf("item 9100 probed %d times, want exactly 1", dogma2[9100])
	}
}

func TestPubContractRegionOrderHubsFirst(t *testing.T) {
	got := pubContractRegionOrder([]int64{10000047, 10000030, 10000001, 10000002, 10000042})
	want := []int64{10000002, 10000030, 10000042, 10000001, 10000047}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("region order = %v, want %v", got, want)
	}
	ids := make([]int64, 0, len(got))
	ids = append(ids, got...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if fmt.Sprint(ids) != fmt.Sprint([]int64{10000001, 10000002, 10000030, 10000042, 10000047}) {
		t.Fatal("region order lost or duplicated a region")
	}
}
