// Package selfupdate is `evesynapse -update`: the program replacing
// itself with a newer build.
//
//	evesynapse -update [-arm64|-x86]
//	    Check the release channel for a newer build of this
//	    computer's kind, and when there is one whose signature
//	    checks out download it, verify it against the checksum the
//	    signed manifest names, swap it into place, and restart onto
//	    it. With no flag the binary's own kind is used.
//
//	evesynapse -update -dev [-arm64|-x86]
//	    The same, from the newest development release.
//
//	evesynapse -update <url> <sha256>  /  -update <file> [sha256]
//	    Install a build from an explicit address or file: verify it
//	    really is an EveSynapse program for this kind of computer
//	    (ELF, and matching the checksum, which a network address
//	    must come with), swap it into place, and ask the running
//	    server to restart onto it. The running process keeps its
//	    old inode during the swap, so an update can never corrupt
//	    a copy that is serving pages right now.
//
// The restart hand-off runs through the server's pidfile (package
// pidfile): the updater signals that PID, after checking the process
// really is this program, and the service manager (Restart=always)
// brings the new build up. No systemctl call is attempted.
//
// What "verify" means for the release channel: the release's
// manifest has to carry a signature made with a release key this
// build was compiled with (package releasesig), and the build has to
// match the checksum that signed manifest names. A release that is
// not signed, or is signed with any other key, is refused before
// anything in it is read. The explicit forms are the operator's own
// word for a build: they check what the operator gives them, and no
// signature.
//
// The package knows nothing about the application: Run is told the
// running version rather than looking it up.
package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/dotenv"
	"evesynapse/internal/pidfile"
	"evesynapse/internal/releasesig"
)

// ---------------------------------------------------------------------------
// -update: download, verify, swap, restart.
// ---------------------------------------------------------------------------

const (
	// updateMaxBytes caps a download: a real build is ~25 MB,
	// so 200 MB is generous headroom that still catches a runaway
	// or hostile response.
	updateMaxBytes = 200 << 20
	// updateMinBytes rejects truncated or error-page downloads:
	// no EveSynapse build has ever been under a megabyte.
	updateMinBytes = 1 << 20
	// updateDownloadTimeout bounds the whole transfer.
	updateDownloadTimeout = 5 * time.Minute
	// updateRestartWait bounds the wait for the old server to
	// stop after SIGTERM.
	updateRestartWait = 20 * time.Second
	// elfMachineAArch64 is ELF e_machine for 64-bit ARM — the
	// only computer the deployed binary ever runs on.
	elfMachineAArch64 = 183
)

// ---------------------------------------------------------------------------
// The release channel: CI publishes every build as a GitHub
// release carrying a tiny manifest per CPU kind — the version it
// names and the checksum of the binary beside it — and that
// manifest's signature. The updater reads its kind's manifest,
// checks the signature, compares versions, and downloads only
// when the release is newer.
// ---------------------------------------------------------------------------

const (
	// elfMachineAMD64 is ELF e_machine for x86-64 — the desktop
	// build's counterpart to elfMachineAArch64.
	elfMachineAMD64 = 62
)

// releaseBaseURL is where release manifests and binaries live
// for the mainline project: GitHub's "latest release" download
// shortcut, so the address stays the same as versions come and
// go. A var so tests can point it at a local server.
var releaseBaseURL = "https://github.com/natemsz/evesynapse/releases/latest/download"

// releaseChannelBase resolves the address updates are fetched
// from. Setting EVESYNAPSE_UPDATE_REPO to a fork's owner/repo
// (in the environment, or in the .env file beside the binary)
// points the updater at that fork's releases instead of the
// mainline ones.
func releaseChannelBase() string {
	if repo := strings.Trim(strings.TrimSpace(os.Getenv("EVESYNAPSE_UPDATE_REPO")), "/"); repo != "" {
		return "https://github.com/" + repo + "/releases/latest/download"
	}
	return releaseBaseURL
}

