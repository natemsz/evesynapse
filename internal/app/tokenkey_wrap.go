package app

// `evesynapse -wrap-token-key`: the oci provider's one setup
// step. The token encryption key that today sits in .env as
// TOKEN_ENCRYPTION_KEY is wrapped by the vault key named in
// TOKEN_KEY_OCI_KEY_ID, and the resulting ciphertext — the value
// to store as TOKEN_KEY_WRAPPED — is printed to stdout, alone.
// The wrap authenticates as the instance itself (instance
// principal), so the command only does anything on the machine
// whose tokens the key protects — which is the point: the
// plaintext is read from the environment on that machine, sent to
// the vault over TLS, and never logged or written anywhere here.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// RunWrapTokenKey implements the command. Exit codes follow the
// other one-shot modes: 2 for a configuration the operator must
// fix, 1 for a failed call, 0 with exactly the ciphertext on
// stdout.
func RunWrapTokenKey(args []string, cfg Config, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key: it takes no arguments; it reads TOKEN_ENCRYPTION_KEY and the TOKEN_KEY_OCI_* settings from the environment")
		return 2
	}
	provider, err := resolveTokenKeyProvider(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key:", err)
		return 2
	}
	if provider != tokenKeyProviderOCI {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key: TOKEN_KEY_PROVIDER is not oci; the wrap is an OCI KMS operation")
		return 2
	}
	if err := validateTokenKeyOCIBase(cfg); err != nil {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key:", err)
		return 2
	}
	secret := strings.TrimSpace(cfg.tokenKey)
	if secret == "" {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key: TOKEN_ENCRYPTION_KEY is not set; there is nothing to wrap")
		return 2
	}
	if _, err := newTokenBox(secret); err != nil {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key: TOKEN_ENCRYPTION_KEY is not usable:", err)
		return 2
	}
	client, err := ociClientForConfig(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wrapped, err := client.Encrypt(ctx, secret)
	if err != nil {
		fmt.Fprintln(stderr, "evesynapse -wrap-token-key: the wrap failed:", err)
		return 1
	}
	fmt.Fprintln(stdout, wrapped)
	return 0
}
