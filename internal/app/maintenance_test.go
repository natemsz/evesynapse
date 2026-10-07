package app

// Hermetic tests for v0.3.05's self-maintenance modes: the -update
// download/verify/swap path (against httptest servers and local
// files only — never the network), the pidfile restart hand-off
// (against a throwaway `sleep` process), and -refresh's cache
// expiry against a seeded database, earned data included so the
// "never touched" list is proven, not promised.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

// startSleep spawns a disposable process whose only job is to be
// a live, signal-able PID for pidfile tests.
func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	// One goroutine reaps the process the moment it dies, whoever
	// kills it. An unreaped child lingers as a zombie that still
	// answers signal-0 — exactly why the real deployment relies on
	// the service manager reaping promptly — and waiting on the
	// same Cmd from two places is a data race.
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd
}

func writeTestFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestVersionMatchesRelease(t *testing.T) {
	if got := Version(); got != "v0.3.39.001" {
		t.Fatalf("Version() = %q, want v0.3.39.001", got)
	}
}

func TestReleaseChannelBase(t *testing.T) {
	if got := releaseChannelBase(); got != releaseBaseURL {
		t.Fatalf("default channel = %q, want %q", got, releaseBaseURL)
	}
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "someone/evesynapse")
	want := "https://github.com/someone/evesynapse/releases/latest/download"
	if got := releaseChannelBase(); got != want {
		t.Fatalf("fork channel = %q, want %q", got, want)
	}
}

