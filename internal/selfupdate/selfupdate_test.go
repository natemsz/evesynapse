package selfupdate

// Hermetic tests for -update: the download/verify/swap path (against
// httptest servers and local files only — never the network) and the
// restart hand-off (against a throwaway `sleep` process).
//
// None of these depends on which release the repository is at: the
// updater is told the running version, and the tests tell it
// testVersion. A release bump has nothing to edit here.
//
// Nor on the real release key, whose private half only the release
// job holds: a test that needs a signed release makes a key of its
// own and has the updater trust that one (trustTestKey).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"evesynapse/internal/pidfile"
	"evesynapse/internal/pidfile/pidfiletest"
	"evesynapse/internal/releasesig"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// testVersion is the version the updater under test believes it is
// running. It is deliberately nothing like a real release number.
const testVersion = "v1.4.2.003"

// fakeELF builds a minimal ELF-looking payload: real magic and
// class bytes, the given e_machine, zero padding out to size.
func fakeELF(machine uint16, size int) []byte {
	b := make([]byte, size)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	binary.LittleEndian.PutUint16(b[16:18], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(b[18:20], machine)
	return b
}

// ownAndOtherELFMachine returns the ELF machine kind of the
// computer running the tests — what the explicit-source update
// form expects of a build, since it installs one this computer can
// run — and a kind it is not.
func ownAndOtherELFMachine(t *testing.T) (own, other uint16) {
	t.Helper()
	own, ok := elfMachineForArch(ownReleaseArch())
	if !ok {
		t.Skipf("EveSynapse publishes no build for %s", ownReleaseArch())
	}
	other = elfMachineAArch64
	if own == elfMachineAArch64 {
		other = elfMachineAMD64
	}
	return own, other
}

func ownELFMachine(t *testing.T) uint16 {
	t.Helper()
	own, _ := ownAndOtherELFMachine(t)
	return own
}

func writeTestFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestUpdateProject(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROJECT", "")
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "")
	if got := updateProject(); got != defaultUpdateProject {
		t.Fatalf("default project = %q, want %q", got, defaultUpdateProject)
	}
	// An install from the GitHub years carries only the old
	// setting; its value is the same project's path.
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "someone/evesynapse")
	if got := updateProject(); got != "someone/evesynapse" {
		t.Fatalf("legacy project = %q, want the EVESYNAPSE_UPDATE_REPO value", got)
	}
	t.Setenv("EVESYNAPSE_UPDATE_PROJECT", "other/evesynapse")
	if got := updateProject(); got != "other/evesynapse" {
		t.Fatalf("project = %q, want the EVESYNAPSE_UPDATE_PROJECT value", got)
	}
}

func TestUpdateAPIBase(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_API_URL", "")
	if got := updateAPIBase(); got != releaseAPIBaseURL {
		t.Fatalf("default API base = %q, want %q", got, releaseAPIBaseURL)
	}
	t.Setenv("EVESYNAPSE_UPDATE_API_URL", "https://gitlab.example.com/api/v4/")
	if got := updateAPIBase(); got != "https://gitlab.example.com/api/v4" {
		t.Fatalf("API base = %q, want the override without its trailing slash", got)
	}
}

// TestSelectReleaseChannels pins the channel choice the move
// to GitLab rests on: GitLab's list is ordered by release
// date, not by version, and its newest entry may be a
// development build or one not out yet. The stable channel
// takes the greatest plain version; the dev channel takes
// the greatest -dev one.
func TestSelectReleaseChannels(t *testing.T) {
	mk := func(tag string, upcoming bool) gitlabRelease {
		var rel gitlabRelease
		rel.TagName = tag
		rel.Upcoming = upcoming
		return rel
	}
	releases := []gitlabRelease{
		mk("v0.7.0.9-dev", false), // newest by number, development
		mk("v0.7.0.8", true),      // plain, but not out yet
		mk("v0.7.0.6", false),
		mk("v0.7.0.7-dev", false),
		mk("v0.7.0.5", false),
	}
	stable, ok := selectRelease(releases, false)
	if !ok || stable.TagName != "v0.7.0.6" {
		t.Fatalf("stable channel picked %q (found %t), want v0.7.0.6", stable.TagName, ok)
	}
	dev, ok := selectRelease(releases, true)
	if !ok || dev.TagName != "v0.7.0.9-dev" {
		t.Fatalf("dev channel picked %q (found %t), want v0.7.0.9-dev", dev.TagName, ok)
	}
	if _, ok := selectRelease([]gitlabRelease{mk("v0.7.0.9-dev", false)}, false); ok {
		t.Fatal("stable channel found a release in a dev-only list")
	}
}

func TestLoadInstallEnv(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".env"),
		[]byte("EVESYNAPSE_UPDATE_REPO=someone/evesynapse\n"), 0o600)
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "")
	os.Unsetenv("EVESYNAPSE_UPDATE_REPO")
	if err := loadInstallEnv(filepath.Join(dir, "evesynapse")); err != nil {
		t.Fatalf("loadInstallEnv: %v", err)
	}
	if got := os.Getenv("EVESYNAPSE_UPDATE_REPO"); got != "someone/evesynapse" {
		t.Fatalf("EVESYNAPSE_UPDATE_REPO = %q after loadInstallEnv", got)
	}
}

