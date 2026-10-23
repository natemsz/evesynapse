package app

import (
	"context"
	"database/sql"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// The learned ship pricing (truevalue_learn.go): the abyssal
// method adapted to hulls. Weights mirror the abyssal corpus;
// a hull prices from its family's learned correction where
// the model earns its place, else the family's bounded
// factor, else its own; a hull whose ladder is fiction or
// absent prices from a learned level instead; and a single
// sample never prices anything.
// ---------------------------------------------------------------------------

func tvLearnSample(price float64, source string, at time.Time) tvContractSample {
	return tvContractSample{price: price, at: at, source: source}
}

func TestTVSampleWeight(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	// Tier weights at age zero, mirroring the abyssal corpus:
	// a confirmed sale counts most, a vanished listing a
	// little more than a live ask.
	if w := tvSampleWeight(tvLearnSample(1, tvSampleFulfilled, now), now); w != abyssalSoldWeight {
		t.Errorf("sale weight = %v, want %v", w, abyssalSoldWeight)
	}
	if w := tvSampleWeight(tvLearnSample(1, tvSampleVanished, now), now); w != tvVanishedWeight {
		t.Errorf("vanished weight = %v, want %v", w, tvVanishedWeight)
	}
	if w := tvSampleWeight(tvLearnSample(1, tvSampleListing, now), now); w != 1 {
		t.Errorf("ask weight = %v, want 1", w)
	}
	// Age: each tier decays on its own curve -- the abyssal
	// shape, a longer memory. A live ask halves on the
	// abyssal 30 days but floors at 0.2 (twice the abyssal
	// floor); a likely sale halves every 60 days to 0.25; a
	// confirmed sale halves every 90 days and floors at 0.4,
	// so even fully aged it speaks at 2.5 x 0.4 = 1.0, a
	// fresh ask's full voice.
	day := 24 * time.Hour
	for _, tc := range []struct {
		source string
		age    time.Duration
		want   float64
	}{
		{tvSampleListing, 30 * day, 0.5},
		{tvSampleListing, 300 * day, tvAskFloor},
		{tvSampleVanished, 60 * day, tvVanishedWeight * 0.5},
		{tvSampleVanished, 240 * day, tvVanishedWeight * tvVanishedFloor},
		{tvSampleFulfilled, 90 * day, abyssalSoldWeight * 0.5},
		{tvSampleFulfilled, 360 * day, abyssalSoldWeight * tvSaleFloor},
	} {
		if w := tvSampleWeight(tvLearnSample(1, tc.source, now.Add(-tc.age)), now); math.Abs(w-tc.want) > 1e-9 {
			t.Errorf("%s at %v: weight = %v, want %v", tc.source, tc.age, w, tc.want)
		}
	}
}

// TestTVShipOwnFactor: a lone hull in its family prices from
// its own samples through the bounded factor, shrunk toward
// its ladder -- never the raw median, and never from one
// sample.
func TestTVShipOwnFactor(t *testing.T) {
	now := time.Now()
	subjects := map[int64]bool{1: true}
	ladders := map[int64]float64{1: 100}
	families := map[int64]string{1: "Fixture"}
	samples := map[int64][]tvContractSample{1: {
		tvLearnSample(110, tvSampleListing, now),
		tvLearnSample(130, tvSampleListing, now),
	}}
	p := buildTVShipPricing(subjects, ladders, samples, families, now)
	// Weighted median log ratio (the lower sample's,
	// log 1.1), shrunk 2/(2+6): 100 x exp(0.25 x log 1.1).
	est, source, count, ok := p.price(1)
	if !ok || math.Abs(est-102.411) > 0.01 || source != tvSampleListing || count != 2 {
		t.Errorf("own factor = (%v, %q, %d, %v), want (~102.411, listing, 2, true)", est, source, count, ok)
	}

	// One sample is an anecdote: no estimate.
	samples[1] = samples[1][:1]
	p = buildTVShipPricing(subjects, ladders, samples, families, now)
	if _, _, _, ok := p.price(1); ok {
		t.Error("a single sample priced the hull")
	}
	// No samples at all: no estimate either -- the ladder is
	// a cost, not a price.
	samples[1] = nil
	p = buildTVShipPricing(subjects, ladders, samples, families, now)
	if _, _, _, ok := p.price(1); ok {
		t.Error("a hull with no evidence priced from its ladder alone")
	}
}

// TestTVShipFamilyFactor: a family's pooled comps price
// every hull in it -- including one with a single sample of
// its own and one with none -- through the family factor.
func TestTVShipFamilyFactor(t *testing.T) {
	now := time.Now()
	subjects := map[int64]bool{1: true, 2: true, 3: true}
	ladders := map[int64]float64{1: 100, 2: 200, 3: 50}
	families := map[int64]string{1: "Fixture", 2: "Fixture", 3: "Fixture"}
	samples := map[int64][]tvContractSample{
		1: {tvLearnSample(120, tvSampleListing, now), tvLearnSample(120, tvSampleListing, now), tvLearnSample(120, tvSampleListing, now)},
		2: {tvLearnSample(240, tvSampleListing, now)},
	}
	p := buildTVShipPricing(subjects, ladders, samples, families, now)
	fam := p.families["Fixture"]
	if fam == nil || !fam.factorOK || fam.useModel {
		t.Fatalf("family fit = %+v, want a factor fit and no model", fam)
	}
	// Four rows at ratio 1.2: median log 1.2 shrunk 4/(4+6)
	// -> factor exp(0.4 x log 1.2) = 1.07566.
	for typeID, want := range map[int64]float64{1: 107.566, 2: 215.132, 3: 53.783} {
		est, source, count, ok := p.price(typeID)
		if !ok || math.Abs(est-want) > 0.01 || source != tvSampleListing || count != 4 {
			t.Errorf("price(%d) = (%v, %q, %d, %v), want (~%v, listing, 4, true)", typeID, est, source, count, ok, want)
		}
	}
}

// TestTVShipModelGate: where a family's premium over build
// cost moves with the hull's scale, the boosted model beats
// the constant factor in the backtest and earns its place;
// its corrections stay inside the band and rise with the
// ladder, as every split is constrained to.
func TestTVShipModelGate(t *testing.T) {
	now := time.Now()
	ladders := map[int64]float64{1: 100, 2: 200, 3: 400, 4: 800}
	subjects := map[int64]bool{1: true, 2: true, 3: true, 4: true}
	families := map[int64]string{1: "Fixture", 2: "Fixture", 3: "Fixture", 4: "Fixture"}
	samples := map[int64][]tvContractSample{}
	// Six asks per hull at a premium that grows with the
	// ladder (ratios 1, 1.55, 2.65, 4.85 -- inside the
	// sampler's band), interleaved in time so the holdout
	// spans every hull.
	ratio := func(ladder float64) float64 { return 1 + (ladder/100-1)*0.55 }
	for i := 0; i < 6; i++ {
		for _, id := range []int64{1, 2, 3, 4} {
			at := now.Add(-time.Duration(24-i*4-int(id)) * time.Hour)
			samples[id] = append(samples[id], tvLearnSample(ladders[id]*ratio(ladders[id]), tvSampleListing, at))
		}
	}
	p := buildTVShipPricing(subjects, ladders, samples, families, now)
	fam := p.families["Fixture"]
	if fam == nil || !fam.hasBacktest || !fam.useModel {
		t.Fatalf("family fit = %+v, want a backtested model in use", fam)
	}
	lo, _, _, okLo := p.price(1)
	hi, _, _, okHi := p.price(4)
	if !okLo || !okHi {
		t.Fatalf("model prices: hull 1 ok=%v, hull 4 ok=%v", okLo, okHi)
	}
	if hi/800 <= lo/100 {
		t.Errorf("correction does not rise with the ladder: hull 1 x%v, hull 4 x%v", lo/100, hi/800)
	}
	if hi > 800*tvLearnBandMax {
		t.Errorf("correction escaped the band: %v against ladder 800", hi)
	}
}

// TestTVShipOldEvidenceStillCounts: age is a weight, not a
// cutoff. Two confirmed sales from 45 days ago -- far past
// the tiered path's 14-day window -- still price their hull
// through its own factor, discounted by the decay; a single
// old sale still cannot.
func TestTVShipOldEvidenceStillCounts(t *testing.T) {
	now := time.Now()
	old := now.Add(-45 * 24 * time.Hour)
	subjects := map[int64]bool{1: true}
	ladders := map[int64]float64{1: 100}
	families := map[int64]string{1: "Fixture"}
	samples := map[int64][]tvContractSample{1: {
		tvLearnSample(130, tvSampleFulfilled, old),
		tvLearnSample(130, tvSampleFulfilled, old),
	}}
	p := buildTVShipPricing(subjects, ladders, samples, families, now)
	// Each sale weighs 2.5 x 0.5^(45/90) = 1.768 by now; the
	// effective count is still 2, so the factor is the fresh
	// one: 100 x exp(0.25 x log 1.3) = 106.779.
	est, source, count, ok := p.price(1)
	if !ok || math.Abs(est-106.779) > 0.05 || source != tvSampleFulfilled || count != 2 {
		t.Errorf("old sales = (%v, %q, %d, %v), want (~106.779, fulfilled, 2, true)", est, source, count, ok)
	}
	samples[1] = samples[1][:1]
	p = buildTVShipPricing(subjects, ladders, samples, families, now)
	if _, _, _, ok := p.price(1); ok {
		t.Error("a single 45-day-old sale priced the hull")
	}
}

// TestTVCorpusSampleAging: the corpus miner returns a live
// ask the sweep last saw 30 days ago -- stamped at its
// last-seen time for the fit to age-weight -- and records
// its observation; no freshness gate drops it on the floor.
func TestTVCorpusSampleAging(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	lastSeen := now.Add(-30 * 24 * time.Hour)
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO pub_contracts (contract_id, region_id, type, price, date_issued, date_expired, items_state, first_seen, last_seen)
		 VALUES (9101, 10000002, 'item_exchange', 5e10, $1, $2, 'stored', $1, $1)`,
		lastSeen, now.Add(7*24*time.Hour)); err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity, is_included)
		 VALUES (1, 9101, 5005, 1, true)`); err != nil {
		t.Fatalf("seed contract item: %v", err)
	}
	priceOf := tvStoredPricerFrom(nil, nil, nil)
	samples := app.corpusListingSamples(ctx, 5005,
		map[int64]bool{5005: true}, map[int64]bool{5005: true}, priceOf, now)
	if len(samples) != 1 || samples[0].price != 5e10 || math.Abs(samples[0].at.Sub(lastSeen).Seconds()) > 1 || samples[0].source != tvSampleListing {
		t.Fatalf("corpus samples = %+v, want the 30-day-old ask at its last-seen stamp", samples)
	}
	obs, err := q.ListTVContractObservationsByRegion(ctx, 10000002)
	if err != nil {
		t.Fatalf("list observations: %v", err)
	}
	if len(obs) != 1 || obs[0].ContractID != 9101 {
		t.Errorf("observations = %+v, want the corpus contract recorded", obs)
	}
}

