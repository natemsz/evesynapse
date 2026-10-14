package app

import (
	"math"
	"testing"
)

// The sampling and trust transitions, as pure functions: how a
// signer's score moves with its ledger, where the pull gate closes,
// and how often its entries are checked against ESI as trust moves.
// (The draw itself, the failure streak, decay and auto-exclusion are
// covered in swarm_api_test.go.)

// TestSwarmTrustPrior: a new signer starts at 0.9, one failure moves
// it little, and only sustained behaviour moves it a lot.
func TestSwarmTrustPrior(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }
	for _, tc := range []struct {
		good, bad int64
		want      float64
	}{
		{0, 0, 0.9},
		{0, 1, 9.0 / 11},
		{10, 0, 19.0 / 20},
		{0, 10, 9.0 / 20},
		{990, 0, 999.0 / 1000},
	} {
		if got := swarmTrust(tc.good, tc.bad); !near(got, tc.want) {
			t.Errorf("trust(%d good, %d bad) = %v, want %v", tc.good, tc.bad, got, tc.want)
		}
	}
	// More good never lowers trust; more bad never raises it.
	for good := int64(0); good < 50; good++ {
		for bad := int64(0); bad < 50; bad++ {
			if swarmTrust(good+1, bad) < swarmTrust(good, bad) {
				t.Fatalf("a good check lowered trust at %d/%d", good, bad)
			}
			if swarmTrust(good, bad+1) > swarmTrust(good, bad) {
				t.Fatalf("a bad check raised trust at %d/%d", good, bad)
			}
		}
	}
}

// TestSwarmPullGateEdges: a peer is judged only once it has
// swarmTrustMinChecks checks, and then leaves the rotation only
// strictly below the floor.
func TestSwarmPullGateEdges(t *testing.T) {
	for _, tc := range []struct {
		good, bad int64
		want      bool
	}{
		{0, 9, true},   // trust 0.47 but only nine checks: not judged yet
		{0, 10, false}, // ten checks, trust 0.45: out
		{2, 10, true},  // trust exactly 0.5: the floor itself is in
		{1, 10, false}, // 0.476: out
		{10, 10, true}, // 0.63
		{500, 60, true},
	} {
		if got := swarmPeerPullable(tc.good, tc.bad); got != tc.want {
			t.Errorf("pullable(%d good, %d bad) = %v, want %v (trust %.3f)", tc.good, tc.bad, got, tc.want, swarmTrust(tc.good, tc.bad))
		}
	}
}

// sampleRate is the share of draws that verify, for a signer's
// ledger: the probability swarmCheckDraw answers true, measured on a
// fine grid of draws.
func sampleRate(good, bad int64) float64 {
	const steps = 10000
	hits := 0
	for i := 0; i < steps; i++ {
		if swarmCheckDraw(swarmTrust(good, bad), good+bad, float64(i)/steps) {
			hits++
		}
	}
	return float64(hits) / steps
}

// TestSwarmSampleRateFollowsTrust: a new signer is always checked;
// once judged, the rate falls as good checks accumulate, stays inside
// [swarmSampleMin, swarmSampleMax], and a failure raises it again.
func TestSwarmSampleRateFollowsTrust(t *testing.T) {
	for total := int64(0); total < swarmTrustMinChecks; total++ {
		if r := sampleRate(total, 0); r != 1 {
			t.Errorf("signer with %d checks sampled at %v, want always", total, r)
		}
	}
	prev := 1.0
	for good := int64(swarmTrustMinChecks); good <= 2000; good += 10 {
		r := sampleRate(good, 0)
		if r > prev+1e-9 {
			t.Fatalf("rate rose from %v to %v as good checks grew to %d", prev, r, good)
		}
		if r < swarmSampleMin-1e-4 || r > swarmSampleMax+1e-4 {
			t.Fatalf("rate %v at %d good checks is outside [%v, %v]", r, good, swarmSampleMin, swarmSampleMax)
		}
		prev = r
	}
	if prev > swarmSampleMin+1e-4 {
		t.Errorf("a long clean record samples at %v, want the floor %v", prev, swarmSampleMin)
	}
	// Failures bring the checks back.
	if clean, failed := sampleRate(200, 0), sampleRate(200, 20); failed <= clean {
		t.Errorf("twenty failures left the rate at %v (clean %v); want it higher", failed, clean)
	}
	if r := sampleRate(0, 50); r < swarmSampleMax-1e-4 {
		t.Errorf("an all-failing signer samples at %v, want the cap %v", r, swarmSampleMax)
	}
}