// TestLoadInstallEnvUnreadable: an install .env that exists but
// cannot be read is an error — it may carry the update channel,
// and updating from the wrong channel is worse than not
// updating. A directory in place of the file reads as one on
// every platform.
func TestLoadInstallEnvUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := loadInstallEnv(filepath.Join(dir, "evesynapse")); err == nil {
		t.Fatal("loadInstallEnv over an unreadable .env succeeded, want an error")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// stable.major.feature.fix, each part a plain number.
		{"0.4.1.5", "0.4.1.4", 1},
		{"0.4.1.4", "0.4.2.0", -1},
		{"0.5.0.0", "0.4.9.9", 1},
		{"1.0.0.0", "0.99.99.99", 1},
		{"0.4.1.10", "0.4.1.9", 1}, // numeric, not lexical
		{"0.4.10.0", "0.4.9.9", 1},
		{"v0.4.1.4", "0.4.1.4", 0},
		{"0.4.1.4-dev", "0.4.1.4", -1},
		{"0.4.1.4", "0.4.1.3-dev", 1},
		// From the last zero-padded release to the unpadded ones: an
		// install still on the padded numbering has to see the new
		// releases as newer, and the padding itself changes nothing.
		{"0.4.1.4", "0.4.01.003", 1},
		{"0.4.2.0", "0.4.01.003", 1},
		{"1.0.0.0", "0.4.01.003", 1},
		{"0.4.01.003", "0.4.1.4", -1},
		{"0.4.1.4-dev", "0.4.01.003", 1},
		{"0.4.01.003", "0.4.1.3", 0},
		// The padded numbering, as before.
		{"0.3.17.1", "0.3.16.9", 1},
		{"0.3.16.9", "0.3.17.1", -1},
		{"0.3.17.001", "0.3.17.1", 0},
		{"v0.3.17.1", "0.3.17.1", 0},
		{"0.3.17", "0.3.17.0", 0},
		{"0.3.17.2", "0.3.17.10", -1}, // numeric, not lexical
		{"0.3.17.10", "0.3.17.2", 1},
		// A -dev suffix is not part of the number: the numbers
		// decide first, and on a tie the plain release is newer.
		// (Made-up numbers on purpose: these cases used to carry
		// the current release, and a release bump's search and
		// replace changed what they compared.)
		{"1.4.1.002-dev", "1.4.2.001-dev", -1},
		{"1.4.2.001-dev", "1.4.1.002-dev", 1},
		{"1.4.2.001-dev", "1.4.2.001-dev", 0},
		{"1.4.2.001-dev", "1.4.2.001", -1},
		{"1.4.2.001", "v1.4.2.001-dev", 1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// lastPaddedRelease is the last release numbered with zero-padded
// parts. Installs on it, and on anything before it,
// decide for themselves whether a release is newer.
const lastPaddedRelease = "0.4.01.003"

// TestVersionFileIsNotOlderThanTheLastPaddedRelease: whatever
// version.txt is changed to, the installs still on the old numbering
// must see it as an update. A number below 0.4.1.3, part by part,
// reads as older to them, and they would never move.
func TestVersionFileIsNotOlderThanTheLastPaddedRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "app", "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(raw))
	if compareVersions(version, lastPaddedRelease) < 0 {
		t.Fatalf("version.txt is %s, which an install on %s takes for an older version and will not update to; the number has to be above 0.4.1.3 part by part (0.4.1.4, 0.4.2.0, 0.5.0.0, ...)", version, lastPaddedRelease)
	}
}

func TestParseArchArg(t *testing.T) {
	cases := map[string]string{
		"-arm64":        "arm64",
		"arm64":         "arm64",
		"-amd64":        "amd64",
		"-x86":          "amd64",
		"-x64":          "amd64",
		"https://x/y":   "",
		"-refresh":      "",
		"":              "",
		"evesynapse.db": "",
	}
	for arg, want := range cases {
		if got := parseArchArg(arg); got != want {
			t.Errorf("parseArchArg(%q) = %q, want %q", arg, got, want)
		}
	}
}

// channelRelease describes one release on the test channel:
// its tag, and the paths on the test server its files are
// served from. A field left empty is a file the release does
// not carry among its assets.
type channelRelease struct {
	tag      string
	arch     string // defaults to arm64
	manifest string
	sig      string
	binary   string
}

// serveChannel registers the GitLab releases listing for the
// given releases on mux, in the API's own shape. The asset
// addresses are composed from the request's host, so the
// listing can be registered before the server's address is
// known.
func serveChannel(mux *http.ServeMux, rels ...channelRelease) {
	mux.HandleFunc("/projects/", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		var sb strings.Builder
		sb.WriteString("[")
		for i, rel := range rels {
			if i > 0 {
				sb.WriteString(",")
			}
			arch := rel.arch
			if arch == "" {
				arch = "arm64"
			}
			var links []string
			link := func(name, path string) {
				links = append(links, fmt.Sprintf(`{"name":%q,"url":%q}`, name, base+path))
			}
			if rel.manifest != "" {
				link("latest-"+arch+".json", rel.manifest)
			}
			if rel.sig != "" {
				link("latest-"+arch+".json"+releasesig.SigSuffix, rel.sig)
			}
			if rel.binary != "" {
				link("evesynapse-"+arch, rel.binary)
			}
			fmt.Fprintf(&sb, `{"tag_name":%q,"upcoming_release":false,"assets":{"links":[%s]}}`,
				rel.tag, strings.Join(links, ","))
		}
		sb.WriteString("]")
		_, _ = io.WriteString(w, sb.String())
	})
}

// trustTestKey has the updater under test trust a key made for this
// test, in place of the release keys compiled into the build, and
// returns the half that signs.
func trustTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	withTrustedKeys(t, func() ([]ed25519.PublicKey, error) { return []ed25519.PublicKey{pub}, nil })
	return priv
}

// withTrustedKeys stands in for the build's release keys for the
// duration of one test.
func withTrustedKeys(t *testing.T, keys func() ([]ed25519.PublicKey, error)) {
	t.Helper()
	old := trustedReleaseKeys
	trustedReleaseKeys = keys
	t.Cleanup(func() { trustedReleaseKeys = old })
}

// manifestJSON is a release manifest exactly as the release job
// writes one.
func manifestJSON(version, arch, sum string) string {
	return fmt.Sprintf(`{"version":%q,"arch":%q,"sha256":%q}`+"\n", version, arch, sum)
}

// signedBy is the signature file the release job publishes beside a
// manifest: what `releasesign sign` writes for it.
func signedBy(key ed25519.PrivateKey, manifest string) []byte {
	return releasesig.Sign([]ed25519.PrivateKey{key}, []byte(manifest))
}

// serve answers path with body.
func serve(mux *http.ServeMux, path string, body []byte) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) })
}

// publish serves a manifest at path and its signature by key beside
// it, the way a release carries the two.
func publish(mux *http.ServeMux, path, manifest string, key ed25519.PrivateKey) {
	serve(mux, path, []byte(manifest))
	serve(mux, path+releasesig.SigSuffix, signedBy(key, manifest))
}

