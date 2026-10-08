package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"evesynapse/internal/releasesig"
)

// helperEnv turns this test binary into the command keygen hands the
// private key to: "store=<file>" copies its standard input to the
// file, the way a secret store takes a key; "fail" reads it and exits
// with an error, the way one refuses.
const helperEnv = "RELEASESIGN_TEST_HELPER"

func TestMain(m *testing.M) {
	switch mode := os.Getenv(helperEnv); {
	case strings.HasPrefix(mode, "store="):
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(strings.TrimPrefix(mode, "store="), in, 0o600); err != nil {
			os.Exit(4)
		}
		os.Stdout.WriteString("stored\n")
		os.Exit(0)
	case mode == "fail":
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Stderr.WriteString("not signed in\n")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

// testTool is the tool with its output captured and its trusted keys
// read from a file the test owns, so signing can follow a keygen the
// way the release job follows a commit.
type testTool struct {
	tool
	out, err *bytes.Buffer
	pubPath  string
	setenv   func(key, value string)
}

const commentary = "The keys releases are signed with.\n"

func newTestTool(t *testing.T) *testTool {
	t.Helper()
	tt := &testTool{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	tt.pubPath = filepath.Join(t.TempDir(), "trusted_keys.pem")
	if err := os.WriteFile(tt.pubPath, []byte(commentary), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	tt.tool = tool{
		getenv: func(k string) string { return env[k] },
		trusted: func() ([]ed25519.PublicKey, error) {
			data, err := os.ReadFile(tt.pubPath)
			if err != nil {
				return nil, err
			}
			return releasesig.ParsePublicKeys(data)
		},
		stdout: tt.out,
		stderr: tt.err,
	}
	tt.setenv = func(k, v string) { env[k] = v }
	return tt
}

func (tt *testTool) output() string { return tt.out.String() + tt.err.String() }

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func trustedIn(t *testing.T, path string) []ed25519.PublicKey {
	t.Helper()
	keys, err := releasesig.ParsePublicKeys(readFile(t, path))
	if err != nil {
		t.Fatalf("%s can't be read after keygen: %v", path, err)
	}
	return keys
}

// privateIn reads the one private key a file was handed and checks it
// is the other half of pub.
func privateIn(t *testing.T, path string, pub ed25519.PublicKey) ed25519.PrivateKey {
	t.Helper()
	keys, err := releasesig.ParsePrivateKeys(readFile(t, path))
	if err != nil || len(keys) != 1 {
		t.Fatalf("%s holds %d private keys (%v), want the one that was made", path, len(keys), err)
	}
	if !keys[0].Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatalf("the private key in %s is not the other half of the public key that was added", path)
	}
	return keys[0]
}

func TestKeygenHandsThePrivateHalfToTheCommand(t *testing.T) {
	tt := newTestTool(t)
	stored := filepath.Join(t.TempDir(), "secret-store")
	t.Setenv(helperEnv, "store="+stored)

	if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "--", os.Args[0], "secret", "set"}); code != 0 {
		t.Fatalf("code %d, output %q", code, tt.output())
	}
	keys := trustedIn(t, tt.pubPath)
	if len(keys) != 1 {
		t.Fatalf("the trusted keys file holds %d keys after keygen, want 1", len(keys))
	}
	privateIn(t, stored, keys[0])

	// What was in the file is still there, ahead of the new key.
	if got := readFile(t, tt.pubPath); !bytes.HasPrefix(got, []byte(commentary)) {
		t.Fatalf("the trusted keys file lost its commentary: %q", got)
	}
	// The private half went to the command and nowhere else.
	if strings.Contains(tt.output(), "PRIVATE KEY") {
		t.Fatalf("keygen printed the private key: %q", tt.output())
	}
	// The command's own output is passed on, and the summary names
	// the line to look for in the file.
	if !strings.Contains(tt.out.String(), "stored") || !strings.Contains(tt.out.String(), releasesig.KeyLine(keys[0])) {
		t.Fatalf("output %q is missing the command's output or the new key's line", tt.out.String())
	}
}

func TestKeygenWritesACopyToAFile(t *testing.T) {
	tt := newTestTool(t)
	dir := t.TempDir()
	stored, copyPath := filepath.Join(dir, "secret-store"), filepath.Join(dir, "release-key.pem")
	t.Setenv(helperEnv, "store="+stored)

	if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "-out", copyPath, "--", os.Args[0]}); code != 0 {
		t.Fatalf("code %d, output %q", code, tt.output())
	}
	keys := trustedIn(t, tt.pubPath)
	if len(keys) != 1 {
		t.Fatalf("the trusted keys file holds %d keys, want 1", len(keys))
	}
	// The command and the file were given the same key.
	if !bytes.Equal(readFile(t, stored), readFile(t, copyPath)) {
		t.Fatal("the command and the -out file were given different keys")
	}
	privateIn(t, copyPath, keys[0])
	if runtime.GOOS != "windows" {
		info, err := os.Stat(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("the private key file has permissions %v, want 0600", perm)
		}
	}

	// -out alone is enough: a key kept in a file and nowhere else.
	alone := newTestTool(t)
	alonePath := filepath.Join(dir, "another-key.pem")
	if code := alone.run([]string{"keygen", "-pub", alone.pubPath, "-out", alonePath}); code != 0 {
		t.Fatalf("-out alone: code %d, output %q", code, alone.output())
	}
	privateIn(t, alonePath, trustedIn(t, alone.pubPath)[0])
}

