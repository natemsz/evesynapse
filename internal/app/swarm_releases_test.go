package app

// The release learner's end-to-end tests: a learn pass over fixture
// Forgejo release payloads records the binary builds in
// release_builds, classification picks them up, a failed pass
// changes nothing, and relearning the same payload corrupts nothing.
// The signed app manifests and the mirror fallback are pinned beside
// the learner (swarm/releases_test.go), where a test key can sign.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"evesynapse/internal/app/swarm"
)

const stormReleasesFixture = `[
 {"tag_name": "v0.1.1", "draft": false, "assets": [
  {"name": "storm-amd64", "browser_download_url": "https://natems.dev/EveSynapse/evesynapse-storm/releases/download/v0.1.1/storm-amd64"},
  {"name": "SHA256SUMS", "browser_download_url": "https://natems.dev/EveSynapse/evesynapse-storm/releases/download/v0.1.1/SHA256SUMS"}]},
 {"tag_name": "v0.1.0", "draft": false, "assets": [
  {"name": "SHA256SUMS", "browser_download_url": "https://natems.dev/EveSynapse/evesynapse-storm/releases/download/v0.1.0/SHA256SUMS"}]}
]`

const stormSumsV011 = `9ec717334ac81d5229bb2e245c77a4fd829de63d7115b3d90344a5fda8371b3e  storm-amd64
707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9  storm-arm64
c9e5da88df0be2503e767ccb03ab7d10c84c86ae10f6379ad170232c75d015c2  storm-linux-amd64.tar.gz
6be01a65db1505255093457e41b56dfa3e7518b003937b608706140378310b89  storm-linux-arm64.tar.gz
`

const stormSumsV010 = `efab584ee7240a2b4844f5dd4c0fd5a15556e24b600dd91c89ba6c1a3d96f0d2  storm-amd64
120e2f4d3f8c4646f2f9b9be0a8fcf7b55c4c137db9c3e688fbfef3bf07a6eaf  storm-arm64
89cc081c4716ffeeede82202e5401c4a8383821f1c5a94733c68e8da134718ba  storm-linux-amd64.tar.gz
`

// Storm's GitHub mirror carries a release Forgejo does not.
const stormGitHubReleasesFixture = `[
 {"tag_name": "v0.1.4", "draft": false, "assets": [
  {"name": "SHA256SUMS", "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.4/SHA256SUMS"}]}
]`

const stormSumsV014 = `a0cd5a88a8136a032e77042acd6f3115e6406511e520f8e15759cac77fd5f3f2  storm-amd64
5f477e9fa95c4b8f283d070fb1fb9feefbb265d05264ad5015eb35a1fe73857d  storm-arm64
447a0343e135cc5f706af080e6c1bb158d6c98dee1887ed517f12344153256c4  storm-linux-amd64.tar.gz
`

// An app release with no signed manifests attached contributes no
// builds: its binaries are not hashed on trust.
const appReleasesFixture = `[
 {"tag_name": "v0.5.7.3", "draft": false, "assets": [
  {"name": "evesynapse-amd64", "browser_download_url": "https://natems.dev/EveSynapse/evesynapse/releases/download/v0.5.7.3/evesynapse-amd64"}]}
]`

// fixtureReleaseFetch serves the fixture payloads by URL.
func fixtureReleaseFetch(ctx context.Context, url string) ([]byte, error) {
	switch {
	case strings.Contains(url, "/repos/EveSynapse/evesynapse-storm/releases?"):
		return []byte(stormReleasesFixture), nil
	case strings.Contains(url, "api.github.com/repos/natemsz/evesynapse-storm/releases?"):
		return []byte(stormGitHubReleasesFixture), nil
	case strings.HasSuffix(url, "/v0.1.4/SHA256SUMS"):
		return []byte(stormSumsV014), nil
	case strings.Contains(url, "/repos/EveSynapse/evesynapse/releases?"):
		return []byte(appReleasesFixture), nil
	case strings.HasSuffix(url, "/v0.1.1/SHA256SUMS"):
		return []byte(stormSumsV011), nil
	case strings.HasSuffix(url, "/v0.1.0/SHA256SUMS"):
		return []byte(stormSumsV010), nil
	}
	return nil, errors.New("unexpected URL " + url)
}

// TestLearnReleasesRecordsBuilds: one pass records exactly the six
// storm binary builds on the fixture payloads (two on each of two
// Forgejo releases and two on a release only the GitHub mirror
// carries; the archives listed beside them are not builds), the
// storm v0.1.1 arm64 build then classifies as Mainline through the
// learned path, and the seeded library still stands beside it.
func TestLearnReleasesRecordsBuilds(t *testing.T) {
	defer swarm.SetLearnedBuilds(swarm.LearnedBuilds())
	app := swarmTestApp(t)
	st := swarm.StateFor(app)
	st.ReleaseFetch = fixtureReleaseFetch
	ctx := context.Background()
	if err := st.LearnReleases(ctx); err != nil {
		t.Fatalf("learn pass: %v", err)
	}
	rows, err := app.queries.ListLearnedBuilds(ctx)
	if err != nil {
		t.Fatalf("list learned builds: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("learned %d builds, want 6: %+v", len(rows), rows)
	}
	const stormV011Arm64 = "707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9"
	if got := swarm.BuildLabel(stormV011Arm64); got != "Mainline · Storm v0.1.1" {
		t.Fatalf("storm v0.1.1 label after learn %q, want the learned mainline rendering", got)
	}
	if got := swarm.BuildLabel("a0cd5a88a8136a032e77042acd6f3115e6406511e520f8e15759cac77fd5f3f2"); got != "Mainline · Storm v0.1.4" {
		t.Fatalf("storm v0.1.4 (GitHub only) label after learn %q, want the learned mainline rendering", got)
	}
	if got := swarm.BuildLabel(claimKnownA); got != "Mainline · v0.5.2.10" {
		t.Fatalf("seeded label after learn %q, want the seeded rendering", got)
	}

	// Relearning the same payloads records the same set:
	// no duplicates, no drift.
	if err := st.LearnReleases(ctx); err != nil {
		t.Fatalf("relearn pass: %v", err)
	}
	rows, err = app.queries.ListLearnedBuilds(ctx)
	if err != nil {
		t.Fatalf("list learned builds after relearn: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("after relearn: %d builds, want 6", len(rows))
	}

	// A failed pass keeps everything it had: same rows, same
	// classification.
	st.ReleaseFetch = func(ctx context.Context, url string) ([]byte, error) {
		return nil, errors.New("network down")
	}
	if err := st.LearnReleases(ctx); err == nil {
		t.Fatal("a pass whose fetches all fail reported no error")
	}
	rows, err = app.queries.ListLearnedBuilds(ctx)
	if err != nil {
		t.Fatalf("list learned builds after failed pass: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("after failed pass: %d builds, want 6", len(rows))
	}
	if got := swarm.BuildLabel(stormV011Arm64); got != "Mainline · Storm v0.1.1" {
		t.Fatalf("storm v0.1.1 label after failed pass %q, want it unchanged", got)
	}
}
