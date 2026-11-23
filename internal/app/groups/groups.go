package groups

import (
	"context"
	"evesynapse/internal/humanize"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Groups (/groups): sets of characters that the directors of a
// corporation or alliance, or the leader and admins of a community,
// keep by hand. A special interest group, the fleet commanders, a
// logistics wing. A group is a list and nothing more; what it is used
// for is elsewhere: Discord roles (discord_roles.go) for a corporation
// or alliance, and who runs a programme for any owner.
//
// Who may: a director of a corporation for its groups; a director of
// the executor corporation for an alliance's; the leader or an admin
// of a community for its groups.
//
// Who can be put in a group: a character that is linked to EveSynapse
// and is in the corporation (or in a corporation of the alliance), or
// an active member of the community, with or without an account.
// ---------------------------------------------------------------------------

const (
	groupsPath       = "/groups/"
	groupNameMax     = 60
	groupDescMax     = 200
	groupsPerOwner   = 50
	groupMembersMost = 500
	// communityPickerMost is how many members a community group's
	// picker lists. Past it, a member is added by name.
	communityPickerMost = 50
	// communityGroupPage is how many of an open community group's
	// members one page shows.
	communityGroupPage = 50
)

// groupsView is the /groups page.
type groupsView struct {
	OwnerOptions []module.OwnerOption
	Owner        *groupOwnerView // the corporation or alliance the page is about
}

type groupOwnerView struct {
	Key    string
	Kind   string
	Name   string
	Groups []groupView
	// Community: the owner is a community.
	Community bool
}

type groupView struct {
	ID          int64
	Name        string
	Description string
	Count       int
	Members     []groupMemberView
	// Addable are the candidates not in the group yet.
	Addable []groupCandidate
	// Folded: a community group that is not the open one.
	Folded bool
	// ByName: more candidates than the picker lists, so members are
	// also added by typing a name. NextHref is the next page of members.
	ByName   bool
	NextHref string
}

type groupMemberView struct {
	CharacterID int64
	Name        string
	// Outside: the character has left the corporation or alliance (or
	// is no longer linked), so it counts for nothing until it is back.
	Outside bool
}

type groupCandidate struct {
	CharacterID int64
	Name        string
}

func (app *mod) handleGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}
	userID := app.deps.UserID(ctx)
	view := &groupsView{}
	var shown []standing.Owner
	if owner, options, ok := app.deps.PickOwner(ctx, r, app.deps.GroupOwners(ctx, userID)); ok {
		shown, view.OwnerOptions = []standing.Owner{owner}, options
	}
	openGroup, _ := strconv.ParseInt(r.URL.Query().Get("group"), 10, 64)
	for _, owner := range shown {
		ov := groupOwnerView{Key: owner.Key(), Kind: owner.Kind, Name: app.deps.OwnerName(ctx, owner), Community: owner.Kind == standing.OwnerCommunity}
		groups, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
		if err != nil {
			logging.Errorf("groups: groups of %s: %v", owner.Key(), err)
			data.Error = "Could not load the groups; check the server log."
		}
		// One group is open at a time, so a render reads one page of
		// members however many groups there are. A lone group opens itself.
		open := openGroup
		if open == 0 && len(groups) == 1 {
			open = groups[0].ID
		}
		for _, group := range groups {
			gv := groupView{ID: group.ID, Name: group.Name, Description: group.Description, Count: int(group.MemberCount), Folded: group.ID != open}
			if !gv.Folded {
				if ov.Community {
					app.fillCommunityGroup(ctx, &gv, owner, r.URL.Query())
				} else {
					app.fillOrgGroup(ctx, &gv, owner, r.URL.Query())
				}
			}
			ov.Groups = append(ov.Groups, gv)
		}
		view.Owner = &ov
	}
	data.Groups = view
	app.deps.Render(ctx, w, http.StatusOK, "groups.html", data)
}

