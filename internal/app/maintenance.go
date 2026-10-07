package app

// ---------------------------------------------------------------------------
// Self-maintenance modes (v0.3.05): the binary can look after
// itself on the box, so keeping EveSynapse current is two short
// commands instead of a manual binary shuffle.
//
//   evesynapse -version
//       Print the version and exit.
//
//   evesynapse -update [-arm64|-x86]
//       Check the release channel for a newer build of this
//       computer's kind, and when there is one download it,
//       verify it against the published checksum, swap it into
//       place, and restart onto it (the -update form below).
//       With no flag the binary's own kind is used.
//
//   evesynapse -update <url> <sha256>  /  -update <file> [sha256]
//       Download a new build from an explicit address, verify it
//       really is an EveSynapse program for this kind of
//       computer (ELF, and matching the checksum, which a
//       network address must come with), swap it
//       into place, and ask the running server to restart onto
//       it. The running process keeps its old inode during the
//       swap, so an update can never corrupt a copy that is
//       serving pages right now.
//
//   evesynapse -refresh
//       Mark every cached download out of date so the next start
//       fetches fresh copies. Earned data (accounts, watchlist,
//       saved layouts, collected history) is never touched.
//
// The restart hand-off runs through a pidfile the server writes
// in its runtime directory (or, started by hand, next to its own
// executable): the updater signals that PID — after checking the
// process really is this program — and the service manager
// (Restart=always) brings the new build up. No systemctl call is
// attempted.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	"syscall"
	"time"

	"evesynapse/internal/logging"
)

// Version returns the rendered product version ("v0.3.38.001"),
// the same string the page footer shows.
func Version() string { return appVersion }

// ---------------------------------------------------------------------------
// The server pidfile: how a maintenance run finds (and later
// restarts) the running server.
// ---------------------------------------------------------------------------

const serverPidfileName = "evesynapse.pid"

// serverRuntimeDir is the server's own writable directory under
// the service unit (RuntimeDirectory=evesynapse). The install
// directory and the program in it belong to root, so the service
// account cannot write there — which is the point: root runs that
// program for `evesynapse -update`, so the account the server runs
// as must never be able to replace it.
const serverRuntimeDir = "/run/evesynapse"

// serverPidfileCandidates lists where the server's pidfile may
// live, most preferred first: the runtime directory systemd hands
// the service ($RUNTIME_DIRECTORY), that directory's fixed address
// (how a maintenance run, which has no such variable, finds it),
// and next to the executable — where it has always been, and still
// is for a server started by hand or under an older unit.
func serverPidfileCandidates(exeDir string) []string {
	var dirs []string
	// systemd passes a list when a unit declares several runtime
	// directories; this unit declares one.
	if rt := filepath.SplitList(os.Getenv("RUNTIME_DIRECTORY")); len(rt) > 0 {
		dirs = append(dirs, rt[0])
	}
	dirs = append(dirs, serverRuntimeDir, exeDir)
	var paths []string
	seen := map[string]bool{}
	for _, dir := range dirs {
		path := filepath.Join(dir, serverPidfileName)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths
}

// StartServerPidfile records the server's PID in the first
// pidfile location it can write and returns that path ("" when
// none is writable — the server keeps running either way; only the
// self-restart hand-off loses its grip). Called once at startup.
func StartServerPidfile() string {
	exe, err := os.Executable()
	if err != nil {
		logging.Warnf("evesynapse: pidfile: %v (self-restart unavailable)", err)
		return ""
	}
	return writeServerPidfile(serverPidfileCandidates(filepath.Dir(exe)))
}

func writeServerPidfile(candidates []string) string {
	var lastErr error
	for _, path := range candidates {
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
			lastErr = err
			continue
		}
		return path
	}
	logging.Warnf("evesynapse: pidfile: no writable location (%v); self-restart unavailable", lastErr)
	return ""
}

// findLiveServer looks through the pidfile locations for one that
// names a running server, returning its PID and the pidfile that
// named it.
func findLiveServer(candidates []string) (pid int, pidfile string, ok bool) {
	for _, path := range candidates {
		if pid, ok := liveServerPID(path); ok {
			return pid, path, true
		}
	}
	return 0, "", false
}

// serverProcessCheck reports whether pid is running the program
// installed at target. The pidfile is written by the service
// account while an update runs as root, so its contents are a
// claim to check, not an instruction: root only ever signals a
// process that really is this program. A var so tests, whose
// stand-in "server" is a sleep process, can swap it out.
var serverProcessCheck = pidRunsProgram

