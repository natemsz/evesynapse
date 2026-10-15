package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Standing: who directs, belongs to or holds a role in a corporation or
// alliance. An "owner" is a corporation or alliance; an account's
// "standing" is what its working characters amount to (corporations,
// alliances, CEO-ships, in-game roles, groups). Rules ("who") are asked
// against a standing, and the same model decides who may manage the
// corporation's pages (corp_permissions.go) and which Discord roles an
// account is owed (the Discord module).
//
// This is foundation, not part of any module: the corporation pages
// depend on it, and so does Discord. Rule kinds are stored in
// discord_role_rules.kind (never renamed); the table also keeps the
// corporation permissions, which is why the Discord-named storage
// remains.
// ---------------------------------------------------------------------------

// The kinds of owner.
const (
	ownerCorporation = "corporation"
	ownerAlliance    = "alliance"
)

// orgOwner is a corporation or alliance that servers belong to.
type orgOwner struct {
	Kind string
	ID   int64
}

func (o orgOwner) key() string { return o.Kind + ":" + strconv.FormatInt(o.ID, 10) }

func parseOrgOwner(raw string) (orgOwner, bool) {
	kind, id, found := strings.Cut(raw, ":")
	n, err := strconv.ParseInt(id, 10, 64)
	if !found || err != nil || n <= 0 || (kind != ownerCorporation && kind != ownerAlliance) {
		return orgOwner{}, false
	}
	return orgOwner{Kind: kind, ID: n}, true
}

// corpAlliance reports the alliance a corporation is in and that
// alliance's executor corporation, from the stored corporation record.
// Both are 0 for a corporation in no alliance, or one whose record has
// not been fetched yet; the record is asked for then, so that a later
// look finds it.
func (app *Application) corpAlliance(ctx context.Context, corpID int64) (allianceID, executorCorpID int64) {
	rec, err := app.queries.GetCorporationRecord(ctx, corpID)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" {
		app.notePageWantFromContext(ctx, pageWantCorporation, corpID)
		return 0, 0
	}
	var payload corporationRecordPayload
	if json.Unmarshal([]byte(rec.Payload), &payload) != nil || payload.Corp.AllianceID == 0 {
		return 0, 0
	}
	return payload.Corp.AllianceID, payload.Alliance.ExecutorCorporationID
}

// manageableOwners lists the corporations and alliances whose servers
// an account may manage, in the order of its characters.
func (app *Application) manageableOwners(ctx context.Context, userID int64) []orgOwner {
	rows, err := app.queries.ListCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("discord: characters of user %d: %v", userID, err)
		return nil
	}
	working := map[int64]bool{}
	if characters, cerr := app.queries.ListCharactersByUser(ctx, userID); cerr == nil {
		for _, ch := range characters {
			working[ch.CharacterID] = ch.LinkState == "ok"
		}
	}
	seen := map[string]bool{}
	var out []orgOwner
	add := func(o orgOwner) {
		if !seen[o.key()] {
			seen[o.key()] = true
			out = append(out, o)
		}
	}
	for _, row := range rows {
		if !working[row.CharacterID] || row.CorporationID == 0 {
			continue
		}
		var roles esi.CharacterRoles
		if !app.loadCorpSnapshot(ctx, row.CharacterID, esi.SnapCorpRoles, &roles) || !holdsDirector(roles.Roles) {
			continue
		}
		add(orgOwner{ownerCorporation, row.CorporationID})
		if alliance, executor := app.corpAlliance(ctx, row.CorporationID); alliance != 0 && executor == row.CorporationID {
			add(orgOwner{ownerAlliance, alliance})
		}
	}
	return out
}

// holdsDirector reports whether a character's corporation roles
// include Director. This is the in-game role itself, not the
// OPS_MANAGER_ROLES setting: who runs a Discord server is not a
// thing to widen by configuration.
func holdsDirector(roles []string) bool {
	for _, role := range roles {
		if strings.EqualFold(role, "Director") {
			return true
		}
	}
	return false
}

func ownersInclude(owners []orgOwner, o orgOwner) bool {
	for _, owner := range owners {
		if owner == o {
			return true
		}
	}
	return false
}

// ownerName is how an owner is shown.
func (app *Application) ownerName(ctx context.Context, o orgOwner) string {
	if o.Kind == ownerAlliance {
		if name, ok := app.resolvedAllianceName(ctx, o.ID); ok && name != "" {
			return name
		}
		return fmt.Sprintf("Alliance #%d", o.ID)
	}
	return app.corpDisplayName(ctx, o.ID)
}

