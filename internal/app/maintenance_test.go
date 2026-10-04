package app

// Hermetic tests for v0.3.05's self-maintenance modes: the -update
// download/verify/swap path (against httptest servers and local
// files only — never the network), the pidfile restart hand-off
// (against a throwaway `sleep` process), and -refresh's cache
// expiry against a seeded database, earned data included so the
// "never touched" list is proven, not promised.

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// startSleep spawns a disposable process whose only job is to be
// a live, signal-able PID for pidfile tests.
func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
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
	if got := Version(); got != "v0.3.07.014" {
		t.Fatalf("Version() = %q, want v0.3.07.014", got)
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
	if code := runUpdate(target, nil, &out, &errOut); code != 2 {
		t.Fatalf("no args: code %d, want 2", code)
	}
	if code := runUpdate(target, []string{"a", "b", "c"}, &out, &errOut); code != 2 {
		t.Fatalf("too many args: code %d, want 2", code)
	}
	if code := runUpdate(target, []string{"x", "not-a-checksum"}, &out, &errOut); code != 2 {
		t.Fatalf("malformed checksum: code %d, want 2", code)
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
		fresh := fakeELF(elfMachineAArch64, updateMinBytes+100)
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
	valid := fakeELF(elfMachineAArch64, updateMinBytes+100)
	wrongArch := fakeELF(62, updateMinBytes+100)

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
			args: func(url string) []string { return []string{url} },
		},
		{
			name: "wrong architecture",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(wrongArch)
			},
			args: func(url string) []string { return []string{url} },
		},
		{
			name: "truncated (too small)",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("tiny"))
			},
			args: func(url string) []string { return []string{url} },
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
			args: func(url string) []string { return []string{url} },
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
	fresh := fakeELF(elfMachineAArch64, updateMinBytes+100)
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

