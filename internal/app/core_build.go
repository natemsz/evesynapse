package app

// ---------------------------------------------------------------------------
// The running build's identity: the SHA-256 of the binary
// itself. The page footer prints its first nine characters,
// with the full hash in the "view hash" pop-up beside them, so
// anyone can check that the build serving them is the build
// the signed release published: the release's signed manifests
// (latest-<arch>.json) carry the same SHA-256 for the raw
// binary.
//
// The hash is read once, at startup, and served from memory
// after that; nothing is read per request. It is empty when
// the executable cannot be read back, and the footer then
// leaves it out. A missing identity is never an error.
// ---------------------------------------------------------------------------

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// buildIdentity is the running build's identity; its hash is
// empty when it cannot be known.
type buildIdentity struct {
	hash string // the hex SHA-256 of the running executable
}

// hashDisplayLen is how much of the hash the footer line itself
// prints; the full hash rides in the line's "view hash" pop-up.
const hashDisplayLen = 9

// buildHashShort returns the part of a build hash the footer
// line prints: its first hashDisplayLen characters, or the
// whole string when it is shorter than that.
func buildHashShort(hash string) string {
	if len(hash) > hashDisplayLen {
		return hash[:hashDisplayLen]
	}
	return hash
}

// runningBuild is this process's identity, read once when the
// package loads. Tests replace it to pin what the footer shows.
var runningBuild = readBuildIdentity()

// readBuildIdentity reads the identity from the executable
// file itself.
func readBuildIdentity() buildIdentity {
	var id buildIdentity
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