// pidRunsProgram compares /proc/<pid>/exe with target. A program
// file replaced under a running process reads back with a
// " (deleted)" suffix, which is exactly the state right after an
// update's swap. Where /proc is not available (anything but
// Linux) there is nothing to compare, and the pidfile is taken at
// its word as before.
func pidRunsProgram(pid int, target string) bool {
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		if _, statErr := os.Stat("/proc/self/exe"); statErr != nil {
			return true // no /proc here
		}
		return false
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	if exe == target {
		return true
	}
	// The same file reached by another name (a symlinked install
	// directory).
	if resolved, err := filepath.EvalSymlinks(target); err == nil && exe == resolved {
		return true
	}
	return false
}

// RemoveServerPidfile clears the pidfile StartServerPidfile
// wrote. Only removes the file when it still names this process,
// so a slow shutdown can never delete a newer server's marker.
func RemoveServerPidfile(path string) {
	if path == "" {
		return
	}
	if pid, ok := readPidfile(path); ok && pid == os.Getpid() {
		_ = os.Remove(path)
	}
}

// readPidfile parses the PID recorded in path.
func readPidfile(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid names a live process.
// Permission errors count as alive: the process exists, we just
// may not signal it.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// liveServerPID returns the running server's PID from the
// pidfile, or false when there is no pidfile, it names a dead
// process, or it names this very process (a maintenance run is
// never the server).
func liveServerPID(pidfilePath string) (int, bool) {
	pid, ok := readPidfile(pidfilePath)
	if !ok || pid == os.Getpid() {
		return 0, false
	}
	if !processAlive(pid) {
		return 0, false
	}
	return pid, true
}

// signalServerRestart asks the running server to stop (SIGTERM,
// the same signal the service manager sends) and waits for it to
// go away — process exited or pidfile removed, whichever the
// watcher sees first. Reports whether it stopped in time.
func signalServerRestart(pid int, pidfilePath string, timeout time.Duration) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return !processAlive(pid)
	}
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return true
		}
		if _, err := os.Stat(pidfilePath); os.IsNotExist(err) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

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
// names and the checksum of the binary beside it. The updater
// reads its kind's manifest, compares versions, and downloads
// only when the release is newer.
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
// real environment win.
func loadInstallEnv(target string) {
	loadDotEnv(filepath.Join(filepath.Dir(target), ".env"))
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
// "v" is ignored and missing parts count as 0. A development
// build's suffix ("0.3.38.001-dev") is not part of the number:
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
// the manifest and binary asset URLs of the newest tag ending in
// -dev. Both come from that one release, so the checksum in the
// manifest always describes the binary downloaded beside it.
func latestDevRelease(ctx context.Context, arch string) (manifestURL, binaryURL string, err error) {
	repo := "natemsz/evesynapse"
	if r := strings.Trim(strings.TrimSpace(os.Getenv("EVESYNAPSE_UPDATE_REPO")), "/"); r != "" {
		repo = r
	}
	apiURL := releaseAPIBaseURL + "/repos/" + repo + "/releases?per_page=20"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "EveSynapse-Updater")
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("the update server answered %s", resp.Status)
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&releases); err != nil {
		return "", "", errors.New("the update information couldn't be read")
	}
	wantManifest := "latest-" + arch + ".json"
	wantBinary := "evesynapse-" + arch
	for _, rel := range releases {
		if !strings.HasSuffix(rel.TagName, "-dev") {
			continue
		}
		for _, a := range rel.Assets {
			switch a.Name {
			case wantManifest:
				manifestURL = a.BrowserDownloadURL
			case wantBinary:
				binaryURL = a.BrowserDownloadURL
			}
		}
		if manifestURL == "" {
			return "", "", fmt.Errorf("dev release %s has no %s asset", rel.TagName, wantManifest)
		}
		if binaryURL == "" {
			return "", "", fmt.Errorf("dev release %s has no %s asset", rel.TagName, wantBinary)
		}
		return manifestURL, binaryURL, nil
	}
	return "", "", errors.New("no dev releases found")
}

