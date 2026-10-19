package app

// ---------------------------------------------------------------------------
// Where the token box's key comes from (auth_tokencrypt.go holds
// the box itself). TOKEN_KEY_PROVIDER chooses:
//
//   env  — TOKEN_ENCRYPTION_KEY from the environment, as always.
//   http — the root secret is fetched at startup from a key
//          service: the service holds the wrapping key, this
//          machine stores only the wrapped blob (TOKEN_KEY_WRAPPED)
//          and asks the endpoint (TOKEN_KEY_UNWRAP_URL, with the
//          bearer TOKEN_KEY_UNWRAP_CREDENTIAL) to unwrap it. The
//          secret then lives in memory only, so a copy of this
//          machine's files — database, .env and all — cannot open
//          the stored tokens. Any HTTPS endpoint speaking the
//          two-call protocol works; the reference service is a
//          Cloudflare Worker (github.com/natemsz/cloudflare-kms).
//   none — no key; tokens are stored unencrypted, as before.
//
// With no TOKEN_KEY_PROVIDER set, a configured TOKEN_ENCRYPTION_KEY
// means env and no key means none, so existing installs behave
// exactly as they did before providers existed.
//
// The http provider never blocks the start: the app serves at once
// and a background loop retries the unwrap with backoff until it
// succeeds. Until then the box has no key, so sealed rows report
// their key missing (errTokenKeyMissing) and everything that does
// not need a stored token — the public pages included — works as
// usual. A fetched secret is installed only after it has been shown
// to open every stored token, the same check prepareStoredTokens
// runs at boot for the other providers.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

// The TOKEN_KEY_PROVIDER values.
const (
	tokenKeyProviderEnv  = "env"
	tokenKeyProviderHTTP = "http"
	tokenKeyProviderNone = "none"
)

// resolveTokenKeyProvider settles which provider the configuration
// asks for, per the rules in the file header.
func resolveTokenKeyProvider(cfg Config) (string, error) {
	switch p := strings.ToLower(strings.TrimSpace(cfg.tokenKeyProvider)); p {
	case "":
		if strings.TrimSpace(cfg.tokenKey) != "" {
			return tokenKeyProviderEnv, nil
		}
		return tokenKeyProviderNone, nil
	case tokenKeyProviderEnv, tokenKeyProviderHTTP, tokenKeyProviderNone:
		return p, nil
	default:
		return "", fmt.Errorf("TOKEN_KEY_PROVIDER is %q; it must be env, http or none", cfg.tokenKeyProvider)
	}
}

// tokenBoxForConfig builds the boot-time box for the resolved
// provider. For http the box starts locked (no key): the caller
// starts the background unlock (startTokenKeyUnlock) once the app
// exists. Configuration mistakes are boot errors, in the open,
// rather than a silently weaker mode than the operator asked for.
func tokenBoxForConfig(cfg Config) (*tokenBox, string, error) {
	provider, err := resolveTokenKeyProvider(cfg)
	if err != nil {
		return nil, "", err
	}
	switch provider {
	case tokenKeyProviderEnv:
		if strings.TrimSpace(cfg.tokenKey) == "" {
			return nil, "", errors.New("TOKEN_KEY_PROVIDER=env but TOKEN_ENCRYPTION_KEY is not set")
		}
		box, err := newTokenBox(cfg.tokenKey)
		if err != nil {
			return nil, "", err
		}
		return box, provider, nil
	case tokenKeyProviderHTTP:
		if err := validateTokenKeyHTTPConfig(cfg); err != nil {
			return nil, "", err
		}
		return &tokenBox{}, provider, nil
	default: // none
		return &tokenBox{}, provider, nil
	}
}

// validateTokenKeyHTTPConfig names the first missing or malformed
// piece of the http provider's configuration.
func validateTokenKeyHTTPConfig(cfg Config) error {
	if strings.TrimSpace(cfg.tokenKeyWrapped) == "" {
		return errors.New("TOKEN_KEY_PROVIDER=http but TOKEN_KEY_WRAPPED is not set")
	}
	if strings.TrimSpace(cfg.tokenKeyUnwrapURL) == "" {
		return errors.New("TOKEN_KEY_PROVIDER=http but TOKEN_KEY_UNWRAP_URL is not set")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.tokenKeyUnwrapURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("TOKEN_KEY_UNWRAP_URL is %q; it must be the full https address of the unwrap endpoint", cfg.tokenKeyUnwrapURL)
	}
	if strings.TrimSpace(cfg.tokenKeyCred) == "" {
		return errors.New("TOKEN_KEY_PROVIDER=http but TOKEN_KEY_UNWRAP_CREDENTIAL is not set")
	}
	return nil
}

// The unwrap endpoint's wire shapes: the request carries the
// wrapped blob, the answer carries the secret. (The same service's
// wrap call, used at setup, takes {"secret": …} to {"wrapped": …}.)
type tokenUnwrapRequest struct {
	Wrapped string `json:"wrapped"`
}