// eveCorpRoles are the in-game corporation roles a rule can ask for,
// as ESI names them. Director is how a CEO's deputies are told apart;
// the rest are the ones corporations hand out for a job.
var eveCorpRoles = []string{
	"Director", "Personnel_Manager", "Accountant", "Junior_Accountant", "Security_Officer",
	"Station_Manager", "Factory_Manager", "Fitting_Manager", "Contract_Manager",
	"Communications_Officer", "Diplomat", "Trader", "Auditor", "Skill_Plan_Manager", "Brand_Manager",
}

func validEVECorpRole(name string) bool {
	for _, role := range eveCorpRoles {
		if role == name {
			return true
		}
	}
	return false
}

// whoChoice is one thing a new rule can be about. Value is
// "kind" or "kind:ref".
type whoChoice struct {
	Value, Label string
}

// ownerCorporations lists the corporations that belong to an owner: the
// corporation itself, or the corporations of the alliance that
// EveSynapse has linked characters in.
func (app *Application) ownerCorporations(ctx context.Context, owner orgOwner) []int64 {
	if owner.Kind == ownerCorporation {
		return []int64{owner.ID}
	}
	known, err := app.queries.ListKnownCorporations(ctx)
	if err != nil {
		logging.Errorf("discord: known corporations: %v", err)
		return nil
	}
	var out []int64
	for _, corp := range known {
		if alliance, _ := app.corpAlliance(ctx, corp); alliance == owner.ID {
			out = append(out, corp)
		}
	}
	return out
}

// whoChoices lists what a rule in an owner's server can be about.
func (app *Application) whoChoices(ctx context.Context, owner orgOwner) []whoChoice {
	out := []whoChoice{
		{ruleLinked, "Everyone who has connected EveSynapse"},
		{ruleMember, "Members of the " + owner.Kind},
		{ruleCEO, "CEO"},
	}
	if owner.Kind == ownerAlliance {
		out[2].Label = "CEOs of its corporations"
		for _, corp := range app.ownerCorporations(ctx, owner) {
			out = append(out, whoChoice{ruleCorp + ":" + strconv.FormatInt(corp, 10), "Members of " + app.corpDisplayName(ctx, corp)})
		}
	}
	for _, role := range eveCorpRoles {
		out = append(out, whoChoice{ruleEVERole + ":" + role, "In-game role: " + strings.ReplaceAll(role, "_", " ")})
	}
	groups, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("discord: groups of %s: %v", owner.key(), err)
	}
	for _, group := range groups {
		out = append(out, whoChoice{ruleGroup + ":" + strconv.FormatInt(group.ID, 10), "Group: " + group.Name})
	}
	return out
}

// ruleWho says in words who a stored rule is about.
func (app *Application) ruleWho(ctx context.Context, owner orgOwner, rule db.DiscordRoleRule) string {
	switch rule.Kind {
	case ruleLinked:
		return "Everyone who has connected EveSynapse"
	case ruleMember:
		return "Members of the " + owner.Kind
	case ruleCEO:
		if owner.Kind == ownerAlliance {
			return "CEOs of its corporations"
		}
		return "CEO"
	case ruleCorp:
		corp, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return "Members of " + app.corpDisplayName(ctx, corp)
	case ruleEVERole:
		return "In-game role: " + strings.ReplaceAll(rule.Ref, "_", " ")
	case ruleGroup:
		id, _ := strconv.ParseInt(rule.Ref, 10, 64)
		if group, err := app.queries.GetOrgGroup(ctx, id); err == nil {
			return "Group: " + group.Name
		}
		return "A group that no longer exists"
	}
	return rule.Kind
}

// parseRuleWho reads a "kind" or "kind:ref" from the form and checks
// it is something a rule in this owner's server may be about: a
// corporation has to belong to the owner, a group has to be the
// owner's own, an in-game role has to be one that exists.
func (app *Application) parseRuleWho(ctx context.Context, owner orgOwner, raw string) (kind, ref string, ok bool) {
	kind, ref, _ = strings.Cut(raw, ":")
	switch kind {
	case ruleLinked, ruleMember, ruleCEO:
		return kind, "", ref == ""
	case ruleEVERole:
		return kind, ref, validEVECorpRole(ref)
	case ruleCorp:
		corp, err := strconv.ParseInt(ref, 10, 64)
		if err != nil || corp <= 0 {
			return "", "", false
		}
		for _, mine := range app.ownerCorporations(ctx, owner) {
			if mine == corp {
				return kind, strconv.FormatInt(corp, 10), true
			}
		}
	case ruleGroup:
		id, err := strconv.ParseInt(ref, 10, 64)
		if err != nil {
			return "", "", false
		}
		group, gerr := app.queries.GetOrgGroup(ctx, id)
		if gerr == nil && group.OwnerKind == owner.Kind && group.OwnerID == owner.ID {
			return kind, strconv.FormatInt(id, 10), true
		}
	}
	return "", "", false
}

