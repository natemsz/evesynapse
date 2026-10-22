package swarm

import "testing"

// The releases fixtures below are the GitHub releases API's
// real payloads for the two learner sources, recorded live
// and cut to the fields the API carries: every release's tag
// and every asset's name and digest are as published. The
// storm fixture carries both storm releases (v0.1.1 and
// v0.1.0); the app fixture carries v0.5.7.3.

const stormReleasesFixture = `[
 {
  "id": 420739059,
  "tag_name": "v0.1.1",
  "name": "Storm v0.1.1",
  "draft": false,
  "prerelease": false,
  "published_at": "2026-10-22T13:10:44Z",
  "assets": [
   {
    "id": 665921095,
    "name": "SHA256SUMS",
    "content_type": "application/octet-stream",
    "size": 338,
    "download_count": 3,
    "digest": "sha256:bb16500f48cea154b2bd19bee52cbae03dd13738f362f02b4391b7ef0b221377",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.1/SHA256SUMS"
   },
   {
    "id": 665921133,
    "name": "SHA256SUMS.sig",
    "content_type": "application/pgp-signature",
    "size": 89,
    "download_count": 1,
    "digest": "sha256:421c79318c4db220ce1acfb64703224ebc6d8705331e8033d60e2ea1569e0129",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.1/SHA256SUMS.sig"
   },
   {
    "id": 665921093,
    "name": "storm-amd64",
    "content_type": "application/octet-stream",
    "size": 17520834,
    "download_count": 0,
    "digest": "sha256:9ec717334ac81d5229bb2e245c77a4fd829de63d7115b3d90344a5fda8371b3e",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.1/storm-amd64"
   },
   {
    "id": 665921099,
    "name": "storm-arm64",
    "content_type": "application/octet-stream",
    "size": 16644057,
    "download_count": 1,
    "digest": "sha256:707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.1/storm-arm64"
   },
   {
    "id": 665921088,
    "name": "storm-linux-amd64.tar.gz",
    "content_type": "application/x-gtar",
    "size": 10035613,
    "download_count": 0,
    "digest": "sha256:c9e5da88df0be2503e767ccb03ab7d10c84c86ae10f6379ad170232c75d015c2",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.1/storm-linux-amd64.tar.gz"
   },
   {
    "id": 665921096,
    "name": "storm-linux-arm64.tar.gz",
    "content_type": "application/x-gtar",
    "size": 9362611,
    "download_count": 0,
    "digest": "sha256:6be01a65db1505255093457e41b56dfa3e7518b003937b608706140378310b89",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.1/storm-linux-arm64.tar.gz"
   }
  ]
 },
 {
  "id": 416691579,
  "tag_name": "v0.1.0",
  "name": "Storm v0.1.0",
  "draft": false,
  "prerelease": false,
  "published_at": "2026-10-18T20:27:10Z",
  "assets": [
   {
    "id": 654411363,
    "name": "SHA256SUMS",
    "content_type": "application/octet-stream",
    "size": 338,
    "download_count": 1,
    "digest": "sha256:5064f082088c76723ac4250f6432b87cb673090481e20483f1cde097a23912cd",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.0/SHA256SUMS"
   },
   {
    "id": 654411386,
    "name": "SHA256SUMS.sig",
    "content_type": "application/pgp-signature",
    "size": 89,
    "download_count": 1,
    "digest": "sha256:04319a956a072f9952264d11098b759fd00f861763abceda80203ec97cf22255",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.0/SHA256SUMS.sig"
   },
   {
    "id": 654411364,
    "name": "storm-amd64",
    "content_type": "application/octet-stream",
    "size": 17520205,
    "download_count": 0,
    "digest": "sha256:efab584ee7240a2b4844f5dd4c0fd5a15556e24b600dd91c89ba6c1a3d96f0d2",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.0/storm-amd64"
   },
   {
    "id": 654411360,
    "name": "storm-arm64",
    "content_type": "application/octet-stream",
    "size": 16641708,
    "download_count": 1,
    "digest": "sha256:120e2f4d3f8c4646f2f9b9be0a8fcf7b55c4c137db9c3e688fbfef3bf07a6eaf",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.0/storm-arm64"
   },
   {
    "id": 654411361,
    "name": "storm-linux-amd64.tar.gz",
    "content_type": "application/x-gtar",
    "size": 10034809,
    "download_count": 0,
    "digest": "sha256:89cc081c4716ffeeede82202e5401c4a8383821f1c5a94733c68e8da134718ba",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.0/storm-linux-amd64.tar.gz"
   },
   {
    "id": 654411358,
    "name": "storm-linux-arm64.tar.gz",
    "content_type": "application/x-gtar",
    "size": 9360766,
    "download_count": 0,
    "digest": "sha256:5ccb016887500e4198e06a6c10ab8800c3f20430ea8d409bab784c7559ea5f3f",
    "browser_download_url": "https://github.com/natemsz/evesynapse-storm/releases/download/v0.1.0/storm-linux-arm64.tar.gz"
   }
  ]
 }
]`

