// Command releasesign is the maintainer's side of release signing
// (package releasesig has the other side, which every build carries).
// It makes the release key, signs the update manifests in the release
// job, and checks a signature by hand. It is never deployed.
//
//	releasesign keygen [-add] [-pub file] [-out file] [-- command [args]]
//	    Make a new release key. The private half is handed to the
//	    command on its standard input (the one that stores it as the
//	    release job's secret), written to the -out file, or both. It
//	    is never printed. Only once that has worked is the public half
//	    added to the trusted keys file, which is the change to commit:
//
//	        go run ./cmd/releasesign keygen -- gh secret set RELEASE_SIGNING_KEY
//
//	releasesign sign <manifest>...
//	    Sign each update manifest with the key in RELEASE_SIGNING_KEY,
//	    writing <manifest>.sig beside it. Refuses a key this build
//	    does not trust, since no updater would install what it signs.
//
//	releasesign verify <manifest>...
//	    Check each manifest against the <manifest>.sig beside it, the
//	    way the updater does.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"evesynapse/internal/releasesig"
)

const (
	// signingKeyEnv is where the release job hands sign its key.
	signingKeyEnv = "RELEASE_SIGNING_KEY"
	// trustedKeysPath is the trusted keys file, from the repository
	// root: where keygen adds a public key unless told otherwise.
	trustedKeysPath = "internal/releasesig/trusted_keys.pem"
)

const usage = `releasesign makes the release key and signs releases with it.

  releasesign keygen [-add] [-pub file] [-out file] [-- command [args]]
      Make a new key. The private half goes to the command's standard
      input, to the -out file, or both, and is never printed. The public
      half is added to the trusted keys file, the change to commit.

          go run ./cmd/releasesign keygen -- gh secret set ` + signingKeyEnv + `

  releasesign sign <manifest>...
      Sign each update manifest with the key in ` + signingKeyEnv + `,
      writing <manifest>.sig beside it.

  releasesign verify <manifest>...
      Check each manifest against the <manifest>.sig beside it.
`

// tool is the command with what it reaches outside itself made
// explicit, so the tests can stand in their own.
type tool struct {
	getenv  func(string) string
	trusted func() ([]ed25519.PublicKey, error)
	stdout  io.Writer
	stderr  io.Writer
}

func main() {
	t := tool{getenv: os.Getenv, trusted: releasesig.Trusted, stdout: os.Stdout, stderr: os.Stderr}
	os.Exit(t.run(os.Args[1:]))
}

func (t tool) run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(t.stderr, usage)
		return 2
	}
	switch args[0] {
	case "keygen":
		return t.keygen(args[1:])
	case "sign":
		return t.sign(args[1:])
	case "verify":
		return t.verify(args[1:])
	case "help", "-h", "-help", "--help":
		fmt.Fprint(t.stdout, usage)
		return 0
	}
	fmt.Fprintf(t.stderr, "releasesign: there is no %q command.\n\n%s", args[0], usage)
	return 2
}