// fillOrgGroup reads a corporation's or alliance's open group: one page
// of its members and the linked characters who could be added.
func (app *mod) fillOrgGroup(ctx context.Context, gv *groupView, owner standing.Owner, q url.Values) {
	corps := app.deps.OwnerCorporations(ctx, owner)
	inOwner := make(map[int64]bool, len(corps))
	for _, corp := range corps {
		inOwner[corp] = true
	}
	afterID, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	rows, err := app.queries.ListOrgGroupMembersPage(ctx, db.ListOrgGroupMembersPageParams{
		GroupID: gv.ID, AfterName: q.Get("after"), AfterID: afterID, MaxRows: communityGroupPage + 1,
	})
	if err != nil {
		logging.Errorf("groups: members of group %d: %v", gv.ID, err)
	}
	if len(rows) > communityGroupPage {
		rows = rows[:communityGroupPage]
		last := rows[len(rows)-1]
		gv.NextHref = groupsPath + "?" + url.Values{
			"owner": {owner.Key()}, "group": {strconv.FormatInt(gv.ID, 10)},
			"after": {last.SortName}, "id": {strconv.FormatInt(last.CharacterID, 10)},
		}.Encode()
	}
	for _, m := range rows {
		name := m.Name
		if name == "" {
			name = app.deps.DisplayCharacter(ctx, m.CharacterID)
		}
		gv.Members = append(gv.Members, groupMemberView{CharacterID: m.CharacterID, Name: name, Outside: m.Name == "" || !inOwner[m.CorporationID]})
	}
	if len(corps) == 0 {
		return
	}
	candidates, err := app.queries.ListOrgGroupCandidates(ctx, db.ListOrgGroupCandidatesParams{
		CorporationIds: corps, GroupID: gv.ID, MaxRows: communityPickerMost + 1,
	})
	if err != nil {
		logging.Errorf("groups: candidates of group %d: %v", gv.ID, err)
	}
	if len(candidates) > communityPickerMost {
		candidates, gv.ByName = candidates[:communityPickerMost], true
	}
	for _, c := range candidates {
		gv.Addable = append(gv.Addable, groupCandidate{CharacterID: c.CharacterID, Name: c.Name})
	}
}

// fillCommunityGroup reads the open community group: one page of its
// members and the members who could be added.
func (app *mod) fillCommunityGroup(ctx context.Context, gv *groupView, owner standing.Owner, q url.Values) {
	afterID, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	rows, err := app.queries.ListCommunityGroupMembers(ctx, db.ListCommunityGroupMembersParams{
		CommunityID: owner.ID, GroupID: gv.ID, AfterName: q.Get("after"), AfterID: afterID, MaxRows: communityGroupPage + 1,
	})
	if err != nil {
		logging.Errorf("groups: members of group %d: %v", gv.ID, err)
	}
	if len(rows) > communityGroupPage {
		rows = rows[:communityGroupPage]
		last := rows[len(rows)-1]
		gv.NextHref = groupsPath + "?" + url.Values{
			"owner": {owner.Key()}, "group": {strconv.FormatInt(gv.ID, 10)},
			"after": {last.SortName}, "id": {strconv.FormatInt(last.CharacterID, 10)},
		}.Encode()
	}
	for _, m := range rows {
		gv.Members = append(gv.Members, groupMemberView{CharacterID: m.CharacterID, Name: m.Name})
	}
	candidates, err := app.queries.ListCommunityGroupCandidates(ctx, db.ListCommunityGroupCandidatesParams{
		CommunityID: owner.ID, GroupID: gv.ID, MaxRows: communityPickerMost + 1,
	})
	if err != nil {
		logging.Errorf("groups: candidates of group %d: %v", gv.ID, err)
	}
	if len(candidates) > communityPickerMost {
		candidates, gv.ByName = candidates[:communityPickerMost], true
	}
	for _, c := range candidates {
		gv.Addable = append(gv.Addable, groupCandidate{CharacterID: c.CharacterID, Name: c.Name})
	}
}

// groupFor loads the group named in the address, for an account that
// may manage its owner.
func (app *mod) groupFor(r *http.Request, userID int64) (db.OrgGroup, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "groupID"), 10, 64)
	if err != nil {
		return db.OrgGroup{}, false
	}
	group, err := app.queries.GetOrgGroup(r.Context(), id)
	if err != nil {
		return db.OrgGroup{}, false
	}
	if !standing.Includes(app.deps.GroupOwners(r.Context(), userID), standing.Owner{Kind: group.OwnerKind, ID: group.OwnerID}) {
		return db.OrgGroup{}, false
	}
	return group, true
}

