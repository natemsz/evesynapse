package swarm

import "testing"

// TestSwarmPullMessageBytes pins the exact bytes a puller signs
// against a value worked out outside Go: "pull", the instance ID,
// the decimal timestamp and the hex SHA-256 of the body, joined by
// newlines. The storm node builds the same bytes, so any change
// here breaks every puller and server at once.
func TestSwarmPullMessageBytes(t *testing.T) {
	got := string(SwarmPullMessage("abc", 1700000000, []byte(`{"keys":["type:34"]}`)))
	want := "pull\nabc\n1700000000\n719821e246a9e55c34e6f053469644a518ad00afe64c9d732470769ae6f1528f"
	if got != want {
		t.Fatalf("SwarmPullMessage = %q, want %q", got, want)
	}
}
