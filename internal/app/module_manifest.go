package app

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Module manifest
//
// A module is one feature a user can subscribe to (a home widget, a
// page). The manifest is the single place that says which ESI scopes
// a module needs, what each scope unlocks, and which snapshot kinds
// it reads. Subscribing (user preference) and enabling (scope state)
// are orthogonal: the manifest only describes the second.
//
// Layers:
//   - public:    no character token; always works.
//   - character: lights up per character, by granted scope.
//   - corp:      character token acting for its corporation; also
//     needs an in-corporation role, which scopes do not show.
//   - derived:   aggregates other modules (see Uses); owns no scope.
// ---------------------------------------------------------------------------

type moduleLayer string

const (
	layerPublic    moduleLayer = "public"
	layerCharacter moduleLayer = "character"
	layerCorp      moduleLayer = "corp"
	layerDerived   moduleLayer = "derived"
)

// Scopes that features check individually (a missing one shows a
// re-link notice rather than a dead page).
const (
	mailSendScope        = "esi-mail.send_mail.v1"
	mailOrganizeScope    = "esi-mail.organize_mail.v1"
	calendarRespondScope = "esi-calendar.respond_calendar_events.v1"
	planetScope          = "esi-planets.manage_planets.v1"
	structureScope       = module.StructureScope
)

// scopeUse is one scope a module wants. Required scopes are what the
// module cannot run without (all of them: its absence means locked);
// optional scopes light up a part of it (absence means limited).
type scopeUse struct {
	Scope    string
	Unlocks  string // user-facing: what granting this scope adds
	Optional bool
}

type moduleDef struct {
	ID        string
	Title     string
	Layer     moduleLayer
	Scopes    []scopeUse
	Snapshots []string // snapshot kinds (or kind prefixes) it reads
	// Activities are the kinds of play it serves (see module.Module).
	Activities []string
	Uses       []string // derived modules: ids of the modules aggregated
}

func req(scope, unlocks string) scopeUse { return scopeUse{Scope: scope, Unlocks: unlocks} }
func opt(scope, unlocks string) scopeUse {
	return scopeUse{Scope: scope, Unlocks: unlocks, Optional: true}
}

