package app

// ---------------------------------------------------------------------------
// Sign-up policy. By default anyone who can reach the site and sign
// in with EVE gets an account, and from then on the worker syncs
// every character they link. EVE_ALLOWED_CHARACTER_IDS,
// EVE_ALLOWED_CORPORATION_IDS and EVE_ALLOWED_ALLIANCE_IDS limit
// that to the people the instance is meant for.
//
// The policy is checked at the one moment an account comes into
// being: a sign-in from a fresh session with a character that has
// no account to return to. It is deliberately not applied to:
//
//   - a returning character, whose account already exists;
//   - a character linked from inside a signed-in account, so a
//     member can add alts that are not themselves on the list.
//
// Accounts created before a list was set keep working; the lists
// decide who may join, not who is removed.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
)

var (
	// errSignUpNotAllowed: the instance limits who may sign up and
	// this character is not on any of its lists.
	errSignUpNotAllowed = errors.New("this character may not create an account on this instance")
	// errSignUpUnverified: the character's corporation and
	// alliance could not be looked up, so the lists could not be
	// checked. The sign-up is refused rather than waved through.
	errSignUpUnverified = errors.New("the character's corporation could not be checked")
)

// mayCreateAccount reports whether a sign-in by characterID may
// create a new account: nil when it may, errSignUpNotAllowed or
// errSignUpUnverified when it may not.
func (app *Application) mayCreateAccount(ctx context.Context, characterID int64) error {
	policy := app.cfg.signUp
	if !policy.restricted() {
		return nil
	}
	// The administrators the operator named are always welcome.
	if policy.characterIDs[characterID] || app.cfg.IsAdminCharacter(characterID) {
		return nil
	}
	if len(policy.corporationIDs) == 0 && len(policy.allianceIDs) == 0 {
		return errSignUpNotAllowed
	}

	// Public ESI: which corporation and alliance the character is
	// in right now. No token is needed.
	var affiliations []struct {
		CharacterID   int64 `json:"character_id"`
		CorporationID int64 `json:"corporation_id"`
		AllianceID    int64 `json:"alliance_id"`
	}
	if err := app.esi.PostJSON(ctx, "/characters/affiliation/", []int64{characterID}, &affiliations); err != nil {
		return fmt.Errorf("%w: %v", errSignUpUnverified, err)
	}
	for _, a := range affiliations {
		if a.CharacterID != characterID {
			continue
		}
		if policy.corporationIDs[a.CorporationID] || (a.AllianceID != 0 && policy.allianceIDs[a.AllianceID]) {
			return nil
		}
		return errSignUpNotAllowed
	}
	return fmt.Errorf("%w: the lookup returned nothing for character %d", errSignUpUnverified, characterID)
}
