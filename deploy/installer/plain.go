// Plain interface: the same prompts, review and install as the
// TUI, as plain questions on stdin/stdout, for when stdout is
// not a terminal (a pipe, a script, or ssh without a tty).
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ask reads one line from the reader, returning it trimmed.
func ask(r *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt + " ")
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// askChoice shows numbered options and returns the chosen index,
// or -1 on a blank answer (keep the default).
func askChoice(r *bufio.Reader, title string, options []string, def int) (int, error) {
	fmt.Println(title)
	for i, o := range options {
		mark := " "
		if i == def {
			mark = "*"
		}
		fmt.Printf("  %s %d) %s\n", mark, i+1, o)
	}
	for {
		line, err := ask(r, "Choose a number ["+fmt.Sprint(def+1)+"]:")
		if err != nil {
			return 0, err
		}
		if line == "" {
			return def, nil
		}
		n := 0
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Println("Please type a number from 1 to", len(options))
	}
}

// askYesNo asks a yes/no question, defaulting to def on a blank
// answer.
func askYesNo(r *bufio.Reader, prompt string, def bool) (bool, error) {
	suffix := " [y/N]: "
	if def {
		suffix = " [Y/n]: "
	}
	line, err := ask(r, prompt+suffix)
	if err != nil {
		return false, err
	}
	if line == "" {
		return def, nil
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return def, nil
}

// plainRun runs the prompts and the install as plain text.
func plainRun(a *answers, dryRun bool) {
	r := bufio.NewReader(os.Stdin)
	fmt.Println("EveSynapse installer")
	fmt.Println(tagline)
	fmt.Println(creditsLine)
	fmt.Println()
	fmt.Println("Detected on this machine:")
	for _, d := range a.detectedLines() {
		fmt.Printf("  %s: %s\n", d[0], d[1])
	}
	fmt.Println()

	if err := plainPrompts(a, r, dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "installer:", err)
		os.Exit(1)
	}

	// Review.
	fmt.Println("\nHere is what the install will use:")
	for _, row := range a.reviewRows(false) {
		fmt.Printf("  %-30s %s (%s)\n", row.setting, row.value, row.source.String())
	}
	blockers, warnings := a.reviewProblems()
	for _, w := range warnings {
		fmt.Println("  Note: " + w)
	}
	fmt.Println()
	if len(blockers) > 0 {
		for _, p := range blockers {
			fmt.Fprintln(os.Stderr, "installer: "+p)
		}
		if !dryRun {
			os.Exit(1)
		}
	}
	if dryRun {
		fmt.Println("Dry run: nothing will be installed.")
		return
	}
	ok, err := askYesNo(r, "Go ahead and install?", true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "installer:", err)
		os.Exit(1)
	}
	if !ok {
		fmt.Println("Stopped. Nothing was installed.")
		return
	}

	// Install, with a text progress line per step.
	steps := installStepsFor(a)
	cb := installCallbacks{
		Step: func(done int) {
			fmt.Printf("[%d/%d] %s\n", done, len(steps), stepName(steps, done-1))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	out, err := runInstall(ctx, a, cb)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if out != "" {
			fmt.Fprintln(os.Stderr, "What the installer said:")
			fmt.Fprintln(os.Stderr, lastLines(out, 30))
		}
		os.Exit(1)
	}
	fmt.Println()
	fmt.Println("Done. EveSynapse is at " + a.siteURL() + ".")
	plainDoneNotes(a)
}

// stepName returns the name of step i, or "" outside the list.
func stepName(steps []installStep, i int) string {
	if i < 0 || i >= len(steps) {
		return ""
	}
	return steps[i].name
}

