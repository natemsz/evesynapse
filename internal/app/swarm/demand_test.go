package swarm

import (
	"testing"
	"time"
)

// TestSwarmDemandAllowanceIsPerPullerPerHour: one puller turns at
// most SwarmDemandPerHour misses into wants in an hour, another
// puller has its own allowance, and the next hour starts afresh.
func TestSwarmDemandAllowanceIsPerPullerPerHour(t *testing.T) {
	app := &Mod{}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < SwarmDemandPerHour; i++ {
		if !app.swarmDemandAllowed("peer-a", now) {
			t.Fatalf("demand %d refused inside the allowance", i+1)
		}
	}
	if app.swarmDemandAllowed("peer-a", now.Add(30*time.Minute)) {
		t.Fatal("demand past the hourly allowance was taken")
	}
	if !app.swarmDemandAllowed("peer-b", now) {
		t.Fatal("a second puller shares the first one's allowance")
	}
	if !app.swarmDemandAllowed("peer-a", now.Add(time.Hour)) {
		t.Fatal("the allowance did not renew after an hour")
	}
}
