package app

// Tests for the oci token key provider (auth_tokenkey.go over
// internal/ocikms): an Application pointed at fakes of the OCI
// metadata service, federation endpoint and KMS unlocks with the
// key KMS returns — and refuses a wrong one — while
// -wrap-token-key speaks the encrypt side of the same path.
// Every endpoint in these configs is an httptest server: nothing
// here can reach the real metadata address or Oracle.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// ociFakes is one fake OCI world: metadata, federation and KMS
// servers plus the values they deal in.
type ociFakes struct {
	secret      string // the root secret KMS decrypt returns
	wrapped     string // the ciphertext KMS encrypt returns
	lastEncrypt struct {
		KeyID     string `json:"keyId"`
		Plaintext string `json:"plaintext"`
	}
	mdURL, fedURL, kmsURL string
}

func buildOCIFakes(t *testing.T) *ociFakes {
	t.Helper()
	f := &ociFakes{secret: testTokenKey, wrapped: "d3JhcHBlZC1ieS1rbXM="}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(777),
		Subject: pkix.Name{
			CommonName: "ocid1.instance.oc1.phx.test",
			OrganizationalUnit: []string{
				"opc-tenant:ocid1.tenancy.oc1..aaaa",
				"opc-instance:ocid1.instance.oc1.phx.test",
				"opc-compartment:ocid1.tenancy.oc1..aaaa",
				"opc-certtype:instance",
			},
		},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	md := http.NewServeMux()
	md.HandleFunc("/opc/v2/identity/cert.pem", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(leafPEM) })
	md.HandleFunc("/opc/v2/identity/key.pem", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(keyPEM) })
	md.HandleFunc("/opc/v2/identity/intermediate.pem", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(leafPEM) })
	mdSrv := httptest.NewServer(md)
	t.Cleanup(mdSrv.Close)
	f.mdURL = mdSrv.URL

	b64 := base64.RawURLEncoding.EncodeToString
	token := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		b64([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix()))) + "." + b64([]byte("sig"))
	fed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	}))
	t.Cleanup(fed.Close)
	f.fedURL = fed.URL

	kms := http.NewServeMux()
	kms.HandleFunc("/20180608/decrypt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"plaintext": base64.StdEncoding.EncodeToString([]byte(f.secret)),
		})
	})
	kms.HandleFunc("/20180608/encrypt", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&f.lastEncrypt)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ciphertext": f.wrapped})
	})
	kmsSrv := httptest.NewServer(kms)
	t.Cleanup(kmsSrv.Close)
	f.kmsURL = kmsSrv.URL
	return f
}

// pointOCIAt aims a Config at the fakes.
func (f *ociFakes) pointOCIAt(cfg *Config) {
	cfg.tokenKeyProvider = tokenKeyProviderOCI
	cfg.tokenKeyWrapped = "dGhlLXdycHBlZC1ibG9i"
	cfg.tokenKeyOCIKeyID = "ocid1.key.oc1.phx.test"
	cfg.tokenKeyOCIEndpoint = f.kmsURL
	cfg.tokenKeyOCIMetadataURL = f.mdURL
	cfg.tokenKeyOCIFederationURL = f.fedURL
}

// TestTokenKeyOCIEndToEnd drives one Application through the oci
// provider's arc: KMS answers with the right key (the box unlocks
// and the stored tokens open), then with a wrong one (never
// installed; sealed rows still report their key missing).
func TestTokenKeyOCIEndToEnd(t *testing.T) {
	fakes := buildOCIFakes(t)
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	row := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")

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

	app.tokens = &tokenBox{}
	fakes.pointOCIAt(&app.cfg)
	if !app.tryTokenKeyUnlock(ctx) {
		t.Fatal("unlock failed against the fake KMS holding the right key")
	}
	if access, refresh, err := app.tokens.openTokens(row); err != nil || access != "fixture" || refresh != "fixture" {
		t.Fatalf("open after unlock = %q, %q, %v; want the fixture pair", access, refresh, err)
	}

	// A wrong key from KMS is never installed.
	app.tokens = &tokenBox{}
	fakes.secret = testOtherTokenKey
	if app.tryTokenKeyUnlock(ctx) {
		t.Fatal("unlock reported success with the wrong key")
	}
	if app.tokens.enabled() {
		t.Fatal("the wrong key was installed")
	}
	if _, _, err := app.tokens.openTokens(row); !errors.Is(err, errTokenKeyMissing) {
		t.Fatalf("sealed row after refused key: err = %v; want errTokenKeyMissing", err)
	}
}

// TestRunWrapTokenKey exercises the -wrap-token-key command
// against the fakes: stdout carries exactly the ciphertext, the
// plaintext reaches KMS only as the encrypt call's base64 field,
// and misconfigurations fail with exit 2 naming the problem.
func TestRunWrapTokenKey(t *testing.T) {
	fakes := buildOCIFakes(t)
	cfg := Config{tokenKey: testTokenKey}
	fakes.pointOCIAt(&cfg)

	var out, errBuf bytes.Buffer
	if code := RunWrapTokenKey(nil, cfg, &out, &errBuf); code != 0 {
		t.Fatalf("wrap exit = %d, stderr = %q", code, errBuf.String())
	}
	if out.String() != fakes.wrapped+"\n" {
		t.Fatalf("stdout = %q; want exactly the ciphertext and a newline", out.String())
	}
	if strings.Contains(out.String(), testTokenKey) || strings.Contains(errBuf.String(), testTokenKey) {
		t.Fatal("the plaintext leaked into the command's output")
	}
	raw, err := base64.StdEncoding.DecodeString(fakes.lastEncrypt.Plaintext)
	if err != nil || string(raw) != testTokenKey {
		t.Fatalf("encrypt plaintext field decoded = %q, %v; want the token key", raw, err)
	}
	if fakes.lastEncrypt.KeyID != "ocid1.key.oc1.phx.test" {
		t.Fatalf("encrypt keyId = %q", fakes.lastEncrypt.KeyID)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		args   []string
		wantIn string
	}{
		{"wrong provider", func(c *Config) { c.tokenKeyProvider = "http" }, nil, "not oci"},
		{"no key id", func(c *Config) { c.tokenKeyOCIKeyID = "" }, nil, "TOKEN_KEY_OCI_KEY_ID"},
		{"no endpoint", func(c *Config) { c.tokenKeyOCIEndpoint = "" }, nil, "TOKEN_KEY_OCI_CRYPTO_ENDPOINT"},
		{"nothing to wrap", func(c *Config) { c.tokenKey = "" }, nil, "TOKEN_ENCRYPTION_KEY"},
		{"short key", func(c *Config) { c.tokenKey = "short" }, nil, "TOKEN_ENCRYPTION_KEY"},
		{"extra argument", func(c *Config) {}, []string{"extra"}, "no arguments"},
	}
	for _, tc := range cases {
		broken := cfg
		tc.mutate(&broken)
		out.Reset()
		errBuf.Reset()
		if code := RunWrapTokenKey(tc.args, broken, &out, &errBuf); code != 2 {
			t.Fatalf("%s: exit = %d; want 2 (stdout %q)", tc.name, code, out.String())
		}
		if !strings.Contains(errBuf.String(), tc.wantIn) {
			t.Fatalf("%s: stderr = %q; want it to name %q", tc.name, errBuf.String(), tc.wantIn)
		}
		if out.Len() != 0 {
			t.Fatalf("%s: stdout = %q; want nothing on failure", tc.name, out.String())
		}
	}
}
