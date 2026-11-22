package app

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Who can see, join and run a fleet. Every Fleets surface asks
// fleetViewer.access, so a page, a share link, the calendar and a
// notification cannot disagree.
//
// A share link reveals a public or link-only fleet. It never reveals a
// private one, never passes a join rule and never grants management.
// A verified pilot has no standing: its public corporation and alliance
// can satisfy a member or corporation rule to join a fleet, and nothing
// a pilot holds can manage one.
// ---------------------------------------------------------------------------

// Stored in ops: never rename.
const (
	fleetTypeNPSI        = "npsi"
	fleetTypeCorporation = "corporation"
	fleetTypeAlliance    = "alliance"
	fleetTypeCommunity   = "community"
	fleetTypeCustom      = "custom"

	fleetPublic     = "public"
	fleetUnlisted   = "unlisted"
	fleetRestricted = "restricted"

	joinAnyone   = "anyone"
	joinVerified = "verified"
	joinAccount  = "account"
	joinByRule   = "rule"
)

// Stored in op_access_rules.action: never rename.
const (
	accessView = "view"
	accessJoin = "join"
)

// pilotAffiliationMaxAge is how long a verified pilot's public
// corporation and alliance count after the sign-in that read them, so
// someone who has left a corporation does not keep its fleets.
const pilotAffiliationMaxAge = 24 * time.Hour

// fleetViewer is who is asking: an account, a verified pilot, or nobody.
type fleetViewer struct {
	UserID     int64
	Standing   orgStanding
	Characters []accountCharacter
	Pilot      *fleetPilot
	// plans holds the owners whose fleets the account may run, by key.
	plans map[string]bool
}

// fleetPilot is a verified pilot, with what it can show for joining.
// CorporationID and AllianceID are 0 once the sign-in that read them is
// too old to trust.
type fleetPilot struct {
	CharacterID   int64
	Name          string
	CorporationID int64
	AllianceID    int64
	Communities   map[int64]bool
	Groups        map[int64]bool
}

// fleetViewerFor is the session's viewer.
func (app *Application) fleetViewerFor(ctx context.Context) fleetViewer {
	if userID := app.userID(ctx); userID != 0 {
		return app.accountFleetViewer(ctx, userID)
	}
	if pilot, ok := app.VerifiedPilot(ctx); ok {
		return fleetViewer{Standing: standing.New(), Pilot: app.fleetPilotFor(ctx, pilot, time.Now()), plans: map[string]bool{}}
	}
	return fleetViewer{Standing: standing.New(), plans: map[string]bool{}}
}

// accountFleetViewer is an account as a viewer. A failed read leaves it
// holding less, never more.
func (app *Application) accountFleetViewer(ctx context.Context, userID int64) fleetViewer {
	v := fleetViewer{UserID: userID, Standing: standing.New(), plans: map[string]bool{}}
	st, characters, err := app.standingWithCharacters(ctx, userID)
	if err != nil {
		logging.Errorf("fleets: standing of user %d: %v", userID, err)
		return v
	}
	v.Standing, v.Characters = st, characters
	v.plans = app.ownersPermitted(ctx, st, v.owners(), permFleets)
	return v
}

// fleetPilotFor reads what a verified pilot can show for joining.
func (app *Application) fleetPilotFor(ctx context.Context, pilot module.VerifiedPilot, now time.Time) *fleetPilot {
	out := &fleetPilot{CharacterID: pilot.CharacterID, Name: pilot.Name, Communities: map[int64]bool{}, Groups: map[int64]bool{}}
	row, err := app.queries.GetVerifiedPilot(ctx, pilot.CharacterID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("fleets: verified pilot %d: %v", pilot.CharacterID, err)
	}
	if err == nil && row.AffiliationAt.Valid && now.Sub(row.AffiliationAt.Time) <= pilotAffiliationMaxAge {
		out.CorporationID, out.AllianceID = row.CorporationID, row.AllianceID
	}
	communities, err := app.queries.ListPilotCommunities(ctx, pilot.CharacterID)
	if err != nil {
		logging.Errorf("fleets: communities of pilot %d: %v", pilot.CharacterID, err)
	}
	for _, id := range communities {
		out.Communities[id] = true
	}
	groups, err := app.queries.ListPilotCommunityGroups(ctx, pilot.CharacterID)
	if err != nil {
		logging.Errorf("fleets: groups of pilot %d: %v", pilot.CharacterID, err)
	}
	for _, id := range groups {
		out.Groups[id] = true
	}
	return out
}

