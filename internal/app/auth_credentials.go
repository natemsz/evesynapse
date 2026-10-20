package app

// ---------------------------------------------------------------------------
// Where the working credentials come from — phase 2 of the key
// design (auth_tokenkey.go is phase 1, the token key itself). The
// four credentials the app presents as itself — EVE_CLIENT_SECRET,
// DISCORD_CLIENT_SECRET, DISCORD_BOT_TOKEN, MODEL_API_KEY — can
// be supplied from outside the machine and held in memory only:
//
//   oci  — each lives in OCI Secrets; .env carries only its
//          secret OCID (<name>_OCI_SECRET_ID), and the app reads
//          it at startup with the instance's own identity
//          (internal/ocikms) — no credential is stored on the
//          machine at all.
//   http — each is stored in .env wrapped by the key service
//          (<name>_WRAPPED) and unwrapped at startup through the
//          same endpoint that unwraps the token key.
//
// A credential with no external source keeps its plain env value,
// exactly as before. Until a fetch lands, the plain value (when
// one is set) serves, so the cutover is per credential: add the
// external source, watch it land, then drop the plain value.
// SHARED_CACHE_SECRET is deliberately not among them: it is a
// class proof, shared freely with swarm operators by design.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"evesynapse/internal/logging"
)

// The credential names: the environment variables, used for the
// store keys, the logs and the error messages alike.
const (
	credEVEClientSecret     = "EVE_CLIENT_SECRET"
	credDiscordClientSecret = "DISCORD_CLIENT_SECRET"
	credDiscordBotToken     = "DISCORD_BOT_TOKEN"
	credModelAPIKey         = "MODEL_API_KEY"
)

// credentialStore holds the credential values fetched from an
// external source, keyed by credential name. It starts empty: an
// absent entry means no fetched value has landed, and the Config
// accessors fall back to the plain env fields.
type credentialStore struct {
	mu     sync.RWMutex
	landed map[string]string
}

func newCredentialStore() *credentialStore {
	return &credentialStore{landed: make(map[string]string)}
}

func (s *credentialStore) install(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.landed[name] = value
}

func (s *credentialStore) get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.landed[name]
	return v, ok
}

// credentialSource is one working credential's configured
// external source: under the oci provider an OCI secret OCID,
// under http a blob wrapped by the key service. Both empty means
// the credential is plain-env only.
type credentialSource struct {
	name    string // the environment variable, for logs and errors
	ociID   string // <name>_OCI_SECRET_ID
	wrapped string // <name>_WRAPPED
}

// external reports whether the credential has an external source.
func (s credentialSource) external() bool { return s.ociID != "" || s.wrapped != "" }

// credentialSources lists the four credentials' configured
// external sources, in a fixed order for logs and tests.
func credentialSources(cfg Config) []credentialSource {
	trim := func(v string) string { return strings.TrimSpace(v) }
	return []credentialSource{
		{credEVEClientSecret, trim(cfg.eveClientSecretOCIID), trim(cfg.eveClientSecretWrapped)},
		{credDiscordClientSecret, trim(cfg.discordClientSecretOCIID), trim(cfg.discordClientSecretWrapped)},
		{credDiscordBotToken, trim(cfg.discordBotTokenOCIID), trim(cfg.discordBotTokenWrapped)},
		{credModelAPIKey, trim(cfg.metaAIKeyOCIID), trim(cfg.metaAIKeyWrapped)},
	}
}

// hasExternalCredentials reports whether any credential is
// configured with an external source.
func hasExternalCredentials(cfg Config) bool {
	for _, src := range credentialSources(cfg) {
		if src.external() {
			return true
		}
	}
	return false
}

// validateCredentialSources checks the credentials' external
// sources against the token key provider and the transport each
// source rides, under phase 1's rule: a source named under the
// wrong provider, or with its transport pieces missing, is a boot
// error naming the variable — never a silent fall back to a
// weaker arrangement than the operator asked for.
func validateCredentialSources(cfg Config) error {
	provider, err := resolveTokenKeyProvider(cfg)
	if err != nil {
		return err
	}
	for _, src := range credentialSources(cfg) {
		if src.ociID != "" {
			if provider != tokenKeyProviderOCI {
				return fmt.Errorf("%s_OCI_SECRET_ID is set but TOKEN_KEY_PROVIDER is not oci", src.name)
			}
			if strings.TrimSpace(cfg.tokenKeyOCIEndpoint) == "" {
				return errors.New("TOKEN_KEY_OCI_CRYPTO_ENDPOINT is not set")
			}
		}
		if src.wrapped != "" {
			if provider != tokenKeyProviderHTTP {
				return fmt.Errorf("%s_WRAPPED is set but TOKEN_KEY_PROVIDER is not http", src.name)
			}
			if strings.TrimSpace(cfg.tokenKeyUnwrapURL) == "" {
				return errors.New("TOKEN_KEY_UNWRAP_URL is not set")
			}
			if strings.TrimSpace(cfg.tokenKeyCred) == "" {
				return errors.New("TOKEN_KEY_UNWRAP_CREDENTIAL is not set")
			}
		}
	}
	return nil
}

