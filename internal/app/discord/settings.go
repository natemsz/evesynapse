package discord

import (
	"context"
	"html/template"
	"net/http"
	"net/url"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// discordSettingsView is the Discord part of the notification settings
// page.
type discordSettingsView struct {
	CanLink bool // "Connect Discord" is offered
	HasBot  bool // the bot can message and give roles
	Linked  bool
	Name    string // the connected account
	DM      bool   // notifications go to it as direct messages
	// DMProblem is what went wrong with the last direct message, if
	// anything did.
	DMProblem string
	// Reconnect: the account connected before EveSynapse could add
	// people to servers, or took that permission back on Discord.
	Reconnect bool
}

// settingsViewFor works out the account's Discord settings.
func (app *mod) settingsViewFor(r *http.Request, userID int64) discordSettingsView {
	view := discordSettingsView{CanLink: app.discordCanLink(), HasBot: app.discordHasBot()}
	if link, linked := app.discordLinkFor(r, userID); linked {
		view.Linked, view.Name, view.DM = true, link.Username, link.DmNotifications
		view.Reconnect = view.HasBot && link.AccessToken == ""
		if link.DmNotifications {
			view.DMProblem = link.DmProblem
		}
	}
	return view
}

// settingsChannel is the "also send these to me on Discord" box inside
// the settings form.
func settingsChannel(ctx context.Context, deps module.Deps, r *http.Request, userID int64) template.HTML {
	view := newMod(deps).settingsViewFor(r, userID)
	return fragment(deps, "discord-notify-channel", view)
}

// settingsAccount is the Discord section below the form.
func settingsAccount(ctx context.Context, deps module.Deps, r *http.Request, userID int64) template.HTML {
	view := newMod(deps).settingsViewFor(r, userID)
	return fragment(deps, "discord-notify-account", view)
}

func fragment(deps module.Deps, define string, data discordSettingsView) template.HTML {
	html, err := deps.Fragment("discord_settings.html", define, data)
	if err != nil {
		logging.Errorf("discord: render settings section %s: %v", define, err)
		return ""
	}
	return html
}

// settingsSave stores the box inside the form. The form says whether it
// showed the box (discord_shown): a box left unticked is "off", but a
// form that never had the box must not switch anything off.
func settingsSave(ctx context.Context, deps module.Deps, userID int64, form url.Values) error {
	if form.Get("discord_shown") != "1" {
		return nil
	}
	return deps.DB().SetDiscordDMNotifications(ctx, db.SetDiscordDMNotificationsParams{
		UserID: userID, DmNotifications: form.Get("discord_dm") == "1",
	})
}
