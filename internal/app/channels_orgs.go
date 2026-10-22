package app

// ---------------------------------------------------------------------------
// Intel channels owned by a corporation or alliance, shared by the same
// rules as everything else an organisation runs here (schema 082).
//
// A personal channel is managed by the account that made it, as
// before. A corporation's channel is managed like its other programmes
// (corp_permissions.go): its directors and CEO, and whoever they hand
// "intel channels" to in the corporation's settings. An alliance's
// channel is managed by the directors of its executor corporation, as
// its Discord server is.
//
// Besides naming a pilot, corporation, alliance or group (channels.go),
// an organisation's channel is shared by rule: members, one corporation
// of the alliance, CEOs, an in-game role, a group, or everyone who has
// connected EveSynapse -- the rules Discord roles are given by,
// judged against the reader's standing the same way (internal/standing).
// A new organisation channel starts shared with its members.
// ---------------------------------------------------------------------------

import (
	"context"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// permIntel is the programme "intel channels" (corp_permissions).
// Stored: never rename.
const permIntel = "intel_channels"

// accessChannel is one channel an account can read: whether it may
// post, and whether it manages the channel.
type accessChannel struct {
	db.ListAccessibleChannelsRow
	Manage bool
}

// Org is the channel's owner organisation; ok is false for a personal
// channel.
func (c accessChannel) Org() (orgOwner, bool) {
	if c.OwnerKind == "" {
		return orgOwner{}, false
	}
	return orgOwner{Kind: c.OwnerKind, ID: c.OwnerID}, true
}

// intelManagedOrgs lists the corporations and alliances whose channels
// an account of that standing manages.
func (app *Application) intelManagedOrgs(ctx context.Context, st orgStanding) []orgOwner {
	if !st.Working {
		return nil
	}
	corps := make([]int64, 0, len(st.Corps))
	for corp := range st.Corps {
		corps = append(corps, corp)
	}
	sort.Slice(corps, func(i, j int) bool { return corps[i] < corps[j] })
	var out []orgOwner
	for _, corp := range corps {
		if app.corpPermits(ctx, st, corp, permIntel) {
			out = append(out, orgOwner{Kind: ownerCorporation, ID: corp})
		}
		if alliance, executor := app.corpAlliance(ctx, corp); alliance != 0 && executor == corp && st.Directs(corp) {
			if a := (orgOwner{Kind: ownerAlliance, ID: alliance}); !ownersInclude(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// managesChannel reports whether the identity manages a channel.
func (id intelIdentity) managesChannel(ownerKind string, ownerID, ownerUserID int64) bool {
	if id.userID == 0 {
		return false
	}
	if ownerKind == "" {
		return ownerUserID == id.userID
	}
	return ownersInclude(id.managed, orgOwner{Kind: ownerKind, ID: ownerID})
}

// accessibleChannels lists the channels an account can read: its own,
// those granted to its characters, corporations, alliances and groups,
// the organisation channels whose rules it meets, and every channel of
// the organisations it manages. Sorted by name.
func (app *Application) accessibleChannels(ctx context.Context, id intelIdentity) []accessChannel {
	if id.userID == 0 {
		return nil
	}
	nz := func(s []int64) []int64 {
		if s == nil {
			return []int64{}
		}
		return s
	}
	rows, err := app.queries.ListAccessibleChannels(ctx, db.ListAccessibleChannelsParams{
		UserID: id.userID, CharacterIds: nz(id.characters), CorporationIds: nz(id.corporations),
		AllianceIds: nz(id.alliances), GroupIds: nz(id.groups),
	})
	if err != nil {
		logging.Warnf("channels: accessible for user %d: %v", id.userID, err)
	}
	byID := map[int64]*accessChannel{}
	var order []int64
	add := func(r db.ListAccessibleChannelsRow, canPost, manage bool) {
		if c := byID[r.ID]; c != nil {
			c.CanPost = c.CanPost || canPost
			c.Manage = c.Manage || manage
			return
		}
		r.CanPost = r.CanPost || canPost
		byID[r.ID] = &accessChannel{ListAccessibleChannelsRow: r, Manage: manage}
		order = append(order, r.ID)
	}
	for _, r := range rows {
		add(r, r.CanPost, id.managesChannel(r.OwnerKind, r.OwnerID, r.OwnerUserID))
	}
	if id.standing.Working {
		rules, err := app.queries.ListIntelChannelRuleGrants(ctx)
		if err != nil {
			logging.Warnf("channels: rule grants: %v", err)
		}
		for _, g := range rules {
			owner := orgOwner{Kind: g.OwnerKind, ID: g.OwnerID}
			if !id.standing.Meets(owner, standing.Rule{Kind: g.RuleKind, Ref: g.RuleRef}) {
				continue
			}
			add(db.ListAccessibleChannelsRow{ID: g.ChannelID, OwnerUserID: g.OwnerUserID, Name: g.Name, Description: g.Description,
				CreatedAt: g.CreatedAt, OwnerKind: g.OwnerKind, OwnerID: g.OwnerID}, g.CanPost, id.managesChannel(g.OwnerKind, g.OwnerID, g.OwnerUserID))
		}
	}
	for _, o := range id.managed {
		chans, err := app.queries.ListOrgIntelChannels(ctx, db.ListOrgIntelChannelsParams{OwnerKind: o.Kind, OwnerID: o.ID})
		if err != nil {
			logging.Warnf("channels: channels of %s: %v", o.Key(), err)
			continue
		}
		for _, c := range chans {
			add(db.ListAccessibleChannelsRow{ID: c.ID, OwnerUserID: c.OwnerUserID, Name: c.Name, Description: c.Description,
				CreatedAt: c.CreatedAt, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID}, true, true)
		}
	}
	out := make([]accessChannel, 0, len(order))
	for _, cid := range order {
		out = append(out, *byID[cid])
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// channelRowOf is a channel as the channel list shows it.
func (app *Application) channelRowOf(ctx context.Context, c accessChannel) channelRow {
	row := channelRow{ID: c.ID, Name: c.Name, Description: c.Description, Owner: c.Manage, CanPost: c.CanPost}
	if o, ok := c.Org(); ok {
		row.Org = app.ownerName(ctx, o)
	}
	return row
}

// channelOwnerOption is one owner a new channel can have.
type channelOwnerOption struct {
	Value, Label string
}

// channelOwnerOptions: just me, then the organisations the account
// manages channels for.
func (app *Application) channelOwnerOptions(ctx context.Context, id intelIdentity) []channelOwnerOption {
	out := []channelOwnerOption{{Value: "", Label: "Just me"}}
	for _, o := range id.managed {
		out = append(out, channelOwnerOption{Value: o.Key(), Label: app.ownerName(ctx, o)})
	}
	return out
}

// channelRuleGrants lists an organisation channel's rule grants as the
// access list shows them.
func (app *Application) channelRuleGrants(ctx context.Context, owner orgOwner, channelID int64) []grantView {
	rows, err := app.queries.ListIntelChannelRules(ctx, channelID)
	if err != nil {
		return nil
	}
	out := make([]grantView, 0, len(rows))
	for _, g := range rows {
		who := g.RuleKind
		if g.RuleRef != "" {
			who += ":" + g.RuleRef
		}
		out = append(out, grantView{Kind: "rule", Who: who, CanPost: g.CanPost,
			Name: app.ruleWho(ctx, owner, db.DiscordRoleRule{Kind: g.RuleKind, Ref: g.RuleRef})})
	}
	return out
}

// addChannelRule shares an organisation channel with everyone who
// meets a rule. raw is a "Who" choice ("kind" or "kind:ref").
func (app *Application) addChannelRule(ctx context.Context, owner orgOwner, channelID int64, raw string, canPost bool) (label string, ok bool) {
	kind, ref, valid := app.parseRuleWho(ctx, owner, raw)
	if !valid {
		return "", false
	}
	if err := app.queries.UpsertIntelChannelRule(ctx, db.UpsertIntelChannelRuleParams{ChannelID: channelID, RuleKind: kind, RuleRef: ref, CanPost: canPost, CreatedAt: time.Now().UTC()}); err != nil {
		logging.Errorf("channels: rule %s on channel %d: %v", raw, channelID, err)
		return "", false
	}
	return app.ruleWho(ctx, owner, db.DiscordRoleRule{Kind: kind, Ref: ref}), true
}
