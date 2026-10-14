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

// ageText renders how long before now t was, for people: "just
// now", "12 minutes ago", "3 hours ago", "2 days ago". The one
// relative-age formatter: the freshness line and the market's
// regions strip and scanner all use it.
func ageText(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 90*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		if h := int(d.Hours()); h == 1 {
			return "1 hour ago"
		} else {
			return fmt.Sprintf("%d hours ago", h)
		}
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// freshnessLine is the page footer: "Updated 12 minutes ago" plus the
// reason when a prefetch caused the fetch.
func freshnessLine(now, fetchedAt time.Time, reason string) string {
	s := "Updated " + ageText(now, fetchedAt)
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
