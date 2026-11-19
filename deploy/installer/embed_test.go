// Tests that the embedded installer files match the repository
// files they are copies of. A copy that drifts silently means
// the installer installs something else than the checked-in
// script, or checks signatures against stale keys.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedSetupMatchesRepo(t *testing.T) {
	repo := filepath.Join("..", "setup.sh")
	want, err := os.ReadFile(repo)
	if err != nil {
		t.Fatalf("read %s: %v", repo, err)
	}
	if !bytes.Equal(embeddedSetup, want) {
		t.Errorf("embedded setup.sh does not match %s; run cp deploy/setup.sh deploy/installer/setup.sh", repo)
	}
}

func TestEmbeddedTrustedKeysMatchRepo(t *testing.T) {
	repo := filepath.Join("..", "..", "internal", "releasesig", "trusted_keys.pem")
	want, err := os.ReadFile(repo)
	if err != nil {
		t.Fatalf("read %s: %v", repo, err)
	}
	if !bytes.Equal(embeddedTrustedKeys, want) {
		t.Errorf("embedded trusted_keys.pem does not match %s; run cp internal/releasesig/trusted_keys.pem deploy/installer/trusted_keys.pem", repo)
	}
}
