package app

import (
	"context"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Who runs a corporation's programmes.
//
// A corporation's directors and its CEO run every programme it has
// here. For each one they can also name who else may: the holders of
// an in-game role, or the members of one of the corporation's groups
// (corp_groups.go). The choice is stored in the terms of a Discord
// role rule and judged the same way, from stored data only.
// ---------------------------------------------------------------------------

// Programmes (corp_permissions.permission). Stored: never rename.
const (
	permSRP = "srp"
)

// directs reports whether the account has a director, or the CEO, of
// a corporation.
func (st discordStanding) directs(corp int64) bool {
	return st.working && (st.ceoOf[corp] || st.eveRoles[corp]["director"])
}

// corpPermits reports whether an account of that standing may run one
// of a corporation's programmes.
func (app *Application) corpPermits(ctx context.Context, st discordStanding, corp int64, permission string) bool {
	if st.directs(corp) {
		return true
	}
	if !st.working || !st.corps[corp] {
		return false
	}
	rule, err := app.queries.GetCorpPermission(ctx, db.GetCorpPermissionParams{CorporationID: corp, Permission: permission})
	if err != nil {
		return false
	}
	return st.meets(discordOwner{ownerCorporation, corp}, db.DiscordRoleRule{Kind: rule.Kind, Ref: rule.Ref})
}

// permissionChoice is one answer to "who else may run this".
type permissionChoice struct {
	Value, Label string
	Selected     bool
}

// permissionChoices lists who a corporation's directors can hand a
// programme to, with the stored choice marked.
func (app *Application) permissionChoices(ctx context.Context, corp int64, permission string) []permissionChoice {
	current := ""
	if rule, err := app.queries.GetCorpPermission(ctx, db.GetCorpPermissionParams{CorporationID: corp, Permission: permission}); err == nil {
		current = rule.Kind + ":" + rule.Ref
	}
	out := []permissionChoice{{Value: "", Label: "Nobody else", Selected: current == ""}}
	for _, who := range app.discordWhoChoices(ctx, discordOwner{ownerCorporation, corp}) {
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

// setCorpPermission stores the directors' choice for a programme; the
// empty choice hands it to nobody else.
func (app *Application) setCorpPermission(ctx context.Context, corp int64, permission, raw string, by int64) bool {
	key := db.GetCorpPermissionParams{CorporationID: corp, Permission: permission}
	if raw == "" {
		if err := app.queries.DeleteCorpPermission(ctx, db.DeleteCorpPermissionParams(key)); err != nil {
			logging.Errorf("permissions: clear %s of corporation %d: %v", permission, corp, err)
			return false
		}
		return true
	}
	kind, ref, ok := app.parseRuleWho(ctx, discordOwner{ownerCorporation, corp}, raw)
	if !ok || !permissionRule(kind+":"+ref) {
		return false
	}
	if err := app.queries.SetCorpPermission(ctx, db.SetCorpPermissionParams{
		CorporationID: corp, Permission: permission, Kind: kind, Ref: ref, UpdatedBy: by, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("permissions: set %s of corporation %d: %v", permission, corp, err)
		return false
	}
	return true
}
