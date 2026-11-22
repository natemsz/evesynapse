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
// A corporation's choice is also written to corp_permissions, so the
// previous release reads the same choices if it is restored.
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
	}
	return false
}

// characterStanding is the standing of one character alone, for the
// checks made character by character.
func characterStanding(corp int64, roles []string) orgStanding {
	st := standing.New()
	st.Working = true
	st.Corps[corp] = true
	held := make(map[string]bool, len(roles))
	for _, role := range roles {
		held[strings.ToLower(role)] = true
	}
	st.EVERoles[corp] = held
	return st
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
		if permissionRule(who.Value) {
			out = append(out, permissionChoice{Value: who.Value, Label: who.Label, Selected: who.Value == current})
		}
	}
	return out
}

// permissionRule reports whether a "kind:ref" is one a programme can
// be handed to: an in-game role other than Director, or a group.
func permissionRule(value string) bool {
	kind, ref, _ := strings.Cut(value, ":")
	return kind == ruleGroup || (kind == ruleEVERole && ref != "Director")
}

// setOwnerPermission stores an owner's choice for a programme; the
// empty choice hands it to nobody else.
func (app *Application) setOwnerPermission(ctx context.Context, owner orgOwner, permission, raw string, by int64) bool {
	kind, ref := "", ""
	if raw != "" {
		var ok bool
		kind, ref, ok = app.parseRuleWho(ctx, owner, raw)
		if !ok || !permissionRule(kind+":"+ref) {
			return false
		}
	}
	if err := app.writeOwnerPermission(ctx, owner, permission, kind, ref, by); err != nil {
		logging.Errorf("permissions: set %s of %s: %v", permission, owner.Key(), err)
		return false
	}
	return true
}

// writeOwnerPermission stores one choice, an empty kind clearing it,
// and a corporation's copy in corp_permissions with it.
func (app *Application) writeOwnerPermission(ctx context.Context, owner orgOwner, permission, kind, ref string, by int64) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	now := time.Now().UTC()
	if kind == "" {
		err = q.DeleteOwnerPermission(ctx, db.DeleteOwnerPermissionParams(ownerPermissionKey(owner, permission)))
	} else {
		err = q.SetOwnerPermission(ctx, db.SetOwnerPermissionParams{
			OwnerKind: owner.Kind, OwnerID: owner.ID, Permission: permission, Kind: kind, Ref: ref, UpdatedBy: by, UpdatedAt: now,
		})
	}
	if err != nil {
		return err
	}
	if owner.Kind == ownerCorporation {
		if kind == "" {
			err = q.DeleteCorpPermission(ctx, db.DeleteCorpPermissionParams{CorporationID: owner.ID, Permission: permission})
		} else {
			err = q.SetCorpPermission(ctx, db.SetCorpPermissionParams{
				CorporationID: owner.ID, Permission: permission, Kind: kind, Ref: ref, UpdatedBy: by, UpdatedAt: now,
			})
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
