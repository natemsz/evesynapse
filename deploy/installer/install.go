// The install itself: setup.sh (on Linux) or the local phases
// (elsewhere), then starting the service. Both interfaces drive
// it through installCallbacks, so the TUI and the plain mode
// show the same steps.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// installStep is one progress step: its plain-language name and
// the marker lines in setup.sh's output that say it is done. A
// step with no markers is driven by the installer's own phases.
type installStep struct {
	name    string
	markers []string
}

// installStepsFor is the progress list for this install. On
// Linux the app is installed by setup.sh, then the service is
// started. Elsewhere the installer only gets the binary and
// writes its settings; the Linux steps are explained, not run.
func installStepsFor(a *answers) []installStep {
	if !isLinux() {
		return []installStep{
			{name: "Getting EveSynapse"},
			{name: "Writing your settings"},
		}
	}
	getting := installStep{name: "Downloading EveSynapse", markers: []string{"Verified EveSynapse"}}
	if a.source == sourceBuild {
		// The build prints "Building this checkout..." when it
		// starts; it is done when the binary is installed, so
		// this step carries no marker of its own and completes
		// when the next step's marker appears.
		getting = installStep{name: "Building EveSynapse from source"}
	}
	return []installStep{
		getting,
		{name: "Installing the app", markers: []string{"Installed the program to"}},
		{name: "Setting up the database", markers: []string{"PostgreSQL ready"}},
		{name: "Writing your settings", markers: []string{"Wrote "}},
		{name: "Installing the service", markers: []string{"Service installed"}},
		{name: "Starting EveSynapse"},
	}
}

// installCallbacks is how the install reports back: Step is
// called with the count of steps done so far, Line with every
// setup.sh output line (kept for the failure screen), and Log
// with a plain-language note from one of the installer's own
// phases.
type installCallbacks struct {
	Step func(done int)
	Line func(line string)
	Log  func(msg string)
}

// runInstall executes the install for a. It returns the full
// output (setup.sh's, plus the installer's own notes) for the
// failure screen, and an error naming the step that failed.
func runInstall(ctx context.Context, a *answers, cb installCallbacks) (string, error) {
	var out strings.Builder
	line := func(s string) {
		out.WriteString(s + "\n")
		if cb.Line != nil {
			cb.Line(s)
		}
	}
	log := func(s string) {
		line(s)
		if cb.Log != nil {
			cb.Log(s)
		}
	}
	steps := installStepsFor(a)
	step := func(done int) {
		if cb.Step != nil {
			cb.Step(done)
		}
	}

	if !isLinux() {
		err := runInstallLocal(ctx, a, steps, step, line, log)
		return out.String(), err
	}
	err := runInstallLinux(ctx, a, steps, step, line, log)
	return out.String(), err
}

// runInstallLinux installs on Linux: setup.sh does the binary,
// database, settings and service, then the installer starts the
// service so the site answers when it says it is ready.
func runInstallLinux(ctx context.Context, a *answers, steps []installStep, step func(int), line, log func(string)) error {
	done := 0 // steps done so far
	// The download path already fetched the binary before the
	// prompts, so its first step is done when the install starts.
	if a.source == sourceDownload {
		done = 1
		step(done)
	}

	script, cleanup, env, err := prepareSetupRun(ctx, a)
	if err != nil {
		return fmt.Errorf("could not get the installer ready: %v", err)
	}
	defer cleanup()

	args := []string{}
	if a.source == sourceBuild {
		args = []string{"--build"}
	}
	if err := runSetupScript(ctx, script, args, env, steps, done, step, line); err != nil {
		return err
	}
	// setup.sh's markers cover steps 0-4; the service step is
	// the last one it reports.
	done = 5
	step(done)

	// Start the app, so the site answers when the installer
	// says it is ready. A service that will not start is a
	// warning on the done screen, not a failed install: the
	// files are in place and the operator can see why from the
	// journal. Without systemd there is nothing to start: the
	// operator starts the service by hand.
	if hasSystemd() {
		if out, err := runCmd("systemctl", "start", "evesynapse"); err != nil {
			log("Warning: EveSynapse did not start (" + strings.TrimSpace(out) + "). See: journalctl -u evesynapse")
			a.startWarning = "EveSynapse did not start; see: journalctl -u evesynapse"
		}
	} else {
		log("No systemd on this machine; start EveSynapse by hand: evesynapse")
		a.startWarning = "No systemd on this machine; start EveSynapse by hand: evesynapse"
	}
	done = 6
	step(done)
	return nil
}

// prepareSetupRun gets the embedded setup.sh ready to run and
// returns the script path, a cleanup function, and the
// environment to run it with. For a build from source the
// script is written into the checkout's deploy/ directory, so
// setup.sh works out the checkout as its parent; for a download
// it runs from a temp directory laid out like a checkout root,
// with the .env.example and service unit fetched from the
// release's provider where they could be.
func prepareSetupRun(ctx context.Context, a *answers) (string, func(), []string, error) {
	env := append(os.Environ(), a.envVars()...)
	if a.source == sourceBuild {
		deployDir := filepath.Join(a.sourceDir, "deploy")
		script, err := writeEmbeddedSetup(deployDir, ".installer-setup.sh")
		if err != nil {
			return "", nil, nil, err
		}
		return script, func() { os.Remove(script) }, env, nil
	}

	tmpRoot, err := os.MkdirTemp("", "evesynapse-install-*")
	if err != nil {
		return "", nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmpRoot) }
	var envExample, service []byte
	if a.release != nil {
		tag := "v" + a.release.Version
		if data, err := fetchRawFile(ctx, a.release.Provider, tag, ".env.example"); err == nil {
			envExample = data
		}
		if data, err := fetchRawFile(ctx, a.release.Provider, tag, "deploy/evesynapse.service"); err == nil {
			service = data
		}
	}
	script, err := setupRunDir(tmpRoot, envExample, service)
	if err != nil {
		cleanup()
		return "", nil, nil, err
	}
	if a.downloadedBinary != "" {
		env = append(env, "EVESYNAPSE_BINARY="+a.downloadedBinary)
	}
	return script, cleanup, env, nil
}