// TestKeygenChangesNothingWhenTheKeyIsNotStored: a key whose private
// half reached nobody must not become a trusted key, and no copy of it
// stays behind.
func TestKeygenChangesNothingWhenTheKeyIsNotStored(t *testing.T) {
	tt := newTestTool(t)
	copyPath := filepath.Join(t.TempDir(), "release-key.pem")
	t.Setenv(helperEnv, "fail")

	if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "-out", copyPath, "--", os.Args[0]}); code != 1 {
		t.Fatalf("code %d, want 1; output %q", code, tt.output())
	}
	if got := readFile(t, tt.pubPath); string(got) != commentary {
		t.Fatalf("the trusted keys file changed although the key was not stored: %q", got)
	}
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Fatalf("a copy of the discarded key was left at %s (stat: %v)", copyPath, err)
	}
	if !strings.Contains(tt.err.String(), "not signed in") || !strings.Contains(tt.err.String(), "Nothing was changed") {
		t.Fatalf("stderr %q is missing the command's own error or the nothing-changed note", tt.err.String())
	}

	// A command that does not exist is the same outcome.
	if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "--", filepath.Join(t.TempDir(), "no-such-command")}); code != 1 {
		t.Fatalf("missing command: code %d, want 1", code)
	}
	if got := readFile(t, tt.pubPath); string(got) != commentary {
		t.Fatalf("the trusted keys file changed although the command could not run: %q", got)
	}
}

