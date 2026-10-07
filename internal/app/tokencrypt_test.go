package app

// Tests for token encryption at rest (tokencrypt.go): the box
// itself, the sign-in and refresh paths storing sealed tokens while
// handing usable ones to callers, and the boot pass that encrypts
// tokens stored before a key was set and refuses a key that does
// not open what is stored.

import (
	"context"
	"database/sql"
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
	if !tokenIsEncrypted(sealed) || strings.Contains(sealed, token) {
		t.Fatalf("sealed form %q is not an encrypted value", sealed)
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
		TokenExpiry:  sql.NullString{String: "2999-01-01T00:00:00Z", Valid: true},
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
		TokenExpiry:  sql.NullString{String: "2000-01-01T00:00:00Z", Valid: true},
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