func TestReleaseUpdateUpToDate(t *testing.T) {
	key := trustTestKey(t)
	bare := strings.TrimPrefix(testVersion, "v")
	mux := http.NewServeMux()
	publish(mux, "/latest-arm64.json", manifestJSON(bare, "arm64", strings.Repeat("0", 64)), key)
	serveChannel(mux, channelRelease{tag: testVersion, manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseAPI(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"-arm64"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("output %q missing the up-to-date note", out.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old build" {
		t.Fatal("target changed during an up-to-date check")
	}
}

func TestReleaseUpdateInstallsNewer(t *testing.T) {
	key := trustTestKey(t)
	fresh := fakeELF(elfMachineAArch64, updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	publish(mux, "/latest-arm64.json", manifestJSON("9.9.9.9", "arm64", sum), key)
	serve(mux, "/evesynapse-arm64", fresh)
	serveChannel(mux, channelRelease{tag: "v9.9.9.9", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseAPI(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"-arm64"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "A new version is available: v9.9.9.9") {
		t.Fatalf("output %q missing the new-version note", out.String())
	}
	if !strings.Contains(out.String(), "Checked GitLab: it has v9.9.9.9.") {
		t.Fatalf("output %q missing the provider read-out", out.String())
	}
	if !strings.Contains(out.String(), "Downloading it from GitLab.") {
		t.Fatalf("output %q missing the download source", out.String())
	}
	if !strings.Contains(out.String(), "now on v9.9.9.9") {
		t.Fatalf("output %q missing the new-version readout", out.String())
	}
	// Both checks are reported, the signature before anything is
	// downloaded and the checksum after.
	signature := strings.Index(out.String(), "Release signature verified.")
	download := strings.Index(out.String(), "Downloading the update")
	checksum := strings.Index(out.String(), "Download verified against its checksum.")
	if signature < 0 || download < signature || checksum < download {
		t.Fatalf("output %q: want the signature reported, then the download, then the checksum", out.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fresh) {
		t.Fatal("target not replaced by the release download")
	}
}

func TestReleaseUpdateX86Alias(t *testing.T) {
	key := trustTestKey(t)
	fresh := fakeELF(elfMachineAMD64, updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	publish(mux, "/latest-amd64.json", manifestJSON("9.9.9.9", "amd64", sum), key)
	serve(mux, "/evesynapse-amd64", fresh)
	serveChannel(mux, channelRelease{tag: "v9.9.9.9", arch: "amd64", manifest: "/latest-amd64.json", sig: "/latest-amd64.json" + releasesig.SigSuffix, binary: "/evesynapse-amd64"})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseAPI(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"-x86"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fresh) {
		t.Fatal("target not replaced by the x86 release download")
	}
}

// TestReleaseUpdateLeavesDevBuildsAlone: a development build that
// ends up published under a plain release tag — its manifest
// naming the development version — is never installed by a plain
// -update, whatever its number: nothing is even downloaded. The
// channel's own selection already keeps -dev tags out; this is
// the guard behind it, on the signed manifest itself.
func TestReleaseUpdateLeavesDevBuildsAlone(t *testing.T) {
	key := trustTestKey(t)
	fresh := fakeELF(elfMachineAArch64, updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int64
	mux := http.NewServeMux()
	publish(mux, "/latest-arm64.json", manifestJSON("9.9.9.9-dev", "arm64", sum), key)
	mux.HandleFunc("/evesynapse-arm64", func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(fresh)
	})
	serveChannel(mux, channelRelease{tag: "v9.9.9.9", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseAPI(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"-arm64"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "development build") {
		t.Fatalf("output %q missing the development-build note", out.String())
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("the development build was downloaded %d time(s), want 0", got)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old build" {
		t.Fatal("a plain -update installed a development build")
	}
}

// withReleaseAPI points the updater's release listing at a
// test server for the duration of one test.
func withReleaseAPI(t *testing.T, url string) {
	t.Helper()
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "gitlab")
	old := releaseAPIBaseURL
	releaseAPIBaseURL = url
	t.Cleanup(func() { releaseAPIBaseURL = old })
}

// devChannel registers the two-release channel the -dev tests
// run against: a plain release and the development release the
// -dev channel is after, their files served under "" and "/dev"
// respectively. The development release's signature is left out
// of its assets when devSigned is false.
func devChannel(mux *http.ServeMux, devSigned bool) {
	devSig := ""
	if devSigned {
		devSig = "/dev/latest-arm64.json" + releasesig.SigSuffix
	}
	serveChannel(mux,
		channelRelease{tag: "v9.9.9.8", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"},
		channelRelease{tag: "v9.9.9.9-dev", manifest: "/dev/latest-arm64.json", sig: devSig, binary: "/dev/evesynapse-arm64"},
	)
}

// TestDevUpdateInstallsTheDevReleasesOwnBinary: -update -dev takes
// the manifest, its signature and the binary from the same -dev
// release. The plain release's binary is a different build here,
// so downloading from there — what -dev first did — could only
// ever fail the dev manifest's checksum.
func TestDevUpdateInstallsTheDevReleasesOwnBinary(t *testing.T) {
	key := trustTestKey(t)
	devBuild := fakeELF(elfMachineAArch64, updateMinBytes+100)
	releaseBuild := fakeELF(elfMachineAArch64, updateMinBytes+200)
	sum, err := sha256FileHexBytes(devBuild)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	devChannel(mux, true)
	publish(mux, "/dev/latest-arm64.json", manifestJSON("9.9.9.9-dev", "arm64", sum), key)
	serve(mux, "/dev/evesynapse-arm64", devBuild)
	// The plain release's own files: another build entirely.
	publish(mux, "/latest-arm64.json", manifestJSON("9.9.9.8", "arm64", strings.Repeat("0", 64)), key)
	serve(mux, "/evesynapse-arm64", releaseBuild)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseAPI(t, srv.URL)
	t.Setenv("EVESYNAPSE_UPDATE_PROJECT", "")
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "")

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"-dev", "-arm64"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "A new version is available: v9.9.9.9-dev") {
		t.Fatalf("output %q missing the new-version note", out.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, devBuild) {
		t.Fatal("target not replaced by the dev release's own binary")
	}
}

func TestReleaseUpdateServerDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	withReleaseAPI(t, srv.URL)

	target := filepath.Join(t.TempDir(), "evesynapse")
	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"-arm64"}, &out, &errOut); code != 1 {
		t.Fatalf("code %d, want 1; stdout %q", code, out.String())
	}
	if !strings.Contains(errOut.String(), "Couldn't check for updates") {
		t.Fatalf("stderr %q missing the check failure", errOut.String())
	}
}

// newerRelease is the makings of a release that would install: a
// build newer than testVersion for ARM, and the manifest that names
// it. The tests below publish it with something wrong and check the
// updater leaves the installed program alone.
type newerRelease struct {
	build    []byte
	sum      string
	manifest string
}

func makeNewerRelease(t *testing.T, size int) newerRelease {
	t.Helper()
	build := fakeELF(elfMachineAArch64, updateMinBytes+size)
	sum, err := sha256FileHexBytes(build)
	if err != nil {
		t.Fatal(err)
	}
	return newerRelease{build: build, sum: sum, manifest: manifestJSON("9.9.9.9", "arm64", sum)}
}

// countedBuild serves a build at path and counts how often it is
// fetched, so a test can tell that a refused release was never even
// downloaded.
func countedBuild(mux *http.ServeMux, path string, build []byte) *atomic.Int64 {
	var downloads atomic.Int64
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(build)
	})
	return &downloads
}

// attemptUpdate runs `-update` with args against a release channel
// served by mux, over an installed program whose contents are "old
// build", and fails the test if the attempt changed that program or
// left a download behind. It is for attempts that must not install.
func attemptUpdate(t *testing.T, mux *http.ServeMux, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseAPI(t, srv.URL)
	t.Setenv("EVESYNAPSE_UPDATE_PROJECT", "")
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "")

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	code = runUpdate(testVersion, target, args, &out, &errOut)

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old build" {
		t.Fatalf("the installed program was replaced; stdout %q, stderr %q", out.String(), errOut.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".evesynapse.update-") {
			t.Fatalf("a download was left behind: %s", e.Name())
		}
	}
	return code, out.String(), errOut.String()
}

// TestReleaseUpdateRefusesWhatItCannotVerify: every case is a release
// that would install — a newer version, a build matching the
// checksum in its manifest — except that the manifest cannot be shown
// to come from the release key. None is installed, and none is even
// downloaded: the refusal comes before the build is fetched.
func TestReleaseUpdateRefusesWhatItCannotVerify(t *testing.T) {
	genuine := makeNewerRelease(t, 100)
	// What someone who can replace a release's files would put there:
	// a build of their own, and a manifest to match it.
	forged := makeNewerRelease(t, 300)
	page := []byte("<!DOCTYPE html><html><body>Not Found</body></html>")

	cases := []struct {
		name string
		// publish puts a release on the channel. key is the half of the
		// trusted key that signs; stranger is a key nobody trusts.
		publish func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) (served newerRelease)
		// listed is whether the release carries its signature
		// among its files; an unlisted signature does not count,
		// however the server would answer for it.
		listed bool
		want   string
	}{
		{
			name: "not signed",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				serve(mux, "/latest-arm64.json", []byte(forged.manifest))
				return forged
			},
			want: "isn't signed",
		},
		{
			name: "signed with a key this build does not trust",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				publish(mux, "/latest-arm64.json", forged.manifest, stranger)
				return forged
			},
			listed: true,
			want:   "doesn't check out",
		},
		{
			// The genuine release's signature, kept, with the manifest
			// beside it swapped for one naming another build.
			name: "the build swapped under a genuine signature",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				serve(mux, "/latest-arm64.json", []byte(forged.manifest))
				serve(mux, "/latest-arm64.json"+releasesig.SigSuffix, signedBy(key, genuine.manifest))
				return forged
			},
			listed: true,
			want:   "doesn't check out",
		},
		{
			name: "the version raised after signing",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				older := manifestJSON("1.0.0.0", "arm64", genuine.sum)
				serve(mux, "/latest-arm64.json", []byte(strings.Replace(older, "1.0.0.0", "9.9.9.9", 1)))
				serve(mux, "/latest-arm64.json"+releasesig.SigSuffix, signedBy(key, older))
				return genuine
			},
			listed: true,
			want:   "doesn't check out",
		},
		{
			name: "one byte added to the manifest after signing",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				serve(mux, "/latest-arm64.json", []byte(genuine.manifest+" "))
				serve(mux, "/latest-arm64.json"+releasesig.SigSuffix, signedBy(key, genuine.manifest))
				return genuine
			},
			listed: true,
			want:   "doesn't check out",
		},
		{
			name: "an error page where the signature should be",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				serve(mux, "/latest-arm64.json", []byte(genuine.manifest))
				serve(mux, "/latest-arm64.json"+releasesig.SigSuffix, page)
				return genuine
			},
			listed: true,
			want:   "doesn't check out",
		},
		{
			name: "an empty signature file",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				serve(mux, "/latest-arm64.json", []byte(genuine.manifest))
				serve(mux, "/latest-arm64.json"+releasesig.SigSuffix, nil)
				return genuine
			},
			listed: true,
			want:   "doesn't check out",
		},
		{
			// An unsigned manifest is not believed about anything,
			// including that there is nothing newer to install.
			name: "not signed, and naming the version already installed",
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) newerRelease {
				serve(mux, "/latest-arm64.json", []byte(manifestJSON(strings.TrimPrefix(testVersion, "v"), "arm64", genuine.sum)))
				return genuine
			},
			want: "isn't signed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := trustTestKey(t)
			_, stranger, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			served := tc.publish(mux, key, stranger)
			sigPath := ""
			if tc.listed {
				sigPath = "/latest-arm64.json" + releasesig.SigSuffix
			}
			serveChannel(mux, channelRelease{tag: "v9.9.9.9", manifest: "/latest-arm64.json", sig: sigPath, binary: "/evesynapse-arm64"})
			downloads := countedBuild(mux, "/evesynapse-arm64", served.build)

			code, stdout, stderr := attemptUpdate(t, mux, "-arm64")
			if code != 1 {
				t.Fatalf("code %d, want 1; stdout %q", code, stdout)
			}
			if !strings.Contains(stderr, "Update refused") || !strings.Contains(stderr, tc.want) || !strings.Contains(stderr, "Nothing was changed") {
				t.Fatalf("stderr %q, want a refusal saying the release %s, and that nothing was changed", stderr, tc.want)
			}
			if strings.Contains(stdout, "up to date") || strings.Contains(stdout, "A new version is available") {
				t.Fatalf("stdout %q acts on what an unverified manifest says", stdout)
			}
			if got := downloads.Load(); got != 0 {
				t.Fatalf("the build was downloaded %d time(s) although the release was refused, want 0", got)
			}
		})
	}
}

