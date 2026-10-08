package app

import (
	"strings"

	db "evesynapse/internal/db/sqlc"
)

// moduleState says how much of a module a character's granted scopes
// light up. It is derived from the manifest and characters.scopes; it
// is not stored.
type moduleState string

const (
	// moduleEnabled: every required and optional scope is granted.
	moduleEnabled moduleState = "enabled"
	// moduleLimited: required scopes are granted, some optional ones are not.
	moduleLimited moduleState = "limited"
	// moduleLocked: at least one required scope is missing.
	moduleLocked moduleState = "locked"
)

// moduleStatus is a module's state for one character, with the scopes
// that explain it.
type moduleStatus struct {
	State           moduleState
	MissingRequired []scopeUse
	MissingOptional []scopeUse
}

// scopeSet parses a space-joined granted-scope string.
func scopeSet(granted string) map[string]bool {
	set := map[string]bool{}
	for _, s := range strings.Fields(granted) {
		set[s] = true
	}
	return set
}

// statusFor computes the module's state against a granted-scope set.
// Public and derived modules own no scopes and are always enabled;
// whether a derived module has anything to show depends on the modules
// it uses, which the caller resolves per character.
func (m moduleDef) statusFor(granted map[string]bool) moduleStatus {
	var st moduleStatus
	for _, s := range m.Scopes {
		if granted[s.Scope] {
			continue
		}
		if s.Optional {
			st.MissingOptional = append(st.MissingOptional, s)
		} else {
			st.MissingRequired = append(st.MissingRequired, s)
		}
	}
	switch {
	case len(st.MissingRequired) > 0:
		st.State = moduleLocked
	case len(st.MissingOptional) > 0:
		st.State = moduleLimited
	default:
		st.State = moduleEnabled
	}
	return st
}

// characterModuleStates returns every manifest module's status for ch,
// keyed by module id. Corp modules reflect the token's scopes only;
// the in-corporation role they also need is not visible in scopes.
func characterModuleStates(ch db.Character) map[string]moduleStatus {
	granted := scopeSet(ch.Scopes)
	out := make(map[string]moduleStatus, len(moduleManifest))
	for _, m := range moduleManifest {
		out[m.ID] = m.statusFor(granted)
	}
	return out
}
