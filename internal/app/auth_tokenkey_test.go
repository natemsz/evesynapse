package app

// Tests for the token key providers (auth_tokenkey.go): provider
// selection keeps existing installs behaving as they always did,
// the http provider's boot-time configuration is validated in the
// open, the unwrap call speaks the protocol, and an Application
// whose endpoint is down at boot reports sealed rows as
// key-missing — then unlocks, and can seal and open, once the
// endpoint answers.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func TestResolveTokenKeyProvider(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"key set, provider unset", Config{tokenKey: testTokenKey}, tokenKeyProviderEnv},
		{"nothing set", Config{}, tokenKeyProviderNone},
		{"explicit env", Config{tokenKeyProvider: "env", tokenKey: testTokenKey}, tokenKeyProviderEnv},
		{"explicit http", Config{tokenKeyProvider: "http"}, tokenKeyProviderHTTP},
		{"explicit oci", Config{tokenKeyProvider: "oci"}, tokenKeyProviderOCI},
		{"explicit none beats a key", Config{tokenKeyProvider: "none", tokenKey: testTokenKey}, tokenKeyProviderNone},
		{"case and space tolerated", Config{tokenKeyProvider: " HTTP "}, tokenKeyProviderHTTP},
	}
	for _, c := range cases {
		got, err := resolveTokenKeyProvider(c.cfg)
		if err != nil || got != c.want {
			t.Fatalf("%s: resolve = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	if _, err := resolveTokenKeyProvider(Config{tokenKeyProvider: "carrier-pigeon"}); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}

func TestTokenBoxForConfig(t *testing.T) {
	// env with a key: a working box, as today.
	cfg := Config{tokenKey: testTokenKey}
	box, provider, err := tokenBoxForConfig(cfg)
	if err != nil || provider != tokenKeyProviderEnv || !box.enabled() {
		t.Fatalf("env: box enabled %v, provider %q, err %v", box.enabled(), provider, err)
	}
	// env named but no key: a boot error, not a silent plaintext mode.
	if _, _, err := tokenBoxForConfig(Config{tokenKeyProvider: "env"}); err == nil ||
		!strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") {
		t.Fatalf("env without a key: err = %v", err)
	}
	// env with a short key: the box's own rule still applies.
	if _, _, err := tokenBoxForConfig(Config{tokenKey: "short"}); err == nil {
		t.Fatal("a short env key was accepted")
	}
	// none: an empty box.
	box, provider, err = tokenBoxForConfig(Config{tokenKeyProvider: "none", tokenKey: testTokenKey})
	if err != nil || provider != tokenKeyProviderNone || box.enabled() {
		t.Fatalf("none: box enabled %v, provider %q, err %v", box.enabled(), provider, err)
	}
	// http: each missing piece is named; complete config yields a
	// locked box (no key yet, not an error).
	httpCfg := Config{
		tokenKeyProvider:  "http",
		tokenKeyWrapped:   "wrapped-blob",
		tokenKeyUnwrapURL: "https://keys.example/v1/unwrap",
		tokenKeyCred:      "cred",
	}
	for _, drop := range []string{"tokenKeyWrapped", "tokenKeyUnwrapURL", "tokenKeyCred"} {
		broken := httpCfg
		switch drop {
		case "tokenKeyWrapped":
			broken.tokenKeyWrapped = ""
		case "tokenKeyUnwrapURL":
			broken.tokenKeyUnwrapURL = ""
		case "tokenKeyCred":
			broken.tokenKeyCred = ""
		}
		if _, _, err := tokenBoxForConfig(broken); err == nil || !strings.Contains(err.Error(), dropToEnvName(drop)) {
			t.Fatalf("http without %s: err = %v", drop, err)
		}
	}
	bad := httpCfg
	bad.tokenKeyUnwrapURL = "keys.example/unwrap"
	if _, _, err := tokenBoxForConfig(bad); err == nil {
		t.Fatal("a schemeless unwrap URL was accepted")
	}
	box, provider, err = tokenBoxForConfig(httpCfg)
	if err != nil || provider != tokenKeyProviderHTTP || box.enabled() {
		t.Fatalf("http: box enabled %v, provider %q, err %v; want a locked box", box.enabled(), provider, err)
	}

	// oci: the wrapped blob, the key ID and the endpoint are each
	// named when missing; a complete config yields a locked box.
	ociCfg := Config{
		tokenKeyProvider:    "oci",
		tokenKeyWrapped:     "wrapped-blob",
		tokenKeyOCIKeyID:    "ocid1.key.oc1.phx.test",
		tokenKeyOCIEndpoint: "https://vault-crypto.kms.us-phoenix-1.oraclecloud.com",
	}
	for field, envName := range map[string]string{
		"tokenKeyWrapped": "TOKEN_KEY_WRAPPED", "tokenKeyOCIKeyID": "TOKEN_KEY_OCI_KEY_ID",
		"tokenKeyOCIEndpoint": "TOKEN_KEY_OCI_CRYPTO_ENDPOINT",
	} {
		broken := ociCfg
		switch field {
		case "tokenKeyWrapped":
			broken.tokenKeyWrapped = ""
		case "tokenKeyOCIKeyID":
			broken.tokenKeyOCIKeyID = ""
		case "tokenKeyOCIEndpoint":
			broken.tokenKeyOCIEndpoint = ""
		}
		if _, _, err := tokenBoxForConfig(broken); err == nil || !strings.Contains(err.Error(), envName) {
			t.Fatalf("oci without %s: err = %v", field, err)
		}
	}
	box, provider, err = tokenBoxForConfig(ociCfg)
	if err != nil || provider != tokenKeyProviderOCI || box.enabled() {
		t.Fatalf("oci: box enabled %v, provider %q, err %v; want a locked box", box.enabled(), provider, err)
	}
}

func dropToEnvName(field string) string {
	switch field {
	case "tokenKeyWrapped":
		return "TOKEN_KEY_WRAPPED"
	case "tokenKeyUnwrapURL":
		return "TOKEN_KEY_UNWRAP_URL"
	default:
		return "TOKEN_KEY_UNWRAP_CREDENTIAL"
	}
}

// unwrapStub is the key service end of the protocol: it records
// how it was called and answers with the secret it was set up with
// (or an HTTP status, when failWith is set).
type unwrapStub struct {
	secret   string
	failWith int
	calls    atomic.Int64
	lastAuth string
	lastBody tokenUnwrapRequest
}

func (s *unwrapStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.lastAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &s.lastBody)
		if s.failWith != 0 {
			w.WriteHeader(s.failWith)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenUnwrapResponse{Secret: s.secret})
	}
}

