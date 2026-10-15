package app

import (
	"context"

	"evesynapse/internal/app/module"
	"evesynapse/internal/logging"
)

// How an account acts for a corporation: the character of its that is in
// the corporation, and one that can send EVE mail. Part of the access
// model, so the ship replacement and doctrine modules ask it through Deps.

func (app *Application) CorpActorFor(ctx context.Context, userID, corp int64) module.CorpActor {
	corpOf := map[int64]int64{}
	rows, _ := app.queries.ListCharacterCorporationsByUser(ctx, userID)
	for _, row := range rows {
		corpOf[row.CharacterID] = row.CorporationID
	}
	var actor module.CorpActor
	characters, _ := app.queries.ListCharactersByUser(ctx, userID)
	for i, ch := range characters {
		if ch.LinkState != linkStateOK || corpOf[ch.CharacterID] != corp {
			continue
		}
		if actor.CharacterID == 0 {
			actor.CharacterID = ch.CharacterID
		}
		if actor.Mailer == nil && characterHasScope(ch, mailSendScope) {
			actor.Mailer = &characters[i]
		}
	}
	return actor
}

// UserCorps lists the corporations an account has a character in, in the
// order of its characters.
func (app *Application) UserCorps(ctx context.Context, userID int64) []int64 {
	rows, err := app.queries.ListCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("corporations of user %d: %v", userID, err)
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, row := range rows {
		if row.CorporationID != 0 && !seen[row.CorporationID] {
			seen[row.CorporationID] = true
			out = append(out, row.CorporationID)
		}
	}
	return out
}
