package app

import (
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestWaypointPaths pins the ESI calls a route request becomes:
// the first clears the old route, the rest append, and anything
// the game cannot hold is refused before a call is made.
func TestWaypointPaths(t *testing.T) {
	paths, err := waypointPaths(mapRouteRequest{Mode: "dest", Systems: []int64{30000142, 30000144}})
	if err != nil || len(paths) != 1 || !strings.Contains(paths[0], "clear_other_waypoints=true") ||
		!strings.Contains(paths[0], "destination_id=30000142") {
		t.Errorf("dest = %v, %v", paths, err)
	}

	paths, err = waypointPaths(mapRouteRequest{Mode: "route", Systems: []int64{30000142, 30000144}})
	if err != nil || len(paths) != 2 {
		t.Fatalf("route = %v, %v", paths, err)
	}
	if !strings.Contains(paths[0], "clear_other_waypoints=true") || !strings.Contains(paths[1], "clear_other_waypoints=false") {
		t.Errorf("route clear flags = %v", paths)
	}

	for name, req := range map[string]mapRouteRequest{
		"empty dest":  {Mode: "dest"},
		"empty route": {Mode: "route"},
		"bad mode":    {Mode: "loop", Systems: []int64{30000142}},
		"wormhole":    {Mode: "dest", Systems: []int64{31000001}},
	} {
		if _, err := waypointPaths(req); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestWaypointCharacter: the session character steers when it
// holds the scope, else the first character that does; nobody
// holding it sends nothing.
func TestWaypointCharacter(t *testing.T) {
	chars := []db.Character{
		{CharacterID: 1, Name: "No grant", Scopes: "esi-ui.open_window.v1"},
		{CharacterID: 2, Name: "First grant", Scopes: "esi-ui.open_window.v1 " + waypointScope},
		{CharacterID: 3, Name: "Session", Scopes: waypointScope},
	}
	if got := waypointCharacter(chars, 3); got == nil || got.CharacterID != 3 {
		t.Errorf("session pick = %v", got)
	}
	if got := waypointCharacter(chars, 99); got == nil || got.CharacterID != 2 {
		t.Errorf("first-grant pick = %v", got)
	}
	if got := waypointCharacter(chars[:1], 1); got != nil {
		t.Errorf("no grant = %v, want nil", got)
	}
}
