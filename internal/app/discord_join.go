package app

import (
	"context"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Adding people to a server. Someone whose character is in a
// corporation should not have to hunt for an invite to its Discord
// server: once they have connected Discord, and agreed there to "join
// servers for you", the bot puts them in and gives them their roles in
// the same step.
//
// Who is added where: an account is added to a server only when one of
// its working characters is in the corporation the server belongs to,
// or in a corporation of its alliance. Holding some other role there
// (a server that gives a role to everyone connected) adds nobody: a
// stranger to the corporation is never put in its server.
//
// When: on the worker's pass after the account qualifies, whichever
// happened last: connecting Discord, linking the character, the
// character joining the corporation, or the server being set up. That
// is why the account's Discord token is kept (sealed, like the EVE
// tokens); without it the adding could only happen at the instant of
// connecting.
//
// A server's directors can switch the adding off for their server.
// ---------------------------------------------------------------------------

// discordTokenSkew: a token this close to running out is renewed
// before it is used.
const discordTokenSkew = 2 * time.Minute

// Field names the sealed Discord tokens are bound to (tokenBox).
const (
	discordAccessField  = "discord_access"
	discordRefreshField = "discord_refresh"
)

// discordStoreToken seals and stores an account's Discord token. An
// empty token forgets what was stored.
func (app *Application) discordStoreToken(ctx context.Context, userID int64, tok discord.Token) error {
	params := db.SetDiscordLinkTokensParams{UserID: userID}
	if tok.Access != "" {
		access, err := app.tokens.seal(tok.Access, userID, discordAccessField)
		if err != nil {
			return err
		}
		refresh, err := app.tokens.seal(tok.Refresh, userID, discordRefreshField)
		if err != nil {
			return err
		}
		params.AccessToken, params.RefreshToken, params.TokenExpiry = access, refresh, timeSet(tok.Expiry)
	}
	return app.queries.SetDiscordLinkTokens(ctx, params)
}

// discordUserToken returns an account's Discord token for use now,
// renewing it first when it has run out. ok is false when there is no
// usable token: the account connected before tokens were kept, or has
// since taken its permission back on Discord (the stored token is
// forgotten then, and the settings page asks them to connect again).
func (app *Application) discordUserToken(ctx context.Context, link db.DiscordLink, now time.Time) (token string, ok bool) {
	if link.AccessToken == "" {
		return "", false
	}
	access, err := app.tokens.open(link.AccessToken, link.UserID, discordAccessField)
	if err != nil {
		logging.Errorf("discord: open token of user %d: %v", link.UserID, err)
		return "", false
	}
	if link.TokenExpiry.Valid && link.TokenExpiry.Time.After(now.Add(discordTokenSkew)) {
		return access, true
	}
	refresh, err := app.tokens.open(link.RefreshToken, link.UserID, discordRefreshField)
	if err != nil || refresh == "" {
		return "", false
	}
	renewed, err := app.discord.Refresh(ctx, refresh)
	if err != nil {
		if discord.IsStatus(err, http.StatusBadRequest) || discord.IsStatus(err, http.StatusUnauthorized) {
			// Permission taken back: nothing stored is of any use now.
			if serr := app.discordStoreToken(ctx, link.UserID, discord.Token{}); serr != nil {
				logging.Errorf("discord: forget token of user %d: %v", link.UserID, serr)
			}
		} else {
			logging.Warnf("discord: renew token of user %d: %v", link.UserID, err)
		}
		return "", false
	}
	if renewed.Refresh == "" {
		renewed.Refresh = refresh
	}
	if err := app.discordStoreToken(ctx, link.UserID, renewed); err != nil {
		logging.Errorf("discord: store renewed token of user %d: %v", link.UserID, err)
	}
	return renewed.Access, true
}

// belongs reports whether one of the account's working characters is
// in the owner corporation, or in a corporation of the owner alliance.
func (st discordStanding) belongs(owner discordOwner) bool {
	if !st.working {
		return false
	}
	for corp := range st.corps {
		if st.within(owner, corp) {
			return true
		}
	}
	return false
}

// discordJoin puts an account that belongs to a server's owner into
// the server, with the roles it is owed. joined is false, with no
// error, whenever there is simply nothing to do: the server does not
// add people, the account does not belong, or it has no usable token.
func (app *Application) discordJoin(ctx context.Context, guild db.DiscordGuild, link db.DiscordLink, st discordStanding, wanted []string, now time.Time) (joined bool, err error) {
	if !guild.AutoJoin || !st.belongs(discordOwner{guild.OwnerKind, guild.OwnerID}) {
		return false, nil
	}
	token, ok := app.discordUserToken(ctx, link, now)
	if !ok {
		return false, nil
	}
	if _, err := app.discord.AddMember(ctx, guild.GuildID, link.DiscordID, token, wanted); err != nil {
		return false, err
	}
	logging.Infof("discord: added user %d to %s", link.UserID, guild.Name)
	return true, nil
}
