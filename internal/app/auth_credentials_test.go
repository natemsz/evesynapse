package app

// Tests for the working credentials' external sources
// (auth_credentials.go): configuration mismatches are boot
// errors naming the variable, the plain env value serves until
// a fetch lands and is then replaced, credentials resolve
// independently of one another, and a failed fetch leaves the
// plain value in place for the next pass. Every endpoint here
// is an httptest server; the values are obvious fakes.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestValidateCredentialSources(t *testing.T) {
	ociOK := Config{
		tokenKeyProvider:     "oci",
		tokenKeyOCIEndpoint:  "https://vault-crypto.kms.us-phoenix-1.oraclecloud.com",
		eveClientSecretOCIID: "ocid1.vaultsecret.oc1.phx.fake",
	}
	httpOK := Config{
		tokenKeyProvider:       "http",
		tokenKeyUnwrapURL:      "https://keys.example/v1/unwrap",
		tokenKeyCred:           "cred",
		discordBotTokenWrapped: "wrapped-blob",
	}
	cases := []struct {
		name    string
		cfg     Config
		wantErr string // "" means valid
	}{
		{"nothing external", Config{}, ""},
		{"plain only", Config{tokenKeyProvider: "env", eveClientSecret: "plain"}, ""},
		{"oci source complete", ociOK, ""},
		{"http source complete", httpOK, ""},
		{"oci id under http", Config{tokenKeyProvider: "http", tokenKeyUnwrapURL: "https://keys.example/v1/unwrap", tokenKeyCred: "cred", eveClientSecretOCIID: "ocid1.vaultsecret.oc1.phx.fake"}, "EVE_CLIENT_SECRET_OCI_SECRET_ID"},
		{"oci id under env", Config{tokenKeyProvider: "env", tokenKey: testTokenKey, discordBotTokenOCIID: "ocid1.vaultsecret.oc1.phx.fake"}, "DISCORD_BOT_TOKEN_OCI_SECRET_ID"},
		{"oci id under none", Config{tokenKeyProvider: "none", metaAIKeyOCIID: "ocid1.vaultsecret.oc1.phx.fake"}, "MODEL_API_KEY_OCI_SECRET_ID"},
		{"wrapped under oci", Config{tokenKeyProvider: "oci", tokenKeyOCIEndpoint: "https://vault-crypto.kms.us-phoenix-1.oraclecloud.com", eveClientSecretWrapped: "blob"}, "EVE_CLIENT_SECRET_WRAPPED"},
		{"wrapped under env", Config{tokenKeyProvider: "env", tokenKey: testTokenKey, discordClientSecretWrapped: "blob"}, "DISCORD_CLIENT_SECRET_WRAPPED"},
		{"oci id without endpoint", Config{tokenKeyProvider: "oci", metaAIKeyOCIID: "ocid1.vaultsecret.oc1.phx.fake"}, "TOKEN_KEY_OCI_CRYPTO_ENDPOINT"},
		{"wrapped without url", Config{tokenKeyProvider: "http", tokenKeyCred: "cred", eveClientSecretWrapped: "blob"}, "TOKEN_KEY_UNWRAP_URL"},
		{"wrapped without credential", Config{tokenKeyProvider: "http", tokenKeyUnwrapURL: "https://keys.example/v1/unwrap", eveClientSecretWrapped: "blob"}, "TOKEN_KEY_UNWRAP_CREDENTIAL"},
	}
	for _, c := range cases {
		err := validateCredentialSources(c.cfg)
		if c.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: err = %v; want valid", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("%s: err = %v; want it to name %s", c.name, err, c.wantErr)
		}
	}

	// The boot path refuses a mismatch before anything else
	// starts: New names the variable.
	if _, err := New(Config{tokenKeyProvider: "none", eveClientSecretOCIID: "ocid1.vaultsecret.oc1.phx.fake"}); err == nil ||
		!strings.Contains(err.Error(), "EVE_CLIENT_SECRET_OCI_SECRET_ID") {
		t.Fatalf("New with a mismatched source: err = %v", err)
	}
}

