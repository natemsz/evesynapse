package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// Character link states (characters.link_state): the worker-facing
// health of a linked character. Anything but ok stops syncing
// until the user signs the character in again — a fresh verified
// SSO login always lands back on ok (or owner_changed, below).
const (
	linkStateOK           = "ok"
	linkStateTokenDead    = "token_dead"
	linkStateOwnerChanged = "owner_changed"
)

// linkCharacterInput is one verified EVE SSO sign-in to persist:
// the verified identity (JWT claims) plus the fresh token set.
// Token values are never logged.
type linkCharacterInput struct {
	UserID       int64
	CharacterID  int64
	Name         string
	Scopes       string
	OwnerHash    string // JWT `owner` claim; "" when CCP sent none
	AccessToken  string
	RefreshToken string
	TokenExpiry  sql.NullTime
}

// linkResult reports the policy decisions linkVerifiedCharacter
// took, so the caller can log them.
type linkResult struct {
	// Moved: the character was linked to a different account and
	// moved to this one (a fresh SSO sign-in proves control, so
	// the newest link wins).
	Moved bool
	// OwnerChanged: the character's EVE owner hash differs from
	// the one stored when the link was verified (the character
	// changed EVE accounts). The link is flagged owner_changed
	// and syncing stops until a fresh sign-in with the now-stored
	// hash re-verifies control.
	OwnerChanged bool
}

// linkVerifiedCharacter persists a verified SSO sign-in: it creates
// the character row, attaches it to the signing-in account
// (moving it from another account when it was linked there), and
// applies the owner-hash policy. It is the single write path for
// sign-ins, factored out of the callback so the policy is testable
// without a live JWT.
func (app *Application) linkVerifiedCharacter(ctx context.Context, in linkCharacterInput) (linkResult, error) {
	var result linkResult

	existing, err := app.queries.GetCharacter(ctx, in.CharacterID)
	switch {
	case err == nil:
		result.Moved = existing.UserID != in.UserID
		result.OwnerChanged = ownerHashChanged(existing.OwnerHash, in.OwnerHash)
	case errors.Is(err, sql.ErrNoRows):
		// First time this character has ever signed in.
	default:
		return result, err
	}

	// Never store an empty owner hash over a known one: a token
	// without the claim (shouldn't happen) must not erase the
	// transfer detection baseline.
	ownerHash := in.OwnerHash
	if ownerHash == "" && err == nil {
		ownerHash = existing.OwnerHash
	}

	// An owner change on a character that stays on its account is
	// flagged and parked until a second sign-in confirms it. One
	// that is moving to another account at the same moment needs no
	// second look: the new owner has just proven control and the
	// character has left the account it could not be trusted on.
	state := linkStateOK
	var stateAt sql.NullTime
	if result.OwnerChanged && !result.Moved {
		state = linkStateOwnerChanged
		stateAt = timeSet(time.Now().UTC())
	}

	// The tokens are stored sealed when a TOKEN_ENCRYPTION_KEY is
	// configured (auth_tokencrypt.go), as they arrived otherwise.
	accessToken, refreshToken, err := app.tokens.sealTokens(in.CharacterID, in.AccessToken, in.RefreshToken)
	if err != nil {
		return result, err
	}

	// CachedUntil stays NULL: ESI caching is per-endpoint, while
	// this column models the (single) character-sheet cache; the
	// worker's snapshot rows carry the real expiries.
	if _, err := app.queries.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID:  in.CharacterID,
		UserID:       in.UserID,
		Name:         in.Name,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenExpiry:  in.TokenExpiry,
		Scopes:       in.Scopes,
		OwnerHash:    ownerHash,
		LinkState:    state,
		LinkStateAt:  stateAt,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// ownerHashChanged reports whether a verified sign-in's owner hash
// proves the character sits on a different EVE account than the
// one it was linked from. Either side being unknown ("") proves
// nothing.
func ownerHashChanged(stored, verified string) bool {
	return stored != "" && verified != "" && stored != verified
}

// resolveSignInUser decides which EveSynapse account a verified SSO
// sign-in lands on. A session that already holds an account keeps it
// ("link another character" reuses the same path). A fresh session
// adopts the account the character is already linked to: an expired
// session must never split a returning user's pilots onto a
// brand-new account that holds only the character they happened to
// sign in with. A character that has never signed in creates a new
// account.
//
// The one exception to adopting is a character that changed EVE
// accounts since it was linked (ownerHash is the sign-in's verified
// owner hash). Whoever signs it in now is not the person who built
// the account it sits on, and adopting would hand that person the
// previous owner's whole account — every other character linked to
// it, their data and their tokens. The new owner gets a fresh
// account instead, and linkVerifiedCharacter moves the character
// onto it.
func (app *Application) resolveSignInUser(ctx context.Context, sessionUserID, characterID int64, ownerHash string) (int64, error) {
	if sessionUserID != 0 {
		switch _, err := app.queries.GetUser(ctx, sessionUserID); {
		case err == nil:
			return sessionUserID, nil
		case !errors.Is(err, sql.ErrNoRows):
			return 0, err
		}
		// The session's account has been removed: as a fresh session.
	}
	existing, err := app.queries.GetCharacter(ctx, characterID)
	switch {
	case err == nil && !ownerHashChanged(existing.OwnerHash, ownerHash):
		return existing.UserID, nil
	case err == nil || errors.Is(err, sql.ErrNoRows):
		// The one place an account comes into being, and so the one
		// place the sign-up policy applies (auth_signup.go).
		if err := app.mayCreateAccount(ctx, characterID, ownerHash); err != nil {
			return 0, err
		}
		user, err := app.queries.CreateUser(ctx)
		if err != nil {
			return 0, err
		}
		return user.ID, nil
	default:
		return 0, err
	}
}

// markCharacterTokenDead flags a character token_dead: CCP has
// definitively rejected its refresh token, so syncing stops until
// the user signs the character in again. Only an ok link can go
// dead this way — an owner_changed flag is the stronger signal and
// is left alone. Best-effort: a failed write here only means the
// next cycle classifies the failure again.
func (app *Application) markCharacterTokenDead(ctx context.Context, characterID int64) {
	ch, err := app.queries.GetCharacter(ctx, characterID)
	if err != nil || ch.LinkState != linkStateOK {
		return
	}
	if err := app.queries.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateTokenDead,
		LinkStateAt: timeSet(time.Now().UTC()),
		CharacterID: characterID,
	}); err != nil {
		return
	}
}

// characterSyncs reports whether the worker may sync a character:
// only healthy (ok) links are fetched; token_dead and
// owner_changed characters wait for a fresh sign-in.
func characterSyncs(ch db.Character) bool {
	return ch.LinkState == "" || ch.LinkState == linkStateOK
}

// characterHasScope reports whether the character's granted scope
// set (characters.scopes, space-joined at sign-in) includes scope.
func characterHasScope(ch db.Character, scope string) bool {
	for _, s := range strings.Fields(ch.Scopes) {
		if s == scope {
			return true
		}
	}
	return false
}