func TestUnwrapTokenSecret(t *testing.T) {
	stub := &unwrapStub{secret: testTokenKey}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	cfg := Config{
		tokenKeyWrapped:   "the-wrapped-blob",
		tokenKeyUnwrapURL: srv.URL,
		tokenKeyCred:      "test-credential",
	}
	secret, err := unwrapTokenSecret(context.Background(), cfg)
	if err != nil || secret != testTokenKey {
		t.Fatalf("unwrap = %q, %v; want the stub's secret", secret, err)
	}
	if stub.lastAuth != "Bearer test-credential" {
		t.Fatalf("authorization sent = %q; want the bearer credential", stub.lastAuth)
	}
	if stub.lastBody.Wrapped != "the-wrapped-blob" {
		t.Fatalf("wrapped sent = %q; want the configured blob", stub.lastBody.Wrapped)
	}

	stub.failWith = http.StatusBadGateway
	if _, err := unwrapTokenSecret(context.Background(), cfg); err == nil ||
		!strings.Contains(err.Error(), "502") {
		t.Fatalf("endpoint 502: err = %v", err)
	}
	stub.failWith = 0
	stub.secret = ""
	if _, err := unwrapTokenSecret(context.Background(), cfg); err == nil {
		t.Fatal("an empty secret was accepted")
	}
}

func TestTokenKeyUnlockLoop(t *testing.T) {
	oldStart, oldMax := tokenKeyRetryStart, tokenKeyRetryMax
	tokenKeyRetryStart, tokenKeyRetryMax = time.Millisecond, 2*time.Millisecond
	defer func() { tokenKeyRetryStart, tokenKeyRetryMax = oldStart, oldMax }()

	// Fails twice, then unlocks: the loop stops at the success.
	var attempts atomic.Int64
	runTokenKeyUnlockLoop(context.Background(), func(context.Context) bool {
		return attempts.Add(1) >= 3
	})
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d; want 3", got)
	}

	// A cancelled context ends the loop after the current attempt.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTokenKeyUnlockLoop(ctx, func(context.Context) bool { return false })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the unlock loop did not stop when its context ended")
	}
}

