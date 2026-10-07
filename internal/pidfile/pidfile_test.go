package pidfile

import (
	"evesynapse/internal/pidfile/pidfiletest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPidRunsProgram(t *testing.T) {
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc here: a process's program can't be checked on this system")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !runsProgram(os.Getpid(), exe) {
		t.Fatalf("runsProgram(self, %q) = false", exe)
	}
	if runsProgram(os.Getpid(), filepath.Join(t.TempDir(), "evesynapse")) {
		t.Fatal("runsProgram matched this process against a program it isn't running")
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
		filepath.Join(runtimeDir, Name),
		filepath.Join(serverRuntimeDir, Name),
		filepath.Join(exeDir, Name),
	}
	got := Candidates(exeDir)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("candidates under the unit = %q, want %q", got, want)
	}
	if path := write(got); path != want[0] {
		t.Fatalf("pidfile written to %q, want the runtime directory %q", path, want[0])
	}
	if pid, ok := read(want[0]); !ok || pid != os.Getpid() {
		t.Fatalf("runtime pidfile holds %d, %v; want own pid", pid, ok)
	}

	// No runtime directory (started by hand, or an older unit):
	// the first location is not writable, the next one is used.
	t.Setenv("RUNTIME_DIRECTORY", "")
	beside := filepath.Join(exeDir, Name)
	fallback := []string{filepath.Join(t.TempDir(), "missing", Name), beside}
	if path := write(fallback); path != beside {
		t.Fatalf("pidfile written to %q, want %q beside the executable", path, beside)
	}
	if path := write(fallback[:1]); path != "" {
		t.Fatalf("pidfile reported at %q with no writable location", path)
	}
}

// TestFindLiveServer: a maintenance run finds the live server in
// whichever pidfile location names one, skipping locations that
// are missing or stale, and reports which pidfile it was.
func TestFindLiveServer(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing", Name)
	stale := filepath.Join(dir, "stale-"+Name)
	live := filepath.Join(dir, Name)
	writeTestFile(t, stale, []byte("not a pid\n"), 0o644)

	if _, _, ok := FindLive([]string{missing, stale}); ok {
		t.Fatal("FindLive found a server with no live pidfile")
	}

	sleeper := pidfiletest.StartSleep(t)
	writeTestFile(t, live, []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)
	if !Alive(sleeper.Process.Pid) {
		t.Skip("this system can't probe whether a process is alive")
	}
	pid, pidfile, ok := FindLive([]string{missing, stale, live})
	if !ok || pid != sleeper.Process.Pid || pidfile != live {
		t.Fatalf("FindLive = %d, %q, %v; want the sleeper via %q", pid, pidfile, ok, live)
	}
}

func TestPidfileLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Name)

	// Absent pidfile: nothing live, nothing to remove.
	if _, ok := LivePID(path); ok {
		t.Fatal("LivePID true with no pidfile")
	}
	Remove(path)

	// Own PID: readable, but a maintenance run never counts
	// itself as the server, and Remove clears it.
	writeTestFile(t, path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	if pid, ok := read(path); !ok || pid != os.Getpid() {
		t.Fatalf("read = %d, %v; want own pid", pid, ok)
	}
	if _, ok := LivePID(path); ok {
		t.Fatal("LivePID counted this process as the server")
	}
	Remove(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Remove left its own pidfile behind")
	}

	// Someone else's PID: removal must not touch it, and a live
	// one reads as the server.
	sleeper := pidfiletest.StartSleep(t)
	writeTestFile(t, path, []byte(strconv.Itoa(sleeper.Process.Pid)+"\n"), 0o644)
	Remove(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Remove removed another process's pidfile")
	}
	if pid, ok := LivePID(path); !ok || pid != sleeper.Process.Pid {
		t.Fatalf("LivePID = %d, %v; want the sleeper", pid, ok)
	}

	// Garbage and dead PIDs read as "no server".
	writeTestFile(t, path, []byte("not a pid\n"), 0o644)
	if _, ok := LivePID(path); ok {
		t.Fatal("LivePID true for a garbage pidfile")
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
	pidfile := filepath.Join(dir, Name)
	writeTestFile(t, pidfile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	start := time.Now()
	if SignalRestart(cmd.Process.Pid, pidfile, 1500*time.Millisecond) {
		t.Fatal("SignalRestart reported success for a process that ignored SIGTERM")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("SignalRestart took %v with a 1.5s timeout", elapsed)
	}
}

func writeTestFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