// fetchReleaseManifest downloads and parses the manifest for arch
// from the release channel (the dev channel when dev is set), and
// returns it with the address of the binary it describes.
func fetchReleaseManifest(ctx context.Context, arch string, dev bool) (releaseManifest, string, error) {
	var m releaseManifest
	manifestURL := releaseChannelBase() + "/latest-" + arch + ".json"
	binaryURL := releaseChannelBase() + "/evesynapse-" + arch
	if dev {
		var err error
		manifestURL, binaryURL, err = latestDevRelease(ctx, arch)
		if err != nil {
			return m, "", err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return m, "", err
	}
	req.Header.Set("User-Agent", "EveSynapse-Updater")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return m, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return m, "", fmt.Errorf("the update server answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m); err != nil {
		return m, "", errors.New("the update information couldn't be read")
	}
	m.SHA256 = strings.ToLower(strings.TrimSpace(m.SHA256))
	if _, err := hex.DecodeString(m.SHA256); err != nil || len(m.SHA256) != sha256.Size*2 {
		return m, "", errors.New("the update information couldn't be read")
	}
	if strings.TrimSpace(m.Version) == "" {
		return m, "", errors.New("the update information couldn't be read")
	}
	return m, binaryURL, nil
}

// runReleaseUpdate implements the release-channel forms of
// -update (`-update`, `-update -arm64`, `-update -x86`): check
// the manifest for arch, and only when it names a newer version
// download, verify, and install it. With dev=true it pulls the
// latest -dev tagged release instead of the main channel.
func runReleaseUpdate(target, arch string, stdout, stderr io.Writer, dev bool) int {
	machine, ok := elfMachineForArch(arch)
	if !ok {
		fmt.Fprintf(stderr, "EveSynapse doesn't publish builds for %q computers. Nothing was changed.\n", arch)
		return 2
	}
	loadInstallEnv(target)
	current := Version()
	fmt.Fprintf(stdout, "Checking for updates… you're on %s.\n", current)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m, source, err := fetchReleaseManifest(ctx, arch, dev)
	cancel()
	if err != nil {
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

// RunUpdate implements `evesynapse -update`. It never starts
// the server and never touches the database.
func RunUpdate(args []string, stdout, stderr io.Writer) int {
	target, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't work out where EveSynapse lives: %v\n", err)
		return 1
	}
	return runUpdate(target, args, stdout, stderr)
}

func runUpdate(target string, args []string, stdout, stderr io.Writer) int {
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
		return runReleaseUpdate(target, ownReleaseArch(), stdout, stderr, dev)
	}
	if len(args) == 1 {
		if arch := parseArchArg(args[0]); arch != "" {
			return runReleaseUpdate(target, arch, stdout, stderr, dev)
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

	if pid, pidfile, ok := findLiveServer(serverPidfileCandidates(dir)); ok {
		fmt.Fprintln(stdout, "Update installed.")
		if !serverProcessCheck(pid, target) {
			fmt.Fprintf(stdout, "The pidfile %s names process %d, which isn't EveSynapse, so nothing was signalled. Restart EveSynapse yourself to start the new version (sudo systemctl restart evesynapse).\n", pidfile, pid)
			return 0
		}
		if signalServerRestart(pid, pidfile, updateRestartWait) {
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

// ---------------------------------------------------------------------------
// -refresh: mark every cached download out of date.
// ---------------------------------------------------------------------------

// cacheEpoch is the timestamp every cache is rewound to: any
// freshness check in the app reads it as "as old as it gets".
const cacheEpoch = "1970-01-01T00:00:00Z"

// RunRefresh implements `evesynapse -refresh` (cfg detected the
// same way the server detects it). It refuses to run while the
// server is up: the box flow is stop the service, run this,
// start the service.
func RunRefresh(args []string, cfg Config, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "Usage: evesynapse -refresh")
		return 2
	}
	pidfile := ""
	exe, err := os.Executable()
	if err != nil {
		// Without a resolvable pidfile there is no live-server
		// check to make; say so and carry on rather than fail.
		fmt.Fprintln(stdout, "(Couldn't check whether EveSynapse is running — continuing anyway.)")
	} else if pid, live, ok := findLiveServer(serverPidfileCandidates(filepath.Dir(exe))); ok && serverProcessCheck(pid, exe) {
		// Hand runRefresh the pidfile that names a running server,
		// so it refuses; with none found there is nothing to check.
		// A stale pidfile whose PID now belongs to some other
		// program does not count.
		pidfile = live
	}
	return runRefresh(cfg, pidfile, stdout, stderr)
}

func runRefresh(cfg Config, pidfile string, stdout, stderr io.Writer) int {
	if pidfile != "" {
		if _, live := liveServerPID(pidfile); live {
			fmt.Fprintln(stderr, "EveSynapse is still running. Stop it first (sudo systemctl stop evesynapse), then run this again.")
			return 1
		}
	}
	conn, pool, err := openDB(context.Background(), cfg.databaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't open the EveSynapse database: %v\n", err)
		return 1
	}
	defer conn.Close()
	defer pool.Close()

	steps, err := expireCaches(conn)
	if err != nil {
		fmt.Fprintf(stderr, "The refresh didn't finish: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "Refresh ready — EveSynapse will fetch fresh copies of everything when it next starts:")
	for _, step := range steps {
		fmt.Fprintf(stdout, "  • %s\n", step)
	}
	fmt.Fprintln(stdout, "Your accounts, characters, watchlist, saved layouts, and collected history were not touched.")
	return 0
}

// cacheExpiry is one store's row-expire statement plus the
// sentence the refresh summary reports for it.
type cacheExpiry struct {
	label string
	sql   string
	args  []any
}

// expireCaches rewinds every cache the worker consults to the
// epoch and reports what it did, one human sentence per store.
// Payloads are kept — only the freshness stamps move — so pages
// keep showing the last known data until the fresh copies land,
// and anything the worker can't refetch (a parked character, a
// settled "not there" answer) simply stays as it was.
//
// Earned data is deliberately absent from this list: users,
// characters (tokens, tags, link state), sessions, home layouts
// and widget settings, the watchlist, skill plans, the daily
// wallet history, and the collected market history all survive.
func expireCaches(conn *sql.DB) ([]string, error) {
	expiries := []cacheExpiry{
		{
			label: "Character data (skills, wallets, assets, mail, contracts…)",
			// The ETag goes too: with it, the next fetch would only
			// ask ESI whether the copy is current and keep it. A
			// refresh is for downloading everything again.
			sql:  `UPDATE character_snapshots SET fetched_at = $1, cached_until = $2, etag = ''`,
			args: []any{cacheEpoch, cacheEpoch},
		},
		{
			label: "Character profiles",
			sql:   `UPDATE characters SET cached_until = $1 WHERE cached_until IS NOT NULL`,
			args:  []any{cacheEpoch},
		},
		{
			label: "Public data (incursions, faction warfare…)",
			sql:   `UPDATE global_snapshots SET fetched_at = $1, cached_until = $2, etag = ''`,
			args:  []any{cacheEpoch, cacheEpoch},
		},
		{
			// The stored /markets/prices/ mirror: an epoch
			// cached_until puts it outside ESI's window, so the
			// worker fetches it again on the first cycle.
			label: "Market price guide",
			sql:   `UPDATE guide_prices_meta SET fetched_at = $1, cached_until = $2`,
			args:  []any{cacheEpoch, cacheEpoch},
		},
		{
			// Retry timers, not data: rewinding attempted_at
			// lifts the "asked recently, wait" backoffs so the
			// refetch starts on the first cycle, not the next day.
			label: "Fetch retry timers",
			sql:   `UPDATE snapshot_fetch_state SET attempted_at = $1`,
			args:  []any{cacheEpoch},
		},
		{
			label: "Market fetch timers",
			sql:   `UPDATE market_fetch_state SET attempted_at = $1`,
			args:  []any{cacheEpoch},
		},
		{
			// Ready pilot records past their week-old cutoff are
			// drained again in the background; 'missing' rows are
			// settled answers and stay settled.
			label: "Pilot records",
			sql:   `UPDATE pilot_records SET fetched_at = $1 WHERE state = 'ready'`,
			args:  []any{cacheEpoch},
		},
		{
			// An empty fetched_at is exactly how the item page
			// asks for a description, so clearing it re-asks for
			// every description the ESI fallback ever supplied.
			label: "Item descriptions",
			sql:   `UPDATE type_details SET fetched_at = '' WHERE fetched_at != ''`,
		},
		{
			// Resolved names re-check after 30 days, missing ones
			// after a day; the epoch puts both past their windows.
			label: "Structure names",
			sql:   `UPDATE structure_names SET resolved_at = $1 WHERE state IN ('resolved', 'missing')`,
			args:  []any{cacheEpoch},
		},
	}

	var steps []string
	for _, expiry := range expiries {
		res, err := conn.Exec(expiry.sql, expiry.args...)
		if err != nil {
			return steps, fmt.Errorf("%s: %w", expiry.label, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return steps, fmt.Errorf("%s: %w", expiry.label, err)
		}
		steps = append(steps, fmt.Sprintf("%s: %s marked out of date", expiry.label, pluralCount(n, "entry", "entries")))
	}

	// The static game data (types, stations, systems…) re-imports
	// in its usual checked, all-or-nothing way on next start: any
	// marker other than the current one makes the importer run,
	// and a successful import writes the marker back.
	if _, err := conn.Exec(
		`INSERT INTO sde_meta (key, value) VALUES ('sde_import_version', 'refresh')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		return steps, fmt.Errorf("static game data: %w", err)
	}
	steps = append(steps, "Static game data (items, stations, systems…): will be re-imported")

	return steps, nil
}

// pluralCount formats "1 entry" / "12 entries" for the summary.
func pluralCount(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