const appReleasesFixture = `[
 {
  "id": 421278965,
  "tag_name": "v0.5.7.3",
  "name": "EveSynapse v0.5.7.3",
  "draft": false,
  "prerelease": false,
  "published_at": "2026-10-22T22:52:39Z",
  "assets": [
   {
    "id": 667316973,
    "name": "evesynapse-amd64",
    "content_type": "application/octet-stream",
    "size": 42206691,
    "download_count": 0,
    "digest": "sha256:ddadde94b2aa3975bd518b0fc19c16398270c3da3a5661c7bbaf751f7b68e4c1",
    "browser_download_url": "https://github.com/natemsz/evesynapse/releases/download/v0.5.7.3/evesynapse-amd64"
   },
   {
    "id": 667316976,
    "name": "evesynapse-arm64",
    "content_type": "application/octet-stream",
    "size": 39572567,
    "download_count": 0,
    "digest": "sha256:ca18a697ec450dd2ffc6859904ac371352ea7ddce4188f608d703105ab09161b",
    "browser_download_url": "https://github.com/natemsz/evesynapse/releases/download/v0.5.7.3/evesynapse-arm64"
   },
   {
    "id": 667316977,
    "name": "latest-amd64.json",
    "content_type": "application/json",
    "size": 113,
    "download_count": 0,
    "digest": "sha256:9e5b6d24953cc18b6d1bd32e348a925572132549e39268f084b99559ca193575",
    "browser_download_url": "https://github.com/natemsz/evesynapse/releases/download/v0.5.7.3/latest-amd64.json"
   },
   {
    "id": 667316993,
    "name": "latest-amd64.json.sig",
    "content_type": "application/pgp-signature",
    "size": 89,
    "download_count": 0,
    "digest": "sha256:bdb805257745e12a627a10777e25d079491663e5f9393b4c8b409cb1e3c7dd89",
    "browser_download_url": "https://github.com/natemsz/evesynapse/releases/download/v0.5.7.3/latest-amd64.json.sig"
   },
   {
    "id": 667316975,
    "name": "latest-arm64.json",
    "content_type": "application/json",
    "size": 113,
    "download_count": 0,
    "digest": "sha256:251a1de3667f34f3e32927fa287742887d8029bb772e0166c16abc0c9382abce",
    "browser_download_url": "https://github.com/natemsz/evesynapse/releases/download/v0.5.7.3/latest-arm64.json"
   },
   {
    "id": 667316974,
    "name": "latest-arm64.json.sig",
    "content_type": "application/pgp-signature",
    "size": 89,
    "download_count": 0,
    "digest": "sha256:a9cb90d4577a99150e21f7181ed2f3ab70553a1d6055f512bcb403ebbdb13b80",
    "browser_download_url": "https://github.com/natemsz/evesynapse/releases/download/v0.5.7.3/latest-arm64.json.sig"
   }
  ]
 }
]`

