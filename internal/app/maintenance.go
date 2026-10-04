package app

// ---------------------------------------------------------------------------
// Self-maintenance modes (v0.3.05): the binary can look after
// itself on the box, so keeping EveSynapse current is two short
// commands instead of a manual binary shuffle.
//
//   evesynapse -version
//       Print the version and exit.
//
//   evesynapse -update <url|file> [sha256]
//       Download a new build, verify it really is an EveSynapse
//       program for this kind of computer (ARM ELF, optionally
//       checksum-matched), swap it into place, and ask the
//       running server to restart onto it. The running process
//       keeps its old inode during the swap, so an update can
//       never corrupt a copy that is serving pages right now.
//
//   evesynapse -refresh
//       Mark every cached download out of date so the next start
//       fetches fresh copies. Earned data (accounts, watchlist,
//       saved layouts, collected history) is never touched.
//
// The restart hand-off runs through a pidfile the server writes
// next to its own executable: the updater signals that PID and
// the service manager (Restart=always) brings the new build up.
// No systemctl call is attempted — the updater deliberately runs
// without the privileges that would need.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version returns the rendered product version ("v0.3.07.011"),
// the same string the page footer shows.
func Version() string { return appVersion }

// ---------------------------------------------------------------------------
// The server pidfile: how a maintenance run finds (and later
// restarts) the running server.
// ---------------------------------------------------------------------------

// serverPidfileName sits next to the executable so it works no
// matter which directory the service is started from.
const serverPidfileName = "evesynapse.pid"

// serverPidfilePath resolves <executable dir>/evesynapse.pid.
func serverPidfilePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), serverPidfileName), nil
}

// StartServerPidfile records the server's PID next to the
// executable and returns the pidfile path ("" when the location
// isn't writable — the server keeps running either way; only the
// self-restart hand-off loses its grip). Called once at startup.
func StartServerPidfile() string {
	path, err := serverPidfilePath()
	if err != nil {
		log.Printf("evesynapse: pidfile: %v (self-restart unavailable)", err)
		return ""
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		log.Printf("evesynapse: pidfile: write %s: %v (self-restart unavailable)", path, err)
		return ""
	}
	return path
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

// RunUpdate implements `evesynapse -update <url> [sha256]`. It
// never starts the server and never touches the database.
func RunUpdate(args []string, stdout, stderr io.Writer) int {
	target, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't work out where EveSynapse lives: %v\n", err)
		return 1
	}
	return runUpdate(target, args, stdout, stderr)
}

func runUpdate(target string, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(stderr, "Usage: evesynapse -update <download address> [checksum]")
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
	if err := verifyARMExecutable(tmpPath); err != nil {
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

	pidfile := filepath.Join(dir, serverPidfileName)
	if pid, ok := liveServerPID(pidfile); ok {
		fmt.Fprintln(stdout, "Update installed.")
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
// for 64-bit ARM — the one check that keeps a wrong download
// (error page, build for another kind of computer) from ever
// replacing the working copy.
func verifyARMExecutable(path string) error {
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
	if machine := binary.LittleEndian.Uint16(hdr[18:20]); machine != elfMachineAArch64 {
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
	pidfile, err := serverPidfilePath()
	if err != nil {
		// Without a resolvable pidfile there is no live-server
		// check to make; say so and carry on rather than fail.
		fmt.Fprintln(stdout, "(Couldn't check whether EveSynapse is running — continuing anyway.)")
		pidfile = ""
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
	dbPath := cfg.DBPath()
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stderr, "No EveSynapse data found at %s — there's nothing to refresh yet.\n", dbPath)
		return 1
	}
	conn, err := openDB(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "Couldn't open the EveSynapse data: %v\n", err)
		return 1
	}
	defer conn.Close()

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
			sql:   `UPDATE character_snapshots SET fetched_at = ?, cached_until = ?`,
			args:  []any{cacheEpoch, cacheEpoch},
		},
		{
			label: "Character profiles",
			sql:   `UPDATE characters SET cached_until = ? WHERE cached_until IS NOT NULL`,
			args:  []any{cacheEpoch},
		},
		{
			label: "Public data (incursions, faction warfare…)",
			sql:   `UPDATE global_snapshots SET fetched_at = ?, cached_until = ?`,
			args:  []any{cacheEpoch, cacheEpoch},
		},
		{
			// The stored /markets/prices/ mirror: an epoch
			// cached_until puts it outside ESI's window, so the
			// worker fetches it again on the first cycle.
			label: "Market price guide",
			sql:   `UPDATE guide_prices_meta SET fetched_at = ?, cached_until = ?`,
			args:  []any{cacheEpoch, cacheEpoch},
		},
		{
			// Retry timers, not data: rewinding attempted_at
			// lifts the "asked recently, wait" backoffs so the
			// refetch starts on the first cycle, not the next day.
			label: "Fetch retry timers",
			sql:   `UPDATE snapshot_fetch_state SET attempted_at = ?`,
			args:  []any{cacheEpoch},
		},
		{
			label: "Market fetch timers",
			sql:   `UPDATE market_fetch_state SET attempted_at = ?`,
			args:  []any{cacheEpoch},
		},
		{
			// Ready pilot records past their week-old cutoff are
			// drained again in the background; 'missing' rows are
			// settled answers and stay settled.
			label: "Pilot records",
			sql:   `UPDATE pilot_records SET fetched_at = ? WHERE state = 'ready'`,
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
			sql:   `UPDATE structure_names SET resolved_at = ? WHERE state IN ('resolved', 'missing')`,
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