// credentialFetcher fetches one credential from its external
// source. A fetched value is only ever returned to the caller:
// never logged, never written anywhere.
type credentialFetcher func(ctx context.Context, src credentialSource) (string, error)

// credentialFetcherFor builds the fetcher for the configured
// provider, once per resolution pass, so the oci client's
// security token is exchanged once and shared by every
// credential in the pass. A provider that fetches nothing
// returns a nil fetcher.
func credentialFetcherFor(cfg Config) (credentialFetcher, string, error) {
	provider, err := resolveTokenKeyProvider(cfg)
	if err != nil {
		return nil, "", err
	}
	switch provider {
	case tokenKeyProviderOCI:
		client, err := ociClientForConfig(cfg)
		if err != nil {
			return nil, "", err
		}
		return func(ctx context.Context, src credentialSource) (string, error) {
			return client.GetSecret(ctx, src.ociID)
		}, provider, nil
	case tokenKeyProviderHTTP:
		return func(ctx context.Context, src credentialSource) (string, error) {
			return unwrapViaKeyService(ctx, cfg, src.wrapped)
		}, provider, nil
	default:
		return nil, provider, nil
	}
}

// fetchCredential fetches one credential from its external
// source: credentialFetcherFor for a single call.
func fetchCredential(ctx context.Context, cfg Config, src credentialSource) (string, error) {
	fetch, provider, err := credentialFetcherFor(cfg)
	if err != nil {
		return "", err
	}
	if fetch == nil {
		return "", fmt.Errorf("the %s provider fetches no credentials", provider)
	}
	return fetch(ctx, src)
}

// credStore is the application's credential store, created on
// first use: a Config built by hand (tests do this) may not carry
// one, where LoadConfig always does.
func (app *Application) credStore() *credentialStore {
	if app.cfg.creds == nil {
		app.cfg.creds = newCredentialStore()
	}
	return app.cfg.creds
}

// startCredentialResolution launches the background resolver for
// the externally-sourced credentials: the app serves at once, and
// each credential takes effect through the Config accessors the
// moment its fetch lands. ctx is the worker's: Close cancels it.
func (app *Application) startCredentialResolution(ctx context.Context) {
	go runTokenKeyUnlockLoop(ctx, app.tryResolveCredentials)
}

// tryResolveCredentials makes one resolution pass and reports
// whether every externally-sourced credential has landed. Each
// credential is fetched on its own: one source's failure is
// logged and left to the loop's next pass, and never blocks the
// others.
func (app *Application) tryResolveCredentials(ctx context.Context) bool {
	fetch, provider, err := credentialFetcherFor(app.cfg)
	if err != nil {
		// Settled at boot (New refuses the configuration); there
		// is nothing a retry could change.
		return true
	}
	if fetch == nil {
		return true
	}
	store := app.credStore()
	external, pending := 0, 0
	for _, src := range credentialSources(app.cfg) {
		if !src.external() {
			continue
		}
		external++
		if _, ok := store.get(src.name); ok {
			continue
		}
		value, err := fetch(ctx, src)
		if err != nil {
			logging.Warnf("evesynapse: credential %s fetch failed (will retry): %v", src.name, err)
			pending++
			continue
		}
		if strings.TrimSpace(value) == "" {
			logging.Warnf("evesynapse: credential %s came back empty (will retry)", src.name)
			pending++
			continue
		}
		store.install(src.name, value)
		logging.Infof("evesynapse: credential %s loaded via the %s provider", src.name, provider)
	}
	if pending > 0 {
		return false
	}
	if external > 0 {
		logging.Infof("evesynapse: all %d externally-sourced credentials are loaded", external)
	}
	return true
}
