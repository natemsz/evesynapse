package app

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// The standing model itself is the pure package internal/standing; core
// keeps its names for it and the loader (standingFor) below.
type (
	orgOwner    = standing.Owner
	orgStanding = standing.Standing
)

const (
	ownerCorporation = standing.OwnerCorporation
	ownerAlliance    = standing.OwnerAlliance
	ownerCommunity   = standing.OwnerCommunity

	ruleLinked  = standing.RuleLinked
	ruleMember  = standing.RuleMember
	ruleCorp    = standing.RuleCorp
	ruleCEO     = standing.RuleCEO
	ruleEVERole = standing.RuleEVERole
	ruleGroup   = standing.RuleGroup
)

var eveCorpRoles = standing.EVECorpRoles

func validEVECorpRole(name string) bool                { return standing.ValidEVECorpRole(name) }
func holdsDirector(roles []string) bool                { return standing.HoldsDirector(roles) }
func parseOrgOwner(raw string) (orgOwner, bool)        { return standing.ParseOwner(raw) }
func parseAnyOwner(raw string) (orgOwner, bool)        { return standing.ParseAnyOwner(raw) }
func ownersInclude(owners []orgOwner, o orgOwner) bool { return standing.Includes(owners, o) }

// ---------------------------------------------------------------------------
// Standing: who directs, belongs to or holds a role in an owner. An
// "owner" is a corporation, alliance or community; an account's
// "standing" is what its working characters amount to (corporations,
// alliances, CEO-ships, in-game roles, groups). Rules ("who") are asked
// against a standing, and the same model decides who may run an owner's
// programmes (owner_permissions.go) and which Discord roles an account
// is owed (the Discord module).
//
// This is foundation, not part of any module: the corporation pages
// depend on it, and so does Discord. Rule kinds are stored in
// discord_role_rules.kind and owner_permissions.kind (never renamed).
// ---------------------------------------------------------------------------

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
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("discord: characters of user %d: %v", userID, err)
		return nil
	}
	seen := map[string]bool{}
	var out []orgOwner
	add := func(o orgOwner) {
		if !seen[o.Key()] {
			seen[o.Key()] = true
			out = append(out, o)
		}
	}
	for _, row := range rows {
		if row.CorporationID == 0 {
			continue
		}
		var roles esi.CharacterRoles
		if !app.loadCorpSnapshot(ctx, row.CharacterID, esi.SnapCorpRoles, &roles) || !holdsDirector(roles.Roles) {
			continue
		}
		add(orgOwner{Kind: ownerCorporation, ID: row.CorporationID})
		if alliance, executor := app.corpAlliance(ctx, row.CorporationID); alliance != 0 && executor == row.CorporationID {
			add(orgOwner{Kind: ownerAlliance, ID: alliance})
		}
	}
	return out
}

// ownerName is how an owner is shown.
func (app *Application) ownerName(ctx context.Context, o orgOwner) string {
	switch o.Kind {
	case ownerAlliance:
		if name, ok := app.resolvedAllianceName(ctx, o.ID); ok && name != "" {
			return name
		}
		return fmt.Sprintf("Alliance #%d", o.ID)
	case ownerCommunity:
		if community, err := app.queries.GetCommunity(ctx, o.ID); err == nil {
			return community.Name
		}
		return fmt.Sprintf("Community #%d", o.ID)
	}
	return app.corpDisplayName(ctx, o.ID)
}

// whoChoice is one thing a new rule can be about. Value is
// "kind" or "kind:ref".
type whoChoice struct {
	Value, Label string
}

