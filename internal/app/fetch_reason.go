package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Freshness you can see: pages say when their data landed and,
// when a prefetch caused it, why. The ledgers the worker already
// keeps are the record — foresight targets carry their event,
// guesses carry their box — so resolving a reason is two bounded
// reads and no new columns.

// ageString renders how long ago t was: "40s ago", "2m ago".
func ageString(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// freshnessLine is the page footer: "Updated 40s ago" plus the
// reason when a prefetch caused the fetch.
func freshnessLine(now, fetchedAt time.Time, reason string) string {
	s := "Updated " + ageString(now, fetchedAt)
	if reason != "" {
		s += ", because " + reason
	}
	return s
}

// freshnessReasonWindow bounds how long before a fetch the viewer's
// own warming or search may be to count as its cause.
const freshnessReasonWindow = 15 * time.Minute

// fetchReason explains why the record rendered under viewKey was
// fetched when it was: a foresight warming first, a search guess
// second, routine ("") otherwise. Either counts only when it was
// this viewer's own, inside freshnessReasonWindow before the fetch:
// the line never credits a visitor with somebody else's search or
// event. Best-effort display logic: any database trouble resolves to
// routine, never to a broken page.
func (app *Application) fetchReason(ctx context.Context, viewKey, kind string, entityID, regionID int64, fetchedAt time.Time) string {
	userID := app.userID(ctx)
	if userID == 0 {
		return ""
	}
	since := fetchedAt.Add(-freshnessReasonWindow)
	event, err := app.queries.GetForesightWarmedForView(ctx, db.GetForesightWarmedForViewParams{
		ViewKey: viewKey, UserID: userID, WarmedAt: timeSet(fetchedAt), Since: timeSet(since),
	})
	if err == nil {
		switch event {
		case eventDock:
			return "you docked"
		case eventJob:
			return "an industry job finished"
		case eventUndercut:
			return "one of your orders was undercut"
		case eventQueue:
			return "your skill queue ran out"
		case eventWar:
			return "a war was declared"
		default:
			return "an event in your records"
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("freshness: foresight reason for %s: %v", viewKey, err)
	}
	if _, err := app.queries.GetTypingGuessNotedForEntity(ctx, db.GetTypingGuessNotedForEntityParams{
		Kind: kind, EntityID: entityID, RegionID: regionID,
		BoxPrefix: strconv.FormatInt(userID, 10) + "|", NotedAt: fetchedAt, Since: since,
	}); err == nil {
		return "your search"
	} else if !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("freshness: guess reason for %s %d: %v", kind, entityID, err)
	}
	return ""
}
