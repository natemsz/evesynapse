package app

import (
	"net/http"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Who else plans an alliance's or community's fleets and keeps its
// doctrines. Only the people who run the owner by position can choose.
// A corporation makes the same choices in its settings.
// ---------------------------------------------------------------------------

const fleetPermissionsPath = "/fleets/permissions/"

// fleetPermissionsView is the page, for one owner.
type fleetPermissionsView struct {
	OwnerOptions []corpOption
	OwnerKey     string // empty when the viewer runs no such owner
	OwnerName    string
	Community    bool
	Doctrines    []permissionChoice
	Fleets       []permissionChoice
	GroupsHref   string
	// Corporations: the viewer directs a corporation, whose choices are
	// in its settings.
	Corporations bool
}

// handOverOwners lists the alliances and communities the viewer runs by
// position.
func (app *Application) handOverOwners(r *http.Request, v fleetViewer) []orgOwner {
	var out []orgOwner
	for _, owner := range v.owners() {
		if owner.Kind != ownerCorporation && app.holdsProgramme(r.Context(), v.Standing, owner, permDoctrines) {
			out = append(out, owner)
		}
	}
	return out
}

// handOverChoices lists who an owner can hand a programme to, from what
// the viewer's standing already holds, with the stored choice kept.
func (app *Application) handOverChoices(r *http.Request, v fleetViewer, owner orgOwner, permission string) []permissionChoice {
	ctx := r.Context()
	current := ""
	if rule, err := app.queries.GetOwnerPermission(ctx, ownerPermissionKey(owner, permission)); err == nil {
		current = ruleValue(rule.Kind, rule.Ref)
	}
	out := []permissionChoice{{Value: "", Label: "Nobody else", Selected: current == ""}}
	for _, who := range app.fleetRuleChoices(ctx, v, owner, current) {
		if permissionRule(owner, who.Value) {
			out = append(out, permissionChoice{Value: who.Value, Label: who.Label, Selected: who.Value == current})
		}
	}
	return out
}

func hasPermissionChoice(choices []permissionChoice, value string) bool {
	for _, c := range choices {
		if c.Value == value {
			return true
		}
	}
	return false
}

func (app *Application) handleFleetPermissions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	if v.UserID == 0 {
		app.flash(ctx, "Choosing who runs fleets needs an EveSynapse account.")
		http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
		return
	}
	view := &fleetPermissionsView{Corporations: len(app.directedCorps(ctx, v.UserID)) > 0}
	if owner, options, ok := app.pickOwner(ctx, r, app.handOverOwners(r, v)); ok {
		view.OwnerOptions, view.OwnerKey, view.OwnerName = options, owner.Key(), app.ownerName(ctx, owner)
		view.Community = owner.Kind == ownerCommunity
		view.Doctrines = app.handOverChoices(r, v, owner, permDoctrines)
		view.Fleets = app.handOverChoices(r, v, owner, permFleets)
		if moduleCompiledIn("groups") {
			view.GroupsHref = standing.Address("/groups/", owner.Key())
		}
	}
	app.render(ctx, w, http.StatusOK, "fleet_permissions.html", pageData{ShellData: app.PublicShell(ctx), FleetPermissions: view})
}

// handleFleetPermissionsSave stores both choices or neither.
func (app *Application) handleFleetPermissionsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	if err := r.ParseForm(); err != nil || v.UserID == 0 {
		http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
		return
	}
	owner, ok := parseAnyOwner(r.PostForm.Get("owner"))
	if !ok || !ownersInclude(app.handOverOwners(r, v), owner) {
		app.flashBack(w, r, fleetPermissionsPath)("Only the people who run it can change that.")
		return
	}
	back := app.flashBack(w, r, standing.Address(fleetPermissionsPath, owner.Key()))
	doctrines, fleets := r.PostForm.Get("doctrines_who"), r.PostForm.Get("fleets_who")
	if !hasPermissionChoice(app.handOverChoices(r, v, owner, permDoctrines), doctrines) ||
		!hasPermissionChoice(app.handOverChoices(r, v, owner, permFleets), fleets) {
		back("That cannot be handed to the choice made; nothing was saved.")
		return
	}
	by := app.OwnerActorFor(ctx, v.UserID, owner)
	err := app.inOwnerPermissionTx(r, func(q *db.Queries) error {
		for _, choice := range []struct{ permission, value string }{{permDoctrines, doctrines}, {permFleets, fleets}} {
			kind, ref := splitRuleValue(choice.value)
			if err := storeOwnerPermission(ctx, q, owner, choice.permission, kind, ref, by); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		logging.Errorf("fleets: permissions of %s: %v", owner.Key(), err)
		back("The choices could not be saved. Try again in a moment.")
		return
	}
	logging.Infof("fleets: user %d changed who runs the fleets and doctrines of %s", v.UserID, owner.Key())
	back("Saved.")
}

// inOwnerPermissionTx runs fn in one transaction.
func (app *Application) inOwnerPermissionTx(r *http.Request, fn func(q *db.Queries) error) error {
	ctx := r.Context()
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(app.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