// loadInstallEnv fills in configuration from the .env file
// beside the installed binary, so a hand-run `-update` sees the
// same settings the service runs with. Values already in the
// real environment win. A file that exists but cannot be read is
// an error: it may carry the update channel, and updating from
// the wrong channel is worse than not updating.
func loadInstallEnv(target string) error {
	return dotenv.Load(filepath.Join(filepath.Dir(target), ".env"))
}

// releaseManifest is the per-arch pointer published with each
// release: the version it names and the checksum of the binary
// beside it.
type releaseManifest struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
}

// parseArchArg normalizes an -update arch flag ("-arm64",
// "-amd64", "-x86") to the release arch name, or "" when the
// argument isn't an arch flag.
func parseArchArg(arg string) string {
	switch strings.ToLower(strings.TrimPrefix(arg, "-")) {
	case "arm64":
		return "arm64"
	case "amd64", "x86", "x86_64", "x64":
		return "amd64"
	}
	return ""
}

// ownReleaseArch is the CPU kind this very binary was built for.
func ownReleaseArch() string { return runtime.GOARCH }

// elfMachineForArch maps a release arch to its ELF e_machine.
func elfMachineForArch(arch string) (uint16, bool) {
	switch arch {
	case "arm64":
		return elfMachineAArch64, true
	case "amd64":
		return elfMachineAMD64, true
	}
	return 0, false
}

// compareVersions orders dotted numeric versions: -1 when a is
// the older, 0 when equal, +1 when a is the newer. A leading
// "v" is ignored and missing parts count as 0.
//
// Versions are four plain numbers, stable.major.feature.fix
// ("0.4.1.4"): the first is 0 until there is a feature-complete stable
// release. Releases up to 0.4.01.003 padded the last two with zeros;
// those still have to be ordered correctly, because an install on one
// of them compares it with the release it is offered. Each part is
// read as a number, so the padding makes no difference: "0.4.01.003"
// is 0.4.1.3, and 0.4.1.4 is the release after it.
//
// A development build's suffix ("0.4.1.4-dev") is not part of the number:
// the numbers are compared first, and when they tie the plain
// release is the newer of the two — a dev build leads up to the
// release that carries its number.
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
		v = v[:i] // the suffix is weighed by compareVersions, not here
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

// releaseAPIBaseURL is the GitHub API root the dev channel lists
// releases from. A var so tests can point it at a local server.
var releaseAPIBaseURL = "https://api.github.com"

// latestDevRelease queries the GitHub API for releases and returns
// the manifest, signature and binary asset URLs of the newest tag
// ending in -dev. All three come from that one release, so the
// signature is the manifest's own and the checksum in the manifest
// always describes the binary downloaded beside it.
func latestDevRelease(ctx context.Context, arch string) (manifestURL, sigURL, binaryURL string, err error) {
	repo := "natemsz/evesynapse"
	if r := strings.Trim(strings.TrimSpace(os.Getenv("EVESYNAPSE_UPDATE_REPO")), "/"); r != "" {
		repo = r
	}
	apiURL := releaseAPIBaseURL + "/repos/" + repo + "/releases?per_page=20"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "EveSynapse-Updater")
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("the update server answered %s", resp.Status)
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&releases); err != nil {
		return "", "", "", errors.New("the update information couldn't be read")
	}
	wantManifest := "latest-" + arch + ".json"
	wantSig := wantManifest + releasesig.SigSuffix
	wantBinary := "evesynapse-" + arch
	for _, rel := range releases {
		if !strings.HasSuffix(rel.TagName, "-dev") {
			continue
		}
		for _, a := range rel.Assets {
			switch a.Name {
			case wantManifest:
				manifestURL = a.BrowserDownloadURL
			case wantSig:
				sigURL = a.BrowserDownloadURL
			case wantBinary:
				binaryURL = a.BrowserDownloadURL
			}
		}
		if manifestURL == "" {
			return "", "", "", fmt.Errorf("dev release %s has no %s asset", rel.TagName, wantManifest)
		}
		if binaryURL == "" {
			return "", "", "", fmt.Errorf("dev release %s has no %s asset", rel.TagName, wantBinary)
		}
		if sigURL == "" {
			return "", "", "", &refusal{fmt.Sprintf("the newest development release (%s) isn't signed, so there is no way to tell it from a forgery.", rel.TagName)}
		}
		return manifestURL, sigURL, binaryURL, nil
	}
	return "", "", "", errors.New("no dev releases found")
}

