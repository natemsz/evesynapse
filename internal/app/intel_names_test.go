package app

import (
	"encoding/json"
	"testing"
)

// A /universe/ids/ answer's other kinds are kept per name, a place
// ahead of an item, and a name ESI gave only as a character has none.
func TestESIIDsThings(t *testing.T) {
	var ids esiIDs
	raw := `{"characters":[{"id":1,"name":"Delve"},{"id":2,"name":"Hostile One"},{"id":3,"name":"Wreck"}],
		"regions":[{"id":10000060,"name":"Delve"}],
		"inventory_types":[{"id":26468,"name":"Wreck"},{"id":10000060,"name":"Delve"}]}`
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		t.Fatal(err)
	}
	got := ids.things()
	if got["delve"] != intelThingRegion || got["wreck"] != intelThingType {
		t.Errorf("things %v", got)
	}
	if _, ok := got["hostile one"]; ok {
		t.Errorf("a character-only name has a thing: %v", got)
	}
}
