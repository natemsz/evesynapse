package swarm

// ---------------------------------------------------------------------------
// The release learner: the release library's second,
// self-keeping half (build.go has the first, seeded one).
//
// On a slow cadence the learner reads the recent releases of
// my two Forgejo projects and records every binary build's
// SHA-256 in the release_builds table, under the release's tag and
// the project's product kind. An app release carries a signed
// manifest per architecture (latest-<arch>.json and its .sig): the
// learner takes the hash from the manifest, and only when the
// signature verifies under the release keys this build trusts. A
// storm release carries a SHA256SUMS file; storm's releases are
// published on its GitHub mirror, so the learner reads them there
// as well, and either home is enough. A recorded hash is
// remembered for good: it stays known after its release scrolls off
// the first page, so the library keeps itself current without
// anyone seeding new hashes by hand.
//
// When Forgejo cannot be read, the app's builds are learned from
// the project's static mirror (cdn.synap6.io) instead: its channel
// manifests name the current builds, and they are taken only when
// their signatures verify, so the mirror's host is never trusted.
//
// The projects are hardcoded to mine because "mainline" is
// defined by my releases: a fork self-hosting EveSynapse learns
// the same library.
//
// Failure tolerance is the shape of the thing: a fetch or parse
// failure changes nothing (recorded builds stay recorded, the
// in-memory snapshot stays as it was) and the pass is simply due
// again later: once a day after a success, no sooner than six hours
// after a failure. Nothing here is on a render path, and the
// recorded data feeds the same display-only classification as the
// seeded library: the trust model never reads it.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/releasesig"
)

// releaseSource is one Forgejo project the learner reads: where it
// lives, the product name its binary files carry, the library kind
// its builds belong to, and whether its manifests are signed with
// the app's release keys (and mirrored on the CDN).
type releaseSource struct {
	repo    string
	product string
	kind    ReleaseKind
	signed  bool
	github  string // the GitHub mirror that also publishes its releases, "" for none
}

// releaseSources are the learner's projects: the app and the storm
// node, the two products the swarm carries.
var releaseSources = []releaseSource{
	{"EveSynapse/evesynapse", "evesynapse", ReleaseEveSynapse, true, ""},
	// Storm's tag-triggered release workflow publishes on its
	// GitHub mirror, so its builds are read there as well.
	{"EveSynapse/evesynapse-storm", "storm", ReleaseStorm, false, "natemsz/evesynapse-storm"},
}

const (
	// releaseLearnInterval is the cadence after a successful
	// pass; releaseRetryInterval is the soonest a failed pass
	// is tried again.
	releaseLearnInterval = 24 * time.Hour
	releaseRetryInterval = 6 * time.Hour
	releaseFetchTimeout  = 30 * time.Second
	releasePerPage       = 10
	releaseMaxBodyBytes  = 1 << 20
)

// releaseArches are the architectures a release builds.
var releaseArches = []string{"amd64", "arm64"}

// forgejoBaseURL is the forge the projects live on, and
// releaseMirrorBaseURL the app's static release mirror. Vars so
// tests can point them at local servers.
var (
	forgejoBaseURL       = "https://natems.dev"
	releaseMirrorBaseURL = "https://cdn.synap6.io/evesynapse"
	// githubAPIBaseURL lists a mirror's releases; githubBaseURL is
	// where their files are served from.
	githubAPIBaseURL = "https://api.github.com"
	githubBaseURL    = "https://github.com"
)

// releaseTrustedKeys is the set of keys an app manifest has to be
// signed with: the ones compiled into this build. A var so tests
// can stand in a key of their own.
var releaseTrustedKeys = releasesig.Trusted

// releaseListURL is one project's recent releases.
func releaseListURL(src releaseSource) string {
	return fmt.Sprintf("%s/api/v1/repos/%s/releases?limit=%d", forgejoBaseURL, src.repo, releasePerPage)
}

// githubReleaseListURL is one GitHub mirror's recent releases.
func githubReleaseListURL(src releaseSource) string {
	return fmt.Sprintf("%s/repos/%s/releases?per_page=%d", githubAPIBaseURL, src.github, releasePerPage)
}

// errReleaseNotFound is a file the server does not have.
var errReleaseNotFound = errors.New("not found")

// httpReleaseFetch is the learner's default fetch: one
// unauthenticated GET, bounded by the context and the body cap.
func httpReleaseFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "evesynapse-release-learner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errReleaseNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, releaseMaxBodyBytes))
}

