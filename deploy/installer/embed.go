// Embedded files the installer distributes as one binary.
//
// setup.sh is a byte-identical copy of deploy/setup.sh: the installer
// runs the embedded copy instead of a setup.sh file sitting next to
// it, so the installer ships as a single file. Keep it in sync:
// embed_test.go fails when the copy drifts from deploy/setup.sh,
// and the release workflow checks the hashes match before building.
//
// trusted_keys.pem is a copy of internal/releasesig/trusted_keys.pem
// (public keys only). The installer checks a downloaded release's
// signature against it before installing anything from the release
// channel. The same sync rule applies.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed setup.sh
var embeddedSetup []byte

//go:embed trusted_keys.pem
var embeddedTrustedKeys []byte

// writeEmbeddedSetup writes the embedded setup.sh into dir under
// name and makes it executable, returning the full path.
func writeEmbeddedSetup(dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, embeddedSetup, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// writeFile writes data to dir/name with the given permissions.
func writeFile(dir, name string, data []byte, perm os.FileMode) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, perm); err != nil {
		return "", err
	}
	return p, nil
}

// readTrustedKeys parses the embedded release keys. It returns an
// error naming the problem, never any key material.
func readTrustedKeys() ([][]byte, error) {
	return parsePublicKeys(embeddedTrustedKeys)
}

// setupRunDir prepares a directory the embedded setup.sh can run
// from: the script at dir/deploy/setup.sh, with .env.example and
// the service unit beside it where the caller supplies them, so
// setup.sh finds them locally instead of fetching them from
// GitLab. It returns the script path. setup.sh works out the repo
// root as the script's parent's parent, so dir is that root.
func setupRunDir(dir string, envExample, service []byte) (string, error) {
	deployDir := filepath.Join(dir, "deploy")
	script, err := writeEmbeddedSetup(deployDir, "setup.sh")
	if err != nil {
		return "", err
	}
	if envExample != nil {
		if _, err := writeFile(dir, ".env.example", envExample, 0o644); err != nil {
			return "", err
		}
	}
	if service != nil {
		if _, err := writeFile(deployDir, "evesynapse.service", service, 0o644); err != nil {
			return "", err
		}
	}
	return script, nil
}

var errNoSetup = fmt.Errorf("the embedded setup.sh is missing (this installer was built without it)")