// ownerCorporations lists the corporations that belong to an owner: the
// corporation itself, or the corporations of the alliance that
// EveSynapse has linked characters in. No other kind of owner has any.
func (app *Application) ownerCorporations(ctx context.Context, owner orgOwner) []int64 {
	switch owner.Kind {
	case ownerCorporation:
		return []int64{owner.ID}
	case ownerAlliance:
	default:
		return nil
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
	var out []whoChoice
	switch owner.Kind {
	case ownerCommunity:
		// A community has no in-game roles, CEO or corporations.
		out = []whoChoice{{ruleMember, "Members of the community"}}
	default:
		out = []whoChoice{
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
	}
	groups, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("discord: groups of %s: %v", owner.Key(), err)
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
	if owner.Kind == ownerCommunity && kind != ruleMember && kind != ruleGroup {
		return "", "", false
	}
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

// standingFor works out an account's standing from stored data: what
// its working characters hold between them. A corporation whose
// alliance is not known yet counts for itself only.
func (app *Application) standingFor(ctx context.Context, userID int64) (orgStanding, error) {
	st, _, err := app.standingWithCharacters(ctx, userID)
	return st, err
}

// standingWithCharacters is an account's standing and the characters it
// is made of, from one read of them.
func (app *Application) standingWithCharacters(ctx context.Context, userID int64) (orgStanding, []accountCharacter, error) {
	st := standing.New()
	n, err := app.queries.CountLinkedCharactersByUser(ctx, userID)
	if err != nil || n == 0 {
		return st, nil, err
	}
	st.Working = true
	characters, err := app.accountCharacters(ctx, userID)
	if err != nil {
		return st, nil, err
	}
	for _, c := range characters {
		st.Merge(c.Standing)
	}
	return st, characters, nil
}

// accountCharacter is one of an account's working characters with the
// standing it has alone, for what a character does in its own name.
type accountCharacter struct {
	CharacterID   int64
	CorporationID int64
	Name          string
	Standing      orgStanding
}

// accountCharacters lists an account's working characters, each with
// its own standing, from stored data.
func (app *Application) accountCharacters(ctx context.Context, userID int64) ([]accountCharacter, error) {
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]accountCharacter, 0, len(rows))
	at := make(map[int64]int, len(rows))
	alliances := map[int64]int64{}
	ceos := map[int64]int64{}
	for _, row := range rows {
		st := standing.New()
		st.Working = true
		if corp := row.CorporationID; corp != 0 {
			st.Corps[corp] = true
			alliance, known := alliances[corp]
			if !known {
				alliance, _ = app.corpAlliance(ctx, corp)
				alliances[corp] = alliance
				ceos[corp] = app.corpCEO(ctx, corp)
			}
			if alliance != 0 {
				st.Alliance[corp] = alliance
				st.Alliances[alliance] = true
			}
			if ceos[corp] == row.CharacterID {
				st.CEOOf[corp] = true
			}
			var roles esi.CharacterRoles
			if app.loadCorpSnapshot(ctx, row.CharacterID, esi.SnapCorpRoles, &roles) {
				for _, role := range roles.Roles {
					if st.EVERoles[corp] == nil {
						st.EVERoles[corp] = map[string]bool{}
					}
					st.EVERoles[corp][strings.ToLower(role)] = true
				}
			}
		}
		at[row.CharacterID] = len(out)
		out = append(out, accountCharacter{CharacterID: row.CharacterID, CorporationID: row.CorporationID, Name: row.Name, Standing: st})
	}
	places, err := app.queries.ListCommunityMembershipsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	inCommunity := make(map[[2]int64]bool, len(places)) // {character, community}
	for _, place := range places {
		i, known := at[place.CharacterID]
		if !known {
			// A character can hold a community place before its
			// corporation is stored.
			st := standing.New()
			st.Working = true
			i = len(out)
			at[place.CharacterID] = i
			out = append(out, accountCharacter{CharacterID: place.CharacterID, Name: app.displayCharacter(ctx, place.CharacterID), Standing: st})
		}
		out[i].Standing.NoteCommunity(place.CommunityID, place.Role)
		inCommunity[[2]int64{place.CharacterID, place.CommunityID}] = true
	}
	memberships, err := app.queries.ListOrgGroupsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, m := range memberships {
		i, known := at[m.CharacterID]
		if !known {
			continue
		}
		c := &out[i]
		// A character counts for a community's group only while it is
		// an active member of that community, and for any other group
		// only while it is in the corporation or alliance the group
		// belongs to.
		if m.OwnerKind == ownerCommunity {
			if inCommunity[[2]int64{m.CharacterID, m.OwnerID}] {
				c.Standing.Groups[m.GroupID] = true
			}
			continue
		}
		if c.CorporationID != 0 && c.Standing.Within(orgOwner{Kind: m.OwnerKind, ID: m.OwnerID}, c.CorporationID) {
			c.Standing.Groups[m.GroupID] = true
		}
	}
	return out, nil
}
