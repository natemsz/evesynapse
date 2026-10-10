package app

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// One corporation per page.
//
// The pages a corporation runs here (doctrines, its fits, ship
// replacement, its settings) show one corporation at a time. Which one
// is named in the address (?corporation=); the corporation selector at
// the top of the page, the character selector's look, switches between
// the ones the page is offered for.
// ---------------------------------------------------------------------------

// corpOption is one corporation in the selector.
type corpOption struct {
	ID     int64
	Name   string
	Active bool
	// Param and Value are what the option's link sets in the address.
	Param, Value string
}

// npcCorporation reports whether an id is one of the game's own
// corporations, which nobody runs: they have no directors, so no
// doctrines, replacement or settings here.
func npcCorporation(id int64) bool { return id >= 1_000_000 && id < 2_000_000 }

// playerCorps lists the player corporations an account has a character
// in, in the order of its characters.
func (app *Application) playerCorps(ctx context.Context, userID int64) []int64 {
	var out []int64
	for _, corp := range app.srpCorps(ctx, userID) {
		if !npcCorporation(corp) {
			out = append(out, corp)
		}
	}
	return out
}

// pickCorporation chooses the page's corporation among those offered:
// the one the address names, else the one the character in use is in,
// else the first. It returns 0 when none is offered.
func (app *Application) pickCorporation(ctx context.Context, r *http.Request, offered []int64) (int64, []corpOption) {
	if len(offered) == 0 {
		return 0, nil
	}
	has := func(id int64) bool {
		for _, corp := range offered {
			if corp == id {
				return true
			}
		}
		return false
	}
	active := offered[0]
	if row, err := app.queries.GetCharacterCorporation(ctx, sessionCharID(app.sessions, ctx)); err == nil && has(row.CorporationID) {
		active = row.CorporationID
	}
	if id, err := strconv.ParseInt(r.URL.Query().Get("corporation"), 10, 64); err == nil && has(id) {
		active = id
	}
	options := make([]corpOption, 0, len(offered))
	for _, corp := range offered {
		options = append(options, corpOption{ID: corp, Name: app.corpDisplayName(ctx, corp), Active: corp == active, Param: "corporation", Value: strconv.FormatInt(corp, 10)})
	}
	return active, options
}

// pickOwner does the same for the pages that a corporation's or an
// alliance's directors run (groups, Discord servers): among the owners
// offered, the one the address names (?owner=), else the corporation
// the character in use is in, else the first.
func (app *Application) pickOwner(ctx context.Context, r *http.Request, offered []discordOwner) (discordOwner, []corpOption, bool) {
	if len(offered) == 0 {
		return discordOwner{}, nil, false
	}
	active := offered[0]
	if row, err := app.queries.GetCharacterCorporation(ctx, sessionCharID(app.sessions, ctx)); err == nil {
		if mine := (discordOwner{ownerCorporation, row.CorporationID}); discordManages(offered, mine) {
			active = mine
		}
	}
	if named, ok := parseDiscordOwner(r.URL.Query().Get("owner")); ok && discordManages(offered, named) {
		active = named
	}
	options := make([]corpOption, 0, len(offered))
	for _, owner := range offered {
		name := app.ownerName(ctx, owner)
		if owner.Kind == ownerAlliance {
			name += " (alliance)"
		}
		options = append(options, corpOption{ID: owner.ID, Name: name, Active: owner == active, Param: "owner", Value: owner.key()})
	}
	return active, options, true
}

// ownerAddress is such a page's address for one owner, named by its
// key; the plain page where the key is not one.
func ownerAddress(path, key string) string {
	if _, ok := parseDiscordOwner(key); !ok {
		return path
	}
	return path + "?owner=" + url.QueryEscape(key)
}

// corpAddress is a corporation page's address for one corporation.
func corpAddress(path string, corp int64) string {
	if corp == 0 {
		return path
	}
	return path + "?corporation=" + strconv.FormatInt(corp, 10)
}

