package app

import (
	"context"
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
	p := newSwarmPair(t, swarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	const corpID, strandedID = int64(98000001), int64(98000002)
	truth := func(name string) string {
		return fmt.Sprintf(`{"name":%q,"ticker":"FIX","member_count":42,"ceo_id":1,"creator_id":1,"alliance_id":0,"home_station_id":60003760,"tax_rate":0.1,"date_founded":"2020-01-01T00:00:00Z","description":"","url":""}`, name)
	}
	p.stubA.corps = map[int64]string{corpID: truth("Warmed Corp")}
	p.stubB.corps = map[int64]string{corpID: truth("Warmed Corp"), strandedID: truth("Stranded Corp")}
	p.a.cfg.swarm.warms = parseSwarmWarm("corporation_record")
	p.pinA(t)
	for _, id := range []int64{corpID, strandedID} {
		if err := p.b.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{CorporationID: id, Priority: wantViewed}); err != nil {
			t.Fatalf("want corp %d on B: %v", id, err)
		}
	}

	// B learns what A warms and leaves its misses to it.
	p.b.swarmRefreshCoverage(ctx, now)
	if !p.b.swarmCovered(swarmKindCorporation, now) {
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
	p.b.swarmFetchPhase(ctx, now)
	if rec, err := p.a.queries.GetCorporationRecord(ctx, corpID); err != nil || rec.State != orgStatePending {
		t.Fatalf("A record after B's pull: %+v (%v), want a queued lookup", rec, err)
	}
	if settled, _ := p.a.drainCorporationRecord(ctx, corpID, &fetchBudget{left: 10}, now); !settled || p.stubA.calls == 0 {
		t.Fatalf("A did not fetch the demand on its own budget (settled %v, calls %d)", settled, p.stubA.calls)
	}

	// B's next pull brings it back; the trust pipeline applies it.
	p.b.swarmFetchPhase(ctx, now)
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
	later := now.Add(swarmWarmGrace + time.Minute)
	if settled, _ := p.b.drainCorporationRecord(ctx, strandedID, &fetchBudget{left: 10}, later); !settled {
		t.Fatal("B never fell back to its own fetch after the grace")
	}
	if p.stubB.calls == spotCalls {
		t.Fatal("B's fallback spent no ESI call")
	}
}
