package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwtTestApp builds the sliver of Application that verifies EVE
// tokens: the client_id the audience must name, and a JWKS cache
// holding the test key (no network).
func jwtTestApp(t *testing.T, clientID string) (*Application, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := &Application{
		cfg:  Config{eveClientID: clientID},
		jwks: &jwksCache{keys: map[string]*rsa.PublicKey{"test-kid": &key.PublicKey}},
	}
	return app, key
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test-kid"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func eveClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   eveIssuer,
		"sub":   "CHARACTER:EVE:90000001",
		"name":  "Fixture Alpha",
		"owner": "owner-hash",
		"scp":   []string{"esi-assets.read_assets.v1"},
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"aud":   []string{"test-client-id", "EVE Online"},
	}
}

// TestVerifyAccessTokenAcceptsEVEToken: a token shaped like CCP's
// (signature, issuer, expiry, audience for this client) verifies
// and yields its character.
func TestVerifyAccessTokenAcceptsEVEToken(t *testing.T) {
	app, key := jwtTestApp(t, "test-client-id")
	charID, name, scopes, owner, err := app.verifyAccessToken(context.Background(), signTestToken(t, key, eveClaims()))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if charID != 90000001 || name != "Fixture Alpha" || owner != "owner-hash" {
		t.Fatalf("verify = %d %q %q", charID, name, owner)
	}
	if scopes != "esi-assets.read_assets.v1" {
		t.Fatalf("scopes = %q", scopes)
	}
}

// TestVerifyAccessTokenChecks: issuer (both forms CCP sends),
// expiry, and audience each gate the token.
func TestVerifyAccessTokenChecks(t *testing.T) {
	app, key := jwtTestApp(t, "test-client-id")
	cases := map[string]struct {
		mutate func(jwt.MapClaims)
		ok     bool
	}{
		"bare-host issuer":   {func(c jwt.MapClaims) { c["iss"] = eveIssuerHost }, true},
		"wrong issuer":       {func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" }, false},
		"expired":            {func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, false},
		"missing expiry":     {func(c jwt.MapClaims) { delete(c, "exp") }, false},
		"missing audience":   {func(c jwt.MapClaims) { delete(c, "aud") }, false},
		"another client":     {func(c jwt.MapClaims) { c["aud"] = []string{"someone-else", "EVE Online"} }, false},
		"missing EVE Online": {func(c jwt.MapClaims) { c["aud"] = []string{"test-client-id"} }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			claims := eveClaims()
			tc.mutate(claims)
			_, _, _, _, err := app.verifyAccessToken(context.Background(), signTestToken(t, key, claims))
			if tc.ok && err != nil {
				t.Fatalf("verify: %v, want success", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("verify succeeded, want rejection")
			}
		})
	}
}