// level says how far the viewer has come: "anonymous", "pilot" or
// "account".
func (v fleetViewer) level() string {
	switch {
	case v.UserID != 0:
		return "account"
	case v.Pilot != nil:
		return "pilot"
	}
	return "anonymous"
}

// characterIDs lists the characters the viewer acts as.
func (v fleetViewer) characterIDs() []int64 {
	if v.Pilot != nil {
		return []int64{v.Pilot.CharacterID}
	}
	ids := make([]int64, 0, len(v.Characters))
	for _, c := range v.Characters {
		ids = append(ids, c.CharacterID)
	}
	return ids
}

// owners lists every owner the viewer belongs to, once each:
// corporations, then alliances, then communities.
func (v fleetViewer) owners() []orgOwner {
	seen := map[string]bool{}
	var corps, alliances, communities []orgOwner
	add := func(list *[]orgOwner, o orgOwner) {
		if o.ID != 0 && !seen[o.Key()] {
			seen[o.Key()] = true
			*list = append(*list, o)
		}
	}
	if p := v.Pilot; p != nil {
		add(&corps, orgOwner{Kind: ownerCorporation, ID: p.CorporationID})
		add(&alliances, orgOwner{Kind: ownerAlliance, ID: p.AllianceID})
		for _, id := range sortedKeys(p.Communities) {
			add(&communities, orgOwner{Kind: ownerCommunity, ID: id})
		}
	}
	for _, c := range v.Characters {
		for _, corp := range sortedKeys(c.Standing.Corps) {
			add(&corps, orgOwner{Kind: ownerCorporation, ID: corp})
			add(&alliances, orgOwner{Kind: ownerAlliance, ID: c.Standing.Alliance[corp]})
		}
		for _, id := range sortedKeys(c.Standing.Communities) {
			add(&communities, orgOwner{Kind: ownerCommunity, ID: id})
		}
	}
	return append(append(corps, alliances...), communities...)
}

// planOwners lists the owners the account may plan fleets for.
func (v fleetViewer) planOwners() []orgOwner {
	var out []orgOwner
	for _, o := range v.owners() {
		if v.plans[o.Key()] {
			out = append(out, o)
		}
	}
	return out
}

// belongs reports whether the viewer is in an owner. A pilot counts by
// its public affiliation and its community places.
func (v fleetViewer) belongs(owner orgOwner) bool {
	p := v.Pilot
	if p == nil {
		return v.UserID != 0 && v.Standing.Belongs(owner)
	}
	switch owner.Kind {
	case ownerCorporation:
		return p.CorporationID != 0 && p.CorporationID == owner.ID
	case ownerAlliance:
		return p.AllianceID != 0 && p.AllianceID == owner.ID
	case ownerCommunity:
		return p.Communities[owner.ID]
	}
	return false
}

// meets reports whether the viewer satisfies one rule of a fleet. A
// pilot can meet a member, corporation or community group rule, and
// never one that rests on an account or an in-game role.
func (v fleetViewer) meets(owner orgOwner, rule standing.Rule) bool {
	p := v.Pilot
	if p == nil {
		return v.UserID != 0 && v.Standing.Meets(owner, rule)
	}
	switch rule.Kind {
	case ruleMember:
		return v.belongs(owner)
	case ruleCorp:
		corp, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return corp != 0 && corp == p.CorporationID && v.belongs(owner)
	case ruleGroup:
		group, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return owner.Kind == ownerCommunity && p.Groups[group]
	}
	return false
}