func TestRunUpdateRestartsRunningServer(t *testing.T) {
	sleeper := startSleep(t)
	fresh := fakeELF(elfMachineAArch64, updateMinBytes+100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fresh)
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "evesynapse")
	writeTestFile(t, target, []byte("old build"), 0o755)
	writeTestFile(t, filepath.Join(dir, serverPidfileName),
		[]byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	// Reap the sleeper as it dies: an unreaped child lingers as
	// a zombie that still answers signal-0, exactly why the real
	// deployment relies on the service manager reaping promptly.
	go func() { _ = sleeper.Wait() }()

	var out, errOut bytes.Buffer
	if code := runUpdate(target, []string{srv.URL}, &out, &errOut); code != 0 {
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
// data whose survival the test then asserts.
func seedRefreshDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evesynapse.db")
	conn, err := openDB(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
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
		 VALUES (779, '', 'pending', '')`,
		`INSERT INTO type_details (type_id, description, fetched_at)
		 VALUES (34, 'A mineral.', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000001, 'Jita IV-4', 'resolved', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000002, '', 'missing', '` + testFreshStamp + `')`,
		`INSERT INTO structure_names (structure_id, name, state, resolved_at)
		 VALUES (60000003, '', 'pending', '')`,
		`INSERT INTO sde_meta (key, value) VALUES ('sde_import_version', '5')`,
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
	return path, conn
}

func queryString(t *testing.T, conn *sql.DB, query string, args ...any) string {
	t.Helper()
	var v string
	if err := conn.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

func TestRefreshExpiresCachesKeepsEarnedData(t *testing.T) {
	path, conn := seedRefreshDB(t)
	conn.Close()

	cfg := Config{dbPath: path}
	var out, errOut bytes.Buffer
	// No pidfile anywhere near the fake executable path: run with
	// a path that doesn't exist so no live-server check fires.
	if code := runRefresh(cfg, filepath.Join(t.TempDir(), serverPidfileName), &out, &errOut); code != 0 {
		t.Fatalf("runRefresh code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "were not touched") {
		t.Fatalf("output %q missing the earned-data note", out.String())
	}

	conn2, err := openDB(path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer conn2.Close()

	// Every consulted cache rewound to the epoch.
	if got := queryString(t, conn2, `SELECT cached_until FROM character_snapshots WHERE character_id = 9001`); got != cacheEpoch {
		t.Fatalf("character_snapshots.cached_until = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT fetched_at FROM character_snapshots WHERE character_id = 9001`); got != cacheEpoch {
		t.Fatalf("character_snapshots.fetched_at = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT cached_until FROM characters WHERE character_id = 9001`); got != cacheEpoch {
		t.Fatalf("characters.cached_until = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT cached_until FROM global_snapshots WHERE kind = 'incursions'`); got != cacheEpoch {
		t.Fatalf("global_snapshots.cached_until = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT cached_until FROM guide_prices_meta WHERE id = 1`); got != cacheEpoch {
		t.Fatalf("guide_prices_meta.cached_until = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT attempted_at FROM snapshot_fetch_state WHERE character_id = 9001`); got != cacheEpoch {
		t.Fatalf("snapshot_fetch_state.attempted_at = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT attempted_at FROM market_fetch_state WHERE kind = 'history_10000002_34'`); got != cacheEpoch {
		t.Fatalf("market_fetch_state.attempted_at = %q, want epoch", got)
	}
	// Pilot records: ready rewound, missing settled, pending queued — as they were.
	if got := queryString(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 777`); got != cacheEpoch {
		t.Fatalf("pilot ready fetched_at = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 778`); got != testFreshStamp {
		t.Fatalf("pilot missing fetched_at = %q, want untouched", got)
	}
	if got := queryString(t, conn2, `SELECT fetched_at FROM pilot_records WHERE character_id = 779`); got != "" {
		t.Fatalf("pilot pending fetched_at = %q, want untouched empty", got)
	}
	// Type descriptions re-asked (empty stamp), text preserved meanwhile.
	if got := queryString(t, conn2, `SELECT fetched_at FROM type_details WHERE type_id = 34`); got != "" {
		t.Fatalf("type_details.fetched_at = %q, want empty (re-ask)", got)
	}
	if got := queryString(t, conn2, `SELECT description FROM type_details WHERE type_id = 34`); got != "A mineral." {
		t.Fatalf("type_details.description = %q, want preserved", got)
	}
	// Structure names: settled rows rewound, pending row as it was, names kept.
	if got := queryString(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000001`); got != cacheEpoch {
		t.Fatalf("structure resolved resolved_at = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000002`); got != cacheEpoch {
		t.Fatalf("structure missing resolved_at = %q, want epoch", got)
	}
	if got := queryString(t, conn2, `SELECT resolved_at FROM structure_names WHERE structure_id = 60000003`); got != "" {
		t.Fatalf("structure pending resolved_at = %q, want untouched empty", got)
	}
	if got := queryString(t, conn2, `SELECT name FROM structure_names WHERE structure_id = 60000001`); got != "Jita IV-4" {
		t.Fatalf("structure name = %q, want preserved", got)
	}
	// SDE re-import marker reset (anything but the current version).
	if got := queryString(t, conn2, `SELECT value FROM sde_meta WHERE key = 'sde_import_version'`); got == "5" || got == "" {
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
	path, conn := seedRefreshDB(t)
	conn.Close()

	sleeper := startSleep(t)
	pidfile := filepath.Join(t.TempDir(), serverPidfileName)
	writeTestFile(t, pidfile, []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)

	var out, errOut bytes.Buffer
	if code := runRefresh(Config{dbPath: path}, pidfile, &out, &errOut); code != 1 {
		t.Fatalf("runRefresh code %d, want refusal (1); stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "still running") {
		t.Fatalf("stderr %q missing the still-running explanation", errOut.String())
	}

	// The refusal happened before the database was touched.
	conn2, err := openDB(path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer conn2.Close()
	if got := queryString(t, conn2, `SELECT cached_until FROM character_snapshots WHERE character_id = 9001`); got != testFreshStamp {
		t.Fatalf("cache stamp = %q after refusal, want untouched fresh stamp", got)
	}
}

func TestRefreshMissingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.db")
	var out, errOut bytes.Buffer
	if code := runRefresh(Config{dbPath: missing}, "", &out, &errOut); code != 1 {
		t.Fatalf("runRefresh code %d, want 1", code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("runRefresh created a database where none existed")
	}
}
