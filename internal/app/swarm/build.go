package swarm

// ---------------------------------------------------------------------------
// Build claims: which build each instance is running, proven
// as well as a self-report can prove it.
//
// A claim is the SHA-256 of the instance's own running
// binary, read from its executable file at startup. The Admin
// page's Build column looks the hash up in the release
// library below: a hash the library knows renders as
// Mainline with that release's version label, a hash it does
// not know renders as Fork, and no claim at all renders as —.
//
// The claim is still self-reported metadata for the
// operator's situational awareness: a hash is the sender's
// word about its own binary (nothing stops a modified build
// from copying an official hash), and it gates nothing — the
// trust model (trust.go) never reads it, so approval,
// exclusion, spot-check sampling and the reputation ledger
// are blind to what build a peer runs or claims to run.
// Trust persists across releases: a peer that changes its
// claim — an upgrade, a downgrade, a flip between the
// mainline and fork labels below — keeps its trust row
// exactly as it was, and earns or loses trust at the same
// rate as every other instance.
//
// Claims ride the two membership channels: the seed announce
// seals one (seed.go), and the members list carries one per
// member (api.go). A receiver files the newest claim a
// member's own signed address tuple arrives with; the claim
// itself rides beside the tuple, unsigned, and is stored
// only as display text on the peer row.
// ---------------------------------------------------------------------------

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"sync"
)

// BuildClaim is this instance's own build claim: the SHA-256
// (hex) of the running binary, hashed once at startup from
// the executable file. Empty when the executable cannot be
// read — an instance that cannot name its build makes no
// claim, and its peers show it as unknown.
func BuildClaim() string {
	ownBuildHashOnce.Do(func() {
		ownBuildHash = hashOwnExecutable()
	})
	return ownBuildHash
}

var (
	ownBuildHash     string
	ownBuildHashOnce sync.Once
)

func hashOwnExecutable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BuildClass is a claimed build's class for display: mainline,
// fork, or unknown (no claim heard). The class is computed
// where it is rendered, never stored.
type BuildClass int

const (
	BuildUnknown BuildClass = iota
	BuildMainline
	BuildFork
)

// ReleaseKind names which product a release library entry
// belongs to. The swarm carries two kinds of instance — the
// EveSynapse app and the Storm warming node — and their
// release lines are numbered independently, so an entry's
// label means nothing without the product it belongs to.
type ReleaseKind string

const (
	ReleaseEveSynapse ReleaseKind = "evesynapse"
	ReleaseStorm      ReleaseKind = "storm"
)

// ReleaseBuild is one library entry: the product the build
// belongs to and that release's version label.
type ReleaseBuild struct {
	Kind  ReleaseKind
	Label string
}