type tokenUnwrapResponse struct {
	Secret string `json:"secret"`
}

// tokenKeyHTTPClient is the unwrap call's client; a package-level
// value so the timeout lives in one place.
var tokenKeyHTTPClient = &http.Client{Timeout: 10 * time.Second}

// unwrapTokenSecret asks the configured endpoint to unwrap the
// stored blob into the root secret. The secret is only ever
// returned to the caller: it is never logged and never written
// anywhere.
func unwrapTokenSecret(ctx context.Context, cfg Config) (string, error) {
	body, err := json.Marshal(tokenUnwrapRequest{Wrapped: strings.TrimSpace(cfg.tokenKeyWrapped)})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(cfg.tokenKeyUnwrapURL), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.tokenKeyCred))
	resp, err := tokenKeyHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call the token key unwrap endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("the token key unwrap endpoint answered HTTP %d", resp.StatusCode)
	}
	var answer tokenUnwrapResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&answer); err != nil {
		return "", fmt.Errorf("read the token key unwrap answer: %w", err)
	}
	if strings.TrimSpace(answer.Secret) == "" {
		return "", errors.New("the token key unwrap endpoint returned no secret")
	}
	return answer.Secret, nil
}

// The background unlock's retry pacing: quick at first (the usual
// case is a service that answers a moment after this one starts),
// backing off to a slow poll so an unreachable endpoint is not
// hammered for the life of the process.
var (
	tokenKeyRetryStart = 5 * time.Second
	tokenKeyRetryMax   = 2 * time.Minute
)

// startTokenKeyUnlock launches the background unlock for the http
// provider. ctx is the worker's: Close cancels it.
func (app *Application) startTokenKeyUnlock(ctx context.Context) {
	go runTokenKeyUnlockLoop(ctx, app.tryTokenKeyUnlock)
}

// runTokenKeyUnlockLoop retries attempt (which reports whether the
// box is unlocked) with backoff until it succeeds or ctx ends.
func runTokenKeyUnlockLoop(ctx context.Context, attempt func(context.Context) bool) {
	interval := tokenKeyRetryStart
	for {
		if attempt(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if interval < tokenKeyRetryMax {
			interval *= 2
			if interval > tokenKeyRetryMax {
				interval = tokenKeyRetryMax
			}
		}
	}
}

// tryTokenKeyUnlock makes one unwrap attempt and reports whether
// the box is unlocked now. Every failure is logged and left to the
// loop's next attempt; nothing here can fail the running app.
func (app *Application) tryTokenKeyUnlock(ctx context.Context) bool {
	secret, err := unwrapTokenSecret(ctx, app.cfg)
	if err != nil {
		logging.Warnf("evesynapse: token key unwrap failed (will retry): %v", err)
		return false
	}
	candidate, err := newTokenBox(secret)
	if err != nil {
		// A short or malformed secret from the endpoint is
		// rejected by the same rule as a short env key.
		logging.Warnf("evesynapse: the token key unwrap endpoint returned an unusable secret (will retry): %v", err)
		return false
	}
	if err := app.checkStoredTokensOpen(ctx, candidate); err != nil {
		logging.Warnf("evesynapse: the unwrapped token key does not open the stored tokens (will retry): %v", err)
		return false
	}
	app.tokens.install(candidate)
	// With the box able to open rows, bring the stored tokens in
	// line — sealing anything written while the box was locked and
	// re-sealing retired envelopes — the same pass the other
	// providers get at boot. A failure here is logged, not fatal:
	// the app is already serving, and the next start re-runs it.
	if err := app.prepareStoredTokens(ctx); err != nil {
		logging.Warnf("evesynapse: token key unlocked, but the stored-token pass failed: %v", err)
	}
	logging.Infof("evesynapse: token encryption key loaded from the unwrap endpoint; stored tokens are unlocked")
	return true
}

// checkStoredTokensOpen reports whether every stored token pair —
// EVE characters and Discord links alike — opens with box. It is
// the unlock's pre-flight: a fetched key that fails here is never
// installed, so the running app is never left sealing new tokens
// under a key that cannot read the old ones.
func (app *Application) checkStoredTokensOpen(ctx context.Context, box *tokenBox) error {
	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		return fmt.Errorf("list characters: %w", err)
	}
	for _, ch := range characters {
		if _, _, err := box.openTokens(ch); err != nil {
			return fmt.Errorf("character %d: %w", ch.CharacterID, err)
		}
	}
	links, err := app.queries.ListDiscordLinks(ctx)
	if err != nil {
		return fmt.Errorf("list discord links: %w", err)
	}
	for _, link := range links {
		if _, _, err := box.openDiscordTokens(link); err != nil {
			return fmt.Errorf("discord link of user %d: %w", link.UserID, err)
		}
	}
	return nil
}