// TestReleaseUpdateRefusesWithoutAReleaseKey: a build with no usable
// release key has nothing to check a release against, so it installs
// none, however well signed, and says how a build can still be
// installed by hand.
func TestReleaseUpdateRefusesWithoutAReleaseKey(t *testing.T) {
	release := makeNewerRelease(t, 100)
	// Each case is what the build finds when it looks for its release
	// keys; signer is the key the release on offer was signed with.
	for name, keys := range map[string]func(signer ed25519.PublicKey) ([]ed25519.PublicKey, error){
		"no release key in the build": func(ed25519.PublicKey) ([]ed25519.PublicKey, error) {
			return nil, nil
		},
		"release keys that cannot be read": func(ed25519.PublicKey) ([]ed25519.PublicKey, error) {
			return nil, errors.New("damaged")
		},
		// A key file that is partly readable is not trusted in part.
		"the right key, returned along with a failure": func(signer ed25519.PublicKey) ([]ed25519.PublicKey, error) {
			return []ed25519.PublicKey{signer}, errors.New("damaged")
		},
	} {
		t.Run(name, func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			withTrustedKeys(t, func() ([]ed25519.PublicKey, error) { return keys(pub) })
			mux := http.NewServeMux()
			publish(mux, "/latest-arm64.json", release.manifest, priv)
			serveChannel(mux, channelRelease{tag: "v9.9.9.9", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
			downloads := countedBuild(mux, "/evesynapse-arm64", release.build)

			code, stdout, stderr := attemptUpdate(t, mux, "-arm64")
			if code != 1 {
				t.Fatalf("code %d, want 1; stdout %q", code, stdout)
			}
			if !strings.Contains(stderr, "Update refused") || !strings.Contains(stderr, "without a usable release key") {
				t.Fatalf("stderr %q, want a refusal for want of a release key", stderr)
			}
			if !strings.Contains(stderr, "-update <download address> <sha256>") {
				t.Fatalf("stderr %q does not say how to install a build by hand", stderr)
			}
			if got := downloads.Load(); got != 0 {
				t.Fatalf("the build was downloaded %d time(s), want 0", got)
			}
		})
	}
}

// TestReleaseUpdateNeverGoesBackwards: an older release is as
// genuinely signed as the newest. The signature alone would let
// whoever controls the channel hand an install a build with known
// flaws; the version, which the signature covers, is what stops it.
func TestReleaseUpdateNeverGoesBackwards(t *testing.T) {
	key := trustTestKey(t)
	older := makeNewerRelease(t, 100)
	older.manifest = manifestJSON("1.4.2.002", "arm64", older.sum) // just behind testVersion
	mux := http.NewServeMux()
	publish(mux, "/latest-arm64.json", older.manifest, key)
	serveChannel(mux, channelRelease{tag: "v1.4.2.002", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
	downloads := countedBuild(mux, "/evesynapse-arm64", older.build)

	code, stdout, stderr := attemptUpdate(t, mux, "-arm64")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "up to date") {
		t.Fatalf("stdout %q missing the up-to-date note", stdout)
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("an older release was downloaded %d time(s), want 0", got)
	}
}

// TestReleaseUpdateRefusesAnotherKindsManifest: the ARM release is
// genuine and signed, and it is served where the Intel/AMD one
// belongs. The signature covers the kind of computer, so it is not
// taken for the release that was asked for.
func TestReleaseUpdateRefusesAnotherKindsManifest(t *testing.T) {
	key := trustTestKey(t)
	arm := makeNewerRelease(t, 100)
	mux := http.NewServeMux()
	publish(mux, "/latest-amd64.json", arm.manifest, key)
	serveChannel(mux, channelRelease{tag: "v9.9.9.9", arch: "amd64", manifest: "/latest-amd64.json", sig: "/latest-amd64.json" + releasesig.SigSuffix, binary: "/evesynapse-amd64"})
	downloads := countedBuild(mux, "/evesynapse-amd64", arm.build)

	code, stdout, stderr := attemptUpdate(t, mux, "-x86")
	if code != 1 {
		t.Fatalf("code %d, want 1; stdout %q", code, stdout)
	}
	if !strings.Contains(stderr, `for "arm64" computers, not "amd64"`) {
		t.Fatalf("stderr %q does not say the manifest is for another kind of computer", stderr)
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("the build was downloaded %d time(s), want 0", got)
	}
}

// TestReleaseUpdateDuringAKeyReplacement: while a key is being
// replaced a release carries two signatures, and an updater installs
// it as long as it trusts either key; once the old key is retired, an
// updater that lists both still installs what the new one signs.
func TestReleaseUpdateDuringAKeyReplacement(t *testing.T) {
	oldPub, oldPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPub, newPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	release := makeNewerRelease(t, 100)
	bothSign := releasesig.Sign([]ed25519.PrivateKey{oldPriv, newPriv}, []byte(release.manifest))
	newSigns := releasesig.Sign([]ed25519.PrivateKey{newPriv}, []byte(release.manifest))

	cases := []struct {
		name    string
		trusted []ed25519.PublicKey
		sig     []byte
		install bool
	}{
		{"an updater from before the replacement, a release signed with both", []ed25519.PublicKey{oldPub}, bothSign, true},
		{"an updater that knows both keys, a release signed with both", []ed25519.PublicKey{oldPub, newPub}, bothSign, true},
		{"an updater that knows both keys, a release signed with the new one", []ed25519.PublicKey{oldPub, newPub}, newSigns, true},
		{"an updater that knows only the new key, a release signed with it", []ed25519.PublicKey{newPub}, newSigns, true},
		{"an updater from before the replacement, a release signed with the new key only", []ed25519.PublicKey{oldPub}, newSigns, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTrustedKeys(t, func() ([]ed25519.PublicKey, error) { return tc.trusted, nil })
			mux := http.NewServeMux()
			serve(mux, "/latest-arm64.json", []byte(release.manifest))
			serve(mux, "/latest-arm64.json"+releasesig.SigSuffix, tc.sig)
			serveChannel(mux, channelRelease{tag: "v9.9.9.9", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
			downloads := countedBuild(mux, "/evesynapse-arm64", release.build)

			if !tc.install {
				code, _, stderr := attemptUpdate(t, mux, "-arm64")
				if code != 1 || !strings.Contains(stderr, "Update refused") || downloads.Load() != 0 {
					t.Fatalf("code %d, %d download(s), stderr %q; want a refusal and no download", code, downloads.Load(), stderr)
				}
				return
			}
			srv := httptest.NewServer(mux)
			defer srv.Close()
			withReleaseAPI(t, srv.URL)
			target := filepath.Join(t.TempDir(), "evesynapse")
			writeTestFile(t, target, []byte("old build"), 0o755)
			var out, errOut bytes.Buffer
			if code := runUpdate(testVersion, target, []string{"-arm64"}, &out, &errOut); code != 0 {
				t.Fatalf("code %d, stderr %q", code, errOut.String())
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, release.build) {
				t.Fatal("the release was not installed")
			}
		})
	}
}

// TestReleaseUpdateSignatureThatCannotBeFetched: a signature the
// server fails to hand over is a check that could not be made, not a
// release that is unsigned. Either way nothing is installed, but the
// operator is told to expect better from trying again.
func TestReleaseUpdateSignatureThatCannotBeFetched(t *testing.T) {
	release := makeNewerRelease(t, 100)
	for name, handler := range map[string]http.HandlerFunc{
		"the server fails": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "try later", http.StatusServiceUnavailable)
		},
		"something far too large to be a signature": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("A"), releasesig.MaxSigBytes+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			trustTestKey(t)
			mux := http.NewServeMux()
			serve(mux, "/latest-arm64.json", []byte(release.manifest))
			mux.HandleFunc("/latest-arm64.json"+releasesig.SigSuffix, handler)
			serveChannel(mux, channelRelease{tag: "v9.9.9.9", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
			downloads := countedBuild(mux, "/evesynapse-arm64", release.build)

			code, stdout, stderr := attemptUpdate(t, mux, "-arm64")
			if code != 1 {
				t.Fatalf("code %d, want 1; stdout %q", code, stdout)
			}
			if !strings.Contains(stderr, "Couldn't check for updates") || strings.Contains(stderr, "Update refused") {
				t.Fatalf("stderr %q, want a failed check rather than a refusal", stderr)
			}
			if got := downloads.Load(); got != 0 {
				t.Fatalf("the build was downloaded %d time(s), want 0", got)
			}
		})
	}
}

// TestDevUpdateRefusesWhatItCannotVerify: the development channel is
// held to the same rule as the release channel.
func TestDevUpdateRefusesWhatItCannotVerify(t *testing.T) {
	dev := makeNewerRelease(t, 100)
	dev.manifest = manifestJSON("9.9.9.9-dev", "arm64", dev.sum)

	cases := []struct {
		name    string
		listed  bool // whether the release lists a signature among its files
		publish func(mux *http.ServeMux, key, stranger ed25519.PrivateKey)
		want    string
	}{
		{
			name:   "a development release with no signature among its files",
			listed: false,
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) {
				// The signature exists on the server, but the release
				// does not list it: only a release's own files count.
				publish(mux, "/dev/latest-arm64.json", dev.manifest, key)
			},
			want: "v9.9.9.9-dev) isn't signed",
		},
		{
			name:   "a development release signed with a key this build does not trust",
			listed: true,
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) {
				publish(mux, "/dev/latest-arm64.json", dev.manifest, stranger)
			},
			want: "doesn't check out",
		},
		{
			// The plain release's signature is genuine, and it is not
			// the development manifest's.
			name:   "a development release carrying the plain release's signature",
			listed: true,
			publish: func(mux *http.ServeMux, key, stranger ed25519.PrivateKey) {
				serve(mux, "/dev/latest-arm64.json", []byte(dev.manifest))
				serve(mux, "/dev/latest-arm64.json"+releasesig.SigSuffix, signedBy(key, manifestJSON("9.9.9.8", "arm64", dev.sum)))
			},
			want: "doesn't check out",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := trustTestKey(t)
			_, stranger, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			devChannel(mux, tc.listed)
			tc.publish(mux, key, stranger)
			downloads := countedBuild(mux, "/dev/evesynapse-arm64", dev.build)
			// The plain release beside it is in perfect order.
			publish(mux, "/latest-arm64.json", manifestJSON("9.9.9.8", "arm64", dev.sum), key)

			code, stdout, stderr := attemptUpdate(t, mux, "-dev", "-arm64")
			if code != 1 {
				t.Fatalf("code %d, want 1; stdout %q", code, stdout)
			}
			if !strings.Contains(stderr, "Update refused") || !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr %q, want a refusal containing %q", stderr, tc.want)
			}
			if got := downloads.Load(); got != 0 {
				t.Fatalf("the development build was downloaded %d time(s), want 0", got)
			}
		})
	}
}