// forgejoRelease is one release in the API's payload, cut to the
// fields the learner reads.
type forgejoRelease struct {
	TagName string         `json:"tag_name"`
	Draft   bool           `json:"draft"`
	Assets  []forgejoAsset `json:"assets"`
}

// forgejoAsset is one file attached to a release.
type forgejoAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// releaseManifest is a signed per-architecture manifest.
type releaseManifest struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
}

// learnedBuild is one (hash, entry) pair read off a release.
type learnedBuild struct {
	hash  string
	entry ReleaseBuild
}

// validSHA256 reports whether s is a hex SHA-256.
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// manifestBuild reads one signed manifest: the build it names when
// the signature verifies under trusted and the manifest is the
// architecture it was asked for, ok false otherwise.
func manifestBuild(src releaseSource, label, arch string, manifest, sig []byte, trusted []ed25519.PublicKey) (learnedBuild, bool) {
	if err := releasesig.Verify(trusted, manifest, sig); err != nil {
		return learnedBuild{}, false
	}
	var m releaseManifest
	if err := json.Unmarshal(manifest, &m); err != nil || m.Arch != arch || !validSHA256(m.SHA256) {
		return learnedBuild{}, false
	}
	if label == "" {
		label = "v" + m.Version
	}
	return learnedBuild{hash: strings.ToLower(m.SHA256), entry: ReleaseBuild{Kind: src.kind, Label: label}}, true
}

// parseSHA256Sums reads a SHA256SUMS file: the builds of the
// product's own binaries ("<hash>  storm-amd64"); archives and
// anything else listed beside them are not builds.
func parseSHA256Sums(src releaseSource, tag string, body []byte) []learnedBuild {
	var out []learnedBuild
	for line := range strings.SplitSeq(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !validSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		for _, arch := range releaseArches {
			if name == src.product+"-"+arch {
				out = append(out, learnedBuild{hash: strings.ToLower(fields[0]), entry: ReleaseBuild{Kind: src.kind, Label: tag}})
			}
		}
	}
	return out
}

// learnSource reads one source's builds off its recent published
// releases on Forgejo. An asset is only fetched from the forge
// itself. A release without readable manifests contributes nothing;
// a failure to read the release list fails the source, and the
// caller keeps what it recorded before.
func learnSource(ctx context.Context, fetch func(context.Context, string) ([]byte, error), src releaseSource, trusted []ed25519.PublicKey) ([]learnedBuild, error) {
	return learnReleaseList(ctx, fetch, src, releaseListURL(src), forgejoBaseURL+"/", trusted)
}

// learnGitHub reads an unsigned source's builds off its GitHub
// mirror's recent releases, the same way: SHA256SUMS files served
// from github.com only.
func learnGitHub(ctx context.Context, fetch func(context.Context, string) ([]byte, error), src releaseSource, trusted []ed25519.PublicKey) ([]learnedBuild, error) {
	return learnReleaseList(ctx, fetch, src, githubReleaseListURL(src), githubBaseURL+"/", trusted)
}

// learnReleaseList reads one release list (Forgejo and GitHub share
// the payload's shape), fetching assets only from assetPrefix.
func learnReleaseList(ctx context.Context, fetch func(context.Context, string) ([]byte, error), src releaseSource, listURL, assetPrefix string, trusted []ed25519.PublicKey) ([]learnedBuild, error) {
	body, err := fetch(ctx, listURL)
	if err != nil {
		return nil, err
	}
	var rels []forgejoRelease
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("releases payload for %s: %w", src.repo, err)
	}
	var out []learnedBuild
	for _, rel := range rels {
		if rel.Draft || rel.TagName == "" {
			continue
		}
		assets := map[string]string{}
		for _, a := range rel.Assets {
			if strings.HasPrefix(a.URL, assetPrefix) {
				assets[a.Name] = a.URL
			}
		}
		if src.signed {
			for _, arch := range releaseArches {
				name := "latest-" + arch + ".json"
				if assets[name] == "" || assets[name+releasesig.SigSuffix] == "" {
					continue
				}
				manifest, err := fetch(ctx, assets[name])
				if err != nil {
					return nil, err
				}
				sig, err := fetch(ctx, assets[name+releasesig.SigSuffix])
				if err != nil {
					return nil, err
				}
				if b, ok := manifestBuild(src, rel.TagName, arch, manifest, sig, trusted); ok {
					out = append(out, b)
				}
			}
			continue
		}
		if url := assets["SHA256SUMS"]; url != "" {
			sums, err := fetch(ctx, url)
			if err != nil {
				return nil, err
			}
			out = append(out, parseSHA256Sums(src, rel.TagName, sums)...)
		}
	}
	return out, nil
}

