package app

import (
	"context"
	"evesynapse/internal/app/module"
	"fmt"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Mail page (/mail/): the character's 50 most recent headers, a label
// filter and a body view, cache-only from the worker-warmed mail
// snapshots.
//
// Bodies are EVE-flavored HTML and hostile by default: they are
// rendered only through htmlsanitize.Sanitize, a strict allowlist filter
// that drops external images with every other tag not on the list.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Mail body sanitizer. EVE mail bodies are HTML written by other
// players: hostile input. The filter below keeps a small allowlist
// of formatting tags, drops active-content tags together with
// their contents, keeps only http(s) links, drops every other
// attribute, and escapes all text. Nothing from the source reaches
// the page unescaped except the literal tags emitted here.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Compose
// ---------------------------------------------------------------------------

// errMailToken: the sending character's sign-in could not be used.
var errMailToken = module.ErrMailToken

// sendEVEMail sends one in-game mail from a character
// (POST /characters/{id}/mail/). approvedCost is the most ISK EVE may
// charge the sender for reaching the recipient.
//
// ESI answers a sent mail with 201 and the new mail's ID as a bare
// number. Nothing needs the ID, so the answer is not decoded at all:
// reading it as an object fails on the number and reports a mail that
// was in fact sent as refused, inviting a second, duplicate send.
func (app *Application) sendEVEMail(ctx context.Context, from db.Character, recipientID int64, recipientType, subject, body string, approvedCost int) error {
	token, err := app.validAccessToken(ctx, from)
	if err != nil {
		logging.Errorf("mail send: token for character %d: %v", from.CharacterID, err)
		return errMailToken
	}
	payload := map[string]any{
		"approved_cost": approvedCost,
		"body":          body,
		"recipients":    []map[string]any{{"recipient_id": recipientID, "recipient_type": recipientType}},
		"subject":       subject,
	}
	return app.esi.PostJSONAuthed(ctx, token, fmt.Sprintf("/characters/%d/mail/", from.CharacterID), payload, nil)
}
