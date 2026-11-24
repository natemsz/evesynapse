package app

import (
	"slices"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/standing"
)

// TestFleetSignInDecidesWhoSignsUpAndSeesTheFormUp: a fleet that asks
// for its permissions takes only the characters that granted them and
// names the rest to be asked; a pilot with no account needs one first;
// a fleet that asks for an EVE sign-in keeps where it forms up from a
// signed-out visitor, and one that asks nothing shows it to anyone.
func TestFleetSignInDecidesWhoSignsUpAndSeesTheFormUp(t *testing.T) {
	op := db.Op{ID: 1, OwnerKind: ownerCommunity, OwnerID: 5, Visibility: fleetPublic, JoinGate: joinAnyone, SignIn: signInScopes}
	account := fleetViewer{UserID: 9, Standing: standing.New(), plans: map[string]bool{},
		Characters: []accountCharacter{{CharacterID: 1, Name: "Granted"}, {CharacterID: 2, Name: "Not yet"}},
		scopes:     map[int64]string{1: strings.Join(fleetPilotScopes, " "), 2: "esi-killmails.read_killmails.v1"}}
	a := account.access(op, fleetFacts{})
	if len(a.Signers) != 1 || a.Signers[0].CharacterID != 1 {
		t.Errorf("signers = %+v, want only the character that granted the fleet's permissions", a.Signers)
	}
	if len(a.NeedGrant) != 1 || a.NeedGrant[0].CharacterID != 2 {
		t.Errorf("to be asked = %+v, want the character missing a permission", a.NeedGrant)
	}

	pilot := fleetViewer{Standing: standing.New(), plans: map[string]bool{}, Pilot: &fleetPilot{CharacterID: 7, Name: "Pilot"}}
	if a := pilot.access(op, fleetFacts{}); a.Join || !a.NeedAccount {
		t.Errorf("a pilot with no account: join=%v needs account=%v, want no sign-up and an account first", a.Join, a.NeedAccount)
	}

	signedOut := fleetViewer{Standing: standing.New(), plans: map[string]bool{}}
	op.SignIn = signInEVE
	if signedOut.access(op, fleetFacts{}).SeeFormUp {
		t.Error("a fleet asking for an EVE sign-in shows where it forms up to a signed-out visitor")
	}
	if !pilot.access(op, fleetFacts{}).Join {
		t.Error("a pilot signed in with EVE cannot sign up to a fleet asking for an EVE sign-in")
	}
	op.SignIn = signInNone
	if !signedOut.access(op, fleetFacts{}).SeeFormUp {
		t.Error("a fleet asking nothing hides where it forms up")
	}

	// Granting asks for those scopes and no others, every one of them on
	// the developer portal's list, so the sign-in cannot fail on them.
	m, ok := relinkModule(fleetPilotModule)
	if !ok || len(m.Scopes) != len(fleetPilotScopes) {
		t.Fatalf("the fleet's permissions re-link = %+v", m)
	}
	for _, s := range m.Scopes {
		if !slices.Contains(eveScopes, s.Scope) {
			t.Errorf("the fleet's permissions ask for %q, which the portal list lacks", s.Scope)
		}
	}
}