// trustedReleaseKeys is the set of keys a release has to be signed
// with: the ones compiled into this build. A var so tests can stand
// in a key of their own.
var trustedReleaseKeys = releasesig.Trusted

// refusal is a release the updater will not install because it
// cannot be shown to be genuine. It is reported apart from a check
// that merely failed: nothing was wrong with the connection, and
// trying again will not help.
type refusal struct{ why string }

func (r *refusal) Error() string { return r.why }

// releaseStatusError is an answer other than 200 for one of a
// release's files.
type releaseStatusError struct {
	code   int
	status string
}

func (e *releaseStatusError) Error() string { return "the update server answered " + e.status }

// fetchReleaseFile downloads one of a release's small files, a
// manifest or its signature, into memory. Anything over limit bytes
// is not one of those and is not read.
func fetchReleaseFile(ctx context.Context, fileURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "EveSynapse-Updater")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &releaseStatusError{code: resp.StatusCode, status: resp.Status}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("the update information couldn't be read")
	}
	return body, nil
}

// verifyReleaseSignature fetches the signature published beside a
// manifest and checks it against the release keys this build trusts.
// The error is a *refusal when the release is not signed, or not by
// one of those keys.
func verifyReleaseSignature(ctx context.Context, manifest []byte, sigURL string) error {
	keys, err := trustedReleaseKeys()
	if err != nil || len(keys) == 0 {
		return &refusal{"this copy of EveSynapse was built without a usable release key, so it can't tell a genuine release from a forgery. To install a build you trust yourself: evesynapse -update <download address> <sha256>"}
	}
	sig, err := fetchReleaseFile(ctx, sigURL, releasesig.MaxSigBytes)
	var status *releaseStatusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return &refusal{"the newest release isn't signed, so there is no way to tell it from a forgery."}
	}
	if err != nil {
		return err
	}
	if err := releasesig.Verify(keys, manifest, sig); err != nil {
		return &refusal{"the newest release's signature doesn't check out: it was not signed with a release key this copy of EveSynapse trusts, or it was changed after it was signed."}
	}
	return nil
}

// fetchReleaseManifest downloads the manifest for arch from the
// release channel (the dev channel when dev is set), checks its
// signature, and returns it parsed with the address of the binary
// it describes.
func fetchReleaseManifest(ctx context.Context, arch string, dev bool) (releaseManifest, string, error) {
	var m releaseManifest
	manifestURL := releaseChannelBase() + "/latest-" + arch + ".json"
	sigURL := manifestURL + releasesig.SigSuffix
	binaryURL := releaseChannelBase() + "/evesynapse-" + arch
	if dev {
		var err error
		manifestURL, sigURL, binaryURL, err = latestDevRelease(ctx, arch)
		if err != nil {
			return m, "", err
		}
	}
	raw, err := fetchReleaseFile(ctx, manifestURL, releasesig.MaxManifestBytes)
	if err != nil {
		return m, "", err
	}
	if err := verifyReleaseSignature(ctx, raw, sigURL); err != nil {
		return m, "", err
	}
	// Only now is the manifest read. Nothing in it is acted on, or
	// even parsed, before its signature has been checked, and what is
	// parsed is the very bytes that were signed.
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&m); err != nil {
		return m, "", errors.New("the update information couldn't be read")
	}
	m.SHA256 = strings.ToLower(strings.TrimSpace(m.SHA256))
	if _, err := hex.DecodeString(m.SHA256); err != nil || len(m.SHA256) != sha256.Size*2 {
		return m, "", errors.New("the update information couldn't be read")
	}
	if strings.TrimSpace(m.Version) == "" {
		return m, "", errors.New("the update information couldn't be read")
	}
	// The signature covers the kind of computer too, so a genuine
	// manifest for another kind is not accepted in this one's place.
	if m.Arch != arch {
		return m, "", fmt.Errorf("the update information is for %q computers, not %q", m.Arch, arch)
	}
	return m, binaryURL, nil
}