// keygen makes a key pair, puts the private half where it was told
// to, and only then adds the public half to the trusted keys file. In
// that order a failure leaves either nothing changed or a stored key
// whose public half is printed for adding by hand; never a trusted
// key nobody holds.
func (t tool) keygen(args []string) int {
	fs := flag.NewFlagSet("releasesign keygen", flag.ContinueOnError)
	fs.SetOutput(t.stderr)
	pubPath := fs.String("pub", trustedKeysPath, "the trusted keys `file` the public half is added to")
	outPath := fs.String("out", "", "also write the private half to this new `file`, for a copy kept offline")
	add := fs.Bool("add", false, "add a key although the trusted keys file already holds one (when replacing a key)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	command := fs.Args()
	if *outPath == "" && len(command) == 0 {
		fmt.Fprintf(t.stderr, "releasesign keygen: the private half has to go somewhere, and it is never printed.\nGive the command that stores it, a file to write it to (-out), or both:\n\n    go run ./cmd/releasesign keygen -- gh secret set %s\n\nNothing was changed.\n", signingKeyEnv)
		return 2
	}

	existing, err := os.ReadFile(*pubPath)
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign keygen: can't read the trusted keys file: %v\nRun this from the repository root, or say where the file is with -pub.\nNothing was changed.\n", err)
		return 1
	}
	held, err := releasesig.ParsePublicKeys(existing)
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign keygen: %s can't be read as it stands: %v\nNothing was changed.\n", *pubPath, err)
		return 1
	}
	if len(held) > 0 && !*add {
		fmt.Fprintf(t.stderr, "releasesign keygen: %s already holds a release key, so no new one was made.\nTo replace a key, see \"Release signing\" in the README; it uses keygen -add.\nNothing was changed.\n", *pubPath)
		return 1
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign keygen: couldn't make a key: %v\nNothing was changed.\n", err)
		return 1
	}
	privPEM, err := releasesig.EncodePrivateKey(priv)
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign keygen: couldn't make a key: %v\nNothing was changed.\n", err)
		return 1
	}
	pubPEM, err := releasesig.EncodePublicKey(pub)
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign keygen: couldn't make a key: %v\nNothing was changed.\n", err)
		return 1
	}

	if *outPath != "" {
		if err := writeNewFile(*outPath, privPEM); err != nil {
			fmt.Fprintf(t.stderr, "releasesign keygen: couldn't write the private half: %v\nThe key was thrown away. Nothing was changed.\n", err)
			return 1
		}
	}
	if len(command) > 0 {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stdin = bytes.NewReader(privPEM)
		cmd.Stdout = t.stdout
		cmd.Stderr = t.stderr
		if err := cmd.Run(); err != nil {
			if *outPath != "" {
				_ = os.Remove(*outPath)
			}
			fmt.Fprintf(t.stderr, "releasesign keygen: %s did not take the key (%v).\nThe key was thrown away. Nothing was changed.\n", command[0], err)
			return 1
		}
	}

	updated := bytes.Clone(existing)
	if len(updated) > 0 && !bytes.HasSuffix(updated, []byte("\n")) {
		updated = append(updated, '\n')
	}
	updated = append(updated, '\n')
	updated = append(updated, pubPEM...)
	if err := os.WriteFile(*pubPath, updated, 0o644); err != nil {
		fmt.Fprintf(t.stderr, "releasesign keygen: the private half is stored, but %s couldn't be written: %v\nAdd this block to that file by hand, then commit it:\n\n%s\n", *pubPath, err, pubPEM)
		return 1
	}

	fmt.Fprintln(t.stdout, "A new release key was made.")
	if len(command) > 0 {
		fmt.Fprintf(t.stdout, "  Private half: handed to `%s`.\n", strings.Join(command, " "))
	}
	if *outPath != "" {
		fmt.Fprintf(t.stdout, "  Private half: written to %s. Keep it offline, and never in the repository.\n", *outPath)
	}
	fmt.Fprintf(t.stdout, "  Public half:  added to %s\n                (%s)\n", *pubPath, releasesig.KeyLine(pub))
	fmt.Fprintln(t.stdout, "Commit that file. A release built from a commit that has it is signed with this key.")
	return 0
}