// plainDoneNotes prints the next steps after a plain install.
func plainDoneNotes(a *answers) {
	if isLinux() {
		fmt.Println("  Settings: /opt/evesynapse/.env")
		fmt.Println("  Check the service: sudo systemctl status evesynapse")
	} else {
		fmt.Println("  The binary is at " + a.installDir + string(os.PathSeparator) + exeName())
		fmt.Println("  Settings: " + a.installDir + string(os.PathSeparator) + ".env")
		fmt.Println("  Run it with: " + a.installDir + string(os.PathSeparator) + exeName())
		fmt.Println("  " + linuxNote("The service and local database setup"))
	}
	fmt.Println("  Your reverse proxy is yours to arrange: point Ferron, Caddy,")
	fmt.Println("  nginx, or a Cloudflare Tunnel at " + strings.TrimPrefix(a.siteURL(), "http://") + " to reach it")
	fmt.Println("  from the internet.")
	if a.clientID == "" || a.clientSecret == "" {
		fmt.Println("  Sign-in stays off until you add your EVE client ID and secret")
		if isLinux() {
			fmt.Println("  to /opt/evesynapse/.env and restart the service.")
		} else {
			fmt.Println("  to the .env beside the binary and restart it.")
		}
	}
	if a.startWarning != "" {
		fmt.Println("  Warning: " + a.startWarning)
	}
	fmt.Println()
	fmt.Println(creditsLine)
}

