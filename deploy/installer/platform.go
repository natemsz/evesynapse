// Platform detection: what this machine is, and which install
// steps only make sense on Linux. The installer runs on Linux,
// macOS and Windows; the Linux-only steps (systemd service and
// PostgreSQL provisioning) explain plainly on other systems
// that they need a Linux target, instead of failing obscurely.
package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// isLinux reports whether the installer is running on Linux.
func isLinux() bool { return runtime.GOOS == "linux" }

// targetArch is the CPU kind the release ships for this machine:
// amd64 or arm64, the two the release publishes.
func targetArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	default:
		return "amd64"
	}
}

// platformName reads out the platform for the review screen:
// "linux/amd64", "darwin/arm64", and so on.
func platformName() string { return runtime.GOOS + "/" + runtime.GOARCH }

// linuxNote is the plain explanation shown wherever a step
// needs Linux and this is not it.
func linuxNote(what string) string {
	return what + " needs a Linux machine. This installer is running on " +
		platformName() + ", so that step is skipped here: copy the binary " +
		"to your Linux server and run the installer there, or finish the " +
		"step by hand on the server."
}

// commandAvailable reports whether name is on PATH.
func commandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// runCmd runs name with args, returning its combined output and
// error. It is the shared helper for the install steps that shell
// out (systemctl, package installs).
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// hasSystemd reports whether this machine runs systemd.
func hasSystemd() bool {
	if !isLinux() {
		return false
	}
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

// serviceActive reports whether a systemd service is active.
// On a machine without systemd it is always false.
func serviceActive(unit string) bool {
	if !hasSystemd() {
		return false
	}
	_, err := runCmd("systemctl", "is-active", "--quiet", unit)
	return err == nil
}
