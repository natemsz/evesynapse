// Source tree detection: where the EveSynapse checkout is, for
// a build from source. The installer looks in the current
// directory first, then the usual checkout location, and shows
// what it found (or that it found nothing) so the operator can
// accept it or type a different folder.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sourceCandidates are the places a checkout is looked for, in
// order: the directory the installer runs from, then the usual
// home checkout.
func sourceCandidates() []string {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var out []string
	for _, d := range []string{cwd, filepath.Join(home, "evesynapse")} {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// detectSourceDir finds a checkout holding the repo, or "" when
// none of the usual places holds one.
func detectSourceDir() string {
	for _, d := range sourceCandidates() {
		if err := validateSourceDir(d); err == nil {
			return d
		}
	}
	return ""
}

// validateSourceDir checks that dir holds an EveSynapse source
// tree: the repo's go.mod and the setup script the build runs
// through. A bad path gets a plain error naming what is missing.
func validateSourceDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("enter the folder the EveSynapse checkout is in")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("no such folder: %s", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("%s does not look like the EveSynapse checkout (no go.mod there)", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "setup.sh")); err != nil {
		return fmt.Errorf("%s does not look like the EveSynapse checkout (no deploy/setup.sh there)", dir)
	}
	return nil
}

// expandPath expands a leading ~ to the home directory.
func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}