func TestCredentialAccessors(t *testing.T) {
	cfg := Config{
		eveClientID:     "client-id",
		eveClientSecret: "plain-eve",
		metaAIKey:       "plain-model",
		creds:           newCredentialStore(),
	}
	if got := cfg.EVEClientSecret(); got != "plain-eve" {
		t.Fatalf("EVEClientSecret = %q; want the plain value before any fetch", got)
	}
	if !cfg.SSOConfigured() {
		t.Fatal("SSOConfigured = false with the plain secret set")
	}
	if !cfg.TradeIdeasConfigured() || !cfg.tradeIdeasUsesMeta() {
		t.Fatal("trade ideas should read as Meta-configured from the plain key")
	}
	// A landed value replaces the plain one through every reader.
	cfg.creds.install(credEVEClientSecret, "fetched-eve")
	cfg.creds.install(credModelAPIKey, "fetched-model")
	if got := cfg.EVEClientSecret(); got != "fetched-eve" {
		t.Fatalf("EVEClientSecret = %q; want the fetched value", got)
	}
	if got := cfg.MetaAIKey(); got != "fetched-model" {
		t.Fatalf("MetaAIKey = %q; want the fetched value", got)
	}
	// A credential can also ARRIVE for a feature that was off:
	// nothing plain, then the fetch lands.
	cfg2 := Config{eveClientID: "client-id", creds: newCredentialStore()}
	if cfg2.SSOConfigured() {
		t.Fatal("SSOConfigured = true with no secret anywhere")
	}
	cfg2.creds.install(credEVEClientSecret, "fetched-eve")
	if !cfg2.SSOConfigured() {
		t.Fatal("SSOConfigured = false after the secret landed")
	}
	// A hand-built Config without a store still reads its fields.
	bare := Config{discordBotToken: "plain-bot"}
	if got := bare.DiscordBotToken(); got != "plain-bot" {
		t.Fatalf("DiscordBotToken without a store = %q", got)
	}
}

// unwrapMapStub is the key service end of the phase-2 protocol:
// one unwrap call per credential, answering from a wrapped→secret
// map, with per-blob failures the test can arm and clear.
type unwrapMapStub struct {
	mu      sync.Mutex
	secrets map[string]string
	fail    map[string]bool
	auth    string
	calls   map[string]int
}

func (s *unwrapMapStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body tokenUnwrapRequest
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.auth = r.Header.Get("Authorization")
		if s.calls == nil {
			s.calls = map[string]int{}
		}
		s.calls[body.Wrapped]++
		fail := s.fail[body.Wrapped]
		secret, ok := s.secrets[body.Wrapped]
		s.mu.Unlock()
		if fail || !ok {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenUnwrapResponse{Secret: secret})
	}
}

func TestResolveCredentialsHTTP(t *testing.T) {
	stub := &unwrapMapStub{
		secrets: map[string]string{
			"wrapped-eve": "fetched-eve-secret",
			"wrapped-bot": "fetched-bot-token",
		},
		fail: map[string]bool{"wrapped-bot": true},
	}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()

	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	app.cfg.tokenKeyProvider = tokenKeyProviderHTTP
	app.cfg.tokenKeyWrapped = "wrapped-token-key"
	app.cfg.tokenKeyUnwrapURL = srv.URL
	app.cfg.tokenKeyCred = "test-credential"
	// The EVE secret is mid-cutover: plain value still present,
	// external source configured. The bot token is external-only.
	// The model key is plain-only and must never be fetched.
	app.cfg.eveClientSecret = "plain-eve-secret"
	app.cfg.eveClientSecretWrapped = "wrapped-eve"
	app.cfg.discordBotTokenWrapped = "wrapped-bot"
	app.cfg.metaAIKey = "plain-model-key"

	if app.tryResolveCredentials(ctx) {
		t.Fatal("resolution reported done while the bot token fetch was failing")
	}
	if got := app.cfg.EVEClientSecret(); got != "fetched-eve-secret" {
		t.Fatalf("EVEClientSecret = %q; want the fetched value over the plain one", got)
	}
	if got := app.cfg.DiscordBotToken(); got != "" {
		t.Fatalf("DiscordBotToken = %q; want still unset while its fetch fails", got)
	}
	if got := app.cfg.MetaAIKey(); got != "plain-model-key" {
		t.Fatalf("MetaAIKey = %q; want the untouched plain value", got)
	}
	if stub.auth != "Bearer test-credential" {
		t.Fatalf("unwrap authorization = %q", stub.auth)
	}
	if stub.calls["wrapped-eve"] != 1 || stub.calls["wrapped-bot"] != 1 || stub.calls["wrapped-token-key"] != 0 {
		t.Fatalf("unwrap calls = %v; want one per credential and none for the token key", stub.calls)
	}

	// The endpoint recovers: the next pass lands the bot token
	// and reports done; landed credentials are not refetched.
	stub.mu.Lock()
	stub.fail["wrapped-bot"] = false
	stub.mu.Unlock()
	if !app.tryResolveCredentials(ctx) {
		t.Fatal("resolution did not finish after the endpoint recovered")
	}
	if got := app.cfg.DiscordBotToken(); got != "fetched-bot-token" {
		t.Fatalf("DiscordBotToken = %q; want the fetched token", got)
	}
	if stub.calls["wrapped-eve"] != 1 {
		t.Fatalf("landed credential was refetched: calls = %v", stub.calls)
	}
}