// runReleaseUpdate implements the release-channel forms of
// -update (`-update`, `-update -arm64`, `-update -x86`): check
// the manifest for arch and its signature, and only when it is
// genuine and names a newer version download, verify, and install
// it. With dev=true it pulls the latest -dev tagged release
// instead of the main channel.
func runReleaseUpdate(current, target, arch string, stdout, stderr io.Writer, dev bool) int {
	machine, ok := elfMachineForArch(arch)
	if !ok {
		fmt.Fprintf(stderr, "EveSynapse doesn't publish builds for %q computers. Nothing was changed.\n", arch)
		return 2
	}
	if err := loadInstallEnv(target); err != nil {
		fmt.Fprintf(stderr, "Couldn't read the install's .env file: %v\nNothing was changed.\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Checking for updates… you're on %s.\n", current)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m, source, err := fetchReleaseManifest(ctx, arch, dev)
	cancel()
	if err != nil {
		var refused *refusal
		if errors.As(err, &refused) {
			fmt.Fprintf(stderr, "Update refused: %s\nNothing was changed.\n", refused.why)
			return 1
		}
		fmt.Fprintf(stderr, "Couldn't check for updates: %v\nNothing was changed.\n", err)
		return 1
	}
	latest := "v" + strings.TrimPrefix(m.Version, "v")
	// The release channel only ever installs releases. Should a
	// development build be published as the latest release by
	// mistake, it is left alone here rather than installed onto a
	// production box; -update -dev is the way to follow those.
	if !dev && versionIsPrerelease(m.Version) {
		fmt.Fprintf(stdout, "The newest published build (%s) is a development build, which a plain -update doesn't install. You're staying on %s.\n", latest, current)
		return 0
	}
	if compareVersions(m.Version, current) <= 0 {
		fmt.Fprintf(stdout, "You're up to date — %s is the latest version.\n", current)
		return 0
	}
	fmt.Fprintf(stdout, "A new version is available: %s.\n", latest)
	fmt.Fprintln(stdout, "Release signature verified.")
	if code := installUpdate(target, source, m.SHA256, machine, stdout, stderr); code != 0 {
		return code
	}
	if got, ok := installedVersion(target); ok {
		fmt.Fprintf(stdout, "EveSynapse is now on %s.\n", got)
	} else {
		fmt.Fprintf(stdout, "EveSynapse is now on %s.\n", latest)
	}
	return 0
}

// installedVersion runs the freshly installed binary's -version
// mode and returns what it reports, so an update reads its own
// new version back instead of trusting the download.
func installedVersion(target string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, target, "-version").Output()
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(out))
	if v == "" || !strings.HasPrefix(v, "v") {
		return "", false
	}
	return v, true
}

// Run implements `evesynapse -update`. It never starts the server
// and never touches the database. current is the version of the
// program doing the updating: what the release channel's newest
// build is compared against.
func Run(current string, args []string, stdout, stderr io.Writer) int {
	target, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't work out where EveSynapse lives: %v\n", err)
		return 1
	}
	return runUpdate(current, target, args, stdout, stderr)
}

