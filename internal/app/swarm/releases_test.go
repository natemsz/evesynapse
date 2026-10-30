package swarm

import (
	"context"
	"fmt"
	"testing"
)

// The fixtures below are in the GitLab API's own shapes for
// the three calls the learner makes — a project's releases,
// its generic packages, and one package's files — carrying
// the same releases and build hashes the projects published
// on GitHub: storm v0.1.1 and v0.1.0, the app v0.5.7.3. The
// hashes moved; the wire shape is GitLab's.

const stormReleasesFixture = `[
 {"tag_name": "v0.1.1", "name": "Storm v0.1.1", "upcoming_release": false, "released_at": "2026-10-22T13:10:44Z"},
 {"tag_name": "v0.1.0", "name": "Storm v0.1.0", "upcoming_release": false, "released_at": "2026-10-18T20:27:10Z"}
]`

const stormPackagesFixture = `[
 {"id": 101, "name": "storm", "version": "v0.1.1", "package_type": "generic", "status": "default"},
 {"id": 100, "name": "storm", "version": "v0.1.0", "package_type": "generic", "status": "default"}
]`

const stormFilesV011Fixture = `[
 {"id": 1101, "package_id": 101, "file_name": "SHA256SUMS", "file_sha256": "bb16500f48cea154b2bd19bee52cbae03dd13738f362f02b4391b7ef0b221377", "size": 338},
 {"id": 1102, "package_id": 101, "file_name": "SHA256SUMS.sig", "file_sha256": "421c79318c4db220ce1acfb64703224ebc6d8705331e8033d60e2ea1569e0129", "size": 89},
 {"id": 1103, "package_id": 101, "file_name": "storm-amd64", "file_sha256": "9ec717334ac81d5229bb2e245c77a4fd829de63d7115b3d90344a5fda8371b3e", "size": 17520834},
 {"id": 1104, "package_id": 101, "file_name": "storm-arm64", "file_sha256": "707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9", "size": 16644057},
 {"id": 1105, "package_id": 101, "file_name": "storm-linux-amd64.tar.gz", "file_sha256": "c9e5da88df0be2503e767ccb03ab7d10c84c86ae10f6379ad170232c75d015c2", "size": 10035613},
 {"id": 1106, "package_id": 101, "file_name": "storm-linux-arm64.tar.gz", "file_sha256": "6be01a65db1505255093457e41b56dfa3e7518b003937b608706140378310b89", "size": 9362611}
]`

const stormFilesV010Fixture = `[
 {"id": 1001, "package_id": 100, "file_name": "SHA256SUMS", "file_sha256": "5064f082088c76723ac4250f6432b87cb673090481e20483f1cde097a23912cd", "size": 338},
 {"id": 1002, "package_id": 100, "file_name": "SHA256SUMS.sig", "file_sha256": "04319a956a072f9952264d11098b759fd00f861763abceda80203ec97cf22255", "size": 89},
 {"id": 1003, "package_id": 100, "file_name": "storm-amd64", "file_sha256": "efab584ee7240a2b4844f5dd4c0fd5a15556e24b600dd91c89ba6c1a3d96f0d2", "size": 17520205},
 {"id": 1004, "package_id": 100, "file_name": "storm-arm64", "file_sha256": "120e2f4d3f8c4646f2f9b9be0a8fcf7b55c4c137db9c3e688fbfef3bf07a6eaf", "size": 16641708},
 {"id": 1005, "package_id": 100, "file_name": "storm-linux-amd64.tar.gz", "file_sha256": "89cc081c4716ffeeede82202e5401c4a8383821f1c5a94733c68e8da134718ba", "size": 10034809},
 {"id": 1006, "package_id": 100, "file_name": "storm-linux-arm64.tar.gz", "file_sha256": "5ccb016887500e4198e06a6c10ab8800c3f20430ea8d409bab784c7559ea5f3f", "size": 9360766}
]`

const appReleasesFixture = `[
 {"tag_name": "v0.5.7.3", "name": "EveSynapse v0.5.7.3", "upcoming_release": false, "released_at": "2026-10-22T22:52:39Z"}
]`

const appPackagesFixture = `[
 {"id": 201, "name": "evesynapse", "version": "v0.5.7.3", "package_type": "generic", "status": "default"}
]`

