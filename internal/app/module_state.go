package app

import (
	"net/url"
	"strconv"
	"strings"
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

// lockedNotice is the view model for the "locked-notice" template
// partial: what a locked or limited module is missing and what
// granting it adds.
type lockedNotice struct {
	Locked        bool // required scopes are missing (otherwise only optional ones)
	Heading       string
	Scopes        []scopeUse // the missing scopes whose Unlocks text is listed
	CharacterName string
	RelinkURL     string
}

// relinkURL is the sign-in link that asks for just module id's scopes
// on top of what the character already granted.
func relinkURL(moduleID string, characterID int64) string {
	q := url.Values{}
	q.Set("module", moduleID)
	q.Set("character", strconv.FormatInt(characterID, 10))
	return "/auth/eve?" + q.Encode()
}

// relinkScopes is the scope list for a targeted re-link: every scope
// the character already granted, then the module's own scopes that it
// has not. The new token's scope claim replaces the stored grant, so
// the request has to carry the old grants along or the re-link would
// quietly revoke other modules.
func relinkScopes(granted string, m moduleDef) []string {
	have := scopeSet(granted)
	out := strings.Fields(granted)
	for _, s := range m.Scopes {
		if !have[s.Scope] {
			out = append(out, s.Scope)
			have[s.Scope] = true
		}
	}
	return out
}

// snapshotModule returns the manifest module that reads snapshot kind,
// if any. Kinds no module lists (the profile, say) return false.
func snapshotModule(kind string) (moduleDef, bool) {
	for _, m := range moduleDefs() {
		for _, k := range m.Snapshots {
			if k == kind {
				return m, true
			}
		}
	}
	return moduleDef{}, false
}

// kindLockedOut reports whether the character granted none of the
// scopes of the module that reads kind. It drives page display
// (a section whose module is wholly ungranted has no feeder).
// The worker planner does not use it: planning excludes per
// kind, by the scope that gates the kind itself
// (kind_scopes.go), so a partial grant never plans the kinds
// gated by the scopes it lacks.
func kindLockedOut(granted map[string]bool, kind string) bool {
	m, ok := snapshotModule(kind)
	if !ok || len(m.Scopes) == 0 {
		return false
	}
	for _, s := range m.Scopes {
		if granted[s.Scope] {
			return false
		}
	}
	return true
}