func (app *mod) groupsBack(w http.ResponseWriter, r *http.Request, message string) {
	app.deps.Flash(r.Context(), message)
	_ = r.ParseForm()
	path := standing.Address(groupsPath, r.Form.Get("owner"))
	if id, err := strconv.ParseInt(r.Form.Get("group"), 10, 64); err == nil && id > 0 && strings.Contains(path, "?") {
		path += "&group=" + strconv.FormatInt(id, 10)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (app *mod) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	_ = r.ParseForm()
	owner, ok := standing.ParseAnyOwner(r.Form.Get("owner"))
	if !ok || !standing.Includes(app.deps.GroupOwners(ctx, userID), owner) {
		app.groupsBack(w, r, "Only a director, or a community's leader or admin, can make its groups.")
		return
	}
	name := strings.Join(strings.Fields(r.Form.Get("name")), " ")
	if name == "" || len(name) > groupNameMax {
		app.groupsBack(w, r, "A group needs a name, of at most 60 characters.")
		return
	}
	existing, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err == nil && len(existing) >= groupsPerOwner {
		app.groupsBack(w, r, "That is as many groups as one "+owner.Kind+" can have.")
		return
	}
	for _, g := range existing {
		if strings.EqualFold(g.Name, name) {
			app.groupsBack(w, r, "There is already a group called "+g.Name+".")
			return
		}
	}
	if _, err := app.queries.CreateOrgGroup(ctx, db.CreateOrgGroupParams{
		OwnerKind: owner.Kind, OwnerID: owner.ID, Name: name,
		Description: humanize.Clip(strings.TrimSpace(r.Form.Get("description")), groupDescMax),
		CreatedBy:   userID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("groups: create %q for %s: %v", name, owner.Key(), err)
		app.groupsBack(w, r, "The group could not be made; check the server log.")
		return
	}
	logging.Infof("groups: user %d made group %q for %s", userID, name, owner.Key())
	app.groupsBack(w, r, "Group "+name+" made. Add its members below.")
}

// handleGroupDelete removes a group, and with it the Discord role
// rules that named it: whoever held a role only through the group
// loses it on the worker's next pass.
func (app *mod) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	group, ok := app.groupFor(r, userID)
	if !ok {
		app.groupsBack(w, r, "That group is not yours to change.")
		return
	}
	tx, err := app.deps.PGPool().Begin(ctx)
	if err == nil {
		defer tx.Rollback(ctx)
		q := app.queries.WithTx(tx)
		ref := strconv.FormatInt(group.ID, 10)
		if err = q.DeleteDiscordRoleRulesForGroup(ctx, ref); err == nil {
			err = q.DeleteOwnerPermissionsForGroup(ctx, db.DeleteOwnerPermissionsForGroupParams{OwnerKind: group.OwnerKind, OwnerID: group.OwnerID, Ref: ref})
		}
		if err == nil {
			err = q.DeleteOrgGroup(ctx, group.ID)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
	}
	if err != nil {
		logging.Errorf("groups: delete group %d: %v", group.ID, err)
		app.groupsBack(w, r, "The group could not be removed; check the server log.")
		return
	}
	logging.Infof("groups: user %d removed group %d (%s)", userID, group.ID, group.Name)
	app.groupsBack(w, r, "Group "+group.Name+" removed, with any Discord role rules and permissions that used it.")
}

// handleGroupMemberAdd puts characters in a group. Each has to be a
// linked character in the corporation, or in a corporation of the
// alliance, the group belongs to.
func (app *mod) handleGroupMemberAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	group, ok := app.groupFor(r, userID)
	if !ok || r.ParseForm() != nil {
		app.groupsBack(w, r, "That group is not yours to change.")
		return
	}
	characters := r.Form["character"]
	if name := strings.TrimSpace(r.Form.Get("name")); name != "" {
		if group.OwnerKind == standing.OwnerCommunity {
			member, err := app.queries.GetActiveCommunityMemberByName(ctx, db.GetActiveCommunityMemberByNameParams{CommunityID: group.OwnerID, Name: name})
			if err != nil {
				app.groupsBack(w, r, "No active member of this community is called "+name+".")
				return
			}
			characters = append(characters, strconv.FormatInt(member.CharacterID, 10))
		} else {
			owner := standing.Owner{Kind: group.OwnerKind, ID: group.OwnerID}
			id, err := app.queries.GetCharacterInCorporationsByName(ctx, db.GetCharacterInCorporationsByNameParams{
				CorporationIds: app.deps.OwnerCorporations(ctx, owner), Name: name,
			})
			if err != nil {
				app.groupsBack(w, r, "No linked character in this "+owner.Kind+" is called "+name+".")
				return
			}
			characters = append(characters, strconv.FormatInt(id, 10))
		}
	}
	app.groupsBack(w, r, app.addToGroup(ctx, userID, group, characters))
}

// addToGroup puts the characters named in a group and says what
// happened. Each has to be a linked character in the corporation, or in
// a corporation of the alliance, the group belongs to, or an active
// member of its community.
func (app *mod) addToGroup(ctx context.Context, userID int64, group db.OrgGroup, characters []string) string {
	owner := standing.Owner{Kind: group.OwnerKind, ID: group.OwnerID}
	allowed := map[int64]bool{}
	ids := make([]int64, 0, len(characters))
	for _, raw := range characters {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if owner.Kind == standing.OwnerCommunity {
		active, err := app.queries.ListActiveCommunityMembersIn(ctx, db.ListActiveCommunityMembersInParams{CommunityID: owner.ID, CharacterIds: ids})
		if err != nil {
			logging.Errorf("groups: members of %s: %v", owner.Key(), err)
		}
		for _, id := range active {
			allowed[id] = true
		}
	} else if corps := app.deps.OwnerCorporations(ctx, owner); len(corps) > 0 {
		rows, err := app.queries.ListCharactersAmongCorporations(ctx, db.ListCharactersAmongCorporationsParams{CorporationIds: corps, CharacterIds: ids})
		if err != nil {
			logging.Errorf("groups: characters of %s: %v", owner.Key(), err)
		}
		for _, id := range rows {
			allowed[id] = true
		}
	}
	room := groupMembersMost - int(group.MemberCount)
	added := 0
	for _, raw := range characters {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || !allowed[id] || added >= room {
			continue
		}
		if err := app.queries.AddOrgGroupMember(ctx, db.AddOrgGroupMemberParams{
			GroupID: group.ID, CharacterID: id, AddedBy: userID, AddedAt: time.Now().UTC(),
		}); err != nil {
			logging.Errorf("groups: add %d to group %d: %v", id, group.ID, err)
			continue
		}
		added++
	}
	if added == 0 {
		if owner.Kind == standing.OwnerCommunity {
			return "Nobody was added. Only active members of the community can be."
		}
		return "Nobody was added. Only characters linked to EveSynapse that are in the " + owner.Kind + " can be."
	}
	logging.Infof("groups: user %d added %d member(s) to group %d (%s)", userID, added, group.ID, group.Name)
	return strconv.Itoa(added) + " added to " + group.Name + "."
}

func (app *mod) handleGroupMemberRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	group, ok := app.groupFor(r, userID)
	if !ok || r.ParseForm() != nil {
		app.groupsBack(w, r, "That group is not yours to change.")
		return
	}
	id, _ := strconv.ParseInt(r.Form.Get("character"), 10, 64)
	if err := app.queries.RemoveOrgGroupMember(ctx, db.RemoveOrgGroupMemberParams{GroupID: group.ID, CharacterID: id}); err != nil {
		logging.Errorf("groups: remove %d from group %d: %v", id, group.ID, err)
		app.groupsBack(w, r, "That could not be saved; check the server log.")
		return
	}
	logging.Infof("groups: user %d removed character %d from group %d (%s)", userID, id, group.ID, group.Name)
	app.groupsBack(w, r, "Removed from "+group.Name+".")
}

// handleCorpMembersGroup adds the members ticked on the corporation's
// members list to one of its groups (POST /corporations/members/group).
func (app *mod) handleCorpMembersGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	_ = r.ParseForm()
	path := "/corporations/members/"
	if id, err := strconv.ParseInt(r.Form.Get("as"), 10, 64); err == nil && id > 0 {
		path += "?character=" + strconv.FormatInt(id, 10)
	}
	back := app.deps.FlashBack(w, r, path)
	id, _ := strconv.ParseInt(r.Form.Get("group"), 10, 64)
	group, err := app.queries.GetOrgGroup(ctx, id)
	if err != nil || !standing.Includes(app.deps.ManageableOwners(ctx, userID), standing.Owner{Kind: group.OwnerKind, ID: group.OwnerID}) {
		back("That group is not yours to change.")
		return
	}
	if len(r.Form["character"]) == 0 {
		back("Tick the members to add first.")
		return
	}
	back(app.addToGroup(ctx, userID, group, r.Form["character"]))
}
