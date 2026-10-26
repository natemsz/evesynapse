package app

import (
	"strings"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// The kind-to-scope manifest: the one scope whose grant gates
// each per-character snapshot kind, from the ESI endpoint the
// kind reads. The worker's planners (the refresh* passes and the
// warm* want builders) consult it before any fetch is planned: a
// kind whose required scope is not in the character's recorded
// grants is excluded from the plan outright, so ESI is never
// asked for data the login cannot have. That is a plan fact, not
// a refusal: nothing is recorded and nothing is logged.
//
// The manifest is per kind, never per module. The module
// manifest (module_manifest.go) groups scopes for display, but a
// module with some of its scopes granted says nothing about the
// kinds gated by the scopes it lacks: the industry module pairs
// the jobs scope with the blueprints scope, and a character
// holding only the first must never be asked for blueprints.
//
// A character with no recorded grants is unknown, not locked:
// nothing is excluded, and fetches proceed as they always have
// (the refusal settle in fetch_refusal.go remains the backstop
// for grants that turn out stale). Kinds whose endpoint is
// public carry an empty scope and are never excluded. A kind
// missing from the manifest is never excluded either; the worker
// tests pin every planned kind to an entry here, so a new kind
// cannot join a pass without declaring its scope.
// ---------------------------------------------------------------------------

var kindScopes = map[string]string{
	// Character kinds.
	esi.SnapProfile:       "", // public character record
	esi.SnapSkills:        "esi-skills.read_skills.v1",
	esi.SnapSkillqueue:    "esi-skills.read_skillqueue.v1",
	esi.SnapAttributes:    "esi-skills.read_skills.v1",
	esi.SnapWallet:        "esi-wallet.read_character_wallet.v1",
	esi.SnapWalletJournal: "esi-wallet.read_character_wallet.v1",
	esi.SnapWalletTxns:    "esi-wallet.read_character_wallet.v1",
	esi.SnapAssets:        "esi-assets.read_assets.v1",
	esi.SnapLocation:      "esi-location.read_location.v1",
	esi.SnapShip:          "esi-location.read_ship_type.v1",
	esi.SnapOnline:        "esi-location.read_online.v1",
	esi.SnapClones:        "esi-clones.read_clones.v1",
	esi.SnapImplants:      "esi-clones.read_implants.v1",
	esi.SnapFittings:      "esi-fittings.read_fittings.v1",
	esi.SnapFatigue:       "esi-characters.read_fatigue.v1",
	esi.SnapKillmails:     "esi-killmails.read_killmails.v1",
	esi.SnapCorpRoles:     "esi-characters.read_corporation_roles.v1",
	esi.SnapOrders:        "esi-markets.read_character_orders.v1",
	esi.SnapOrdersHistory: "esi-markets.read_character_orders.v1",
	esi.SnapContracts:     "esi-contracts.read_character_contracts.v1",
	esi.SnapIndustryJobs:  "esi-industry.read_character_jobs.v1",
	esi.SnapBlueprints:    "esi-characters.read_blueprints.v1",
	esi.SnapMining:        "esi-industry.read_character_mining.v1",
	esi.SnapPlanets:       planetScope,
	esi.SnapMail:          "esi-mail.read_mail.v1",
	esi.SnapMailLabels:    "esi-mail.read_mail.v1",
	esi.SnapMailLists:     "esi-mail.read_mail.v1",
	esi.SnapCalendar:      "esi-calendar.read_calendar_events.v1",
	esi.SnapContacts:      "esi-characters.read_contacts.v1",

	// Corporation kinds (fetched with a character's token against
	// the character's corporation; the in-game role some of them
	// also need cannot be known at plan time and stays with the
	// refusal settle).
	esi.SnapCorpInfo:           "", // public corporation record
	esi.SnapCorpMembers:        "esi-corporations.read_corporation_membership.v1",
	esi.SnapCorpMemberTracking: "esi-corporations.track_members.v1",
	esi.SnapCorpWallets:        "esi-wallet.read_corporation_wallets.v1",
	esi.SnapCorpOrders:         "esi-markets.read_corporation_orders.v1",
	esi.SnapCorpAssets:         "esi-assets.read_corporation_assets.v1",
	esi.SnapCorpStructures:     "esi-corporations.read_structures.v1",
	esi.SnapCorpKillmails:      "esi-killmails.read_corporation_killmails.v1",
}

// kindScopePrefixes resolves the suffix-keyed kind families
// (per-division wallets, per-entity details) to their scope.
var kindScopePrefixes = []struct {
	prefix string
	scope  string
}{
	{esi.SnapCorpJournalPrefix, "esi-wallet.read_corporation_wallets.v1"},
	{esi.SnapCorpTxnsPrefix, "esi-wallet.read_corporation_wallets.v1"},
	{esi.SnapPlanetLayoutPrefix, planetScope},
	{esi.SnapMailBodyPrefix, "esi-mail.read_mail.v1"},
	{esi.SnapCalendarEventPrefix, "esi-calendar.read_calendar_events.v1"},
	{esi.SnapCalendarAttPrefix, "esi-calendar.read_calendar_events.v1"},
}

// kindRequiredScope returns the scope that gates kind and
// whether the manifest covers the kind at all. A covered kind
// with an empty scope is public: no grant gates it.
func kindRequiredScope(kind string) (string, bool) {
	if scope, ok := kindScopes[kind]; ok {
		return scope, true
	}
	for _, p := range kindScopePrefixes {
		if strings.HasPrefix(kind, p.prefix) {
			return p.scope, true
		}
	}
	return "", false
}

// kindExcludedByGrants reports whether the plan must leave kind
// out for a character with these recorded grants: the kind's
// required scope is known and not granted. Unknown kinds,
// public kinds, and characters with no recorded grants are
// never excluded.
func kindExcludedByGrants(granted map[string]bool, kind string) bool {
	scope, known := kindRequiredScope(kind)
	return known && scope != "" && len(granted) > 0 && !granted[scope]
}
