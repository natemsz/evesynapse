package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"golang.org/x/oauth2"

	db "evesynapse/internal/db/sqlc"
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
// Refreshes are serialized process-wide for the same reason.
//
// Token values are never logged; errors carry the character ID only.
func (app *Application) validAccessToken(ctx context.Context, ch db.Character) (string, error) {
	app.tokenMu.Lock()
	defer app.tokenMu.Unlock()

	// Re-read: the row we were handed may predate a recent refresh.
	if fresh, err := app.queries.GetCharacter(ctx, ch.CharacterID); err == nil {
		ch = fresh
	}

	if expiry, ok := parseTokenExpiry(ch.TokenExpiry); ok && time.Until(expiry) > tokenRefreshWindow {
		return ch.AccessToken, nil
	}
	if ch.RefreshToken == "" {
		return "", fmt.Errorf("character %d: no refresh token stored", ch.CharacterID)
	}

	// Force the refresh by presenting an already-expired token; CCP
	// answers with a fresh access token and a rotated refresh token.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, loginHTTPClient)
	stale := &oauth2.Token{
		RefreshToken: ch.RefreshToken,
		Expiry:       time.Now().Add(-time.Hour),
	}
	tok, err := eveOAuthConfig(app.cfg).TokenSource(ctx, stale).Token()
	if err != nil {
		return "", fmt.Errorf("character %d: token refresh failed: %w", ch.CharacterID, err)
	}

	expiry := ""
	if !tok.Expiry.IsZero() {
		expiry = tok.Expiry.UTC().Format(time.RFC3339)
	}
	refreshToken := tok.RefreshToken
	if refreshToken == "" {
		// CCP always rotates, but never store an empty token over a
		// good one if a response ever omits it.
		refreshToken = ch.RefreshToken
	}
	if err := app.queries.UpdateCharacterTokens(ctx, db.UpdateCharacterTokensParams{
		AccessToken:  tok.AccessToken,
		RefreshToken: refreshToken,
		TokenExpiry:  sql.NullString{String: expiry, Valid: expiry != ""},
		CharacterID:  ch.CharacterID,
	}); err != nil {
		return "", fmt.Errorf("character %d: persist refreshed tokens: %w", ch.CharacterID, err)
	}
	log.Printf("sso: refreshed access token for character %d", ch.CharacterID)
	return tok.AccessToken, nil
}

// parseTokenExpiry decodes the stored RFC3339 token_expiry, reporting
// false when it is absent or malformed (treat as expired).
func parseTokenExpiry(v sql.NullString) (time.Time, bool) {
	if !v.Valid || v.String == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v.String)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