func TestKeygenRefusals(t *testing.T) {
	dir := t.TempDir()
	stored := filepath.Join(dir, "secret-store")
	t.Setenv(helperEnv, "store="+stored)
	notStored := func(t *testing.T, what string) {
		t.Helper()
		if _, err := os.Stat(stored); !os.IsNotExist(err) {
			t.Fatalf("%s: a key was made and handed over anyway", what)
		}
	}

	t.Run("nowhere to put the private half", func(t *testing.T) {
		tt := newTestTool(t)
		if code := tt.run([]string{"keygen", "-pub", tt.pubPath}); code != 2 {
			t.Fatalf("code %d, want 2", code)
		}
		if strings.Contains(tt.output(), "PRIVATE KEY") {
			t.Fatal("keygen printed a private key for want of anywhere to put it")
		}
		if got := readFile(t, tt.pubPath); string(got) != commentary {
			t.Fatalf("the trusted keys file changed: %q", got)
		}
	})

	t.Run("no trusted keys file", func(t *testing.T) {
		tt := newTestTool(t)
		if code := tt.run([]string{"keygen", "-pub", filepath.Join(dir, "missing.pem"), "--", os.Args[0]}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		notStored(t, "missing trusted keys file")
	})

	t.Run("a trusted keys file that is damaged", func(t *testing.T) {
		tt := newTestTool(t)
		damaged := []byte("-----BEGIN PUBLIC KEY-----\n!!!!\n-----END PUBLIC KEY-----\n")
		if err := os.WriteFile(tt.pubPath, damaged, 0o644); err != nil {
			t.Fatal(err)
		}
		if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "--", os.Args[0]}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		notStored(t, "damaged trusted keys file")
		if got := readFile(t, tt.pubPath); !bytes.Equal(got, damaged) {
			t.Fatal("a damaged trusted keys file was written to")
		}
	})

	t.Run("the -out file already exists", func(t *testing.T) {
		tt := newTestTool(t)
		precious := filepath.Join(dir, "precious")
		if err := os.WriteFile(precious, []byte("an earlier key"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "-out", precious, "--", os.Args[0]}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		if got := readFile(t, precious); string(got) != "an earlier key" {
			t.Fatalf("the existing file was replaced or removed: %q", got)
		}
		notStored(t, "existing -out file")
		if got := readFile(t, tt.pubPath); string(got) != commentary {
			t.Fatalf("the trusted keys file changed: %q", got)
		}
	})

	// Run twice by accident, the second run must not replace the key
	// the first one stored: the secret would then belong to a key
	// that is listed second, and the first would be held by nobody.
	t.Run("a key already exists", func(t *testing.T) {
		tt := newTestTool(t)
		if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "--", os.Args[0]}); code != 0 {
			t.Fatalf("first key: code %d, output %q", code, tt.output())
		}
		first := readFile(t, tt.pubPath)
		firstStored := readFile(t, stored)
		if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "--", os.Args[0]}); code != 1 {
			t.Fatalf("second run: code %d, want 1", code)
		}
		if !bytes.Equal(readFile(t, tt.pubPath), first) || !bytes.Equal(readFile(t, stored), firstStored) {
			t.Fatal("a second keygen replaced the key or the stored secret")
		}
		// Replacing a key is asked for by name, and adds to the list.
		if code := tt.run([]string{"keygen", "-add", "-pub", tt.pubPath, "--", os.Args[0]}); code != 0 {
			t.Fatalf("keygen -add: code %d, output %q", code, tt.output())
		}
		keys := trustedIn(t, tt.pubPath)
		if len(keys) != 2 || keys[0].Equal(keys[1]) {
			t.Fatalf("after keygen -add the file holds %d keys, want two different ones", len(keys))
		}
		if !bytes.HasPrefix(readFile(t, tt.pubPath), first) {
			t.Fatal("keygen -add did not keep the first key ahead of the new one")
		}
		privateIn(t, stored, keys[1])
	})
}

// keyedTool is a test tool that has made its own key: the trusted
// keys file holds the public half, and RELEASE_SIGNING_KEY the private.
func keyedTool(t *testing.T) (*testTool, ed25519.PublicKey) {
	t.Helper()
	tt := newTestTool(t)
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	if code := tt.run([]string{"keygen", "-pub", tt.pubPath, "-out", keyPath}); code != 0 {
		t.Fatalf("keygen: code %d, output %q", code, tt.output())
	}
	tt.setenv(signingKeyEnv, string(readFile(t, keyPath)))
	tt.out.Reset()
	tt.err.Reset()
	return tt, trustedIn(t, tt.pubPath)[0]
}