// TestTVShipLevelRefutedLadder: a prize hull's build cost is
// fiction and every real ask refutes it, so the hull prices
// from the learned level of its own asks -- not from any
// correction of the fiction, and not at all from one ask.
func TestTVShipLevelRefutedLadder(t *testing.T) {
	now := time.Now()
	subjects := map[int64]bool{1: true}
	ladders := map[int64]float64{1: 548_521}
	families := map[int64]string{1: "Fixture"}
	samples := map[int64][]tvContractSample{1: {
		tvLearnSample(4e10, tvSampleListing, now),
		tvLearnSample(5e10, tvSampleListing, now),
		tvLearnSample(6e10, tvSampleListing, now),
	}}
	p := buildTVShipPricing(subjects, ladders, samples, families, now)
	// The level: the mean of the asks' log prices.
	est, source, count, ok := p.price(1)
	want := math.Exp((math.Log(4e10) + math.Log(5e10) + math.Log(6e10)) / 3)
	if !ok || math.Abs(est-want) > want*0.01 || source != tvSampleListing || count != 3 {
		t.Errorf("level = (%v, %q, %d, %v), want (~%v, listing, 3, true)", est, source, count, ok, want)
	}

	// One in-band sample keeps the hull on its ladder: the
	// out-of-band pair does not drag it into level mode, and
	// one in-band sample alone cannot price it either.
	samples[1] = []tvContractSample{
		tvLearnSample(600_000, tvSampleListing, now),
		tvLearnSample(5e10, tvSampleListing, now),
		tvLearnSample(6e10, tvSampleListing, now),
	}
	p = buildTVShipPricing(subjects, ladders, samples, families, now)
	if _, _, _, ok := p.price(1); ok {
		t.Error("one in-band sample plus refuting noise priced the hull")
	}
}