// learnMirror reads the app's current builds off the static
// mirror's channel manifests (latest and latest-dev), taken only
// when their signatures verify. A channel the mirror does not carry
// is skipped.
func learnMirror(ctx context.Context, fetch func(context.Context, string) ([]byte, error), src releaseSource, trusted []ed25519.PublicKey) ([]learnedBuild, error) {
	var out []learnedBuild
	for _, channel := range []string{"latest", "latest-dev"} {
		for _, arch := range releaseArches {
			url := releaseMirrorBaseURL + "/" + channel + "-" + arch + ".json"
			manifest, err := fetch(ctx, url)
			if errors.Is(err, errReleaseNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			sig, err := fetch(ctx, url+releasesig.SigSuffix)
			if err != nil {
				return nil, err
			}
			if b, ok := manifestBuild(src, "", arch, manifest, sig, trusted); ok {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// LoadLearnedBuilds refreshes the in-memory learned snapshot
// from release_builds. It runs at startup and after every
// learn pass; a failed load keeps the previous snapshot.
func (m *Mod) LoadLearnedBuilds(ctx context.Context) error {
	rows, err := m.Queries.ListLearnedBuilds(ctx)
	if err != nil {
		return err
	}
	byHash := make(map[string]ReleaseBuild, len(rows))
	for _, r := range rows {
		byHash[r.Hash] = ReleaseBuild{Kind: ReleaseKind(r.Kind), Label: r.Label}
	}
	SetLearnedBuilds(byHash)
	return nil
}

// LearnReleases runs one learn pass: read both projects' recent
// releases (the app's from the mirror when Forgejo cannot be
// read), record every binary build's hash, and refresh the
// in-memory snapshot from what is stored. A source that fails to
// fetch, parse or store keeps its previously recorded builds; the
// pass reports the first failure it met after giving every source
// its chance.
func (m *Mod) LearnReleases(ctx context.Context) error {
	fetch := m.ReleaseFetch
	if fetch == nil {
		fetch = httpReleaseFetch
	}
	trusted, err := releaseTrustedKeys()
	if err != nil {
		return fmt.Errorf("release keys: %w", err)
	}
	var firstErr error
	learned := 0
	for _, src := range releaseSources {
		fctx, cancel := context.WithTimeout(ctx, releaseFetchTimeout)
		pairs, err := learnSource(fctx, fetch, src, trusted)
		if err != nil && src.signed {
			logging.Warnf("swarm: release learner: %s on Forgejo: %v; reading the mirror", src.repo, err)
			pairs, err = learnMirror(fctx, fetch, src, trusted)
		}
		if src.github != "" {
			// Either home counts: the source fails only when
			// neither could be read.
			gh, ghErr := learnGitHub(fctx, fetch, src, trusted)
			switch {
			case ghErr == nil:
				pairs, err = append(pairs, gh...), nil
			case err == nil:
				logging.Warnf("swarm: release learner: %s on GitHub: %v", src.github, ghErr)
			}
		}
		cancel()
		for _, p := range pairs {
			if err != nil {
				break
			}
			err = m.Queries.UpsertLearnedBuild(ctx, db.UpsertLearnedBuildParams{
				Hash: p.hash, Kind: string(p.entry.Kind), Label: p.entry.Label,
				LearnedAt: time.Now().UTC(),
			})
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		learned += len(pairs)
	}
	if err := m.LoadLearnedBuilds(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	if learned > 0 {
		logging.Infof("swarm: release learner recorded %d build(s)", learned)
	}
	return firstErr
}

// MaybeLearnReleases is the worker job's pass: when a learn
// is due it runs beside the cycle (Background), never inside
// it, a slow or hung fetch must not hold the cycle up. A
// learn is due at startup, then a day after a successful
// pass and no sooner than six hours after a failed one.
func (m *Mod) MaybeLearnReleases(ctx context.Context) {
	m.LearnMu.Lock()
	if m.LearnBusy || time.Now().Before(m.LearnNext) {
		m.LearnMu.Unlock()
		return
	}
	m.LearnBusy = true
	m.LearnMu.Unlock()
	m.Deps.Background(func() {
		err := m.LearnReleases(ctx)
		m.LearnMu.Lock()
		m.LearnBusy = false
		if err != nil {
			logging.Warnf("swarm: release learner: %v (keeping what was learned before)", err)
			m.LearnNext = time.Now().Add(releaseRetryInterval)
		} else {
			m.LearnNext = time.Now().Add(releaseLearnInterval)
		}
		m.LearnMu.Unlock()
	})
}
