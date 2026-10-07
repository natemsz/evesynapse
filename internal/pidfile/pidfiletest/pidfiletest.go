// Package pidfiletest is test support for code that deals with the
// server's pidfile: it supplies a real, running process to point a
// pidfile at. Only tests import it.
package pidfiletest

import (
	"os/exec"
	"testing"
)

// StartSleep spawns a disposable process whose only job is to be
// a live, signal-able PID for pidfile tests. It is killed and reaped
// when the test ends. Where there is no sleep program the test is
// skipped.
func StartSleep(t *testing.T) *exec.Cmd {
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
