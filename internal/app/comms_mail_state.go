package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// rewriteSnapshot edits one stored snapshot payload in place. edit
// returns the new payload and whether to store it; the row's
// fetched_at, cached_until and etag are kept, so ESI's cache window
// is not extended and the next conditional fetch still compares
// against what ESI last sent. Best effort: a missing or undecodable
// snapshot is simply left alone.
func (app *Application) rewriteSnapshot(ctx context.Context, characterID int64, kind string, edit func(payload string) (string, bool)) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: characterID, Kind: kind})
	if err != nil {
		return
	}
	next, ok := edit(snap.Payload)
	if !ok {
		return
	}
	if err := app.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: characterID, Kind: kind, Payload: next,
		FetchedAt: snap.FetchedAt, CachedUntil: snap.CachedUntil, Etag: snap.Etag,
	}); err != nil {
		logging.Errorf("mail: rewrite %s snapshot for character %d: %v", kind, characterID, err)
		return
	}
	app.esi.NoteCharacterChanged(characterID)
}

// markMailReadLocally records a mark-as-read that ESI has accepted in
// the stored snapshots the mail page renders from: the header list
// (the unread flag), the label counts (the unread badges), and the
// open mail's own body snapshot. ESI took the change, but the page
// reads these copies, and they would otherwise keep showing the mail
// as unread until the worker's next refresh of each.
func (app *Application) markMailReadLocally(ctx context.Context, characterID, mailID int64) {
	var wasUnread bool
	var labelIDs []int64
	app.rewriteSnapshot(ctx, characterID, esi.SnapMail, func(payload string) (string, bool) {
		var headers esi.MailHeaders
		if json.Unmarshal([]byte(payload), &headers) != nil {
			return "", false
		}
		for i := range headers {
			if headers[i].MailID != mailID {
				continue
			}
			wasUnread = !headers[i].IsRead
			labelIDs = headers[i].Labels
			headers[i].IsRead = true
			out, err := json.Marshal(headers)
			return string(out), err == nil
		}
		return "", false
	})

	if wasUnread {
		app.rewriteSnapshot(ctx, characterID, esi.SnapMailLabels, func(payload string) (string, bool) {
			var labels esi.MailLabels
			if json.Unmarshal([]byte(payload), &labels) != nil {
				return "", false
			}
			for i := range labels.Labels {
				if containsInt64(labelIDs, labels.Labels[i].LabelID) && labels.Labels[i].UnreadCount > 0 {
					labels.Labels[i].UnreadCount--
				}
			}
			if labels.TotalUnreadCount > 0 {
				labels.TotalUnreadCount--
			}
			out, err := json.Marshal(labels)
			return string(out), err == nil
		})
	}

	app.rewriteSnapshot(ctx, characterID, esi.MailBodyKind(mailID), func(payload string) (string, bool) {
		var mail esi.Mail
		if json.Unmarshal([]byte(payload), &mail) != nil {
			return "", false
		}
		mail.Read = true
		out, err := json.Marshal(mail)
		return string(out), err == nil
	})
}

// esiRefusalDetail says why ESI refused a write, for the message
// shown to the reader: " (HTTP 400: <what ESI said>)", or just the
// status when ESI gave no text, or "" when the failure was not an
// ESI answer at all (a network error). The text is ESI's own error
// string for the request the reader made; it carries no tokens.
func esiRefusalDetail(err error) string {
	var se *esi.StatusError
	if !errors.As(err, &se) {
		return ""
	}
	if se.Detail != "" {
		return fmt.Sprintf(" (HTTP %d: %s)", se.Code, se.Detail)
	}
	return fmt.Sprintf(" (HTTP %d)", se.Code)
}