func TestVerifyARMExecutable(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"arm", fakeELF(elfMachineAArch64, 64), true},
		{"x86-64", fakeELF(62, 64), false},
		{"arm32", fakeELF(40, 64), false},
		{"html error page", []byte("<!DOCTYPE html><html>404</html>"), false},
		{"truncated", fakeELF(elfMachineAArch64, 64)[:10], false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "candidate")
			writeTestFile(t, path, tc.data, 0o644)
			err := verifyARMExecutable(path)
			if tc.ok && err != nil {
				t.Fatalf("verifyARMExecutable: %v, want success", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("verifyARMExecutable succeeded, want refusal")
			}
		})
	}
}

func TestRunUpdateUsageErrors(t *testing.T) {
	target := filepath.Join(t.TempDir(), "evesynapse")
	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"a", "b", "c"}, &out, &errOut); code != 2 {
		t.Fatalf("too many args: code %d, want 2", code)
	}
	if code := runUpdate(testVersion, target, []string{"x", "not-a-checksum"}, &out, &errOut); code != 2 {
		t.Fatalf("malformed checksum: code %d, want 2", code)
	}

	// A download address with no checksum is refused before
	// anything is fetched: this runs as root and replaces the
	// program root runs, so a network download is only installed
	// against a checksum the operator supplies.
	var fetched atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Store(true)
	}))
	defer srv.Close()
	errOut.Reset()
	if code := runUpdate(testVersion, target, []string{srv.URL + "/evesynapse"}, &out, &errOut); code != 2 {
		t.Fatalf("download address without a checksum: code %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "needs the checksum") {
		t.Fatalf("stderr %q does not ask for the checksum", errOut.String())
	}
	if fetched.Load() {
		t.Fatal("the address was fetched although no checksum was given")
	}
}

func TestRunUpdateFromLocalFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	for _, source := range []string{
		filepath.Join(dir, "download"),
		"file://" + filepath.Join(dir, "download"),
	} {
		fresh := fakeELF(ownELFMachine(t), updateMinBytes+100)
		writeTestFile(t, filepath.Join(dir, "download"), fresh, 0o644)
		writeTestFile(t, target, []byte("old build"), 0o755)

		var out, errOut bytes.Buffer
		if code := runUpdate(testVersion, target, []string{source}, &out, &errOut); code != 0 {
			t.Fatalf("source %q: code %d, stderr %q", source, code, errOut.String())
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, fresh) {
			t.Fatalf("source %q: target not replaced", source)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("source %q: permissions %v, want 0755", source, info.Mode().Perm())
		}
		if !strings.Contains(out.String(), "isn't running right now") {
			t.Fatalf("source %q: output %q missing the not-running note", source, out.String())
		}
	}
}

func TestRunUpdateRejectsBadDownloads(t *testing.T) {
	own, other := ownAndOtherELFMachine(t)
	valid := fakeELF(own, updateMinBytes+100)
	wrongArch := fakeELF(other, updateMinBytes+100)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		args    func(url string) []string
	}{
		{
			name: "http 404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
			args: func(url string) []string { return []string{url, strings.Repeat("0", 64)} },
		},
		{
			name: "wrong architecture",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(wrongArch)
			},
			args: func(url string) []string { return []string{url, strings.Repeat("0", 64)} },
		},
		{
			name: "truncated (too small)",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("tiny"))
			},
			args: func(url string) []string { return []string{url, strings.Repeat("0", 64)} },
		},
		{
			name: "checksum mismatch",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(valid)
			},
			args: func(url string) []string {
				return []string{url, strings.Repeat("0", 64)}
			},
		},
		{
			name: "declared oversize",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(updateMaxBytes+1))
				w.WriteHeader(http.StatusOK)
			},
			args: func(url string) []string { return []string{url, strings.Repeat("0", 64)} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			dir := t.TempDir()
			target := filepath.Join(dir, "evesynapse")
			writeTestFile(t, target, []byte("old build"), 0o755)

			var out, errOut bytes.Buffer
			if code := runUpdate(testVersion, target, tc.args(srv.URL), &out, &errOut); code == 0 {
				t.Fatalf("code 0, want failure; stdout %q", out.String())
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "old build" {
				t.Fatal("target was replaced by a rejected download")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".evesynapse.update-") {
					t.Fatalf("leftover temp file %s after failed update", e.Name())
				}
			}
		})
	}
}