func TestLoadInstallEnv(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".env"),
		[]byte("EVESYNAPSE_UPDATE_REPO=someone/evesynapse\n"), 0o600)
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "")
	os.Unsetenv("EVESYNAPSE_UPDATE_REPO")
	loadInstallEnv(filepath.Join(dir, "evesynapse"))
	if got := os.Getenv("EVESYNAPSE_UPDATE_REPO"); got != "someone/evesynapse" {
		t.Fatalf("EVESYNAPSE_UPDATE_REPO = %q after loadInstallEnv", got)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.17.1", "0.3.16.9", 1},
		{"0.3.16.9", "0.3.17.1", -1},
		{"0.3.17.001", "0.3.17.1", 0},
		{"v0.3.17.1", "0.3.17.1", 0},
		{"0.3.17", "0.3.17.0", 0},
		{"0.3.17.2", "0.3.17.10", -1}, // numeric, not lexical
		{"0.3.17.10", "0.3.17.2", 1},
		// A -dev suffix is not part of the number: the numbers
		// decide first, and on a tie the plain release is newer.
		{"0.3.38.002-dev", "0.3.39.001-dev", -1},
		{"0.3.39.001-dev", "0.3.38.002-dev", 1},
		{"0.3.39.001-dev", "0.3.39.001-dev", 0},
		{"0.3.39.001-dev", "0.3.39.001", -1},
		{"0.3.39.001", "v0.3.39.001-dev", 1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
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

// withReleaseBase points the release channel at a test server
// for the duration of one test.
func withReleaseBase(t *testing.T, url string) {
	t.Helper()
	old := releaseBaseURL
	releaseBaseURL = url
	t.Cleanup(func() { releaseBaseURL = old })
}

func TestReleaseUpdateUpToDate(t *testing.T) {
	bare := strings.TrimPrefix(Version(), "v")
	mux := http.NewServeMux()
	mux.HandleFunc("/latest-arm64.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q,"arch":"arm64","sha256":%q}`, bare, strings.Repeat("0", 64))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseBase(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{"-arm64"}, &out, &errOut); code != 0 {
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
	fresh := fakeELF(elfMachineAArch64, updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest-arm64.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"9.9.9.9","arch":"arm64","sha256":%q}`, sum)
	})
	mux.HandleFunc("/evesynapse-arm64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fresh)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseBase(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{"-arm64"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "A new version is available: v9.9.9.9") {
		t.Fatalf("output %q missing the new-version note", out.String())
	}
	if !strings.Contains(out.String(), "now on v9.9.9.9") {
		t.Fatalf("output %q missing the new-version readout", out.String())
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
	fresh := fakeELF(elfMachineAMD64, updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest-amd64.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"9.9.9.9","arch":"amd64","sha256":%q}`, sum)
	})
	mux.HandleFunc("/evesynapse-amd64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fresh)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseBase(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{"-x86"}, &out, &errOut); code != 0 {
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
// ends up published as the latest release is never installed by a
// plain -update, whatever its number — nothing is even downloaded.
func TestReleaseUpdateLeavesDevBuildsAlone(t *testing.T) {
	fresh := fakeELF(elfMachineAArch64, updateMinBytes+100)
	sum, err := sha256FileHexBytes(fresh)
	if err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/latest-arm64.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"9.9.9.9-dev","arch":"arm64","sha256":%q}`, sum)
	})
	mux.HandleFunc("/evesynapse-arm64", func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(fresh)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withReleaseBase(t, srv.URL)

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{"-arm64"}, &out, &errOut); code != 0 {
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

// withReleaseAPI points the dev channel's release listing at a
// test server for the duration of one test.
func withReleaseAPI(t *testing.T, url string) {
	t.Helper()
	old := releaseAPIBaseURL
	releaseAPIBaseURL = url
	t.Cleanup(func() { releaseAPIBaseURL = old })
}

// TestDevUpdateInstallsTheDevReleasesOwnBinary: -update -dev takes
// the manifest and the binary from the same -dev release. The
// release channel's "latest" binary is a different build here, so
// downloading from there — what -dev first did — could only ever
// fail the dev manifest's checksum.
func TestDevUpdateInstallsTheDevReleasesOwnBinary(t *testing.T) {
	devBuild := fakeELF(elfMachineAArch64, updateMinBytes+100)
	releaseBuild := fakeELF(elfMachineAArch64, updateMinBytes+200)
	sum, err := sha256FileHexBytes(devBuild)
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	mux := http.NewServeMux()
	// The release listing, newest first: a plain release, then the
	// dev build the -dev channel is after.
	mux.HandleFunc("/repos/natemsz/evesynapse/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[
			{"tag_name":"v9.9.9.8","assets":[
				{"name":"latest-arm64.json","browser_download_url":%q},
				{"name":"evesynapse-arm64","browser_download_url":%q}]},
			{"tag_name":"v9.9.9.9-dev","assets":[
				{"name":"latest-arm64.json","browser_download_url":%q},
				{"name":"evesynapse-arm64","browser_download_url":%q}]}
		]`,
			srv.URL+"/latest-arm64.json", srv.URL+"/evesynapse-arm64",
			srv.URL+"/dev/latest-arm64.json", srv.URL+"/dev/evesynapse-arm64")
	})
	mux.HandleFunc("/dev/latest-arm64.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"9.9.9.9-dev","arch":"arm64","sha256":%q}`, sum)
	})
	mux.HandleFunc("/dev/evesynapse-arm64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(devBuild)
	})
	// The release channel's own files: another build entirely.
	mux.HandleFunc("/latest-arm64.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"9.9.9.8","arch":"arm64","sha256":%q}`, strings.Repeat("0", 64))
	})
	mux.HandleFunc("/evesynapse-arm64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(releaseBuild)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	withReleaseBase(t, srv.URL)
	withReleaseAPI(t, srv.URL)
	t.Setenv("EVESYNAPSE_UPDATE_REPO", "")

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{"-dev", "-arm64"}, &out, &errOut); code != 0 {
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
	withReleaseBase(t, srv.URL)

	target := filepath.Join(t.TempDir(), "evesynapse")
	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{"-arm64"}, &out, &errOut); code != 1 {
		t.Fatalf("code %d, want 1; stdout %q", code, out.String())
	}
	if !strings.Contains(errOut.String(), "Couldn't check for updates") {
		t.Fatalf("stderr %q missing the check failure", errOut.String())
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
	if code := runUpdate(target, []string{"a", "b", "c"}, &out, &errOut); code != 2 {
		t.Fatalf("too many args: code %d, want 2", code)
	}
	if code := runUpdate(target, []string{"x", "not-a-checksum"}, &out, &errOut); code != 2 {
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
	if code := runUpdate(target, []string{srv.URL + "/evesynapse"}, &out, &errOut); code != 2 {
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
		if code := runUpdate(target, []string{source}, &out, &errOut); code != 0 {
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
			if code := runUpdate(target, tc.args(srv.URL), &out, &errOut); code == 0 {
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
	if code := runUpdate(target, []string{srv.URL + "/old", sum}, &out, &errOut); code != 0 {
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
	old := serverProcessCheck
	serverProcessCheck = func(int, string) bool { return true }
	t.Cleanup(func() { serverProcessCheck = old })
}

// TestRunUpdateNeverSignalsAForeignProcess: the pidfile is written
// by the service account and read by an update running as root, so
// a pidfile naming some other program must not get that program
// signalled. The update itself still lands.
func TestRunUpdateNeverSignalsAForeignProcess(t *testing.T) {
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc here: a process's program can't be checked on this system")
	}
	sleeper := startSleep(t)
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
	writeTestFile(t, filepath.Join(dir, serverPidfileName),
		[]byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{srv.URL, sum}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "isn't EveSynapse") {
		t.Fatalf("output %q missing the not-our-process note", out.String())
	}
	if !processAlive(sleeper.Process.Pid) {
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

func TestPidRunsProgram(t *testing.T) {
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc here: a process's program can't be checked on this system")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !pidRunsProgram(os.Getpid(), exe) {
		t.Fatalf("pidRunsProgram(self, %q) = false", exe)
	}
	if pidRunsProgram(os.Getpid(), filepath.Join(t.TempDir(), "evesynapse")) {
		t.Fatal("pidRunsProgram matched this process against a program it isn't running")
	}
}

// TestServerPidfileLocations: under the service unit the pidfile
// goes to the runtime directory, because the install directory
// belongs to root; started by hand it still lands beside the
// executable.
func TestServerPidfileLocations(t *testing.T) {
	exeDir := t.TempDir()
	runtimeDir := t.TempDir()

	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)
	want := []string{
		filepath.Join(runtimeDir, serverPidfileName),
		filepath.Join(serverRuntimeDir, serverPidfileName),
		filepath.Join(exeDir, serverPidfileName),
	}
	got := serverPidfileCandidates(exeDir)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("candidates under the unit = %q, want %q", got, want)
	}
	if path := writeServerPidfile(got); path != want[0] {
		t.Fatalf("pidfile written to %q, want the runtime directory %q", path, want[0])
	}
	if pid, ok := readPidfile(want[0]); !ok || pid != os.Getpid() {
		t.Fatalf("runtime pidfile holds %d, %v; want own pid", pid, ok)
	}

	// No runtime directory (started by hand, or an older unit):
	// the first location is not writable, the next one is used.
	t.Setenv("RUNTIME_DIRECTORY", "")
	beside := filepath.Join(exeDir, serverPidfileName)
	fallback := []string{filepath.Join(t.TempDir(), "missing", serverPidfileName), beside}
	if path := writeServerPidfile(fallback); path != beside {
		t.Fatalf("pidfile written to %q, want %q beside the executable", path, beside)
	}
	if path := writeServerPidfile(fallback[:1]); path != "" {
		t.Fatalf("pidfile reported at %q with no writable location", path)
	}
}

// TestFindLiveServer: a maintenance run finds the live server in
// whichever pidfile location names one, skipping locations that
// are missing or stale, and reports which pidfile it was.
func TestFindLiveServer(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing", serverPidfileName)
	stale := filepath.Join(dir, "stale-"+serverPidfileName)
	live := filepath.Join(dir, serverPidfileName)
	writeTestFile(t, stale, []byte("not a pid\n"), 0o644)

	if _, _, ok := findLiveServer([]string{missing, stale}); ok {
		t.Fatal("findLiveServer found a server with no live pidfile")
	}

	sleeper := startSleep(t)
	writeTestFile(t, live, []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)
	if !processAlive(sleeper.Process.Pid) {
		t.Skip("this system can't probe whether a process is alive")
	}
	pid, pidfile, ok := findLiveServer([]string{missing, stale, live})
	if !ok || pid != sleeper.Process.Pid || pidfile != live {
		t.Fatalf("findLiveServer = %d, %q, %v; want the sleeper via %q", pid, pidfile, ok, live)
	}
}

func TestRunUpdateRestartsRunningServer(t *testing.T) {
	trustPidfiles(t)
	sleeper := startSleep(t)
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
	writeTestFile(t, filepath.Join(dir, serverPidfileName),
		[]byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{srv.URL, sum}, &out, &errOut); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "restarting on the new version") {
		t.Fatalf("output %q missing restart confirmation", out.String())
	}
	if processAlive(sleeper.Process.Pid) {
		t.Fatal("the 'server' process is still alive after the restart hand-off")
	}
}

func TestPidfileLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, serverPidfileName)

	// Absent pidfile: nothing live, nothing to remove.
	if _, ok := liveServerPID(path); ok {
		t.Fatal("liveServerPID true with no pidfile")
	}
	RemoveServerPidfile(path)

	// Own PID: readable, but a maintenance run never counts
	// itself as the server, and RemoveServerPidfile clears it.
	writeTestFile(t, path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	if pid, ok := readPidfile(path); !ok || pid != os.Getpid() {
		t.Fatalf("readPidfile = %d, %v; want own pid", pid, ok)
	}
	if _, ok := liveServerPID(path); ok {
		t.Fatal("liveServerPID counted this process as the server")
	}
	RemoveServerPidfile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("RemoveServerPidfile left its own pidfile behind")
	}

	// Someone else's PID: removal must not touch it, and a live
	// one reads as the server.
	sleeper := startSleep(t)
	writeTestFile(t, path, []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)
	RemoveServerPidfile(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("RemoveServerPidfile removed another process's pidfile")
	}
	if pid, ok := liveServerPID(path); !ok || pid != sleeper.Process.Pid {
		t.Fatalf("liveServerPID = %d, %v; want the sleeper", pid, ok)
	}

	// Garbage and dead PIDs read as "no server".
	writeTestFile(t, path, []byte("not a pid\n"), 0o644)
	if _, ok := liveServerPID(path); ok {
		t.Fatal("liveServerPID true for a garbage pidfile")
	}
}

