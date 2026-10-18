package app

// Two-instance swarm tests: two fixture apps over separate
// databases talk over real HTTP (one serves its handler through
// httptest), and a canned ESI transport plays CCP's truth for
// the spot checks. They prove sharing works, poison never lands
// in the tables, sustained lies exile the liar, and the crypto
// checks hold — at a bounded ESI cost.

import (
	"context"
	"encoding/json"
	"evesynapse/internal/app/swarm"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// swarmESISStub serves CCP's public endpoints from fixtures and
// counts calls, so the tests see exactly what each check costs.
type swarmESISStub struct {
	history string // /markets/.../history/
	types   map[int64]string
	corps   map[int64]string
	allies  map[int64]string
	// contractItems serves /contracts/public/items/{id}/, and
	// contractStatus forces a status for one contract's route
	// (a 400 for courier cargo, say) ahead of any body.
	contractItems  map[int64]string
	contractStatus map[int64]int
	calls          int
}

func (s *swarmESISStub) respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Expires":      []string{"Wed, 01 Jan 2999 00:00:00 GMT"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func (s *swarmESISStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	path := req.URL.Path
	id := int64(0)
	for _, c := range strings.Trim(path, "/") {
		if c >= '0' && c <= '9' {
			id = id*10 + int64(c-'0')
		}
	}
	switch {
	case strings.Contains(path, "/history/"):
		if s.history == "" {
			return s.respond(http.StatusNotFound, `{"error":"no history"}`), nil
		}
		return s.respond(http.StatusOK, s.history), nil
	case strings.HasPrefix(path, "/universe/types/"):
		if body, ok := s.types[id]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"no such type"}`), nil
	case strings.HasPrefix(path, "/corporations/"):
		if body, ok := s.corps[id]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"no such corporation"}`), nil
	case strings.HasPrefix(path, "/alliances/"):
		if body, ok := s.allies[id]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"no such alliance"}`), nil
	case strings.HasPrefix(path, "/contracts/public/items/"):
		if status, ok := s.contractStatus[id]; ok {
			return s.respond(status, `{"error":"refused"}`), nil
		}
		if body, ok := s.contractItems[id]; ok {
			return s.respond(http.StatusOK, body), nil
		}
		return s.respond(http.StatusNotFound, `{"error":"no such contract"}`), nil
	default:
		return s.respond(http.StatusNotFound, `{"error":"unknown"}`), nil
	}
}

// swarmPair is two instances: A serves data, B pulls and
// checks. Each has its own database; ESI truth is canned per
// side (A never needs it, B checks against it).
type swarmPair struct {
	a, b   *Application
	urlA   string
	stubA  *swarmESISStub
	stubB  *swarmESISStub
	secret string
}

func newSwarmPair(t *testing.T, modeB string) *swarmPair {
	t.Helper()
	secret := "pair-shared-secret-0123456789abcdef"
	stubA := &swarmESISStub{}
	ta, _, _ := buildCorpTestApp(t, stubA)
	swarm.StateFor(ta).Cfg = swarm.SwarmConfig{Mode: swarm.SwarmOpen, Secret: secret, GossipHours: swarm.DefaultSwarmGossipHours}
	swarm.StateFor(ta).SwarmID, _ = swarm.MintSwarmIdentity()
	swarm.StateFor(ta).SwarmID.ID = "instance-a"
	swarm.StateFor(ta).SwarmCli = swarm.NewSwarmClient(secret)

	stubB := &swarmESISStub{}
	tb, _, _ := buildCorpTestApp(t, stubB)
	swarm.StateFor(tb).Cfg = swarm.SwarmConfig{Mode: modeB, Secret: secret, GossipHours: swarm.DefaultSwarmGossipHours}
	swarm.StateFor(tb).SwarmID, _ = swarm.MintSwarmIdentity()
	swarm.StateFor(tb).SwarmID.ID = "instance-b"
	swarm.StateFor(tb).SwarmCli = swarm.NewSwarmClient(secret)

	srv := httptest.NewServer(ta.Handler())
	t.Cleanup(srv.Close)
	return &swarmPair{a: ta, b: tb, urlA: srv.URL, stubA: stubA, stubB: stubB, secret: secret}
}

// pinA records A's identity in B as an approved peer, the way
// gossip-plus-approval leaves it.
func (p *swarmPair) pinA(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := p.b.queries.UpsertSwarmPeer(context.Background(), db.UpsertSwarmPeerParams{
		InstanceID: "instance-a", BaseUrl: p.urlA, PublicKey: swarm.StateFor(p.a).SwarmID.PublicString(),
		Source: "manual", FirstSeen: now,
	}); err != nil {
		t.Fatalf("pin A: %v", err)
	}
	if err := p.b.queries.SetSwarmPeerApproved(context.Background(), db.SetSwarmPeerApprovedParams{InstanceID: "instance-a", Approved: true}); err != nil {
		t.Fatalf("approve A: %v", err)
	}
}

// pullStages asks A for keys and stages what B is served, through
// the path the worker uses.
func (p *swarmPair) pullStages(t *testing.T, keys []swarm.SwarmPullKey) {
	t.Helper()
	ctx := context.Background()
	entries, err := swarm.StateFor(p.b).SwarmCli.PullEntries(ctx, p.urlA, keys)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("pull: no entries served")
	}
	if got := swarm.StateFor(p.b).SwarmStagePulled(ctx, "instance-a", p.urlA, keys, entries, time.Now().UTC()); got != len(entries) {
		t.Fatalf("staged %d of %d served entries", got, len(entries))
	}
}

// spotRuns runs B's spot-check pass and returns what applied.
func (p *swarmPair) spotRuns(t *testing.T) int {
	t.Helper()
	applied, limited := swarm.StateFor(p.b).SwarmSpotCheck(context.Background(), &fetchBudget{left: 120}, nil, time.Now().UTC())
	if limited {
		t.Fatalf("spot check hit the error limit on canned ESI")
	}
	return applied
}

func TestSwarmPairSharesNames(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	if err := p.a.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	p.stubB.types = map[int64]string{34: `{"name":"Tritanium","group_id":18}`}
	p.pinA(t)

	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindTypeName, Key: "34"}})
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d, want 1", got)
	}
	name, err := p.b.queries.GetTypeName(ctx, 34)
	if err != nil || name != "Tritanium" {
		t.Fatalf("B holds %q err %v, want Tritanium", name, err)
	}
	// One shared entry cost one ESI swarm.Call to verify — the pass is
	// bounded, not per-row chatty.
	if p.stubB.calls != 1 {
		t.Fatalf("spot check cost %d ESI calls, want 1", p.stubB.calls)
	}
	counters := swarm.StateFor(p.b).SwarmCounterMap(ctx)
	if counters[swarm.SwarmCounterAppliedNames] != 1 || counters[swarm.SwarmCounterSpotPassed] != 1 {
		t.Fatalf("counters: %+v", counters)
	}
}

func TestSwarmPairSharesHistory(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	key := marketKey{RegionID: 10000002, TypeID: 34}
	truth := `[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10},{"date":"2026-10-02","average":5.7,"highest":6.2,"lowest":5.1,"volume":110,"order_count":11}]`
	p.stubB.history = truth
	// A holds exactly ESI's truth, freshly fetched.
	var days []struct {
		Date       string  `json:"date"`
		Average    float64 `json:"average"`
		Highest    float64 `json:"highest"`
		Lowest     float64 `json:"lowest"`
		Volume     int64   `json:"volume"`
		OrderCount int64   `json:"order_count"`
	}
	if err := decodeJSON(strings.NewReader(truth), &days); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, day := range days {
		if err := p.a.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
			RegionID: key.RegionID, TypeID: key.TypeID, Date: day.Date,
			Average: day.Average, Highest: day.Highest, Lowest: day.Lowest,
			Volume: day.Volume, OrderCount: day.OrderCount,
		}); err != nil {
			t.Fatalf("seed A: %v", err)
		}
	}
	p.a.recordMarketFetch(ctx, marketFetchKind("history", key), fetchStateOK, "")
	p.pinA(t)

	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindHistory, Key: "10000002/34"}})
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d, want 1", got)
	}
	rows, err := p.b.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: key.RegionID, TypeID: key.TypeID, RowLimit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("B holds %d history rows err %v, want 2", len(rows), err)
	}
	// B's worker now considers the pair fetched: the entry saved
	// the ESI swarm.Call B would have spent.
	if p.b.marketFetchDue(ctx, marketFetchKind("history", key), historyRefetchGate) {
		t.Fatalf("B still considers the pair due after applying the shared entry")
	}
}

// serveCorp seeds A with one ready corporation record, and wants
// it on B: the demand the early fetch phase must pull.
func serveCorp(t *testing.T, p *swarmPair) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := p.a.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000001, Payload: `{"name":"Shared Corp","ticker":"SHR"}`,
		State: orgStateReady, FetchedAt: timeSet(now),
	}); err != nil {
		t.Fatalf("seed A corp: %v", err)
	}
	if err := p.b.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{
		CorporationID: 98000001, Priority: wantViewed,
	}); err != nil {
		t.Fatalf("want corp on B: %v", err)
	}
	p.pinA(t)
}

// TestSwarmFetchPhasePullsDemand proves the early pass pulls
// what is wanted while it is still wanted: after the phase, B
// holds A's entry staged, before B's own worker would spend an
// ESI swarm.Call on the same record.
func TestSwarmFetchPhasePullsDemand(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	serveCorp(t, p)
	swarm.StateFor(p.b).SwarmFetchPhase(ctx, time.Now().UTC())
	staged, err := p.b.queries.ListUncheckedStaging(ctx, 10)
	if err != nil || len(staged) != 1 || staged[0].Kind != swarm.SwarmKindCorporation || staged[0].Key != "98000001" {
		t.Fatalf("staged after fetch phase: %+v err=%v, want the corporation entry", staged, err)
	}
}

// TestSwarmPullAsksADirectoryLikeAnyPeer: no instance ID is
// singled out. A peer that holds no data is pulled like the
// rest and answers with nothing, and the peer that does hold
// the record still serves it.
func TestSwarmPullAsksADirectoryLikeAnyPeer(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	serveCorp(t, p)
	var seedHits int
	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seedHits++
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	t.Cleanup(seed.Close)
	// A serves data AND is a configured seed directory: still pulled.
	swarm.StateFor(p.b).Cfg.Seeds = []string{p.urlA}
	if _, err := p.b.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "seed", BaseUrl: seed.URL, PublicKey: strings.Repeat("A", 44),
		Source: "seed", FirstSeen: now,
	}); err != nil {
		t.Fatalf("pin seed: %v", err)
	}
	if err := p.b.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: "seed", Approved: true}); err != nil {
		t.Fatalf("approve seed: %v", err)
	}
	swarm.StateFor(p.b).SwarmPull(ctx, now)
	// A seed is a member like any other: it is asked in its
	// turn, answers with nothing, and costs nobody anything.
	if seedHits != 1 {
		t.Fatalf("the directory saw %d pulls, want 1", seedHits)
	}
	staged, err := p.b.queries.ListUncheckedStaging(ctx, 10)
	if err != nil || len(staged) != 1 || staged[0].Key != "98000001" {
		t.Fatalf("staged beside the seed: %+v err=%v, want A's entry", staged, err)
	}
}

func TestSwarmPairPoisonNeverServed(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	key := marketKey{RegionID: 10000002, TypeID: 34}
	// ESI's truth.
	p.stubB.history = `[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10}]`
	// A holds poison: the same day at a false price, freshly
	// "fetched" so it serves.
	if err := p.a.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
		RegionID: key.RegionID, TypeID: key.TypeID, Date: "2026-10-01",
		Average: 999.99, Highest: 999.99, Lowest: 999.99, Volume: 100, OrderCount: 10,
	}); err != nil {
		t.Fatalf("seed A poison: %v", err)
	}
	p.a.recordMarketFetch(ctx, marketFetchKind("history", key), fetchStateOK, "")
	p.pinA(t)

	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindHistory, Key: "10000002/34"}})
	if got := p.spotRuns(t); got != 0 {
		t.Fatalf("applied %d poisoned entries, want 0", got)
	}
	// Nothing staged, nothing stored: the render path cannot see
	// what never reached the tables.
	staged, err := p.b.queries.ListUncheckedStaging(ctx, 10)
	if err != nil || len(staged) != 0 {
		t.Fatalf("staged rows left: %d err %v", len(staged), err)
	}
	rows, err := p.b.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: key.RegionID, TypeID: key.TypeID, RowLimit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("B holds %d poisoned rows, want 0", len(rows))
	}
	// And the signer is flagged: one bad check on its ledger, one
	// failed spot check on the Sync counters.
	peer, err := p.b.queries.GetSwarmPeer(ctx, "instance-a")
	if err != nil || peer.BadChecks != 1 {
		t.Fatalf("A bad checks: %+v err %v, want 1", peer, err)
	}
	counters := swarm.StateFor(p.b).SwarmCounterMap(ctx)
	if counters[swarm.SwarmCounterSpotFailed] != 1 {
		t.Fatalf("counters: %+v, want 1 spot failure", counters)
	}
	if peer.Excluded {
		t.Fatalf("single failure excluded A (the floor needs sustained lies)")
	}
}

func TestSwarmPairSustainedPoisonExcludes(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	key := marketKey{RegionID: 10000002, TypeID: 34}
	p.stubB.history = `[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10}]`
	if err := p.a.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
		RegionID: key.RegionID, TypeID: key.TypeID, Date: "2026-10-01",
		Average: 999.99, Highest: 999.99, Lowest: 999.99, Volume: 100, OrderCount: 10,
	}); err != nil {
		t.Fatalf("seed A poison: %v", err)
	}
	p.a.recordMarketFetch(ctx, marketFetchKind("history", key), fetchStateOK, "")
	p.pinA(t)

	// Ten poisoned rounds: every one discarded, the eleventh
	// never pulled (A leaves the pull set on exclusion).
	for i := 0; i < swarm.SwarmTrustMinChecks; i++ {
		p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindHistory, Key: "10000002/34"}})
		if got := p.spotRuns(t); got != 0 {
			t.Fatalf("round %d: applied poison", i)
		}
	}
	peer, err := p.b.queries.GetSwarmPeer(ctx, "instance-a")
	if err != nil || !peer.Excluded {
		t.Fatalf("sustained poisoner not excluded: %+v err %v", peer, err)
	}
	pull, err := p.b.queries.ListSwarmPullPeers(ctx, time.Now().Add(-swarm.SwarmServesWindow))
	if err != nil {
		t.Fatalf("pull peers: %v", err)
	}
	for _, pr := range pull {
		if pr.InstanceID == "instance-a" {
			t.Fatalf("excluded A still pulled from")
		}
	}
	rows, _ := p.b.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: key.RegionID, TypeID: key.TypeID, RowLimit: 10})
	if len(rows) != 0 {
		t.Fatalf("B holds %d poisoned rows after %d rounds", len(rows), swarm.SwarmTrustMinChecks)
	}
}

func TestSwarmPairCryptoChecks(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	// Wrong secret: no members, no pulls.
	req, _ := http.NewRequest(http.MethodGet, p.urlA+"/swarm/members", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret-0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("members request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("members with wrong secret: status %d, want 403", resp.StatusCode)
	}
	// A tampered entry (payload rewritten after signing) fails
	// staging even under the right signer name.
	p.pinA(t)
	now := time.Now().UTC()
	entry, err := swarm.SignSwarmEntry(swarm.StateFor(p.a).SwarmID, "instance-a", swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	entry.Payload = `{"type_id":34,"name":"Scordite"}`
	wantStageRefused(t, swarm.StateFor(p.b).StageSwarmEntry(context.Background(), entry, "instance-a", now), "signature does not verify")
	// A replay onto another key fails too: the signature covers
	// the key it was minted for. (The payload names type 34, so
	// the payload check would refuse it as well; the reason shows
	// the signature caught it first.)
	entry, err = swarm.SignSwarmEntry(swarm.StateFor(p.a).SwarmID, "instance-a", swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	entry.Key = "35"
	wantStageRefused(t, swarm.StateFor(p.b).StageSwarmEntry(context.Background(), entry, "instance-a", now), "signature does not verify")
}

func TestSwarmPairGossipLearns(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	// B knows only the seed URL; gossip must introduce A
	// (trust on first use) and approve it in open mode.
	swarm.StateFor(p.b).Cfg.Seeds = []string{p.urlA}
	swarm.StateFor(p.b).SwarmGossip(context.Background(), time.Now().UTC())
	peer, err := p.b.queries.GetSwarmPeer(context.Background(), "instance-a")
	if err != nil {
		t.Fatalf("gossip did not learn A: %v", err)
	}
	if peer.PublicKey != swarm.StateFor(p.a).SwarmID.PublicString() {
		t.Fatalf("learned key is not A's")
	}
	if !peer.Approved || peer.Excluded {
		t.Fatalf("open mode: learned peer approved=%v excluded=%v", peer.Approved, peer.Excluded)
	}
}

func TestSwarmPairAllowlistWaits(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmAllowlist)
	swarm.StateFor(p.b).Cfg.Seeds = []string{p.urlA}
	swarm.StateFor(p.b).SwarmGossip(context.Background(), time.Now().UTC())
	peer, err := p.b.queries.GetSwarmPeer(context.Background(), "instance-a")
	if err != nil {
		t.Fatalf("gossip did not learn A: %v", err)
	}
	// Learned but waiting: known, unapproved, never pulled from.
	if peer.Approved {
		t.Fatalf("allowlist mode auto-approved a gossip-learned peer")
	}
	pull, err := p.b.queries.ListSwarmPullPeers(context.Background(), time.Now().Add(-swarm.SwarmServesWindow))
	if err != nil {
		t.Fatalf("pull peers: %v", err)
	}
	for _, pr := range pull {
		if pr.InstanceID == "instance-a" {
			t.Fatalf("unapproved peer in the pull set")
		}
	}
}

func TestSwarmPairStructureDefersWithoutScope(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	// A serves a resolved structure name; B links no character
	// with the structure scope, so the entry waits staged —
	// never applied blind, never costing ESI.
	if err := p.a.queries.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: 1044752365771, Name: "Holding", State: esi.StructureResolved,
		ResolvedAt: timeSet(time.Now().UTC()), Source: esi.StructureSourceESI,
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	p.pinA(t)
	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindStructure, Key: "1044752365771"}})
	if got := p.spotRuns(t); got != 0 {
		t.Fatalf("applied %d, want 0 (no scoped character to check against)", got)
	}
	staged, err := p.b.queries.ListUncheckedStaging(ctx, 10)
	if err != nil || len(staged) != 1 {
		t.Fatalf("staged rows: %d err %v, want the waiting entry", len(staged), err)
	}
	if p.stubB.calls != 0 {
		t.Fatalf("deferred check cost %d ESI calls, want 0", p.stubB.calls)
	}
}

func TestSwarmPairCorpRecord(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	const corpID = int64(98000001)
	// ESI's truth for the identity fields.
	p.stubB.corps = map[int64]string{corpID: `{"name":"Fixture Corp","ticker":"FIX","member_count":42,"ceo_id":1,"creator_id":1,"alliance_id":0,"home_station_id":60003760,"tax_rate":0.1,"date_founded":"2020-01-01T00:00:00Z","description":"","url":""}`}
	// A holds the assembled record the worker stores.
	record := fmt.Sprintf(`{"corp":%s,"alliance":{"name":"","ticker":"","creator_id":0,"creator_corporation_id":0,"executor_corporation_id":0,"date_founded":""}}`, p.stubB.corps[corpID])
	if err := p.a.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: corpID, Payload: record, State: orgStateReady, FetchedAt: timeSet(time.Now().UTC()),
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	p.pinA(t)
	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindCorporation, Key: fmt.Sprintf("%d", corpID)}})
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d, want 1", got)
	}
	rec, err := p.b.queries.GetCorporationRecord(ctx, corpID)
	if err != nil || rec.State != orgStateReady || !strings.Contains(rec.Payload, "Fixture Corp") {
		t.Fatalf("B record: %+v err %v", rec, err)
	}
}

// TestSwarmPairCorpRecordBareDialect: a peer whose stored
// record is the bare ESI corporation — the dialect storm
// serves — is checked on the fields it actually sent. The
// record matches ESI, so it passes and applies, and its
// signer earns no mark. Read against the wrapped shape alone
// it decoded to empty fields and failed every check, which
// is what exiled storm three times.
func TestSwarmPairCorpRecordBareDialect(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	const corpID = int64(98000001)
	truth := `{"name":"Fixture Corp","ticker":"FIX","member_count":42,"ceo_id":1,"creator_id":1,"alliance_id":0,"home_station_id":60003760,"tax_rate":0.1,"date_founded":"2020-01-01T00:00:00Z","description":"","url":""}`
	p.stubB.corps = map[int64]string{corpID: truth}
	// A holds the bare ESI object as its record, as storm does.
	if err := p.a.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: corpID, Payload: truth, State: orgStateReady, FetchedAt: timeSet(time.Now().UTC()),
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	p.pinA(t)
	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindCorporation, Key: fmt.Sprintf("%d", corpID)}})
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d, want 1 (a true bare record must pass its check)", got)
	}
	rec, err := p.b.queries.GetCorporationRecord(ctx, corpID)
	if err != nil || rec.State != orgStateReady || !strings.Contains(rec.Payload, "Fixture Corp") {
		t.Fatalf("B record: %+v err %v", rec, err)
	}
	peer, err := p.b.queries.GetSwarmPeer(ctx, "instance-a")
	if err != nil || peer.BadChecks != 0 {
		t.Fatalf("an honest bare record cost its signer %d bad checks (err %v), want 0", peer.BadChecks, err)
	}
}

// TestSwarmPairCorpRecordBareDialectLie: the same bare dialect
// carrying a wrong identity still convicts — the tolerant read
// compares the real fields, so a name ESI does not have fails
// the check, nothing applies, and the signer takes the mark.
func TestSwarmPairCorpRecordBareDialectLie(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	const corpID = int64(98000001)
	p.stubB.corps = map[int64]string{corpID: `{"name":"Fixture Corp","ticker":"FIX","member_count":42,"ceo_id":1,"creator_id":1,"alliance_id":0,"home_station_id":60003760,"tax_rate":0.1,"date_founded":"2020-01-01T00:00:00Z","description":"","url":""}`}
	lie := `{"name":"Liar Corp","ticker":"FIX","member_count":42,"ceo_id":1,"creator_id":1,"alliance_id":0,"home_station_id":60003760,"tax_rate":0.1,"date_founded":"2020-01-01T00:00:00Z","description":"","url":""}`
	if err := p.a.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: corpID, Payload: lie, State: orgStateReady, FetchedAt: timeSet(time.Now().UTC()),
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	p.pinA(t)
	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindCorporation, Key: fmt.Sprintf("%d", corpID)}})
	if got := p.spotRuns(t); got != 0 {
		t.Fatalf("applied %d, want 0 (a bare record naming the wrong corporation)", got)
	}
	if rec, err := p.b.queries.GetCorporationRecord(ctx, corpID); err == nil && rec.State == orgStateReady {
		t.Fatalf("the lying record stands on B: %+v", rec)
	}
	peer, err := p.b.queries.GetSwarmPeer(ctx, "instance-a")
	if err != nil || peer.BadChecks < 1 {
		t.Fatalf("the lie cost its signer %d bad checks (err %v), want at least 1", peer.BadChecks, err)
	}
}

// decodeJSON is the tests' fixture reader.
func decodeJSON(r *strings.Reader, into any) error {
	return json.NewDecoder(r).Decode(into)
}

// TestUrgentDrainPullsPeerFirst: a due history want is served
// from the peer when it holds the pair fresh (the ESI fetch is
// skipped and the fetch state says swarm), and falls back to
// ESI for keys the peer lacks. B's trust in A is empty, so the
// volume gate verifies every entry: the hit still costs the
// one verification swarm.Call, never a second fetch.
func TestUrgentDrainPullsPeerFirst(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	days := `[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10}]`
	if err := p.a.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
		RegionID: 10000002, TypeID: 34, Date: "2026-10-01",
		Average: 5.5, Highest: 6.0, Lowest: 5.0, Volume: 100, OrderCount: 10,
	}); err != nil {
		t.Fatalf("seed A history: %v", err)
	}
	p.a.recordMarketFetch(ctx, "history_10000002_34", fetchStateOK, "")
	p.stubB.history = days
	p.pinA(t)
	if err := p.b.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: 10000002, TypeID: 34, LastRequestedAt: now, Priority: wantViewed,
	}); err != nil {
		t.Fatalf("seed want: %v", err)
	}
	calls := p.stubB.calls
	p.b.urgentDrain(ctx)
	if p.stubB.calls != calls+1 {
		t.Fatalf("urgent ESI calls: %d, want exactly 1 (the verification)", p.stubB.calls-calls)
	}
	rows, err := p.b.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 34, RowLimit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("B holds %d rows, want the peer-served one", len(rows))
	}
	state, err := p.b.queries.GetMarketFetchState(ctx, "history_10000002_34")
	if err != nil || state.Detail != "swarm" {
		t.Fatalf("fetch detail %q, want swarm (peer-served, not ESI-fetched)", state.Detail)
	}
	// A key A lacks falls back to ESI: one swarm.Call, stored rows.
	if err := p.b.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: 10000002, TypeID: 35, LastRequestedAt: now, Priority: wantViewed,
	}); err != nil {
		t.Fatalf("seed want: %v", err)
	}
	calls = p.stubB.calls
	p.b.urgentDrain(ctx)
	if p.stubB.calls != calls+1 {
		t.Fatalf("fallback ESI calls: %d, want exactly 1 (the direct fetch)", p.stubB.calls-calls)
	}
	rows, err = p.b.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 35, RowLimit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("B holds %d rows for the fallback key, want 1", len(rows))
	}
}