func TestRunUpdateHTTPRedirectAndChecksum(t *testing.T) {
	fresh := fakeELF(ownELFMachine(t), updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/new", http.StatusFound)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fresh)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{srv.URL + "/old", sum}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "verified against its checksum") {
		t.Fatalf("output %q missing checksum confirmation", out.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fresh) {
		t.Fatal("target not replaced after redirect download")
	}
}

// sha256FileHexBytes hashes in-memory bytes the same way
// sha256FileHex does for files, for checksum test fixtures.
func sha256FileHexBytes(b []byte) (string, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("evesynapse-test-sum-%d", os.Getpid()))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(path)
	return sha256FileHex(path)
}

// trustPidfiles makes the restart hand-off take a pidfile at its
// word for one test. The stand-in "server" in these tests is a
// sleep process, which the real check rightly refuses to signal.
func trustPidfiles(t *testing.T) {
	t.Helper()
	old := pidfile.ProcessCheck
	pidfile.ProcessCheck = func(int, string) bool { return true }
	t.Cleanup(func() { pidfile.ProcessCheck = old })
}

// TestRunUpdateNeverSignalsAForeignProcess: the pidfile is written
// by the service account and read by an update running as root, so
// a pidfile naming some other program must not get that program
// signalled. The update itself still lands.
func TestRunUpdateNeverSignalsAForeignProcess(t *testing.T) {
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc here: a process's program can't be checked on this system")
	}
	sleeper := pidfiletest.StartSleep(t)
	fresh := fakeELF(ownELFMachine(t), updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fresh)
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)
	writeTestFile(t, filepath.Join(dir, pidfile.Name),
		[]byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{srv.URL, sum}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "isn't EveSynapse") {
		t.Fatalf("output %q missing the not-our-process note", out.String())
	}
	if !pidfile.Alive(sleeper.Process.Pid) {
		t.Fatal("the update signalled a process that is not EveSynapse")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fresh) {
		t.Fatal("the update did not install")
	}
}

func TestRunUpdateRestartsRunningServer(t *testing.T) {
	trustPidfiles(t)
	sleeper := pidfiletest.StartSleep(t)
	fresh := fakeELF(ownELFMachine(t), updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fresh)
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)
	writeTestFile(t, filepath.Join(dir, pidfile.Name),
		[]byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{srv.URL, sum}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "restarting on the new version") {
		t.Fatalf("output %q missing restart confirmation", out.String())
	}
	if pidfile.Alive(sleeper.Process.Pid) {
		t.Fatal("the 'server' process is still alive after the restart hand-off")
	}
}

func TestUpdateProvider(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "")
	if got := updateProviders(); len(got) != 0 {
		t.Fatalf("providers = %q, want none (the default chain)", got)
	}
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", " Mirror ")
	if got := updateProviders(); len(got) != 1 || got[0] != "mirror" {
		t.Fatalf("providers = %q, want [mirror]", got)
	}
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "forgejo, gitlab ,")
	if got := updateProviders(); len(got) != 2 || got[0] != "forgejo" || got[1] != "gitlab" {
		t.Fatalf("providers = %q, want [forgejo gitlab]", got)
	}
}

func TestGitHubStableAssets(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "github")
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "someone/evesynapse")
	m, s, b, err := latestReleaseAssets(context.Background(), "arm64", false)
	if err != nil {
		t.Fatal(err)
	}
	base := "https://github.com/someone/evesynapse/releases/latest/download"
	if m != base+"/latest-arm64.json" || s != base+"/latest-arm64.json.sig" || b != base+"/evesynapse-arm64" {
		t.Fatalf("stable assets = %q, %q, %q", m, s, b)
	}
}

func TestGitHubDevAssets(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "github")
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "someone/evesynapse")
	t.Setenv("EVESYNAPSE_UPDATE_API_URL", "")
	mk := func(tag string, draft bool, signed bool) map[string]any {
		assets := []map[string]string{
			{"name": "latest-arm64.json", "browser_download_url": "https://dl.example/" + tag + "/latest-arm64.json"},
			{"name": "evesynapse-arm64", "browser_download_url": "https://dl.example/" + tag + "/evesynapse-arm64"},
		}
		if signed {
			assets = append(assets, map[string]string{"name": "latest-arm64.json.sig", "browser_download_url": "https://dl.example/" + tag + "/latest-arm64.json.sig"})
		}
		return map[string]any{"tag_name": tag, "draft": draft, "assets": assets}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/someone/evesynapse/releases" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			mk("v0.8.0.0", false, true),
			mk("v0.8.1.0-dev", false, true),
			mk("v0.8.2.0-dev", true, true),
			mk("v0.8.0.5-dev", false, true),
		})
	}))
	defer srv.Close()
	old := releaseGitHubAPIBaseURL
	releaseGitHubAPIBaseURL = srv.URL
	t.Cleanup(func() { releaseGitHubAPIBaseURL = old })
	m, s, b, err := latestReleaseAssets(context.Background(), "arm64", true)
	if err != nil {
		t.Fatal(err)
	}
	if m != "https://dl.example/v0.8.1.0-dev/latest-arm64.json" || s != "https://dl.example/v0.8.1.0-dev/latest-arm64.json.sig" || b != "https://dl.example/v0.8.1.0-dev/evesynapse-arm64" {
		t.Fatalf("dev assets = %q, %q, %q; want the v0.8.1.0-dev release's", m, s, b)
	}
}

