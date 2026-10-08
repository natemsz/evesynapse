package app

// Tests for token encryption at rest (tokencrypt.go): the box
// itself, the sign-in and refresh paths storing sealed tokens while
// handing usable ones to callers, and the boot pass that encrypts
// tokens stored before a key was set and refuses a key that does
// not open what is stored.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

const (
	testTokenKey      = "0123456789abcdef0123456789abcdef0123456789abcdef"
	testOtherTokenKey = "fedcba9876543210fedcba9876543210fedcba9876543210"
)

func mustTokenBox(t *testing.T, key string) *tokenBox {
	t.Helper()
	box, err := newTokenBox(key)
	if err != nil {
		t.Fatalf("newTokenBox: %v", err)
	}
	return box
}

func TestTokenBoxSealAndOpen(t *testing.T) {
	box := mustTokenBox(t, testTokenKey)
	const token = "a-refresh-token-value"

	sealed, err := box.seal(token, 42, tokenFieldRefresh)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// New seals wear the current envelope.
	if !strings.HasPrefix(sealed, tokenCipherV2Prefix) || strings.Contains(sealed, token) {
		t.Fatalf("sealed form %q is not a v2 encrypted value", sealed)
	}
	if !tokenIsEncrypted(sealed) {
		t.Fatalf("sealed form %q is not recognized as encrypted", sealed)
	}
	// A fresh nonce every time: the same token never stores the
	// same way twice.
	again, err := box.seal(token, 42, tokenFieldRefresh)
	if err != nil {
		t.Fatalf("seal again: %v", err)
	}
	if again == sealed {
		t.Fatal("two seals of the same token produced the same ciphertext")
	}

	if got, err := box.open(sealed, 42, tokenFieldRefresh); err != nil || got != token {
		t.Fatalf("open = %q, %v; want the token back", got, err)
	}

	// Bound to its row and column: it opens nowhere else.
	if _, err := box.open(sealed, 43, tokenFieldRefresh); !errors.Is(err, errTokenUnreadable) {
		t.Fatalf("open on another character: err = %v, want errTokenUnreadable", err)
	}
	if _, err := box.open(sealed, 42, tokenFieldAccess); !errors.Is(err, errTokenUnreadable) {
		t.Fatalf("open as the other column: err = %v, want errTokenUnreadable", err)
	}
	// Another key, no key, a damaged value.
	if _, err := mustTokenBox(t, testOtherTokenKey).open(sealed, 42, tokenFieldRefresh); !errors.Is(err, errTokenUnreadable) {
		t.Fatalf("open with another key: err = %v, want errTokenUnreadable", err)
	}
	if _, err := mustTokenBox(t, "").open(sealed, 42, tokenFieldRefresh); !errors.Is(err, errTokenKeyMissing) {
		t.Fatalf("open with no key: err = %v, want errTokenKeyMissing", err)
	}
	if _, err := box.open(sealed[:len(sealed)-4]+"AAAA", 42, tokenFieldRefresh); !errors.Is(err, errTokenUnreadable) {
		t.Fatalf("open a damaged value: err = %v, want errTokenUnreadable", err)
	}
}

func TestTokenBoxWithoutAKey(t *testing.T) {
	// No key (and no box at all, as in the test fixtures): tokens
	// pass through untouched in both directions.
	var none *tokenBox
	for name, box := range map[string]*tokenBox{"nil": none, "empty key": mustTokenBox(t, "  ")} {
		if box.enabled() {
			t.Fatalf("%s: box reports a key", name)
		}
		sealed, err := box.seal("plain-token", 7, tokenFieldAccess)
		if err != nil || sealed != "plain-token" {
			t.Fatalf("%s: seal = %q, %v; want the token unchanged", name, sealed, err)
		}
		if got, err := box.open("plain-token", 7, tokenFieldAccess); err != nil || got != "plain-token" {
			t.Fatalf("%s: open = %q, %v; want the token unchanged", name, got, err)
		}
	}

	// With a key, a token stored before it was set still reads, and
	// an empty token stays empty rather than becoming ciphertext.
	box := mustTokenBox(t, testTokenKey)
	if got, err := box.open("stored-before-the-key", 7, tokenFieldAccess); err != nil || got != "stored-before-the-key" {
		t.Fatalf("open a pre-key token = %q, %v", got, err)
	}
	if sealed, err := box.seal("", 7, tokenFieldAccess); err != nil || sealed != "" {
		t.Fatalf("seal of an empty token = %q, %v; want empty", sealed, err)
	}

	if _, err := newTokenBox("too-short"); err == nil {
		t.Fatal("a 9-character key was accepted")
	}
}

// tokenEndpointStub stands in for CCP's token endpoint: it records
// the refresh token it was presented and answers with a new pair.
type tokenEndpointStub struct {
	mu        sync.Mutex
	presented string
	calls     int
}