const appFilesFixture = `[
 {"id": 2101, "package_id": 201, "file_name": "evesynapse-amd64", "file_sha256": "ddadde94b2aa3975bd518b0fc19c16398270c3da3a5661c7bbaf751f7b68e4c1", "size": 42206691},
 {"id": 2102, "package_id": 201, "file_name": "evesynapse-arm64", "file_sha256": "ca18a697ec450dd2ffc6859904ac371352ea7ddce4188f608d703105ab09161b", "size": 39572567},
 {"id": 2103, "package_id": 201, "file_name": "latest-amd64.json", "file_sha256": "9e5b6d24953cc18b6d1bd32e348a925572132549e39268f084b99559ca193575", "size": 113},
 {"id": 2104, "package_id": 201, "file_name": "latest-amd64.json.sig", "file_sha256": "bdb805257745e12a627a10777e25d079491663e5f9393b4c8b409cb1e3c7dd89", "size": 89},
 {"id": 2105, "package_id": 201, "file_name": "latest-arm64.json", "file_sha256": "251a1de3667f34f3e32927fa287742887d8029bb772e0166c16abc0c9382abce", "size": 113},
 {"id": 2106, "package_id": 201, "file_name": "latest-arm64.json.sig", "file_sha256": "a9cb90d4577a99150e21f7181ed2f3ab70553a1d6055f512bcb403ebbdb13b80", "size": 89}
]`

// fixtureFetch stands in for the GitLab API: it answers the
// learner's three calls for one source from the fixtures and
// fails the test on any other address.
func fixtureFetch(t *testing.T, src releaseSource, releases, packages string, files map[int64]string) func(context.Context, string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, rawURL string) ([]byte, error) {
		switch rawURL {
		case releaseReleasesURL(src):
			return []byte(releases), nil
		case releasePackagesURL(src):
			return []byte(packages), nil
		}
		for id, body := range files {
			if rawURL == releasePackageFilesURL(src, id) {
				return []byte(body), nil
			}
		}
		return nil, fmt.Errorf("unexpected fetch %s", rawURL)
	}
}

// TestLearnSourceStorm: the storm source records exactly the
// four binary builds — two architectures on two releases —
// under their tags, and nothing else: the bundles, the
// checksum file and its signature ride in the same packages
// and are not builds.
func TestLearnSourceStorm(t *testing.T) {
	src := releaseSources[1]
	fetch := fixtureFetch(t, src, stormReleasesFixture, stormPackagesFixture, map[int64]string{
		101: stormFilesV011Fixture,
		100: stormFilesV010Fixture,
	})
	pairs, err := learnSource(context.Background(), fetch, src)
	if err != nil {
		t.Fatalf("learn storm source: %v", err)
	}
	want := map[string]string{
		"9ec717334ac81d5229bb2e245c77a4fd829de63d7115b3d90344a5fda8371b3e": "v0.1.1", // storm-amd64
		"707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9": "v0.1.1", // storm-arm64
		"efab584ee7240a2b4844f5dd4c0fd5a15556e24b600dd91c89ba6c1a3d96f0d2": "v0.1.0", // storm-amd64
		"120e2f4d3f8c4646f2f9b9be0a8fcf7b55c4c137db9c3e688fbfef3bf07a6eaf": "v0.1.0", // storm-arm64
	}
	if len(pairs) != 4 {
		t.Fatalf("storm source yielded %d builds, want 4: %+v", len(pairs), pairs)
	}
	for _, p := range pairs {
		label, ok := want[p.hash]
		if !ok {
			t.Fatalf("recorded a non-binary file's hash %s as a build", p.hash)
		}
		if p.entry.Label != label || p.entry.Kind != ReleaseStorm {
			t.Fatalf("build %s recorded as %+v, want storm %s", p.hash[:8], p.entry, label)
		}
		delete(want, p.hash)
	}
	if len(want) != 0 {
		t.Fatalf("binary builds missing from the learn: %v", want)
	}
}

