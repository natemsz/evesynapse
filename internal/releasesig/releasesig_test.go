package releasesig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

// newKey makes a key pair for one test. No test uses the real release
// key: its private half exists only as the release job's secret.
func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	return pub, priv
}

var sampleManifest = []byte(`{"version":"1.4.2.003","arch":"arm64","sha256":"eaddc11e08783568d26c385a71843d4dca3a8e2fd9f2511d896539f604454744"}` + "\n")

func TestSignThenVerify(t *testing.T) {
	pub, priv := newKey(t)
	sig := Sign([]ed25519.PrivateKey{priv}, sampleManifest)
	if err := Verify([]ed25519.PublicKey{pub}, sampleManifest, sig); err != nil {
		t.Fatalf("a manifest signed with the trusted key was refused: %v", err)
	}
	if len(sig) > MaxSigBytes {
		t.Fatalf("a one-key signature file is %d bytes, more than the %d the updater downloads", len(sig), MaxSigBytes)
	}
	if !bytes.HasSuffix(sig, []byte("\n")) || bytes.Count(sig, []byte("\n")) != 1 {
		t.Fatalf("signature file %q, want exactly one line", sig)
	}
}

func TestVerifyRefuses(t *testing.T) {
	pub, priv := newKey(t)
	_, otherPriv := newKey(t)
	trusted := []ed25519.PublicKey{pub}
	good := Sign([]ed25519.PrivateKey{priv}, sampleManifest)

	// Flip one bit of the signature itself.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(good)))
	if err != nil {
		t.Fatal(err)
	}
	raw[10] ^= 0x01
	flipped := []byte(base64.StdEncoding.EncodeToString(raw) + "\n")

	// The same manifest saying something else: a newer version, a
	// different build, another kind of computer, one more byte.
	newerVersion := bytes.Replace(sampleManifest, []byte("1.4.2.003"), []byte("9.9.9.999"), 1)
	otherBuild := bytes.Replace(sampleManifest, []byte("eaddc11e"), []byte("00000000"), 1)
	otherArch := bytes.Replace(sampleManifest, []byte("arm64"), []byte("amd64"), 1)

	cases := []struct {
		name    string
		message []byte
		sig     []byte
		want    error
	}{
		{"signed by a key that is not trusted", sampleManifest, Sign([]ed25519.PrivateKey{otherPriv}, sampleManifest), ErrBadSignature},
		{"version changed after signing", newerVersion, good, ErrBadSignature},
		{"checksum changed after signing", otherBuild, good, ErrBadSignature},
		{"kind of computer changed after signing", otherArch, good, ErrBadSignature},
		{"a byte added after signing", append(bytes.Clone(sampleManifest), ' '), good, ErrBadSignature},
		{"final newline dropped after signing", bytes.TrimRight(sampleManifest, "\n"), good, ErrBadSignature},
		{"a bit flipped in the signature", sampleManifest, flipped, ErrBadSignature},
		{"empty signature file", sampleManifest, nil, ErrNoSignature},
		{"blank lines only", sampleManifest, []byte("\n\n  \n"), ErrNoSignature},
		{"not base64", sampleManifest, []byte("this is not a signature\n"), ErrNoSignature},
		{"an error page", sampleManifest, []byte("<!DOCTYPE html><html>404</html>"), ErrNoSignature},
		{"base64 of the wrong length", sampleManifest, []byte(base64.StdEncoding.EncodeToString(raw[:40]) + "\n"), ErrNoSignature},
		{"signature cut short", sampleManifest, good[:len(good)/2], ErrNoSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(trusted, tc.message, tc.sig); !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestVerifyAcceptsOneGoodSignatureAmongOthers: while a key is being
// replaced a release carries a signature from the old key and one from
// the new, and an updater that trusts either installs it.
func TestVerifyAcceptsOneGoodSignatureAmongOthers(t *testing.T) {
	oldPub, oldPriv := newKey(t)
	newPub, newPriv := newKey(t)
	both := Sign([]ed25519.PrivateKey{oldPriv, newPriv}, sampleManifest)
	if bytes.Count(both, []byte("\n")) != 2 {
		t.Fatalf("signing with two keys wrote %q, want two lines", both)
	}
	for name, trusted := range map[string][]ed25519.PublicKey{
		"an updater that knows only the old key": {oldPub},
		"an updater that knows only the new key": {newPub},
		"an updater that knows both":             {oldPub, newPub},
	} {
		if err := Verify(trusted, sampleManifest, both); err != nil {
			t.Errorf("%s refused a release signed with both: %v", name, err)
		}
	}

	// Once the old key has been retired a release carries the new
	// key's signature alone, and an updater that still lists the old
	// key first finds the new one further down its list.
	newOnly := Sign([]ed25519.PrivateKey{newPriv}, sampleManifest)
	if err := Verify([]ed25519.PublicKey{oldPub, newPub}, sampleManifest, newOnly); err != nil {
		t.Errorf("a release signed with the second trusted key was refused: %v", err)
	}
	if err := Verify([]ed25519.PublicKey{oldPub}, sampleManifest, newOnly); !errors.Is(err, ErrBadSignature) {
		t.Errorf("an updater that knows only the old key: Verify = %v for a release signed with the new one, want %v", err, ErrBadSignature)
	}

	// Lines this version cannot read are passed over, and Windows line
	// endings and stray spaces around a signature do not matter.
	good := strings.TrimSpace(string(Sign([]ed25519.PrivateKey{newPriv}, sampleManifest)))
	padded := []byte("v2:some-later-kind-of-signature\r\n\r\n  " + good + "  \r\n")
	if err := Verify([]ed25519.PublicKey{newPub}, sampleManifest, padded); err != nil {
		t.Fatalf("a good signature among unreadable lines was refused: %v", err)
	}
}

func TestVerifyWithoutTrustedKeys(t *testing.T) {
	_, priv := newKey(t)
	sig := Sign([]ed25519.PrivateKey{priv}, sampleManifest)
	if err := Verify(nil, sampleManifest, sig); !errors.Is(err, ErrNoTrustedKeys) {
		t.Fatalf("Verify with no trusted keys = %v, want %v", err, ErrNoTrustedKeys)
	}
	// A key of the wrong size is no key: it is passed over, not handed
	// to the verifier (which would panic).
	if err := Verify([]ed25519.PublicKey{{1, 2, 3}}, sampleManifest, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify with a malformed trusted key = %v, want %v", err, ErrBadSignature)
	}
}

// TestOpenSSLSignatureVerifies pins the claim that the format is the
// standard one: this key and signature were made with OpenSSL
// (genpkey -algorithm ed25519, then pkeyutl -sign -rawin) and nothing
// of this package. The private half was discarded.
func TestOpenSSLSignatureVerifies(t *testing.T) {
	const pubPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAO+cM1+0iUrSKEj2aTAfx1N03pAlxP+url655YT687iQ=
-----END PUBLIC KEY-----
`
	const sig = "/fwQC7njMyoviMLy3izKvgRBkznOHGrwg5TOIspByrZg50MSrLlgs+z6u78oeCi136V3PyK69i1tr9r7YDISBQ==\n"
	keys, err := ParsePublicKeys([]byte(pubPEM))
	if err != nil || len(keys) != 1 {
		t.Fatalf("ParsePublicKeys = %d keys, %v; want the one OpenSSL wrote", len(keys), err)
	}
	if err := Verify(keys, sampleManifest, []byte(sig)); err != nil {
		t.Fatalf("a signature made with OpenSSL was refused: %v", err)
	}
	if got := KeyLine(keys[0]); !strings.Contains(pubPEM, got) {
		t.Fatalf("KeyLine = %q, which is not the line in the PEM block", got)
	}
}

func encodePub(t *testing.T, key ed25519.PublicKey) string {
	t.Helper()
	b, err := EncodePublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func encodePriv(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	b, err := EncodePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ecdsaKeyPEMs returns a public and a private key of another kind, in
// the same PEM containers the Ed25519 ones use.
func ecdsaKeyPEMs(t *testing.T) (pubPEM, privPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
}

func TestParsePublicKeys(t *testing.T) {
	pubA, privA := newKey(t)
	pubB, _ := newKey(t)
	a, b := encodePub(t, pubA), encodePub(t, pubB)
	ecPub, _ := ecdsaKeyPEMs(t)

	keys, err := ParsePublicKeys([]byte("Notes about the keys.\n\n" + a + "\nThe next one:\n" + b + "trailing notes\n"))
	if err != nil {
		t.Fatalf("two keys with commentary around them: %v", err)
	}
	if len(keys) != 2 || !keys[0].Equal(pubA) || !keys[1].Equal(pubB) {
		t.Fatalf("got %d keys, want the two that were written, in order", len(keys))
	}

	// Windows line endings, and no newline after the last line.
	keys, err = ParsePublicKeys([]byte(strings.TrimRight(strings.ReplaceAll(a, "\n", "\r\n"), "\r\n")))
	if err != nil || len(keys) != 1 || !keys[0].Equal(pubA) {
		t.Fatalf("CRLF key without a final newline: %d keys, %v", len(keys), err)
	}

	// No blocks is no keys, not an error: that is what the file holds
	// before the first key is made.
	keys, err = ParsePublicKeys([]byte("only commentary so far\n"))
	if err != nil || len(keys) != 0 {
		t.Fatalf("a file of commentary: %d keys, %v; want none and no error", len(keys), err)
	}

	damaged := strings.Replace(a, "MCow", "M!ow", 1)
	for name, in := range map[string]string{
		"a private key":           encodePriv(t, privA),
		"a key of another kind":   ecPub,
		"a certificate block":     "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"a block that is damaged": damaged,
		"a good key and a damaged one, which must not become one key": b + damaged,
		"a block that is not a key at all":                            "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n",
	} {
		if keys, err := ParsePublicKeys([]byte(in)); err == nil {
			t.Errorf("%s: accepted as %d key(s), want an error", name, len(keys))
		}
	}
	if _, err := ParsePublicKeys([]byte(encodePriv(t, privA))); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("a private key among the trusted keys: error %v, want one that says it is a private key", err)
	}
}

func TestParsePrivateKeys(t *testing.T) {
	pubA, privA := newKey(t)
	_, privB := newKey(t)
	a, b := encodePriv(t, privA), encodePriv(t, privB)
	_, ecPriv := ecdsaKeyPEMs(t)

	for name, in := range map[string]string{
		"as written":                 a,
		"Windows line endings":       strings.ReplaceAll(a, "\n", "\r\n"),
		"no final newline":           strings.TrimRight(a, "\n"),
		"blank lines around the key": "\n\n" + a + "\n\n",
	} {
		keys, err := ParsePrivateKeys([]byte(in))
		if err != nil || len(keys) != 1 || !keys[0].Equal(privA) {
			t.Errorf("%s: %d keys, %v; want the one key", name, len(keys), err)
			continue
		}
		// The parsed key is the same key: what it signs, the public
		// half verifies.
		if err := Verify([]ed25519.PublicKey{pubA}, sampleManifest, Sign(keys, sampleManifest)); err != nil {
			t.Errorf("%s: a signature by the parsed key was refused: %v", name, err)
		}
	}

	keys, err := ParsePrivateKeys([]byte(a + b))
	if err != nil || len(keys) != 2 || !keys[0].Equal(privA) || !keys[1].Equal(privB) {
		t.Fatalf("two keys: %d keys, %v; want both, in order", len(keys), err)
	}

	// A body that is valid base64 but no key, carrying a marker: no
	// error may repeat what it was given, since what it is given is
	// the signing key.
	const marker = "TOPSECRETKEYMATERIAL"
	notAKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte(strings.Repeat(marker, 3))}))
	damaged := strings.Replace(a, "MC", "!!", 1)
	for name, in := range map[string]string{
		"nothing":                              "",
		"text that is not PEM":                 marker + " just some text",
		"a public key":                         encodePub(t, pubA),
		"a key of another kind":                ecPriv,
		"a block that is not a key":            notAKey,
		"a block that is damaged":              damaged,
		"a good key followed by a damaged one": b + damaged,
	} {
		keys, err := ParsePrivateKeys([]byte(in))
		if err == nil {
			t.Errorf("%s: accepted as %d key(s), want an error", name, len(keys))
			continue
		}
		body := base64.StdEncoding.EncodeToString([]byte(marker))
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), body[:12]) {
			t.Errorf("%s: the error repeats its input: %v", name, err)
		}
	}
}

// TestTrustedKeysFile checks the file that is compiled into every
// build: whatever it holds has to be readable Ed25519 public keys,
// each listed once.
func TestTrustedKeysFile(t *testing.T) {
	keys, err := ParsePublicKeys(trustedPEM)
	if err != nil {
		t.Fatalf("trusted_keys.pem can't be read: %v", err)
	}
	for i, key := range keys {
		for j := range i {
			if key.Equal(keys[j]) {
				t.Errorf("trusted_keys.pem lists key %d twice (blocks %d and %d)", i+1, j+1, i+1)
			}
		}
	}
	got, err := Trusted()
	if err != nil || len(got) != len(keys) {
		t.Fatalf("Trusted() = %d keys, %v; want the %d in the file", len(got), err, len(keys))
	}
	// A build without a key refuses every update, and the release job
	// cannot sign for it.
	if len(keys) == 0 {
		t.Fatal("trusted_keys.pem holds no release key. Make one with:\n    go run ./cmd/releasesign keygen -- gh secret set RELEASE_SIGNING_KEY\nand commit the change it makes to this file.")
	}
	// Trusted hands out a copy: a caller cannot change what the next
	// caller is given.
	got[0] = nil
	if again, _ := Trusted(); len(again) == 0 || again[0] == nil {
		t.Fatal("changing the slice Trusted() returned changed the trusted keys")
	}
}
