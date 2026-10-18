package swarm

import "testing"

// TestReleaseLibraryStormEntries: the library's storm type
// resolves and renders end to end. The test files a synthetic
// storm entry the way a new storm release's hashes land, and
// takes it back out; the shipped library also carries the
// real storm v0.1.0 digests, which the loop below counts.
func TestReleaseLibraryStormEntries(t *testing.T) {
	const stormHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, ok := ReleaseLibrary[stormHash]; ok {
		t.Fatal("fixture hash is already in the library; pick another")
	}
	ReleaseLibrary[stormHash] = ReleaseBuild{Kind: ReleaseStorm, Label: "v1.0.0"}
	defer delete(ReleaseLibrary, stormHash)

	rel, ok := ReleaseFor(stormHash)
	if !ok || rel.Kind != ReleaseStorm || rel.Label != "v1.0.0" {
		t.Fatalf("storm entry resolved as %+v (ok %v)", rel, ok)
	}
	if got := ClassifyBuild(stormHash); got != BuildMainline {
		t.Fatalf("storm hash classified %v, want Mainline", got)
	}
	if got := BuildLabel(stormHash); got != "Mainline · Storm v1.0.0" {
		t.Fatalf("storm build renders %q, want the storm rendering", got)
	}

	// No collision: an app entry in the same library still
	// resolves as an app build and renders with the plain
	// release label — the product kinds share one map without
	// bleeding into each other.
	checked, stormSeeded := 0, 0
	for sha, entry := range ReleaseLibrary {
		if sha == stormHash {
			continue
		}
		if entry.Kind == ReleaseStorm {
			stormSeeded++
			if got := BuildLabel(sha); got != "Mainline · Storm "+entry.Label {
				t.Fatalf("storm entry %s… renders %q", sha[:8], got)
			}
			continue
		}
		if entry.Kind != ReleaseEveSynapse {
			t.Fatalf("seeded entry %s… is kind %q, want evesynapse or storm", sha[:8], entry.Kind)
		}
		if got := BuildLabel(sha); got != "Mainline · "+entry.Label {
			t.Fatalf("app entry %s… renders %q", sha[:8], got)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("the shipped library seeded no app entries")
	}
	if stormSeeded != 2 {
		t.Fatalf("shipped library carries %d storm digests, want 2 (storm v0.1.0, both architectures)", stormSeeded)
	}
}