// TestParseReleaseBuildsStorm: the storm payload records
// exactly the four binary builds -- two architectures on two
// releases -- under their tags, and nothing else: the
// bundles, the checksum file and its signature ride the same
// releases and are not builds.
func TestParseReleaseBuildsStorm(t *testing.T) {
	pairs, err := parseReleaseBuilds(releaseSources[1], []byte(stormReleasesFixture))
	if err != nil {
		t.Fatalf("parse storm payload: %v", err)
	}
	want := map[string]string{
		"9ec717334ac81d5229bb2e245c77a4fd829de63d7115b3d90344a5fda8371b3e": "v0.1.1", // storm-amd64
		"707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9": "v0.1.1", // storm-arm64
		"efab584ee7240a2b4844f5dd4c0fd5a15556e24b600dd91c89ba6c1a3d96f0d2": "v0.1.0", // storm-amd64
		"120e2f4d3f8c4646f2f9b9be0a8fcf7b55c4c137db9c3e688fbfef3bf07a6eaf": "v0.1.0", // storm-arm64
	}
	if len(pairs) != 4 {
		t.Fatalf("storm payload yielded %d builds, want 4: %+v", len(pairs), pairs)
	}
	for _, p := range pairs {
		label, ok := want[p.hash]
		if !ok {
			t.Fatalf("recorded a non-binary asset's digest %s as a build", p.hash)
		}
		if p.entry.Label != label || p.entry.Kind != ReleaseStorm {
			t.Fatalf("build %s recorded as %+v, want storm %s", p.hash[:8], p.entry, label)
		}
		delete(want, p.hash)
	}
	if len(want) != 0 {
		t.Fatalf("binary builds missing from the parse: %v", want)
	}
}

// TestParseReleaseBuildsApp: the app payload records its two
// binaries under the release's tag; the update metadata and
// its signatures are not builds.
func TestParseReleaseBuildsApp(t *testing.T) {
	pairs, err := parseReleaseBuilds(releaseSources[0], []byte(appReleasesFixture))
	if err != nil {
		t.Fatalf("parse app payload: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("app payload yielded %d builds, want 2: %+v", len(pairs), pairs)
	}
	for _, p := range pairs {
		if p.entry.Kind != ReleaseEveSynapse || p.entry.Label != "v0.5.7.3" {
			t.Fatalf("build %s recorded as %+v, want evesynapse v0.5.7.3", p.hash[:8], p.entry)
		}
	}
}

// TestParseReleaseBuildsSkips: a draft release, an asset
// with no digest and an asset whose digest is not a SHA-256
// all record nothing, and a body that is not a releases
// payload is an error, not an empty learn.
func TestParseReleaseBuildsSkips(t *testing.T) {
	body := []byte(`[
		{"tag_name": "v9.9.9", "draft": true, "assets": [
			{"name": "storm-amd64", "digest": "sha256:707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9"}
		]},
		{"tag_name": "v9.9.8", "draft": false, "assets": [
			{"name": "storm-amd64", "digest": ""},
			{"name": "storm-arm64", "digest": "sha256:not-a-hash"},
			{"name": "storm-linux-arm64.tar.gz", "digest": "sha256:6be01a65db1505255093457e41b56dfa3e7518b003937b608706140378310b89"}
		]}
	]`)
	pairs, err := parseReleaseBuilds(releaseSources[1], body)
	if err != nil {
		t.Fatalf("parse skip payload: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("skip payload yielded %d builds, want 0: %+v", len(pairs), pairs)
	}
	if _, err := parseReleaseBuilds(releaseSources[1], []byte(`{"message":"rate limited"}`)); err == nil {
		t.Fatal("a non-payload body parsed without error")
	}
}

// TestClassifyLearnedBuild: a hash only the learned set knows
// classifies as Mainline with its learned label -- the storm
// v0.1.1 case -- while a seeded hash still resolves, a
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
