package app

import (
	"fmt"
	"sort"
	"strings"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// What a player can ask EveSynapse for, as the sign-up sees it.
//
// A feature is a module the build has, or one of core's own data features
// (skills, assets, location, ...). Each declares the ESI scopes it cannot
// work without and the optional ones that light up more of it, so the
// link to EVE is built from the declarations, never from a list of its
// own. A player answers what they want to do (an activity); the registry's
// Requires closure turns that into the features to switch on; the features
// declare the scopes. A playstyle answer grants the required scopes and
// the enhancements of its features alike: the summary starts from all
// of them, and the player unticks what they do not want.
//
// A request is only ever a subset of eveScopes, the list the EVE developer
// portal allows: asking for a scope the portal does not have fails the
// sign-in for everyone, so a link is built safe by construction.
// ---------------------------------------------------------------------------

// linkActivity is one answer to "what do you want to do with EveSynapse?".
type linkActivity struct {
	ID     string
	Title  string
	Blurb  string
	Accent int // 1-6, the Trade Ideas accent palette
}

// activityEverything is the answer that stands for every feature.
const activityEverything = "everything"

var linkActivities = []linkActivity{
	{"pilots", "Track my pilots", "Skills and training, wallets, assets, mail, contacts and where your characters are.", 1},
	{"market", "Trade the market", "The spread scanner, trade routes, top stations, your orders and contracts.", 2},
	{"industry", "Build & produce", "Build plans, blueprints, jobs and planetary industry.", 3},
	{"corporation", "Run my corporation", "Members, wallets, ship replacement, doctrines, groups and Discord.", 4},
	{"fitting", "Fit & plan ships", "The ship workbench, loadouts, skill plans and doctrine fits.", 5},
	{activityEverything, "A bit of everything", "Every feature. You still choose which extra permissions to add.", 6},
}

// linkEssentials is the starting set a fresh sign-up opens with
// pre-ticked: tracking your pilots (skills, wallet, assets, location and
// the rest of the core character picture). Later links open on the
// account's remembered choice instead; an explicit submission that
// unticks everything is a choice of nothing and is never widened.
var linkEssentials = []string{"pilots"}

func linkActivityByID(id string) (linkActivity, bool) {
	for _, a := range linkActivities {
		if a.ID == id {
			return a, true
		}
	}
	return linkActivity{}, false
}

// linkFeature is one switchable feature.
type linkFeature struct {
	ID         string
	Title      string
	Requires   []string
	Required   []scopeUse
	Enhance    []scopeUse
	Role       []linkRole // corporation data: the scope is only worth having with the in-game role
	Activities []string
}

// linkRole is a permission whose data EVE gives only to a character holding
// an in-game corporation role. It is declared required (the feature dies
// without it) but, for a given player, that is true of nobody and somebody
// at once, so the sign-up offers it as an opt-in framed by the role.
type linkRole struct {
	scopeUse
	Roles string // "Accountant or Junior Accountant"; "" when it varies
}

// corpFeatureKinds names the corporation snapshot whose 403 tells which
// role a feature needs (corpKindRole has the labels).
var corpFeatureKinds = map[string]string{
	"corp_wallet":     esi.SnapCorpWallets,
	"corp_assets":     esi.SnapCorpAssets,
	"corp_market":     esi.SnapCorpOrders,
	"corp_killmails":  esi.SnapCorpKillmails,
	"corp_structures": esi.SnapCorpStructures,
}

// linkFeatures lists the features the sign-up offers, in the order they
// are declared: the registered modules and core's data features. Derived
// features (they own no scope) and public ones with nothing to switch on
// are left out.
func linkFeatures() []linkFeature {
	var out []linkFeature
	for _, def := range moduleDefs() {
		if def.Layer == layerDerived {
			continue
		}
		required := 0
		for _, sc := range def.Scopes {
			if !sc.Optional {
				required++
			}
		}
		// Nothing required and no kind of play: not a feature the player
		// can pick (the shared cache is operator-level; its one extra is
		// offered through Module.LinkExtras when the server has it on).
		if required == 0 && len(def.Activities) == 0 {
			continue
		}
		f := linkFeature{ID: def.ID, Title: def.Title, Activities: def.Activities}
		f.Requires = append(f.Requires, def.Uses...)
		for _, s := range def.Scopes {
			switch {
			case s.Optional:
				f.Enhance = append(f.Enhance, s)
			case def.Layer == layerCorp:
				f.Role = append(f.Role, linkRole{scopeUse: s, Roles: corpKindRole(corpFeatureKinds[def.ID])})
			default:
				f.Required = append(f.Required, s)
			}
		}
		out = append(out, f)
	}
	return out
}

// linkChoice is what a player picked, as it is remembered.
type linkChoice struct {
	Activities []string `json:"activities,omitempty"`
	Features   []string `json:"features,omitempty"`
	Enhance    []string `json:"enhance,omitempty"`
}

func (c linkChoice) empty() bool {
	return len(c.Activities) == 0 && len(c.Features) == 0 && len(c.Enhance) == 0
}

// linkFeatureRow is one feature of a resolved choice.
type linkFeatureRow struct {
	linkFeature
	Pinned   bool     // picked by an answer or ticked, not only required by another
	NeededBy []string // titles of the features that require it
}

// linkEnhancement is one optional scope of a chosen feature.
type linkEnhancement struct {
	Scope   string
	Unlocks string
	Feature string
	Chosen  bool
}

// linkResolved is a choice turned into features and scopes.
type linkResolved struct {
	Choice       linkChoice
	Features     []linkFeatureRow
	Required     []string // the scopes the features cannot work without
	Enhancements []linkEnhancement
	RoleOptIns   []linkRoleOptIn   // corporation permissions, framed by the role they need
	ServerExtras []linkEnhancement // what this server offers on top (Module.LinkExtras)
	Scopes       []string          // required plus opted-in enhancements, sorted
}

// linkRoleOptIn is one role-gated permission of a chosen corporation feature.
type linkRoleOptIn struct {
	Scope   string
	Unlocks string
	Feature string
	Roles   string
	Chosen  bool
}

// resolveLink works a choice out: the features it names (an activity names
// the features that declare it), the closure over what they require, the
// scopes those declare, and the opted-in enhancements. An unknown activity,
// feature or an enhancement that belongs to no chosen feature is an error.
func resolveLink(c linkChoice) (linkResolved, error) { return resolveLinkWith(c, nil) }

// resolveLink is the package function with what this server offers on top.
func (app *Application) resolveLink(c linkChoice) (linkResolved, error) {
	return resolveLinkWith(c, app.linkServerExtras())
}

// linkServerExtras are the optional permissions the built-in modules offer
// on this server (Module.LinkExtras): none unless the module's own settings
// call for them.
func (app *Application) linkServerExtras() []linkEnhancement {
	var out []linkEnhancement
	for _, m := range registeredModules {
		if m.LinkExtras == nil {
			continue
		}
		for _, sc := range m.LinkExtras(app) {
			out = append(out, linkEnhancement{Scope: sc.Scope, Unlocks: sc.Unlocks, Feature: "This server"})
		}
	}
	return out
}

func resolveLinkWith(c linkChoice, extras []linkEnhancement) (linkResolved, error) {
	all := linkFeatures()
	byID := make(map[string]linkFeature, len(all))
	for _, f := range all {
		byID[f.ID] = f
	}
	pinned := map[string]bool{}
	for _, id := range c.Activities {
		if _, ok := linkActivityByID(id); !ok {
			return linkResolved{}, fmt.Errorf("unknown activity %q", id)
		}
		for _, f := range all {
			if id == activityEverything || contains(f.Activities, id) {
				pinned[f.ID] = true
			}
		}
	}
	for _, id := range c.Features {
		if _, ok := byID[id]; !ok {
			return linkResolved{}, fmt.Errorf("unknown feature %q", id)
		}
		pinned[id] = true
	}
	in := map[string]bool{}
	neededBy := map[string][]string{}
	var add func(id string)
	add = func(id string) {
		if in[id] {
			return
		}
		in[id] = true
		f := byID[id]
		for _, req := range f.Requires {
			if _, ok := byID[req]; !ok {
				continue
			}
			neededBy[req] = append(neededBy[req], f.Title)
			add(req)
		}
	}
	for id := range pinned {
		add(id)
	}

	res := linkResolved{Choice: c}
	required := map[string]bool{}
	optional := map[string]linkEnhancement{}
	roles := map[string]linkRoleOptIn{}
	var roleOrder []string
	for _, f := range all {
		if !in[f.ID] {
			continue
		}
		res.Features = append(res.Features, linkFeatureRow{linkFeature: f, Pinned: pinned[f.ID], NeededBy: neededBy[f.ID]})
		for _, s := range f.Required {
			required[s.Scope] = true
		}
		for _, r := range f.Role {
			if _, seen := roles[r.Scope]; !seen {
				roles[r.Scope] = linkRoleOptIn{Scope: r.Scope, Unlocks: r.Unlocks, Feature: f.Title, Roles: r.Roles}
				roleOrder = append(roleOrder, r.Scope)
			}
		}
		for _, s := range f.Enhance {
			if _, seen := optional[s.Scope]; !seen {
				optional[s.Scope] = linkEnhancement{Scope: s.Scope, Unlocks: s.Unlocks, Feature: f.Title}
			}
		}
	}
	// A permission a role opt-in already offers is not a second extra.
	for scope := range roles {
		delete(optional, scope)
	}
	serverExtra := map[string]linkEnhancement{}
	for _, e := range extras {
		if required[e.Scope] {
			continue
		}
		if _, isRole := roles[e.Scope]; isRole {
			continue
		}
		if _, isOpt := optional[e.Scope]; isOpt {
			continue
		}
		serverExtra[e.Scope] = e
	}
	chosen := map[string]bool{}
	for _, scope := range c.Enhance {
		if required[scope] {
			continue // already needed: not an extra
		}
		if r, ok := roles[scope]; ok {
			r.Chosen = true
			roles[scope] = r
			chosen[scope] = true
			continue
		}
		if e, ok := serverExtra[scope]; ok {
			e.Chosen = true
			serverExtra[scope] = e
			chosen[scope] = true
			continue
		}
		e, ok := optional[scope]
		if !ok {
			return linkResolved{}, fmt.Errorf("permission %q belongs to no chosen feature", scope)
		}
		e.Chosen = true
		optional[scope] = e
		chosen[scope] = true
	}
	for scope, e := range optional {
		if required[scope] {
			continue
		}
		res.Enhancements = append(res.Enhancements, e)
	}
	sort.Slice(res.Enhancements, func(i, j int) bool {
		a, b := res.Enhancements[i], res.Enhancements[j]
		if a.Feature != b.Feature {
			return a.Feature < b.Feature
		}
		return a.Scope < b.Scope
	})
	for _, scope := range roleOrder {
		res.RoleOptIns = append(res.RoleOptIns, roles[scope])
	}
	for _, e := range serverExtra {
		res.ServerExtras = append(res.ServerExtras, e)
	}
	sort.Slice(res.ServerExtras, func(i, j int) bool { return res.ServerExtras[i].Scope < res.ServerExtras[j].Scope })
	for scope := range required {
		res.Required = append(res.Required, scope)
	}
	sort.Strings(res.Required)
	union := map[string]bool{}
	for _, s := range res.Required {
		union[s] = true
	}
	for s := range chosen {
		union[s] = true
	}
	res.Scopes = portalSubset(union)
	return res, nil
}

// fullLinkChoice is every feature with every enhancement: today's link.
func fullLinkChoice() linkChoice { return fullLinkChoiceWith(nil) }

func fullLinkChoiceWith(extras []linkEnhancement) linkChoice {
	c := linkChoice{Activities: []string{activityEverything}}
	add := func(scope string) {
		if !contains(c.Enhance, scope) {
			c.Enhance = append(c.Enhance, scope)
		}
	}
	for _, f := range linkFeatures() {
		for _, s := range f.Enhance {
			add(s.Scope)
		}
		for _, r := range f.Role {
			add(r.Scope)
		}
	}
	for _, e := range extras {
		add(e.Scope)
	}
	return c
}

// requestableScopes is every scope this build can ask for: the full link.
// In a build with every module it is eveScopes; a build without a module
// leaves that module's scopes out.
func requestableScopes() []string { return requestableScopesWith(nil) }

func (app *Application) requestableScopes() []string {
	return requestableScopesWith(app.linkServerExtras())
}

func requestableScopesWith(extras []linkEnhancement) []string {
	res, err := resolveLinkWith(fullLinkChoiceWith(extras), extras)
	if err != nil {
		logging.Errorf("link: the full selection did not resolve: %v", err)
		return eveScopes
	}
	return res.Scopes
}

// portalSubset keeps only the scopes the developer portal allows, sorted.
// A scope outside eveScopes would fail the sign-in for everyone, so it is
// dropped (and logged: it means a declaration names a scope the portal
// list lacks).
func portalSubset(want map[string]bool) []string {
	allowed := make(map[string]bool, len(eveScopes))
	for _, s := range eveScopes {
		allowed[s] = true
	}
	var out []string
	for s := range want {
		if !allowed[s] {
			logging.Errorf("link: dropping %q: it is not in the developer portal's scope list", s)
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// titleList says feature titles in a sentence: "A", "A and B", "A, B and C".
func titleList(titles []string) string {
	switch len(titles) {
	case 0:
		return ""
	case 1:
		return titles[0]
	}
	return strings.Join(titles[:len(titles)-1], ", ") + " and " + titles[len(titles)-1]
}

// mergeScopes is the union of two scope lists, sorted, kept to the
// developer portal's list.
func mergeScopes(a, b []string) []string {
	union := map[string]bool{}
	for _, s := range a {
		union[s] = true
	}
	for _, s := range b {
		union[s] = true
	}
	return portalSubset(union)
}
