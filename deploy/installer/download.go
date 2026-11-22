// Downloading the latest release, the way `evesynapse -update`
// does it: the providers are tried in order, GitHub, then GitLab,
// then natems.dev (Forgejo), and a single provider failing just
// moves on to the next. A release is only ever used when its
// manifest's signature checks out against the release keys
// embedded in this installer and the binary matches the checksum
// that signed manifest names. (The updater walks a fourth
// provider, the static mirror, after the three forges; the
// installer does not, because Nate named the three forges.)
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	downloadMaxBytes     = 200 << 20 // a real build is ~25 MB
	downloadMinBytes     = 1 << 20
	downloadTimeout      = 5 * time.Minute
	providerListTimeout  = 30 * time.Second
	maxManifestBytes     = 64 << 10
	maxSigBytes          = 4 << 10
	elfMachineAArch64    = 183
	elfMachineAMD64      = 62
)

var (
	githubBaseURL    = "https://github.com"
	githubAPIBaseURL = "https://api.github.com"
	gitlabAPIBaseURL = "https://gitlab.com/api/v4"
	forgejoBaseURL   = "https://natems.dev"

	updateProject      = "natemsz/evesynapse"
	updateForgejoRepo  = "EveSynapse/evesynapse"
)

// releaseManifest is the per-arch pointer published with each
// release: the version it names and the checksum of the binary
// beside it.
type releaseManifest struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
}

// releaseInfo is a release the installer can install: which
// provider served it, its version, and where its binary is.
type releaseInfo struct {
	Provider  string // github, gitlab or forgejo
	Version   string // without a leading v
	Manifest  releaseManifest
	BinaryURL string
}

// providerAttempt is one provider's outcome, for the read-out:
// tried, and what it found or why it failed.
type providerAttempt struct {
	Provider string
	Version  string // "" when it served nothing usable
	Err      error  // nil when it served a release
}

// releaseProviders is the order the release is fetched in.
var releaseProviders = []string{"github", "gitlab", "forgejo"}

// providerDisplayName reads a provider out for the operator.
func providerDisplayName(p string) string {
	switch p {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "forgejo":
		return "natems.dev"
	}
	return p
}

// providerDownloadPages is where the operator downloads a
// release by hand when every provider fails.
var providerDownloadPages = []string{
	"https://github.com/natemsz/evesynapse/releases/latest",
	"https://gitlab.com/natemsz/evesynapse/-/releases",
	"https://natems.dev/EveSynapse/evesynapse/releases",
}

// compareVersions orders dotted numeric versions: -1 when a is
// the older, 0 when equal, +1 when a is the newer. A leading "v"
// is ignored and missing parts count as 0. A development build's
// suffix is not part of the number: the numbers are compared
// first, and when they tie the plain release is the newer.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	switch preA, preB := versionIsPrerelease(a), versionIsPrerelease(b); {
	case preA && !preB:
		return -1
	case !preA && preB:
		return 1
	}
	return 0
}

// versionIsPrerelease reports whether v names a development
// build: anything carrying a "-suffix" after its number.
func versionIsPrerelease(v string) bool {
	return strings.Contains(strings.TrimSpace(v), "-")
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	fields := strings.Split(v, ".")
	parts := make([]int, len(fields))
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			n = 0
		}
		parts[i] = n
	}
	return parts
}

// parsePublicKeys reads the Ed25519 public keys in a
// trusted-keys file: PEM "PUBLIC KEY" blocks. Text outside the
// blocks is commentary.
func parsePublicKeys(pemData []byte) ([][]byte, error) {
	var keys [][]byte
	for rest := pemData; ; {
		idx := bytes.Index(rest, []byte("-----BEGIN "))
		if idx < 0 {
			break
		}
		block, after := pem.Decode(rest[idx:])
		if block == nil {
			break
		}
		if block.Type == "PUBLIC KEY" {
			parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("a release key can't be read: %v", err)
			}
			key, ok := parsed.(ed25519.PublicKey)
			if !ok || len(key) != ed25519.PublicKeySize {
				return nil, errors.New("a release key is not an Ed25519 key")
			}
			keys = append(keys, []byte(key))
		}
		rest = after
	}
	if len(keys) == 0 {
		return nil, errors.New("no trusted release key to check the signature against")
	}
	return keys, nil
}