// TestTokenKeyUnlockEndToEnd drives one Application through the
// http provider's whole arc: endpoint down at boot (sealed rows
// report their key missing), endpoint up (the box unlocks with the
// fetched key, proven by sealing and opening), and the two answers
// that must never install — a short secret and a wrong key.
func TestTokenKeyUnlockEndToEnd(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	row := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

	// The stored pair, sealed under the real key.
	sealer := mustTokenBox(t, testTokenKey)
	sealedAccess, sealedRefresh, err := sealer.sealTokens(fixtureCharA, "fixture", "fixture")
	if err != nil {
		t.Fatalf("seal fixture tokens: %v", err)
	}
	if err := q.UpdateCharacterTokens(ctx, db.UpdateCharacterTokensParams{
		AccessToken:  sealedAccess,
		RefreshToken: sealedRefresh,
		TokenExpiry:  row.TokenExpiry,
		CharacterID:  fixtureCharA,
	}); err != nil {
		t.Fatalf("store sealed tokens: %v", err)
	}
	row, err = q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("read character back: %v", err)
	}

	// The app boots locked, pointing at an endpoint that is down
	// (a server that was closed before the app ever called it).
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	app.tokens = &tokenBox{}
	app.cfg.tokenKeyProvider = tokenKeyProviderHTTP
	app.cfg.tokenKeyWrapped = "wrapped-blob"
	app.cfg.tokenKeyCred = "test-credential"
	app.cfg.tokenKeyUnwrapURL = downURL

	if app.tryTokenKeyUnlock(ctx) {
		t.Fatal("unlock reported success with the endpoint down")
	}
	if _, _, err := app.tokens.openTokens(row); !errors.Is(err, errTokenKeyMissing) {
		t.Fatalf("sealed row while locked: err = %v; want errTokenKeyMissing", err)
	}

	// The endpoint appears, holding the right key.
	stub := &unwrapStub{secret: testTokenKey}
	up := httptest.NewServer(stub.handler(t))
	defer up.Close()
	app.cfg.tokenKeyUnwrapURL = up.URL
	if !app.tryTokenKeyUnlock(ctx) {
		t.Fatal("unlock failed with the endpoint up")
	}
	if stub.lastAuth != "Bearer test-credential" || stub.lastBody.Wrapped != "wrapped-blob" {
		t.Fatalf("unwrap call carried auth %q and wrapped %q", stub.lastAuth, stub.lastBody.Wrapped)
	}
	if !app.tokens.enabled() {
		t.Fatal("the box is still locked after a successful unlock")
	}
	if access, refresh, err := app.tokens.openTokens(row); err != nil || access != "fixture" || refresh != "fixture" {
		t.Fatalf("open after unlock = %q, %q, %v; want the fixture pair", access, refresh, err)
	}
	// Seal and open round-trip under the fetched key.
	sealed, err := app.tokens.seal("fresh-token", fixtureCharA, tokenFieldAccess)
	if err != nil || !tokenIsEncrypted(sealed) {
		t.Fatalf("seal after unlock = %q, %v", sealed, err)
	}
	if got, err := app.tokens.open(sealed, fixtureCharA, tokenFieldAccess); err != nil || got != "fresh-token" {
		t.Fatalf("round-trip after unlock = %q, %v", got, err)
	}

	// A short secret is rejected like a short env key, and a wrong
	// key is never installed over rows it cannot open.
	for name, secret := range map[string]string{"short": "short", "wrong key": testOtherTokenKey} {
		app.tokens = &tokenBox{}
		stub.secret = secret
		if app.tryTokenKeyUnlock(ctx) {
			t.Fatalf("%s: unlock reported success", name)
		}
		if app.tokens.enabled() {
			t.Fatalf("%s: an unusable key was installed", name)
		}
		if _, _, err := app.tokens.openTokens(row); !errors.Is(err, errTokenKeyMissing) {
			t.Fatalf("%s: sealed row err = %v; want errTokenKeyMissing", name, err)
		}
	}
}
