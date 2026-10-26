package app

import (
	"context"
	"evesynapse/internal/app/swarm"
	"fmt"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestSwarmFirstWarming: a miss is warmed swarm-first. A is a
// dedicated node that warms corporation records; B is a main with a
// corporation it wants and does not hold.
//
//   - B learns what A warms and leaves the miss to it: its own drain
//     spends nothing on the covered kind.
//   - B's pull reaches A as demand; A queues the key and fetches it on
//     its own budget (a dedicated node never defers its own kind).
//   - B's next pull brings the record back through the unchanged trust
//     pipeline (staged, spot-checked, applied).
//   - A miss nobody delivers is still B's to fetch once the grace has
//     run out: deferring delays a fetch, never cancels it.
func TestSwarmFirstWarming(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	const corpID, strandedID = int64(98000001), int64(98000002)
	truth := func(name string) string {
		return fmt.Sprintf(`{"name":%q,"ticker":"FIX","member_count":42,"ceo_id":1,"creator_id":1,"alliance_id":0,"home_station_id":60003760,"tax_rate":0.1,"date_founded":"2020-01-01T00:00:00Z","description":"","url":""}`, name)
	}
	p.stubA.corps = map[int64]string{corpID: truth("Warmed Corp")}
	p.stubB.corps = map[int64]string{corpID: truth("Warmed Corp"), strandedID: truth("Stranded Corp")}
	swarm.StateFor(p.a).Cfg.Warms = swarm.ParseSwarmWarm("corporation_record")
	p.pinA(t)
	for _, id := range []int64{corpID, strandedID} {
		if err := p.b.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{CorporationID: id, Priority: wantViewed}); err != nil {
			t.Fatalf("want corp %d on B: %v", id, err)
		}
	}

	// B learns what A warms and leaves its misses to it.
	swarm.StateFor(p.b).SwarmRefreshCoverage(ctx, now)
	if !swarm.StateFor(p.b).SwarmCovered(swarm.SwarmKindCorporation, now) {
		t.Fatal("B did not learn that A warms corporation records")
	}
	for _, id := range []int64{corpID, strandedID} {
		if settled, _ := p.b.drainCorporationRecord(ctx, id, &fetchBudget{left: 10}, now); settled {
			t.Fatalf("B fetched covered corporation %d itself", id)
		}
	}
	if p.stubB.calls != 0 {
		t.Fatalf("B spent %d ESI calls on covered misses, want 0", p.stubB.calls)
	}

	// The pull is A's demand: it queues the key for its own worker.
	swarm.StateFor(p.b).SwarmFetchPhase(ctx, now)
	if rec, err := p.a.queries.GetCorporationRecord(ctx, corpID); err != nil || rec.State != orgStatePending {
		t.Fatalf("A record after B's pull: %+v (%v), want a queued lookup", rec, err)
	}
	if settled, _ := p.a.drainCorporationRecord(ctx, corpID, &fetchBudget{left: 10}, now); !settled || p.stubA.calls == 0 {
		t.Fatalf("A did not fetch the demand on its own budget (settled %v, calls %d)", settled, p.stubA.calls)
	}

	// B's next pull brings it back; the trust pipeline applies it.
	swarm.StateFor(p.b).SwarmFetchPhase(ctx, now)
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("B applied %d entries, want 1", got)
	}
	rec, err := p.b.queries.GetCorporationRecord(ctx, corpID)
	if err != nil || rec.State != orgStateReady || !strings.Contains(rec.Payload, "Warmed Corp") {
		t.Fatalf("B record: %+v (%v), want A's warmed copy", rec, err)
	}
	spotCalls := p.stubB.calls

	// Nobody delivers the other one: past the grace, B's own budget
	// is the last resort.
	later := now.Add(swarm.SwarmWarmGrace + time.Minute)
	if settled, _ := p.b.drainCorporationRecord(ctx, strandedID, &fetchBudget{left: 10}, later); !settled {
		t.Fatal("B never fell back to its own fetch after the grace")
	}
	if p.stubB.calls == spotCalls {
		t.Fatal("B's fallback spent no ESI swarm.Call")
	}
}

// TestSwarmWarmPeerLearnsPuller: B files its heartbeat with a
// warming peer when it learns what the peer warms, so the peer
// (a directory-verifying node like storm) can list B and verify
// its signed pulls instead of falling back to the bearer.
func TestSwarmWarmPeerLearnsPuller(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	swarm.StateFor(p.a).Cfg.Warms = swarm.ParseSwarmWarm("corporation_record")
	p.b.cfg.eveCallbackURL = "https://b.example.org/auth/callback"
	p.pinA(t)
	swarm.StateFor(p.b).SwarmRefreshCoverage(ctx, now)
	peer, err := p.a.queries.GetSwarmPeer(ctx, "instance-b")
	if err != nil {
		t.Fatalf("warm peer did not learn the puller: %v", err)
	}
	if peer.PublicKey != swarm.StateFor(p.b).SwarmID.PublicString() {
		t.Fatalf("warm peer holds the wrong key for the puller")
	}
}