// verifyManifestSignature checks that sigFile holds a signature
// over manifest's exact bytes made by one of the trusted keys.
// The signature file holds one base64 signature per line.
func verifyManifestSignature(trusted [][]byte, manifest, sigFile []byte) error {
	signatures := 0
	for _, line := range bytes.Split(sigFile, []byte("\n")) {
		sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(line)))
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		signatures++
		for _, k := range trusted {
			if ed25519.Verify(ed25519.PublicKey(k), manifest, sig) {
				return nil
			}
		}
	}
	if signatures == 0 {
		return errors.New("the release's signature file holds no signature")
	}
	return errors.New("the release's signature was not made by a trusted release key over this release (it may have been changed after it was signed)")
}

// httpGet fetches url with a User-Agent, returning the body when
// the server answers 200, and an error naming the answer
// otherwise. The body is capped at limit+1 bytes.
func httpGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "EveSynapse-Installer")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("the answer was too large to be a release file")
	}
	return body, nil
}

// fetchProviderRelease asks one provider for the channel's
// release: its manifest, checked against the trusted keys, and
// the binary address. A provider that cannot be reached, or
// has no usable release, fails and the caller tries the next.
func fetchProviderRelease(ctx context.Context, provider, arch string) (*releaseInfo, error) {
	manifestURL, sigURL, binaryURL, err := providerAssets(ctx, provider, arch)
	if err != nil {
		return nil, err
	}
	raw, err := httpGet(ctx, manifestURL, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	bundled := sigURL == ""
	var bundleSigs []byte
	if bundled {
		// A combined latest.json carries the signed bytes and their
		// signatures, and this computer's entry is read from it.
		var file struct {
			Manifest   []byte   `json:"manifest"`
			Signatures []string `json:"signatures"`
		}
		if err := json.Unmarshal(raw, &file); err != nil || len(file.Manifest) == 0 {
			return nil, errors.New("the release information couldn't be read")
		}
		raw, bundleSigs = file.Manifest, []byte(strings.Join(file.Signatures, "\n"))
	}
	trusted, err := readTrustedKeys()
	if err != nil {
		return nil, err
	}
	sig := bundleSigs
	if !bundled {
		sig, err = httpGet(ctx, sigURL, maxSigBytes)
		if err != nil {
			return nil, fmt.Errorf("the release's signature could not be fetched: %v", err)
		}
	}
	if err := verifyManifestSignature(trusted, raw, sig); err != nil {
		// A release whose signature does not check out is not a
		// provider being down, it is a forgery being offered:
		// stop here rather than trying the next mirror.
		return nil, &refusalError{why: "the release " + providerDisplayName(provider) + " served is not signed with a trusted release key: " + err.Error()}
	}
	var m releaseManifest
	if bundled {
		var body struct {
			Version string            `json:"version"`
			SHA256  map[string]string `json:"sha256"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, errors.New("the release information couldn't be read")
		}
		m = releaseManifest{Version: body.Version, Arch: arch, SHA256: body.SHA256[arch]}
	} else if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&m); err != nil {
		return nil, errors.New("the release information couldn't be read")
	}
	m.SHA256 = strings.ToLower(strings.TrimSpace(m.SHA256))
	if _, err := hex.DecodeString(m.SHA256); err != nil || len(m.SHA256) != sha256.Size*2 {
		return nil, errors.New("the release information couldn't be read")
	}
	if strings.TrimSpace(m.Version) == "" {
		return nil, errors.New("the release information couldn't be read")
	}
	if m.Arch != arch {
		return nil, fmt.Errorf("the release is for %q computers, not %q", m.Arch, arch)
	}
	return &releaseInfo{
		Provider:  provider,
		Version:   strings.TrimPrefix(m.Version, "v"),
		Manifest:  m,
		BinaryURL: binaryURL,
	}, nil
}

// refusalError is a release that will not be installed because
// it cannot be shown to be genuine.
type refusalError struct{ why string }

func (e *refusalError) Error() string { return e.why }

// providerAssets resolves the channel's asset URLs against one
// named provider: the manifest, its signature, and the binary.
func providerAssets(ctx context.Context, provider, arch string) (manifestURL, sigURL, binaryURL string, err error) {
	wantManifest := "latest-" + arch + ".json"
	wantSig := wantManifest + ".sig"
	wantBinary := "evesynapse-" + arch
	switch provider {
	case "github":
		// The stable channel needs no listing: the
		// repository's "latest" download address always serves
		// the newest plain release's files.
		base := githubBaseURL + "/" + updateProject + "/releases/latest/download"
		if _, err := httpGet(ctx, base+"/latest.json", maxManifestBytes); err == nil {
			return base + "/latest.json", "", base + "/" + wantBinary, nil
		}
		return base + "/" + wantManifest, base + "/" + wantSig, base + "/" + wantBinary, nil
	case "gitlab":
		return gitlabReleaseAssets(ctx, arch)
	case "forgejo":
		return forgejoReleaseAssets(ctx, arch)
	}
	return "", "", "", fmt.Errorf("unknown release provider %q", provider)
}

// gitlabRelease is one release in the GitLab API's payload.
type gitlabRelease struct {
	TagName  string `json:"tag_name"`
	Upcoming bool   `json:"upcoming_release"`
	Assets   struct {
		Links []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"links"`
	} `json:"assets"`
}

// gitlabReleaseAssets resolves the channel against the GitLab
// project's release list: the greatest version whose tag carries
// no suffix, among the releases that are out.
func gitlabReleaseAssets(ctx context.Context, arch string) (manifestURL, sigURL, binaryURL string, err error) {
	project := url.PathEscape(updateProject)
	var all []gitlabRelease
	for page := 1; page <= 3; page++ {
		apiURL := fmt.Sprintf("%s/projects/%s/releases?per_page=100&page=%d", gitlabAPIBaseURL, project, page)
		raw, err := httpGet(ctx, apiURL, 4<<20)
		if err != nil {
			return "", "", "", err
		}
		var releases []gitlabRelease
		if err := json.Unmarshal(raw, &releases); err != nil {
			return "", "", "", errors.New("the release information couldn't be read")
		}
		all = append(all, releases...)
		if len(releases) < 100 {
			break
		}
	}
	var best *gitlabRelease
	for i := range all {
		rel := &all[i]
		if rel.Upcoming || rel.TagName == "" || versionIsPrerelease(rel.TagName) {
			continue
		}
		if best == nil || compareVersions(rel.TagName, best.TagName) > 0 {
			best = rel
		}
	}
	if best == nil {
		return "", "", "", errors.New("no releases found")
	}
	links := make([]namedURL, 0, len(best.Assets.Links))
	for _, a := range best.Assets.Links {
		links = append(links, namedURL{Name: a.Name, URL: a.URL})
	}
	return pickAssets(best.TagName, wantAssetNames(arch), links)
}

// forgejoRelease is one release in the Forgejo API's payload.
type forgejoRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// forgejoReleaseAssets resolves the channel against the
// project's Forgejo: the greatest stable tag that is not a draft.
func forgejoReleaseAssets(ctx context.Context, arch string) (manifestURL, sigURL, binaryURL string, err error) {
	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/releases?limit=100", forgejoBaseURL, updateForgejoRepo)
	raw, err := httpGet(ctx, apiURL, 4<<20)
	if err != nil {
		return "", "", "", err
	}
	var releases []forgejoRelease
	if err := json.Unmarshal(raw, &releases); err != nil {
		return "", "", "", errors.New("the release information couldn't be read")
	}
	var best *forgejoRelease
	for i := range releases {
		rel := &releases[i]
		if rel.Draft || rel.TagName == "" || versionIsPrerelease(rel.TagName) {
			continue
		}
		if best == nil || compareVersions(rel.TagName, best.TagName) > 0 {
			best = rel
		}
	}
	if best == nil {
		return "", "", "", errors.New("no releases found")
	}
	links := make([]namedURL, 0, len(best.Assets))
	for _, a := range best.Assets {
		links = append(links, namedURL{Name: a.Name, URL: a.URL})
	}
	return pickAssets(best.TagName, wantAssetNames(arch), links)
}

func wantAssetNames(arch string) [3]string {
	m := "latest-" + arch + ".json"
	return [3]string{m, m + ".sig", "evesynapse-" + arch}
}

// pickAssets matches the wanted asset names against a release's
// links, returning the manifest, signature and binary URLs.
func pickAssets(tag string, want [3]string, links []namedURL) (string, string, string, error) {
	var m, s, b, bundle string
	for _, a := range links {
		switch a.Name {
		case "latest.json":
			bundle = a.URL
		case want[0]:
			m = a.URL
		case want[1]:
			s = a.URL
		case want[2]:
			b = a.URL
		}
	}
	if bundle != "" && b != "" {
		return bundle, "", b, nil
	}
	if m == "" {
		return "", "", "", fmt.Errorf("release %s has no %s asset", tag, want[0])
	}
	if b == "" {
		return "", "", "", fmt.Errorf("release %s has no %s asset", tag, want[2])
	}
	if s == "" {
		return "", "", "", fmt.Errorf("release %s is not signed", tag)
	}
	return m, s, b, nil
}

// namedURL is one asset link: its name and download address.
type namedURL struct {
	Name string
	URL  string
}

// fetchLatestRelease tries the providers in order and returns
// the first release that checks out, with one attempt record per
// provider tried. A refusal (a release that fails its signature)
// stops the walk. When every provider fails, the error says so
// and names the manual download pages.
func fetchLatestRelease(ctx context.Context, arch string, report func(providerAttempt)) (*releaseInfo, error) {
	var firstErr error
	for _, p := range releaseProviders {
		rel, err := fetchProviderRelease(ctx, p, arch)
		if err != nil {
			if _, ok := err.(*refusalError); ok {
				if report != nil {
					report(providerAttempt{Provider: p, Err: err})
				}
				return nil, err
			}
			if report != nil {
				report(providerAttempt{Provider: p, Err: err})
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if report != nil {
			report(providerAttempt{Provider: p, Version: rel.Version})
		}
		return rel, nil
	}
	if firstErr == nil {
		firstErr = errors.New("no provider answered")
	}
	return nil, &allProvidersFailedError{first: firstErr}
}

// allProvidersFailedError is every provider failing: the install
// stops and the operator is told where to download by hand.
type allProvidersFailedError struct{ first error }

func (e *allProvidersFailedError) Error() string {
	return "could not reach the latest release from GitHub, GitLab or natems.dev (" + e.first.Error() + "). " +
		"Download the release by hand from one of these pages: " +
		strings.Join(providerDownloadPages, ", ") +
		", then run this installer again with the download, or run setup.sh from the release."
}

// downloadProgress reports a download: bytes moved and the total
// when it is known (0 while unknown).
type downloadProgress struct {
	Done  int64
	Total int64
}

// downloadBinary fetches the release's binary to dest,
// checking it is a whole program for this computer's kind and
// matches the checksum the signed manifest names. progress is
// called as the bytes move, when it is not nil.
func downloadBinary(ctx context.Context, rel *releaseInfo, dest string, progress func(downloadProgress)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.BinaryURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "EveSynapse-Installer")
	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered %s", resp.Status)
	}
	if resp.ContentLength > downloadMaxBytes {
		return fmt.Errorf("the download is larger than %d MB", downloadMaxBytes>>20)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	hash := sha256.New()
	var written int64
	buf := make([]byte, 128*1024)
	total := resp.ContentLength
	for {
		n, rerr := io.ReadFull(resp.Body, buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := out.Write(chunk); werr != nil {
				return werr
			}
			hash.Write(chunk)
			written += int64(n)
			if progress != nil {
				progress(downloadProgress{Done: written, Total: total})
			}
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			return rerr
		}
		if written > downloadMaxBytes {
			return fmt.Errorf("the download is larger than %d MB", downloadMaxBytes>>20)
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if written < downloadMinBytes {
		return errors.New("the download is too small to be the EveSynapse program (it may be an error page instead)")
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if got != rel.Manifest.SHA256 {
		return errors.New("the download does not match the checksum its signed release names; it may have been damaged on the way")
	}
	if err := checkELFMachine(dest, rel.Manifest.Arch); err != nil {
		return err
	}
	return nil
}

// checkELFMachine checks that path is an ELF program built for
// the release arch: the check that keeps a wrong download (an
// error page, a build for another kind of computer) from ever
// being installed.
func checkELFMachine(path, arch string) error {
	want, ok := map[string]uint16{"arm64": elfMachineAArch64, "amd64": elfMachineAMD64}[arch]
	if !ok {
		return fmt.Errorf("unknown release arch %q", arch)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [20]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return errors.New("the download isn't a complete program file")
	}
	if hdr[0] != 0x7f || hdr[1] != 'E' || hdr[2] != 'L' || hdr[3] != 'F' {
		return errors.New("the download isn't an EveSynapse program; it may be an error page instead of the release")
	}
	if machine := binary.LittleEndian.Uint16(hdr[18:20]); machine != want {
		return errors.New("the download was built for a different kind of computer, so it wouldn't run here")
	}
	return nil
}

// fetchRawFile fetches one file (a .env.example, a service unit)
// from a provider's raw file view, at the release tag. It is
// how the installer gets the files setup.sh would otherwise
// fetch from GitLab itself, from the provider that served the
// release.
func fetchRawFile(ctx context.Context, provider, tag, path string) ([]byte, error) {
	var rawURL string
	switch provider {
	case "github":
		rawURL = "https://raw.githubusercontent.com/" + updateProject + "/" + tag + "/" + path
	case "gitlab":
		rawURL = "https://gitlab.com/" + updateProject + "/-/raw/" + tag + "/" + path
	case "forgejo":
		rawURL = forgejoBaseURL + "/" + updateForgejoRepo + "/raw/tag/" + tag + "/" + path
	default:
		return nil, fmt.Errorf("unknown release provider %q", provider)
	}
	return httpGet(ctx, rawURL, 1<<20)
}
