package app

import (
	"context"
	"database/sql"
	"errors"
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
	TokenExpiry  sql.NullString
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
		result.OwnerChanged = existing.OwnerHash != "" && in.OwnerHash != "" && existing.OwnerHash != in.OwnerHash
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

	state := linkStateOK
	var stateAt sql.NullString
	if result.OwnerChanged {
		state = linkStateOwnerChanged
		stateAt = sql.NullString{String: time.Now().UTC().Format(time.RFC3339), Valid: true}
	}

	// CachedUntil stays NULL: ESI caching is per-endpoint, while
	// this column models the (single) character-sheet cache; the
	// worker's snapshot rows carry the real expiries.
	if _, err := app.queries.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID:  in.CharacterID,
		UserID:       in.UserID,
		Name:         in.Name,
		AccessToken:  in.AccessToken,
		RefreshToken: in.RefreshToken,
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
	now := time.Now().UTC().Format(time.RFC3339)
	if err := app.queries.SetCharacterLinkState(ctx, db.SetCharacterLinkStateParams{
		LinkState:   linkStateTokenDead,
		LinkStateAt: sql.NullString{String: now, Valid: true},
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
