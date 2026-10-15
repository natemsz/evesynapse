package app

import (
	"fmt"
	"sort"
	"strings"

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
// declare the scopes. Playstyle answers grant required scopes only: the
// enhancements stay opt-ins, offered beside the summary.
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
	Activities []string
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
		if len(def.Scopes) == 0 && len(def.Activities) == 0 {
			continue
		}
		f := linkFeature{ID: def.ID, Title: def.Title, Activities: def.Activities}
		for _, use := range def.Uses {
			f.Requires = append(f.Requires, use)
		}
		for _, s := range def.Scopes {
			if s.Optional {
				f.Enhance = append(f.Enhance, s)
			} else {
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
	Scopes       []string // required plus opted-in enhancements, sorted
}

// resolveLink works a choice out: the features it names (an activity names
// the features that declare it), the closure over what they require, the
// scopes those declare, and the opted-in enhancements. An unknown activity,
// feature or an enhancement that belongs to no chosen feature is an error.
func resolveLink(c linkChoice) (linkResolved, error) {
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
	for _, f := range all {
		if !in[f.ID] {
			continue
		}
		res.Features = append(res.Features, linkFeatureRow{linkFeature: f, Pinned: pinned[f.ID], NeededBy: neededBy[f.ID]})
		for _, s := range f.Required {
			required[s.Scope] = true
		}
		for _, s := range f.Enhance {
			if _, seen := optional[s.Scope]; !seen {
				optional[s.Scope] = linkEnhancement{Scope: s.Scope, Unlocks: s.Unlocks, Feature: f.Title}
			}
		}
	}
	chosen := map[string]bool{}
	for _, scope := range c.Enhance {
		e, ok := optional[scope]
		if !ok {
			return linkResolved{}, fmt.Errorf("permission %q belongs to no chosen feature", scope)
		}
		if required[scope] {
			continue // already needed: not an extra
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
func fullLinkChoice() linkChoice {
	c := linkChoice{Activities: []string{activityEverything}}
	for _, f := range linkFeatures() {
		for _, s := range f.Enhance {
			if !contains(c.Enhance, s.Scope) {
				c.Enhance = append(c.Enhance, s.Scope)
			}
		}
	}
	return c
}

// requestableScopes is every scope this build can ask for: the full link.
// In a build with every module it is eveScopes; a build without a module
// leaves that module's scopes out.
func requestableScopes() []string {
	res, err := resolveLink(fullLinkChoice())
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
