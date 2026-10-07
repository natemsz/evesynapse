// Package pidfile is the server's pidfile: how a maintenance run
// (the self-updater, or -refresh) finds the running server, makes sure
// the process it found really is the server, and asks it to restart.
//
// The server writes its PID at start-up (Start) and clears it on the
// way out (Remove). A maintenance run looks through the same
// locations (Candidates, FindLive).
package pidfile

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"evesynapse/internal/logging"
)

const Name = "evesynapse.pid"

// serverRuntimeDir is the server's own writable directory under
// the service unit (RuntimeDirectory=evesynapse). The install
// directory and the program in it belong to root, so the service
// account cannot write there — which is the point: root runs that
// program for `evesynapse -update`, so the account the server runs
// as must never be able to replace it.
const serverRuntimeDir = "/run/evesynapse"

// Candidates lists where the server's pidfile may
// live, most preferred first: the runtime directory systemd hands
// the service ($RUNTIME_DIRECTORY), that directory's fixed address
// (how a maintenance run, which has no such variable, finds it),
// and next to the executable — where it has always been, and still
// is for a server started by hand or under an older unit.
func Candidates(exeDir string) []string {
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
		path := filepath.Join(dir, Name)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths
}

// Start records the server's PID in the first
// pidfile location it can write and returns that path ("" when
// none is writable — the server keeps running either way; only the
// self-restart hand-off loses its grip). Called once at startup.
func Start() string {
	exe, err := os.Executable()
	if err != nil {
		logging.Warnf("evesynapse: pidfile: %v (self-restart unavailable)", err)
		return ""
	}
	return write(Candidates(filepath.Dir(exe)))
}

func write(candidates []string) string {
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

// FindLive looks through the pidfile locations for one that
// names a running server, returning its PID and the pidfile that
// named it.
func FindLive(candidates []string) (pid int, pidfile string, ok bool) {
	for _, path := range candidates {
		if pid, ok := LivePID(path); ok {
			return pid, path, true
		}
	}
	return 0, "", false
}

// ProcessCheck reports whether pid is running the program
// installed at target. The pidfile is written by the service
// account while an update runs as root, so its contents are a
// claim to check, not an instruction: root only ever signals a
// process that really is this program. A var so tests, whose
// stand-in "server" is a sleep process, can swap it out.
var ProcessCheck = runsProgram

// runsProgram compares /proc/<pid>/exe with target. A program
// file replaced under a running process reads back with a
// " (deleted)" suffix, which is exactly the state right after an
// update's swap. Where /proc is not available (anything but
// Linux) there is nothing to compare, and the pidfile is taken at
// its word as before.
func runsProgram(pid int, target string) bool {
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

// Remove clears the pidfile Start
// wrote. Only removes the file when it still names this process,
// so a slow shutdown can never delete a newer server's marker.
func Remove(path string) {
	if path == "" {
		return
	}
	if pid, ok := read(path); ok && pid == os.Getpid() {
		_ = os.Remove(path)
	}
}

// read parses the PID recorded in path.
func read(path string) (int, bool) {
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

// Alive reports whether pid names a live process.
// Permission errors count as alive: the process exists, we just
// may not signal it.
func Alive(pid int) bool {
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

// LivePID returns the running server's PID from the
// pidfile, or false when there is no pidfile, it names a dead
// process, or it names this very process (a maintenance run is
// never the server).
func LivePID(pidfilePath string) (int, bool) {
	pid, ok := read(pidfilePath)
	if !ok || pid == os.Getpid() {
		return 0, false
	}
	if !Alive(pid) {
		return 0, false
	}
	return pid, true
}

// SignalRestart asks the running server to stop (SIGTERM,
// the same signal the service manager sends) and waits for it to
// go away — process exited or pidfile removed, whichever the
// watcher sees first. Reports whether it stopped in time.
func SignalRestart(pid int, pidfilePath string, timeout time.Duration) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return !Alive(pid)
	}
	deadline := time.Now().Add(timeout)
	for {
		if !Alive(pid) {
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
