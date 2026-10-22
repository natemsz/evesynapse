package swarm

// ---------------------------------------------------------------------------
// The release learner: the release library's second,
// self-keeping half (build.go has the first, seeded one).
//
// On a slow cadence the learner fetches the recent releases
// of the owner's two GitHub repositories and records every
// binary asset's SHA-256 digest — the releases API reports
// one per asset — in the release_builds table, under the
// release's tag and the repository's product kind. A recorded
// hash is remembered for good: it stays known after its
// release scrolls off the API's first page, so the library
// keeps itself current without anyone seeding new hashes by
// hand when a release ships.
//
// The repositories are hardcoded to the owner's because
// "mainline" is defined by his releases: a build is mainline
// when one of these two repositories published it. A fork
// self-hosting EveSynapse learns the same library — the
// upstream's builds are the mainline whatever instance asks.
//
// Failure tolerance is the shape of the thing: a fetch or
// parse failure changes nothing (the previously recorded
// builds stay recorded, the in-memory snapshot stays as it
// was) and the pass is simply due again later. The cadence
// is deliberately slow — once a day after a success, no
// sooner than six hours after a failure — which the
// unauthenticated GitHub API allowance absorbs without
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
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// releaseSource is one repository the learner reads: where
// it lives, the product name its binary assets carry, and
// the library kind its builds belong to.
type releaseSource struct {
	repo    string
	product string
	kind    ReleaseKind
}

// releaseSources are the learner's repositories: the app and
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

// releaseReleasesURL is the releases API page for one source.
func releaseReleasesURL(src releaseSource) string {
	return fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", src.repo, releasePerPage)
}

// httpReleaseFetch is the built-in ReleaseFetch: one GET
// with the API's headers, the body capped well past any real
// releases page.
func httpReleaseFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "evesynapse-release-learner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases API answered %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// githubRelease is one release in the API's payload, cut to
// the fields the learner reads. The digest is the asset's
// SHA-256 as the API reports it, "sha256:" and the hex.
type githubRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// learnedBuild is one (hash, entry) pair read off a release.
type learnedBuild struct {
	hash  string
	entry ReleaseBuild
}

// parseReleaseBuilds reads one repository's releases payload
// and returns the build of every binary asset on it. A
// binary asset is the product's own executable for one
// architecture — evesynapse-amd64, storm-arm64 — named
// exactly; bundles, checksum files, signatures and update
// metadata ride the same releases and are not builds, so
// they are skipped by name, never recorded. An asset whose
// digest is missing or not a SHA-256 is skipped the same
// way. Drafts are skipped: unpublished work is not a release.
func parseReleaseBuilds(src releaseSource, body []byte) ([]learnedBuild, error) {
	var rels []githubRelease
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("releases payload for %s: %w", src.repo, err)
	}
	var out []learnedBuild
	for _, rel := range rels {
		if rel.Draft || rel.TagName == "" {
			continue
		}
		for _, a := range rel.Assets {
			if a.Name != src.product+"-amd64" && a.Name != src.product+"-arm64" {
				continue
			}
			hash, ok := strings.CutPrefix(a.Digest, "sha256:")
			if !ok || len(hash) != 64 {
				continue
			}
			if _, err := hex.DecodeString(hash); err != nil {
				continue
			}
			out = append(out, learnedBuild{
				hash:  strings.ToLower(hash),
				entry: ReleaseBuild{Kind: src.kind, Label: rel.TagName},
			})
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

// LearnReleases runs one learn pass: fetch both repositories'
// recent releases, record every binary asset's digest, and
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
		body, err := fetch(fctx, releaseReleasesURL(src))
		cancel()
		var pairs []learnedBuild
		if err == nil {
			pairs, err = parseReleaseBuilds(src, body)
		}
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
