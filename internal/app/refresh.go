package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// tokenRefreshWindow: refresh the access token when it expires within
// this window (or already has). EVE SSO access tokens live ~20 minutes.
const tokenRefreshWindow = 60 * time.Second

// validAccessToken returns an access token for the character that is
// safe to use right now. The stored token is returned as-is when it
// has more than tokenRefreshWindow left; otherwise it is refreshed
// against CCP and the new token set is persisted (CCP rotates refresh
// tokens on every refresh, so the returned refresh token always
// replaces the stored one).
//
// The character row is re-read first: another caller may have just
// refreshed, and reusing a rotated-away refresh token would fail.
// Refreshes of one character's token are serialized for the same
// reason; different characters refresh side by side.
//
// Token values are never logged; errors carry the character ID only.
func (app *Application) validAccessToken(ctx context.Context, ch db.Character) (string, error) {
	defer app.tokenLocks.lock(ch.CharacterID)()

	// Re-read: the row we were handed may predate a recent refresh.
	if fresh, err := app.queries.GetCharacter(ctx, ch.CharacterID); err == nil {
		ch = fresh
	}

	// The stored pair is sealed when a TOKEN_ENCRYPTION_KEY is
	// configured (tokencrypt.go). A pair that does not open is a
	// configuration problem, not a dead login: it is reported and
	// retried, never parked.
	accessToken, storedRefresh, err := app.tokens.openTokens(ch)
	if err != nil {
		return "", fmt.Errorf("character %d: %w", ch.CharacterID, err)
	}

	if ch.TokenExpiry.Valid && time.Until(ch.TokenExpiry.Time) > tokenRefreshWindow {
		return accessToken, nil
	}
	if storedRefresh == "" {
		return "", fmt.Errorf("character %d: no refresh token stored", ch.CharacterID)
	}

	// Force the refresh by presenting an already-expired token; CCP
	// answers with a fresh access token and a rotated refresh token.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, loginHTTPClient)
	stale := &oauth2.Token{
		RefreshToken: storedRefresh,
		Expiry:       time.Now().Add(-time.Hour),
	}
	tok, err := eveOAuthConfig(app.cfg).TokenSource(ctx, stale).Token()
	if err != nil {
		// A definitive rejection (revoked/expired refresh token)
		// parks the character as token-dead instead of failing
		// the same way every worker cycle; transient failures
		// just retry next time.
		if isDefinitiveTokenFailure(err) {
			app.markCharacterTokenDead(ctx, ch.CharacterID)
		}
		return "", fmt.Errorf("character %d: token refresh failed: %w", ch.CharacterID, err)
	}

	refreshToken := tok.RefreshToken
	if refreshToken == "" {
		// CCP always rotates, but never store an empty token over a
		// good one if a response ever omits it.
		refreshToken = storedRefresh
	}
	sealedAccess, sealedRefresh, err := app.tokens.sealTokens(ch.CharacterID, tok.AccessToken, refreshToken)
	if err != nil {
		return "", fmt.Errorf("character %d: seal refreshed tokens: %w", ch.CharacterID, err)
	}
	if err := app.queries.UpdateCharacterTokens(ctx, db.UpdateCharacterTokensParams{
		AccessToken:  sealedAccess,
		RefreshToken: sealedRefresh,
		TokenExpiry:  sql.NullTime{Time: tok.Expiry.UTC(), Valid: !tok.Expiry.IsZero()},
		CharacterID:  ch.CharacterID,
	}); err != nil {
		return "", fmt.Errorf("character %d: persist refreshed tokens: %w", ch.CharacterID, err)
	}
	logging.Infof("sso: refreshed access token for character %d", ch.CharacterID)
	return tok.AccessToken, nil
}

// isDefinitiveTokenFailure reports whether a token-refresh or ESI
// failure proves the character's credentials are dead rather than
// the network being flaky: CCP's token endpoint answering
// invalid_grant (revoked/expired refresh token) or an ESI 401 for a
// token we believed valid. Only definitive failures park a
// character as token-dead; everything else retries next cycle.
func isDefinitiveTokenFailure(err error) bool {
	var rerr *oauth2.RetrieveError
	if errors.As(err, &rerr) {
		switch rerr.ErrorCode {
		case "invalid_grant", "invalid_token":
			return true
		}
	}
	if code, ok := esi.StatusCode(err); ok && code == http.StatusUnauthorized {
		return true
	}
	return false
}
