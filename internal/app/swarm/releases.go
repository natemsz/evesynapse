package swarm

// ---------------------------------------------------------------------------
// The release learner: the release library's second,
// self-keeping half (build.go has the first, seeded one).
//
// On a slow cadence the learner fetches the recent releases
// of the owner's two GitLab projects and records every binary
// build's SHA-256 — the Packages API reports one per package
// file — in the release_builds table, under the release's
// tag and the project's product kind. A recorded hash is
// remembered for good: it stays known after its release
// scrolls off the API's first page, so the library keeps
// itself current without anyone seeding new hashes by hand
// when a release ships.
//
// The projects are hardcoded to the owner's because
// "mainline" is defined by his releases: a build is mainline
// when one of these two projects published it. A fork
// self-hosting EveSynapse learns the same library — the
// upstream's builds are the mainline whatever instance asks.
//
// Failure tolerance is the shape of the thing: a fetch or
// parse failure changes nothing (the previously recorded
// builds stay recorded, the in-memory snapshot stays as it
// was) and the pass is simply due again later. The cadence
// is deliberately slow — once a day after a success, no
// sooner than six hours after a failure — which the
// unauthenticated GitLab API allowance absorbs without
// noticing. Nothing here is on a render path, and the
// recorded data feeds the same display-only classification
// as the seeded library: the trust model never reads it.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// releaseSource is one GitLab project the learner reads:
// where it lives, the product name its binary files carry,
// and the library kind its builds belong to.
type releaseSource struct {
	project string
	product string
	kind    ReleaseKind
}

// releaseSources are the learner's projects: the app and
// the storm node, the two products the swarm carries.
var releaseSources = []releaseSource{
	{"natemsz/evesynapse", "evesynapse", ReleaseEveSynapse},
	{"natemsz/evesynapse-storm", "storm", ReleaseStorm},
}

const (
	// releaseLearnInterval is the cadence after a successful
	// pass; releaseRetryInterval is the soonest a failed pass
	// is tried again.
	releaseLearnInterval = 24 * time.Hour
	releaseRetryInterval = 6 * time.Hour
	releaseFetchTimeout  = 30 * time.Second
	releasePerPage       = 10
)

// gitlabAPIBaseURL is the GitLab API root the learner reads
// from. A var so tests can point it at a local server.
var gitlabAPIBaseURL = "https://gitlab.com/api/v4"

// gitlabProjectAPI is the API path prefix for one source's
// project: the project path, URL-escaped as the GitLab API
// wants it.
func gitlabProjectAPI(src releaseSource) string {
	return fmt.Sprintf("%s/projects/%s", gitlabAPIBaseURL, url.PathEscape(src.project))
}

// releaseReleasesURL lists a source's recent releases.
func releaseReleasesURL(src releaseSource) string {
	return fmt.Sprintf("%s/releases?per_page=%d", gitlabProjectAPI(src), releasePerPage)
}

// releasePackagesURL lists the source's generic packages
// under its product name: one package per published
// release, its version the release's tag.
func releasePackagesURL(src releaseSource) string {
	return fmt.Sprintf("%s/packages?package_type=generic&package_name=%s&per_page=100",
		gitlabProjectAPI(src), url.QueryEscape(src.product))
}

// releasePackageFilesURL lists one package's files.
func releasePackageFilesURL(src releaseSource, packageID int64) string {
	return fmt.Sprintf("%s/packages/%d/package_files?per_page=100", gitlabProjectAPI(src), packageID)
}

// httpReleaseFetch is the built-in ReleaseFetch: one GET,
// the body capped well past any real API page.
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitLab API answered %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// gitlabRelease is one release in the API's payload, cut to
// the fields the learner reads.
type gitlabRelease struct {
	TagName  string `json:"tag_name"`
	Upcoming bool   `json:"upcoming_release"`
}

// gitlabPackage is one generic package in the API's payload:
// a published release's files live under the package whose
// name is the product and whose version is the release's tag.
type gitlabPackage struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// gitlabPackageFile is one file in a package, with the
// SHA-256 the API reports for it, bare hex.
type gitlabPackageFile struct {
	Name   string `json:"file_name"`
	SHA256 string `json:"file_sha256"`
}

// learnedBuild is one (hash, entry) pair read off a release.
type learnedBuild struct {
	hash  string
	entry ReleaseBuild
}