// plainPrompts asks every question the TUI asks, in the same
// order, skipping anything already given.
func plainPrompts(a *answers, r *bufio.Reader, dryRun bool) error {
	// Source.
	if !a.given["source"] {
		def := 1 // download, when no checkout is here
		if detectSourceDir() != "" {
			def = 0 // build
		}
		choice, err := askChoice(r, "How should EveSynapse be installed?",
			[]string{
				"Build from a checkout on this machine",
				"Download the latest release",
			}, def)
		if err != nil {
			return err
		}
		if choice == 0 {
			a.source = sourceBuild
		} else {
			a.source = sourceDownload
		}
	}

	// Source folder, when building.
	if a.source == sourceBuild && !(a.given["sourceDir"] && validateSourceDir(a.sourceDir) == nil) {
		detected := detectSourceDir()
		prefill := a.sourceDir
		if prefill == "" {
			prefill = detected
		}
		for {
			if detected != "" {
				fmt.Println("Found a checkout at " + detected + "; change it below if that is not the right one.")
			} else {
				fmt.Println("No checkout found in the usual places (this directory, ~/evesynapse).")
			}
			line, err := ask(r, "Folder holding the EveSynapse checkout ["+prefill+"]:")
			if err != nil {
				return err
			}
			if line == "" {
				line = prefill
			}
			if err := validateSourceDir(expandPath(line)); err != nil {
				fmt.Println(err.Error())
				continue
			}
			a.sourceDir = expandPath(line)
			a.orgSourceDir = orgNew
			break
		}
	}

	// Download the release, when that is the source.
	if a.source == sourceDownload && a.release == nil {
		fmt.Println("Downloading the latest release...")
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		rel, err := fetchLatestRelease(ctx, targetArch(), func(att providerAttempt) {
			if att.Err != nil {
				fmt.Printf("  %s: could not be reached (%v)\n", providerDisplayName(att.Provider), att.Err)
			} else {
				fmt.Printf("  %s: release v%s\n", providerDisplayName(att.Provider), att.Version)
			}
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "The latest release could not be fetched:", err)
			fmt.Fprintln(os.Stderr, "Download it by hand from one of these pages:")
			for _, p := range providerDownloadPages {
				fmt.Fprintln(os.Stderr, "  "+p)
			}
			return err
		}
		if dryRun {
			a.release = rel
			fmt.Println("  Release v" + rel.Version + " from " + providerDisplayName(rel.Provider) + ".")
		} else {
			fmt.Println("  Release v" + rel.Version + " from " + providerDisplayName(rel.Provider) + ".")
			fmt.Println("  Downloading the binary...")
			tmpDir, err := os.MkdirTemp("", "evesynapse-release-*")
			if err != nil {
				return err
			}
			dest := tmpDir + string(os.PathSeparator) + "evesynapse-" + targetArch()
			err = downloadBinary(ctx, rel, dest, func(p downloadProgress) {
				if p.Total > 0 {
					fmt.Printf("\r  %d%%", 100*p.Done/p.Total)
				}
			})
			fmt.Println()
			if err != nil {
				os.RemoveAll(tmpDir)
				return err
			}
			a.release = rel
			a.downloadedBinary = dest
			a.downloadDir = tmpDir
		}
	}

	// Database.
	if isLinux() {
		if !a.given["databaseURL"] && a.databaseURL != "present" {
			def := 0
			if !a.dbLocal {
				def = 1
			}
			choice, err := askChoice(r, "Where should EveSynapse keep its data?",
				[]string{
					"Set up PostgreSQL on this machine (recommended)",
					"Use a PostgreSQL database I already have",
				}, def)
			if err != nil {
				return err
			}
			a.dbLocal = choice == 0
			if a.dbLocal {
				a.databaseURL = ""
			}
		}
	}
	if !a.dbLocal && a.databaseURL != "present" && !a.given["databaseURL"] {
		if !isLinux() {
			fmt.Println("Local provisioning needs a Linux machine, so give the")
			fmt.Println("DATABASE_URL of a PostgreSQL you already have, or leave")
			fmt.Println("it blank to add it later (the app will not start without one).")
		}
		for {
			line, err := ask(r, "Connection string for your database (postgres://...):")
			if err != nil {
				return err
			}
			if line == "" && isLinux() {
				fmt.Println("A connection string is needed for a database you already have.")
				continue
			}
			if line != "" && !strings.HasPrefix(line, "postgres://") && !strings.HasPrefix(line, "postgresql://") {
				fmt.Println("That does not look like a connection string. It should start with postgres://")
				continue
			}
			if line != "" {
				a.databaseURL = line
				a.orgDB = orgNew
			}
			break
		}
	}

	// EVE client ID and secret.
	if !a.given["clientID"] && a.existingEnv["EVE_CLIENT_ID"] == "" {
		fmt.Println("Your EVE Online application client ID (from developers.eveonline.com).")
		fmt.Println("Leave it blank to add it later; sign-in stays off until then.")
		line, err := ask(r, "EVE client ID:")
		if err != nil {
			return err
		}
		a.clientID = line
		if line == "" {
			a.orgClientID = orgSkipped
		} else {
			a.orgClientID = orgNew
		}
	}
	if !a.given["clientSecret"] && a.existingEnv["EVE_CLIENT_SECRET"] == "" {
		fmt.Println("Your EVE Online application client secret (same application page).")
		fmt.Println("Leave it blank to add it later; sign-in stays off until then.")
		line, err := ask(r, "EVE client secret:")
		if err != nil {
			return err
		}
		a.clientSecret = line
		if line == "" {
			a.orgClientSecret = orgSkipped
		} else {
			a.orgClientSecret = orgNew
		}
	}

	// Callback address.
	if !a.given["callback"] && a.existingEnv["EVE_CALLBACK_URL"] == "" {
		s, _ := settingFor("EVE_CALLBACK_URL")
		fmt.Println("The address CCP sends the browser back to after login.")
		fmt.Println("It must match your EVE application exactly.")
		for {
			line, err := ask(r, "Callback URL ["+s.Default+"]:")
			if err != nil {
				return err
			}
			if line == "" {
				line = s.Default
			}
			if err := s.validate(line); err != nil {
				fmt.Println(err.Error())
				continue
			}
			a.callback = line
			a.orgCallback = orgNew
			break
		}
	}

	// ESI contact.
	if !a.given["contact"] && a.existingEnv["ESI_CONTACT"] == "" {
		fmt.Println("What email should CCP see as the contact for your EVE application?")
		fmt.Println("Leave it blank to skip it.")
		line, err := ask(r, "Contact email:")
		if err != nil {
			return err
		}
		a.contact = line
		if line == "" {
			a.orgContact = orgSkipped
		} else {
			a.orgContact = orgNew
		}
	}
	return nil
}