// secretsStub is the OCI Secrets end: SDK-shaped bundles per
// secret ID, with a failure the test can arm and clear.
type secretsStub struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]bool
	paths  []string
}

func (s *secretsStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/20190301/secretbundles/")
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		fail := s.fail[id]
		value, ok := s.values[id]
		s.mu.Unlock()
		if fail || !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secretId":      id,
			"versionNumber": 1,
			"secretBundleContent": map[string]string{
				"contentType": "BASE64",
				"content":     base64.StdEncoding.EncodeToString([]byte(value)),
			},
		})
	}
}

func TestResolveCredentialsOCI(t *testing.T) {
	fakes := buildOCIFakes(t)
	const eveID = "ocid1.vaultsecret.oc1.phx.fakeeve"
	const botID = "ocid1.vaultsecret.oc1.phx.fakebot"
	stub := &secretsStub{
		values: map[string]string{eveID: "fetched-eve-secret", botID: "fetched-bot-token"},
		fail:   map[string]bool{botID: true},
	}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()

	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	fakes.pointOCIAt(&app.cfg)
	app.cfg.tokenKeyOCISecretsURL = srv.URL
	app.cfg.eveClientSecretOCIID = eveID
	app.cfg.discordBotTokenOCIID = botID
	app.cfg.discordClientSecret = "plain-discord-secret"

	if app.tryResolveCredentials(ctx) {
		t.Fatal("resolution reported done while the bot token fetch was failing")
	}
	if got := app.cfg.EVEClientSecret(); got != "fetched-eve-secret" {
		t.Fatalf("EVEClientSecret = %q; want the value from OCI Secrets", got)
	}
	if got := app.cfg.DiscordBotToken(); got != "" {
		t.Fatalf("DiscordBotToken = %q; want still unset while its fetch fails", got)
	}
	if got := app.cfg.DiscordClientSecret(); got != "plain-discord-secret" {
		t.Fatalf("DiscordClientSecret = %q; want the plain value (no external source)", got)
	}
	wantPath := "/20190301/secretbundles/" + eveID
	if len(stub.paths) == 0 || stub.paths[0] != wantPath {
		t.Fatalf("secrets paths = %v; want the first to be %q", stub.paths, wantPath)
	}

	stub.mu.Lock()
	stub.fail[botID] = false
	stub.mu.Unlock()
	if !app.tryResolveCredentials(ctx) {
		t.Fatal("resolution did not finish after the secrets service recovered")
	}
	if got := app.cfg.DiscordBotToken(); got != "fetched-bot-token" {
		t.Fatalf("DiscordBotToken = %q; want the value from OCI Secrets", got)
	}
}

func TestFetchCredentialProviders(t *testing.T) {
	// env/none fetch nothing.
	src := credentialSource{name: credEVEClientSecret, ociID: "ocid1.vaultsecret.oc1.phx.fake"}
	if _, err := fetchCredential(context.Background(), Config{tokenKeyProvider: "none"}, src); err == nil {
		t.Fatal("the none provider fetched a credential")
	}
	// hasExternalCredentials sees exactly the configured ones.
	cfg := Config{discordBotTokenWrapped: "blob"}
	if !hasExternalCredentials(cfg) {
		t.Fatal("hasExternalCredentials = false with a wrapped source set")
	}
	if hasExternalCredentials(Config{}) {
		t.Fatal("hasExternalCredentials = true with nothing set")
	}
}
