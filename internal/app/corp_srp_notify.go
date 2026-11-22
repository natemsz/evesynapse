package app

import (
	"context"
	"fmt"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
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
// A handler is told of each open request of a corporation, alliance or
// community whose requests it may answer (notifySRPNew), other than its
// own. A pilot
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
	characterIDs := make([]int64, 0, len(rows))
	var owners []orgOwner
	asked := map[orgOwner]string{}
	// Every owner has its source from the start, so its first request is
	// news and not part of a baseline.
	addOwner := func(o orgOwner) {
		if o.ID == 0 {
			return
		}
		if _, seen := asked[o]; seen {
			return
		}
		if o.Kind == ownerCorporation {
			asked[o] = c.source(notifySRPNew, o.ID)
		} else {
			asked[o] = c.source(notifySRPNew, o.Key())
		}
		owners = append(owners, o)
	}
	for _, row := range rows {
		mine[row.CharacterID] = true
		characterIDs = append(characterIDs, row.CharacterID)
		if row.CorporationID == 0 {
			continue
		}
		addOwner(orgOwner{Kind: ownerCorporation, ID: row.CorporationID})
		if alliance, _ := app.corpAlliance(ctx, row.CorporationID); alliance != 0 {
			addOwner(orgOwner{Kind: ownerAlliance, ID: alliance})
		}
	}
	if len(characterIDs) > 0 {
		communities, err := app.queries.ListCommunitiesOfCharacters(ctx, characterIDs)
		if err != nil {
			logging.Errorf("notify: communities of user %d: %v", userID, err)
		}
		for _, id := range communities {
			addOwner(orgOwner{Kind: ownerCommunity, ID: id})
		}
	}
	if len(owners) == 0 {
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
			Title: title, URL: standing.Address(srpPath, orgOwner{Kind: req.OwnerKind, ID: req.OwnerID}.Key()),
		})
	}

	kinds := make([]string, 0, len(owners))
	ids := make([]int64, 0, len(owners))
	for _, o := range owners {
		kinds, ids = append(kinds, o.Kind), append(ids, o.ID)
	}
	open, err := app.queries.ListNewestOpenSRPRequests(ctx, db.ListNewestOpenSRPRequestsParams{OwnerKinds: kinds, OwnerIds: ids})
	if err != nil || len(open) == 0 {
		return
	}
	st, err := app.standingFor(ctx, userID)
	if err != nil {
		return
	}
	handles := map[orgOwner]bool{}
	for _, req := range open {
		if req.UserID.Valid && req.UserID.Int64 == userID {
			continue
		}
		owner := orgOwner{Kind: req.OwnerKind, ID: req.OwnerID}
		may, known := handles[owner]
		if !known {
			may = app.ownerPermits(ctx, st, owner, permSRP)
			handles[owner] = may
		}
		if !may {
			continue
		}
		c.add(notifyEvent{
			Kind: notifySRPNew, Source: asked[owner],
			Key:   fmt.Sprintf("srp|%d", req.ID),
			Title: fmt.Sprintf("SRP request: %s lost a %s", app.srpPilotName(ctx, req.CharacterID), app.typeNameOrID(ctx, req.ShipTypeID)),
			URL:   standing.Address(srpPath, owner.Key()),
		})
	}
}