// moduleManifest is core's own declaration of the features whose data core
// itself reads (the fleet, net worth and briefing read the skills, assets,
// wallet, market orders and the rest). A feature that only a module's pages
// read has no entry here: its scopes and snapshot kinds are the module's
// declaration, so a build without the module neither requests nor fetches
// them. "wallet" and "market" stay because net worth reads the wallet and
// the open orders; the modules of the same id declare the same scopes, and
// TestRegisteredFeaturesAgreeWithTheManifest pins that they agree.
var moduleManifest = []moduleDef{
	// --- public ---------------------------------------------------------
	{ID: "server", Title: "Tranquility", Layer: layerPublic,
		Snapshots: []string{esi.GlobalStatus}},
	{ID: "market_public", Title: "Market watchlist", Layer: layerPublic, Activities: []string{"market"}},

	// --- character ------------------------------------------------------
	{ID: "skills", Title: "Skills", Layer: layerCharacter,
		Activities: []string{"pilots"},
		Scopes: []scopeUse{
			req("esi-skills.read_skills.v1", "Trained skills and attributes."),
			req("esi-skills.read_skillqueue.v1", "The training queue and finish times."),
		},
		Snapshots: []string{esi.SnapSkills, esi.SnapSkillqueue, esi.SnapAttributes}},
	{ID: "wallet", Title: "Wallet", Layer: layerCharacter,
		Activities: []string{"pilots"},
		Scopes: []scopeUse{
			req("esi-wallet.read_character_wallet.v1", "Balance, journal and transactions."),
		},
		Snapshots: []string{esi.SnapWallet, esi.SnapWalletJournal, esi.SnapWalletTxns}},
	{ID: "assets", Title: "Assets", Layer: layerCharacter,
		Activities: []string{"pilots", "industry"},
		Scopes: []scopeUse{
			req("esi-assets.read_assets.v1", "Everything the character owns, and where."),
		},
		Snapshots: []string{esi.SnapAssets}},
	{ID: "location", Title: "Location and ship", Layer: layerCharacter,
		Activities: []string{"pilots"},
		Scopes: []scopeUse{
			req("esi-location.read_location.v1", "Current system or station."),
			req("esi-location.read_online.v1", "Online status and last login."),
			req("esi-location.read_ship_type.v1", "The ship being flown."),
		},
		Snapshots: []string{esi.SnapLocation, esi.SnapOnline, esi.SnapShip}},
	{ID: "clones", Title: "Clones and implants", Layer: layerCharacter,
		Activities: []string{"pilots", "fitting"},
		Scopes: []scopeUse{
			req("esi-clones.read_clones.v1", "Home station, jump clones."),
			req("esi-clones.read_implants.v1", "Active implants."),
		},
		Snapshots: []string{esi.SnapClones, esi.SnapImplants}},
	{ID: "fittings", Title: "Fittings", Layer: layerCharacter,
		Activities: []string{"fitting"},
		Scopes: []scopeUse{
			req("esi-fittings.read_fittings.v1", "Saved in-game fittings."),
			opt("esi-fittings.write_fittings.v1", "Save to EVE from the fitting tools."),
		},
		Snapshots: []string{esi.SnapFittings}},
	{ID: "fatigue", Title: "Jump fatigue", Layer: layerCharacter,
		Activities: []string{"pilots"},
		Scopes: []scopeUse{
			req("esi-characters.read_fatigue.v1", "Jump fatigue timers."),
		},
		Snapshots: []string{esi.SnapFatigue}},
	{ID: "industry", Title: "Industry", Layer: layerCharacter,
		Activities: []string{"industry"},
		Scopes: []scopeUse{
			req("esi-industry.read_character_jobs.v1", "Active and finished jobs."),
			opt("esi-characters.read_blueprints.v1", "Owned blueprints and their research levels."),
		},
		Snapshots: []string{esi.SnapIndustryJobs, esi.SnapBlueprints}},
	{ID: "mining", Title: "Mining ledger", Layer: layerCharacter,
		Activities: []string{"industry"},
		Scopes: []scopeUse{
			req("esi-industry.read_character_mining.v1", "The mining ledger."),
		},
		Snapshots: []string{esi.SnapMining}},
	{ID: "market", Title: "Market orders", Layer: layerCharacter,
		Activities: []string{"market"},
		Scopes: []scopeUse{
			req("esi-markets.read_character_orders.v1", "Open orders and order history."),
			opt("esi-markets.structure_markets.v1", "Prices at player-owned structures."),
		},
		Snapshots: []string{esi.SnapOrders, esi.SnapOrdersHistory}},
	{ID: "killmails", Title: "Killmails", Layer: layerCharacter,
		Activities: []string{"pilots"},
		Scopes: []scopeUse{
			req("esi-killmails.read_killmails.v1", "The character's kills and losses."),
		},
		Snapshots: []string{esi.SnapKillmails}},
	{ID: "fleet_ops", Title: "Fleet operations", Layer: layerCharacter,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-fleets.read_fleet.v1", "Live fleet composition while the character is in a fleet."),
		}},
	{ID: "planets", Title: "Planetary industry", Layer: layerCharacter,
		Activities: []string{"industry"},
		Scopes: []scopeUse{
			req(planetScope, "Colonies and their layouts (the only scope that gates colony reads)."),
			opt("esi-planets.read_customs_offices.v1", "Customs office tax rates."),
		},
		Snapshots: []string{esi.SnapPlanets, esi.SnapPlanetLayoutPrefix}},
	{ID: "structures", Title: "Structures", Layer: layerCharacter,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req(structureScope, "Names of player-owned structures."),
			opt("esi-search.search_structures.v1", "Searching structures by name."),
		}},
	{ID: "standings", Title: "Standings and loyalty", Layer: layerCharacter,
		Activities: []string{"pilots"},
		Scopes: []scopeUse{
			req("esi-characters.read_standings.v1", "NPC standings."),
			opt("esi-characters.read_loyalty.v1", "Loyalty points."),
			opt("esi-characters.read_fw_stats.v1", "Faction Warfare stats."),
		}},
	{ID: "corp_roles", Title: "Corporation roles", Layer: layerCharacter,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-characters.read_corporation_roles.v1", "The corporation roles held, which decide who may create ops."),
		},
		Snapshots: []string{esi.SnapCorpRoles}},
	{ID: "character_record", Title: "Character record", Layer: layerCharacter,
		Activities: []string{"pilots"},
		Scopes: []scopeUse{
			req("esi-characters.read_notifications.v1", "In-game notifications."),
			opt("esi-characters.read_medals.v1", "Medals."),
			opt("esi-characters.read_titles.v1", "Corporation titles held."),
			opt("esi-characters.read_agents_research.v1", "Agent research points."),
			opt("esi.activity.char:read", "Character activity."),
			opt("esi.cosmetic.char:read", "Cosmetic unlocks."),
		}},

	// --- corp -----------------------------------------------------------
	{ID: "corp_wallet", Title: "Corporation wallets", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-wallet.read_corporation_wallets.v1", "Division balances, journals and transactions."),
		},
		Snapshots: []string{esi.SnapCorpWallets, esi.SnapCorpJournalPrefix, esi.SnapCorpTxnsPrefix}},
	{ID: "corp_assets", Title: "Corporation assets", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-assets.read_corporation_assets.v1", "Corporation hangars and assets."),
		},
		Snapshots: []string{esi.SnapCorpAssets}},
	{ID: "corp_industry", Title: "Corporation industry", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-industry.read_corporation_jobs.v1", "Corporation industry jobs."),
			opt("esi-industry.read_corporation_mining.v1", "Corporation mining observers."),
			opt("esi-corporations.read_blueprints.v1", "Corporation blueprints."),
			opt("esi-corporations.read_container_logs.v1", "Container access logs."),
		}},
	{ID: "corp_market", Title: "Corporation orders", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-markets.read_corporation_orders.v1", "Corporation market orders."),
		},
		Snapshots: []string{esi.SnapCorpOrders}},
	{ID: "corp_contracts", Title: "Corporation contracts", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-contracts.read_corporation_contracts.v1", "Corporation contracts."),
		}},
	{ID: "corp_killmails", Title: "Corporation killmails", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-killmails.read_corporation_killmails.v1", "The corporation's kills and losses."),
		},
		Snapshots: []string{esi.SnapCorpKillmails}},
	{ID: "corp_structures", Title: "Corporation structures", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-corporations.read_structures.v1", "Structures and their fuel and state."),
			opt("esi-corporations.read_starbases.v1", "Starbases."),
			opt("esi-corporations.read_facilities.v1", "Industry facilities."),
		},
		Snapshots: []string{esi.SnapCorpStructures}},
	{ID: "corp_members", Title: "Corporation members", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-corporations.read_corporation_membership.v1", "The member list."),
			opt("esi-corporations.track_members.v1", "Member locations, ships and last login."),
			opt("esi-corporations.read_titles.v1", "Corporation titles."),
			opt("esi-corporations.read_divisions.v1", "Hangar and wallet division names."),
		},
		Snapshots: []string{esi.SnapCorpMembers, esi.SnapCorpMemberTracking}},
	{ID: "corp_standings", Title: "Corporation standings", Layer: layerCorp,
		Activities: []string{"corporation"},
		Scopes: []scopeUse{
			req("esi-corporations.read_standings.v1", "Corporation standings."),
			opt("esi-corporations.read_fw_stats.v1", "Corporation Faction Warfare stats."),
			opt("esi-corporations.read_medals.v1", "Corporation medals."),
		}},

	// --- derived --------------------------------------------------------
	{ID: "fleet", Title: "Fleet overview", Layer: layerDerived,
		Uses: []string{"location", "skills", "wallet"}},
	{ID: "briefing", Title: "Briefing", Layer: layerDerived,
		Uses: []string{"skills", "industry", "market", "contracts", "planets"}},
	{ID: "attention", Title: "Needs attention", Layer: layerDerived,
		Uses: []string{"skills", "industry", "market", "contracts", "planets"}},
	{ID: "networth", Title: "Net worth", Layer: layerDerived,
		Uses: []string{"wallet", "assets", "market"}},
}