// TestLearnSourceApp: the app source records its two binaries
// under the release's tag; the update metadata and its
// signatures are not builds.
func TestLearnSourceApp(t *testing.T) {
	src := releaseSources[0]
	fetch := fixtureFetch(t, src, appReleasesFixture, appPackagesFixture, map[int64]string{
		201: appFilesFixture,
	})
	pairs, err := learnSource(context.Background(), fetch, src)
	if err != nil {
		t.Fatalf("learn app source: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("app source yielded %d builds, want 2: %+v", len(pairs), pairs)
	}
	for _, p := range pairs {
		if p.entry.Kind != ReleaseEveSynapse || p.entry.Label != "v0.5.7.3" {
			t.Fatalf("build %s recorded as %+v, want evesynapse v0.5.7.3", p.hash[:8], p.entry)
		}
	}
}

// TestLearnSourceSkips: an upcoming release, a release with
// no package behind it, a file with no checksum and a file
// whose checksum is not a SHA-256 all record nothing, and a
// body that is not an API payload is an error, not an empty
// learn.
func TestLearnSourceSkips(t *testing.T) {
	src := releaseSources[1]
	const goodHash = "707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9"
	releases := `[
		{"tag_name": "v9.9.9", "upcoming_release": true},
		{"tag_name": "v9.9.8", "upcoming_release": false},
		{"tag_name": "v9.9.7", "upcoming_release": false}
	]`
	packages := `[
		{"id": 901, "name": "storm", "version": "v9.9.9", "package_type": "generic"},
		{"id": 900, "name": "storm", "version": "v9.9.8", "package_type": "generic"}
	]`
	files := map[int64]string{
		901: `[{"file_name": "storm-amd64", "file_sha256": "` + goodHash + `"}]`,
		900: `[
			{"file_name": "storm-amd64", "file_sha256": ""},
			{"file_name": "storm-arm64", "file_sha256": "not-a-hash"},
			{"file_name": "storm-linux-arm64.tar.gz", "file_sha256": "6be01a65db1505255093457e41b56dfa3e7518b003937b608706140378310b89"}
		]`,
	}
	pairs, err := learnSource(context.Background(), fixtureFetch(t, src, releases, packages, files), src)
	if err != nil {
		t.Fatalf("learn skip payloads: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("skip payloads yielded %d builds, want 0: %+v", len(pairs), pairs)
	}
	bad := fixtureFetch(t, src, `{"message":"404 Project Not Found"}`, packages, files)
	if _, err := learnSource(context.Background(), bad, src); err == nil {
		t.Fatal("a non-payload releases body learned without error")
	}
}

// TestClassifyLearnedBuild: a hash only the learned set knows
// classifies as Mainline with its learned label — the storm
// v0.1.1 case — while a seeded hash still resolves, a
// learned entry wins over a seeded one for the same hash,
// and an unknown hash stays a Fork.
func TestClassifyLearnedBuild(t *testing.T) {
	defer SetLearnedBuilds(LearnedBuilds())
	const stormV011Arm64 = "707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9"
	const seededApp = "90e1321d7066de809fc143199a2a824b93c1a35e31a6bf45e83826fb4bc555d4" // v0.5.2.10 amd64
	SetLearnedBuilds(map[string]ReleaseBuild{
		stormV011Arm64: {Kind: ReleaseStorm, Label: "v0.1.1"},
	})
	if got := ClassifyBuild(stormV011Arm64); got != BuildMainline {
		t.Fatalf("learned storm hash classified %v, want Mainline", got)
	}
	if got := BuildLabel(stormV011Arm64); got != "Mainline · Storm v0.1.1" {
		t.Fatalf("learned storm label %q, want the storm v0.1.1 rendering", got)
	}
	if got := BuildLabel(seededApp); got != "Mainline · v0.5.2.10" {
		t.Fatalf("seeded label beside a learned set %q, want the seeded rendering", got)
	}
	SetLearnedBuilds(map[string]ReleaseBuild{
		stormV011Arm64: {Kind: ReleaseStorm, Label: "v0.1.1"},
		seededApp:      {Kind: ReleaseEveSynapse, Label: "v0.0.0-learned"},
	})
	if got := BuildLabel(seededApp); got != "Mainline · v0.0.0-learned" {
		t.Fatalf("conflicting hash rendered %q, want the learned entry to win", got)
	}
	if got := ClassifyBuild("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); got != BuildFork {
		t.Fatalf("unknown hash classified %v, want Fork", got)
	}
}
