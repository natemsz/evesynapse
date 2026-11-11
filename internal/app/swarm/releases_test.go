package swarm

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"

	"evesynapse/internal/releasesig"
)

// releaseKey is a signing key the tests trust in place of the
// release keys compiled into the build.
func releaseKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// signedManifest is one per-architecture manifest and its .sig,
// signed by priv.
func signedManifest(priv ed25519.PrivateKey, version, arch, sha string) (string, string) {
	m := fmt.Sprintf(`{"version":"%s","arch":"%s","sha256":"%s"}`, version, arch, sha)
	return m, string(releasesig.Sign([]ed25519.PrivateKey{priv}, []byte(m)))
}

// mapFetch answers from a URL map; a missing URL is a 404.
func mapFetch(files map[string]string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, url string) ([]byte, error) {
		body, ok := files[url]
		if !ok {
			return nil, errReleaseNotFound
		}
		return []byte(body), nil
	}
}

const (
	hashAmd64 = "ddadde94b2aa3975bd518b0fc19c16398270c3da3a5661c7bbaf751f7b68e4c1"
	hashArm64 = "ca18a697ec450dd2ffc6859904ac371352ea7ddce4188f608d703105ab09161b"
)

// TestLearnSourceApp: an app release's builds come from its signed
// manifests, one per architecture, under the release's tag. A
// manifest signed by a key this build does not trust, a draft, and
// an asset served from anywhere but the forge contribute nothing.
func TestLearnSourceApp(t *testing.T) {
	pub, priv := releaseKey(t)
	_, stranger := releaseKey(t)
	src := releaseSources[0]
	dl := forgejoBaseURL + "/EveSynapse/evesynapse/releases/download/"
	amd, amdSig := signedManifest(priv, "0.8.2.0", "amd64", hashAmd64)
	arm, armSig := signedManifest(priv, "0.8.2.0", "arm64", hashArm64)
	forged, forgedSig := signedManifest(stranger, "0.8.1.9", "amd64", strings.Repeat("a", 64))
	off, offSig := signedManifest(priv, "0.8.1.8", "amd64", strings.Repeat("b", 64))
	list := `[
 {"tag_name":"v0.8.2.0","draft":false,"assets":[
  {"name":"latest-amd64.json","browser_download_url":"` + dl + `v0.8.2.0/latest-amd64.json"},
  {"name":"latest-amd64.json.sig","browser_download_url":"` + dl + `v0.8.2.0/latest-amd64.json.sig"},
  {"name":"latest-arm64.json","browser_download_url":"` + dl + `v0.8.2.0/latest-arm64.json"},
  {"name":"latest-arm64.json.sig","browser_download_url":"` + dl + `v0.8.2.0/latest-arm64.json.sig"}]},
 {"tag_name":"v0.8.1.9","draft":false,"assets":[
  {"name":"latest-amd64.json","browser_download_url":"` + dl + `v0.8.1.9/latest-amd64.json"},
  {"name":"latest-amd64.json.sig","browser_download_url":"` + dl + `v0.8.1.9/latest-amd64.json.sig"}]},
 {"tag_name":"v0.8.1.8","draft":false,"assets":[
  {"name":"latest-amd64.json","browser_download_url":"https://elsewhere.example/latest-amd64.json"},
  {"name":"latest-amd64.json.sig","browser_download_url":"https://elsewhere.example/latest-amd64.json.sig"}]},
 {"tag_name":"v0.8.3.0","draft":true,"assets":[]}
]`
	fetch := mapFetch(map[string]string{
		releaseListURL(src):                               list,
		dl + "v0.8.2.0/latest-amd64.json":                 amd,
		dl + "v0.8.2.0/latest-amd64.json.sig":             amdSig,
		dl + "v0.8.2.0/latest-arm64.json":                 arm,
		dl + "v0.8.2.0/latest-arm64.json.sig":             armSig,
		dl + "v0.8.1.9/latest-amd64.json":                 forged,
		dl + "v0.8.1.9/latest-amd64.json.sig":             forgedSig,
		"https://elsewhere.example/latest-amd64.json":     off,
		"https://elsewhere.example/latest-amd64.json.sig": offSig,
	})
	got, err := learnSource(context.Background(), fetch, src, []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatalf("learn: %v", err)
	}
	want := map[string]bool{hashAmd64: true, hashArm64: true}
	if len(got) != 2 {
		t.Fatalf("learned %d builds, want 2: %+v", len(got), got)
	}
	for _, b := range got {
		if !want[b.hash] || b.entry.Kind != ReleaseEveSynapse || b.entry.Label != "v0.8.2.0" {
			t.Fatalf("unexpected build %+v", b)
		}
	}
}

// TestLearnSourceStorm: a storm release's builds come from its
// SHA256SUMS, the two binaries only, never the archives beside them.
func TestLearnSourceStorm(t *testing.T) {
	src := releaseSources[1]
	dl := forgejoBaseURL + "/EveSynapse/evesynapse-storm/releases/download/v0.1.1/"
	fetch := mapFetch(map[string]string{
		releaseListURL(src): `[{"tag_name":"v0.1.1","draft":false,"assets":[{"name":"SHA256SUMS","browser_download_url":"` + dl + `SHA256SUMS"}]}]`,
		dl + "SHA256SUMS": "9ec717334ac81d5229bb2e245c77a4fd829de63d7115b3d90344a5fda8371b3e  storm-amd64\n" +
			"707d8bb6048c2cc8ab3d9ad402c55e0c06324ba81487da19b5a75b72348b0db9  storm-arm64\n" +
			"c9e5da88df0be2503e767ccb03ab7d10c84c86ae10f6379ad170232c75d015c2  storm-linux-amd64.tar.gz\n",
	})
	got, err := learnSource(context.Background(), fetch, src, nil)
	if err != nil {
		t.Fatalf("learn: %v", err)
	}
	if len(got) != 2 || got[0].entry.Label != "v0.1.1" || got[0].entry.Kind != ReleaseStorm {
		t.Fatalf("learned %+v, want the two storm binaries under v0.1.1", got)
	}
}

// TestLearnMirror: the mirror's channel manifests name the current
// builds, labeled by their version, taken only when their signatures
// verify; a channel the mirror does not carry is skipped.
func TestLearnMirror(t *testing.T) {
	pub, priv := releaseKey(t)
	_, stranger := releaseKey(t)
	amd, amdSig := signedManifest(priv, "0.8.2.0", "amd64", hashAmd64)
	arm, armSig := signedManifest(stranger, "0.8.2.0", "arm64", hashArm64)
	base := releaseMirrorBaseURL + "/"
	fetch := mapFetch(map[string]string{
		base + "latest-amd64.json":     amd,
		base + "latest-amd64.json.sig": amdSig,
		base + "latest-arm64.json":     arm,
		base + "latest-arm64.json.sig": armSig,
	})
	got, err := learnMirror(context.Background(), fetch, releaseSources[0], []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatalf("learn mirror: %v", err)
	}
	if len(got) != 1 || got[0].hash != hashAmd64 || got[0].entry.Label != "v0.8.2.0" {
		t.Fatalf("learned %+v, want only the trusted amd64 build as v0.8.2.0", got)
	}
}

// TestClassifyLearnedBuild: a hash only the learned set knows
// classifies as Mainline with its learned label, the storm
// v0.1.1 case, while a seeded hash still resolves, a
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
