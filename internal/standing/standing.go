// Package standing is the corporation/alliance standing model: who owns,
// directs, belongs to or holds a role in a corporation or alliance, and
// the rule kinds asked against that. It is types and checks only. It
// reads no store, calls no ESI and holds no configuration: core's loader
// works a Standing out from stored data and hands the value over, and
// modules read it through the foundation interface.
package standing

import (
	"strconv"
	"strings"
)

// The kinds of owner.
const (
	OwnerCorporation = "corporation"
	OwnerAlliance    = "alliance"
)

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
	for _, role := range EVECorpRoles {
		if role == name {
			return true
		}
	}
	return false
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

// Owner is a corporation or alliance that things (servers, groups)
// belong to.
type Owner struct {
	Kind string
	ID   int64
}

// Key is the owner as one string ("corporation:98000001").
func (o Owner) Key() string { return o.Kind + ":" + strconv.FormatInt(o.ID, 10) }

// ParseOwner reads a Key back.
func ParseOwner(raw string) (Owner, bool) {
	kind, id, found := strings.Cut(raw, ":")
	n, err := strconv.ParseInt(id, 10, 64)
	if !found || err != nil || n <= 0 || (kind != OwnerCorporation && kind != OwnerAlliance) {
		return Owner{}, false
	}
	return Owner{Kind: kind, ID: n}, true
}

// Includes reports whether o is among owners.
func Includes(owners []Owner, o Owner) bool {
	for _, owner := range owners {
		if owner == o {
			return true
		}
	}
	return false
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
}

// New is an empty Standing with its maps made.
func New() Standing {
	return Standing{
		Corps: map[int64]bool{}, Alliance: map[int64]int64{}, Alliances: map[int64]bool{},
		CEOOf: map[int64]bool{}, EVERoles: map[int64]map[string]bool{}, Groups: map[int64]bool{},
	}
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
// is the owner, or is in the owner alliance.
func (st Standing) Within(owner Owner, corp int64) bool {
	if owner.Kind == OwnerCorporation {
		return corp == owner.ID
	}
	return st.Alliance[corp] == owner.ID
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
		for corp := range st.Corps {
			if st.Within(owner, corp) {
				return true
			}
		}
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
// the owner corporation, or in a corporation of the owner alliance.
func (st Standing) Belongs(owner Owner) bool {
	if !st.Working {
		return false
	}
	for corp := range st.Corps {
		if st.Within(owner, corp) {
			return true
		}
	}
	return false
}