// ---------------------------------------------------------------------------
// Corporation settings: what a corporation's directors decide about
// how it runs here, on one page. Who handles ship replacement and what
// the pilots are told about it, and who keeps the doctrines
// (corp_permissions.go). Groups and the Discord server have pages of
// their own, linked from here.
// ---------------------------------------------------------------------------

const corpSettingsPath = "/corporations/settings/"

type corpSettingsView struct {
	CorpOptions  []corpOption
	ID           int64
	Name         string
	SRPWho       []permissionChoice
	SRPPolicy    string
	DoctrinesWho []permissionChoice
}

// directedCorps lists the corporations an account has a director, or
// the CEO, of.
func (app *Application) directedCorps(ctx context.Context, userID int64) []int64 {
	st, err := app.discordStandingFor(ctx, userID)
	if err != nil {
		logging.Errorf("corporation settings: standing of user %d: %v", userID, err)
		return nil
	}
	var out []int64
	for _, corp := range app.playerCorps(ctx, userID) {
		if st.directs(corp) {
			out = append(out, corp)
		}
	}
	return out
}

func (app *Application) handleCorpSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	view := &corpSettingsView{}
	data.CorpSettings = view
	corp, options := app.pickCorporation(ctx, r, app.directedCorps(ctx, app.userID(ctx)))
	if corp != 0 {
		view.CorpOptions, view.ID, view.Name = options, corp, app.corpDisplayName(ctx, corp)
		view.SRPWho = app.permissionChoices(ctx, corp, permSRP)
		view.DoctrinesWho = app.permissionChoices(ctx, corp, permDoctrines)
		if settings, err := app.queries.GetSRPSettings(ctx, corp); err == nil {
			view.SRPPolicy = settings.Policy
		}
	}
	app.render(ctx, w, http.StatusOK, "corp_settings.html", data)
}

// handleCorpSettingsSave saves a corporation's settings
// (POST /corporations/settings/save). Directors and the CEO only, and
// all or nothing: one choice that cannot be made saves none of them.
func (app *Application) handleCorpSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	_ = r.ParseForm()
	corp, _ := strconv.ParseInt(r.Form.Get("corporation"), 10, 64)
	back := app.flashBack(w, r, corpAddress(corpSettingsPath, corp))
	st, err := app.discordStandingFor(ctx, userID)
	if err != nil || corp == 0 || !st.directs(corp) {
		app.flashBack(w, r, corpSettingsPath)("Only a director or the CEO of the corporation can change its settings.")
		return
	}
	owner := discordOwner{ownerCorporation, corp}
	for _, raw := range []string{r.Form.Get("srp_who"), r.Form.Get("doctrines_who")} {
		if kind, ref, ok := app.parseRuleWho(ctx, owner, raw); raw != "" && (!ok || !permissionRule(kind+":"+ref)) {
			back("That cannot be handed to the choice made; nothing was saved.")
			return
		}
	}
	by := app.srpActorFor(ctx, userID, corp).CharacterID
	saved := app.setCorpPermission(ctx, corp, permSRP, r.Form.Get("srp_who"), by) &&
		app.setCorpPermission(ctx, corp, permDoctrines, r.Form.Get("doctrines_who"), by)
	if saved {
		err = app.queries.SetSRPSettings(ctx, db.SetSRPSettingsParams{
			CorporationID: corp, Policy: clip(strings.TrimSpace(r.Form.Get("srp_policy")), srpPolicyMax), UpdatedAt: time.Now().UTC(),
		})
	}
	if !saved || err != nil {
		logging.Errorf("corporation settings: save for corporation %d: %v", corp, err)
		back("The settings could not be saved; check the server log.")
		return
	}
	logging.Infof("corporation settings: user %d changed the settings of corporation %d", userID, corp)
	back("Saved.")
}
