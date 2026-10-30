package app

// The release learner's end-to-end tests: a learn pass over
// fixture GitLab API payloads (releases, packages, package
// files) records the binary builds in release_builds,
// classification picks them up, a failed pass changes
// nothing, and relearning the same payload corrupts nothing.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"evesynapse/internal/app/swarm"
)

const stormReleasesFixture = `[
 {"tag_name": "v0.1.1", "name": "Storm v0.1.1", "upcoming_release": false},
 {"tag_name": "v0.1.0", "name": "Storm v0.1.0", "upcoming_release": false}
]`

const stormPackagesFixture = `[
 {"id": 101, "name": "storm", "version": "v0.1.1", "package_type": "generic"},
 {"id": 100, "name": "storm", "version": "v0.1.0", "package_type": "generic"}
]`

const stormFilesV011Fixture = `[
 {"file_name": "SHA256SUMS", "file_sha256": "bb16500f48cea154b2bd19bee52cbae03dd13738f362f02b4391b7ef0b221377"},
 {"file_name": "SHA256SUMS.sig", "file_sha256": "421c79318c4db220ce1acfb64703224ebc6d8705331e8033d60e2ea1569e0129"},
 {"file_name": "storm-amd64", "file_sha256": "9ec717334ac81d5229bb2e245c77a4fd829de63d7115b3d90344a5fda8371b3e"},
 {"file_name": "storm-arm64", "file_sha256": "707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9"},
 {"file_name": "storm-linux-amd64.tar.gz", "file_sha256": "c9e5da88df0be2503e767ccb03ab7d10c84c86ae10f6379ad170232c75d015c2"},
 {"file_name": "storm-linux-arm64.tar.gz", "file_sha256": "6be01a65db1505255093457e41b56dfa3e7518b003937b608706140378310b89"}
]`

const stormFilesV010Fixture = `[
 {"file_name": "SHA256SUMS", "file_sha256": "5064f082088c76723ac4250f6432b87cb673090481e20483f1cde097a23912cd"},
 {"file_name": "SHA256SUMS.sig", "file_sha256": "04319a956a072f9952264d11098b759fd00f861763abceda80203ec97cf22255"},
 {"file_name": "storm-amd64", "file_sha256": "efab584ee7240a2b4844f5dd4c0fd5a15556e24b600dd91c89ba6c1a3d96f0d2"},
 {"file_name": "storm-arm64", "file_sha256": "120e2f4d3f8c4646f2f9b9be0a8fcf7b55c4c137db9c3e688fbfef3bf07a6eaf"},
 {"file_name": "storm-linux-amd64.tar.gz", "file_sha256": "89cc081c4716ffeeede82202e5401c4a8383821f1c5a94733c68e8da134718ba"},
 {"file_name": "storm-linux-arm64.tar.gz", "file_sha256": "5ccb016887500e4198e06a6c10ab8800c3f20430ea8d409bab784c7559ea5f3f"}
]`

const appReleasesFixture = `[
 {"tag_name": "v0.5.7.3", "name": "EveSynapse v0.5.7.3", "upcoming_release": false}
]`

const appPackagesFixture = `[
 {"id": 201, "name": "evesynapse", "version": "v0.5.7.3", "package_type": "generic"}
]`

const appFilesFixture = `[
 {"file_name": "evesynapse-amd64", "file_sha256": "ddadde94b2aa3975bd518b0fc19c16398270c3da3a5661c7bbaf751f7b68e4c1"},
 {"file_name": "evesynapse-arm64", "file_sha256": "ca18a697ec450dd2ffc6859904ac371352ea7ddce4188f608d703105ab09161b"},
 {"file_name": "latest-amd64.json", "file_sha256": "9e5b6d24953cc18b6d1bd32e348a925572132549e39268f084b99559ca193575"},
 {"file_name": "latest-amd64.json.sig", "file_sha256": "bdb805257745e12a627a10777e25d079491663e5f9393b4c8b409cb1e3c7dd89"},
 {"file_name": "latest-arm64.json", "file_sha256": "251a1de3667f34f3e32927fa287742887d8029bb772e0166c16abc0c9382abce"},
 {"file_name": "latest-arm64.json.sig", "file_sha256": "a9cb90d4577a99150e21f7181ed2f3ab70553a1d6055f512bcb403ebbdb13b80"}
]`

// fixtureReleaseFetch serves the fixture payloads the way
// the learner's URLs name them: by project, then by call.
func fixtureReleaseFetch(ctx context.Context, url string) ([]byte, error) {
	storm := strings.Contains(url, "evesynapse-storm")
	switch {
	case strings.Contains(url, "/releases?"):
		if storm {
			return []byte(stormReleasesFixture), nil
		}
		return []byte(appReleasesFixture), nil
	case strings.Contains(url, "/packages?"):
		if storm {
			return []byte(stormPackagesFixture), nil
		}
		return []byte(appPackagesFixture), nil
	case strings.Contains(url, "/packages/101/package_files"):
		return []byte(stormFilesV011Fixture), nil
	case strings.Contains(url, "/packages/100/package_files"):
		return []byte(stormFilesV010Fixture), nil
	case strings.Contains(url, "/packages/201/package_files"):
		return []byte(appFilesFixture), nil
	}
	return nil, errors.New("unexpected URL " + url)
}

// TestLearnReleasesRecordsBuilds: one pass records exactly
// the six binary builds on the fixture payloads (two app
// binaries on one release, two storm binaries on each of two
// releases), the storm v0.1.1 arm64 build then classifies as
// Mainline through the learned path, and the seeded library
// still stands beside it.
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