func writeManifests(t *testing.T) (arm, amd string) {
	t.Helper()
	dir := t.TempDir()
	arm, amd = filepath.Join(dir, "latest-arm64.json"), filepath.Join(dir, "latest-amd64.json")
	for path, arch := range map[string]string{arm: "arm64", amd: "amd64"} {
		body := `{"version":"1.4.2.003","arch":"` + arch + `","sha256":"` + strings.Repeat("ab", 32) + `"}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return arm, amd
}

// TestSignThenVerify is the release job's step followed by the check
// an updater makes: what sign writes beside a manifest verifies
// against the trusted keys, and stops verifying when either changes.
func TestSignThenVerify(t *testing.T) {
	tt, pub := keyedTool(t)
	arm, amd := writeManifests(t)

	if code := tt.run([]string{"sign", arm, amd}); code != 0 {
		t.Fatalf("sign: code %d, output %q", code, tt.output())
	}
	for _, path := range []string{arm, amd} {
		sig := readFile(t, path+releasesig.SigSuffix)
		if err := releasesig.Verify([]ed25519.PublicKey{pub}, readFile(t, path), sig); err != nil {
			t.Fatalf("%s: the signature sign wrote does not verify: %v", filepath.Base(path), err)
		}
	}
	// One manifest's signature is not the other's.
	if err := releasesig.Verify([]ed25519.PublicKey{pub}, readFile(t, arm), readFile(t, amd+releasesig.SigSuffix)); err == nil {
		t.Fatal("the amd64 manifest's signature verified the arm64 manifest")
	}
	if strings.Contains(tt.output(), "PRIVATE KEY") {
		t.Fatalf("sign printed the signing key: %q", tt.output())
	}

	if code := tt.run([]string{"verify", arm, amd}); code != 0 {
		t.Fatalf("verify of freshly signed manifests: code %d, output %q", code, tt.output())
	}
	if err := os.WriteFile(amd, bytes.Replace(readFile(t, amd), []byte("1.4.2.003"), []byte("9.9.9.999"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	tt.out.Reset()
	tt.err.Reset()
	if code := tt.run([]string{"verify", arm, amd}); code != 1 {
		t.Fatalf("verify after a manifest was changed: code %d, want 1", code)
	}
	if !strings.Contains(tt.out.String(), "latest-arm64.json: signed") || !strings.Contains(tt.err.String(), "latest-amd64.json: NOT VERIFIED") {
		t.Fatalf("verify did not tell the good manifest from the changed one: stdout %q, stderr %q", tt.out.String(), tt.err.String())
	}
	if err := os.Remove(arm + releasesig.SigSuffix); err != nil {
		t.Fatal(err)
	}
	if code := tt.run([]string{"verify", arm}); code != 1 {
		t.Fatalf("verify of a manifest with no signature file: code %d, want 1", code)
	}
}

// TestSignLeavesNoTempFiles: the two-phase commit renames every
// temp signature into its .sig name; none stay behind beside the
// manifests.
func TestSignLeavesNoTempFiles(t *testing.T) {
	tt, _ := keyedTool(t)
	arm, _ := writeManifests(t)
	if code := tt.run([]string{"sign", arm}); code != 0 {
		t.Fatalf("sign: code %d, output %q", code, tt.output())
	}
	left, err := filepath.Glob(filepath.Join(filepath.Dir(arm), ".releasesign-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Fatalf("temp signature files left behind: %v", left)
	}
}

// TestSignWithTwoKeys: while a key is being replaced the secret holds
// the old key and the new, and each signs every release.
func TestSignWithTwoKeys(t *testing.T) {
	tt, oldPub := keyedTool(t)
	newKeyPath := filepath.Join(t.TempDir(), "new-key.pem")
	if code := tt.run([]string{"keygen", "-add", "-pub", tt.pubPath, "-out", newKeyPath}); code != 0 {
		t.Fatalf("keygen -add: code %d, output %q", code, tt.output())
	}
	newPub := trustedIn(t, tt.pubPath)[1]
	tt.setenv(signingKeyEnv, tt.getenv(signingKeyEnv)+string(readFile(t, newKeyPath)))

	arm, _ := writeManifests(t)
	if code := tt.run([]string{"sign", arm}); code != 0 {
		t.Fatalf("sign: code %d, output %q", code, tt.output())
	}
	sig := readFile(t, arm+releasesig.SigSuffix)
	if bytes.Count(sig, []byte("\n")) != 2 {
		t.Fatalf("signature file %q, want one line per key", sig)
	}
	for name, pub := range map[string]ed25519.PublicKey{"old": oldPub, "new": newPub} {
		if err := releasesig.Verify([]ed25519.PublicKey{pub}, readFile(t, arm), sig); err != nil {
			t.Errorf("an updater that trusts only the %s key refused the release: %v", name, err)
		}
	}
}

func TestSignRefusals(t *testing.T) {
	noSignatures := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, path := range paths {
			if _, err := os.Stat(path + releasesig.SigSuffix); !os.IsNotExist(err) {
				t.Fatalf("a signature file was written for %s", filepath.Base(path))
			}
		}
	}

	t.Run("no key", func(t *testing.T) {
		tt, _ := keyedTool(t)
		tt.setenv(signingKeyEnv, "  \n")
		arm, amd := writeManifests(t)
		if code := tt.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
		if !strings.Contains(tt.err.String(), signingKeyEnv+" is not set") {
			t.Fatalf("stderr %q does not say the key is missing", tt.err.String())
		}
	})

	t.Run("a key that is not a key", func(t *testing.T) {
		tt, _ := keyedTool(t)
		const marker = "hunter2-not-a-pem-key"
		tt.setenv(signingKeyEnv, marker)
		arm, amd := writeManifests(t)
		if code := tt.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
		if strings.Contains(tt.output(), marker) {
			t.Fatalf("sign repeated the contents of %s: %q", signingKeyEnv, tt.output())
		}
	})

	// The secret and the committed file out of step: a key nobody's
	// updater trusts must not sign a release.
	t.Run("a key this build does not trust", func(t *testing.T) {
		tt, _ := keyedTool(t)
		_, stranger, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		strangerPEM, err := releasesig.EncodePrivateKey(stranger)
		if err != nil {
			t.Fatal(err)
		}
		arm, amd := writeManifests(t)

		tt.setenv(signingKeyEnv, string(strangerPEM))
		if code := tt.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
		if !strings.Contains(tt.err.String(), "not one this build trusts") {
			t.Fatalf("stderr %q does not say the key is untrusted", tt.err.String())
		}

		// A trusted key beside an untrusted one is refused too.
		tt2, _ := keyedTool(t)
		tt2.setenv(signingKeyEnv, tt2.getenv(signingKeyEnv)+string(strangerPEM))
		if code := tt2.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("a trusted key with an untrusted one: code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
	})

	t.Run("no trusted keys at all", func(t *testing.T) {
		tt, _ := keyedTool(t)
		if err := os.WriteFile(tt.pubPath, []byte(commentary), 0o644); err != nil {
			t.Fatal(err)
		}
		arm, amd := writeManifests(t)
		if code := tt.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
	})

	// All or nothing: one manifest that cannot be signed leaves the
	// other unsigned too, so a release is never half signed.
	t.Run("a manifest that is missing", func(t *testing.T) {
		tt, _ := keyedTool(t)
		arm, amd := writeManifests(t)
		if err := os.Remove(amd); err != nil {
			t.Fatal(err)
		}
		if code := tt.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
	})

	t.Run("a manifest larger than an updater reads", func(t *testing.T) {
		tt, _ := keyedTool(t)
		arm, amd := writeManifests(t)
		if err := os.WriteFile(amd, bytes.Repeat([]byte("x"), releasesig.MaxManifestBytes+1), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := tt.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
	})

	t.Run("an empty manifest", func(t *testing.T) {
		tt, _ := keyedTool(t)
		arm, amd := writeManifests(t)
		if err := os.WriteFile(arm, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if code := tt.run([]string{"sign", arm, amd}); code != 1 {
			t.Fatalf("code %d, want 1", code)
		}
		noSignatures(t, arm, amd)
	})
}

func TestUsage(t *testing.T) {
	for name, args := range map[string][]string{
		"no command":                  nil,
		"an unknown command":          {"frobnicate"},
		"sign with nothing":           {"sign"},
		"verify with nothing":         {"verify"},
		"keygen with an unknown flag": {"keygen", "-print"},
	} {
		tt := newTestTool(t)
		if code := tt.run(args); code != 2 {
			t.Errorf("%s: code %d, want 2", name, code)
		}
	}
	tt := newTestTool(t)
	if code := tt.run([]string{"help"}); code != 0 || !strings.Contains(tt.out.String(), "keygen") {
		t.Errorf("help: code %d, stdout %q", code, tt.out.String())
	}
}