func (s *tokenEndpointStub) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	s.mu.Lock()
	s.presented = form.Get("refresh_token")
	s.calls++
	s.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"access-two","token_type":"Bearer","expires_in":1200,"refresh_token":"refresh-two"}`)),
		Request:    req,
	}, nil
}

// TestTokensAreStoredSealedAndUsedOpened follows a character
// through sign-in, a use of its fresh access token, and a refresh:
// the database only ever holds sealed values, callers only ever
// get usable ones, and CCP is presented the real refresh token.
func TestTokensAreStoredSealedAndUsedOpened(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	app.tokens = mustTokenBox(t, testTokenKey)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
		UserID:       user.ID,
		CharacterID:  fixtureCharA,
		Name:         "Fixture Alpha",
		Scopes:       "esi-skills.read_skills.v1",
		OwnerHash:    "hash-one",
		AccessToken:  "access-one",
		RefreshToken: "refresh-one",
		TokenExpiry:  mustNullTime("2999-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	row, err := q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("get character: %v", err)
	}
	if !tokenIsEncrypted(row.AccessToken) || !tokenIsEncrypted(row.RefreshToken) {
		t.Fatalf("sign-in stored access %q / refresh %q; want both sealed", row.AccessToken, row.RefreshToken)
	}

	// Still fresh: the access token comes back opened, CCP is not
	// asked.
	stub := &tokenEndpointStub{}
	old := loginHTTPClient
	loginHTTPClient = &http.Client{Transport: stub}
	t.Cleanup(func() { loginHTTPClient = old })

	got, err := app.validAccessToken(ctx, row)
	if err != nil || got != "access-one" {
		t.Fatalf("fresh token: got %q, %v; want access-one", got, err)
	}
	if stub.calls != 0 {
		t.Fatalf("a fresh token made %d call(s) to the token endpoint", stub.calls)
	}

	// Expired: the refresh presents the opened refresh token and
	// stores the rotated pair sealed.
	if err := q.UpdateCharacterTokens(ctx, db.UpdateCharacterTokensParams{
		AccessToken:  row.AccessToken,
		RefreshToken: row.RefreshToken,
		TokenExpiry:  mustNullTime("2000-01-01T00:00:00Z"),
		CharacterID:  fixtureCharA,
	}); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	got, err = app.validAccessToken(ctx, row)
	if err != nil || got != "access-two" {
		t.Fatalf("refreshed token: got %q, %v; want access-two", got, err)
	}
	if stub.presented != "refresh-one" {
		t.Fatalf("CCP was presented refresh token %q, want the opened refresh-one", stub.presented)
	}
	row, err = q.GetCharacter(ctx, fixtureCharA)
	if err != nil {
		t.Fatalf("get character after refresh: %v", err)
	}
	if !tokenIsEncrypted(row.AccessToken) || !tokenIsEncrypted(row.RefreshToken) {
		t.Fatalf("refresh stored access %q / refresh %q; want both sealed", row.AccessToken, row.RefreshToken)
	}
	access, refresh, err := app.tokens.openTokens(row)
	if err != nil || access != "access-two" || refresh != "refresh-two" {
		t.Fatalf("stored pair opens to %q / %q, %v; want the rotated pair", access, refresh, err)
	}
}

// TestPrepareStoredTokens covers the boot pass: tokens stored
// before a key existed are encrypted in place once one is set, the
// pass changes nothing the second time, and a key that does not
// open what is stored (changed, or removed) stops the start
// instead of running on.
func TestPrepareStoredTokens(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha") // plain "fixture" tokens

	// No key: nothing changes.
	if err := app.prepareStoredTokens(ctx); err != nil {
		t.Fatalf("prepare without a key: %v", err)
	}
	row, _ := q.GetCharacter(ctx, fixtureCharA)
	if row.AccessToken != "fixture" || row.RefreshToken != "fixture" {
		t.Fatalf("without a key the tokens became %q / %q", row.AccessToken, row.RefreshToken)
	}

	// A key is set: the stored tokens are encrypted in place and
	// still open to what they were.
	app.tokens = mustTokenBox(t, testTokenKey)
	if err := app.prepareStoredTokens(ctx); err != nil {
		t.Fatalf("prepare with a key: %v", err)
	}
	row, _ = q.GetCharacter(ctx, fixtureCharA)
	if !tokenIsEncrypted(row.AccessToken) || !tokenIsEncrypted(row.RefreshToken) {
		t.Fatalf("with a key the tokens are %q / %q; want both sealed", row.AccessToken, row.RefreshToken)
	}
	if got, err := app.validAccessToken(ctx, row); err != nil || got != "fixture" {
		t.Fatalf("encrypted-in-place token: got %q, %v; want fixture", got, err)
	}

	// Already encrypted: a second pass leaves the row alone.
	if err := app.prepareStoredTokens(ctx); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	again, _ := q.GetCharacter(ctx, fixtureCharA)
	if again.AccessToken != row.AccessToken || again.RefreshToken != row.RefreshToken {
		t.Fatal("a second pass re-encrypted tokens that were already sealed")
	}

	// A different key, then no key: both refuse, and say why.
	for name, box := range map[string]*tokenBox{
		"changed key": mustTokenBox(t, testOtherTokenKey),
		"removed key": mustTokenBox(t, ""),
	} {
		app.tokens = box
		err := app.prepareStoredTokens(ctx)
		if err == nil {
			t.Fatalf("%s: prepare succeeded over tokens it cannot open", name)
		}
		if !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") {
			t.Fatalf("%s: error %q does not point at TOKEN_ENCRYPTION_KEY", name, err)
		}
	}
}

// sealV1TestOnly replays the retired v1 envelope (AES-GCM under one
// SHA-256 of the secret) so the compatibility path has something
// old to open. It mirrors the pre-HKDF seal exactly: same label,
// same AAD, same v1 prefix.
func sealV1TestOnly(t *testing.T, secret, plain string, characterID int64, field string) string {
	t.Helper()
	key := sha256.Sum256([]byte("evesynapse/token-encryption/v1\x00" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plain), tokenAAD(characterID, field))
	return tokenCipherPrefix + base64.RawStdEncoding.EncodeToString(sealed)
}

// TestTokenBoxOpensV1: rows sealed before the HKDF switch still
// open with the same key, and the boot pass re-seals them as v2.
func TestTokenBoxOpensV1(t *testing.T) {
	box := mustTokenBox(t, testTokenKey)
	old := sealV1TestOnly(t, testTokenKey, "old-refresh-token", 42, tokenFieldRefresh)
	if !tokenIsEncrypted(old) || tokenSealedCurrent(old) {
		t.Fatalf("v1 value %q is not recognized as sealed-but-retired", old)
	}
	plain, err := box.open(old, 42, tokenFieldRefresh)
	if err != nil || plain != "old-refresh-token" {
		t.Fatalf("v1 value opens to %q, %v; want old-refresh-token", plain, err)
	}
	// The wrong character still does not open it.
	if _, err := box.open(old, 43, tokenFieldRefresh); err == nil {
		t.Fatal("v1 value opened under another character ID")
	}
}

// TestPrepareStoredTokensUpgradesV1: a v1-sealed row comes out of
// the boot pass as v2, opening to the same tokens.
func TestPrepareStoredTokensUpgradesV1(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	app.tokens = mustTokenBox(t, testTokenKey)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	oldAccess := sealV1TestOnly(t, testTokenKey, "access-one", fixtureCharA, tokenFieldAccess)
	oldRefresh := sealV1TestOnly(t, testTokenKey, "refresh-one", fixtureCharA, tokenFieldRefresh)
	if err := q.UpdateCharacterTokens(ctx, db.UpdateCharacterTokensParams{
		AccessToken:  oldAccess,
		RefreshToken: oldRefresh,
		TokenExpiry:  mustNullTime("2999-01-01T00:00:00Z"),
		CharacterID:  fixtureCharA,
	}); err != nil {
		t.Fatalf("store v1 tokens: %v", err)
	}

	if err := app.prepareStoredTokens(ctx); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	row, _ := q.GetCharacter(ctx, fixtureCharA)
	if !tokenSealedCurrent(row.AccessToken) || !tokenSealedCurrent(row.RefreshToken) {
		t.Fatalf("after prepare the tokens are %q / %q; want both v2", row.AccessToken, row.RefreshToken)
	}
	access, refresh, err := app.tokens.openTokens(row)
	if err != nil || access != "access-one" || refresh != "refresh-one" {
		t.Fatalf("upgraded pair opens to %q / %q, %v", access, refresh, err)
	}
}

// TestTokenV2KeyPinned: a v2 value sealed once, by hand, still opens.
// The HKDF salt and label are part of the stored format; changing
// either (or the hash) would strand every v2 row, and this fails
// first. Do not regenerate the constant to make the test pass.
func TestTokenV2KeyPinned(t *testing.T) {
	const pinned = "enc:v2:9piEryQnw3NC+cL8Mdf+lPoloWQSfbP4p3DzyZoSUmODVGM/TMb9KQf2H/SMnYp3"
	box := mustTokenBox(t, testTokenKey)
	got, err := box.open(pinned, 42, tokenFieldRefresh)
	if err != nil || got != "pinned-refresh-token" {
		t.Fatalf("pinned v2 value opens to %q, %v; want pinned-refresh-token", got, err)
	}
	if _, err := box.open(pinned, 43, tokenFieldRefresh); err == nil {
		t.Fatal("pinned v2 value opened under another character ID")
	}
}