func TestSignalServerRestartTimeout(t *testing.T) {
	// A process that ignores SIGTERM never stops; the watcher
	// must give up within its timeout rather than hang the update.
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 60")
	if err := cmd.Start(); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	dir := t.TempDir()
	pidfile := filepath.Join(dir, serverPidfileName)
	writeTestFile(t, pidfile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	start := time.Now()
	if signalServerRestart(cmd.Process.Pid, pidfile, 1500*time.Millisecond) {
		t.Fatal("signalServerRestart reported success for a process that ignored SIGTERM")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("signalServerRestart took %v with a 1.5s timeout", elapsed)
	}
}

// ---------------------------------------------------------------------------
// -refresh: expiry against a seeded database.
// ---------------------------------------------------------------------------

const testFreshStamp = "2026-10-04T00:00:00Z"

// seedRefreshDB builds a database holding one of everything the
// refresh cares about: live caches with fresh stamps, and earned
// data whose survival the test then asserts. Returns the DSN
// (for building the Config runRefresh takes) and the open handle.
func seedRefreshDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dsn := pgtest.FreshDSN(t)
	conn, pool, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	stmts := []string{
		`INSERT INTO users (id) VALUES (1)`,
		`INSERT INTO characters (character_id, user_id, name, access_token, refresh_token, cached_until)
		 VALUES (9001, 1, 'Test Char', 'access', 'refresh', '` + testFreshStamp + `')`,
		// Caches with fresh stamps.
		`INSERT INTO character_snapshots (character_id, kind, payload, fetched_at, cached_until)
		 VALUES (9001, 'wallet', '{"balance":1}', '` + testFreshStamp + `', '` + testFreshStamp + `')`,
		`INSERT INTO global_snapshots (kind, payload, fetched_at, cached_until)
		 VALUES ('incursions', '[]', '` + testFreshStamp + `', '` + testFreshStamp + `')`,
		`INSERT INTO guide_prices_meta (id, fetched_at, cached_until)
		 VALUES (1, '` + testFreshStamp + `', '` + testFreshStamp + `')`,
		`INSERT INTO snapshot_fetch_state (character_id, kind, state, detail, attempted_at)
		 VALUES (9001, 'skills', 'ok', '', '` + testFreshStamp + `')`,
		`INSERT INTO market_fetch_state (kind, state, detail, attempted_at)
		 VALUES ('history_10000002_34', 'ok', '', '` + testFreshStamp + `')`,
		`INSERT INTO pilot_records (character_id, payload, state, fetched_at)
		 VALUES (777, '{"name":"Pilot"}', 'ready', '` + testFreshStamp + `')`,
		`INSERT INTO pilot_records (character_id, payload, state, fetched_at)
		 VALUES (778, '', 'missing', '` + testFreshStamp + `')`,
		`INSERT INTO pilot_records (character_id, payload, state, fetched_at)
		 VALUES (779, '', 'pending', NULL)`,
		`INSERT INTO type_details (type_id, description, fetched_at)
		 VALUES (34, 'A mineral.', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000001, 'Jita IV-4', 'resolved', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000002, '', 'missing', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000003, '', 'pending', NULL)`,
		`INSERT INTO sde_meta (key, value) VALUES ('sde_import_version', '6')`,
		// Earned data: must survive untouched.
		`INSERT INTO wallet_history (user_id, character_id, day, balance, net_worth, sampled_at)
		 VALUES (1, 9001, '2026-10-03', 123.45, 999.5, '` + testFreshStamp + `')`,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count)
		 VALUES (10000002, 34, '2026-10-01', 5.5, 6.0, 5.0, 100, 10)`,
		`INSERT INTO market_watchlist (user_id, type_id, region_id, threshold_pct, created_at)
		 VALUES (1, 34, 10000002, 5.0, '` + testFreshStamp + `')`,
		`INSERT INTO widget_configs (user_id, widget_id, config, updated_at)
		 VALUES (1, 'orders', '{"scope":"all"}', '` + testFreshStamp + `')`,
		`INSERT INTO skill_plans (id, user_id, character_id, name, created_at)
		 VALUES (10, 1, 9001, 'My Plan', '` + testFreshStamp + `')`,
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return dsn, conn
}

func queryString(t *testing.T, conn *sql.DB, query string, args ...any) string {
	t.Helper()
	var v string
	if err := conn.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

// queryStamp reads one time column the way the app prints a time
// (RFC 3339, UTC), with "" for one that was never set.
func queryStamp(t *testing.T, conn *sql.DB, query string, args ...any) string {
	t.Helper()
	var v sql.NullTime
	if err := conn.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return rfc3339Or(v, "")
}

func TestRefreshExpiresCachesKeepsEarnedData(t *testing.T) {
	dsn, conn := seedRefreshDB(t)
	conn.Close()

	cfg := Config{databaseURL: dsn}
	var out, errOut bytes.Buffer
	// No pidfile anywhere near the fake executable path: run with
	// a path that doesn't exist so no live-server check fires.
	if code := runRefresh(cfg, filepath.Join(t.TempDir(), serverPidfileName), &out, &errOut); code != 0 {
		t.Fatalf("runRefresh code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "were not touched") {
		t.Fatalf("output %q missing the earned-data note", out.String())
	}

	conn2, pool2, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer conn2.Close()
	defer pool2.Close()

	// Every consulted cache rewound to the epoch.
	if got := queryStamp(t, conn2, `SELECT cached_until FROM character_snapshots WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("character_snapshots.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM character_snapshots WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("character_snapshots.fetched_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT cached_until FROM characters WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("characters.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT cached_until FROM global_snapshots WHERE kind = 'incursions'`); got != rfc3339(cacheEpoch) {
		t.Fatalf("global_snapshots.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT cached_until FROM guide_prices_meta WHERE id = 1`); got != rfc3339(cacheEpoch) {
		t.Fatalf("guide_prices_meta.cached_until = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT attempted_at FROM snapshot_fetch_state WHERE character_id = 9001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("snapshot_fetch_state.attempted_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT attempted_at FROM market_fetch_state WHERE kind = 'history_10000002_34'`); got != rfc3339(cacheEpoch) {
		t.Fatalf("market_fetch_state.attempted_at = %q, want epoch", got)
	}
	// Pilot records: ready rewound, missing settled, pending queued — as they were.
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 777`); got != rfc3339(cacheEpoch) {
		t.Fatalf("pilot ready fetched_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 778`); got != testFreshStamp {
		t.Fatalf("pilot missing fetched_at = %q, want untouched", got)
	}
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 779`); got != "" {
		t.Fatalf("pilot pending fetched_at = %q, want untouched empty", got)
	}
	// Type descriptions re-asked (empty stamp), text preserved meanwhile.
	if got := queryStamp(t, conn2, `SELECT fetched_at FROM type_details WHERE type_id = 34`); got != "" {
		t.Fatalf("type_details.fetched_at = %q, want empty (re-ask)", got)
	}
	if got := queryString(t, conn2, `SELECT description FROM type_details WHERE type_id = 34`); got != "A mineral." {
		t.Fatalf("type_details.description = %q, want preserved", got)
	}
	// Structure names: settled rows rewound, pending row as it was, names kept.
	if got := queryStamp(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000001`); got != rfc3339(cacheEpoch) {
		t.Fatalf("structure resolved resolved_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000002`); got != rfc3339(cacheEpoch) {
		t.Fatalf("structure missing resolved_at = %q, want epoch", got)
	}
	if got := queryStamp(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000003`); got != "" {
		t.Fatalf("structure pending resolved_at = %q, want untouched empty", got)
	}
	if got := queryString(t, conn2, `SELECT name FROM structure_names WHERE structure_id = 60000001`); got != "Jita IV-4" {
		t.Fatalf("structure name = %q, want preserved", got)
	}
	// SDE re-import marker reset (anything but the current version).
	if got := queryString(t, conn2, `SELECT value FROM sde_meta WHERE key = 'sde_import_version'`); got == "6" || got == "" {
		t.Fatalf("sde_import_version = %q, want a reset marker", got)
	}

	// Earned data byte-for-byte intact.
	if got := queryString(t, conn2, `SELECT access_token FROM characters WHERE character_id = 9001`); got != "access" {
		t.Fatalf("access_token = %q, want untouched", got)
	}
	if got := queryString(t, conn2, `SELECT payload FROM character_snapshots WHERE character_id = 9001`); got != `{"balance":1}` {
		t.Fatalf("snapshot payload = %q, want preserved", got)
	}
	var balance, netWorth float64
	if err := conn2.QueryRow(`SELECT balance, net_worth FROM wallet_history WHERE character_id = 9001`).Scan(&balance, &netWorth); err != nil {
		t.Fatal(err)
	}
	if balance != 123.45 || netWorth != 999.5 {
		t.Fatalf("wallet_history = %v/%v, want untouched", balance, netWorth)
	}
	var average float64
	if err := conn2.QueryRow(`SELECT average FROM market_history WHERE type_id = 34`).Scan(&average); err != nil {
		t.Fatal(err)
	}
	if average != 5.5 {
		t.Fatalf("market_history average = %v, want untouched", average)
	}
	if got := queryString(t, conn2, `SELECT config FROM widget_configs WHERE user_id = 1 AND widget_id = 'orders'`); got != `{"scope":"all"}` {
		t.Fatalf("widget_configs = %q, want untouched", got)
	}
	if got := queryString(t, conn2, `SELECT name FROM skill_plans WHERE id = 10`); got != "My Plan" {
		t.Fatalf("skill_plans = %q, want untouched", got)
	}
	var watchCount int
	if err := conn2.QueryRow(`SELECT COUNT(*) FROM market_watchlist WHERE user_id = 1`).Scan(&watchCount); err != nil {
		t.Fatal(err)
	}
	if watchCount != 1 {
		t.Fatalf("market_watchlist rows = %d, want untouched 1", watchCount)
	}
}

func TestRefreshRefusesWhileServerRuns(t *testing.T) {
	dsn, conn := seedRefreshDB(t)
	conn.Close()

	sleeper := startSleep(t)
	pidfile := filepath.Join(t.TempDir(), serverPidfileName)
	writeTestFile(t, pidfile, []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	var out, errOut bytes.Buffer
	if code := runRefresh(Config{databaseURL: dsn}, pidfile, &out, &errOut); code != 1 {
		t.Fatalf("runRefresh code %d, want refusal (1); stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "still running") {
		t.Fatalf("stderr %q missing the still-running explanation", errOut.String())
	}

	// The refusal happened before the database was touched.
	conn2, pool2, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer conn2.Close()
	defer pool2.Close()
	if got := queryStamp(t, conn2, `SELECT cached_until FROM character_snapshots WHERE character_id = 9001`); got != testFreshStamp {
		t.Fatalf("cache stamp = %q after refusal, want untouched fresh stamp", got)
	}
}

func TestRefreshMissingDatabase(t *testing.T) {
	// A DSN naming a database that was never created: the
	// refresh must refuse and must not create anything.
	missing := pgtest.DSNFor(t, "evetest_missing")
	var out, errOut bytes.Buffer
	if code := runRefresh(Config{databaseURL: missing}, "", &out, &errOut); code != 1 {
		t.Fatalf("runRefresh code %d, want 1", code)
	}
	if _, _, err := store.Open(context.Background(), missing); err == nil {
		t.Fatal("runRefresh created a database where none existed")
	}
}
