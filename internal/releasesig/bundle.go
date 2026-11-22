package releasesig

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
)

// BundleName is the release file holding every platform's checksum
// and its signature, in place of a manifest and .sig per platform.
const BundleName = "latest.json"

// BundleManifest is what a bundle signs: the version and the SHA-256
// of the build for each platform key (arm64, windows-amd64, ...).
type BundleManifest struct {
	Version string            `json:"version"`
	SHA256  map[string]string `json:"sha256"`
}

// bundleFile is latest.json: the exact signed bytes, base64 in JSON,
// and one signature per signing key.
type bundleFile struct {
	Manifest   []byte   `json:"manifest"`
	Signatures []string `json:"signatures"`
}

// NewBundle signs m and returns the contents of latest.json.
func NewBundle(keys []ed25519.PrivateKey, m BundleManifest) ([]byte, error) {
	signed, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(string(Sign(keys, signed)))
	out, err := json.Marshal(bundleFile{Manifest: signed, Signatures: lines})
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// OpenBundle checks a latest.json against the trusted keys and only
// then reads what it signed.
func OpenBundle(trusted []ed25519.PublicKey, raw []byte) (BundleManifest, ed25519.PublicKey, error) {
	var m BundleManifest
	var file bundleFile
	if err := json.Unmarshal(raw, &file); err != nil || len(file.Manifest) == 0 {
		return m, nil, ErrNoSignature
	}
	key, err := VerifyingKey(trusted, file.Manifest, []byte(strings.Join(file.Signatures, "\n")))
	if err != nil {
		return m, nil, err
	}
	if err := json.Unmarshal(file.Manifest, &m); err != nil || m.Version == "" || len(m.SHA256) == 0 {
		return BundleManifest{}, nil, errors.New("the signed release information can't be read")
	}
	return m, key, nil
}
