package selfupdate

// Hermetic tests for -update: the download/verify/swap path (against
// httptest servers and local files only — never the network) and the
// restart hand-off (against a throwaway `sleep` process).
//
// None of these depends on which release the repository is at: the
// updater is told the running version, and the tests tell it
// testVersion. A release bump has nothing to edit here.

import (
	"bytes"
	"encoding/binary"
	"evesynapse/internal/pidfile"
	"evesynapse/internal/pidfile/pidfiletest"
	"fmt"
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
	bare := strings.TrimPrefix(testVersion, "v")
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
	if code := runUpdate(testVersion, target, []string{"-arm64"}, &out, &errOut); code != 0 {
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
	withReleaseBase(t, srv.URL)

	target := filepath.Join(t.TempDir(), "evesynapse")
	var out, errOut bytes.Buffer
	if code := runUpdate(testVersion, target, []string{"-arm64"}, &out, &errOut); code != 1 {
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