// commands reports whether one of the account's characters is a
// commander of the fleet and still belongs to its owner, so leaving a
// corporation ends the place.
func (v fleetViewer) commands(op db.Op, commanders []int64) bool {
	owner := opOwner(op)
	for _, c := range v.Characters {
		if !c.Standing.Belongs(owner) {
			continue
		}
		if c.CharacterID == op.FcCharacterID {
			return true
		}
		for _, id := range commanders {
			if id == c.CharacterID {
				return true
			}
		}
	}
	return false
}

// opOwner is who a fleet belongs to.
func opOwner(op db.Op) orgOwner { return orgOwner{Kind: op.OwnerKind, ID: op.OwnerID} }

// fleetFacts is what the access decision needs to know about a fleet
// beyond its own row.
type fleetFacts struct {
	Rules      []db.OpAccessRule
	Commanders []int64
	// SignedUp: one of the viewer's characters has answered.
	SignedUp bool
	// ViaToken: the viewer came with the fleet's current share link.
	ViaToken bool
	// Unread: the rules could not be read, so nothing is shown.
	Unread bool
}

// fleetAccess is what a viewer may do with one fleet.
type fleetAccess struct {
	View bool
	// Join: at least one of the viewer's characters may sign up.
	Join bool
	// Manage: change the details, cancel, and mark attendance.
	Manage bool
	// Plan: everything Manage does, and also who can see and join the
	// fleet, its commanders and its share link.
	Plan      bool
	SeeNames  bool
	SeeFormUp bool
	// Signers are the characters that may sign up.
	Signers []accountCharacter
}

// access decides what the viewer may do with a fleet.
func (v fleetViewer) access(op db.Op, facts fleetFacts) fleetAccess {
	if facts.Unread {
		return fleetAccess{}
	}
	owner := opOwner(op)
	var viewRules, joinRules []standing.Rule
	for _, r := range facts.Rules {
		rule := standing.Rule{Kind: r.RuleKind, Ref: r.RuleRef}
		switch r.Action {
		case accessView:
			viewRules = append(viewRules, rule)
		case accessJoin:
			joinRules = append(joinRules, rule)
		}
	}
	member := v.belongs(owner)
	a := fleetAccess{}
	if v.UserID != 0 {
		a.Plan = v.plans[owner.Key()]
		a.Manage = a.Plan || v.commands(op, facts.Commanders)
	}
	switch op.Visibility {
	case fleetPublic:
		a.View = true
	case fleetUnlisted:
		a.View = facts.ViaToken || facts.SignedUp || member || a.Manage
	default:
		inView := member
		if len(viewRules) > 0 {
			inView = v.meetsAny(owner, viewRules)
		}
		a.View = a.Manage || inView || (op.JoinGate == joinByRule && len(joinRules) > 0 && v.meetsAny(owner, joinRules))
	}
	if !a.View {
		return fleetAccess{}
	}
	a.Signers = v.signers(op, owner, joinRules)
	a.Join = len(a.Signers) > 0
	a.SeeNames = op.RosterPublic || a.Manage || a.Join
	a.SeeFormUp = op.JoinGate == joinAnyone || a.Manage || a.Join
	return a
}

func (v fleetViewer) meetsAny(owner orgOwner, rules []standing.Rule) bool {
	for _, rule := range rules {
		if v.meets(owner, rule) {
			return true
		}
	}
	return false
}

