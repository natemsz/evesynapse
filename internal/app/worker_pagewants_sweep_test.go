package app

import (
	"testing"
	"time"
)

// TestSweepPageWants: expired wants are forgotten in every
// scope, fresh ones are kept, and a scope emptied by the sweep
// goes with them — including scopes nobody polls any more,
// which pendingPageWantCount never gets the chance to settle.
func TestSweepPageWants(t *testing.T) {
	now := time.Now()
	app := &Application{pageWants: map[string]map[string]pageWant{
		"user:1": {
			"character:5": {Kind: pageWantCharacter, ID: 5, NotedAt: now.Add(-pageWantTTL - time.Minute)},
			"character:6": {Kind: pageWantCharacter, ID: 6, NotedAt: now.Add(-time.Minute)},
		},
		"user:2": {
			"place:9": {Kind: pageWantPlace, ID: 9, NotedAt: now.Add(-2 * pageWantTTL)},
		},
	}}
	app.sweepPageWants(now)

	set := app.pageWants["user:1"]
	if len(set) != 1 {
		t.Fatalf("user:1 keeps %d wants, want the fresh one only", len(set))
	}
	if _, ok := set["character:6"]; !ok {
		t.Fatal("the fresh want was swept")
	}
	if _, ok := app.pageWants["user:2"]; ok {
		t.Fatal("a scope emptied by the sweep was kept")
	}
}