func TestMirrorAssets(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "mirror")
	t.Setenv("EVESYNAPSE_UPDATE_MIRROR_URL", "https://mirror.example/releases/")
	m, s, b, err := latestReleaseAssets(context.Background(), "amd64", false)
	if err != nil {
		t.Fatal(err)
	}
	if m != "https://mirror.example/releases/latest-amd64.json" || s != m+".sig" || b != "https://mirror.example/releases/latest/evesynapse-amd64" {
		t.Fatalf("stable mirror assets = %q, %q, %q", m, s, b)
	}
	m, s, b, err = latestReleaseAssets(context.Background(), "amd64", true)
	if err != nil {
		t.Fatal(err)
	}
	if m != "https://mirror.example/releases/latest-dev-amd64.json" || s != m+".sig" || b != "https://mirror.example/releases/latest-dev/evesynapse-amd64" {
		t.Fatalf("dev mirror assets = %q, %q, %q", m, s, b)
	}
}

func TestUnknownProvider(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "carrier-pigeon")
	if _, _, _, err := latestReleaseAssets(context.Background(), "arm64", false); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}

// serveForgejoChannel answers the Forgejo release list the way
// the project's Forgejo does: the given tags, each carrying the
// three release assets under its own path on the same server.
func serveForgejoChannel(mux *http.ServeMux, tags ...string) {
	mux.HandleFunc("/api/v1/repos/EveSynapse/evesynapse/releases", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		var sb strings.Builder
		sb.WriteString("[")
		for i, tag := range tags {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `{"tag_name":%q,"draft":false,"assets":[`+
				`{"name":"latest-arm64.json","browser_download_url":%q},`+
				`{"name":"latest-arm64.json.sig","browser_download_url":%q},`+
				`{"name":"evesynapse-arm64","browser_download_url":%q}]}`,
				tag, base+"/"+tag+"/latest-arm64.json", base+"/"+tag+"/latest-arm64.json.sig", base+"/"+tag+"/evesynapse-arm64")
		}
		sb.WriteString("]")
		_, _ = io.WriteString(w, sb.String())
	})
}

func withForgejo(t *testing.T, url string) {
	t.Helper()
	t.Setenv("EVESYNAPSE_UPDATE_API_URL", "")
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "")
	old := releaseForgejoBaseURL
	releaseForgejoBaseURL = url
	t.Cleanup(func() { releaseForgejoBaseURL = old })
}

func TestForgejoStableAssets(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "forgejo")
	mux := http.NewServeMux()
	serveForgejoChannel(mux, "v0.7.0.9-dev", "v0.7.0.8", "v0.7.0.7")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withForgejo(t, srv.URL)
	m, s, b, err := latestReleaseAssets(context.Background(), "arm64", false)
	if err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/v0.7.0.8"
	if m != base+"/latest-arm64.json" || s != base+"/latest-arm64.json.sig" || b != base+"/evesynapse-arm64" {
		t.Fatalf("stable assets = %q, %q, %q; want the v0.7.0.8 release's", m, s, b)
	}
}

func TestForgejoDevAssets(t *testing.T) {
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "forgejo")
	mux := http.NewServeMux()
	serveForgejoChannel(mux, "v0.7.0.8", "v0.7.1.0-dev", "v0.7.0.5-dev")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withForgejo(t, srv.URL)
	m, s, b, err := latestReleaseAssets(context.Background(), "arm64", true)
	if err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/v0.7.1.0-dev"
	if m != base+"/latest-arm64.json" || s != base+"/latest-arm64.json.sig" || b != base+"/evesynapse-arm64" {
		t.Fatalf("dev assets = %q, %q, %q; want the v0.7.1.0-dev release's", m, s, b)
	}
}

func TestChainPicksNewestForge(t *testing.T) {
	key := trustTestKey(t)
	t.Setenv("EVESYNAPSE_UPDATE_PROVIDER", "")
	sum := strings.Repeat("0", 64)

	fjMux := http.NewServeMux()
	serveForgejoChannel(fjMux, "v9.9.9.0")
	publish(fjMux, "/v9.9.9.0/latest-arm64.json", manifestJSON("9.9.9.0", "arm64", sum), key)
	fjSrv := httptest.NewServer(fjMux)
	defer fjSrv.Close()
	withForgejo(t, fjSrv.URL)

	glMux := http.NewServeMux()
	serveChannel(glMux, channelRelease{tag: "v9.9.9.9", manifest: "/latest-arm64.json", sig: "/latest-arm64.json" + releasesig.SigSuffix, binary: "/evesynapse-arm64"})
	publish(glMux, "/latest-arm64.json", manifestJSON("9.9.9.9", "arm64", sum), key)
	glSrv := httptest.NewServer(glMux)
	defer glSrv.Close()
	oldAPI := releaseAPIBaseURL
	releaseAPIBaseURL = glSrv.URL
	t.Cleanup(func() { releaseAPIBaseURL = oldAPI })

	// GitHub and the mirror are unreachable; the chain must
	// still land on the newest forge that answers.
	oldGH := releaseGitHubBaseURL
	releaseGitHubBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { releaseGitHubBaseURL = oldGH })
	oldMirror := releaseMirrorBaseURL
	releaseMirrorBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { releaseMirrorBaseURL = oldMirror })

	var out bytes.Buffer
	m, provider, binaryURL, err := fetchReleaseManifest(context.Background(), &out, "arm64", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "9.9.9.9" {
		t.Fatalf("chain picked version %q, want 9.9.9.9 from GitLab", m.Version)
	}
	if provider != "gitlab" {
		t.Fatalf("chain picked provider %q, want gitlab", provider)
	}
	if binaryURL != glSrv.URL+"/evesynapse-arm64" {
		t.Fatalf("chain picked binary %q, want GitLab's", binaryURL)
	}
	// The read-out names each forge as it is checked, in the
	// default chain's order, and what each one found.
	ghLine := strings.Index(out.String(), "Checked GitHub: no usable release.")
	glLine := strings.Index(out.String(), "Checked GitLab: it has v9.9.9.9.")
	fjLine := strings.Index(out.String(), "Checked Forgejo: it has v9.9.9.0.")
	if ghLine < 0 || glLine < ghLine || fjLine < glLine {
		t.Fatalf("read-out %q: want GitHub (no usable release), then GitLab (v9.9.9.9), then Forgejo (v9.9.9.0)", out.String())
	}
}