// signers lists the viewer's characters that may sign up. Under a rule
// each character has to meet it alone: one character's role does not
// let its alts on the roster.
func (v fleetViewer) signers(op db.Op, owner orgOwner, joinRules []standing.Rule) []accountCharacter {
	if p := v.Pilot; p != nil {
		switch op.JoinGate {
		case joinAccount:
			return nil
		case joinByRule:
			if len(joinRules) == 0 && !v.belongs(owner) {
				return nil
			}
			if len(joinRules) > 0 && !v.meetsAny(owner, joinRules) {
				return nil
			}
		}
		return []accountCharacter{{CharacterID: p.CharacterID, Name: p.Name}}
	}
	if v.UserID == 0 {
		return nil
	}
	if op.JoinGate != joinByRule {
		return v.Characters
	}
	var out []accountCharacter
	for _, c := range v.Characters {
		ok := c.Standing.Belongs(owner)
		if len(joinRules) > 0 {
			ok = false
			for _, rule := range joinRules {
				ok = ok || c.Standing.Meets(owner, rule)
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// fleetFactsFor reads the facts for several fleets at once.
func (app *Application) fleetFactsFor(ctx context.Context, v fleetViewer, ops []db.Op) map[int64]fleetFacts {
	out := make(map[int64]fleetFacts, len(ops))
	if len(ops) == 0 {
		return out
	}
	ids := make([]int64, len(ops))
	for i, op := range ops {
		ids[i] = op.ID
		out[op.ID] = fleetFacts{}
	}
	rules, err := app.queries.ListOpAccessRulesForOps(ctx, ids)
	if err != nil {
		logging.Errorf("fleets: access rules: %v", err)
		for id := range out {
			out[id] = fleetFacts{Unread: true}
		}
		return out
	}
	for _, rule := range rules {
		f := out[rule.OpID]
		f.Rules = append(f.Rules, rule)
		out[rule.OpID] = f
	}
	commanders, err := app.queries.ListOpCommandersForOps(ctx, ids)
	if err != nil {
		logging.Errorf("fleets: commanders: %v", err)
	}
	for _, row := range commanders {
		f := out[row.OpID]
		f.Commanders = append(f.Commanders, row.CharacterID)
		out[row.OpID] = f
	}
	if characters := v.characterIDs(); len(characters) > 0 {
		answers, err := app.queries.ListSignupsOfCharacters(ctx, db.ListSignupsOfCharactersParams{OpIds: ids, CharacterIds: characters})
		if err != nil {
			logging.Errorf("fleets: sign-ups of viewer: %v", err)
		}
		for _, row := range answers {
			f := out[row.OpID]
			f.SignedUp = true
			out[row.OpID] = f
		}
	}
	return out
}

// fleetRuleChoices lists who a fleet's access can be narrowed to, from
// what the viewer's own standing already holds, so no choice scans the
// known corporations. keep carries a stored rule the viewer could not
// have picked, so another manager's choice survives an edit.
func (app *Application) fleetRuleChoices(ctx context.Context, v fleetViewer, owner orgOwner, keep ...string) []whoChoice {
	out := []whoChoice{{ruleMember, "Members of the " + owner.Kind}}
	switch owner.Kind {
	case ownerCorporation:
		for _, role := range eveCorpRoles {
			out = append(out, whoChoice{ruleEVERole + ":" + role, "In-game role: " + strings.ReplaceAll(role, "_", " ")})
		}
	case ownerAlliance:
		for _, corp := range sortedKeys(v.Standing.Corps) {
			if v.Standing.Alliance[corp] == owner.ID {
				out = append(out, whoChoice{ruleCorp + ":" + strconv.FormatInt(corp, 10), "Members of " + app.corpDisplayName(ctx, corp)})
			}
		}
	}
	groups, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("fleets: groups of %s: %v", owner.Key(), err)
	}
	for _, group := range groups {
		out = append(out, whoChoice{ruleGroup + ":" + strconv.FormatInt(group.ID, 10), "Group: " + group.Name})
	}
	for _, value := range keep {
		if value == "" || hasWhoChoice(out, value) {
			continue
		}
		kind, ref := splitRuleValue(value)
		out = append(out, whoChoice{value, app.ruleWho(ctx, owner, db.DiscordRoleRule{Kind: kind, Ref: ref})})
	}
	return out
}

func hasWhoChoice(choices []whoChoice, value string) bool {
	for _, c := range choices {
		if c.Value == value {
			return true
		}
	}
	return false
}

// splitRuleValue reads a "kind" or "kind:ref".
func splitRuleValue(value string) (kind, ref string) {
	kind, ref, _ = strings.Cut(value, ":")
	return kind, ref
}

// sortedKeys lists a set of ids in order.
func sortedKeys[V any](m map[int64]V) []int64 { return slices.Sorted(maps.Keys(m)) }

// ruleValue is a stored rule as a form value.
func ruleValue(kind, ref string) string {
	if ref == "" {
		return kind
	}
	return kind + ":" + ref
}