// writeNewFile writes a file that must not exist yet, readable by its
// owner only: a private key never replaces another file, and one that
// could only be half written is removed.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// sign writes the signature file beside each manifest. Every manifest
// is signed and checked, and every signature written to a temp file,
// before any .sig file takes its name, so a failure before the renames
// leaves no partly signed release behind. The renames themselves are
// the one step that can still fail partway (a rename error stops at
// the first and removes the unrenamed temps).
func (t tool) sign(paths []string) int {
	if len(paths) == 0 {
		fmt.Fprint(t.stderr, "releasesign sign: name the manifests to sign.\n\n"+usage)
		return 2
	}
	keyPEM := t.getenv(signingKeyEnv)
	if strings.TrimSpace(keyPEM) == "" {
		fmt.Fprintf(t.stderr, "releasesign sign: %s is not set, so there is nothing to sign with, and a release is only published signed.\nIn the release job it comes from the repository secret of that name. To make a key and store it there:\n\n    go run ./cmd/releasesign keygen -- gh secret set %s\n\nNothing was signed.\n", signingKeyEnv, signingKeyEnv)
		return 1
	}
	keys, err := releasesig.ParsePrivateKeys([]byte(keyPEM))
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign sign: %s does not hold a signing key as keygen writes it: %v\nNothing was signed.\n", signingKeyEnv, err)
		return 1
	}
	trusted, err := t.trusted()
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign sign: the trusted keys in this build can't be read: %v\nNothing was signed.\n", err)
		return 1
	}
	for _, key := range keys {
		pub := key.Public().(ed25519.PublicKey)
		if !slices.ContainsFunc(trusted, func(k ed25519.PublicKey) bool { return k.Equal(pub) }) {
			fmt.Fprintf(t.stderr, "releasesign sign: the signing key is not one this build trusts.\nIts public half is not in %s:\n    %s\nso no updater built from this commit would install a release it signed. The secret and that file have to hold the two halves of the same key.\nNothing was signed.\n", trustedKeysPath, releasesig.KeyLine(pub))
			return 1
		}
	}

	signatures := make([][]byte, len(paths))
	for i, path := range paths {
		manifest, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(t.stderr, "releasesign sign: %v\nNothing was signed.\n", err)
			return 1
		}
		if len(manifest) == 0 || len(manifest) > releasesig.MaxManifestBytes {
			fmt.Fprintf(t.stderr, "releasesign sign: %s is %d bytes; a manifest is between 1 and %d, the most an updater will read.\nNothing was signed.\n", path, len(manifest), releasesig.MaxManifestBytes)
			return 1
		}
		sig := releasesig.Sign(keys, manifest)
		if len(sig) > releasesig.MaxSigBytes {
			fmt.Fprintf(t.stderr, "releasesign sign: signing with %d keys makes a signature file of %d bytes, more than the %d an updater will read.\nNothing was signed.\n", len(keys), len(sig), releasesig.MaxSigBytes)
			return 1
		}
		// The check an updater will make, made here first.
		if err := releasesig.Verify(trusted, manifest, sig); err != nil {
			fmt.Fprintf(t.stderr, "releasesign sign: the signature just made for %s does not verify: %v\nNothing was signed.\n", path, err)
			return 1
		}
		signatures[i] = sig
	}
	// Two-phase commit: every signature lands in a temp file
	// first, and only when all of them are down do the temps take
	// their .sig names. A failure anywhere before the renames
	// leaves no partly signed release behind.
	temps := make([]string, len(paths))
	untemp := func() {
		for _, tmp := range temps {
			if tmp != "" {
				_ = os.Remove(tmp)
			}
		}
	}
	for i, path := range paths {
		tmp, err := os.CreateTemp(filepath.Dir(path), ".releasesign-*.tmp")
		if err != nil {
			untemp()
			fmt.Fprintf(t.stderr, "releasesign sign: %v\nNothing was signed.\n", err)
			return 1
		}
		temps[i] = tmp.Name()
		_, werr := tmp.Write(signatures[i])
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			untemp()
			if werr == nil {
				werr = cerr
			}
			fmt.Fprintf(t.stderr, "releasesign sign: %v\nNothing was signed.\n", werr)
			return 1
		}
		if err := os.Chmod(tmp.Name(), 0o644); err != nil {
			untemp()
			fmt.Fprintf(t.stderr, "releasesign sign: %v\nNothing was signed.\n", err)
			return 1
		}
	}
	for i, path := range paths {
		if err := os.Rename(temps[i], path+releasesig.SigSuffix); err != nil {
			untemp()
			fmt.Fprintf(t.stderr, "releasesign sign: %v\n", err)
			return 1
		}
		fmt.Fprintf(t.stdout, "signed %s -> %s\n", path, path+releasesig.SigSuffix)
	}
	return 0
}

// verify checks each manifest against the signature file beside it
// and the trusted keys in this build.
func (t tool) verify(paths []string) int {
	if len(paths) == 0 {
		fmt.Fprint(t.stderr, "releasesign verify: name the manifests to check.\n\n"+usage)
		return 2
	}
	trusted, err := t.trusted()
	if err != nil {
		fmt.Fprintf(t.stderr, "releasesign verify: the trusted keys in this build can't be read: %v\n", err)
		return 1
	}
	code := 0
	for _, path := range paths {
		manifest, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(t.stderr, "%s: NOT VERIFIED: %v\n", path, err)
			code = 1
			continue
		}
		sig, err := os.ReadFile(path + releasesig.SigSuffix)
		if err != nil {
			fmt.Fprintf(t.stderr, "%s: NOT VERIFIED: %v\n", path, err)
			code = 1
			continue
		}
		if err := releasesig.Verify(trusted, manifest, sig); err != nil {
			fmt.Fprintf(t.stderr, "%s: NOT VERIFIED: %v\n", path, err)
			code = 1
			continue
		}
		fmt.Fprintf(t.stdout, "%s: signed by a trusted release key\n", path)
	}
	return code
}