// ReleaseLibrary names the known official builds: SHA-256 of
// a published release binary → the product and version label
// of that release. The entries below are the official
// evesynapse binaries (both architectures) of the v0.5.2.x
// line, seeded from the asset digests the GitHub releases API
// reports for each release — every digest verified there,
// none computed or guessed locally.
//
// Storm entries follow the app entries: storm cuts signed
// releases of its own (tag-triggered workflow there), and each
// adds its hashes here, once, the way app releases do. The
// v0.1.0 digests below are from that release's SHA256SUMS,
// whose signature was verified against the storm repo's
// committed release-signing.pub before they were recorded.
//
// The library grows by hand: each future release adds its
// hashes here, once, at release time. Official releases
// before the v0.5.2.x line are not seeded; a peer running one
// shows as a fork until its hashes are added. The library
// decides a display label only — absence is not a verdict
// about the peer, just a fact about the library.
var ReleaseLibrary = map[string]ReleaseBuild{
	// v0.5.2.11
	"6ba22b5a89cd1a379529cda12a39c3aba21f7bebba200463856d89564bd1a091": {ReleaseEveSynapse, "v0.5.2.11"}, // amd64
	"67d304f4e4abcea33e625d3205a20f745cff0a597ff44572f78bf1395392ca3c": {ReleaseEveSynapse, "v0.5.2.11"}, // arm64
	// v0.5.2.10
	"90e1321d7066de809fc143199a2a824b93c1a35e31a6bf45e83826fb4bc555d4": {ReleaseEveSynapse, "v0.5.2.10"}, // amd64
	"18139b81fb608b686e489762ed1d6d7342358ce4d9bb5f42684c85d1e4e1b734": {ReleaseEveSynapse, "v0.5.2.10"}, // arm64
	// v0.5.2.9
	"79d0ddce5d1cface2f27ef1deb9d3a6863a9d62848b4b107e71bdff30e90a1a5": {ReleaseEveSynapse, "v0.5.2.9"}, // amd64
	"d89dcb778654bc5d1aeba60bdf175758a756a8d53b21cf13910d95edfa655b86": {ReleaseEveSynapse, "v0.5.2.9"}, // arm64
	// v0.5.2.8
	"cb6dc4f6c17f67c311a958f0f2d32d25ee42ba7717f0bd52e9c9643a43324665": {ReleaseEveSynapse, "v0.5.2.8"}, // amd64
	"c339e672c27929acc7ee94f73da0509b19fc27591ccec16f9e4921429a5e086e": {ReleaseEveSynapse, "v0.5.2.8"}, // arm64
	// v0.5.2.7
	"c7c175a1218f11c64ce2c01ce4085b66783cf7465917423911adcd3fd9779d4a": {ReleaseEveSynapse, "v0.5.2.7"}, // amd64
	"b13d5c8bfc55a318869d78e8b4e671e6694a361567fe74abac07ee98848d86fd": {ReleaseEveSynapse, "v0.5.2.7"}, // arm64
	// v0.5.2.6
	"c591e938616fde56979af84538c9d7799463b714f58a4a1433397de4ce3ec3d9": {ReleaseEveSynapse, "v0.5.2.6"}, // amd64
	"2c73b7f732de2719f89e3d4133ec02d148c451d5aa0dfdb984ceeb42883e26d8": {ReleaseEveSynapse, "v0.5.2.6"}, // arm64
	// v0.5.2.5
	"983b9455a15d0d73ac1a3d4fe2207a9b3c98da3c4aa6e823d3014c95695342e2": {ReleaseEveSynapse, "v0.5.2.5"}, // amd64
	"04937c1698ee4545bcf8607144b6616bd19226d8d2f1eb0b5061d01f217e9a24": {ReleaseEveSynapse, "v0.5.2.5"}, // arm64
	// v0.5.2.4
	"7d2108524e5e102f6b3897d7fe218ad9624c04b80094c7f67117140dc45379a7": {ReleaseEveSynapse, "v0.5.2.4"}, // amd64
	"e44587bb2f4603ca938d0fe2742dbc27721ebaac987cabed918fa5a63c04bc52": {ReleaseEveSynapse, "v0.5.2.4"}, // arm64
	// v0.5.2.3
	"b2abc97918dda7fac2e2f5869ad8356f8d69b7733222e8e7f3379cff6b686dc6": {ReleaseEveSynapse, "v0.5.2.3"}, // amd64
	"841555b4e16adc7a82067c40fb94e3a92b5679114103541eb93149bbc8132412": {ReleaseEveSynapse, "v0.5.2.3"}, // arm64
	// v0.5.2.2
	"372b382c29df838681310a25705698d8e3555e15cd6739abb1ae383b5a0ef7f8": {ReleaseEveSynapse, "v0.5.2.2"}, // amd64
	"8ad04cd5b5a36f4a892b88f096b21468885b20088a8c586901f3fc14a5623299": {ReleaseEveSynapse, "v0.5.2.2"}, // arm64
	// v0.5.2.1
	"a86313308dca6098a6594f931be9f769a3240f0879752607982e2991ea9da794": {ReleaseEveSynapse, "v0.5.2.1"}, // amd64
	"0b970754d955496495194077ca264302cfaeace3be1a3e98c8c331d4cd26da64": {ReleaseEveSynapse, "v0.5.2.1"}, // arm64
	// v0.5.2.0
	"f7a3e2163d3f8e58d7d79c7810c5421d1c83c28596c0df6c168e7e440abc99a2": {ReleaseEveSynapse, "v0.5.2.0"}, // amd64
	"aeb5bfa7d997c778a5944bc2b3c8969b537bb39aecd1e16765970a7fce355d4e": {ReleaseEveSynapse, "v0.5.2.0"}, // arm64
	// Storm v0.1.0
	"efab584ee7240a2b4844f5dd4c0fd5a15556e24b600dd91c89ba6c1a3d96f0d2": {ReleaseStorm, "v0.1.0"}, // storm amd64
	"120e2f4d3f8c4646f2f9b9be0a8fcf7b55c4c137db9c3e688fbfef3bf07a6eaf": {ReleaseStorm, "v0.1.0"}, // storm arm64
}

// ReleaseFor looks one claim up in the release library: the
// entry it names, and whether it names any.
func ReleaseFor(claim string) (ReleaseBuild, bool) {
	rel, ok := ReleaseLibrary[strings.TrimSpace(claim)]
	return rel, ok
}

// ClassifyBuild sorts one peer's claim for the Admin page:
// no claim is Unknown, a claim the release library names is
// Mainline, and any other claimed hash is a Fork (a build no
// official release produced — a fact, not an accusation).
func ClassifyBuild(claim string) BuildClass {
	claim = strings.TrimSpace(claim)
	if claim == "" {
		return BuildUnknown
	}
	if _, ok := ReleaseLibrary[claim]; ok {
		return BuildMainline
	}
	return BuildFork
}

// BuildLabel renders one peer's Build cell: the class and
// what backs it — "Mainline · v0.5.2.11" for an app release
// (the release's label, not the raw hash), "Mainline · Storm
// v1.0.0" for a storm release, "Fork" for a hash no release
// matches, or "—" when the peer has never made a claim.
func BuildLabel(claim string) string {
	claim = strings.TrimSpace(claim)
	switch ClassifyBuild(claim) {
	case BuildMainline:
		rel := ReleaseLibrary[claim]
		if rel.Kind == ReleaseStorm {
			return "Mainline · Storm " + rel.Label
		}
		return "Mainline · " + rel.Label
	case BuildFork:
		return "Fork"
	default:
		return "—"
	}
}
