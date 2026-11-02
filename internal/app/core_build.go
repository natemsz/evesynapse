package app

// ---------------------------------------------------------------------------
// The running build's identity: which source commit this binary
// was built from, and the SHA-256 of the binary itself. The page
// footer shows both, so anyone can check that the build serving
// them is the build the signed release published: the release's
// signed manifests (latest-<arch>.json) carry the same SHA-256
// for the raw binary, and the commit names the exact source in
// the public repo.
//
// Both halves are read once, at startup, and served from memory
// after that; nothing is read per request. Either half is empty
// when it cannot be known (a build with no VCS stamp, an
// executable that cannot be read back), and the footer then
// leaves that half out. A missing identity is never an error.
// ---------------------------------------------------------------------------

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime/debug"
)

// buildIdentity is the running build's commit and binary hash;
// either field is empty when that half is unknown.
type buildIdentity struct {
	commit string // the full vcs.revision the build was stamped with
	hash   string // the hex SHA-256 of the running executable
}

// runningBuild is this process's identity, read once when the
// package loads. Tests replace it to pin what the footer shows.
var runningBuild = readBuildIdentity()

// readBuildIdentity reads the identity from the two places that
// know it: the build info the toolchain stamps into the binary,
// and the executable file itself.
func readBuildIdentity() buildIdentity {
	var id buildIdentity
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				id.commit = setting.Value
				break
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if f, err := os.Open(exe); err == nil {
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				id.hash = hex.EncodeToString(h.Sum(nil))
			}
			f.Close()
		}
	}
	return id
}

// shortCommit is the commit's display form: its first eight
// characters, the short form the repo hosts use. A shorter
// stamp is shown whole.
func shortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

// shortHash is the binary hash's display form: its first twelve
// characters and an ellipsis. The full hash rides in the footer
// line's title attribute.
func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12] + "…"
	}
	return hash
}