// moduleDefs is every module the scope surfaces know of: the registry's
// declarations first (the source of truth for a module that has been
// extracted), then the manifest entries no registered module has taken
// over. When a stage extracts a feature it deletes its manifest entry
// and its declaration serves; by the last stage the manifest is gone.
func moduleDefs() []moduleDef {
	defs := make([]moduleDef, 0, len(registeredModules)+len(moduleManifest))
	taken := map[string]bool{}
	for _, m := range registeredModules {
		defs = append(defs, moduleDefFromRegistry(m))
		taken[m.ID] = true
	}
	for _, m := range moduleManifest {
		if !taken[m.ID] {
			defs = append(defs, m)
		}
	}
	return defs
}

// moduleDefFromRegistry reads a module's declaration as a moduleDef:
// its required scopes, then its enhancement scopes as the optional
// ones. A module that wants no scope at all is public.
func moduleDefFromRegistry(m module.Module) moduleDef {
	d := moduleDef{ID: m.ID, Title: m.Title, Layer: layerPublic, Uses: m.Requires, Snapshots: m.Snapshots, Activities: m.Activities}
	for _, s := range m.Scopes {
		d.Scopes = append(d.Scopes, req(s.Scope, s.Unlocks))
	}
	for _, s := range m.EnhanceScopes {
		d.Scopes = append(d.Scopes, opt(s.Scope, s.Unlocks))
	}
	if len(d.Scopes) > 0 {
		d.Layer = layerCharacter
	}
	return d
}

// moduleByID returns the declaration or manifest entry for id.
func moduleByID(id string) (moduleDef, bool) {
	for _, m := range moduleDefs() {
		if m.ID == id {
			return m, true
		}
	}
	return moduleDef{}, false
}

// requiredScopes lists the scopes the module cannot run without.
func (m moduleDef) requiredScopes() []string {
	var out []string
	for _, s := range m.Scopes {
		if !s.Optional {
			out = append(out, s.Scope)
		}
	}
	return out
}