// parseReleaseTags reads one project's releases payload and
// returns the tags of the releases that are out: a release
// still upcoming is announced, not published, and an
// untagged entry is not a release at all.
func parseReleaseTags(src releaseSource, body []byte) ([]string, error) {
	var rels []gitlabRelease
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("releases payload for %s: %w", src.project, err)
	}
	var tags []string
	for _, rel := range rels {
		if rel.Upcoming || rel.TagName == "" {
			continue
		}
		tags = append(tags, rel.TagName)
	}
	return tags, nil
}

// parseReleasePackages reads one project's packages payload
// and returns its packages by version, keeping only the ones
// named for the source's product.
func parseReleasePackages(src releaseSource, body []byte) (map[string]int64, error) {
	var pkgs []gitlabPackage
	if err := json.Unmarshal(body, &pkgs); err != nil {
		return nil, fmt.Errorf("packages payload for %s: %w", src.project, err)
	}
	byVersion := make(map[string]int64, len(pkgs))
	for _, p := range pkgs {
		if p.Name == src.product && p.Version != "" {
			byVersion[p.Version] = p.ID
		}
	}
	return byVersion, nil
}

// parsePackageBuilds reads one package's files payload and
// returns the build of every binary file in it. A binary
// file is the product's own executable for one architecture
// — evesynapse-amd64, storm-arm64 — named exactly; checksum
// files, signatures and manifests ride in the same package
// and are not builds, so they are skipped by name, never
// recorded. A file whose checksum is missing or not a
// SHA-256 is skipped the same way.
func parsePackageBuilds(src releaseSource, tag string, body []byte) ([]learnedBuild, error) {
	var files []gitlabPackageFile
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("package files payload for %s %s: %w", src.project, tag, err)
	}
	var out []learnedBuild
	for _, f := range files {
		if f.Name != src.product+"-amd64" && f.Name != src.product+"-arm64" {
			continue
		}
		if len(f.SHA256) != 64 {
			continue
		}
		if _, err := hex.DecodeString(f.SHA256); err != nil {
			continue
		}
		out = append(out, learnedBuild{
			hash:  strings.ToLower(f.SHA256),
			entry: ReleaseBuild{Kind: src.kind, Label: tag},
		})
	}
	return out, nil
}

// learnSource reads one source's builds: the tags of its
// published releases, the package standing behind each tag,
// and the binary files in those packages. A release with no
// package behind it has published no files the learner can
// hash, so it contributes nothing; any fetch or parse
// failure fails the source, and the caller keeps what it
// recorded before.
func learnSource(ctx context.Context, fetch func(context.Context, string) ([]byte, error), src releaseSource) ([]learnedBuild, error) {
	body, err := fetch(ctx, releaseReleasesURL(src))
	if err != nil {
		return nil, err
	}
	tags, err := parseReleaseTags(src, body)
	if err != nil {
		return nil, err
	}
	body, err = fetch(ctx, releasePackagesURL(src))
	if err != nil {
		return nil, err
	}
	packages, err := parseReleasePackages(src, body)
	if err != nil {
		return nil, err
	}
	var out []learnedBuild
	for _, tag := range tags {
		packageID, ok := packages[tag]
		if !ok {
			continue
		}
		body, err := fetch(ctx, releasePackageFilesURL(src, packageID))
		if err != nil {
			return nil, err
		}
		pairs, err := parsePackageBuilds(src, tag, body)
		if err != nil {
			return nil, err
		}
		out = append(out, pairs...)
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

// LearnReleases runs one learn pass: fetch both projects'
// recent releases, record every binary build's hash, and
// refresh the in-memory snapshot from what is stored. A
// source that fails to fetch, parse or store keeps its
// previously recorded builds; the pass reports the first
// failure it met after giving every source its chance.
func (m *Mod) LearnReleases(ctx context.Context) error {
	fetch := m.ReleaseFetch
	if fetch == nil {
		fetch = httpReleaseFetch
	}
	var firstErr error
	learned := 0
	for _, src := range releaseSources {
		fctx, cancel := context.WithTimeout(ctx, releaseFetchTimeout)
		pairs, err := learnSource(fctx, fetch, src)
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
// it — a slow or hung fetch must not hold the cycle up. A
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