// TestTVShipLevelPeers: level hulls in a family shrink
// toward each other -- two hulls with two asks apiece meet
// at the pooled level -- while a peerless hull with too
// little of its own stays silent.
func TestTVShipLevelPeers(t *testing.T) {
	now := time.Now()
	subjects := map[int64]bool{1: true, 2: true, 3: true}
	ladders := map[int64]float64{1: 0, 2: 0, 3: 0}
	families := map[int64]string{1: "Fixture", 2: "Fixture", 3: "Other"}
	samples := map[int64][]tvContractSample{
		1: {tvLearnSample(5e10, tvSampleListing, now), tvLearnSample(5e10, tvSampleListing, now)},
		2: {tvLearnSample(1e11, tvSampleListing, now), tvLearnSample(1e11, tvSampleListing, now)},
		3: {tvLearnSample(1e9, tvSampleListing, now), tvLearnSample(1e12, tvSampleListing, now)},
	}
	p := buildTVShipPricing(subjects, ladders, samples, families, now)
	// Each Fixture hull: (2 x own log + 2 x peer log) / 4 --
	// the geometric mean of the two levels.
	want := math.Sqrt(5e10 * 1e11)
	for _, id := range []int64{1, 2} {
		est, _, _, ok := p.price(id)
		if !ok || math.Abs(est-want) > want*0.01 {
			t.Errorf("peer level(%d) = (%v, %v), want ~%v", id, est, ok, want)
		}
	}
	// Hull 3 has no peers and two asks three orders apart:
	// its effective count is 2, under the solo bar.
	if _, _, _, ok := p.price(3); ok {
		t.Error("a peerless hull priced from two scattered asks")
	}
}

