// Package standing is the corporation/alliance standing model: who owns,
// directs, belongs to or holds a role in a corporation or alliance, and
// the rule kinds asked against that. It is types and checks only. It
// reads no store, calls no ESI and holds no configuration: core's loader
// works a Standing out from stored data and hands the value over, and
// modules read it through the foundation interface.
package standing

import (
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The kinds of owner. A community is a group of players outside the
// game's corporations: it has no in-game roles, CEO or member
// corporations, so a rule resting on those is never met for one.
const (
	OwnerCorporation = "corporation"
	OwnerAlliance    = "alliance"
	OwnerCommunity   = "community"
)

// The places a character can hold in a community
// (community_members.role). Stored: never rename.
const (
	CommunityLeader = "leader"
	CommunityAdmin  = "admin"
	CommunityMember = "member"
)

// CommunityRank orders the places in a community, highest first by
// value; 0 is no place at all.
func CommunityRank(role string) int {
	switch role {
	case CommunityLeader:
		return 3
	case CommunityAdmin:
		return 2
	case CommunityMember:
		return 1
	}
	return 0
}

// The kinds of rule (discord_role_rules.kind, which also stores the
// corporation permissions). Stored: never rename.
const (
	RuleLinked  = "linked"
	RuleMember  = "member"
	RuleCorp    = "corp"
	RuleCEO     = "ceo"
	RuleEVERole = "eve_role"
	RuleGroup   = "group"
)

// EVECorpRoles are the in-game corporation roles a rule can ask for, as
// ESI names them. Director is how a CEO's deputies are told apart; the
// rest are the ones corporations hand out for a job.
var EVECorpRoles = []string{
	"Director", "Personnel_Manager", "Accountant", "Junior_Accountant", "Security_Officer",
	"Station_Manager", "Factory_Manager", "Fitting_Manager", "Contract_Manager",
	"Communications_Officer", "Diplomat", "Trader", "Auditor", "Skill_Plan_Manager", "Brand_Manager",
}

// ValidEVECorpRole reports whether name is a role a rule can ask for.
func ValidEVECorpRole(name string) bool {
	return slices.Contains(EVECorpRoles, name)
}

// HoldsDirector reports whether a character's corporation roles include
// Director: the in-game role itself, not a setting.
func HoldsDirector(roles []string) bool {
	for _, role := range roles {
		if strings.EqualFold(role, "Director") {
			return true
		}
	}
	return false
}

// Owner is a corporation, alliance or community that things (servers,
// groups, programmes) belong to.
type Owner struct {
	Kind string
	ID   int64
}

// CorporationOwner is a corporation as an owner.
func CorporationOwner(id int64) Owner { return Owner{Kind: OwnerCorporation, ID: id} }

// Key is the owner as one string ("corporation:98000001").
func (o Owner) Key() string { return o.Kind + ":" + strconv.FormatInt(o.ID, 10) }

// ParseOwner reads the Key of a corporation or alliance, the only
// owners of Discord servers and intel channels.
func ParseOwner(raw string) (Owner, bool) {
	o, ok := ParseAnyOwner(raw)
	if !ok || (o.Kind != OwnerCorporation && o.Kind != OwnerAlliance) {
		return Owner{}, false
	}
	return o, true
}

// ParseAnyOwner reads the Key of any kind of owner, communities
// included.
func ParseAnyOwner(raw string) (Owner, bool) {
	kind, id, found := strings.Cut(raw, ":")
	n, err := strconv.ParseInt(id, 10, 64)
	if !found || err != nil || n <= 0 {
		return Owner{}, false
	}
	switch kind {
	case OwnerCorporation, OwnerAlliance, OwnerCommunity:
		return Owner{Kind: kind, ID: n}, true
	}
	return Owner{}, false
}

// Address is a page's address for one owner, named by its key; the
// plain page where the key is not one.
func Address(path, key string) string {
	if _, ok := ParseAnyOwner(key); !ok {
		return path
	}
	return path + "?owner=" + url.QueryEscape(key)
}

// Includes reports whether o is among owners.
func Includes(owners []Owner, o Owner) bool {
	return slices.Contains(owners, o)
}

// Rule is one "who" asked against a standing: a kind from the rule
// kinds and, for some, a reference (a corporation id, a role name, a
// group id).
type Rule struct {
	Kind string
	Ref  string
}

// Standing is what an account's working characters amount to. The
// loader (core) fills it; the checks below only read it.
type Standing struct {
	Working   bool
	Corps     map[int64]bool            // corporations a character is in
	Alliance  map[int64]int64           // corporation -> its alliance, where known
	CEOOf     map[int64]bool            // corporations a character is CEO of
	EVERoles  map[int64]map[string]bool // corporation -> in-game roles held there (lower case)
	Groups    map[int64]bool            // groups a character is in, and still belongs to the owner of
	Alliances map[int64]bool
	// Communities is the highest place a working character holds in
	// each open community it is an active member of.
	Communities map[int64]string
}

// New is an empty Standing with its maps made.
func New() Standing {
	return Standing{
		Corps: map[int64]bool{}, Alliance: map[int64]int64{}, Alliances: map[int64]bool{},
		CEOOf: map[int64]bool{}, EVERoles: map[int64]map[string]bool{}, Groups: map[int64]bool{},
		Communities: map[int64]string{},
	}
}

// Merge adds what another standing holds to this one: an account's
// standing is what its characters hold between them.
func (st *Standing) Merge(other Standing) {
	st.Working = st.Working || other.Working
	maps.Copy(st.Corps, other.Corps)
	maps.Copy(st.Alliance, other.Alliance)
	maps.Copy(st.Alliances, other.Alliances)
	maps.Copy(st.CEOOf, other.CEOOf)
	maps.Copy(st.Groups, other.Groups)
	for corp, roles := range other.EVERoles {
		if st.EVERoles[corp] == nil {
			st.EVERoles[corp] = map[string]bool{}
		}
		maps.Copy(st.EVERoles[corp], roles)
	}
	for community, role := range other.Communities {
		st.NoteCommunity(community, role)
	}
}

// NoteCommunity records a place held in a community, keeping the
// highest.
func (st Standing) NoteCommunity(community int64, role string) {
	if CommunityRank(role) > CommunityRank(st.Communities[community]) {
		st.Communities[community] = role
	}
}

// ManagesCommunity reports whether a working character leads or is an
// admin of a community.
func (st Standing) ManagesCommunity(community int64) bool {
	return st.Working && CommunityRank(st.Communities[community]) >= CommunityRank(CommunityAdmin)
}

// Directs reports whether the account has a director, or the CEO, of
// a corporation.
func (st Standing) Directs(corp int64) bool {
	return st.Working && (st.CEOOf[corp] || st.EVERoles[corp]["director"])
}

// InCorporation reports whether a working character is in corp.
func (st Standing) InCorporation(corp int64) bool {
	return st.Working && st.Corps[corp]
}

// Within reports whether a corporation the account has a character in
// is the owner, or is in the owner alliance. No corporation is within
// any other kind of owner, whatever its ID.
func (st Standing) Within(owner Owner, corp int64) bool {
	switch owner.Kind {
	case OwnerCorporation:
		return corp == owner.ID
	case OwnerAlliance:
		return st.Alliance[corp] == owner.ID
	}
	return false
}

// Meets reports whether the account satisfies one rule of something
// belonging to owner.
func (st Standing) Meets(owner Owner, rule Rule) bool {
	if !st.Working {
		return false
	}
	switch rule.Kind {
	case RuleLinked:
		return true
	case RuleMember:
		return st.Belongs(owner)
	case RuleCorp:
		corp, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return corp != 0 && st.Corps[corp] && st.Within(owner, corp)
	case RuleCEO:
		for corp := range st.CEOOf {
			if st.Within(owner, corp) {
				return true
			}
		}
	case RuleEVERole:
		want := strings.ToLower(rule.Ref)
		for corp, roles := range st.EVERoles {
			if roles[want] && st.Within(owner, corp) {
				return true
			}
		}
	case RuleGroup:
		group, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return st.Groups[group]
	}
	return false
}

// Belongs reports whether one of the account's working characters is in
// the owner corporation, in a corporation of the owner alliance, or an
// active member of the owner community.
func (st Standing) Belongs(owner Owner) bool {
	if !st.Working {
		return false
	}
	if owner.Kind == OwnerCommunity {
		return st.Communities[owner.ID] != ""
	}
	for corp := range st.Corps {
		if st.Within(owner, corp) {
			return true
		}
	}
	return false
}