// runSetupScript runs bash on script with args and env, calling
// step with the count of steps done as the marker lines appear.
// A step whose marker never appears (a local database that was
// already there, for example) is passed over when a later
// step's marker shows: the scan always moves to the highest
// marker seen so far. It returns an error naming the step the
// script stopped at.
func runSetupScript(ctx context.Context, script string, args, env []string, steps []installStep, startStep int, step func(int), line func(string)) error {
	cmd := exec.CommandContext(ctx, "bash", append([]string{script}, args...)...)
	cmd.Env = env
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	done := startStep
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		l := scanner.Text()
		line(l)
		matched := -1
		for j := done; j < len(steps); j++ {
			for _, mk := range steps[j].markers {
				if mk != "" && strings.Contains(l, mk) {
					matched = j
					break
				}
			}
		}
		if matched >= done {
			done = matched + 1
			step(done)
		}
	}
	if err := cmd.Wait(); err != nil {
		name := "Finishing up"
		if done < len(steps) {
			name = steps[done].name
		}
		return fmt.Errorf("the install stopped at %s: %v", name, err)
	}
	return nil
}

// runInstallLocal installs where there is no Linux to install
// onto: the binary is already fetched (download) or built here
// (build from source), and the settings are written into a .env
// beside it. The Linux steps are explained, not run.
func runInstallLocal(ctx context.Context, a *answers, steps []installStep, step func(int), line, log func(string)) error {
	binPath := ""
	if a.source == sourceDownload {
		// The Linux build the release ships cannot run here;
		// it is saved for the Linux server it is for.
		dst := filepath.Join(a.installDir, exeName())
		if err := copyFile(a.downloadedBinary, dst, 0o755); err != nil {
			return fmt.Errorf("could not save the download: %v", err)
		}
		binPath = dst
		log("Saved the Linux build to " + dst + " for your Linux server.")
		step(1)
	} else {
		// Build natively in the checkout: setup.sh is Linux
		// only, so the build runs here directly.
		if err := runLocalBuild(ctx, a, line); err != nil {
			return err
		}
		binPath = filepath.Join(a.sourceDir, "bin", exeName())
		step(1)
	}
	if err := writeLocalEnv(a, binPath); err != nil {
		return fmt.Errorf("could not write the settings: %v", err)
	}
	step(2)
	return nil
}

// runLocalBuild builds the checkout natively: assets, then the
// server. It streams the build's output through line.
func runLocalBuild(ctx context.Context, a *answers, line func(string)) error {
	for _, args := range [][]string{{"make", "assets"}, {"make", "build"}} {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = a.sourceDir
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("could not start %s: %v", strings.Join(args, " "), err)
		}
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line(scanner.Text())
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("the build stopped at %s: %v", strings.Join(args, " "), err)
		}
	}
	return nil
}

// writeLocalEnv writes the collected settings into a .env
// beside the binary, appending only keys that are not already
// there, the same rule setup.sh follows.
func writeLocalEnv(a *answers, binPath string) error {
	dir := filepath.Dir(binPath)
	envPath := filepath.Join(dir, ".env")
	existing := map[string]bool{}
	if data, err := os.ReadFile(envPath); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if k, _, ok := strings.Cut(l, "="); ok {
				existing[strings.TrimSpace(k)] = true
			}
		}
	}
	f, err := os.OpenFile(envPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	write := func(key, value, comment string) {
		if value == "" || existing[key] {
			return
		}
		fmt.Fprintf(f, "\n# %s\n%s=%s\n", comment, key, value)
		existing[key] = true
	}
	if !a.dbLocal {
		write("DATABASE_URL", a.databaseURL, "Postgres connection (given to the installer).")
	}
	write("EVE_CLIENT_ID", a.clientID, "EVE Online application client ID (given to the installer).")
	write("EVE_CLIENT_SECRET", a.clientSecret, "EVE Online application client secret (given to the installer).")
	write("ESI_CONTACT", a.contact, "Contact email CCP sees for the EVE application (given to the installer).")
	if !existing["TOKEN_ENCRYPTION_KEY"] {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err == nil {
			fmt.Fprintf(f, "\n# Encrypts the EVE login tokens stored in the database (generated by the installer).\nTOKEN_ENCRYPTION_KEY=%s\n", hex.EncodeToString(key))
		}
	}
	return nil
}

// copyFile copies src to dst with the given mode.
func copyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, perm)
}

// exeName is the binary's file name on this platform.
func exeName() string {
	if runtime.GOOS == "windows" {
		return "evesynapse.exe"
	}
	return "evesynapse"
}
