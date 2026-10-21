package app

import (
	"context"
	"fmt"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// The ship replacement notification source. The notifier asks every
// source for its events; this one reads the SRP request tables (core's
// schema) and the corporation permissions (core's access model), so it
// stays here whether or not the SRP module is built in: with no module,
// no request is ever made and it finds nothing. These constants repeat
// the Ship Replacement module's own and must stay in step with them.
const (
	srpPath         = "/srp/"
	srpPaid         = "paid"
	srpNotifyWindow = 14 * 24 * time.Hour
)

// srpISKText writes an amount in whole ISK; nothing where there is none.
func srpISKText(v float64) string {
	if v <= 0 {
		return ""
	}
	return esi.FormatInt(int64(v+0.5)) + " ISK"
}

// srpPilotName names a pilot: by the linked character's name, else however
// the character is known.
func (app *Application) srpPilotName(ctx context.Context, characterID int64) string {
	if ch, err := app.queries.GetCharacter(ctx, characterID); err == nil && ch.Name != "" {
		return ch.Name
	}
	return app.displayCharacter(ctx, characterID)
}

// notifySRPEvents: what ship replacement tells an account.
//
// A handler is told of each open request in a corporation whose
// requests it may answer (notifySRPNew), other than its own. A pilot
// is told when a request of theirs is paid or denied (notifySRPDone),
// in the name of the character that lost the ship; an answer that is
// taken back and given again is told again.
//
// Both go by the clock, like ops, and read nothing from EVE. An
// account's standing is worked out only when there is an open request
// to tell it about.
func (app *Application) notifySRPEvents(ctx context.Context, c *notifyCollector, userID int64, now time.Time) {
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("notify: corporations of user %d: %v", userID, err)
		return
	}
	mine := map[int64]bool{}
	asked := map[int64]string{}
	var corps []int64
	for _, row := range rows {
		mine[row.CharacterID] = true
		if row.CorporationID != 0 && asked[row.CorporationID] == "" {
			asked[row.CorporationID] = c.source(notifySRPNew, row.CorporationID)
			corps = append(corps, row.CorporationID)
		}
	}
	if len(corps) == 0 {
		return
	}

	answered := c.source(notifySRPDone, userID)
	decided, err := app.queries.ListSRPRequestsDecidedForUser(ctx, db.ListSRPRequestsDecidedForUserParams{UserID: userID, Since: timeSet(now.Add(-srpNotifyWindow))})
	if err != nil {
		logging.Errorf("notify: answered requests of user %d: %v", userID, err)
	}
	for _, req := range decided {
		if mine[req.HandledBy] {
			continue
		}
		title := "SRP denied: " + app.typeNameOrID(ctx, req.ShipTypeID)
		if req.HandlerNote != "" {
			title += " (" + req.HandlerNote + ")"
		}
		if req.Status == srpPaid {
			title = "SRP paid: " + app.typeNameOrID(ctx, req.ShipTypeID) + ", " + srpISKText(req.Payout)
		}
		c.add(notifyEvent{
			Kind: notifySRPDone, CharacterID: req.CharacterID, Source: answered,
			Key:   fmt.Sprintf("srpdone|%d|%s|%d", req.ID, req.Status, req.HandledAt.Time.Unix()),
			Title: title, URL: corpAddress(srpPath, req.CorporationID),
		})
	}

	open, err := app.queries.ListOpenSRPRequests(ctx, corps)
	if err != nil || len(open) == 0 {
		return
	}
	st, err := app.standingFor(ctx, userID)
	if err != nil {
		return
	}
	handles := map[int64]bool{}
	for _, req := range open {
		if req.UserID == userID {
			continue
		}
		may, known := handles[req.CorporationID]
		if !known {
			may = app.corpPermits(ctx, st, req.CorporationID, permSRP)
			handles[req.CorporationID] = may
		}
		if !may {
			continue
		}
		c.add(notifyEvent{
			Kind: notifySRPNew, Source: asked[req.CorporationID],
			Key:   fmt.Sprintf("srp|%d", req.ID),
			Title: fmt.Sprintf("SRP request: %s lost a %s", app.srpPilotName(ctx, req.CharacterID), app.typeNameOrID(ctx, req.ShipTypeID)),
			URL:   corpAddress(srpPath, req.CorporationID),
		})
	}
}