func runUpdate(current, target string, args []string, stdout, stderr io.Writer) int {
	// Release-channel forms first: no argument (this computer's
	// own kind) or a single arch flag. The -dev flag pulls from
	// the dev branch releases (tags ending in -dev). Everything
	// else keeps the original explicit-address form below.
	dev := false
	filtered := args[:0]
	for _, a := range args {
		if a == "-dev" {
			dev = true
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered
	if len(args) == 0 {
		return runReleaseUpdate(current, target, ownReleaseArch(), stdout, stderr, dev)
	}
	if len(args) == 1 {
		if arch := parseArchArg(args[0]); arch != "" {
			return runReleaseUpdate(current, target, arch, stdout, stderr, dev)
		}
	}
	if len(args) > 2 {
		fmt.Fprintln(stderr, "Usage: evesynapse -update [-dev] [-arm64|-x86] or evesynapse -update <download address> <checksum> or evesynapse -update <file> [checksum]")
		return 2
	}
	source := args[0]
	wantHash := ""
	if len(args) == 2 {
		wantHash = strings.ToLower(strings.TrimSpace(args[1]))
		if _, err := hex.DecodeString(wantHash); err != nil || len(wantHash) != sha256.Size*2 {
			fmt.Fprintln(stderr, "That checksum doesn't look right — it should be 64 characters of 0-9 and a-f. Nothing was changed.")
			return 2
		}
	}
	// This runs as root and replaces the program root runs, so a
	// build fetched over the network is only ever installed against
	// a checksum the operator supplies: without one, whatever the
	// address answered with — over plain http, whatever anyone on
	// the path answered with — would be installed. A file already
	// on this computer is the operator's own and needs none.
	if updateSourceIsRemote(source) && wantHash == "" {
		fmt.Fprintln(stderr, "A download address needs the checksum published with that build, so the download can be checked before it is installed:\n  evesynapse -update <download address> <sha256>\nNothing was changed.")
		return 2
	}
	// The build has to be one this computer can run: the same kind
	// as the program doing the updating.
	machine, ok := elfMachineForArch(ownReleaseArch())
	if !ok {
		fmt.Fprintf(stderr, "EveSynapse doesn't publish builds for %q computers. Nothing was changed.\n", ownReleaseArch())
		return 2
	}

	return installUpdate(target, source, wantHash, machine, stdout, stderr)
}

// updateSourceIsRemote reports whether an explicit update source
// is a network address rather than a file on this computer.
func updateSourceIsRemote(source string) bool {
	u, err := url.Parse(source)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// installUpdate downloads source, verifies it (size, ELF
// machine kind, checksum when wantHash is set), swaps it into
// place at target, and asks the running server to restart onto
// it. Shared by the explicit-address and release-channel forms.
func installUpdate(target, source, wantHash string, wantMachine uint16, stdout, stderr io.Writer) int {
	dir := filepath.Dir(target)
	tmpPath := filepath.Join(dir, fmt.Sprintf(".evesynapse.update-%d", os.Getpid()))
	defer os.Remove(tmpPath) // no-op once the temp file has been renamed into place

	fmt.Fprintln(stdout, "Downloading the update…")
	ctx, cancel := context.WithTimeout(context.Background(), updateDownloadTimeout)
	defer cancel()
	if err := fetchUpdate(ctx, source, tmpPath); err != nil {
		fmt.Fprintf(stderr, "Couldn't download the update: %v\nNothing was changed.\n", err)
		return 1
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't check the downloaded file: %v\nNothing was changed.\n", err)
		return 1
	}
	if info.Size() < updateMinBytes {
		fmt.Fprintln(stderr, "The download is too small to be the EveSynapse program — it may not have downloaded properly. Nothing was changed.")
		return 1
	}
	if err := verifyExecutableMachine(tmpPath, wantMachine); err != nil {
		fmt.Fprintf(stderr, "%v\nNothing was changed.\n", err)
		return 1
	}
	if wantHash != "" {
		gotHash, err := sha256FileHex(tmpPath)
		if err != nil {
			fmt.Fprintf(stderr, "Couldn't check the downloaded file: %v\nNothing was changed.\n", err)
			return 1
		}
		if gotHash != wantHash {
			fmt.Fprintln(stderr, "The downloaded file doesn't match the checksum that came with it — it may have been damaged on the way. Nothing was changed.")
			return 1
		}
		fmt.Fprintln(stdout, "Download verified against its checksum.")
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		fmt.Fprintf(stderr, "Couldn't prepare the new version: %v\nNothing was changed.\n", err)
		return 1
	}
	if err := os.Rename(tmpPath, target); err != nil {
		fmt.Fprintf(stderr, "Couldn't install the new version: %v\nThe current version is still in place.\n", err)
		return 1
	}
	syncDir(dir)
	relabelExecutable(target)

	if pid, pidPath, ok := pidfile.FindLive(pidfile.Candidates(dir)); ok {
		fmt.Fprintln(stdout, "Update installed.")
		if !pidfile.ProcessCheck(pid, target) {
			fmt.Fprintf(stdout, "The pidfile %s names process %d, which isn't EveSynapse, so nothing was signalled. Restart EveSynapse yourself to start the new version (sudo systemctl restart evesynapse).\n", pidPath, pid)
			return 0
		}
		if pidfile.SignalRestart(pid, pidPath, updateRestartWait) {
			fmt.Fprintln(stdout, "EveSynapse is restarting on the new version.")
		} else {
			fmt.Fprintln(stdout, "EveSynapse is still finishing up — it will pick up the new version when it next restarts.")
		}
		return 0
	}
	fmt.Fprintln(stdout, "Update installed. EveSynapse isn't running right now — the new version will be in place next time it starts.")
	return 0
}

// fetchUpdate copies source (an http/https URL, a file:// URL,
// or a plain local path) into dstPath, streaming so even a slow
// link never holds the build in memory.
func fetchUpdate(ctx context.Context, source, dstPath string) error {
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	var body io.Reader
	if u, perr := url.Parse(source); perr == nil && (u.Scheme == "http" || u.Scheme == "https") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "EveSynapse-Updater")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the server answered %s", resp.Status)
		}
		if resp.ContentLength > updateMaxBytes {
			return fmt.Errorf("the download is larger than %d MB", updateMaxBytes>>20)
		}
		body = resp.Body
	} else {
		path := source
		if u, perr := url.Parse(source); perr == nil && u.Scheme == "file" {
			if u.Host != "" && u.Host != "localhost" {
				return fmt.Errorf("%q isn't a file on this computer", source)
			}
			path = u.Path
		} else if strings.Contains(source, "://") {
			return fmt.Errorf("%q isn't a download address EveSynapse understands", source)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		body = in
	}

	written, err := io.Copy(out, io.LimitReader(body, updateMaxBytes+1))
	if err != nil {
		return err
	}
	if written > updateMaxBytes {
		return fmt.Errorf("the download is larger than %d MB", updateMaxBytes>>20)
	}
	return out.Sync()
}

// verifyARMExecutable checks that path is an ELF program built
// for 64-bit ARM. Kept for the explicit-address update form,
// whose downloads are always the ARM build.
func verifyARMExecutable(path string) error {
	return verifyExecutableMachine(path, elfMachineAArch64)
}

// verifyExecutableMachine checks that path is an ELF program
// built for the given machine kind — the one check that keeps a
// wrong download (error page, build for another kind of
// computer) from ever replacing the working copy.
func verifyExecutableMachine(path string, wantMachine uint16) error {
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
		return errors.New("the download isn't an EveSynapse program — it may be an error page instead of the update")
	}
	if machine := binary.LittleEndian.Uint16(hdr[18:20]); machine != wantMachine {
		return errors.New("the download was built for a different kind of computer, so it wouldn't run here")
	}
	return nil
}

// sha256FileHex streams a file's SHA-256 as lowercase hex.
func sha256FileHex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// syncDir flushes a directory entry to disk (best effort): after
// the rename, the new name should survive a power cut too.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// relabelExecutable restores the file's SELinux label where the
// restorecon tool exists (the deployment box runs SELinux in
// enforcing mode, and a freshly downloaded file can carry the
// wrong label for a service to run). Best effort by design: on
// systems without SELinux it silently does nothing.
func relabelExecutable(path string) {
	bin, err := exec.LookPath("restorecon")
	if err != nil {
		return
	}
	_ = exec.Command(bin, path).Run()
}
