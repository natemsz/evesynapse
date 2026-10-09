package app

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Groups (/groups): sets of characters that the directors of a
// corporation or alliance keep by hand. A special interest group, the
// fleet commanders, a logistics wing. A group is a list and nothing
// more; what it is used for is elsewhere, and at present that is
// Discord roles (discord_roles.go): "whoever is in this group gets
// that role".
//
// Who may: the same people who may manage the owner's Discord servers
// (discord_servers.go). A director of a corporation for its groups; a
// director of the executor corporation for an alliance's.
//
// Who can be put in a group: a character that is linked to EveSynapse
// and is in the corporation (or in a corporation of the alliance).
// Somebody who has never signed in cannot be given a Discord role in
// any case, since nothing connects them to a Discord account.
// ---------------------------------------------------------------------------

const (
	groupsPath       = "/groups/"
	groupNameMax     = 60
	groupDescMax     = 200
	groupsPerOwner   = 50
	groupMembersMost = 500
)

// groupsView is the /groups page.
type groupsView struct {
	Owners []groupOwnerView
}

type groupOwnerView struct {
	Key    string
	Kind   string
	Name   string
	Groups []groupView
	// Candidates are the characters that can be added to this owner's
	// groups.
	Candidates []groupCandidate
}

type groupView struct {
	ID          int64
	Name        string
	Description string
	Members     []groupMemberView
	// Addable are the candidates not in the group yet.
	Addable []groupCandidate
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

func (app *Application) handleGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	view := &groupsView{}
	for _, owner := range app.discordManageable(ctx, userID) {
		ov := groupOwnerView{Key: owner.key(), Kind: owner.Kind, Name: app.ownerName(ctx, owner)}
		corps := app.ownerCorporations(ctx, owner)
		inside := map[int64]bool{}
		if len(corps) > 0 {
			rows, err := app.queries.ListCharactersInCorporations(ctx, corps)
			if err != nil {
				logging.Errorf("groups: characters of %s: %v", owner.key(), err)
			}
			for _, row := range rows {
				inside[row.CharacterID] = true
				ov.Candidates = append(ov.Candidates, groupCandidate{CharacterID: row.CharacterID, Name: row.Name})
			}
		}
		groups, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
		if err != nil {
			logging.Errorf("groups: groups of %s: %v", owner.key(), err)
			data.Error = "Could not load the groups; check the server log."
		}
		for _, group := range groups {
			gv := groupView{ID: group.ID, Name: group.Name, Description: group.Description}
			members, err := app.queries.ListOrgGroupMembers(ctx, group.ID)
			if err != nil {
				logging.Errorf("groups: members of group %d: %v", group.ID, err)
			}
			in := map[int64]bool{}
			for _, m := range members {
				in[m.CharacterID] = true
				name := m.Name
				if name == "" {
					name = app.displayCharacter(ctx, m.CharacterID)
				}
				gv.Members = append(gv.Members, groupMemberView{CharacterID: m.CharacterID, Name: name, Outside: !inside[m.CharacterID]})
			}
			for _, c := range ov.Candidates {
				if !in[c.CharacterID] {
					gv.Addable = append(gv.Addable, c)
				}
			}
			ov.Groups = append(ov.Groups, gv)
		}
		view.Owners = append(view.Owners, ov)
	}
	data.Groups = view
	app.render(ctx, w, http.StatusOK, "groups.html", data)
}

// groupFor loads the group named in the address, for an account that
// may manage its owner.
func (app *Application) groupFor(r *http.Request, userID int64) (db.OrgGroup, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "groupID"), 10, 64)
	if err != nil {
		return db.OrgGroup{}, false
	}
	group, err := app.queries.GetOrgGroup(r.Context(), id)
	if err != nil {
		return db.OrgGroup{}, false
	}
	if !discordManages(app.discordManageable(r.Context(), userID), discordOwner{group.OwnerKind, group.OwnerID}) {
		return db.OrgGroup{}, false
	}
	return group, true
}

func (app *Application) groupsBack(w http.ResponseWriter, r *http.Request, message string) {
	app.flash(r.Context(), message)
	http.Redirect(w, r, groupsPath, http.StatusSeeOther)
}

func (app *Application) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	_ = r.ParseForm()
	owner, ok := parseDiscordOwner(r.Form.Get("owner"))
	if !ok || !discordManages(app.discordManageable(ctx, userID), owner) {
		app.groupsBack(w, r, "Only a director can make groups for that corporation or alliance.")
		return
	}
	name := strings.Join(strings.Fields(r.Form.Get("name")), " ")
	if name == "" || len(name) > groupNameMax {
		app.groupsBack(w, r, "A group needs a name, of at most 60 characters.")
		return
	}
	existing, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err == nil && len(existing) >= groupsPerOwner {
		app.groupsBack(w, r, "That is as many groups as one corporation or alliance can have.")
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
		Description: clip(strings.TrimSpace(r.Form.Get("description")), groupDescMax),
		CreatedBy:   userID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("groups: create %q for %s: %v", name, owner.key(), err)
		app.groupsBack(w, r, "The group could not be made; check the server log.")
		return
	}
	logging.Infof("groups: user %d made group %q for %s", userID, name, owner.key())
	app.groupsBack(w, r, "Group "+name+" made. Add its members below.")
}

// handleGroupDelete removes a group, and with it the Discord role
// rules that named it: whoever held a role only through the group
// loses it on the worker's next pass.
func (app *Application) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	group, ok := app.groupFor(r, userID)
	if !ok {
		app.groupsBack(w, r, "That group is not yours to change.")
		return
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		q := app.queries.WithTx(tx)
		if err = q.DeleteDiscordRoleRulesForGroup(ctx, strconv.FormatInt(group.ID, 10)); err == nil {
			err = q.DeleteOrgGroup(ctx, group.ID)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		logging.Errorf("groups: delete group %d: %v", group.ID, err)
		app.groupsBack(w, r, "The group could not be removed; check the server log.")
		return
	}
	logging.Infof("groups: user %d removed group %d (%s)", userID, group.ID, group.Name)
	app.groupsBack(w, r, "Group "+group.Name+" removed, with any Discord role rules that used it.")
}

// handleGroupMemberAdd puts characters in a group. Each has to be a
// linked character in the corporation, or in a corporation of the
// alliance, the group belongs to.
func (app *Application) handleGroupMemberAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	group, ok := app.groupFor(r, userID)
	if !ok || r.ParseForm() != nil {
		app.groupsBack(w, r, "That group is not yours to change.")
		return
	}
	owner := discordOwner{group.OwnerKind, group.OwnerID}
	allowed := map[int64]bool{}
	if corps := app.ownerCorporations(ctx, owner); len(corps) > 0 {
		rows, err := app.queries.ListCharactersInCorporations(ctx, corps)
		if err != nil {
			logging.Errorf("groups: characters of %s: %v", owner.key(), err)
		}
		for _, row := range rows {
			allowed[row.CharacterID] = true
		}
	}
	members, _ := app.queries.ListOrgGroupMembers(ctx, group.ID)
	room := groupMembersMost - len(members)
	added := 0
	for _, raw := range r.Form["character"] {
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
		app.groupsBack(w, r, "Nobody was added. Only characters linked to EveSynapse that are in the "+owner.Kind+" can be.")
		return
	}
	logging.Infof("groups: user %d added %d member(s) to group %d (%s)", userID, added, group.ID, group.Name)
	app.groupsBack(w, r, strconv.Itoa(added)+" added to "+group.Name+".")
}

func (app *Application) handleGroupMemberRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
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
