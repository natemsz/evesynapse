// Package releasesig is the signature on a release: how the release
// job signs the update manifests it publishes, and how the updater
// checks one before acting on it.
//
// A manifest (latest-<arch>.json) names a version, the kind of
// computer it is for, and the SHA-256 of the build published beside
// it. The release job signs each manifest's exact bytes with Ed25519
// and publishes the result as latest-<arch>.json.sig. The updater
// carries the public half of the key (trusted_keys.pem, compiled in)
// and installs nothing from the release channel unless that signature
// verifies. Because the manifest is what is signed, the signature
// covers the version, the kind of computer and, through the checksum,
// every byte of the build.
//
// What that proves: the release was published by someone holding the
// signing key, which in practice means by this repository's release
// job. Being able to replace the files of a release is no longer
// enough to get a build installed. It is no protection against
// someone who controls the repository itself, since they control
// what the release job signs.
//
// Keys are ordinary PEM (PKCS #8 private, PKIX public) and the
// signatures plain Ed25519, so a release can be checked with OpenSSL
// alone, without this program:
//
//	base64 -d latest-arm64.json.sig > sig.bin
//	openssl pkeyutl -verify -pubin -inkey trusted_keys.pem -rawin \
//	    -in latest-arm64.json -sigfile sig.bin
package releasesig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

const (
	// SigSuffix is what a manifest's file name gains to name its
	// signature: latest-arm64.json is signed by latest-arm64.json.sig.
	SigSuffix = ".sig"

	// MaxManifestBytes and MaxSigBytes bound what the updater will
	// download for each. The release job refuses to sign anything
	// larger, so it cannot publish what no updater would accept.
	MaxManifestBytes = 64 << 10
	MaxSigBytes      = 4 << 10
)

var (
	// ErrNoTrustedKeys: there is nothing to check a signature against.
	ErrNoTrustedKeys = errors.New("no trusted release key to check the signature against")
	// ErrNoSignature: the signature file holds no signature this
	// version can read.
	ErrNoSignature = errors.New("the signature file holds no signature")
	// ErrBadSignature: there is a signature, and no trusted key made it
	// over this message.
	ErrBadSignature = errors.New("the signature was not made by a trusted release key over this content")
)

// Sign returns the contents of the signature file for message: one
// line per key, each the base64 of that key's Ed25519 signature over
// the message's exact bytes.
func Sign(keys []ed25519.PrivateKey, message []byte) []byte {
	var out bytes.Buffer
	for _, key := range keys {
		out.WriteString(base64.StdEncoding.EncodeToString(ed25519.Sign(key, message)))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// Verify checks that sigFile holds a signature over message's exact
// bytes made by one of the trusted keys, and says why not otherwise.
//
// One good signature is enough. While a key is being replaced a
// release is signed with the old key and the new one, a line each,
// and an updater that knows either accepts it. A line this version
// cannot read is passed over rather than treated as a failure, so a
// later kind of signature can be published beside this one.
func Verify(trusted []ed25519.PublicKey, message, sigFile []byte) error {
	if len(trusted) == 0 {
		return ErrNoTrustedKeys
	}
	signatures := 0
	for _, line := range bytes.Split(sigFile, []byte("\n")) {
		sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(line)))
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		signatures++
		for _, key := range trusted {
			if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, message, sig) {
				return nil
			}
		}
	}
	if signatures == 0 {
		return ErrNoSignature
	}
	return ErrBadSignature
}

//go:embed trusted_keys.pem
var trustedPEM []byte

var trustedOnce = sync.OnceValues(func() ([]ed25519.PublicKey, error) {
	return ParsePublicKeys(trustedPEM)
})

// Trusted returns the release keys compiled into this build: the
// public keys in trusted_keys.pem. A build made before a key was
// added has none, and its updater then refuses every release.
func Trusted() ([]ed25519.PublicKey, error) {
	keys, err := trustedOnce()
	return slices.Clone(keys), err
}

// ParsePublicKeys reads the public keys in a trusted-keys file: PEM
// "PUBLIC KEY" blocks holding Ed25519 keys. Text outside the blocks
// is commentary. A file with no blocks holds no keys, which is not an
// error; anything in it that is not a readable Ed25519 public key is.
func ParsePublicKeys(pemData []byte) ([]ed25519.PublicKey, error) {
	blocks, err := pemBlocks(pemData)
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for i, block := range blocks {
		if strings.Contains(block.Type, "PRIVATE") {
			return nil, fmt.Errorf("block %d is a private key: only public keys belong here, and a private key that was put here has to be treated as leaked and replaced", i+1)
		}
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("block %d is a %q, not a PUBLIC KEY", i+1, block.Type)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("key %d can't be read: %v", i+1, err)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("key %d is not an Ed25519 key", i+1)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// ParsePrivateKeys reads the signing key(s) the release job is given:
// PEM "PRIVATE KEY" blocks (PKCS #8) holding Ed25519 keys, at least
// one. Its errors never repeat any part of the input.
func ParsePrivateKeys(pemData []byte) ([]ed25519.PrivateKey, error) {
	blocks, err := pemBlocks(pemData)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, errors.New("there is no PEM block in it (a key starts with -----BEGIN PRIVATE KEY-----)")
	}
	var keys []ed25519.PrivateKey
	for i, block := range blocks {
		if block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("block %d is a %q, not a PRIVATE KEY", i+1, block.Type)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("key %d can't be read", i+1)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("key %d is not an Ed25519 key", i+1)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// pemBlocks returns every PEM block in data. The standard decoder
// steps over a block it cannot read, which would quietly turn a
// damaged key into one key fewer; counting the BEGIN lines makes that
// an error instead.
func pemBlocks(data []byte) ([]*pem.Block, error) {
	var blocks []*pem.Block
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
	}
	if begins := bytes.Count(data, []byte("-----BEGIN ")); begins != len(blocks) {
		return nil, fmt.Errorf("%d of its %d PEM blocks can't be read", begins-len(blocks), begins)
	}
	return blocks, nil
}

// EncodePublicKey writes a public key as the PEM block the trusted
// keys file holds.
func EncodePublicKey(key ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// EncodePrivateKey writes a signing key as the PEM block the release
// job is given.
func EncodePrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// KeyLine is the one line of base64 inside a public key's PEM block:
// what to look for in the trusted keys file to find that key.
func KeyLine(key ed25519.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "(unreadable key)"
	}
	return base64.StdEncoding.EncodeToString(der)
}
