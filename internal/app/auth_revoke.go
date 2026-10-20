package app

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// revokeCharacterTokenAtCCP asks EVE SSO to revoke the character's
// refresh token (RFC 7009), so an unlink can fully disconnect the
// character at CCP instead of only deleting the local copy.
//
// Best-effort by design: it runs before the local delete, and every
// failure — the stored token will not open, there is no token, the
// request fails, CCP answers an error — is logged and swallowed.
// Revocation never blocks or fails an unlink. Token values are
// never logged; errors carry the character ID only.
func (app *Application) revokeCharacterTokenAtCCP(ctx context.Context, ch db.Character) {
	_, refresh, err := app.tokens.openTokens(ch)
	if err != nil {
		logging.Errorf("sso: unlink revoke for character %d: open stored tokens: %v", ch.CharacterID, err)
		return
	}
	if refresh == "" {
		logging.Infof("sso: unlink revoke for character %d skipped: no refresh token stored", ch.CharacterID)
		return
	}

	form := url.Values{
		"token":           {refresh},
		"token_type_hint": {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, app.cfg.EveRevokeURL(), strings.NewReader(form.Encode()))
	if err != nil {
		logging.Errorf("sso: unlink revoke for character %d: build request: %v", ch.CharacterID, err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(app.cfg.eveClientID, app.cfg.EVEClientSecret())

	resp, err := loginHTTPClient.Do(req)
	if err != nil {
		logging.Errorf("sso: unlink revoke for character %d: %v", ch.CharacterID, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		logging.Errorf("sso: unlink revoke for character %d: CCP answered %s", ch.CharacterID, resp.Status)
		return
	}
	logging.Infof("sso: revoked refresh token for character %d at unlink", ch.CharacterID)
}
