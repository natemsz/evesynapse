package app

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Who runs an owner's programmes.
//
// Some people run a programme by their position: a corporation's
// directors and CEO, and the directors of an alliance's executor
// corporation. A corporation's fleets go instead to the holders of its
// ops manager roles (OPS_MANAGER_ROLES, Director by default). For each
// programme an owner can also name who else may: the holders of an
// in-game role, or the members of one of its groups. The choice is
// stored in owner_permissions in the terms of a Discord role rule and
// judged the same way, from stored data only.
//
// corp_permissions is no longer written. It stays, with the trigger that
// carries its writes here, so a restored earlier release still saves.
// ---------------------------------------------------------------------------

// Programmes (owner_permissions.permission). Stored: never rename.
const (
	permSRP       = "srp"
	permDoctrines = "doctrines"
	permFleets    = "fleets"
)

// ownerPermits reports whether an account of that standing may run one
// of an owner's programmes: by position, or by the owner's stored
// choice of who else may.
func (app *Application) ownerPermits(ctx context.Context, st orgStanding, owner orgOwner, permission string) bool {
	if app.holdsProgramme(ctx, st, owner, permission) {
		return true
	}
	if !st.Belongs(owner) {
		return false
	}
	rule, err := app.queries.GetOwnerPermission(ctx, ownerPermissionKey(owner, permission))
	if err != nil {
		return false
	}
	return st.Meets(owner, standing.Rule{Kind: rule.Kind, Ref: rule.Ref})
}

// holdsProgramme reports whether a standing runs an owner's programme
// by position alone, without reading a stored choice. Any other kind
// of owner has nobody by position.
func (app *Application) holdsProgramme(ctx context.Context, st orgStanding, owner orgOwner, permission string) bool {
	switch owner.Kind {
	case ownerCorporation:
		if permission == permFleets {
			return st.Working && app.cfg.holdsOpsManagerRole(slices.Collect(maps.Keys(st.EVERoles[owner.ID])))
		}
		return st.Directs(owner.ID)
	case ownerAlliance:
		for corp := range st.Corps {
			if !st.Directs(corp) {
				continue
			}
			if alliance, executor := app.corpAlliance(ctx, corp); alliance == owner.ID && executor == corp {
				return true
			}
		}
	case ownerCommunity:
		return st.ManagesCommunity(owner.ID)
	}
	return false
}

// ownersPermitted answers ownerPermits for several owners with one read
// of their stored choices.
func (app *Application) ownersPermitted(ctx context.Context, st orgStanding, owners []orgOwner, permission string) map[string]bool {
	out := make(map[string]bool, len(owners))
	if len(owners) == 0 {
		return out
	}
	kinds, ids := ownerArrays(owners)
	rows, err := app.queries.ListOwnerPermissionsForOwners(ctx, db.ListOwnerPermissionsForOwnersParams{
		Permission: permission, OwnerKinds: kinds, OwnerIds: ids,
	})
	if err != nil {
		logging.Errorf("permissions: read %s choices: %v", permission, err)
	}
	stored := make(map[string]standing.Rule, len(rows))
	for _, row := range rows {
		stored[orgOwner{Kind: row.OwnerKind, ID: row.OwnerID}.Key()] = standing.Rule{Kind: row.Kind, Ref: row.Ref}
	}
	for _, owner := range owners {
		rule, chosen := stored[owner.Key()]
		out[owner.Key()] = app.holdsProgramme(ctx, st, owner, permission) ||
			(chosen && st.Belongs(owner) && st.Meets(owner, rule))
	}
	return out
}

// ownerArrays splits owners into the parallel lists a query takes.
func ownerArrays(owners []orgOwner) (kinds []string, ids []int64) {
	kinds = make([]string, len(owners))
	ids = make([]int64, len(owners))
	for i, owner := range owners {
		kinds[i], ids[i] = owner.Kind, owner.ID
	}
	return kinds, ids
}

func ownerPermissionKey(owner orgOwner, permission string) db.GetOwnerPermissionParams {
	return db.GetOwnerPermissionParams{OwnerKind: owner.Kind, OwnerID: owner.ID, Permission: permission}
}

// permissionChoice is one answer to "who else may run this".
type permissionChoice struct {
	Value, Label string
	Selected     bool
}

// permissionChoices lists who an owner can hand a programme to, with
// the stored choice marked.
func (app *Application) permissionChoices(ctx context.Context, owner orgOwner, permission string) []permissionChoice {
	current := ""
	if rule, err := app.queries.GetOwnerPermission(ctx, ownerPermissionKey(owner, permission)); err == nil {
		current = rule.Kind + ":" + rule.Ref
	}
	out := []permissionChoice{{Value: "", Label: "Nobody else", Selected: current == ""}}
	for _, who := range app.whoChoices(ctx, owner) {
		if permissionRule(owner, who.Value) {
			out = append(out, permissionChoice{Value: who.Value, Label: who.Label, Selected: who.Value == current})
		}
	}
	return out
}

// permissionRule reports whether a "kind:ref" is one an owner's
// programme can be handed to. Every owner can name one of its groups.
// A corporation can also name an in-game role other than Director, and
// an alliance one of its corporations. An in-game role never hands over
// an alliance's programme, since any member corporation could grant it.
func permissionRule(owner orgOwner, value string) bool {
	kind, ref, _ := strings.Cut(value, ":")
	if kind == ruleGroup {
		return true
	}
	switch owner.Kind {
	case ownerCorporation:
		return kind == ruleEVERole && ref != "Director"
	case ownerAlliance:
		return kind == ruleCorp
	}
	return false
}

// storeOwnerPermission writes one choice inside a caller's transaction,
// an empty kind clearing it.
func storeOwnerPermission(ctx context.Context, q *db.Queries, owner orgOwner, permission, kind, ref string, by int64) error {
	if kind == "" {
		return q.DeleteOwnerPermission(ctx, db.DeleteOwnerPermissionParams(ownerPermissionKey(owner, permission)))
	}
	return q.SetOwnerPermission(ctx, db.SetOwnerPermissionParams{
		OwnerKind: owner.Kind, OwnerID: owner.ID, Permission: permission, Kind: kind, Ref: ref, UpdatedBy: by, UpdatedAt: time.Now().UTC(),
	})
}