// The kinds of rule (discord_role_rules.kind). Stored: never rename.
const (
	ruleLinked  = "linked"
	ruleMember  = "member"
	ruleCorp    = "corp"
	ruleCEO     = "ceo"
	ruleEVERole = "eve_role"
	ruleGroup   = "group"
)

// orgStanding is what an account's working characters amount to.
type orgStanding struct {
	working   bool
	corps     map[int64]bool            // corporations a character is in
	alliance  map[int64]int64           // corporation -> its alliance, where known
	ceoOf     map[int64]bool            // corporations a character is CEO of
	eveRoles  map[int64]map[string]bool // corporation -> in-game roles held there (lower case)
	groups    map[int64]bool            // groups a character is in, and still belongs to the owner of
	alliances map[int64]bool
}

// corpCEO reports a corporation's CEO from its stored record; 0 when
// the record has not been fetched.
func (app *Application) corpCEO(ctx context.Context, corpID int64) int64 {
	rec, err := app.queries.GetCorporationRecord(ctx, corpID)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" {
		return 0
	}
	var payload corporationRecordPayload
	if json.Unmarshal([]byte(rec.Payload), &payload) != nil {
		return 0
	}
	return payload.Corp.CEOID
}

// standingFor works out an account's standing from stored data.
// A corporation whose alliance is not known yet counts for itself
// only.
func (app *Application) standingFor(ctx context.Context, userID int64) (orgStanding, error) {
	st := orgStanding{
		corps: map[int64]bool{}, alliance: map[int64]int64{}, alliances: map[int64]bool{},
		ceoOf: map[int64]bool{}, eveRoles: map[int64]map[string]bool{}, groups: map[int64]bool{},
	}
	n, err := app.queries.CountLinkedCharactersByUser(ctx, userID)
	if err != nil || n == 0 {
		return st, err
	}
	st.working = true
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		return st, err
	}
	corpOf := map[int64]int64{} // character -> corporation
	for _, row := range rows {
		corp := row.CorporationID
		if corp == 0 {
			continue
		}
		corpOf[row.CharacterID] = corp
		if !st.corps[corp] {
			st.corps[corp] = true
			if alliance, _ := app.corpAlliance(ctx, corp); alliance != 0 {
				st.alliance[corp] = alliance
				st.alliances[alliance] = true
			}
		}
		if app.corpCEO(ctx, corp) == row.CharacterID {
			st.ceoOf[corp] = true
		}
		var roles esi.CharacterRoles
		if app.loadCorpSnapshot(ctx, row.CharacterID, esi.SnapCorpRoles, &roles) {
			for _, role := range roles.Roles {
				if st.eveRoles[corp] == nil {
					st.eveRoles[corp] = map[string]bool{}
				}
				st.eveRoles[corp][strings.ToLower(role)] = true
			}
		}
	}
	memberships, err := app.queries.ListOrgGroupsByUser(ctx, userID)
	if err != nil {
		return st, err
	}
	for _, m := range memberships {
		// A character counts for a group only while it is in the
		// corporation or alliance the group belongs to.
		corp := corpOf[m.CharacterID]
		if corp != 0 && st.within(orgOwner{m.OwnerKind, m.OwnerID}, corp) {
			st.groups[m.GroupID] = true
		}
	}
	return st, nil
}

// within reports whether a corporation the account has a character in
// is the owner, or is in the owner alliance.
func (st orgStanding) within(owner orgOwner, corp int64) bool {
	if owner.Kind == ownerCorporation {
		return corp == owner.ID
	}
	return st.alliance[corp] == owner.ID
}

// meets reports whether the account satisfies one rule of a server
// belonging to owner.
func (st orgStanding) meets(owner orgOwner, rule db.DiscordRoleRule) bool {
	if !st.working {
		return false
	}
	switch rule.Kind {
	case ruleLinked:
		return true
	case ruleMember:
		for corp := range st.corps {
			if st.within(owner, corp) {
				return true
			}
		}
	case ruleCorp:
		corp, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return corp != 0 && st.corps[corp] && st.within(owner, corp)
	case ruleCEO:
		for corp := range st.ceoOf {
			if st.within(owner, corp) {
				return true
			}
		}
	case ruleEVERole:
		want := strings.ToLower(rule.Ref)
		for corp, roles := range st.eveRoles {
			if roles[want] && st.within(owner, corp) {
				return true
			}
		}
	case ruleGroup:
		group, _ := strconv.ParseInt(rule.Ref, 10, 64)
		return st.groups[group]
	}
	return false
}

// belongs reports whether one of the account's working characters is
// in the owner corporation, or in a corporation of the owner alliance.
func (st orgStanding) belongs(owner orgOwner) bool {
	if !st.working {
		return false
	}
	for corp := range st.corps {
		if st.within(owner, corp) {
			return true
		}
	}
	return false
}