// TestTVDedupeSamplesByContract: one contract speaks once,
// at its strongest tier, however many stores saw it.
func TestTVDedupeSamplesByContract(t *testing.T) {
	now := time.Now()
	in := []tvContractSample{
		{price: 100, at: now, source: tvSampleListing, contractID: 7},
		{price: 90, at: now, source: tvSampleFulfilled, contractID: 7},
		{price: 50, at: now, source: tvSampleVanished, contractID: 8},
		{price: 1, at: now, source: tvSampleListing},
		{price: 2, at: now, source: tvSampleListing},
	}
	out := dedupeSamplesByContract(in)
	if len(out) != 4 {
		t.Fatalf("deduped = %d samples, want 4", len(out))
	}
	var seven *tvContractSample
	for i := range out {
		if out[i].contractID == 7 {
			seven = &out[i]
		}
	}
	if seven == nil || seven.source != tvSampleFulfilled || seven.price != 90 {
		t.Errorf("contract 7 speaks as %+v, want the fulfilled sample at 90", seven)
	}
}

// tvExitTransport answers the public items route with a
// fixed status per contract, and counts what it was asked.
type tvExitTransport struct {
	codes map[int64]int
	calls map[int64]int
}

func (s *tvExitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(req.URL.Path, "/contracts/public/items/")
	id, err := strconv.ParseInt(strings.Trim(path, "/"), 10, 64)
	if err != nil {
		return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	if s.calls == nil {
		s.calls = make(map[int64]int)
	}
	s.calls[id]++
	code, ok := s.codes[id]
	if !ok {
		code = 500
	}
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

// TestTVShipSaleProbes: the corpus-scale exit probe. A
// departed ship listing ESI confirms accepted (204 before
// expiry) is stamped a confirmed sale; one that answers 404
// well before expiry graduates to likely; one that leaves
// inside the expiry margin is forgotten; one still listed is
// left alone. Listings for other types, and listings still
// in the corpus, are never asked about.
func TestTVShipSaleProbes(t *testing.T) {
	transport := &tvExitTransport{codes: map[int64]int{7001: 204, 7002: 404, 7003: 404, 7004: 200, 7005: 204, 7006: 204}}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	observe := func(id, typeID int64, expires time.Time) {
		t.Helper()
		if err := q.UpsertTVContractObservation(ctx, db.UpsertTVContractObservationParams{
			ContractID: id, RegionID: 10000002, TypeID: typeID, Shape: tvContractShapeSingle,
			ListedPrice: 5e9, SamplePrice: 5e9, FirstSeenAt: now.Add(-24 * time.Hour), ExpiresAt: expires,
		}); err != nil {
			t.Fatalf("seed observation %d: %v", id, err)
		}
	}
	observe(7001, 5003, now.Add(10*24*time.Hour)) // accepted before expiry
	observe(7002, 5003, now.Add(10*24*time.Hour)) // gone before expiry
	observe(7003, 5003, now.Add(time.Hour))       // gone inside the margin
	observe(7004, 5003, now.Add(10*24*time.Hour)) // still listed
	observe(7005, 9999, now.Add(10*24*time.Hour)) // not a ship type: never due
	observe(7006, 5003, now.Add(10*24*time.Hour)) // still in the corpus: not departed
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO pub_contracts (contract_id, region_id, type, price, date_issued, date_expired, items_state, first_seen, last_seen)
		 VALUES (7006, 10000002, 'item_exchange', 5e9, $1, $2, 'stored', $1, $1)`,
		now.Add(-24*time.Hour), now.Add(10*24*time.Hour)); err != nil {
		t.Fatalf("seed corpus contract: %v", err)
	}

	app.probeShipContractSales(ctx, []int64{5003}, now)

	if transport.calls[7005] != 0 || transport.calls[7006] != 0 {
		t.Errorf("probes asked about 7005 x%d, 7006 x%d; want neither asked", transport.calls[7005], transport.calls[7006])
	}
	vanished, err := q.ListTVVanishedSamplesSince(ctx, sql.NullTime{Time: now.Add(-time.Hour), Valid: true})
	if err != nil {
		t.Fatalf("list vanished: %v", err)
	}
	byID := make(map[int64]db.ListTVVanishedSamplesSinceRow)
	for _, row := range vanished {
		byID[row.ContractID] = row
	}
	if row, ok := byID[7001]; !ok || !row.Sold {
		t.Errorf("contract 7001 = %+v (present %v), want a confirmed sale", row, ok)
	}
	if row, ok := byID[7002]; !ok || row.Sold {
		t.Errorf("contract 7002 = %+v (present %v), want a likely sale", row, ok)
	}
	if _, ok := byID[7003]; ok {
		t.Error("contract 7003 counted as evidence; expiry explains its exit and it must be forgotten")
	}
	live, err := q.ListTVContractObservationsByRegion(ctx, 10000002)
	if err != nil {
		t.Fatalf("list live observations: %v", err)
	}
	liveIDs := make(map[int64]bool)
	for _, o := range live {
		liveIDs[o.ContractID] = true
	}
	for _, id := range []int64{7004, 7005, 7006} {
		if !liveIDs[id] {
			t.Errorf("observation %d missing from the live set, want it watched still", id)
		}
	}
	if liveIDs[7003] {
		t.Error("observation 7003 still watched; it should have been forgotten")
	}
}
